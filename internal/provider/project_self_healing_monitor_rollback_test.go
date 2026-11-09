package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// Every live test here leaves rollback at "report", so a monitor never acts
// on the test workspace.

func TestProjectSelfHealing_monitorRollbackRoundTrips(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// The server's default, read back before anything sets it.
				Config: projectConfig(env, identifier, `  name = "Monitor mode"`),
				Check:  resource.TestCheckResourceAttr(projectRes, "self_healing.monitor_rollback", "off"),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Monitor mode"
  self_healing = {
    monitor_rollback = "report"
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.monitor_rollback", "report"),
			},
			{
				// Unset keeps what the project has.
				Config: projectConfig(env, identifier, `
  name = "Monitor mode"
  self_healing = {
    bake_minutes = 25
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "25"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.monitor_rollback", "report"),
				),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Monitor mode"
  self_healing = {
    monitor_rollback = "off"
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.monitor_rollback", "off"),
			},
		},
	})
}

// "auto" while rollback is "report": the plan says a monitor only reports
// until rollback is "auto" too, and after the apply each thing that would stop
// a monitor's rollback is a warning. Flightdeck lists the same two codes
// whatever else the test workspace has: rollback is not "auto", and no routing
// key can trip the loop.
func TestProjectSelfHealing_monitorRollbackAutoWarns(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	identifier := randIdentifier()
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Monitor auto"
  self_healing = {
    rollback         = "report"
    monitor_rollback = "auto"
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.monitor_rollback", "auto"),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Monitor auto"
  self_healing = {
    rollback         = "report"
    monitor_rollback = "off"
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.monitor_rollback", "off"),
			},
		},
	})
	if !anyDiagnostic(recorded.planWarnings(), "This apply lets monitors roll back project "+identifier,
		`Its rollback is "report", so until that is "auto" too, a monitor's incident is only reported`) {
		t.Fatalf("no plan warning for monitor_rollback moving to auto: %s", describe(recorded.planWarnings()))
	}
	applied := recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning)
	for _, code := range []string{"monitor_auto_needs_rollback_auto", "no_trip_keys"} {
		if !anyDiagnostic(applied, "A monitor cannot roll back project "+identifier+" right now: "+code) {
			t.Fatalf("no %s warning after apply: %s", code, describe(applied))
		}
	}
	// Moving it back to "off" warns of nothing.
	if anyDiagnostic(recorded.planWarnings(), "This apply lets monitors roll back", "to \"off\"") {
		t.Fatalf("a warning for turning monitor_rollback off: %s", describe(recorded.planWarnings()))
	}
}

// A mode changed in the console is drift while Terraform owns the setting,
// and is simply read while it does not.
func TestProjectSelfHealing_monitorRollbackConsoleChange(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	owned := projectConfig(env, identifier, `
  name = "Console monitor"
  self_healing = {
    monitor_rollback = "report"
  }`)
	unowned := projectConfig(env, identifier, `
  name = "Console monitor"
  self_healing = {
    bake_minutes = 20
  }`)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: owned,
				Check:  captureAttr(projectRes, "id", &id),
			},
			{
				PreConfig: func() { env.fake.SetMonitorRollback(mustInt(id), "off") },
				Config:    owned,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.monitor_rollback", "report"),
			},
			{
				PreConfig: func() { env.fake.SetMonitorRollback(mustInt(id), "off") },
				Config:    unowned,
				Check:     resource.TestCheckResourceAttr(projectRes, "self_healing.monitor_rollback", "off"),
			},
			{
				Config: unowned,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// A configuration that never names monitor_rollback never sends it.
func TestProjectSelfHealing_monitorRollbackOmittedIsNotSent(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	env.requireFake(t)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Untouched monitor"
  self_healing = {
    rollback     = "report"
    bake_minutes = 33
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "33"),
			},
		},
	})
	sent := false
	for _, r := range env.fake.RequestsMatching("PATCH", "/api/v1/projects/") {
		if !strings.HasSuffix(r.Path, "/self-healing") {
			continue
		}
		sent = true
		if strings.Contains(string(r.Body), "monitor_rollback") {
			t.Fatalf("self-healing write named monitor_rollback although the configuration did not: %s", r.Body)
		}
	}
	if !sent {
		t.Fatal("no self-healing write was made, so the test proves nothing")
	}
}

