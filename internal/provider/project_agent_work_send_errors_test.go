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

// None of these tests turns agent work on or ticks fix-error, so send_errors
// never has an item to send, live or not.

// sendErrorsConfig is the project with the given agent_work settings.
func sendErrorsConfig(env *testEnv, identifier, settings string) string {
	return projectConfig(env, identifier, `
  name = "Send errors"
  agent_work = {
`+settings+`
  }`)
}

// send_errors is set, imported, turned off and dropped from the configuration
// like any other agent work setting, and the data source reports it. Only
// send_errors is ever sent, never the read's send_errors_since. This one runs
// live too: it creates and destroys its own project.
func TestProjectAgentWork_sendErrorsRoundTrip(t *testing.T) {
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
				Config: sendErrorsConfig(env, identifier, `    send_errors = true`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "true"),
					// send_errors alone does not turn agent work on, and leaves
					// the other switches alone.
					resource.TestCheckResourceAttr(projectRes, "agent_work.enabled", "false"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "false"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "false"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "true"),
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
				Config: sendErrorsConfig(env, identifier, `    send_errors = false`) + lookup,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "false"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "2"),
				),
			},
			{
				// Back on, and the data source reads it on the next step's plan.
				Config: sendErrorsConfig(env, identifier, `    send_errors = true`) + lookup,
				Check:  resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "true"),
			},
			{
				// Dropping it from the configuration keeps the stored value and
				// plans nothing.
				Config: sendErrorsConfig(env, identifier, `    max_in_progress = 1`) + lookup,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "true"),
					resource.TestCheckResourceAttr("data.flightdeck_project.lookup", "agent_work.send_errors", "true"),
				),
			},
			{
				// Off again before the destroy.
				Config: sendErrorsConfig(env, identifier, `    send_errors = false`),
				Check:  resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "false"),
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
		if body := agentWorkBody(t, writes[i]); len(body) != 1 || body["send_errors"] != want {
			t.Errorf("write %d must send send_errors = %v only, got %v", i+1, want, body)
		}
	}
}

// Unset, send_errors is never sent: state reads whatever the server holds,
// including a value changed on the settings page, and plans nothing for it.
func TestProjectAgentWork_sendErrorsUnsetIsNeverSent(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: sendErrorsConfig(env, identifier, `    max_in_progress = 2`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					// The server's default.
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "false"),
				),
			},
			{
				// Turned on on the settings page: adopted, nothing planned.
				PreConfig: func() {
					env.fake.SetAgentWorkOutOfBand(mustInt64(t, id), func(row *flightdecktest.AgentWorkSetting) { row.SendErrors = true })
				},
				Config: sendErrorsConfig(env, identifier, `    max_in_progress = 2`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "true"),
			},
			{
				// Another setting changes; send_errors stays as the page left it,
				// and so does the time the page turned it on.
				Config: sendErrorsConfig(env, identifier, `    max_in_progress = 3`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "3"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "true"),
					func(*terraform.State) error {
						if row := env.fake.AgentWorkOf(mustInt64(t, id)); row == nil || !row.SendErrors || row.SendErrorsSince == nil {
							return fmt.Errorf("the write changed the stored send_errors: %+v", row)
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
			t.Errorf("write %d must send max_in_progress only, never send_errors: %v", i+1, body)
		}
	}
}

// Configured, a value changed on the settings page shows in the next plan and
// is set back, whichever way it was changed.
func TestProjectAgentWork_sendErrorsDrift(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	outOfBand := func(on bool) func() {
		return func() {
			env.fake.SetAgentWorkOutOfBand(mustInt64(t, id), func(row *flightdecktest.AgentWorkSetting) { row.SendErrors = on })
		}
	}
	update := resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: sendErrorsConfig(env, identifier, `    send_errors = true`),
				Check:  captureAttr(projectRes, "id", &id),
			},
			{
				PreConfig:        outOfBand(false),
				Config:           sendErrorsConfig(env, identifier, `    send_errors = true`),
				ConfigPlanChecks: update,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "true"),
					// Created at 1, moved by the page to 2, written back at 3.
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "3"),
				),
			},
			{Config: sendErrorsConfig(env, identifier, `    send_errors = false`)},
			{
				PreConfig:        outOfBand(true),
				Config:           sendErrorsConfig(env, identifier, `    send_errors = false`),
				ConfigPlanChecks: update,
				Check:            resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "false"),
			},
			{Config: sendErrorsConfig(env, identifier, `    send_errors = false`), PlanOnly: true},
		},
	})
}

