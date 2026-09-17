package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

func TestProjectSelfHealing_thresholdsWithServerDefaults(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Self-healing"
  self_healing = {
    bake_minutes = 30
    burn_rate    = 10.0
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "30"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.burn_rate", "10"),
					// Unset thresholds come back with the server's documented defaults.
					resource.TestCheckResourceAttr(projectRes, "self_healing.sustain_count", "3"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.baseline_multiplier", "5"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.cooldown_minutes", "30"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.recovery_window_minutes", "15"),
					// Arming is console-only and reads false on a fresh project.
					resource.TestCheckResourceAttr(projectRes, "self_healing.armed", "false"),
				),
			},
			{
				ResourceName:            projectRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"features"},
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Self-healing"
  self_healing = {
    bake_minutes           = 45
    burn_rate              = 10.0
    max_rollbacks_per_hour = 2
  }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "45"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.max_rollbacks_per_hour", "2"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.sustain_count", "3"),
				),
			},
			{
				// Dropping the block leaves the server config alone and plans nothing.
				Config: projectConfig(env, identifier, `  name = "Self-healing"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "45"),
			},
		},
	})
}

func TestProjectSelfHealing_armedIsReadOnly(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Terraform itself refuses a value for a computed-only nested attribute.
				Config: projectConfig(env, identifier, `
  name = "Armed"
  self_healing = {
    armed = true
  }`),
				ExpectError: regexMust(`Invalid Configuration for Read-Only Attribute`),
			},
		},
	})
}

func TestProjectSelfHealing_reflectsConsoleArming(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Armed elsewhere"
  self_healing = {
    bake_minutes = 30
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "self_healing.armed", "false"),
				),
			},
			{
				// Armed from the console: the refresh picks it up as a computed
				// change and the plan stays empty (nothing to reconcile).
				PreConfig: func() { env.fake.ArmSelfHealing(mustInt(id), true) },
				Config: projectConfig(env, identifier, `
  name = "Armed elsewhere"
  self_healing = {
    bake_minutes = 30
  }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.armed", "true"),
			},
		},
	})
}

func TestProjectSelfHealing_nonAdminToken(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	env.requireFake(t)
	env.fake.SetWorkspaceAdmin(false)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Not reported to this token: the block is null and nothing is sent.
				Config: projectConfig(env, identifier, `  name = "Non-admin"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(projectRes, "self_healing.%"),
				),
			},
			{
				// Configuring thresholds without the role is a clear 403.
				Config: projectConfig(env, identifier, `
  name = "Non-admin"
  self_healing = {
    bake_minutes = 30
  }`),
				ExpectError: regexMust(`Self-healing configuration requires a workspace admin`),
			},
		},
	})
}

func TestProjectDataSource_selfHealing(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "DS"
  self_healing = {
    bake_minutes = 25
  }`) + fmt.Sprintf(`
data "flightdeck_project" "x" {
  identifier = %q
  depends_on = [flightdeck_project.test]
}
`, identifier),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.flightdeck_project.x", "self_healing.bake_minutes", "25"),
					resource.TestCheckResourceAttr("data.flightdeck_project.x", "self_healing.armed", "false"),
					resource.TestCheckResourceAttr("data.flightdeck_project.x", "self_healing.burn_rate", "14.4"),
				),
			},
		},
	})
}

func TestProjectSelfHealing_unrelatedUpdateDoesNotRewriteThresholds(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	env.requireFake(t)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Admin token: thresholds land in state.
				Config: projectConfig(env, identifier, `
  name = "Thresholds"
  self_healing = {
    bake_minutes = 30
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "30"),
			},
			{
				// Block removed from configuration and the project renamed: the
				// PATCH must carry the rename only.
				Config: projectConfig(env, identifier, `  name = "Thresholds renamed"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "name", "Thresholds renamed"),
					// State still knows the thresholds (read back, not rewritten).
					resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "30"),
				),
			},
		},
	})
	var projectPatches, selfHealingPatches int
	for _, r := range env.fake.RequestsMatching("PATCH", "/api/v1/projects/") {
		if strings.HasSuffix(r.Path, "/self-healing") {
			selfHealingPatches++
		} else {
			projectPatches++
		}
	}
	if projectPatches != 1 {
		t.Fatalf("expected exactly one project PATCH (the rename), got %d", projectPatches)
	}
	// Exactly one self-healing PATCH: the one in step 1 that set the threshold.
	if selfHealingPatches != 1 {
		t.Fatalf("expected the rename to leave self-healing alone, saw %d self-healing PATCHes", selfHealingPatches)
	}
}

