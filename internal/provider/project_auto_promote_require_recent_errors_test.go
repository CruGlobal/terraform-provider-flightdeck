package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// auto_promote.require_recent_errors: whether auto-promote waits for a
// production error in the last 7 days. Every live test here ends with the
// mode off, and none of them turns auto-rollback on.

// sentRecentErrors reports what each recorded self-healing PATCH sent for
// require_recent_errors: "true", "false", or "-" when it did not name it.
func sentRecentErrors(t *testing.T, env *testEnv) []string {
	t.Helper()
	_, bodies := selfHealingPatches(t, env)
	out := make([]string, 0, len(bodies))
	for _, body := range bodies {
		v, named := body["require_recent_errors"]
		if !named {
			out = append(out, "-")
			continue
		}
		out = append(out, fmt.Sprint(v))
	}
	return out
}

// It round trips with the mode, a second plan is empty, leaving it unset
// keeps what the project has, and the data source reads it. Turning it off
// warns at plan time; turning it back on does not.
func TestProjectAutoPromote_requireRecentErrorsRoundTrips(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	identifier := randIdentifier()
	lookup := `
data "flightdeck_project" "lookup" {
  id = flightdeck_project.test.id
}
`
	turnedOff := autoPromoteConfig(env, identifier, "    mode                  = \"deps_only\"\n    require_recent_errors = false")
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Flightdeck's default, read back before anything sets it.
				Config: autoPromoteConfig(env, identifier, `    mode = "off"`),
				Check:  resource.TestCheckResourceAttr(projectRes, "auto_promote.require_recent_errors", "true"),
			},
			{
				Config: turnedOff,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "auto_promote.mode", "deps_only"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.require_recent_errors", "false"),
				),
			},
			{
				Config: turnedOff,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Unset keeps what the project has: the plan already knows the
				// value, and the data source reads it.
				Config: autoPromoteConfig(env, identifier, `    mode = "off"`) + lookup,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("auto_promote").AtMapKey("require_recent_errors"),
							knownvalue.Bool(false)),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "auto_promote.mode", "off"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.require_recent_errors", "false"),
					resource.TestCheckResourceAttr("data.flightdeck_project.lookup", "auto_promote.require_recent_errors", "false"),
				),
			},
			{
				Config: autoPromoteConfig(env, identifier, "    mode                  = \"off\"\n    require_recent_errors = true"),
				Check:  resource.TestCheckResourceAttr(projectRes, "auto_promote.require_recent_errors", "true"),
			},
		},
	})
	project := "project " + identifier
	if !anyDiagnostic(recorded.planWarnings(), "Auto-promote stops waiting for a recent production error on "+project,
		"This apply sets auto_promote.require_recent_errors for "+project+" to false.",
		"Auto-promote will no longer wait for a recent production error, so it can't tell a quiet app from one whose "+
			"error reporting is broken. It still needs an error ingestion token and at least one production error received "+
			"at some point.") {
		t.Fatalf("no plan warning for turning the recent-errors wait off: %s", describe(recorded.planWarnings()))
	}
	if anyDiagnostic(recorded.planWarnings(), "may stop waiting for a recent production error") {
		t.Fatalf("every value was known at plan time: %s", describe(recorded.planWarnings()))
	}
	if env.live() {
		return
	}
	// The create and the step that leaves it unset never name it; the others
	// send what they set.
	if got := strings.Join(sentRecentErrors(t, env), ","); got != "-,false,-,true" {
		t.Fatalf("require_recent_errors sent per write = %s, want -,false,-,true", got)
	}
}

// A value changed outside Terraform is drift while the configuration names
// it, and is set back. While the configuration leaves it out, it is read as
// Flightdeck has it, never sent, and plans nothing.
func TestProjectAutoPromote_requireRecentErrorsConsoleChange(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	owned := autoPromoteConfig(env, identifier, `    require_recent_errors = false`)
	unowned := autoPromoteConfig(env, identifier, `    soak_hours = 30`)
	setInConsole := func(v bool) func() {
		return func() {
			env.fake.SetAutoPromoteOutOfBand(mustInt(id), func(m map[string]any) { m["require_recent_errors"] = v })
		}
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: owned,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.require_recent_errors", "false"),
				),
			},
			{
				PreConfig: setInConsole(true),
				Config:    owned,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "auto_promote.require_recent_errors", "false"),
			},
			{
				PreConfig: setInConsole(true),
				Config:    unowned,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "auto_promote.require_recent_errors", "true"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "30"),
				),
			},
			{
				PreConfig: setInConsole(false),
				Config:    unowned,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "auto_promote.require_recent_errors", "false"),
			},
		},
	})
	if got := strings.Join(sentRecentErrors(t, env), ","); got != "false,false,-" {
		t.Fatalf("require_recent_errors sent per write = %s, want false,false,-", got)
	}
}

