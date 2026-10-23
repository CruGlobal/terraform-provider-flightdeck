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

// None of these tests turns agent work on, so auto_merge never has a pull
// request to act on, live or not.

// autoMergeConfig is the project with the given agent_work settings.
func autoMergeConfig(env *testEnv, identifier, settings string) string {
	return projectConfig(env, identifier, `
  name = "Auto-merge"
  agent_work = {
`+settings+`
  }`)
}

// auto_merge is set, imported, turned off and dropped from the configuration
// like any other agent work setting, and the data source reports it. This one
// runs live too: it creates and destroys its own project.
func TestProjectAgentWork_autoMergeRoundTrip(t *testing.T) {
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
				Config: autoMergeConfig(env, identifier, `    auto_merge = true`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "true"),
					// auto_merge alone does not turn agent work on.
					resource.TestCheckResourceAttr(projectRes, "agent_work.enabled", "false"),
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
				Config: autoMergeConfig(env, identifier, `    auto_merge = false`) + lookup,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "false"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "2"),
				),
			},
			{
				// Back on, and the data source reads it on the next step's plan.
				Config: autoMergeConfig(env, identifier, `    auto_merge = true`) + lookup,
				Check:  resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "true"),
			},
			{
				// Dropping it from the configuration keeps the stored value and
				// plans nothing.
				Config: autoMergeConfig(env, identifier, `    max_in_progress = 1`) + lookup,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "true"),
					resource.TestCheckResourceAttr("data.flightdeck_project.lookup", "agent_work.auto_merge", "true"),
				),
			},
			{
				// Off again before the destroy.
				Config: autoMergeConfig(env, identifier, `    auto_merge = false`),
				Check:  resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "false"),
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
		if body := agentWorkBody(t, writes[i]); len(body) != 1 || body["auto_merge"] != want {
			t.Errorf("write %d must send auto_merge = %v only, got %v", i+1, want, body)
		}
	}
}

// Unset, auto_merge is never sent: state reads whatever the server holds,
// including a value changed on the settings page, and plans nothing for it.
func TestProjectAgentWork_autoMergeUnsetIsNeverSent(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: autoMergeConfig(env, identifier, `    max_in_progress = 2`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					// The server's default.
					resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "false"),
				),
			},
			{
				// Turned on on the settings page: adopted, nothing planned.
				PreConfig: func() {
					env.fake.SetAgentWorkOutOfBand(mustInt64(t, id), func(row *flightdecktest.AgentWorkSetting) { row.AutoMerge = true })
				},
				Config: autoMergeConfig(env, identifier, `    max_in_progress = 2`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "true"),
			},
			{
				// Another setting changes; auto_merge stays as the page left it.
				Config: autoMergeConfig(env, identifier, `    max_in_progress = 3`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "3"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "true"),
					func(*terraform.State) error {
						if row := env.fake.AgentWorkOf(mustInt64(t, id)); row == nil || !row.AutoMerge {
							return fmt.Errorf("the write changed the stored auto_merge: %+v", row)
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
			t.Errorf("write %d must send max_in_progress only, never auto_merge: %v", i+1, body)
		}
	}
}

// Configured, a value changed on the settings page shows in the next plan and
// is set back, whichever way it was changed.
func TestProjectAgentWork_autoMergeDrift(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	outOfBand := func(on bool) func() {
		return func() {
			env.fake.SetAgentWorkOutOfBand(mustInt64(t, id), func(row *flightdecktest.AgentWorkSetting) { row.AutoMerge = on })
		}
	}
	update := resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: autoMergeConfig(env, identifier, `    auto_merge = true`),
				Check:  captureAttr(projectRes, "id", &id),
			},
			{
				PreConfig:        outOfBand(false),
				Config:           autoMergeConfig(env, identifier, `    auto_merge = true`),
				ConfigPlanChecks: update,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "true"),
					// Created at 1, moved by the page to 2, written back at 3.
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "3"),
				),
			},
			{Config: autoMergeConfig(env, identifier, `    auto_merge = false`)},
			{
				PreConfig:        outOfBand(true),
				Config:           autoMergeConfig(env, identifier, `    auto_merge = false`),
				ConfigPlanChecks: update,
				Check:            resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "false"),
			},
			{Config: autoMergeConfig(env, identifier, `    auto_merge = false`), PlanOnly: true},
		},
	})
}

