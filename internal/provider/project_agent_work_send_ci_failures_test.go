package provider

import (
	"fmt"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// None of these tests turns agent work on, so send_ci_failures never has an
// item to send, live or not.

// sendCIFailuresConfig is the project with the given agent_work settings.
func sendCIFailuresConfig(env *testEnv, identifier, settings string) string {
	return projectConfig(env, identifier, `
  name = "CI failures"
  agent_work = {
`+settings+`
  }`)
}

// send_ci_failures is set, imported, turned off and dropped from the
// configuration like any other agent work setting, and the data source
// reports it. This one runs live too: it creates and destroys its own project.
func TestProjectAgentWork_sendCIFailuresRoundTrip(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	identifier := randIdentifier()
	lookup := `
data "flightdeck_project" "lookup" {
  id = flightdeck_project.test.id
}
`
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: sendCIFailuresConfig(env, identifier, `    send_ci_failures = true`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "true"),
					// send_ci_failures alone does not turn agent work on, and
					// leaves auto_merge alone.
					resource.TestCheckResourceAttr(projectRes, "agent_work.enabled", "false"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "false"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "1"),
				),
			},
			{
				ResourceName:            projectRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"features"},
			},
			{
				Config: sendCIFailuresConfig(env, identifier, `    send_ci_failures = false`) + lookup,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "false"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "2"),
				),
			},
			{
				// Back on, and the data source reads it on the next step's plan.
				Config: sendCIFailuresConfig(env, identifier, `    send_ci_failures = true`) + lookup,
				Check:  resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "true"),
			},
			{
				// Dropping it from the configuration keeps the stored value and
				// plans nothing.
				Config: sendCIFailuresConfig(env, identifier, `    max_in_progress = 1`) + lookup,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "true"),
					resource.TestCheckResourceAttr("data.flightdeck_project.lookup", "agent_work.send_ci_failures", "true"),
				),
			},
			{
				// Off again before the destroy.
				Config: sendCIFailuresConfig(env, identifier, `    send_ci_failures = false`),
				Check:  resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "false"),
			},
		},
	})
	if env.live() {
		return
	}
	writes := agentWorkRequests(env.fake.RequestsMatching("PATCH", "/api/v1/projects/"))
	if len(writes) != 4 {
		t.Fatalf("expected four agent-work PATCHes, got %d", len(writes))
	}
	for i, want := range []any{true, false, true, false} {
		if body := agentWorkBody(t, writes[i]); len(body) != 1 || body["send_ci_failures"] != want {
			t.Errorf("write %d must send send_ci_failures = %v only, got %v", i+1, want, body)
		}
	}
}

// Unset, send_ci_failures is never sent: state reads whatever the server
// holds, including a value changed on the settings page, and plans nothing
// for it.
func TestProjectAgentWork_sendCIFailuresUnsetIsNeverSent(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: sendCIFailuresConfig(env, identifier, `    max_in_progress = 2`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					// The server's default.
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "false"),
				),
			},
			{
				// Turned on on the settings page: adopted, nothing planned.
				PreConfig: func() {
					env.fake.SetAgentWorkOutOfBand(mustInt64(t, id), func(row *flightdecktest.AgentWorkSetting) { row.SendCIFailures = true })
				},
				Config: sendCIFailuresConfig(env, identifier, `    max_in_progress = 2`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "true"),
			},
			{
				// Another setting changes; send_ci_failures stays as the page left it.
				Config: sendCIFailuresConfig(env, identifier, `    max_in_progress = 3`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "3"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "true"),
					func(*terraform.State) error {
						if row := env.fake.AgentWorkOf(mustInt64(t, id)); row == nil || !row.SendCIFailures {
							return fmt.Errorf("the write changed the stored send_ci_failures: %+v", row)
						}
						return nil
					},
				),
			},
		},
	})
	writes := agentWorkRequests(env.fake.RequestsMatching("PATCH", "/api/v1/projects/"))
	if len(writes) != 2 {
		t.Fatalf("expected two agent-work PATCHes, got %d", len(writes))
	}
	for i, r := range writes {
		if body := agentWorkBody(t, r); len(body) != 1 || body["max_in_progress"] == nil {
			t.Errorf("write %d must send max_in_progress only, never send_ci_failures: %v", i+1, body)
		}
	}
}

// Configured, a value changed on the settings page shows in the next plan and
// is set back, whichever way it was changed.
func TestProjectAgentWork_sendCIFailuresDrift(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	outOfBand := func(on bool) func() {
		return func() {
			env.fake.SetAgentWorkOutOfBand(mustInt64(t, id), func(row *flightdecktest.AgentWorkSetting) { row.SendCIFailures = on })
		}
	}
	update := resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: sendCIFailuresConfig(env, identifier, `    send_ci_failures = true`),
				Check:  captureAttr(projectRes, "id", &id),
			},
			{
				PreConfig:        outOfBand(false),
				Config:           sendCIFailuresConfig(env, identifier, `    send_ci_failures = true`),
				ConfigPlanChecks: update,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "true"),
					// Created at 1, moved by the page to 2, written back at 3.
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "3"),
				),
			},
			{Config: sendCIFailuresConfig(env, identifier, `    send_ci_failures = false`)},
			{
				PreConfig:        outOfBand(true),
				Config:           sendCIFailuresConfig(env, identifier, `    send_ci_failures = false`),
				ConfigPlanChecks: update,
				Check:            resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "false"),
			},
			{Config: sendCIFailuresConfig(env, identifier, `    send_ci_failures = false`), PlanOnly: true},
		},
	})
}