// A configuration that never names require_recent_errors never sends it, on
// a create or an update.
func TestProjectAutoPromote_requireRecentErrorsOmittedIsNotSent(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: autoPromoteConfig(env, identifier, `    soak_hours = 33`)},
			{
				Config: autoPromoteConfig(env, identifier, `    soak_hours = 34`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "34"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.require_recent_errors", "true"),
				),
			},
		},
	})
	if got := strings.Join(sentRecentErrors(t, env), ","); got != "-,-" {
		t.Fatalf("require_recent_errors sent per write = %s, want two writes that leave it out", got)
	}
}

// A Flightdeck that has auto-promote but predates require_recent_errors
// reads it as null, which plans nothing while it is unset. Setting it fails
// on the attribute, saying the server is too old, and saves nothing, the
// other auto-promote setting in the same write included. Once Flightdeck is
// upgraded, the same configuration applies.
func TestProjectAutoPromote_requireRecentErrorsOnAnOlderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	env.fake.SetRequireRecentErrorsSupported(false)
	identifier := randIdentifier()
	var id string
	unset := autoPromoteConfig(env, identifier, `    soak_hours = 30`)
	set := autoPromoteConfig(env, identifier, "    soak_hours            = 31\n    require_recent_errors = false")
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: unset,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckNoResourceAttr(projectRes, "auto_promote.require_recent_errors"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "30"),
				),
			},
			{
				Config: unset,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config:      set,
				ExpectError: regexMust(`This Flightdeck is too old for\s+auto_promote\.require_recent_errors`),
			},
			{
				PreConfig: func() {
					if row := env.fake.AutoPromoteOf(mustInt(id)); row["soak_hours"] != int64(30) {
						t.Errorf("the refused write must save nothing, got %v", row)
					}
					env.fake.SetRequireRecentErrorsSupported(true)
				},
				Config: set,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "auto_promote.require_recent_errors", "false"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "31"),
				),
			},
			{Config: set, PlanOnly: true},
		},
	})
	errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError)
	at := tftypes.NewAttributePath().WithAttributeName("auto_promote").WithAttributeName("require_recent_errors")
	if len(errs) != 1 || !diagnosticAt(errs, "This Flightdeck is too old for auto_promote.require_recent_errors", at,
		"Remove require_recent_errors from the configuration, or upgrade Flightdeck.",
		"None of the auto_promote settings in this apply were saved.",
		"unknown self-healing setting: require_recent_errors") {
		t.Fatalf("want one error on auto_promote.require_recent_errors, got %s", describeWithPaths(errs))
	}
}