// A Flightdeck older than auto_merge: a project whose configuration leaves it
// unset works as before (it reads null and is never sent). Setting it fails
// on the attribute, saying the server is too old, saves nothing, and applies
// once Flightdeck is upgraded.
func TestProjectAgentWork_autoMergeOnAnOlderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkAutoMergeSupported(false)
	identifier := randIdentifier()
	var id string
	withAutoMerge := autoMergeConfig(env, identifier, "    max_in_progress = 3\n    auto_merge      = true")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: autoMergeConfig(env, identifier, `    max_in_progress = 2`) + `
data "flightdeck_project" "lookup" {
  id = flightdeck_project.test.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "2"),
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.auto_merge"),
					resource.TestCheckNoResourceAttr("data.flightdeck_project.lookup", "agent_work.auto_merge"),
				),
			},
			{
				Config: withAutoMerge,
				ExpectError: regexMust(`(?s)This Flightdeck does not support agent_work\.auto_merge yet.*` +
					`older\s+than\s+the\s+release\s+that\s+added\s+auto-merge.*Upgrade\s+Flightdeck\s+first.*` +
					`unknown key: auto_merge`),
			},
			{
				// Nothing was saved, max_in_progress included.
				PreConfig: func() {
					if row := env.fake.AgentWorkOf(mustInt64(t, id)); row == nil || row.MaxInProgress != 2 {
						t.Errorf("a refused write must save nothing, but the settings are %+v", row)
					}
				},
				Config:             withAutoMerge,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Flightdeck is upgraded; the same configuration applies.
				PreConfig: func() { env.fake.SetAgentWorkAutoMergeSupported(true) },
				Config:    withAutoMerge,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "true"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "3"),
				),
			},
			{Config: withAutoMerge, PlanOnly: true},
		},
	})
	writes := agentWorkRequests(env.fake.RequestsMatching("PATCH", "/api/v1/projects/"))
	if len(writes) == 0 {
		t.Fatal("no agent-work PATCH was recorded")
	}
	if _, sent := agentWorkBody(t, writes[0])["auto_merge"]; sent {
		t.Errorf("an unset auto_merge must not be sent to an older Flightdeck: %v", agentWorkBody(t, writes[0]))
	}
}

// On the apply that creates a project, an older Flightdeck's refusal of
// auto_merge is a warning on the attribute, like any refusal of the block's
// settings there: an error would taint the new project. The next apply is an
// update of the same project.
func TestProjectAgentWork_autoMergeOnAnOlderFlightdeckAtCreateWarns(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkAutoMergeSupported(false)
	identifier := randIdentifier()
	config := autoMergeConfig(env, identifier, "    max_in_progress = 2\n    auto_merge      = true")
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
				PreConfig: func() { env.fake.SetAgentWorkAutoMergeSupported(true) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "true"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "2"),
				),
			},
			{Config: config, PlanOnly: true},
		},
	})
	if errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError); len(errs) != 0 {
		t.Errorf("no apply may fail, got %d error(s): %s", len(errs), errs[0].Summary)
	}
	at := tftypes.NewAttributePath().WithAttributeName("agent_work").WithAttributeName("auto_merge")
	if !diagnosticAt(recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning),
		"This Flightdeck does not support agent_work.auto_merge yet", at, "The project was created", "unknown key: auto_merge") {
		t.Fatalf("no create-time warning on agent_work.auto_merge; apply warnings: %s",
			describeWithPaths(recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning)))
	}
	// This Flightdeck is older than send_ci_failures, wait_for_checks and
	// send_errors too, but the configuration sets none of them, so nothing may
	// warn about them.
	for _, w := range recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning) {
		for _, key := range []string{"send_ci_failures", "wait_for_checks", "send_errors"} {
			if strings.Contains(w.Summary, key) {
				t.Errorf("%s was not sent, so nothing may warn about it: %s", key, w.Summary)
			}
		}
	}
}

// How writeAgentWork reports a refused update that sent auto_merge. The read
// after the refusal, not the API's wording, decides whether the server is too
// old: an older Flightdeck's refusal lists research_label_id among the
// settable keys, and must still land on auto_merge rather than be taken for a
// refusal of the research label. When the read fails, the refusal is reported
// as it came with a note on what an unknown key means; a Flightdeck that knows
// auto_merge and refuses something else gets no upgrade hint.
func TestWriteAgentWork_autoMergeRefusals(t *testing.T) {
	autoMergePath := path.Root("agent_work").AtName("auto_merge")
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
		projectID, other := create("Auto-merge refusal"), create("Another project")
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
	researchPath := path.Root("agent_work").AtName("research_label_id")
	blockPath := path.Root("agent_work")
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
		writeAgentWork(t.Context(), c, projectID, "AUTO", block, types.ObjectNull(agentWorkAttrTypes),
			types.ObjectUnknown(agentWorkAttrTypes), agentWorkOnUpdate, &diags)
		return diags
	}

	t.Run("older Flightdeck, sent with the research label", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkAutoMergeSupported(false)
		c, id, research, _ := newProject(t, env)
		diags := write(t, c, id, agentWorkModel{AutoMerge: types.BoolValue(true), ResearchLabelID: types.Int64Value(research)})
		errs := diags.Errors()
		if len(errs) != 1 {
			t.Fatalf("expected exactly one error, got %v", diags)
		}
		if got := errs[0].Summary(); got != "This Flightdeck does not support agent_work.auto_merge yet" {
			t.Errorf("summary = %q", got)
		}
		if withPath, ok := errs[0].(diag.DiagnosticWithPath); !ok || !withPath.Path().Equal(autoMergePath) {
			t.Errorf("the error is not reported against agent_work.auto_merge: %#v", errs[0])
		}
		for _, want := range []string{"Upgrade Flightdeck first", "leave `auto_merge` out", "unknown key: auto_merge"} {
			if !strings.Contains(errs[0].Detail(), want) {
				t.Errorf("detail does not carry %q:\n%s", want, errs[0].Detail())
			}
		}
	})

	t.Run("older Flightdeck, the read fails", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkAutoMergeSupported(false)
		c, id, _, _ := newProject(t, env)
		readFailsAfterRefusal(env, id)
		diags := write(t, c, id, agentWorkModel{AutoMerge: types.BoolValue(true)})
		errs := diags.Errors()
		if len(errs) != 1 {
			t.Fatalf("expected exactly one error, got %v", diags)
		}
		if got := errs[0].Summary(); got != "Flightdeck refused the agent work settings" {
			t.Errorf("summary = %q", got)
		}
		for _, want := range []string{"unknown key: auto_merge", agentWorkAutoMergeUnknownNote} {
			if !strings.Contains(errs[0].Detail(), want) {
				t.Errorf("detail does not carry %q:\n%s", want, errs[0].Detail())
			}
		}
	})

	t.Run("a Flightdeck with auto_merge refuses something else", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		c, id, _, _ := newProject(t, env)
		// More than the stored daily budget (10), which Flightdeck refuses.
		diags := write(t, c, id, agentWorkModel{AutoMerge: types.BoolValue(true), TaskMaxUSD: types.Float64Value(20)})
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
			t.Errorf("a Flightdeck that knows auto_merge must not be told to upgrade:\n%s", detail)
		}
		if row := env.fake.AgentWorkOf(id); row != nil {
			t.Errorf("a refused write must save nothing, got %+v", row)
		}
	})

	// The refusal says the research label is the problem, so it lands there
	// whatever the read after it did, and nothing says to upgrade.
	for _, readFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("a Flightdeck with auto_merge refuses the research label, read fails %t", readFails), func(t *testing.T) {
			env := newTestEnv(t, "agent_work")
			env.requireFake(t)
			c, id, _, foreign := newProject(t, env)
			if readFails {
				readFailsAfterRefusal(env, id)
			}
			diags := write(t, c, id, agentWorkModel{AutoMerge: types.BoolValue(true), ResearchLabelID: types.Int64Value(foreign)})
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
			detail := errs[0].Detail()
			if !strings.Contains(detail, "Research label must be a label in this project") {
				t.Errorf("detail does not carry the API's refusal:\n%s", detail)
			}
			if strings.Contains(detail, agentWorkAutoMergeUnknownNote) {
				t.Errorf("a refusal that does not name auto_merge must not carry the auto_merge note:\n%s", detail)
			}
		})
	}

	// An older Flightdeck's refusal of auto_merge lists research_label_id among
	// the settable keys. When the read cannot tell, that list must not pass
	// for a refusal of the research label, and the refusal's own words, which
	// name auto_merge as unknown, bring the upgrade note.
	t.Run("older Flightdeck, sent with the research label, the read fails", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkAutoMergeSupported(false)
		c, id, research, _ := newProject(t, env)
		readFailsAfterRefusal(env, id)
		diags := write(t, c, id, agentWorkModel{AutoMerge: types.BoolValue(true), ResearchLabelID: types.Int64Value(research)})
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
		for _, want := range []string{"unknown key: auto_merge", agentWorkAutoMergeUnknownNote} {
			if !strings.Contains(errs[0].Detail(), want) {
				t.Errorf("detail does not carry %q:\n%s", want, errs[0].Detail())
			}
		}
	})
}

// refusalUnknownKeys reads the keys an unknown-key refusal names, in
// Flightdeck's words, and never the settable list after them.
func TestRefusalUnknownKeys(t *testing.T) {
	refusal := func(code, msg string) error {
		return &client.Error{Method: "PATCH", Path: "/projects/4/agent-work", Status: 422, Code: code, Message: msg}
	}
	settable := " (settable: enabled, kinds, auto_merge, label_id, research_label_id)"
	cases := []struct {
		name      string
		err       error
		want      string
		autoMerge bool
	}{
		{"one key", refusal(client.CodeInvalidAttribute, "unknown key: auto_merge"+settable), "auto_merge", true},
		{"several keys", refusal(client.CodeInvalidAttribute, "unknown keys: auto_merge, zebra"+settable), "auto_merge|zebra", true},
		{"several, some left out", refusal(client.CodeInvalidAttribute, "unknown keys: alpha, auto_merge and 4 more"+settable), "alpha|auto_merge", true},
		{"another key, auto_merge settable", refusal(client.CodeInvalidAttribute, "unknown key: zebra"+settable), "zebra", false},
		{"a type error", refusal(client.CodeInvalidAttribute, `auto_merge must be true or false, got "true"`), "", false},
		{"a rule", refusal(client.CodeValidationFailed, "unknown key: auto_merge"), "", false},
		{"not a 422", &client.Error{Status: 409, Code: client.CodeStaleObject, Message: "unknown key: auto_merge"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(refusalUnknownKeys(tc.err), "|"); got != tc.want {
				t.Errorf("refusalUnknownKeys = %q, want %q", got, tc.want)
			}
			if got := refusesAsUnknown(tc.err, "auto_merge"); got != tc.autoMerge {
				t.Errorf("refusesAsUnknown(auto_merge) = %t, want %t", got, tc.autoMerge)
			}
		})
	}
}

// Flightdeck is upgraded to auto_merge, or rolled back below it, between the
// plan and the apply, as a saved plan applied later can see. An unset
// auto_merge is never sent, so the apply keeps the value the plan promised
// rather than fail with "Provider produced inconsistent result", and the next
// refresh adopts the server's value without planning anything.
func TestProjectAgentWork_autoMergeUnsetSurvivesAServerChangeDuringApply(t *testing.T) {
	cases := []struct {
		name string
		// before is whether Flightdeck has auto_merge at plan time.
		before bool
		// block is whether the configuration has an agent_work block (one
		// that leaves auto_merge unset) or none at all.
		block bool
	}{
		{name: "upgraded, block without auto_merge", before: false, block: true},
		{name: "rolled back, block without auto_merge", before: true, block: true},
		{name: "upgraded, no block", before: false, block: false},
		{name: "rolled back, no block", before: true, block: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, "agent_work")
			env.requireFake(t)
			env.fake.SetAgentWorkAutoMergeSupported(tc.before)
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
					return resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "false")
				}
				return resource.TestCheckNoResourceAttr(projectRes, "agent_work.auto_merge")
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
							env.fake.OnNextRequest("PATCH", route, func() { env.fake.SetAgentWorkAutoMergeSupported(!tc.before) })
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

// A configured auto_merge gets no such allowance: a Flightdeck rolled back
// below the setting between the plan and the apply refuses the write, and the
// apply fails saying so.
func TestProjectAgentWork_autoMergeConfiguredMeetsARollbackDuringApply(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: autoMergeConfig(env, identifier, `    auto_merge = false`), Check: captureAttr(projectRes, "id", &id)},
			{
				PreConfig: func() {
					env.fake.OnNextRequest("PATCH", fmt.Sprintf("/api/v1/projects/%s/agent-work", id), func() {
						env.fake.SetAgentWorkAutoMergeSupported(false)
					})
				},
				Config:      autoMergeConfig(env, identifier, `    auto_merge = true`),
				ExpectError: regexMust(`This Flightdeck does not support agent_work\.auto_merge yet`),
			},
		},
	})
}

// keepUnsetAsPlanned changes auto_merge in the result only when the
// configuration leaves it unset and the plan knew its value.
func TestKeepUnsetAutoMergeAsPlanned(t *testing.T) {
	ctx := t.Context()
	on, off := types.BoolValue(true), types.BoolValue(false)
	obj := func(v types.Bool) types.Object {
		return agentWorkObj(t, &agentWorkModel{AutoMerge: v, MaxInProgress: types.Int64Value(3)})
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
			out := keepUnsetAsPlanned("auto_merge", tc.config, tc.planned, tc.after, &diags)
			if diags.HasError() {
				t.Fatalf("errors: %v", diags.Errors())
			}
			var got agentWorkModel
			diags.Append(out.As(ctx, &got, objectAsOptions)...)
			if !got.AutoMerge.Equal(tc.want) {
				t.Errorf("auto_merge = %v, want %v", got.AutoMerge, tc.want)
			}
			if got.MaxInProgress.ValueInt64() != 3 {
				t.Errorf("the rest of the block changed: %+v", got)
			}
		})
	}
	// A failed write leaves no block, and stays that way.
	var diags diag.Diagnostics
	if out := keepUnsetAsPlanned("auto_merge", obj(types.BoolNull()), obj(off), types.ObjectNull(agentWorkAttrTypes), &diags); !out.IsNull() {
		t.Fatalf("a null result must stay null, got %v", out)
	}
}

// A change elsewhere in the project plans an unset auto_merge as the value in
// state, not "known after apply", and so leaves the settings' lock_version
// known too. On an older Flightdeck that value is null.
func TestProjectAgentWork_autoMergeUnsetStaysKnownOnARename(t *testing.T) {
	for _, supported := range []bool{true, false} {
		t.Run(fmt.Sprintf("Flightdeck has auto_merge: %t", supported), func(t *testing.T) {
			env := newTestEnv(t, "agent_work")
			env.requireFake(t)
			env.fake.SetAgentWorkAutoMergeSupported(supported)
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
								plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("agent_work").AtMapKey("auto_merge"), want),
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
// the API's wording: only a 422 to a write that sent auto_merge, from a
// Flightdeck whose read has no auto_merge, is the server being too old.
func TestAgentWorkAutoMergeRefused(t *testing.T) {
	on := true
	knows := &client.AgentWork{AutoMerge: &on}
	older := &client.AgentWork{}
	unknownKey := &client.Error{Method: "PATCH", Path: "/projects/4/agent-work", Status: 422, Code: client.CodeInvalidAttribute,
		Message: "unknown key: auto_merge (settable: enabled, kinds, label_id, accept_machine_labels, research_label_id, " +
			"agent_account_id, base_ref, max_in_progress, daily_budget_usd, task_max_usd, task_max_minutes, queue_minutes, runbook)"}
	stale := &client.Error{Status: 409, Code: client.CodeStaleObject, Message: "auto_merge changed"}
	sent := client.Fields{"auto_merge": true}
	cases := []struct {
		name    string
		current *client.AgentWork
		sent    client.Fields
		err     error
		want    bool
	}{
		{"older Flightdeck, sent", older, sent, unknownKey, true},
		{"older Flightdeck, sent false", older, client.Fields{"auto_merge": false}, unknownKey, true},
		{"a Flightdeck that knows it", knows, sent, unknownKey, false},
		{"not sent", older, client.Fields{"max_in_progress": int64(2)}, unknownKey, false},
		{"not a 422", older, sent, stale, false},
		{"no read", nil, sent, unknownKey, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentWorkSettingRefused(agentWorkAutoMerge, tc.current, tc.sent, tc.err); got != tc.want {
				t.Fatalf("got %t, want %t", got, tc.want)
			}
		})
	}
}