func TestProjectSelfHealing_writesGoToTheOwnEndpointWithTheProjectLockVersion(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	env.requireFake(t)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Transport"
  self_healing = {
    bake_minutes = 30
    burn_rate    = 10.0
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "30"),
					// Created at 0, bumped once by the self-healing write.
					resource.TestCheckResourceAttr(projectRes, "lock_version", "1"),
				),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Transport"
  self_healing = {
    bake_minutes = 45
    burn_rate    = 10.0
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "45"),
					// Project PATCH (2) then self-healing PATCH (3).
					resource.TestCheckResourceAttr(projectRes, "lock_version", "3"),
				),
			},
		},
	})
	patches := env.fake.RequestsMatching("PATCH", "/api/v1/projects/")
	var shPatches []flightdecktestRequest
	for _, r := range patches {
		if strings.HasSuffix(r.Path, "/self-healing") {
			shPatches = append(shPatches, r)
		}
	}
	if len(shPatches) != 2 {
		t.Fatalf("expected 2 self-healing PATCHes, got %d", len(shPatches))
	}
	var body map[string]map[string]any
	if err := json.Unmarshal(shPatches[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	settings := body["self_healing"]
	if settings["bake_minutes"] != float64(30) || settings["burn_rate"] != float64(10) {
		t.Errorf("self-healing PATCH body = %s", shPatches[0].Body)
	}
	for k := range settings {
		if k == "armed" || k == "config" || k == "feature_enabled" {
			t.Errorf("self-healing PATCH must send threshold keys only, got %q", k)
		}
	}
	if shPatches[0].Header.Get("If-Match") != `"0"` || shPatches[1].Header.Get("If-Match") != `"2"` {
		t.Errorf("self-healing If-Match must be the project's current lock_version: %q, %q",
			shPatches[0].Header.Get("If-Match"), shPatches[1].Header.Get("If-Match"))
	}
}

func TestProjectSelfHealing_endpointAbsent(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	env.requireFake(t)
	env.fake.SetSelfHealingEndpoint(false)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// A Flightdeck without the endpoint: the block reads as null.
				Config: projectConfig(env, identifier, `  name = "Old server"`),
				Check:  resource.TestCheckNoResourceAttr(projectRes, "self_healing.%"),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Old server"
  self_healing = {
    bake_minutes = 30
  }`),
				ExpectError: regexMust(`Self-healing configuration is not available on this Flightdeck`),
			},
		},
	})
}

func TestProjectSelfHealing_thresholdValidation(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "x"
  self_healing = {
    max_rollbacks_per_hour = 0
  }`),
				ExpectError: regexMust(`(?s)value must be between 1 and\s+100`),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "x"
  self_healing = {
    burn_rate = 0
  }`),
				ExpectError: regexMust(`must be greater than 0`),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "x"
  self_healing = {
    short_window_minutes = 90
    long_window_minutes  = 60
  }`),
				ExpectError: regexMust(`Incoherent burn-rate windows`),
			},
		},
	})
}

type flightdecktestRequest = flightdecktest.RecordedRequest