// A Flightdeck older than send_ci_failures (but with auto_merge, the release
// before it): a project whose configuration leaves it unset works as before
// (it reads null and is never sent). Setting it fails on the attribute,
// saying the server is too old, saves nothing, and applies once Flightdeck is
// upgraded.
func TestProjectAgentWork_sendCIFailuresOnAnOlderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkSendCIFailuresSupported(false)
	identifier := randIdentifier()
	var id string
	withSendCIFailures := sendCIFailuresConfig(env, identifier, "    max_in_progress  = 3\n    auto_merge       = true\n    send_ci_failures = true")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: sendCIFailuresConfig(env, identifier, `    max_in_progress = 2`) + `
data "flightdeck_project" "lookup" {
  id = flightdeck_project.test.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "2"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "false"),
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.send_ci_failures"),
					resource.TestCheckNoResourceAttr("data.flightdeck_project.lookup", "agent_work.send_ci_failures"),
				),
			},
			{
				// The error names send_ci_failures, not auto_merge, which this
				// Flightdeck knows.
				Config: withSendCIFailures,
				ExpectError: regexMust(`(?s)This Flightdeck does not support agent_work\.send_ci_failures yet.*` +
					`older\s+than\s+the\s+release\s+that\s+added\s+sending\s+CI\s+failures\s+to\s+agents.*` +
					`Upgrade\s+Flightdeck\s+first.*unknown key: send_ci_failures`),
			},
			{
				// Nothing was saved, max_in_progress and auto_merge included.
				PreConfig: func() {
					if row := env.fake.AgentWorkOf(mustInt64(t, id)); row == nil || row.MaxInProgress != 2 || row.AutoMerge {
						t.Errorf("a refused write must save nothing, but the settings are %+v", row)
					}
				},
				Config:             withSendCIFailures,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Flightdeck is upgraded; the same configuration applies.
				PreConfig: func() { env.fake.SetAgentWorkSendCIFailuresSupported(true) },
				Config:    withSendCIFailures,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "true"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "true"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "3"),
				),
			},
			{Config: withSendCIFailures, PlanOnly: true},
		},
	})
	writes := agentWorkRequests(env.fake.RequestsMatching("PATCH", "/api/v1/projects/"))
	if len(writes) == 0 {
		t.Fatal("no agent-work PATCH was recorded")
	}
	if _, sent := agentWorkBody(t, writes[0])["send_ci_failures"]; sent {
		t.Errorf("an unset send_ci_failures must not be sent to an older Flightdeck: %v", agentWorkBody(t, writes[0]))
	}
}

// On the apply that creates a project, an older Flightdeck's refusal of
// send_ci_failures is a warning on the attribute, like any refusal of the
// block's settings there: an error would taint the new project. The next
// apply is an update of the same project.
func TestProjectAgentWork_sendCIFailuresOnAnOlderFlightdeckAtCreateWarns(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkSendCIFailuresSupported(false)
	identifier := randIdentifier()
	config := sendCIFailuresConfig(env, identifier, "    max_in_progress  = 2\n    send_ci_failures = true")
	var id string
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:             config,
				ExpectNonEmptyPlan: true,
				Check:              captureAttr(projectRes, "id", &id),
			},
			{
				PreConfig: func() {
					if row := env.fake.AgentWorkOf(mustInt64(t, id)); row != nil {
						t.Errorf("a refused write must save nothing, but the settings were stored: %+v", row)
					}
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// An update, not a replace: the create did not taint the project.
				PreConfig: func() { env.fake.SetAgentWorkSendCIFailuresSupported(true) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "true"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "2"),
				),
			},
			{Config: config, PlanOnly: true},
		},
	})
	if errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError); len(errs) != 0 {
		t.Errorf("no apply may fail, got %d error(s): %s", len(errs), errs[0].Summary)
	}
	warnings := recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning)
	at := tftypes.NewAttributePath().WithAttributeName("agent_work").WithAttributeName("send_ci_failures")
	if !diagnosticAt(warnings, "This Flightdeck does not support agent_work.send_ci_failures yet", at,
		"The project was created", "unknown key: send_ci_failures") {
		t.Fatalf("no create-time warning on agent_work.send_ci_failures; apply warnings: %s", describeWithPaths(warnings))
	}
	for _, w := range warnings {
		if strings.Contains(w.Summary, "auto_merge") {
			t.Errorf("auto_merge was not sent, so nothing may warn about it: %s", w.Summary)
		}
	}
}

// How writeAgentWork reports a refused update that sent send_ci_failures,
// alone or with auto_merge. The read after the refusal, not the API's
// wording, decides which settings the server is too old for, and each is
// reported on its own attribute. When the read fails, the refusal is reported
// as it came, with a note for each newer setting it names as unknown; a
// Flightdeck that knows send_ci_failures and refuses something else gets no
// upgrade hint.
func TestWriteAgentWork_sendCIFailuresRefusals(t *testing.T) {
	sendPath := path.Root("agent_work").AtName("send_ci_failures")
	autoMergePath := path.Root("agent_work").AtName("auto_merge")
	researchPath := path.Root("agent_work").AtName("research_label_id")
	blockPath := path.Root("agent_work")
	// newProject returns a project, one of its labels, and a label of another
	// project, which Flightdeck refuses as the research label.
	newProject := func(t *testing.T, env *testEnv) (c *client.Client, projectID, research, foreign int64) {
		t.Helper()
		c, err := client.New(env.fake.URL, env.fake.Token())
		if err != nil {
			t.Fatal(err)
		}
		create := func(name string) int64 {
			fields := client.Fields{"name": name, "identifier": randIdentifier()}
			p, err := c.CreateProject(t.Context(), fields, client.PayloadKey("project", "", fields))
			if err != nil {
				t.Fatal(err)
			}
			return p.ID
		}
		label := func(projectID int64, name string) int64 {
			l, err := c.CreateLabel(t.Context(), projectID, client.Fields{"name": name}, "label-"+name)
			if err != nil {
				t.Fatal(err)
			}
			return l.ID
		}
		projectID, other := create("CI failures refusal"), create("Another project")
		return c, projectID, label(projectID, "research-first"), label(other, "research-elsewhere")
	}
	// readFailsAfterRefusal makes the read that follows the next agent-work
	// PATCH fail, as if the endpoint went away in between.
	readFailsAfterRefusal := func(env *testEnv, id int64) {
		route := fmt.Sprintf("/api/v1/projects/%d/agent-work", id)
		env.fake.OnNextRequest("PATCH", route, func() {
			env.fake.OnNextRequest("GET", route, func() { env.fake.SetAgentWorkEndpoint(false) })
		})
	}
	at := func(d diag.Diagnostic) path.Path {
		if withPath, ok := d.(diag.DiagnosticWithPath); ok {
			return withPath.Path()
		}
		return path.Empty()
	}
	write := func(t *testing.T, c *client.Client, projectID int64, m agentWorkModel) diag.Diagnostics {
		t.Helper()
		var diags diag.Diagnostics
		block := agentWorkObj(t, &m)
		writeAgentWork(t.Context(), c, projectID, "CIF", block, types.ObjectNull(agentWorkAttrTypes),
			types.ObjectUnknown(agentWorkAttrTypes), agentWorkOnUpdate, &diags)
		return diags
	}
	wantUnsupported := func(t *testing.T, d diag.Diagnostic, key string, where path.Path, added string) {
		t.Helper()
		if got := d.Summary(); got != "This Flightdeck does not support agent_work."+key+" yet" {
			t.Errorf("summary = %q", got)
		}
		if got := at(d); !got.Equal(where) {
			t.Errorf("reported at %s, want %s", got, where)
		}
		for _, want := range []string{"older than the release that added " + added, "Upgrade Flightdeck first",
			"leave `" + key + "` out", "unknown key"} {
			if !strings.Contains(d.Detail(), want) {
				t.Errorf("detail does not carry %q:\n%s", want, d.Detail())
			}
		}
	}

	t.Run("older Flightdeck, sent with the research label", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkSendCIFailuresSupported(false)
		c, id, research, _ := newProject(t, env)
		diags := write(t, c, id, agentWorkModel{SendCIFailures: types.BoolValue(true), ResearchLabelID: types.Int64Value(research)})
		errs := diags.Errors()
		if len(errs) != 1 {
			t.Fatalf("expected exactly one error, got %v", diags)
		}
		wantUnsupported(t, errs[0], "send_ci_failures", sendPath, "sending CI failures to agents")
		if !strings.Contains(errs[0].Detail(), "unknown key: send_ci_failures") {
			t.Errorf("detail does not carry the API's refusal:\n%s", errs[0].Detail())
		}
	})

	// A Flightdeck with auto_merge but not send_ci_failures refuses the write
	// for send_ci_failures alone; auto_merge is not blamed.
	t.Run("older Flightdeck, sent with auto_merge", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkSendCIFailuresSupported(false)
		c, id, _, _ := newProject(t, env)
		diags := write(t, c, id, agentWorkModel{SendCIFailures: types.BoolValue(true), AutoMerge: types.BoolValue(true)})
		errs := diags.Errors()
		if len(errs) != 1 {
			t.Fatalf("expected exactly one error, got %v", diags)
		}
		wantUnsupported(t, errs[0], "send_ci_failures", sendPath, "sending CI failures to agents")
		if row := env.fake.AgentWorkOf(id); row != nil {
			t.Errorf("a refused write must save nothing, auto_merge included, got %+v", row)
		}
	})

	// A Flightdeck older than both refuses both, and each is reported on its
	// own attribute.
	t.Run("Flightdeck older than both, both sent", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkBeforeAutoMerge)
		c, id, _, _ := newProject(t, env)
		diags := write(t, c, id, agentWorkModel{SendCIFailures: types.BoolValue(false), AutoMerge: types.BoolValue(false)})
		errs := diags.Errors()
		if len(errs) != 2 {
			t.Fatalf("expected exactly two errors, got %v", diags)
		}
		wantUnsupported(t, errs[0], "auto_merge", autoMergePath, "auto-merge for agent pull requests")
		wantUnsupported(t, errs[1], "send_ci_failures", sendPath, "sending CI failures to agents")
		for _, e := range errs {
			if !strings.Contains(e.Detail(), "unknown keys: auto_merge, send_ci_failures") {
				t.Errorf("detail does not carry the API's refusal:\n%s", e.Detail())
			}
		}
	})

	t.Run("older Flightdeck, the read fails", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkSendCIFailuresSupported(false)
		c, id, _, _ := newProject(t, env)
		readFailsAfterRefusal(env, id)
		diags := write(t, c, id, agentWorkModel{SendCIFailures: types.BoolValue(true)})
		errs := diags.Errors()
		if len(errs) != 1 {
			t.Fatalf("expected exactly one error, got %v", diags)
		}
		if got := errs[0].Summary(); got != "Flightdeck refused the agent work settings" {
			t.Errorf("summary = %q", got)
		}
		detail := errs[0].Detail()
		for _, want := range []string{"unknown key: send_ci_failures", agentWorkSendCIFailuresUnknownNote} {
			if !strings.Contains(detail, want) {
				t.Errorf("detail does not carry %q:\n%s", want, detail)
			}
		}
		// auto_merge is in the settable list, not refused.
		if strings.Contains(detail, agentWorkAutoMergeUnknownNote) {
			t.Errorf("a refusal that lists auto_merge as settable must not carry its note:\n%s", detail)
		}
	})

	t.Run("Flightdeck older than both, both sent, the read fails", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkBeforeAutoMerge)
		c, id, _, _ := newProject(t, env)
		readFailsAfterRefusal(env, id)
		diags := write(t, c, id, agentWorkModel{SendCIFailures: types.BoolValue(true), AutoMerge: types.BoolValue(true)})
		errs := diags.Errors()
		if len(errs) != 1 {
			t.Fatalf("expected exactly one error, got %v", diags)
		}
		if got := at(errs[0]); !got.Equal(blockPath) {
			t.Errorf("reported at %s, want %s", got, blockPath)
		}
		for _, want := range []string{"unknown keys: auto_merge, send_ci_failures", agentWorkAutoMergeUnknownNote,
			agentWorkSendCIFailuresUnknownNote} {
			if !strings.Contains(errs[0].Detail(), want) {
				t.Errorf("detail does not carry %q:\n%s", want, errs[0].Detail())
			}
		}
	})

	t.Run("a Flightdeck with send_ci_failures refuses something else", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		c, id, _, _ := newProject(t, env)
		// More than the stored daily budget (10), which Flightdeck refuses.
		diags := write(t, c, id, agentWorkModel{SendCIFailures: types.BoolValue(true), TaskMaxUSD: types.Float64Value(20)})
		errs := diags.Errors()
		if len(errs) != 1 {
			t.Fatalf("expected exactly one error, got %v", diags)
		}
		if got := errs[0].Summary(); got != "Flightdeck refused the agent work settings" {
			t.Errorf("summary = %q", got)
		}
		detail := errs[0].Detail()
		if !strings.Contains(detail, "can't be more than the daily budget") {
			t.Errorf("detail does not carry the API's refusal:\n%s", detail)
		}
		if strings.Contains(detail, "older than the release") || strings.Contains(strings.ToLower(detail), "upgrade") {
			t.Errorf("a Flightdeck that knows send_ci_failures must not be told to upgrade:\n%s", detail)
		}
		if row := env.fake.AgentWorkOf(id); row != nil {
			t.Errorf("a refused write must save nothing, got %+v", row)
		}
	})

	// The refusal says the research label is the problem, so it lands there
	// whatever the read after it did, and nothing says to upgrade.
	for _, readFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("a Flightdeck with send_ci_failures refuses the research label, read fails %t", readFails), func(t *testing.T) {
			env := newTestEnv(t, "agent_work")
			env.requireFake(t)
			c, id, _, foreign := newProject(t, env)
			if readFails {
				readFailsAfterRefusal(env, id)
			}
			diags := write(t, c, id, agentWorkModel{SendCIFailures: types.BoolValue(true), ResearchLabelID: types.Int64Value(foreign)})
			errs := diags.Errors()
			if len(errs) != 1 {
				t.Fatalf("expected exactly one error, got %v", diags)
			}
			if got := errs[0].Summary(); got != "Flightdeck refused the research label" {
				t.Errorf("summary = %q", got)
			}
			if got := at(errs[0]); !got.Equal(researchPath) {
				t.Errorf("reported at %s, want %s", got, researchPath)
			}
			if strings.Contains(errs[0].Detail(), agentWorkSendCIFailuresUnknownNote) {
				t.Errorf("a refusal that does not name send_ci_failures must not carry its note:\n%s", errs[0].Detail())
			}
		})
	}

	// An older Flightdeck's refusal of send_ci_failures lists
	// research_label_id among the settable keys. When the read cannot tell,
	// that list must not pass for a refusal of the research label, and the
	// refusal's own words, which name send_ci_failures as unknown, bring the
	// upgrade note.
	t.Run("older Flightdeck, sent with the research label, the read fails", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkSendCIFailuresSupported(false)
		c, id, research, _ := newProject(t, env)
		readFailsAfterRefusal(env, id)
		diags := write(t, c, id, agentWorkModel{SendCIFailures: types.BoolValue(true), ResearchLabelID: types.Int64Value(research)})
		errs := diags.Errors()
		if len(errs) != 1 {
			t.Fatalf("expected exactly one error, got %v", diags)
		}
		if got := errs[0].Summary(); got != "Flightdeck refused the agent work settings" {
			t.Errorf("summary = %q", got)
		}
		if got := at(errs[0]); !got.Equal(blockPath) {
			t.Errorf("reported at %s, want %s", got, blockPath)
		}
		for _, want := range []string{"unknown key: send_ci_failures", agentWorkSendCIFailuresUnknownNote} {
			if !strings.Contains(errs[0].Detail(), want) {
				t.Errorf("detail does not carry %q:\n%s", want, errs[0].Detail())
			}
		}
	})
}

// refusesAsUnknown and agentWorkUnknownNotes read the keys an unknown-key
// refusal names, in Flightdeck's words, and never the settable list after
// them.
func TestRefusesAsUnknown_sendCIFailures(t *testing.T) {
	refusal := func(code, msg string) error {
		return &client.Error{Method: "PATCH", Path: "/projects/4/agent-work", Status: 422, Code: code, Message: msg}
	}
	settable := " (settable: enabled, kinds, auto_merge, send_ci_failures, label_id, research_label_id)"
	cases := []struct {
		name            string
		err             error
		send, autoMerge bool
	}{
		{"one key", refusal(client.CodeInvalidAttribute, "unknown key: send_ci_failures"+settable), true, false},
		{"both keys", refusal(client.CodeInvalidAttribute, "unknown keys: auto_merge, send_ci_failures"+settable), true, true},
		{"several, some left out", refusal(client.CodeInvalidAttribute, "unknown keys: alpha, send_ci_failures and 4 more"+settable), true, false},
		{"another key, both settable", refusal(client.CodeInvalidAttribute, "unknown key: zebra"+settable), false, false},
		{"a type error", refusal(client.CodeInvalidAttribute, `send_ci_failures must be true or false, got "true"`), false, false},
		{"a rule", refusal(client.CodeValidationFailed, "unknown key: send_ci_failures"), false, false},
		{"not a 422", &client.Error{Status: 409, Code: client.CodeStaleObject, Message: "unknown key: send_ci_failures"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := refusesAsUnknown(tc.err, "send_ci_failures"); got != tc.send {
				t.Errorf("refusesAsUnknown(send_ci_failures) = %t, want %t", got, tc.send)
			}
			if got := refusesAsUnknown(tc.err, "auto_merge"); got != tc.autoMerge {
				t.Errorf("refusesAsUnknown(auto_merge) = %t, want %t", got, tc.autoMerge)
			}
			want := ""
			if tc.autoMerge {
				want += "\n\n" + agentWorkAutoMergeUnknownNote
			}
			if tc.send {
				want += "\n\n" + agentWorkSendCIFailuresUnknownNote
			}
			if got := agentWorkUnknownNotes(tc.err); got != want {
				t.Errorf("agentWorkUnknownNotes = %q, want %q", got, want)
			}
		})
	}
}

// Flightdeck is upgraded to send_ci_failures, or rolled back below it,
// between the plan and the apply, as a saved plan applied later can see. An
// unset send_ci_failures is never sent, so the apply keeps the value the plan
// promised rather than fail with "Provider produced inconsistent result", and
// the next refresh adopts the server's value without planning anything.
func TestProjectAgentWork_sendCIFailuresUnsetSurvivesAServerChangeDuringApply(t *testing.T) {
	cases := []struct {
		name string
		// before is whether Flightdeck has send_ci_failures at plan time.
		before bool
		// block is whether the configuration has an agent_work block (one
		// that leaves send_ci_failures unset) or none at all.
		block bool
	}{
		{name: "upgraded, block without send_ci_failures", before: false, block: true},
		{name: "rolled back, block without send_ci_failures", before: true, block: true},
		{name: "upgraded, no block", before: false, block: false},
		{name: "rolled back, no block", before: true, block: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, "agent_work")
			env.requireFake(t)
			env.fake.SetAgentWorkSendCIFailuresSupported(tc.before)
			identifier := randIdentifier()
			var id string
			config := func(name string, n int) string {
				body := fmt.Sprintf("  name = %q", name)
				if tc.block {
					body += fmt.Sprintf("\n  agent_work = {\n    max_in_progress = %d\n  }", n)
				}
				return projectConfig(env, identifier, body)
			}
			// The value state holds before the change, and the one Flightdeck
			// reports after it.
			check := func(supported bool) resource.TestCheckFunc {
				if supported {
					return resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "false")
				}
				return resource.TestCheckNoResourceAttr(projectRes, "agent_work.send_ci_failures")
			}
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{Config: config("Server change", 2), Check: resource.ComposeAggregateTestCheckFunc(captureAttr(projectRes, "id", &id), check(tc.before))},
					{
						// The change lands after the plan, just before the write
						// that touches the project: its agent-work PATCH, or with
						// no block, the project PATCH ahead of the agent-work read.
						PreConfig: func() {
							route := fmt.Sprintf("/api/v1/projects/%s", id)
							if tc.block {
								route += "/agent-work"
							}
							env.fake.OnNextRequest("PATCH", route, func() { env.fake.SetAgentWorkSendCIFailuresSupported(!tc.before) })
						},
						Config: config("Server change renamed", 3),
						// The plan's value, kept.
						Check: check(tc.before),
					},
					{
						// The refresh adopts what Flightdeck now reports, and
						// plans nothing for it.
						Config: config("Server change renamed", 3),
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
						},
						Check: check(!tc.before),
					},
				},
			})
		})
	}
}

// A configured send_ci_failures gets no such allowance: a Flightdeck rolled
// back below the setting between the plan and the apply refuses the write,
// and the apply fails saying so.
func TestProjectAgentWork_sendCIFailuresConfiguredMeetsARollbackDuringApply(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: sendCIFailuresConfig(env, identifier, `    send_ci_failures = false`), Check: captureAttr(projectRes, "id", &id)},
			{
				PreConfig: func() {
					env.fake.OnNextRequest("PATCH", fmt.Sprintf("/api/v1/projects/%s/agent-work", id), func() {
						env.fake.SetAgentWorkSendCIFailuresSupported(false)
					})
				},
				Config:      sendCIFailuresConfig(env, identifier, `    send_ci_failures = true`),
				ExpectError: regexMust(`This Flightdeck does not support agent_work\.send_ci_failures yet`),
			},
		},
	})
}

// keepUnsetAsPlanned changes send_ci_failures in the result only when the
// configuration leaves it unset and the plan knew its value, and never
// touches the other newer setting.
func TestKeepUnsetSendCIFailuresAsPlanned(t *testing.T) {
	ctx := t.Context()
	on, off := types.BoolValue(true), types.BoolValue(false)
	obj := func(v types.Bool) types.Object {
		return agentWorkObj(t, &agentWorkModel{SendCIFailures: v, AutoMerge: on, MaxInProgress: types.Int64Value(3)})
	}
	cases := []struct {
		name                   string
		config, planned, after types.Object
		want                   types.Bool
	}{
		{"unset, the plan had null, the server says false", obj(types.BoolNull()), obj(types.BoolNull()), obj(off), types.BoolNull()},
		{"unset, the plan had false, the server says null", obj(types.BoolNull()), obj(off), obj(types.BoolNull()), off},
		{"no block, the plan had null", types.ObjectNull(agentWorkAttrTypes), obj(types.BoolNull()), obj(off), types.BoolNull()},
		{"unset, the plan did not know", obj(types.BoolNull()), obj(types.BoolUnknown()), obj(off), off},
		{"configured true, the server says false", obj(on), obj(on), obj(off), off},
		{"configured false, the server says null", obj(off), obj(off), obj(types.BoolNull()), types.BoolNull()},
		{"configured but not known until apply", obj(types.BoolUnknown()), obj(types.BoolUnknown()), obj(on), on},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			out := keepUnsetAsPlanned("send_ci_failures", tc.config, tc.planned, tc.after, &diags)
			if diags.HasError() {
				t.Fatalf("errors: %v", diags.Errors())
			}
			var got agentWorkModel
			diags.Append(out.As(ctx, &got, objectAsOptions)...)
			if !got.SendCIFailures.Equal(tc.want) {
				t.Errorf("send_ci_failures = %v, want %v", got.SendCIFailures, tc.want)
			}
			if got.MaxInProgress.ValueInt64() != 3 || !got.AutoMerge.Equal(on) {
				t.Errorf("the rest of the block changed: %+v", got)
			}
		})
	}
	// A failed write leaves no block, and stays that way.
	var diags diag.Diagnostics
	if out := keepUnsetAsPlanned("send_ci_failures", obj(types.BoolNull()), obj(off), types.ObjectNull(agentWorkAttrTypes), &diags); !out.IsNull() {
		t.Fatalf("a null result must stay null, got %v", out)
	}
	// Each newer setting is corrected on its own: an unset auto_merge the
	// server changed is kept as planned while a configured send_ci_failures
	// is left as the server reports it.
	config := agentWorkObj(t, &agentWorkModel{SendCIFailures: on, AutoMerge: types.BoolNull(), MaxInProgress: types.Int64Value(3)})
	planned := agentWorkObj(t, &agentWorkModel{SendCIFailures: on, AutoMerge: types.BoolNull(), MaxInProgress: types.Int64Value(3)})
	after := agentWorkObj(t, &agentWorkModel{SendCIFailures: off, AutoMerge: off, MaxInProgress: types.Int64Value(3)})
	out := after
	for _, setting := range agentWorkNewerSettings {
		out = keepUnsetAsPlanned(setting.key, config, planned, out, &diags)
	}
	var got agentWorkModel
	diags.Append(out.As(ctx, &got, objectAsOptions)...)
	if diags.HasError() {
		t.Fatalf("errors: %v", diags.Errors())
	}
	if !got.AutoMerge.IsNull() || !got.SendCIFailures.Equal(off) {
		t.Errorf("want auto_merge kept as planned (null) and send_ci_failures as reported (false), got %v and %v",
			got.AutoMerge, got.SendCIFailures)
	}
}

// A change elsewhere in the project plans an unset send_ci_failures as the
// value in state, not "known after apply", and so leaves the settings'
// lock_version known too. On an older Flightdeck that value is null.
func TestProjectAgentWork_sendCIFailuresUnsetStaysKnownOnARename(t *testing.T) {
	for _, supported := range []bool{true, false} {
		t.Run(fmt.Sprintf("Flightdeck has send_ci_failures: %t", supported), func(t *testing.T) {
			env := newTestEnv(t, "agent_work")
			env.requireFake(t)
			env.fake.SetAgentWorkSendCIFailuresSupported(supported)
			identifier := randIdentifier()
			config := func(name string) string {
				return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  agent_work = {
    max_in_progress = 2
  }`, name))
			}
			var want knownvalue.Check = knownvalue.Null()
			if supported {
				want = knownvalue.Bool(false)
			}
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{Config: config("Known")},
					{
						Config: config("Known renamed"),
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{
								plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate),
								plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("agent_work").AtMapKey("send_ci_failures"), want),
								plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("agent_work").AtMapKey("lock_version"), knownvalue.Int64Exact(1)),
							},
						},
					},
				},
			})
		})
	}
}

