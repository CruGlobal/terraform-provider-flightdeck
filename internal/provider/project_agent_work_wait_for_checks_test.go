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

// None of these tests turns agent work on, so no agent ever runs, live or
// not, whatever wait_for_checks says. Unlike the other switches,
// wait_for_checks is on by default, so false is the value that changes
// anything.

// waitForChecksConfig is the project with the given agent_work settings.
func waitForChecksConfig(env *testEnv, identifier, settings string) string {
	return projectConfig(env, identifier, `
  name = "Wait for checks"
  agent_work = {
`+settings+`
  }`)
}

// wait_for_checks is set, imported, turned back on and dropped from the
// configuration like any other agent work setting, and the data source
// reports it. This one runs live too: it creates and destroys its own project,
// and leaves the setting at its default before the destroy.
func TestProjectAgentWork_waitForChecksRoundTrip(t *testing.T) {
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
				Config: waitForChecksConfig(env, identifier, `    wait_for_checks = false`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "false"),
					// wait_for_checks alone does not turn agent work on, and
					// leaves the other switches alone.
					resource.TestCheckResourceAttr(projectRes, "agent_work.enabled", "false"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "false"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "false"),
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
				Config: waitForChecksConfig(env, identifier, `    wait_for_checks = true`) + lookup,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "true"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "2"),
				),
			},
			{
				// Off again, and the data source reads it on the next step's plan.
				Config: waitForChecksConfig(env, identifier, `    wait_for_checks = false`) + lookup,
				Check:  resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "false"),
			},
			{
				// Dropping it from the configuration keeps the stored value and
				// plans nothing.
				Config: waitForChecksConfig(env, identifier, `    max_in_progress = 1`) + lookup,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "false"),
					resource.TestCheckResourceAttr("data.flightdeck_project.lookup", "agent_work.wait_for_checks", "false"),
				),
			},
			{
				// Back to the default before the destroy.
				Config: waitForChecksConfig(env, identifier, `    wait_for_checks = true`),
				Check:  resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "true"),
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
	for i, want := range []any{false, true, false, true} {
		if body := agentWorkBody(t, writes[i]); len(body) != 1 || body["wait_for_checks"] != want {
			t.Errorf("write %d must send wait_for_checks = %v only, got %v", i+1, want, body)
		}
	}
}

// A project created with wait_for_checks = true, the default, sends it: set
// explicitly, Terraform owns it, even though the write changes nothing.
func TestProjectAgentWork_waitForChecksTrueAtCreateIsSent(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	config := waitForChecksConfig(env, identifier, `    wait_for_checks = true`)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "true"),
					// Nothing changed, so no settings row was created.
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "0"),
				),
			},
			{Config: config, PlanOnly: true},
			{
				// Turned off on the settings page: the configuration sets it back.
				PreConfig: func() {
					env.fake.SetAgentWorkOutOfBand(mustInt64(t, id), func(row *flightdecktest.AgentWorkSetting) { row.WaitForChecks = false })
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "true"),
			},
		},
	})
	writes := agentWorkRequests(env.fake.RequestsMatching("PATCH", "/api/v1/projects/"))
	if len(writes) != 2 {
		t.Fatalf("expected two agent-work PATCHes, got %d", len(writes))
	}
	for i, r := range writes {
		if body := agentWorkBody(t, r); len(body) != 1 || body["wait_for_checks"] != true {
			t.Errorf("write %d must send wait_for_checks = true only, got %v", i+1, body)
		}
	}
}