func TestProjectSelfHealing_featureEnabledRoundTrips(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// A fresh project has the loop switched off.
				Config: projectConfig(env, identifier, `
  name = "Shadow"
  self_healing = {
    feature_enabled = false
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "self_healing.feature_enabled", "false"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.armed", "false"),
				),
			},
			{
				// True puts it in shadow mode; arming is a separate, console-only step.
				Config: projectConfig(env, identifier, `
  name = "Shadow"
  self_healing = {
    feature_enabled = true
  }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "self_healing.feature_enabled", "true"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.armed", "false"),
				),
			},
			{
				// ...and back off again.
				Config: projectConfig(env, identifier, `
  name = "Shadow"
  self_healing = {
    feature_enabled = false
  }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.feature_enabled", "false"),
			},
		},
	})
}

// Turning the feature on must not disturb a threshold the configuration does
// not mention: the endpoint merges, and the provider only sends what it is
// given. This is the failure the provider has shipped twice before.
func TestProjectSelfHealing_featureEnabledLeavesThresholdsAlone(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Merge"
  self_healing = {
    bake_minutes = 45
    burn_rate    = 7.5
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "45"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.burn_rate", "7.5"),
				),
			},
			{
				// Only feature_enabled and one threshold are named; burn_rate is
				// not, and must survive at its overridden value rather than
				// snapping back to the 14.4 default.
				Config: projectConfig(env, identifier, `
  name = "Merge"
  self_healing = {
    feature_enabled = true
    bake_minutes    = 50
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "self_healing.feature_enabled", "true"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "50"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.burn_rate", "7.5"),
				),
			},
		},
	})
}

// A write that names only thresholds must not carry feature_enabled, or
// Terraform would silently switch the loop off for anyone who never set it.
func TestProjectSelfHealing_featureEnabledOmittedIsNotSent(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	env.requireFake(t)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Untouched"
  self_healing = {
    bake_minutes = 33
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "33"),
			},
		},
	})
	for _, r := range env.fake.RequestsMatching("PATCH", "/api/v1/projects/") {
		if !strings.HasSuffix(r.Path, "/self-healing") {
			continue
		}
		if strings.Contains(string(r.Body), "feature_enabled") {
			t.Fatalf("self-healing write named feature_enabled although the configuration did not: %s", r.Body)
		}
	}
}

// warnSelfHealingWindows is what catches the case validateSelfHealingConfig
// cannot see: a configuration that raises one burn-rate window past a stored
// value it never mentions. The pair it judges comes from the PLAN, which fills
// a dropped threshold from prior state — the same value the API merges into.
func TestProjectSelfHealing_mergedWindowWarning(t *testing.T) {
	ctx := context.Background()
	object := func(shortW, longW types.Int64) types.Object {
		obj, d := types.ObjectValueFrom(ctx, selfHealingAttrTypes, selfHealingModel{
			ShortWindowMinutes: shortW,
			LongWindowMinutes:  longW,
		})
		if d.HasError() {
			t.Fatalf("building the block: %v", d)
		}
		return obj
	}
	null := types.Int64Null()

	for _, tc := range []struct {
		name          string
		config, plan  types.Object
		expectWarning bool
	}{
		{
			// The review's reproduction: long dropped from configuration,
			// short raised past the value the server still holds.
			name:          "one configured, merged pair incoherent",
			config:        object(types.Int64Value(60), null),
			plan:          object(types.Int64Value(60), types.Int64Value(10)),
			expectWarning: true,
		},
		{
			name:   "one configured, merged pair coherent",
			config: object(types.Int64Value(5), null),
			plan:   object(types.Int64Value(5), types.Int64Value(60)),
		},
		{
			// Both configured has no staleness question, so it is an error
			// from validateSelfHealingConfig rather than a warning here.
			name:   "both configured",
			config: object(types.Int64Value(60), types.Int64Value(10)),
			plan:   object(types.Int64Value(60), types.Int64Value(10)),
		},
		{
			// Neither is being written, so nothing changes server-side.
			name:   "neither configured",
			config: object(null, null),
			plan:   object(types.Int64Value(60), types.Int64Value(10)),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			warnSelfHealingWindows(ctx, tc.config, tc.plan, &diags)
			if diags.HasError() {
				t.Fatalf("expected no errors, got %v", diags.Errors())
			}
			if got := diags.WarningsCount() > 0; got != tc.expectWarning {
				t.Fatalf("warning = %v, want %v (diags: %v)", got, tc.expectWarning, diags)
			}
		})
	}
}