// agentWorkSettingRefused decides from the read after a refusal, never from
// the API's wording: only a 422 to a write that sent send_ci_failures, from a
// Flightdeck whose read has no send_ci_failures, is the server being too old
// for it. agentWorkRefusedSettings lists every newer setting that holds for.
func TestAgentWorkSendCIFailuresRefused(t *testing.T) {
	on := true
	knowsBoth := &client.AgentWork{AutoMerge: &on, SendCIFailures: &on}
	knowsAutoMerge := &client.AgentWork{AutoMerge: &on}
	olderThanBoth := &client.AgentWork{}
	unknownKey := &client.Error{Method: "PATCH", Path: "/projects/4/agent-work", Status: 422, Code: client.CodeInvalidAttribute,
		Message: "unknown key: send_ci_failures (settable: enabled, kinds, auto_merge, label_id, accept_machine_labels, " +
			"research_label_id, agent_account_id, base_ref, max_in_progress, daily_budget_usd, task_max_usd, " +
			"task_max_minutes, queue_minutes, runbook)"}
	stale := &client.Error{Status: 409, Code: client.CodeStaleObject, Message: "send_ci_failures changed"}
	sent := client.Fields{"send_ci_failures": true}
	both := client.Fields{"send_ci_failures": true, "auto_merge": false}
	cases := []struct {
		name    string
		current *client.AgentWork
		sent    client.Fields
		err     error
		want    bool
		refused []string
	}{
		{"older Flightdeck, sent", knowsAutoMerge, sent, unknownKey, true, []string{"send_ci_failures"}},
		{"older Flightdeck, sent false", knowsAutoMerge, client.Fields{"send_ci_failures": false}, unknownKey, true, []string{"send_ci_failures"}},
		{"older Flightdeck, sent with auto_merge", knowsAutoMerge, both, unknownKey, true, []string{"send_ci_failures"}},
		{"older than both, both sent", olderThanBoth, both, unknownKey, true, []string{"auto_merge", "send_ci_failures"}},
		{"older than both, only auto_merge sent", olderThanBoth, client.Fields{"auto_merge": true}, unknownKey, false, []string{"auto_merge"}},
		{"a Flightdeck that knows it", knowsBoth, both, unknownKey, false, nil},
		{"not sent", knowsAutoMerge, client.Fields{"max_in_progress": int64(2)}, unknownKey, false, nil},
		{"not a 422", knowsAutoMerge, sent, stale, false, nil},
		{"no read", nil, sent, unknownKey, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentWorkSettingRefused(agentWorkSendCIFailures, tc.current, tc.sent, tc.err); got != tc.want {
				t.Fatalf("agentWorkSettingRefused = %t, want %t", got, tc.want)
			}
			var keys []string
			for _, setting := range agentWorkRefusedSettings(tc.current, tc.sent, tc.err) {
				keys = append(keys, setting.key)
			}
			if strings.Join(keys, ",") != strings.Join(tc.refused, ",") {
				t.Fatalf("agentWorkRefusedSettings = %v, want %v", keys, tc.refused)
			}
		})
	}
}