// Unset, wait_for_checks is never sent: state reads whatever the server
// holds, including a value changed on the settings page, and plans nothing
// for it.
func TestProjectAgentWork_waitForChecksUnsetIsNeverSent(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: waitForChecksConfig(env, identifier, `    max_in_progress = 2`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					// The server's default.
					resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "true"),
				),
			},
			{
				// Turned off on the settings page: adopted, nothing planned.
				PreConfig: func() {
					env.fake.SetAgentWorkOutOfBand(mustInt64(t, id), func(row *flightdecktest.AgentWorkSetting) { row.WaitForChecks = false })
				},
				Config: waitForChecksConfig(env, identifier, `    max_in_progress = 2`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "false"),
			},
			{
				// Another setting changes; wait_for_checks stays as the page left it.
				Config: waitForChecksConfig(env, identifier, `    max_in_progress = 3`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "3"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "false"),
					func(*terraform.State) error {
						if row := env.fake.AgentWorkOf(mustInt64(t, id)); row == nil || row.WaitForChecks {
							return fmt.Errorf("the write changed the stored wait_for_checks: %+v", row)
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
			t.Errorf("write %d must send max_in_progress only, never wait_for_checks: %v", i+1, body)
		}
	}
}

// Configured, a value changed on the settings page shows in the next plan and
// is set back, whichever way it was changed.
func TestProjectAgentWork_waitForChecksDrift(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	outOfBand := func(on bool) func() {
		return func() {
			env.fake.SetAgentWorkOutOfBand(mustInt64(t, id), func(row *flightdecktest.AgentWorkSetting) { row.WaitForChecks = on })
		}
	}
	update := resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: waitForChecksConfig(env, identifier, `    wait_for_checks = false`),
				Check:  captureAttr(projectRes, "id", &id),
			},
			{
				PreConfig:        outOfBand(true),
				Config:           waitForChecksConfig(env, identifier, `    wait_for_checks = false`),
				ConfigPlanChecks: update,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "false"),
					// Created at 1, moved by the page to 2, written back at 3.
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "3"),
				),
			},
			{Config: waitForChecksConfig(env, identifier, `    wait_for_checks = true`)},
			{
				PreConfig:        outOfBand(false),
				Config:           waitForChecksConfig(env, identifier, `    wait_for_checks = true`),
				ConfigPlanChecks: update,
				Check:            resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "true"),
			},
			{Config: waitForChecksConfig(env, identifier, `    wait_for_checks = true`), PlanOnly: true},
		},
	})
}

