package provider

import (
	"encoding/json"
	"fmt"
	"testing"

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
				ExpectError: regexMust(`(?s)Error updating Flightdeck project.*HTTP 403 \(forbidden\).*Workspace admin`),
			},
		},
	})
	if got := len(env.fake.RequestsMatching("PATCH", "/api/v1/projects/")); got != 1 {
		t.Errorf("expected exactly one PATCH (the rejected thresholds write), got %d", got)
	}
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
	patches := env.fake.RequestsMatching("PATCH", "/api/v1/projects/")
	if len(patches) != 1 {
		t.Fatalf("expected exactly one PATCH (the rename), got %d", len(patches))
	}
	var body map[string]map[string]any
	if err := json.Unmarshal(patches[0].Body, &body); err != nil {
		t.Fatalf("PATCH body: %v", err)
	}
	if _, has := body["project"]["self_healing"]; has {
		t.Fatalf("rename PATCH rewrote self_healing: %s", patches[0].Body)
	}
	if body["project"]["name"] != "Thresholds renamed" {
		t.Fatalf("PATCH body = %s", patches[0].Body)
	}
}