func TestProjectSelfHealing_monitorRollbackValidation(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	identifier := randIdentifier()
	var steps []resource.TestStep
	// Flightdeck would read "Auto" as auto, and the next read would be a diff.
	for _, bad := range []string{"Auto", "on", "true", ""} {
		steps = append(steps, resource.TestStep{
			Config: projectConfig(env, identifier, fmt.Sprintf(`
  name = "Bad monitor mode"
  self_healing = {
    monitor_rollback = %q
  }`, bad)),
			ExpectError: regexMust(`value must be one of`),
		})
	}
	runTest(t, resource.TestCase{Steps: steps})
}

// A Flightdeck from before monitor_rollback reads it as null, and setting it
// says the server is too old rather than showing the bare refusal.
func TestProjectSelfHealing_monitorRollbackOnAnOlderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	env.requireFake(t)
	env.fake.SetMonitorRollbackSupported(false)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Older monitor"
  self_healing = {
    bake_minutes = 30
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(projectRes, "self_healing.monitor_rollback"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.rollback", "report"),
				),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Older monitor"
  self_healing = {
    bake_minutes = 30
  }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Older monitor"
  self_healing = {
    monitor_rollback = "report"
  }`),
				ExpectError: regexMust(`This Flightdeck is too old for\s+self_healing\.monitor_rollback`),
			},
		},
	})
}

func TestWarnMonitorRollback(t *testing.T) {
	ctx := context.Background()
	block := func(rollback, monitor types.String) types.Object {
		obj, d := types.ObjectValueFrom(ctx, selfHealingAttrTypes, selfHealingModel{
			FeatureEnabled: types.BoolNull(), Rollback: rollback, MonitorRollback: monitor,
			CountBrowserErrors: types.BoolNull(), Armed: types.BoolNull(),
			BakeMinutes: types.Int64Null(), BaselineMultiplier: types.Float64Null(), AbsoluteFloor: types.Float64Null(),
			LongWindowMinutes: types.Int64Null(), ShortWindowMinutes: types.Int64Null(), BurnRate: types.Float64Null(),
			SustainCount: types.Int64Null(), ConsecutiveErrorLimit: types.Int64Null(), CooldownMinutes: types.Int64Null(),
			MaxRollbacksPerHour: types.Int64Null(), RecoveryWindowMinutes: types.Int64Null(),
		})
		if d.HasError() {
			t.Fatal(d)
		}
		return obj
	}
	s := types.StringValue
	null := types.ObjectNull(selfHealingAttrTypes)
	for _, tc := range []struct {
		name                string
		config, prior, plan types.Object
		want                string // the summary, or "" for no warning
		detail              string
	}{
		{name: "to auto on create", config: block(types.StringNull(), s("auto")), prior: null,
			plan: block(s("auto"), s("auto")), want: "This apply lets monitors roll back project APP"},
		{name: "to auto while rollback reports", config: block(s("report"), s("auto")), prior: block(s("report"), s("off")),
			plan: block(s("report"), s("auto")), want: "This apply lets monitors roll back project APP",
			detail: `until that is "auto" too, a monitor's incident is only reported`},
		{name: "not known until apply", config: block(types.StringNull(), types.StringUnknown()), prior: block(s("report"), s("off")),
			plan: block(s("report"), types.StringUnknown()), want: "This apply may let monitors roll back project APP"},
		{name: "already auto", config: block(types.StringNull(), s("auto")), prior: block(s("auto"), s("auto")),
			plan: block(s("auto"), s("auto"))},
		{name: "to report", config: block(types.StringNull(), s("report")), prior: block(s("report"), s("off")),
			plan: block(s("report"), s("report"))},
		{name: "unset, auto in the console", config: block(types.StringNull(), types.StringNull()), prior: block(s("report"), s("off")),
			plan: block(s("report"), s("auto"))},
		{name: "no block", config: null, prior: null, plan: null},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			warnMonitorRollback(ctx, types.StringValue("APP"), tc.config, tc.prior, tc.plan, &diags)
			if diags.HasError() {
				t.Fatal(diags)
			}
			if tc.want == "" {
				if diags.WarningsCount() != 0 {
					t.Fatalf("expected no warning, got %v", diags.Warnings())
				}
				return
			}
			if diags.WarningsCount() != 1 || diags.Warnings()[0].Summary() != tc.want {
				t.Fatalf("expected one warning %q, got %v", tc.want, diags.Warnings())
			}
			if !strings.Contains(diags.Warnings()[0].Detail(), tc.detail) {
				t.Fatalf("warning detail %q does not say %q", diags.Warnings()[0].Detail(), tc.detail)
			}
		})
	}
}

