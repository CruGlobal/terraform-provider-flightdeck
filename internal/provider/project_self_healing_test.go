package provider

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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
