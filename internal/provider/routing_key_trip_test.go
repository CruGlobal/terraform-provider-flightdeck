package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// A key made with neither setting reads the server's defaults. Adding an
// environment, changing it and turning the trip flag on are each an update of
// the same key, never a new one, and an import reads both back.
func TestRoutingKey_environmentAndTriggerRoundTrip(t *testing.T) {
	env := newTestEnv(t, "routing_key")
	identifier := randIdentifier()
	var id string
	update := resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(routingKeyRes, plancheck.ResourceActionUpdate)},
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: routingKeyConfig(env, identifier, `  name = "Latency monitor"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(routingKeyRes, "id", &id),
					resource.TestCheckNoResourceAttr(routingKeyRes, "environment"),
					resource.TestCheckResourceAttr(routingKeyRes, "self_healing_trigger", "false"),
				),
			},
			{
				Config: routingKeyConfig(env, identifier, `
  name        = "Latency monitor"
  environment = "staging"`),
				ConfigPlanChecks: update,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(routingKeyRes, "id", &id),
					resource.TestCheckResourceAttr(routingKeyRes, "environment", "staging"),
					resource.TestCheckResourceAttrSet(routingKeyRes, "routing_key"),
				),
			},
			{
				Config: routingKeyConfig(env, identifier, `
  name                 = "Latency monitor"
  environment          = "production"
  self_healing_trigger = true`),
				ConfigPlanChecks: update,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(routingKeyRes, "id", &id),
					resource.TestCheckResourceAttr(routingKeyRes, "environment", "production"),
					resource.TestCheckResourceAttr(routingKeyRes, "self_healing_trigger", "true"),
				),
			},
			{
				// Left out, both keep what the key has.
				Config: routingKeyConfig(env, identifier, `  name = "Latency monitor"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(routingKeyRes, "environment", "production"),
					resource.TestCheckResourceAttr(routingKeyRes, "self_healing_trigger", "true"),
				),
			},
			{
				ResourceName:            routingKeyRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"routing_key"},
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs := s.RootModule().Resources[routingKeyRes].Primary
					return rs.Attributes["project_id"] + "/" + rs.ID, nil
				},
			},
			{
				// Owned again, so false is sent.
				Config: routingKeyConfig(env, identifier, `
  name                 = "Latency monitor"
  self_healing_trigger = false`),
				ConfigPlanChecks: update,
				Check:            resource.TestCheckResourceAttr(routingKeyRes, "self_healing_trigger", "false"),
			},
		},
	})
}

// A trip key is what lets a project's monitor_rollback act: the project's
// monitor blockers lose no_trip_keys once a key has the flag and the
// environment production, and name a flagged key with another environment.
// The plan warns about that second case before it is applied.
func TestRoutingKey_tripKeyClearsTheMonitorBlocker(t *testing.T) {
	env := newTestEnv(t, "routing_key")
	identifier := randIdentifier()
	config := func(key string) string {
		return projectConfig(env, identifier, `
  name = "Trip key"
  self_healing = {
    rollback         = "report"
    monitor_rollback = "report"
  }`) + fmt.Sprintf(`
resource "flightdeck_routing_key" "test" {
  project_id = flightdeck_project.test.id
  name       = "Checkout latency"
%s
}
`, key)
	}
	blockers := func(want map[string]bool) resource.TestCheckFunc {
		return func(s *terraform.State) error {
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
			listed := map[string]bool{}
			var codes []string
			for _, b := range sh.MonitorRollbackBlockers {
				listed[b.Code] = true
				codes = append(codes, b.Code)
			}
			for code, present := range want {
				if listed[code] != present {
					return fmt.Errorf("monitor_rollback_blockers has %s = %t, want %t: %s", code, listed[code], present, strings.Join(codes, ", "))
				}
			}
			return nil
		}
	}
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config(`
  environment          = "staging"
  self_healing_trigger = true`),
				Check: blockers(map[string]bool{"no_trip_keys": true, "trip_key_environment": true}),
			},
			{
				Config: config(`
  environment          = "production"
  self_healing_trigger = true`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(routingKeyRes, plancheck.ResourceActionUpdate)},
				},
				Check: blockers(map[string]bool{"no_trip_keys": false, "trip_key_environment": false}),
			},
			{
				Config: config(`
  environment          = "production"
  self_healing_trigger = false`),
				Check: blockers(map[string]bool{"no_trip_keys": true, "trip_key_environment": false}),
			},
		},
	})
	if !anyDiagnostic(recorded.planWarnings(), `Routing key "Checkout latency" can't trip self-healing`,
		`has the environment "staging"`) {
		t.Fatalf("no plan warning for a trip key outside production: %s", describe(recorded.planWarnings()))
	}
	if anyDiagnostic(recorded.planWarnings(), "can't trip self-healing", `has the environment "production"`) {
		t.Fatalf("a plan warning for a trip key in production: %s", describe(recorded.planWarnings()))
	}
}