// A Flightdeck older than send_errors (but with the other three newer
// settings, the release before it): a project whose configuration leaves it
// unset works as before (it reads null and is never sent). Setting it fails
// on the attribute, saying the server is too old, saves nothing, and applies
// once Flightdeck is upgraded.
func TestProjectAgentWork_sendErrorsOnAnOlderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkSendErrorsSupported(false)
	identifier := randIdentifier()
	var id string
	withSendErrors := sendErrorsConfig(env, identifier, "    max_in_progress = 3\n    wait_for_checks = false\n    send_errors     = true")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: sendErrorsConfig(env, identifier, `    max_in_progress = 2`) + `
data "flightdeck_project" "lookup" {
  id = flightdeck_project.test.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "2"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "true"),
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.send_errors"),
					resource.TestCheckNoResourceAttr("data.flightdeck_project.lookup", "agent_work.send_errors"),
				),
			},
			{
				// The error names send_errors, not wait_for_checks, which this
				// Flightdeck knows.
				Config: withSendErrors,
				ExpectError: regexMust(`(?s)This Flightdeck does not support agent_work\.send_errors yet.*` +
					`older\s+than\s+the\s+release\s+that\s+added\s+sending\s+errors\s+to\s+agents.*` +
					`Upgrade\s+Flightdeck\s+first.*unknown key: send_errors`),
			},
			{
				// Nothing was saved, max_in_progress and wait_for_checks included.
				PreConfig: func() {
					if row := env.fake.AgentWorkOf(mustInt64(t, id)); row == nil || row.MaxInProgress != 2 || !row.WaitForChecks || row.SendErrors {
						t.Errorf("a refused write must save nothing, but the settings are %+v", row)
					}
				},
				Config:             withSendErrors,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Flightdeck is upgraded; the same configuration applies.
				PreConfig: func() { env.fake.SetAgentWorkSendErrorsSupported(true) },
				Config:    withSendErrors,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "true"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "false"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "3"),
				),
			},
			{Config: withSendErrors, PlanOnly: true},
		},
	})
	writes := agentWorkRequests(env.fake.RequestsMatching("PATCH", "/api/v1/projects/"))
	if len(writes) == 0 {
		t.Fatal("no agent-work PATCH was recorded")
	}
	if _, sent := agentWorkBody(t, writes[0])["send_errors"]; sent {
		t.Errorf("an unset send_errors must not be sent to an older Flightdeck: %v", agentWorkBody(t, writes[0]))
	}
}

// On the apply that creates a project, an older Flightdeck's refusal of
// send_errors is a warning on the attribute, like any refusal of the block's
// settings there: an error would taint the new project. The next apply is an
// update of the same project.
func TestProjectAgentWork_sendErrorsOnAnOlderFlightdeckAtCreateWarns(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkSendErrorsSupported(false)
	identifier := randIdentifier()
	config := sendErrorsConfig(env, identifier, "    max_in_progress = 2\n    send_errors     = true")
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
				PreConfig: func() { env.fake.SetAgentWorkSendErrorsSupported(true) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "true"),
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
	at := tftypes.NewAttributePath().WithAttributeName("agent_work").WithAttributeName("send_errors")
	if !diagnosticAt(warnings, "This Flightdeck does not support agent_work.send_errors yet", at,
		"The project was created", "unknown key: send_errors") {
		t.Fatalf("no create-time warning on agent_work.send_errors; apply warnings: %s", describeWithPaths(warnings))
	}
	for _, w := range warnings {
		for _, key := range []string{"auto_merge", "send_ci_failures", "wait_for_checks"} {
			if strings.Contains(w.Summary, key) {
				t.Errorf("only send_errors was sent, so nothing may warn about %s: %s", key, w.Summary)
			}
		}
	}
}

// How writeAgentWork reports a refused update that sent send_errors, alone or
// with the other newer settings. The read after the refusal, not the API's
// wording, decides which settings the server is too old for, and each is
// reported on its own attribute, in the API's order. When the read fails, the
// refusal is reported as it came, with a note for each newer setting it names
// as unknown; a Flightdeck that knows send_errors and refuses something else
// gets no upgrade hint.
func TestWriteAgentWork_sendErrorsRefusals(t *testing.T) {
	sendPath := path.Root("agent_work").AtName("send_errors")
	blockPath := path.Root("agent_work")
	newProject := func(t *testing.T, env *testEnv) (*client.Client, int64) {
		t.Helper()
		c, err := client.New(env.fake.URL, env.fake.Token())
		if err != nil {
			t.Fatal(err)
		}
		fields := client.Fields{"name": "Send errors refusal", "identifier": randIdentifier()}
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
		writeAgentWork(t.Context(), c, projectID, "SER", block, types.ObjectNull(agentWorkAttrTypes),
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

	// A Flightdeck with the other three newer settings refuses the write for
	// send_errors alone; wait_for_checks is not blamed.
	t.Run("older Flightdeck, sent with wait_for_checks", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkSendErrorsSupported(false)
		c, id := newProject(t, env)
		diags := write(t, c, id, agentWorkModel{SendErrors: types.BoolValue(true), WaitForChecks: types.BoolValue(false)})
		errs := diags.Errors()
		if len(errs) != 1 {
			t.Fatalf("expected exactly one error, got %v", diags)
		}
		wantUnsupported(t, errs[0], "send_errors", "sending errors to agents")
		if !strings.Contains(errs[0].Detail(), "unknown key: send_errors") {
			t.Errorf("detail does not carry the API's refusal:\n%s", errs[0].Detail())
		}
		if row := env.fake.AgentWorkOf(id); row != nil {
			t.Errorf("a refused write must save nothing, wait_for_checks included, got %+v", row)
		}
	})

	// A Flightdeck older than all four refuses all four, and each is reported
	// on its own attribute, in the API's order, which puts send_errors before
	// wait_for_checks although it came later.
	t.Run("Flightdeck older than all four, all sent", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkBeforeAutoMerge)
		c, id := newProject(t, env)
		diags := write(t, c, id, agentWorkModel{AutoMerge: types.BoolValue(false), SendCIFailures: types.BoolValue(false),
			SendErrors: types.BoolValue(false), WaitForChecks: types.BoolValue(true)})
		errs := diags.Errors()
		if len(errs) != 4 {
			t.Fatalf("expected exactly four errors, got %v", diags)
		}
		wantUnsupported(t, errs[0], "auto_merge", "auto-merge for agent pull requests")
		wantUnsupported(t, errs[1], "send_ci_failures", "sending CI failures to agents")
		wantUnsupported(t, errs[2], "send_errors", "sending errors to agents")
		wantUnsupported(t, errs[3], "wait_for_checks", "the choice of whether agents wait for CI")
		for _, e := range errs {
			if !strings.Contains(e.Detail(), "unknown keys: auto_merge, send_ci_failures, send_errors, wait_for_checks") {
				t.Errorf("detail does not carry the API's refusal:\n%s", e.Detail())
			}
		}
	})

	// A Flightdeck that has send_ci_failures but neither wait_for_checks nor
	// send_errors, sent both: two errors, one on each.
	t.Run("Flightdeck older than both of the last two, both sent", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkWithSendCIFailures)
		c, id := newProject(t, env)
		errs := write(t, c, id, agentWorkModel{SendErrors: types.BoolValue(true), WaitForChecks: types.BoolValue(false),
			SendCIFailures: types.BoolValue(true)}).Errors()
		if len(errs) != 2 {
			t.Fatalf("expected exactly two errors, got %v", errs)
		}
		wantUnsupported(t, errs[0], "send_errors", "sending errors to agents")
		wantUnsupported(t, errs[1], "wait_for_checks", "the choice of whether agents wait for CI")
	})

	t.Run("older Flightdeck, the read fails", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkSendErrorsSupported(false)
		c, id := newProject(t, env)
		readFailsAfterRefusal(env, id)
		diags := write(t, c, id, agentWorkModel{SendErrors: types.BoolValue(true)})
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
		for _, want := range []string{"unknown key: send_errors", agentWorkSendErrorsUnknownNote} {
			if !strings.Contains(detail, want) {
				t.Errorf("detail does not carry %q:\n%s", want, detail)
			}
		}
		// The other three are in the settable list, not refused.
		for _, note := range []string{agentWorkAutoMergeUnknownNote, agentWorkSendCIFailuresUnknownNote, agentWorkWaitForChecksUnknownNote} {
			if strings.Contains(detail, note) {
				t.Errorf("a refusal that lists a setting as settable must not carry its note:\n%s", detail)
			}
		}
	})

	t.Run("a Flightdeck with send_errors refuses something else", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		c, id := newProject(t, env)
		// More than the stored daily budget (10), which Flightdeck refuses.
		diags := write(t, c, id, agentWorkModel{SendErrors: types.BoolValue(true), TaskMaxUSD: types.Float64Value(20)})
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
			t.Errorf("a Flightdeck that knows send_errors must not be told to upgrade:\n%s", detail)
		}
		if got := at(errs[0]); got.Equal(sendPath) {
			t.Errorf("a refusal of another setting must not land on send_errors")
		}
		if row := env.fake.AgentWorkOf(id); row != nil {
			t.Errorf("a refused write must save nothing, got %+v", row)
		}
	})
}

// refusesAsUnknown and agentWorkUnknownNotes read the keys an unknown-key
// refusal names, in Flightdeck's words, and never the settable list after
// them. send_errors is a prefix of send_errors_since, so a refusal of the
// read-only key alone is not a refusal of the setting.
func TestRefusesAsUnknown_sendErrors(t *testing.T) {
	refusal := func(code, msg string) error {
		return &client.Error{Method: "PATCH", Path: "/projects/4/agent-work", Status: 422, Code: code, Message: msg}
	}
	settable := " (settable: enabled, kinds, auto_merge, send_ci_failures, send_errors, wait_for_checks, label_id, research_label_id)"
	cases := []struct {
		name                          string
		err                           error
		send, wait, sendCI, autoMerge bool
	}{
		{"one key", refusal(client.CodeInvalidAttribute, "unknown key: send_errors"+settable), true, false, false, false},
		{"all four keys", refusal(client.CodeInvalidAttribute, "unknown keys: auto_merge, send_ci_failures, send_errors, wait_for_checks"+settable),
			true, true, true, true},
		{"the last two", refusal(client.CodeInvalidAttribute, "unknown keys: send_errors, wait_for_checks"+settable), true, true, false, false},
		{"several, some left out", refusal(client.CodeInvalidAttribute, "unknown keys: alpha, send_errors and 4 more"+settable), true, false, false, false},
		{"only the read-only time", refusal(client.CodeInvalidAttribute, "unknown key: send_errors_since"+settable), false, false, false, false},
		{"another key, all settable", refusal(client.CodeInvalidAttribute, "unknown key: zebra"+settable), false, false, false, false},
		{"a type error", refusal(client.CodeInvalidAttribute, `send_errors must be true or false, got "true"`), false, false, false, false},
		{"a rule", refusal(client.CodeValidationFailed, "unknown key: send_errors"), false, false, false, false},
		{"not a 422", &client.Error{Status: 409, Code: client.CodeStaleObject, Message: "unknown key: send_errors"}, false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := refusesAsUnknown(tc.err, "send_errors"); got != tc.send {
				t.Errorf("refusesAsUnknown(send_errors) = %t, want %t", got, tc.send)
			}
			want := ""
			if tc.autoMerge {
				want += "\n\n" + agentWorkAutoMergeUnknownNote
			}
			if tc.sendCI {
				want += "\n\n" + agentWorkSendCIFailuresUnknownNote
			}
			if tc.send {
				want += "\n\n" + agentWorkSendErrorsUnknownNote
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

// Flightdeck is upgraded to send_errors, or rolled back below it, between the
// plan and the apply, as a saved plan applied later can see. An unset
// send_errors is never sent, so the apply keeps the value the plan promised
// rather than fail with "Provider produced inconsistent result", and the next
// refresh adopts the server's value without planning anything.
func TestProjectAgentWork_sendErrorsUnsetSurvivesAServerChangeDuringApply(t *testing.T) {
	cases := []struct {
		name string
		// before is whether Flightdeck has send_errors at plan time.
		before bool
		// block is whether the configuration has an agent_work block (one
		// that leaves send_errors unset) or none at all.
		block bool
	}{
		{name: "upgraded, block without send_errors", before: false, block: true},
		{name: "rolled back, block without send_errors", before: true, block: true},
		{name: "upgraded, no block", before: false, block: false},
		{name: "rolled back, no block", before: true, block: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, "agent_work")
			env.requireFake(t)
			env.fake.SetAgentWorkSendErrorsSupported(tc.before)
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
					return resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "false")
				}
				return resource.TestCheckNoResourceAttr(projectRes, "agent_work.send_errors")
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
							env.fake.OnNextRequest("PATCH", route, func() { env.fake.SetAgentWorkSendErrorsSupported(!tc.before) })
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

// A configured send_errors gets no such allowance: a Flightdeck rolled back
// below the setting between the plan and the apply refuses the write, and the
// apply fails saying so.
func TestProjectAgentWork_sendErrorsConfiguredMeetsARollbackDuringApply(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: sendErrorsConfig(env, identifier, `    send_errors = false`), Check: captureAttr(projectRes, "id", &id)},
			{
				PreConfig: func() {
					env.fake.OnNextRequest("PATCH", fmt.Sprintf("/api/v1/projects/%s/agent-work", id), func() {
						env.fake.SetAgentWorkSendErrorsSupported(false)
					})
				},
				Config:      sendErrorsConfig(env, identifier, `    send_errors = true`),
				ExpectError: regexMust(`This Flightdeck does not support agent_work\.send_errors yet`),
			},
		},
	})
}

// keepUnsetAsPlanned changes send_errors in the result only when the
// configuration leaves it unset and the plan knew its value, and never
// touches the other newer settings.
func TestKeepUnsetSendErrorsAsPlanned(t *testing.T) {
	ctx := t.Context()
	on, off := types.BoolValue(true), types.BoolValue(false)
	obj := func(v types.Bool) types.Object {
		return agentWorkObj(t, &agentWorkModel{SendErrors: v, AutoMerge: on, SendCIFailures: on, WaitForChecks: off,
			MaxInProgress: types.Int64Value(3)})
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
			out := keepUnsetAsPlanned("send_errors", tc.config, tc.planned, tc.after, &diags)
			if diags.HasError() {
				t.Fatalf("errors: %v", diags.Errors())
			}
			var got agentWorkModel
			diags.Append(out.As(ctx, &got, objectAsOptions)...)
			if !got.SendErrors.Equal(tc.want) {
				t.Errorf("send_errors = %v, want %v", got.SendErrors, tc.want)
			}
			if got.MaxInProgress.ValueInt64() != 3 || !got.AutoMerge.Equal(on) || !got.SendCIFailures.Equal(on) || !got.WaitForChecks.Equal(off) {
				t.Errorf("the rest of the block changed: %+v", got)
			}
		})
	}
	// Each newer setting is corrected on its own: an unset send_errors the
	// server changed is kept as planned while a configured send_ci_failures is
	// left as the server reports it.
	var diags diag.Diagnostics
	config := agentWorkObj(t, &agentWorkModel{SendCIFailures: on, SendErrors: types.BoolNull(), MaxInProgress: types.Int64Value(3)})
	planned := agentWorkObj(t, &agentWorkModel{SendCIFailures: on, SendErrors: types.BoolNull(), MaxInProgress: types.Int64Value(3)})
	after := agentWorkObj(t, &agentWorkModel{SendCIFailures: off, SendErrors: off, MaxInProgress: types.Int64Value(3)})
	out := after
	for _, setting := range agentWorkNewerSettings {
		out = keepUnsetAsPlanned(setting.key, config, planned, out, &diags)
	}
	var got agentWorkModel
	diags.Append(out.As(ctx, &got, objectAsOptions)...)
	if diags.HasError() {
		t.Fatalf("errors: %v", diags.Errors())
	}
	if !got.SendErrors.IsNull() || !got.SendCIFailures.Equal(off) {
		t.Errorf("want send_errors kept as planned (null) and send_ci_failures as reported (false), got %v and %v",
			got.SendErrors, got.SendCIFailures)
	}
}

// A change elsewhere in the project plans an unset send_errors as the value in
// state, not "known after apply", and so leaves the settings' lock_version
// known too. On an older Flightdeck that value is null.
func TestProjectAgentWork_sendErrorsUnsetStaysKnownOnARename(t *testing.T) {
	for _, supported := range []bool{true, false} {
		t.Run(fmt.Sprintf("Flightdeck has send_errors: %t", supported), func(t *testing.T) {
			env := newTestEnv(t, "agent_work")
			env.requireFake(t)
			env.fake.SetAgentWorkSendErrorsSupported(supported)
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
								plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("agent_work").AtMapKey("send_errors"), want),
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
// the API's wording: only a 422 to a write that sent send_errors, from a
// Flightdeck whose read has no send_errors, is the server being too old for
// it. agentWorkRefusedSettings lists every newer setting that holds for, in
// the API's order.
func TestAgentWorkSendErrorsRefused(t *testing.T) {
	on := true
	knowsAll := &client.AgentWork{AutoMerge: &on, SendCIFailures: &on, WaitForChecks: &on, SendErrors: &on}
	knowsThree := &client.AgentWork{AutoMerge: &on, SendCIFailures: &on, WaitForChecks: &on}
	olderThanAll := &client.AgentWork{}
	unknownKey := &client.Error{Method: "PATCH", Path: "/projects/4/agent-work", Status: 422, Code: client.CodeInvalidAttribute,
		Message: "unknown key: send_errors (settable: enabled, kinds, auto_merge, send_ci_failures, wait_for_checks, label_id, " +
			"accept_machine_labels, research_label_id, agent_account_id, base_ref, max_in_progress, daily_budget_usd, " +
			"task_max_usd, task_max_minutes, queue_minutes, runbook)"}
	stale := &client.Error{Status: 409, Code: client.CodeStaleObject, Message: "send_errors changed"}
	sent := client.Fields{"send_errors": true}
	all := client.Fields{"send_errors": true, "wait_for_checks": false, "send_ci_failures": true, "auto_merge": false}
	cases := []struct {
		name    string
		current *client.AgentWork
		sent    client.Fields
		err     error
		want    bool
		refused []string
	}{
		{"older Flightdeck, sent", knowsThree, sent, unknownKey, true, []string{"send_errors"}},
		{"older Flightdeck, sent false", knowsThree, client.Fields{"send_errors": false}, unknownKey, true, []string{"send_errors"}},
		{"older Flightdeck, sent with the others", knowsThree, all, unknownKey, true, []string{"send_errors"}},
		{"older than all, all sent", olderThanAll, all, unknownKey, true, []string{"auto_merge", "send_ci_failures", "send_errors", "wait_for_checks"}},
		{"older than all, only wait_for_checks sent", olderThanAll, client.Fields{"wait_for_checks": false}, unknownKey, false, []string{"wait_for_checks"}},
		{"a Flightdeck that knows it", knowsAll, all, unknownKey, false, nil},
		{"not sent", knowsThree, client.Fields{"max_in_progress": int64(2)}, unknownKey, false, nil},
		{"not a 422", knowsThree, sent, stale, false, nil},
		{"no read", nil, sent, unknownKey, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentWorkSettingRefused(agentWorkSendErrors, tc.current, tc.sent, tc.err); got != tc.want {
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

// Creating a project that sets all four newer settings on a Flightdeck older
// than all of them gives four warnings, one on each attribute, and no error,
// so the new project isn't tainted. Once Flightdeck is upgraded to the newest
// release, the next apply is an update of the same project.
func TestProjectAgentWork_everyNewerSettingAtCreateOnAFlightdeckOlderThanAll(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkBeforeAutoMerge)
	identifier := randIdentifier()
	config := sendErrorsConfig(env, identifier,
		"    max_in_progress  = 2\n    auto_merge       = true\n    send_ci_failures = true\n    send_errors      = true\n    wait_for_checks  = false")
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config, ExpectNonEmptyPlan: true},
			{
				PreConfig: func() { env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkWithSendErrors) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "true"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "true"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "true"),
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
	for _, key := range []string{"auto_merge", "send_ci_failures", "send_errors", "wait_for_checks"} {
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
	if unsupported != 4 {
		t.Errorf("want four warnings about unsupported settings, got %d: %s", unsupported, describeWithPaths(warnings))
	}
}

// Flightdeck is upgraded from below auto_merge to the newest release, or
// rolled back from the newest to below auto_merge, between the plan and the
// apply: all four newer settings change at once. Left unset, each keeps the
// value the plan promised, and the next refresh adopts the server's for all
// of them without planning anything.
func TestProjectAgentWork_everyNewerSettingUnsetSurvivesAServerChangeDuringApply(t *testing.T) {
	release := func(all bool) flightdecktest.AgentWorkRelease {
		if all {
			return flightdecktest.AgentWorkWithSendErrors
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
						resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "false"),
						resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "true"),
					)
				}
				return resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.auto_merge"),
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.send_ci_failures"),
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.send_errors"),
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.wait_for_checks"),
				)
			}
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{Config: config("Every change", 2), Check: resource.ComposeAggregateTestCheckFunc(captureAttr(projectRes, "id", &id), check(before))},
					{
						PreConfig: func() {
							env.fake.OnNextRequest("PATCH", fmt.Sprintf("/api/v1/projects/%s/agent-work", id), func() {
								env.fake.SetAgentWorkRelease(release(!before))
							})
						},
						Config: config("Every change renamed", 3),
						// The plan's values, kept.
						Check: check(before),
					},
					{
						Config: config("Every change renamed", 3),
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

// A project imported from a Flightdeck that has wait_for_checks but not
// send_errors reads send_errors as null and matches the state the apply left.
// A later change elsewhere plans it as that null, not "known after apply",
// and keeps the settings' lock_version known too.
func TestProjectAgentWork_sendErrorsImportedFromAnOlderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkWithWaitForChecks)
	identifier := randIdentifier()
	config := func(name string) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  agent_work = {
    wait_for_checks = false
  }`, name))
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config("Imported"),
				Check:  resource.TestCheckNoResourceAttr(projectRes, "agent_work.send_errors"),
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
						plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("agent_work").AtMapKey("send_errors"), knownvalue.Null()),
						plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("agent_work").AtMapKey("wait_for_checks"), knownvalue.Bool(false)),
						plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("agent_work").AtMapKey("lock_version"), knownvalue.Int64Exact(1)),
					},
				},
			},
		},
	})
}
