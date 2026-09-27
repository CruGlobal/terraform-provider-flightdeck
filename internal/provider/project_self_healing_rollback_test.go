package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// Every live test here ends with rollback back at "report" before the
// project is destroyed, so nothing is ever left at auto-rollback.

func TestProjectSelfHealing_rollbackRoundTrips(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	identifier := randIdentifier()
	var id string
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Rollback"
  self_healing = {
    feature_enabled = false
    rollback        = "report"
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "self_healing.rollback", "report"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.armed", "false"),
				),
			},
			{
				// Auto-rollback with the feature off is accepted and stored;
				// nothing acts until the feature is on.
				Config: projectConfig(env, identifier, `
  name = "Rollback"
  self_healing = {
    feature_enabled = false
    rollback        = "auto"
  }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "self_healing.rollback", "auto"),
					// The deprecated mirror follows the mode.
					resource.TestCheckResourceAttr(projectRes, "self_healing.armed", "true"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.feature_enabled", "false"),
				),
			},
			{
				ResourceName:            projectRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"features"},
			},
			{
				// Dropping rollback from configuration leaves the mode alone,
				// even at auto, which is not the server's default.
				Config: projectConfig(env, identifier, `
  name = "Rollback"
  self_healing = {
    feature_enabled = false
  }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.rollback", "auto"),
			},
			{
				// Back to report only, which is also where the test must end.
				Config: projectConfig(env, identifier, `
  name = "Rollback"
  self_healing = {
    feature_enabled = false
    rollback        = "report"
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "self_healing.rollback", "report"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.armed", "false"),
				),
			},
		},
	})

	// Moving to auto warned at plan time, and saying the feature is off.
	if !anyDiagnostic(recorded.planWarnings(), "This apply turns on auto-rollback for project "+identifier, "Nothing acts while feature_enabled is off") {
		t.Fatalf("no plan-time auto-rollback warning; plan warnings: %s", describe(recorded.planWarnings()))
	}
	// While at auto, what blocks it came back as warnings after the apply and
	// on refresh. The feature being off is always one of them.
	summary := fmt.Sprintf("Auto-rollback cannot act on project %s right now: feature_off", identifier)
	if !anyDiagnostic(recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning), summary) {
		t.Fatalf("no feature_off blocker warning after apply; apply warnings: %s", describe(recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning)))
	}
	if !anyDiagnostic(recorded.readWarnings(), summary) {
		t.Fatalf("no feature_off blocker warning on refresh; refresh warnings: %s", describe(recorded.readWarnings()))
	}
}

// Turning the feature on over a stored auto is going live, and the API wants
// that said out loud: the same write must name rollback.
func TestProjectSelfHealing_goingLiveNeedsRollbackInTheSameWrite(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	identifier := randIdentifier()
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Stored as auto-rollback, paused.
				Config: projectConfig(env, identifier, `
  name = "Go live"
  self_healing = {
    feature_enabled = false
    rollback        = "auto"
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.rollback", "auto"),
			},
			{
				// Turning the feature on without rollback in configuration: the
				// write does not name the mode, so the API refuses it.
				Config: projectConfig(env, identifier, `
  name = "Go live"
  self_healing = {
    feature_enabled = true
  }`),
				ExpectError: regexMust(`(?s)Set\s+rollback\s+to\s+turn\s+this\s+project's\s+self-healing\s+on.*rollback\s+explicitly`),
			},
			{
				// Saying "report" starts it in report only.
				Config: projectConfig(env, identifier, `
  name = "Go live"
  self_healing = {
    feature_enabled = true
    rollback        = "report"
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "self_healing.feature_enabled", "true"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.rollback", "report"),
				),
			},
			{
				// Going live on this test project (no releases, so nothing can
				// act) warns at plan time with the plain-words message.
				Config: projectConfig(env, identifier, `
  name = "Go live"
  self_healing = {
    feature_enabled = true
    rollback        = "auto"
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.rollback", "auto"),
			},
			{
				// With the feature already on, another write that names
				// feature_enabled but not rollback is not going live again.
				Config: projectConfig(env, identifier, `
  name = "Go live"
  self_healing = {
    feature_enabled = true
    bake_minutes    = 25
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "25"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.rollback", "auto"),
				),
			},
			{
				// Leave it at report only.
				Config: projectConfig(env, identifier, `
  name = "Go live"
  self_healing = {
    feature_enabled = false
    rollback        = "report"
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.rollback", "report"),
			},
		},
	})
	// The refused step warned that it would be refused, and only the step
	// that named rollback promised rollbacks.
	if !anyDiagnostic(recorded.planWarnings(), "Flightdeck will refuse this apply for project "+identifier+" unless rollback is set") {
		t.Fatalf("no refusal warning for going live without rollback; plan warnings: %s", describe(recorded.planWarnings()))
	}
	if !anyDiagnostic(recorded.planWarnings(), "This apply turns on auto-rollback for project "+identifier,
		"Flightdeck will roll production back by itself when every check passes, and page a person when it can't.") {
		t.Fatalf("no going-live warning; plan warnings: %s", describe(recorded.planWarnings()))
	}
}

// A rollback fed from a value not known until apply: the plan cannot promise
// armed, and warns that the apply MAY turn auto-rollback on.
func TestProjectSelfHealing_rollbackNotKnownUntilApply(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	identifier := randIdentifier()
	config := func(mode string) string {
		return env.providerConfig() + fmt.Sprintf(`
resource "terraform_data" "mode" {
  input = %q
}

resource "flightdeck_project" "test" {
  name       = "Mode from elsewhere"
  identifier = %q
  self_healing = {
    feature_enabled = false
    rollback        = terraform_data.mode.output
  }
}
`, mode, identifier)
	}
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config("report"),
				Check:  resource.TestCheckResourceAttr(projectRes, "self_healing.armed", "false"),
			},
			{
				// terraform_data's output is unknown until the replacement applies.
				Config: config("auto"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "self_healing.rollback", "auto"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.armed", "true"),
				),
			},
			{
				Config: config("report"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "self_healing.rollback", "report"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.armed", "false"),
				),
			},
		},
	})
	if !anyDiagnostic(recorded.planWarnings(), "This apply may turn on auto-rollback for project "+identifier) {
		t.Fatalf("no may-turn-on warning; plan warnings: %s", describe(recorded.planWarnings()))
	}
}