// Flightdeck gains require_recent_errors, or loses it, between the plan and
// the apply, as a saved plan applied later can see. Left unset, it is never
// sent, so the apply keeps what the plan promised rather than fail with
// "Provider produced inconsistent result", and the next refresh adopts what
// Flightdeck reports without planning anything.
func TestProjectAutoPromote_requireRecentErrorsUnsetSurvivesAServerChangeDuringApply(t *testing.T) {
	for _, before := range []bool{false, true} {
		t.Run(fmt.Sprintf("Flightdeck has it at plan time: %t", before), func(t *testing.T) {
			env := newTestEnv(t, "auto_promote")
			env.requireFake(t)
			env.fake.SetRequireRecentErrorsSupported(before)
			identifier := randIdentifier()
			var id string
			config := func(soak int) string {
				return autoPromoteConfig(env, identifier, fmt.Sprintf("    soak_hours = %d", soak))
			}
			check := func(supported bool) resource.TestCheckFunc {
				if supported {
					return resource.TestCheckResourceAttr(projectRes, "auto_promote.require_recent_errors", "true")
				}
				return resource.TestCheckNoResourceAttr(projectRes, "auto_promote.require_recent_errors")
			}
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{Config: config(30), Check: resource.ComposeAggregateTestCheckFunc(captureAttr(projectRes, "id", &id), check(before))},
					{
						PreConfig: func() {
							env.fake.OnNextRequest("PATCH", fmt.Sprintf("/api/v1/projects/%s/self-healing", id), func() {
								env.fake.SetRequireRecentErrorsSupported(!before)
							})
						},
						Config: config(35),
						Check:  check(before),
					},
					{
						Config: config(35),
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

// On a Flightdeck from before auto-promote itself, setting
// require_recent_errors is reported the way any auto-promote setting is
// there, once, on auto_promote.
func TestProjectAutoPromote_requireRecentErrorsBeforeAutoPromote(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	env.fake.SetAutoPromoteSupported(false)
	identifier := randIdentifier()
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: projectConfig(env, identifier, `  name = "Older"`)},
			{
				Config:      autoPromoteConfig(env, identifier, `    require_recent_errors = false`),
				ExpectError: regexMust(`This Flightdeck does not support auto_promote yet`),
			},
		},
	})
	errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError)
	at := tftypes.NewAttributePath().WithAttributeName("auto_promote")
	if len(errs) != 1 || !diagnosticAt(errs, "This Flightdeck does not support auto_promote yet", at,
		"refused require_recent_errors as an unknown setting") {
		t.Fatalf("want one error on auto_promote, got %s", describeWithPaths(errs))
	}
}

// A write that sends require_recent_errors and is refused for something else
// is reported as that refusal, never as a Flightdeck too old for the setting.
func TestProjectAutoPromote_requireRecentErrorsOtherRefusalIsNotCalledTooOld(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	identifier := randIdentifier()
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Flightdeck refuses a time zone in the wrong case, which the plan
				// lets through.
				Config: autoPromoteConfig(env, identifier,
					"    require_recent_errors = false\n    window = {\n      time_zone = \"america/new_york\"\n    }"),
				ExpectError: regexMust(`Flightdeck refused auto_promote\.window\.time_zone`),
			},
		},
	})
	if env.live() {
		return
	}
	errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError)
	if !anyDiagnostic(errs, "Flightdeck refused auto_promote.window.time_zone") {
		t.Fatalf("the refusal was not recorded, so the test proves nothing: %s", describe(errs))
	}
	if anyDiagnostic(errs, "too old") || anyDiagnostic(errs, "does not support auto_promote") {
		t.Fatalf("a refusal for another reason was blamed on the server's age: %s", describe(errs))
	}
}