// A Flightdeck older than auto_merge is older than send_ci_failures too. A
// configuration that sets only auto_merge fails there with one error, on
// auto_merge: send_ci_failures was not sent, so it is not blamed. Upgraded to
// the release with send_ci_failures, the same configuration applies, and the
// unset send_ci_failures is adopted from the server without planning
// anything.
func TestProjectAgentWork_autoMergeOnAFlightdeckOlderThanBoth(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkBeforeAutoMerge)
	identifier := randIdentifier()
	withAutoMerge := autoMergeConfig(env, identifier, "    max_in_progress = 3\n    auto_merge      = true")
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: autoMergeConfig(env, identifier, `    max_in_progress = 2`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.auto_merge"),
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.send_ci_failures"),
				),
			},
			{
				Config:      withAutoMerge,
				ExpectError: regexMust(`This Flightdeck does not support agent_work\.auto_merge yet`),
			},
			{
				// Flightdeck is upgraded to the release with send_ci_failures.
				PreConfig: func() { env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkWithSendCIFailures) },
				Config:    withAutoMerge,
				Check:     resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "true"),
			},
			{
				Config: withAutoMerge,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "false"),
			},
		},
	})
	errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError)
	if len(errs) != 1 {
		t.Fatalf("want exactly one apply error, got %d: %s", len(errs), describeWithPaths(errs))
	}
	at := tftypes.NewAttributePath().WithAttributeName("agent_work").WithAttributeName("auto_merge")
	if !diagnosticAt(errs, "This Flightdeck does not support agent_work.auto_merge yet", at, "unknown key: auto_merge") {
		t.Errorf("the error is not on agent_work.auto_merge: %s", describeWithPaths(errs))
	}
	if strings.Contains(errs[0].Summary, "send_ci_failures") || strings.Contains(errs[0].Detail, "sending CI failures") {
		t.Errorf("send_ci_failures is blamed, but only auto_merge was sent: %s", describeWithPaths(errs))
	}
}