func TestProjectSelfHealing_countBrowserErrorsRoundTrips(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Browser errors"
  self_healing = {
    count_browser_errors = true
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.count_browser_errors", "true"),
			},
			{
				// Unset keeps what the project has.
				Config: projectConfig(env, identifier, `
  name = "Browser errors"
  self_healing = {
    bake_minutes = 25
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "25"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.count_browser_errors", "true"),
				),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Browser errors"
  self_healing = {
    count_browser_errors = false
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.count_browser_errors", "false"),
			},
		},
	})
}

func TestProjectSelfHealing_rollbackValidation(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	identifier := randIdentifier()
	var steps []resource.TestStep
	for _, bad := range []string{"Auto", "on", "armed", ""} {
		steps = append(steps, resource.TestStep{
			Config: projectConfig(env, identifier, fmt.Sprintf(`
  name = "Bad mode"
  self_healing = {
    rollback = %q
  }`, bad)),
			ExpectError: regexMust(`value must be one of`),
		})
	}
	runTest(t, resource.TestCase{Steps: steps})
}

// A Flightdeck from before the rollback setting: the mode is derived from
// config.armed, and configuring either new setting says the server is too old.
func TestProjectSelfHealing_olderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	env.requireFake(t)
	env.fake.SetSelfHealingLegacy(true)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Older Flightdeck"
  self_healing = {
    bake_minutes = 30
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "self_healing.rollback", "report"),
					resource.TestCheckNoResourceAttr(projectRes, "self_healing.count_browser_errors"),
				),
			},
			{
				// Armed in the console: the derived mode follows, and the plan
				// stays empty.
				PreConfig: func() { env.fake.ArmSelfHealing(mustInt(id), true) },
				Config: projectConfig(env, identifier, `
  name = "Older Flightdeck"
  self_healing = {
    bake_minutes = 30
  }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "self_healing.rollback", "auto"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.armed", "true"),
				),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Older Flightdeck"
  self_healing = {
    rollback = "report"
  }`),
				ExpectError: regexMust(`This Flightdeck is too old for self_healing\.rollback`),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Older Flightdeck"
  self_healing = {
    count_browser_errors = true
  }`),
				ExpectError: regexMust(`This Flightdeck is too old for\s+self_healing\.count_browser_errors`),
			},
		},
	})
}