// The warning does not stand in front of the apply: the API still refuses the
// merged pair, and that refusal is what the operator ultimately sees.
func TestProjectSelfHealing_mergedWindowStillFailsAtApply(t *testing.T) {
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
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.long_window_minutes", "10"),
			},
			{
				// long_window_minutes leaves the configuration; short is raised
				// past the value the server still holds for it.
				Config: projectConfig(env, identifier, `
  name = "Windows"
  self_healing = {
    short_window_minutes = 60
  }`),
				ExpectError: regexMust(`(?s)short_window_minutes\s+cannot\s+exceed\s+long_window_minutes`),
			},
		},
	})
}

// SelfHealingThresholdKeys documents the endpoint's writable threshold set,
// but selfHealingFields writes its own list by hand — so the two can drift
// apart silently, and both are exported, which puts them out of reach of the
// unused-symbol linters. These two tests are what hold the invariant: one
// against the write path, one against the API's own answer.
func TestProjectSelfHealing_writePathMatchesThresholdKeys(t *testing.T) {
	ctx := context.Background()
	// Every threshold set, so selfHealingFields emits all of them.
	block, d := types.ObjectValueFrom(ctx, selfHealingAttrTypes, selfHealingModel{
		FeatureEnabled:        types.BoolValue(true),
		BakeMinutes:           types.Int64Value(20),
		BaselineMultiplier:    types.Float64Value(5),
		AbsoluteFloor:         types.Float64Value(5),
		LongWindowMinutes:     types.Int64Value(60),
		ShortWindowMinutes:    types.Int64Value(5),
		BurnRate:              types.Float64Value(14.4),
		SustainCount:          types.Int64Value(3),
		ConsecutiveErrorLimit: types.Int64Value(3),
		CooldownMinutes:       types.Int64Value(30),
		MaxRollbacksPerHour:   types.Int64Value(1),
		RecoveryWindowMinutes: types.Int64Value(15),
	})
	if d.HasError() {
		t.Fatalf("building the block: %v", d)
	}
	var diags diag.Diagnostics
	fields := selfHealingFields(ctx, block, &diags)
	if diags.HasError() {
		t.Fatalf("selfHealingFields: %v", diags.Errors())
	}

	want := append(append([]string{}, client.SelfHealingThresholdKeys...), "feature_enabled")
	got := make([]string, 0, len(fields))
	for k := range fields {
		got = append(got, k)
	}
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(want, ",") != strings.Join(got, ",") {
		t.Fatalf("the write path and SelfHealingThresholdKeys have drifted:\n  sends: %v\n   list: %v", got, want)
	}
	// `armed` is the one thing that must never be written.
	if _, sent := fields["armed"]; sent {
		t.Fatal("selfHealingFields sent `armed`; arming is console-only")
	}
}

// The same list, against the API's own writable_settings. Fake or live, the
// endpoint answers with what it will accept, so drift shows up here.
func TestProjectSelfHealing_thresholdKeysMatchTheAPI(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `  name = "Writable settings"`),
				Check: func(s *terraform.State) error {
					c, err := client.New(env.endpoint, env.token)
					if err != nil {
						return err
					}
					id, err := strconv.ParseInt(s.RootModule().Resources[projectRes].Primary.ID, 10, 64)
					if err != nil {
						return err
					}
					sh, err := c.GetSelfHealing(context.Background(), id)
					if err != nil {
						return err
					}
					want := append(append([]string{}, client.SelfHealingThresholdKeys...), "feature_enabled")
					got := append([]string{}, sh.WritableSettings...)
					sort.Strings(want)
					sort.Strings(got)
					if strings.Join(want, ",") != strings.Join(got, ",") {
						return fmt.Errorf("SelfHealingThresholdKeys no longer mirrors the API:\n  API: %v\n list: %v", got, want)
					}
					for _, k := range got {
						if k == "armed" {
							return fmt.Errorf("the API now reports `armed` as writable; arming is meant to be console-only")
						}
					}
					return nil
				},
			},
		},
	})
}