// Creating a project that sets both newer settings on a Flightdeck older
// than both gives two warnings, one on each attribute, and no error, so the
// new project isn't tainted. Once Flightdeck is upgraded, the next apply is
// an update of the same project.
func TestProjectAgentWork_newerSettingsAtCreateOnAFlightdeckOlderThanBoth(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkBeforeAutoMerge)
	identifier := randIdentifier()
	config := sendCIFailuresConfig(env, identifier, "    max_in_progress  = 2\n    auto_merge       = true\n    send_ci_failures = true")
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config, ExpectNonEmptyPlan: true},
			{
				PreConfig: func() { env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkWithSendCIFailures) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "true"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "true"),
				),
			},
			{Config: config, PlanOnly: true},
		},
	})
	if errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError); len(errs) != 0 {
		t.Errorf("no apply may fail, got %d error(s): %s", len(errs), describeWithPaths(errs))
	}
	warnings := recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning)
	for _, key := range []string{"auto_merge", "send_ci_failures"} {
		at := tftypes.NewAttributePath().WithAttributeName("agent_work").WithAttributeName(key)
		if !diagnosticAt(warnings, "This Flightdeck does not support agent_work."+key+" yet", at, "The project was created") {
			t.Errorf("no create-time warning on agent_work.%s: %s", key, describeWithPaths(warnings))
		}
	}
	unsupported := 0
	for _, w := range warnings {
		if strings.HasPrefix(w.Summary, "This Flightdeck does not support") {
			unsupported++
		}
	}
	if unsupported != 2 {
		t.Errorf("want two warnings about unsupported settings, got %d: %s", unsupported, describeWithPaths(warnings))
	}
}