// A Flightdeck older than wait_for_checks (but with auto_merge and
// send_ci_failures, the release before it): a project whose configuration
// leaves it unset works as before (it reads null and is never sent). Setting
// it fails on the attribute, saying the server is too old, saves nothing, and
// applies once Flightdeck is upgraded.
func TestProjectAgentWork_waitForChecksOnAnOlderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkWaitForChecksSupported(false)
	identifier := randIdentifier()
	var id string
	withWaitForChecks := waitForChecksConfig(env, identifier, "    max_in_progress  = 3\n    send_ci_failures = true\n    wait_for_checks  = false")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: waitForChecksConfig(env, identifier, `    max_in_progress = 2`) + `
data "flightdeck_project" "lookup" {
  id = flightdeck_project.test.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "2"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "false"),
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.wait_for_checks"),
					resource.TestCheckNoResourceAttr("data.flightdeck_project.lookup", "agent_work.wait_for_checks"),
				),
			},
			{
				// The error names wait_for_checks, not send_ci_failures, which
				// this Flightdeck knows.
				Config: withWaitForChecks,
				ExpectError: regexMust(`(?s)This Flightdeck does not support agent_work\.wait_for_checks yet.*` +
					`older\s+than\s+the\s+release\s+that\s+added\s+the\s+choice\s+of\s+whether\s+agents\s+wait\s+for\s+CI.*` +
					`Upgrade\s+Flightdeck\s+first.*unknown key: wait_for_checks`),
			},
			{
				// Nothing was saved, max_in_progress and send_ci_failures included.
				PreConfig: func() {
					if row := env.fake.AgentWorkOf(mustInt64(t, id)); row == nil || row.MaxInProgress != 2 || row.SendCIFailures || !row.WaitForChecks {
						t.Errorf("a refused write must save nothing, but the settings are %+v", row)
					}
				},
				Config:             withWaitForChecks,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Flightdeck is upgraded; the same configuration applies.
				PreConfig: func() { env.fake.SetAgentWorkWaitForChecksSupported(true) },
				Config:    withWaitForChecks,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "false"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "true"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "3"),
				),
			},
			{Config: withWaitForChecks, PlanOnly: true},
		},
	})
	writes := agentWorkRequests(env.fake.RequestsMatching("PATCH", "/api/v1/projects/"))
	if len(writes) == 0 {
		t.Fatal("no agent-work PATCH was recorded")
	}
	if _, sent := agentWorkBody(t, writes[0])["wait_for_checks"]; sent {
		t.Errorf("an unset wait_for_checks must not be sent to an older Flightdeck: %v", agentWorkBody(t, writes[0]))
	}
}

// On the apply that creates a project, an older Flightdeck's refusal of
// wait_for_checks is a warning on the attribute, like any refusal of the
// block's settings there: an error would taint the new project. The next
// apply is an update of the same project.
func TestProjectAgentWork_waitForChecksOnAnOlderFlightdeckAtCreateWarns(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkWaitForChecksSupported(false)
	identifier := randIdentifier()
	config := waitForChecksConfig(env, identifier, "    max_in_progress = 2\n    wait_for_checks = false")
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
				PreConfig: func() { env.fake.SetAgentWorkWaitForChecksSupported(true) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "false"),
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
	at := tftypes.NewAttributePath().WithAttributeName("agent_work").WithAttributeName("wait_for_checks")
	if !diagnosticAt(warnings, "This Flightdeck does not support agent_work.wait_for_checks yet", at,
		"The project was created", "unknown key: wait_for_checks") {
		t.Fatalf("no create-time warning on agent_work.wait_for_checks; apply warnings: %s", describeWithPaths(warnings))
	}
	for _, w := range warnings {
		if strings.Contains(w.Summary, "auto_merge") || strings.Contains(w.Summary, "send_ci_failures") ||
			strings.Contains(w.Summary, "send_errors") {
			t.Errorf("only wait_for_checks was sent, so nothing else may warn about it: %s", w.Summary)
		}
	}
}

// How writeAgentWork reports a refused update that sent wait_for_checks,
// alone or with the other newer settings. The read after the refusal, not the
// API's wording, decides which settings the server is too old for, and each
// is reported on its own attribute. When the read fails, the refusal is
// reported as it came, with a note for each newer setting it names as
// unknown; a Flightdeck that knows wait_for_checks and refuses something else
// gets no upgrade hint.
func TestWriteAgentWork_waitForChecksRefusals(t *testing.T) {
	waitPath := path.Root("agent_work").AtName("wait_for_checks")
	blockPath := path.Root("agent_work")
	newProject := func(t *testing.T, env *testEnv) (*client.Client, int64) {
		t.Helper()
		c, err := client.New(env.fake.URL, env.fake.Token())
		if err != nil {
			t.Fatal(err)
		}
		fields := client.Fields{"name": "Wait for checks refusal", "identifier": randIdentifier()}
		p, err := c.CreateProject(t.Context(), fields, client.PayloadKey("project", "", fields))
		if err != nil {
			t.Fatal(err)
		}
		return c, p.ID
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
		writeAgentWork(t.Context(), c, projectID, "WFC", block, types.ObjectNull(agentWorkAttrTypes),
			types.ObjectUnknown(agentWorkAttrTypes), agentWorkOnUpdate, &diags)
		return diags
	}
	wantUnsupported := func(t *testing.T, d diag.Diagnostic, key, added string) {
		t.Helper()
		if got := d.Summary(); got != "This Flightdeck does not support agent_work."+key+" yet" {
			t.Errorf("summary = %q", got)
		}
		if got, want := at(d), path.Root("agent_work").AtName(key); !got.Equal(want) {
			t.Errorf("reported at %s, want %s", got, want)
		}
		for _, want := range []string{"older than the release that added " + added, "Upgrade Flightdeck first",
			"leave `" + key + "` out", "unknown key"} {
			if !strings.Contains(d.Detail(), want) {
				t.Errorf("detail does not carry %q:\n%s", want, d.Detail())
			}
		}
	}

	// A Flightdeck with the other two newer settings refuses the write for
	// wait_for_checks alone; send_ci_failures is not blamed.
	t.Run("older Flightdeck, sent with send_ci_failures", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkWaitForChecksSupported(false)
		c, id := newProject(t, env)
		diags := write(t, c, id, agentWorkModel{WaitForChecks: types.BoolValue(false), SendCIFailures: types.BoolValue(true)})
		errs := diags.Errors()
		if len(errs) != 1 {
			t.Fatalf("expected exactly one error, got %v", diags)
		}
		wantUnsupported(t, errs[0], "wait_for_checks", "the choice of whether agents wait for CI")
		if !strings.Contains(errs[0].Detail(), "unknown key: wait_for_checks") {
			t.Errorf("detail does not carry the API's refusal:\n%s", errs[0].Detail())
		}
		if row := env.fake.AgentWorkOf(id); row != nil {
			t.Errorf("a refused write must save nothing, send_ci_failures included, got %+v", row)
		}
	})

	// A Flightdeck older than all three refuses all three, and each is
	// reported on its own attribute.
	t.Run("Flightdeck older than all three, all sent", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkBeforeAutoMerge)
		c, id := newProject(t, env)
		diags := write(t, c, id, agentWorkModel{AutoMerge: types.BoolValue(false), SendCIFailures: types.BoolValue(false),
			WaitForChecks: types.BoolValue(true)})
		errs := diags.Errors()
		if len(errs) != 3 {
			t.Fatalf("expected exactly three errors, got %v", diags)
		}
		wantUnsupported(t, errs[0], "auto_merge", "auto-merge for agent pull requests")
		wantUnsupported(t, errs[1], "send_ci_failures", "sending CI failures to agents")
		wantUnsupported(t, errs[2], "wait_for_checks", "the choice of whether agents wait for CI")
		for _, e := range errs {
			if !strings.Contains(e.Detail(), "unknown keys: auto_merge, send_ci_failures, wait_for_checks") {
				t.Errorf("detail does not carry the API's refusal:\n%s", e.Detail())
			}
		}
	})

	// A Flightdeck that has send_ci_failures but not wait_for_checks, sent
	// only wait_for_checks: one error, on wait_for_checks.
	t.Run("older Flightdeck, sent alone", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkWithSendCIFailures)
		c, id := newProject(t, env)
		errs := write(t, c, id, agentWorkModel{WaitForChecks: types.BoolValue(false)}).Errors()
		if len(errs) != 1 {
			t.Fatalf("expected exactly one error, got %v", errs)
		}
		wantUnsupported(t, errs[0], "wait_for_checks", "the choice of whether agents wait for CI")
	})

	t.Run("older Flightdeck, the read fails", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkWaitForChecksSupported(false)
		c, id := newProject(t, env)
		readFailsAfterRefusal(env, id)
		diags := write(t, c, id, agentWorkModel{WaitForChecks: types.BoolValue(false)})
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
		detail := errs[0].Detail()
		for _, want := range []string{"unknown key: wait_for_checks", agentWorkWaitForChecksUnknownNote} {
			if !strings.Contains(detail, want) {
				t.Errorf("detail does not carry %q:\n%s", want, detail)
			}
		}
		// auto_merge and send_ci_failures are in the settable list, not refused.
		for _, note := range []string{agentWorkAutoMergeUnknownNote, agentWorkSendCIFailuresUnknownNote} {
			if strings.Contains(detail, note) {
				t.Errorf("a refusal that lists a setting as settable must not carry its note:\n%s", detail)
			}
		}
	})

	t.Run("a Flightdeck with wait_for_checks refuses something else", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		c, id := newProject(t, env)
		// More than the stored daily budget (10), which Flightdeck refuses.
		diags := write(t, c, id, agentWorkModel{WaitForChecks: types.BoolValue(false), TaskMaxUSD: types.Float64Value(20)})
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
			t.Errorf("a Flightdeck that knows wait_for_checks must not be told to upgrade:\n%s", detail)
		}
		if got := at(errs[0]); got.Equal(waitPath) {
			t.Errorf("a refusal of another setting must not land on wait_for_checks")
		}
		if row := env.fake.AgentWorkOf(id); row != nil {
			t.Errorf("a refused write must save nothing, got %+v", row)
		}
	})
}

// refusesAsUnknown and agentWorkUnknownNotes read the keys an unknown-key
// refusal names, in Flightdeck's words, and never the settable list after
// them.
func TestRefusesAsUnknown_waitForChecks(t *testing.T) {
	refusal := func(code, msg string) error {
		return &client.Error{Method: "PATCH", Path: "/projects/4/agent-work", Status: 422, Code: code, Message: msg}
	}
	settable := " (settable: enabled, kinds, auto_merge, send_ci_failures, wait_for_checks, label_id, research_label_id)"
	cases := []struct {
		name                  string
		err                   error
		wait, send, autoMerge bool
	}{
		{"one key", refusal(client.CodeInvalidAttribute, "unknown key: wait_for_checks"+settable), true, false, false},
		{"all three keys", refusal(client.CodeInvalidAttribute, "unknown keys: auto_merge, send_ci_failures, wait_for_checks"+settable), true, true, true},
		{"several, some left out", refusal(client.CodeInvalidAttribute, "unknown keys: alpha, wait_for_checks and 4 more"+settable), true, false, false},
		{"another key, all settable", refusal(client.CodeInvalidAttribute, "unknown key: zebra"+settable), false, false, false},
		{"a type error", refusal(client.CodeInvalidAttribute, `wait_for_checks must be true or false, got "false"`), false, false, false},
		{"a rule", refusal(client.CodeValidationFailed, "unknown key: wait_for_checks"), false, false, false},
		{"not a 422", &client.Error{Status: 409, Code: client.CodeStaleObject, Message: "unknown key: wait_for_checks"}, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := refusesAsUnknown(tc.err, "wait_for_checks"); got != tc.wait {
				t.Errorf("refusesAsUnknown(wait_for_checks) = %t, want %t", got, tc.wait)
			}
			want := ""
			if tc.autoMerge {
				want += "\n\n" + agentWorkAutoMergeUnknownNote
			}
			if tc.send {
				want += "\n\n" + agentWorkSendCIFailuresUnknownNote
			}
			if tc.wait {
				want += "\n\n" + agentWorkWaitForChecksUnknownNote
			}
			if got := agentWorkUnknownNotes(tc.err); got != want {
				t.Errorf("agentWorkUnknownNotes = %q, want %q", got, want)
			}
		})
	}
}

// Flightdeck is upgraded to wait_for_checks, or rolled back below it, between
// the plan and the apply, as a saved plan applied later can see. An unset
// wait_for_checks is never sent, so the apply keeps the value the plan
// promised rather than fail with "Provider produced inconsistent result", and
// the next refresh adopts the server's value without planning anything.
func TestProjectAgentWork_waitForChecksUnsetSurvivesAServerChangeDuringApply(t *testing.T) {
	cases := []struct {
		name string
		// before is whether Flightdeck has wait_for_checks at plan time.
		before bool
		// block is whether the configuration has an agent_work block (one
		// that leaves wait_for_checks unset) or none at all.
		block bool
	}{
		{name: "upgraded, block without wait_for_checks", before: false, block: true},
		{name: "rolled back, block without wait_for_checks", before: true, block: true},
		{name: "upgraded, no block", before: false, block: false},
		{name: "rolled back, no block", before: true, block: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, "agent_work")
			env.requireFake(t)
			env.fake.SetAgentWorkWaitForChecksSupported(tc.before)
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
			// reports after it: the default, true, on a Flightdeck that has it.
			check := func(supported bool) resource.TestCheckFunc {
				if supported {
					return resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "true")
				}
				return resource.TestCheckNoResourceAttr(projectRes, "agent_work.wait_for_checks")
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
							env.fake.OnNextRequest("PATCH", route, func() { env.fake.SetAgentWorkWaitForChecksSupported(!tc.before) })
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

// A configured wait_for_checks gets no such allowance: a Flightdeck rolled
// back below the setting between the plan and the apply refuses the write,
// and the apply fails saying so.
func TestProjectAgentWork_waitForChecksConfiguredMeetsARollbackDuringApply(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: waitForChecksConfig(env, identifier, `    wait_for_checks = true`), Check: captureAttr(projectRes, "id", &id)},
			{
				PreConfig: func() {
					env.fake.OnNextRequest("PATCH", fmt.Sprintf("/api/v1/projects/%s/agent-work", id), func() {
						env.fake.SetAgentWorkWaitForChecksSupported(false)
					})
				},
				Config:      waitForChecksConfig(env, identifier, `    wait_for_checks = false`),
				ExpectError: regexMust(`This Flightdeck does not support agent_work\.wait_for_checks yet`),
			},
		},
	})
}

// keepUnsetAsPlanned changes wait_for_checks in the result only when the
// configuration leaves it unset and the plan knew its value, and never
// touches the other newer settings.
func TestKeepUnsetWaitForChecksAsPlanned(t *testing.T) {
	ctx := t.Context()
	on, off := types.BoolValue(true), types.BoolValue(false)
	obj := func(v types.Bool) types.Object {
		return agentWorkObj(t, &agentWorkModel{WaitForChecks: v, AutoMerge: on, SendCIFailures: on, MaxInProgress: types.Int64Value(3)})
	}
	cases := []struct {
		name                   string
		config, planned, after types.Object
		want                   types.Bool
	}{
		{"unset, the plan had null, the server says true", obj(types.BoolNull()), obj(types.BoolNull()), obj(on), types.BoolNull()},
		{"unset, the plan had true, the server says null", obj(types.BoolNull()), obj(on), obj(types.BoolNull()), on},
		{"no block, the plan had null", types.ObjectNull(agentWorkAttrTypes), obj(types.BoolNull()), obj(on), types.BoolNull()},
		{"unset, the plan did not know", obj(types.BoolNull()), obj(types.BoolUnknown()), obj(on), on},
		{"configured false, the server says true", obj(off), obj(off), obj(on), on},
		{"configured true, the server says null", obj(on), obj(on), obj(types.BoolNull()), types.BoolNull()},
		{"configured but not known until apply", obj(types.BoolUnknown()), obj(types.BoolUnknown()), obj(off), off},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			out := keepUnsetAsPlanned("wait_for_checks", tc.config, tc.planned, tc.after, &diags)
			if diags.HasError() {
				t.Fatalf("errors: %v", diags.Errors())
			}
			var got agentWorkModel
			diags.Append(out.As(ctx, &got, objectAsOptions)...)
			if !got.WaitForChecks.Equal(tc.want) {
				t.Errorf("wait_for_checks = %v, want %v", got.WaitForChecks, tc.want)
			}
			if got.MaxInProgress.ValueInt64() != 3 || !got.AutoMerge.Equal(on) || !got.SendCIFailures.Equal(on) {
				t.Errorf("the rest of the block changed: %+v", got)
			}
		})
	}
	// Each newer setting is corrected on its own: an unset wait_for_checks the
	// server changed is kept as planned while a configured send_ci_failures is
	// left as the server reports it.
	var diags diag.Diagnostics
	config := agentWorkObj(t, &agentWorkModel{SendCIFailures: on, WaitForChecks: types.BoolNull(), MaxInProgress: types.Int64Value(3)})
	planned := agentWorkObj(t, &agentWorkModel{SendCIFailures: on, WaitForChecks: types.BoolNull(), MaxInProgress: types.Int64Value(3)})
	after := agentWorkObj(t, &agentWorkModel{SendCIFailures: off, WaitForChecks: on, MaxInProgress: types.Int64Value(3)})
	out := after
	for _, setting := range agentWorkNewerSettings {
		out = keepUnsetAsPlanned(setting.key, config, planned, out, &diags)
	}
	var got agentWorkModel
	diags.Append(out.As(ctx, &got, objectAsOptions)...)
	if diags.HasError() {
		t.Fatalf("errors: %v", diags.Errors())
	}
	if !got.WaitForChecks.IsNull() || !got.SendCIFailures.Equal(off) {
		t.Errorf("want wait_for_checks kept as planned (null) and send_ci_failures as reported (false), got %v and %v",
			got.WaitForChecks, got.SendCIFailures)
	}
}

// A change elsewhere in the project plans an unset wait_for_checks as the
// value in state, not "known after apply", and so leaves the settings'
// lock_version known too. On an older Flightdeck that value is null.
func TestProjectAgentWork_waitForChecksUnsetStaysKnownOnARename(t *testing.T) {
	for _, supported := range []bool{true, false} {
		t.Run(fmt.Sprintf("Flightdeck has wait_for_checks: %t", supported), func(t *testing.T) {
			env := newTestEnv(t, "agent_work")
			env.requireFake(t)
			env.fake.SetAgentWorkWaitForChecksSupported(supported)
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
				want = knownvalue.Bool(true)
			}
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{Config: config("Known")},
					{
						Config: config("Known renamed"),
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{
								plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate),
								plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("agent_work").AtMapKey("wait_for_checks"), want),
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
// the API's wording: only a 422 to a write that sent wait_for_checks, from a
// Flightdeck whose read has no wait_for_checks, is the server being too old
// for it. agentWorkRefusedSettings lists every newer setting that holds for.
func TestAgentWorkWaitForChecksRefused(t *testing.T) {
	on := true
	knowsAll := &client.AgentWork{AutoMerge: &on, SendCIFailures: &on, WaitForChecks: &on}
	knowsTwo := &client.AgentWork{AutoMerge: &on, SendCIFailures: &on}
	olderThanAll := &client.AgentWork{}
	unknownKey := &client.Error{Method: "PATCH", Path: "/projects/4/agent-work", Status: 422, Code: client.CodeInvalidAttribute,
		Message: "unknown key: wait_for_checks (settable: enabled, kinds, auto_merge, send_ci_failures, label_id, " +
			"accept_machine_labels, research_label_id, agent_account_id, base_ref, max_in_progress, daily_budget_usd, " +
			"task_max_usd, task_max_minutes, queue_minutes, runbook)"}
	stale := &client.Error{Status: 409, Code: client.CodeStaleObject, Message: "wait_for_checks changed"}
	sent := client.Fields{"wait_for_checks": false}
	all := client.Fields{"wait_for_checks": false, "send_ci_failures": true, "auto_merge": false}
	cases := []struct {
		name    string
		current *client.AgentWork
		sent    client.Fields
		err     error
		want    bool
		refused []string
	}{
		{"older Flightdeck, sent", knowsTwo, sent, unknownKey, true, []string{"wait_for_checks"}},
		{"older Flightdeck, sent true", knowsTwo, client.Fields{"wait_for_checks": true}, unknownKey, true, []string{"wait_for_checks"}},
		{"older Flightdeck, sent with the others", knowsTwo, all, unknownKey, true, []string{"wait_for_checks"}},
		{"older than all, all sent", olderThanAll, all, unknownKey, true, []string{"auto_merge", "send_ci_failures", "wait_for_checks"}},
		{"older than all, only send_ci_failures sent", olderThanAll, client.Fields{"send_ci_failures": true}, unknownKey, false, []string{"send_ci_failures"}},
		{"a Flightdeck that knows it", knowsAll, all, unknownKey, false, nil},
		{"not sent", knowsTwo, client.Fields{"max_in_progress": int64(2)}, unknownKey, false, nil},
		{"not a 422", knowsTwo, sent, stale, false, nil},
		{"no read", nil, sent, unknownKey, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentWorkSettingRefused(agentWorkWaitForChecks, tc.current, tc.sent, tc.err); got != tc.want {
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

// Creating a project that sets all three newer settings on a Flightdeck older
// than all of them gives three warnings, one on each attribute, and no error,
// so the new project isn't tainted. Once Flightdeck is upgraded, the next
// apply is an update of the same project.
func TestProjectAgentWork_newerSettingsAtCreateOnAFlightdeckOlderThanAll(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkBeforeAutoMerge)
	identifier := randIdentifier()
	config := waitForChecksConfig(env, identifier,
		"    max_in_progress  = 2\n    auto_merge       = true\n    send_ci_failures = true\n    wait_for_checks  = false")
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config, ExpectNonEmptyPlan: true},
			{
				PreConfig: func() { env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkWithWaitForChecks) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "true"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "true"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "false"),
				),
			},
			{Config: config, PlanOnly: true},
		},
	})
	if errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError); len(errs) != 0 {
		t.Errorf("no apply may fail, got %d error(s): %s", len(errs), describeWithPaths(errs))
	}
	warnings := recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning)
	for _, key := range []string{"auto_merge", "send_ci_failures", "wait_for_checks"} {
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
	if unsupported != 3 {
		t.Errorf("want three warnings about unsupported settings, got %d: %s", unsupported, describeWithPaths(warnings))
	}
}

// Flightdeck is upgraded from below auto_merge to the release with
// wait_for_checks, or rolled back from that release to below auto_merge,
// between the plan and the apply: auto_merge, send_ci_failures and
// wait_for_checks change at once. Left unset, each keeps the value the plan
// promised, and the next refresh adopts the server's for all of them without
// planning anything.
func TestProjectAgentWork_allNewerSettingsUnsetSurviveAServerChangeDuringApply(t *testing.T) {
	release := func(all bool) flightdecktest.AgentWorkRelease {
		if all {
			return flightdecktest.AgentWorkWithWaitForChecks
		}
		return flightdecktest.AgentWorkBeforeAutoMerge
	}
	for _, before := range []bool{false, true} {
		t.Run(fmt.Sprintf("Flightdeck has them at plan time: %t", before), func(t *testing.T) {
			env := newTestEnv(t, "agent_work")
			env.requireFake(t)
			env.fake.SetAgentWorkRelease(release(before))
			identifier := randIdentifier()
			var id string
			config := func(name string, n int) string {
				return projectConfig(env, identifier, fmt.Sprintf("  name = %q\n  agent_work = {\n    max_in_progress = %d\n  }", name, n))
			}
			check := func(all bool) resource.TestCheckFunc {
				if all {
					return resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "false"),
						resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "false"),
						resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "true"),
					)
				}
				return resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.auto_merge"),
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.send_ci_failures"),
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.wait_for_checks"),
				)
			}
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{Config: config("All change", 2), Check: resource.ComposeAggregateTestCheckFunc(captureAttr(projectRes, "id", &id), check(before))},
					{
						PreConfig: func() {
							env.fake.OnNextRequest("PATCH", fmt.Sprintf("/api/v1/projects/%s/agent-work", id), func() {
								env.fake.SetAgentWorkRelease(release(!before))
							})
						},
						Config: config("All change renamed", 3),
						// The plan's values, kept.
						Check: check(before),
					},
					{
						Config: config("All change renamed", 3),
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
						},
						Check: check(!before),
					},
				},
			})
		})
	}
}

// A project imported from a Flightdeck that has send_ci_failures but not
// wait_for_checks reads wait_for_checks as null and matches the state the
// apply left. A later change elsewhere plans it as that null, not "known
// after apply", and keeps the settings' lock_version known too.
func TestProjectAgentWork_waitForChecksImportedFromAnOlderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkWithSendCIFailures)
	identifier := randIdentifier()
	config := func(name string) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  agent_work = {
    send_ci_failures = true
  }`, name))
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config("Imported"),
				Check:  resource.TestCheckNoResourceAttr(projectRes, "agent_work.wait_for_checks"),
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
						plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("agent_work").AtMapKey("wait_for_checks"), knownvalue.Null()),
						plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("agent_work").AtMapKey("send_ci_failures"), knownvalue.Bool(true)),
						plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("agent_work").AtMapKey("lock_version"), knownvalue.Int64Exact(1)),
					},
				},
			},
		},
	})
}