// A write refused for some other reason while naming rollback is not blamed
// on the server's age.
func TestProjectSelfHealing_otherRefusalIsNotCalledTooOld(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Windows"
  self_healing = {
    long_window_minutes  = 10
    short_window_minutes = 5
  }`),
			},
			{
				// The merged windows are refused; rollback rides along.
				Config: projectConfig(env, identifier, `
  name = "Windows"
  self_healing = {
    rollback             = "report"
    short_window_minutes = 60
  }`),
				ExpectError: regexMust(`(?s)Error\s+updating\s+Flightdeck\s+self-healing\s+configuration.*short_window_minutes\s+\(60\)`),
			},
		},
	})
}

func TestSelfHealingToObject_rollbackMapping(t *testing.T) {
	ctx := context.Background()
	str := func(s string) *string { return &s }
	yes := true
	for _, tc := range []struct {
		name        string
		sh          client.SelfHealing
		wantMode    string
		wantBrowser types.Bool
	}{
		{name: "top-level auto", sh: client.SelfHealing{Rollback: str("auto"), Config: client.SelfHealingConfig{Armed: true, CountBrowserErrors: &yes}}, wantMode: "auto", wantBrowser: types.BoolValue(true)},
		{name: "top-level report", sh: client.SelfHealing{Rollback: str("report")}, wantMode: "report", wantBrowser: types.BoolNull()},
		// An older Flightdeck: no top-level rollback, so armed decides.
		{name: "older server, armed", sh: client.SelfHealing{Config: client.SelfHealingConfig{Armed: true}}, wantMode: "auto", wantBrowser: types.BoolNull()},
		{name: "older server, not armed", sh: client.SelfHealing{}, wantMode: "report", wantBrowser: types.BoolNull()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			obj := selfHealingToObject(&tc.sh, &diags)
			if diags.HasError() {
				t.Fatal(diags)
			}
			var m selfHealingModel
			diags.Append(obj.As(ctx, &m, objectAsOptions)...)
			if diags.HasError() {
				t.Fatal(diags)
			}
			if m.Rollback.ValueString() != tc.wantMode {
				t.Fatalf("rollback = %s, want %s", m.Rollback, tc.wantMode)
			}
			if !m.CountBrowserErrors.Equal(tc.wantBrowser) {
				t.Fatalf("count_browser_errors = %s, want %s", m.CountBrowserErrors, tc.wantBrowser)
			}
		})
	}
}

// armed is computed with UseStateForUnknown, so the plan has to move it with
// the mode or the apply is an inconsistent result.
func TestPlanArmed(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		mode       types.String
		priorArmed types.Bool
		want       types.Bool
	}{
		{name: "to auto", mode: types.StringValue("auto"), priorArmed: types.BoolValue(false), want: types.BoolValue(true)},
		{name: "to report", mode: types.StringValue("report"), priorArmed: types.BoolValue(true), want: types.BoolValue(false)},
		// On an update UseStateForUnknown has already filled armed from state,
		// so an unknown mode has to undo that.
		{name: "mode not known until apply", mode: types.StringUnknown(), priorArmed: types.BoolValue(false), want: types.BoolUnknown()},
		{name: "mode not known on create", mode: types.StringUnknown(), priorArmed: types.BoolUnknown(), want: types.BoolUnknown()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			in, d := types.ObjectValueFrom(ctx, selfHealingAttrTypes, selfHealingModel{Rollback: tc.mode, Armed: tc.priorArmed})
			diags.Append(d...)
			var m selfHealingModel
			diags.Append(planArmed(ctx, in, &diags).As(ctx, &m, objectAsOptions)...)
			if diags.HasError() {
				t.Fatal(diags)
			}
			if !m.Armed.Equal(tc.want) {
				t.Fatalf("armed = %s, want %s", m.Armed, tc.want)
			}
		})
	}
}

func TestWarnAutoRollback(t *testing.T) {
	ctx := context.Background()
	block := func(feature types.Bool, mode types.String) types.Object {
		obj, d := types.ObjectValueFrom(ctx, selfHealingAttrTypes, selfHealingModel{FeatureEnabled: feature, Rollback: mode})
		if d.HasError() {
			t.Fatal(d)
		}
		return obj
	}
	on, off, unknownBool, unsetBool := types.BoolValue(true), types.BoolValue(false), types.BoolUnknown(), types.BoolNull()
	auto, report, unknownMode, unset := types.StringValue("auto"), types.StringValue("report"), types.StringUnknown(), types.StringNull()
	null := types.ObjectNull(selfHealingAttrTypes)
	const (
		turnsOn  = "This apply turns on auto-rollback for project APP"
		mayTurn  = "This apply may turn on auto-rollback for project APP"
		refused  = "Flightdeck will refuse this apply for project APP unless rollback is set"
		live     = "Flightdeck will roll production back by itself when every check passes, and page a person when it can't."
		paused   = "Nothing acts while feature_enabled is off"
		notShown = "which this plan cannot show yet"
	)
	for _, tc := range []struct {
		name                string
		config, prior, plan types.Object
		// wantSummary and wantDetail describe the one warning; an empty
		// summary means no warning at all.
		wantSummary, wantDetail string
	}{
		{name: "report to auto, feature on", config: block(on, auto), prior: block(on, report), plan: block(on, auto), wantSummary: turnsOn, wantDetail: live},
		{name: "report to auto, feature off", config: block(off, auto), prior: block(off, report), plan: block(off, auto), wantSummary: turnsOn, wantDetail: paused},
		{name: "report to auto, feature not known yet", config: block(unknownBool, auto), prior: block(off, report), plan: block(unknownBool, auto), wantSummary: turnsOn, wantDetail: notShown},
		{name: "going live, rollback configured", config: block(on, auto), prior: block(off, auto), plan: block(on, auto), wantSummary: turnsOn, wantDetail: live},
		// The API's go-live guard refuses a write that turns the feature on
		// over a stored auto without naming rollback, so no rollbacks are promised.
		{name: "going live, rollback not configured", config: block(on, unset), prior: block(off, auto), plan: block(on, auto), wantSummary: refused, wantDetail: `Set self_healing.rollback to "auto"`},
		{name: "going live and to auto at once", config: block(on, auto), prior: block(off, report), plan: block(on, auto), wantSummary: turnsOn, wantDetail: live},
		{name: "create at auto, feature on", config: block(on, auto), prior: null, plan: block(on, auto), wantSummary: turnsOn, wantDetail: live},
		{name: "create at auto, feature unset", config: block(unsetBool, auto), prior: null, plan: block(unknownBool, auto), wantSummary: turnsOn, wantDetail: notShown},
		// Unset on create is unknown in the plan, and means the server's report.
		{name: "create, rollback unset", config: block(on, unset), prior: null, plan: block(on, unknownMode)},
		{name: "rollback from a value not known until apply", config: block(on, unknownMode), prior: block(on, report), plan: block(on, unknownMode), wantSummary: mayTurn, wantDetail: live},
		{name: "rollback not known, already live", config: block(on, unknownMode), prior: block(on, auto), plan: block(on, unknownMode)},
		{name: "already live", config: block(on, auto), prior: block(on, auto), plan: block(on, auto)},
		{name: "stays auto, paused", config: block(off, auto), prior: block(off, auto), plan: block(off, auto)},
		{name: "pausing", config: block(off, unset), prior: block(on, auto), plan: block(off, auto)},
		{name: "auto to report", config: block(on, report), prior: block(on, auto), plan: block(on, report)},
		{name: "feature on in report only", config: block(on, unset), prior: block(off, report), plan: block(on, report)},
		{name: "block left out of configuration", config: null, prior: block(on, report), plan: block(on, report)},
		{name: "no block planned", config: null, prior: block(on, report), plan: types.ObjectUnknown(selfHealingAttrTypes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			warnAutoRollback(ctx, types.StringValue("APP"), tc.config, tc.prior, tc.plan, &diags)
			if diags.HasError() {
				t.Fatal(diags.Errors())
			}
			if tc.wantSummary == "" {
				if diags.WarningsCount() != 0 {
					t.Fatalf("expected no warning, got %v", diags.Warnings())
				}
				return
			}
			if diags.WarningsCount() != 1 {
				t.Fatalf("expected one warning, got %v", diags.Warnings())
			}
			w := diags.Warnings()[0]
			if w.Summary() != tc.wantSummary || !strings.Contains(w.Detail(), tc.wantDetail) || !strings.Contains(w.Detail(), "project APP") {
				t.Fatalf("warning is not %q saying %q for project APP:\n%s\n%s", tc.wantSummary, tc.wantDetail, w.Summary(), w.Detail())
			}
			// The refusal must not also promise rollbacks.
			if tc.wantSummary == refused && strings.Contains(w.Detail(), live) {
				t.Fatalf("the refusal warning promises rollbacks:\n%s", w.Detail())
			}
		})
	}
}

func TestWarnRollbackBlockers(t *testing.T) {
	str := func(s string) *string { return &s }
	blockers := []client.RollbackBlocker{
		{Code: "no_current_release", Message: "Promote twice through the pipeline."},
		{Code: "not_rollback_safe", Message: "Mark the live release safe to roll back."},
	}
	repeated := []client.RollbackBlocker{
		{Code: "rollback_target_untrusted", Message: "The release before the live one was not posted through the API."},
		{Code: "rollback_target_untrusted", Message: "The release before that was not posted through the API either."},
	}
	for _, tc := range []struct {
		name string
		sh   *client.SelfHealing
		want []client.RollbackBlocker
	}{
		{name: "no answer", sh: nil},
		{name: "report only lists blockers but warns of none", sh: &client.SelfHealing{Rollback: str("report"), RollbackBlockers: blockers}},
		{name: "auto with nothing in the way", sh: &client.SelfHealing{Rollback: str("auto")}},
		{name: "auto with blockers", sh: &client.SelfHealing{Rollback: str("auto"), RollbackBlockers: blockers}, want: blockers},
		{name: "older server armed", sh: &client.SelfHealing{Config: client.SelfHealingConfig{Armed: true}, RollbackBlockers: blockers}, want: blockers},
		{name: "one code twice", sh: &client.SelfHealing{Rollback: str("auto"), RollbackBlockers: repeated}, want: repeated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			warnRollbackBlockers(tc.sh, "APP", &diags)
			if diags.WarningsCount() != len(tc.want) {
				t.Fatalf("expected %d warnings, got %v", len(tc.want), diags.Warnings())
			}
			summaries := map[string]bool{}
			for i, w := range diags.Warnings() {
				wantSummary := "Auto-rollback cannot act on project APP right now: " + tc.want[i].Code
				if i > 0 && tc.want[i].Code == tc.want[0].Code {
					wantSummary += fmt.Sprintf(" (%d)", i+1)
				}
				if w.Summary() != wantSummary || !strings.Contains(w.Detail(), tc.want[i].Message) {
					t.Fatalf("warning %d = %q / %q, want %q with %q", i, w.Summary(), w.Detail(), wantSummary, tc.want[i].Message)
				}
				// Terraform folds warnings that share a summary into one.
				if summaries[w.Summary()] {
					t.Fatalf("two warnings share the summary %q", w.Summary())
				}
				summaries[w.Summary()] = true
			}
		})
	}
}

// The blockers the fake reports come through end to end, one warning each.
func TestProjectSelfHealing_blockerWarningsEndToEnd(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `  name = "Blockers"`),
				Check:  captureAttr(projectRes, "id", &id),
			},
			{
				PreConfig: func() {
					env.fake.SetRollbackBlockers(mustInt(id), []flightdecktest.RollbackBlocker{
						{Code: "not_rollback_safe", Message: "The live release is not marked safe to roll back."},
						{Code: "no_app", Message: "The live release names no app."},
					})
				},
				Config: projectConfig(env, identifier, `
  name = "Blockers"
  self_healing = {
    feature_enabled = true
    rollback        = "auto"
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.rollback", "auto"),
			},
			{
				// Nothing in the way: no warnings from here on.
				PreConfig: func() { env.fake.SetRollbackBlockers(mustInt(id), nil) },
				Config: projectConfig(env, identifier, `
  name = "Blockers"
  self_healing = {
    feature_enabled = false
    rollback        = "report"
  }`),
			},
		},
	})
	warnings := recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning)
	for _, code := range []string{"not_rollback_safe", "no_app"} {
		if !anyDiagnostic(warnings, "Auto-rollback cannot act on project "+identifier+" right now: "+code) {
			t.Fatalf("no %s warning after apply: %s", code, describe(warnings))
		}
	}
	// feature_off was not a blocker while the feature was on, and the default
	// release blocker was replaced by the ones set.
	for _, code := range []string{"feature_off", "no_current_release"} {
		if anyDiagnostic(warnings, "right now: "+code) {
			t.Fatalf("unexpected %s warning: %s", code, describe(warnings))
		}
	}
}

// anyDiagnostic reports whether one diagnostic's summary and detail together
// contain every substring.
func anyDiagnostic(diags []*tfprotov6.Diagnostic, substrings ...string) bool {
	for _, d := range diags {
		text := d.Summary + "\n" + d.Detail
		all := true
		for _, s := range substrings {
			if !strings.Contains(text, s) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func describe(diags []*tfprotov6.Diagnostic) string {
	parts := make([]string, 0, len(diags))
	for _, d := range diags {
		parts = append(parts, fmt.Sprintf("[%s] %s", d.Summary, d.Detail))
	}
	return strings.Join(parts, "\n")
}