// ignore_changes naming require_recent_errors keeps a value changed in
// Flightdeck, while a configured change to another setting in the same apply
// is sent. The write sends the value in state, not the configuration's.
func TestProjectAutoPromote_ignoreChangesOnRequireRecentErrorsKeepsItsWebEdit(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	identifier := randIdentifier()
	config := func(name string, soak int) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  auto_promote = {
    soak_hours            = %d
    require_recent_errors = false
  }`, name, soak)+ignoreChanges("auto_promote.require_recent_errors"))
	}
	var id string
	changed := config("Recent errors renamed", 40)
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config("Recent errors", 30), Check: captureAttr(projectRes, "id", &id)},
			{
				PreConfig: func() {
					env.fake.SetAutoPromoteOutOfBand(mustInt(id), func(m map[string]any) { m["require_recent_errors"] = true })
				},
				Config: changed,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "40"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.require_recent_errors", "true"),
				),
			},
			{Config: changed, PlanOnly: true},
		},
	})
	writes := patchesTo(t, env, "/self-healing", "self_healing")
	if len(writes) != 2 {
		t.Fatalf("expected 2 self-healing PATCHes, got %d: %v", len(writes), writes)
	}
	if writes[1]["soak_hours"] != float64(40) || writes[1]["require_recent_errors"] != true {
		t.Errorf("the rename's write must send soak_hours 40 (configuration) and require_recent_errors true (state), got %v", writes[1])
	}
	// The plan keeps it on, so only the create's plan says it turns the wait off.
	if !anyDiagnostic(recorded.planWarnings(), "Auto-promote stops waiting for a recent production error") {
		t.Fatalf("the create turned the wait off, and its plan did not say so: %s", describe(recorded.planWarnings()))
	}
}

func TestWarnRecentErrorsWaitOff(t *testing.T) {
	ctx := context.Background()
	block := func(v types.Bool) types.Object { return autoPromoteObj(t, autoPromoteModel{RequireRecentErrors: v}) }
	on, off, unset := types.BoolValue(true), types.BoolValue(false), types.BoolNull()
	null, unknown := types.ObjectNull(autoPromoteAttrTypes), types.ObjectUnknown(autoPromoteAttrTypes)
	const turnsOff = "Auto-promote stops waiting for a recent production error on project APP"
	const mayTurnOff = "Auto-promote may stop waiting for a recent production error on project APP"
	cases := []struct {
		name                  string
		config, prior, plan   types.Object
		wantSummary, wantText string
	}{
		{name: "true to false", config: block(off), prior: block(on), plan: block(off),
			wantSummary: turnsOff, wantText: "This apply sets auto_promote.require_recent_errors for project APP to false."},
		{name: "created at false", config: block(off), prior: null, plan: block(off), wantSummary: turnsOff},
		{name: "an older Flightdeck's null to false", config: block(off), prior: block(unset), plan: block(off), wantSummary: turnsOff},
		{name: "not known until apply", config: block(types.BoolUnknown()), prior: block(on), plan: block(types.BoolUnknown()),
			wantSummary: mayTurnOff, wantText: "is not known until apply. If it is false, this apply turns the wait off."},
		{name: "the whole block not known until apply", config: unknown, prior: block(on), plan: unknown, wantSummary: mayTurnOff},
		{name: "already false", config: block(off), prior: block(off), plan: block(off)},
		{name: "false to true", config: block(on), prior: block(off), plan: block(on)},
		{name: "stays true", config: block(on), prior: block(on), plan: block(on)},
		{name: "unset, true in state", config: block(unset), prior: block(on), plan: block(on)},
		{name: "unset, on create", config: block(unset), prior: null, plan: block(types.BoolUnknown())},
		{name: "ignored: configured false, the plan keeps true", config: block(off), prior: block(on), plan: block(on)},
		{name: "no block", config: null, prior: null, plan: null},
		{name: "no block on create", config: null, prior: null, plan: unknown},
		{name: "no block, false in state", config: null, prior: block(off), plan: block(off)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			warnRecentErrorsWaitOff(ctx, types.StringValue("APP"), tc.config, tc.prior, tc.plan, &diags)
			if diags.HasError() {
				t.Fatalf("errors: %v", diags.Errors())
			}
			if tc.wantSummary == "" {
				if diags.WarningsCount() != 0 {
					t.Fatalf("want no warning, got %v", diags.Warnings())
				}
				return
			}
			if diags.WarningsCount() != 1 {
				t.Fatalf("want one warning, got %v", diags.Warnings())
			}
			w := diags.Warnings()[0]
			if w.Summary() != tc.wantSummary || !strings.Contains(w.Detail(), tc.wantText) ||
				!strings.HasSuffix(w.Detail(), recentErrorsWaitOff) {
				t.Fatalf("got %q / %q", w.Summary(), w.Detail())
			}
			if withPath, ok := w.(diag.DiagnosticWithPath); !ok || !withPath.Path().Equal(path.Root("auto_promote").AtName("require_recent_errors")) {
				t.Fatalf("the warning is not on auto_promote.require_recent_errors")
			}
		})
	}
}

func TestAutoPromoteToObject_requireRecentErrors(t *testing.T) {
	str := func(s string) *string { return &s }
	yes, no := true, false
	for _, tc := range []struct {
		name string
		sh   *client.SelfHealing
		want types.Bool
	}{
		{name: "true", sh: &client.SelfHealing{AutoPromote: str("off"), RequireRecentErrors: &yes}, want: types.BoolValue(true)},
		{name: "false", sh: &client.SelfHealing{AutoPromote: str("off"), RequireRecentErrors: &no}, want: types.BoolValue(false)},
		{name: "a Flightdeck that predates it", sh: &client.SelfHealing{AutoPromote: str("off")}, want: types.BoolNull()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			var m autoPromoteModel
			diags.Append(autoPromoteToObject(tc.sh, &diags).As(context.Background(), &m, objectAsOptions)...)
			if diags.HasError() {
				t.Fatal(diags)
			}
			if !m.RequireRecentErrors.Equal(tc.want) {
				t.Fatalf("require_recent_errors = %s, want %s", m.RequireRecentErrors, tc.want)
			}
		})
	}
}