// A new key with the flag on and no environment will have none, and the plan
// that creates it says it can't trip anything. Plan only, so the warning can
// only have come from the create's own plan.
func TestRoutingKey_triggerWithNoEnvironmentWarns(t *testing.T) {
	env := newTestEnv(t, "routing_key")
	env.requireFake(t)
	identifier := randIdentifier()
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: routingKeyConfig(env, identifier, `
  name                 = "No environment"
  self_healing_trigger = true`),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
	if !anyDiagnostic(recorded.planWarnings(), `Routing key "No environment" can't trip self-healing`, "has no environment") {
		t.Fatalf("no plan warning for a trip key with no environment: %s", describe(recorded.planWarnings()))
	}
}

// A console change to either setting is drift while the configuration sets
// it, and is kept while it does not.
func TestRoutingKey_consoleChangesToEnvironmentAndTrigger(t *testing.T) {
	env := newTestEnv(t, "routing_key")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	owned := routingKeyConfig(env, identifier, `
  name                 = "Console"
  environment          = "production"
  self_healing_trigger = false`)
	unowned := routingKeyConfig(env, identifier, `  name = "Console"`)
	staging := "staging"
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: owned,
				Check:  captureAttr(routingKeyRes, "id", &id),
			},
			{
				PreConfig: func() {
					env.fake.SetRoutingKeyEnvironment(mustInt(id), &staging)
					env.fake.SetRoutingKeyTrigger(mustInt(id), true)
				},
				Config: owned,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(routingKeyRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(routingKeyRes, "environment", "production"),
					resource.TestCheckResourceAttr(routingKeyRes, "self_healing_trigger", "false"),
				),
			},
			{
				PreConfig: func() {
					env.fake.SetRoutingKeyEnvironment(mustInt(id), &staging)
					env.fake.SetRoutingKeyTrigger(mustInt(id), true)
				},
				Config: unowned,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(routingKeyRes, "environment", "staging"),
					resource.TestCheckResourceAttr(routingKeyRes, "self_healing_trigger", "true"),
				),
			},
		},
	})
}

// A configuration that names neither setting sends neither, on create or on
// a rename.
func TestRoutingKey_unsetSettingsAreNotSent(t *testing.T) {
	env := newTestEnv(t, "routing_key")
	env.requireFake(t)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: routingKeyConfig(env, identifier, `  name = "Plain"`)},
			{Config: routingKeyConfig(env, identifier, `  name = "Plain (renamed)"`)},
		},
	})
	writes := 0
	for _, method := range []string{"POST", "PATCH"} {
		for _, r := range env.fake.RequestsMatching(method, "/api/v1/projects/") {
			if !strings.Contains(r.Path, "/routing-keys") {
				continue
			}
			writes++
			for _, key := range []string{"environment", "self_healing_trigger"} {
				if strings.Contains(string(r.Body), key) {
					t.Fatalf("%s %s named %s although the configuration did not: %s", method, r.Path, key, r.Body)
				}
			}
		}
	}
	if writes < 2 {
		t.Fatalf("expected a create and an update, saw %d writes", writes)
	}
}

func TestRoutingKey_environmentValidation(t *testing.T) {
	env := newTestEnv(t, "routing_key")
	identifier := randIdentifier()
	var steps []resource.TestStep
	for _, tc := range []struct{ value, want string }{
		{"", `Blank value`},
		{"   ", `Blank value`},
		{" production", `Blank space at either end`},
		{"production\n", `Blank space at either end`},
		{strings.Repeat("é", 256), `256\s+characters\s+long;\s+Flightdeck\s+accepts\s+at\s+most\s+255`},
	} {
		steps = append(steps, resource.TestStep{
			Config: routingKeyConfig(env, identifier, fmt.Sprintf(`
  name        = "Bad environment"
  environment = %q`, tc.value)),
			ExpectError: regexMust(tc.want),
		})
	}
	// The longest environment Flightdeck takes, counted in characters.
	steps = append(steps, resource.TestStep{
		Config: routingKeyConfig(env, identifier, fmt.Sprintf(`
  name        = "Long environment"
  environment = %q`, strings.Repeat("é", 255))),
		Check: resource.TestCheckResourceAttr(routingKeyRes, "environment", strings.Repeat("é", 255)),
	})
	runTest(t, resource.TestCase{Steps: steps})
}

// A Flightdeck from before either setting reads both as null. Setting one on
// an existing key says the server is too old, and on a new key the error
// says what an unknown-key refusal means.
func TestRoutingKey_environmentAndTriggerOnAnOlderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "routing_key")
	env.requireFake(t)
	env.fake.SetRoutingKeyFieldsSupported(false)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: routingKeyConfig(env, identifier, `  name = "Older"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(routingKeyRes, "environment"),
					resource.TestCheckNoResourceAttr(routingKeyRes, "self_healing_trigger"),
				),
			},
			{
				Config: routingKeyConfig(env, identifier, `
  name                 = "Older"
  environment          = "production"
  self_healing_trigger = true`),
				ExpectError: regexMust(`(?s)This Flightdeck is too old for environment.*This Flightdeck is too old for\s+self_healing_trigger`),
			},
			{
				Config: routingKeyConfig(env, identifier, `  name = "Older"`) + `
resource "flightdeck_routing_key" "second" {
  project_id  = flightdeck_project.parent.id
  name        = "Older, second"
  environment = "production"
}
`,
				ExpectError: regexMust(`(?s)Error creating Flightdeck routing key.*If the API calls environment an\s+unknown key, this Flightdeck is older`),
			},
		},
	})
}