// Flightdeck is upgraded from below auto_merge to the release with
// send_ci_failures, or rolled back from that release to below auto_merge,
// between the plan and the apply: both settings change at once. Left unset, each keeps the value
// the plan promised, and the next refresh adopts the server's for both
// without planning anything.
func TestProjectAgentWork_newerSettingsUnsetSurviveAServerChangeDuringApply(t *testing.T) {
	cases := []struct {
		name string
		// before is whether Flightdeck has both settings at plan time.
		before bool
		// block is whether the configuration has an agent_work block (one
		// that leaves both unset) or none at all.
		block bool
	}{
		{name: "upgraded, block without either", before: false, block: true},
		{name: "rolled back, block without either", before: true, block: true},
		{name: "upgraded, no block", before: false, block: false},
		{name: "rolled back, no block", before: true, block: false},
	}
	release := func(both bool) flightdecktest.AgentWorkRelease {
		if both {
			return flightdecktest.AgentWorkWithSendCIFailures
		}
		return flightdecktest.AgentWorkBeforeAutoMerge
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, "agent_work")
			env.requireFake(t)
			env.fake.SetAgentWorkRelease(release(tc.before))
			identifier := randIdentifier()
			var id string
			config := func(name string, n int) string {
				body := fmt.Sprintf("  name = %q", name)
				if tc.block {
					body += fmt.Sprintf("\n  agent_work = {\n    max_in_progress = %d\n  }", n)
				}
				return projectConfig(env, identifier, body)
			}
			// The values state holds before the change, and the ones
			// Flightdeck reports after it.
			check := func(both bool) resource.TestCheckFunc {
				if both {
					return resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "false"),
						resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "false"),
					)
				}
				return resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.auto_merge"),
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.send_ci_failures"),
				)
			}
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{Config: config("Both change", 2), Check: resource.ComposeAggregateTestCheckFunc(captureAttr(projectRes, "id", &id), check(tc.before))},
					{
						// The change lands after the plan, just before the write
						// that touches the project: its agent-work PATCH, or with
						// no block, the project PATCH ahead of the agent-work read.
						PreConfig: func() {
							route := fmt.Sprintf("/api/v1/projects/%s", id)
							if tc.block {
								route += "/agent-work"
							}
							env.fake.OnNextRequest("PATCH", route, func() { env.fake.SetAgentWorkRelease(release(!tc.before)) })
						},
						Config: config("Both change renamed", 3),
						// The plan's values, kept.
						Check: check(tc.before),
					},
					{
						Config: config("Both change renamed", 3),
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
						},
						Check: check(!tc.before),
					},
				},
			})
		})
	}
}

// A project imported from a Flightdeck that has auto_merge but not
// send_ci_failures reads send_ci_failures as null and matches the state the
// apply left. A later change elsewhere plans it as that null, not "known
// after apply", and keeps the settings' lock_version known too.
func TestProjectAgentWork_sendCIFailuresImportedFromAnOlderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkWithAutoMerge)
	identifier := randIdentifier()
	config := func(name string) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  agent_work = {
    auto_merge = true
  }`, name))
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config("Imported"),
				Check:  resource.TestCheckNoResourceAttr(projectRes, "agent_work.send_ci_failures"),
			},
			{
				ResourceName:            projectRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"features"},
			},
			{
				Config: config("Imported renamed"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("agent_work").AtMapKey("send_ci_failures"), knownvalue.Null()),
						plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("agent_work").AtMapKey("auto_merge"), knownvalue.Bool(true)),
						plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("agent_work").AtMapKey("lock_version"), knownvalue.Int64Exact(1)),
					},
				},
			},
		},
	})
}