func TestWarnMonitorRollbackBlockers(t *testing.T) {
	str := func(s string) *string { return &s }
	shared := client.RollbackBlocker{Code: "no_current_release", Message: "Promote twice through the pipeline."}
	own := client.RollbackBlocker{Code: "no_trip_keys", Message: "No routing key can trip self-healing."}
	needsAuto := client.RollbackBlocker{Code: "monitor_auto_needs_rollback_auto", Message: "rollback isn't auto."}
	for _, tc := range []struct {
		name string
		sh   *client.SelfHealing
		want []string
	}{
		{name: "no answer", sh: nil},
		{name: "older server", sh: &client.SelfHealing{Rollback: str("auto")}},
		{name: "report lists blockers but warns of none", sh: &client.SelfHealing{Rollback: str("report"),
			MonitorRollback: str("report"), MonitorRollbackBlockers: []client.RollbackBlocker{shared, own}}},
		{name: "auto beside report: every blocker", sh: &client.SelfHealing{Rollback: str("report"), MonitorRollback: str("auto"),
			RollbackBlockers: []client.RollbackBlocker{shared}, MonitorRollbackBlockers: []client.RollbackBlocker{needsAuto, shared, own}},
			want: []string{needsAuto.Code, shared.Code, own.Code}},
		// The rollback warning already covers the shared one.
		{name: "auto beside auto: the monitor's own", sh: &client.SelfHealing{Rollback: str("auto"), MonitorRollback: str("auto"),
			RollbackBlockers: []client.RollbackBlocker{shared}, MonitorRollbackBlockers: []client.RollbackBlocker{shared, own}},
			want: []string{own.Code}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			warnMonitorRollbackBlockers(tc.sh, "APP", &diags)
			if diags.WarningsCount() != len(tc.want) {
				t.Fatalf("expected %d warnings, got %v", len(tc.want), diags.Warnings())
			}
			for i, w := range diags.Warnings() {
				if want := "A monitor cannot roll back project APP right now: " + tc.want[i]; w.Summary() != want {
					t.Fatalf("warning %d = %q, want %q", i, w.Summary(), want)
				}
			}
		})
	}
}

func TestSelfHealingToObject_monitorRollback(t *testing.T) {
	ctx := context.Background()
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		name string
		sh   client.SelfHealing
		want types.String
	}{
		{name: "reported", sh: client.SelfHealing{MonitorRollback: str("report")}, want: types.StringValue("report")},
		{name: "older server", sh: client.SelfHealing{}, want: types.StringNull()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			obj := selfHealingToObject(&tc.sh, &diags)
			var m selfHealingModel
			diags.Append(obj.As(ctx, &m, objectAsOptions)...)
			if diags.HasError() {
				t.Fatal(diags)
			}
			if !m.MonitorRollback.Equal(tc.want) {
				t.Fatalf("monitor_rollback = %s, want %s", m.MonitorRollback, tc.want)
			}
		})
	}
}
