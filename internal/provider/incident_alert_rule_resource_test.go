package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const incidentRuleRes = "flightdeck_incident_alert_rule.test"

func incidentRuleConfig(env *testEnv, identifier, body string) string {
	return env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_project" "parent" {
  name       = "Parent %s"
  identifier = %q
  features = {
    errors    = true
    incidents = true
  }
}

resource "flightdeck_incident_alert_rule" "test" {
  project_id = flightdeck_project.parent.id
%s
}
`, identifier, identifier, body)
}

func TestIncidentAlertRule_basicLifecycle(t *testing.T) {
	env := newTestEnv(t, "incident_alert_rule")
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: incidentRuleConfig(env, identifier, `
  name    = "Paged incidents"
  trigger = "incident_opened"
  condition = {
    min_severity = "error"
  }
  action = {
    notify_slack     = true
    create_work_item = true
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(incidentRuleRes, "id", &id),
					resource.TestCheckResourceAttr(incidentRuleRes, "name", "Paged incidents"),
					resource.TestCheckResourceAttr(incidentRuleRes, "enabled", "true"),
					resource.TestCheckResourceAttr(incidentRuleRes, "trigger", "incident_opened"),
					resource.TestCheckResourceAttr(incidentRuleRes, "condition.min_severity", "error"),
					resource.TestCheckNoResourceAttr(incidentRuleRes, "condition.count"),
					resource.TestCheckResourceAttr(incidentRuleRes, "action.notify_slack", "true"),
					resource.TestCheckResourceAttr(incidentRuleRes, "action.create_work_item", "true"),
					resource.TestCheckResourceAttr(incidentRuleRes, "action.notify_email", "false"),
					resource.TestCheckResourceAttrSet(incidentRuleRes, "lock_version"),
				),
			},
			{
				ResourceName:      incidentRuleRes,
				ImportState:       true,
				ImportStateVerify: true,
				// Import has no configuration to defer to, so it records every
				// severity of the priority table; the configuration above
				// manages none of them.
				ImportStateVerifyIgnore: []string{"action.priority_map"},
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs := s.RootModule().Resources[incidentRuleRes].Primary
					return rs.Attributes["project_id"] + "/" + rs.ID, nil
				},
			},
			{
				Config: incidentRuleConfig(env, identifier, `
  name    = "Paged incidents"
  trigger = "incident_repeated"
  condition = {
    min_severity   = "critical"
    count          = 3
    window_minutes = 30
  }
  action = {
    notify_slack = true
  }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(incidentRuleRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(incidentRuleRes, "id", &id),
					resource.TestCheckResourceAttr(incidentRuleRes, "trigger", "incident_repeated"),
					resource.TestCheckResourceAttr(incidentRuleRes, "condition.count", "3"),
					resource.TestCheckResourceAttr(incidentRuleRes, "condition.window_minutes", "30"),
					// The action object replaces, so create_work_item is now off.
					resource.TestCheckResourceAttr(incidentRuleRes, "action.create_work_item", "false"),
				),
			},
		},
	})
}

// The API stores only the priority rows that differ from its defaults, so a
// row set to its own default is dropped server-side. Reading the EFFECTIVE
// table and keeping the configured severities is what stops that being a
// perpetual diff.
func TestIncidentAlertRule_priorityMapRoundTrips(t *testing.T) {
	env := newTestEnv(t, "incident_alert_rule")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: incidentRuleConfig(env, identifier, `
  name    = "Priorities"
  trigger = "incident_opened"
  action = {
    file_intake = true
    priority_map = {
      critical = "urgent"
      warning  = "high"
    }
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					// "urgent" IS critical's default, so the API stores nothing
					// for it — and it must still read back.
					resource.TestCheckResourceAttr(incidentRuleRes, "action.priority_map.critical", "urgent"),
					resource.TestCheckResourceAttr(incidentRuleRes, "action.priority_map.warning", "high"),
					// Severities the configuration does not name are not recorded.
					resource.TestCheckNoResourceAttr(incidentRuleRes, "action.priority_map.info"),
					resource.TestCheckNoResourceAttr(incidentRuleRes, "action.priority_map.error"),
				),
			},
			{
				// Re-applying the same configuration plans nothing.
				Config: incidentRuleConfig(env, identifier, `
  name    = "Priorities"
  trigger = "incident_opened"
  action = {
    file_intake = true
    priority_map = {
      critical = "urgent"
      warning  = "high"
    }
  }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// A rule turned off stays off across an apply that does not mention `enabled`.
// This runs live too: it is the user-visible half of the omit-does-not-reset
// guarantee, and needs no out-of-band mutation to show it.
func TestIncidentAlertRule_omittedEnabledSurvives(t *testing.T) {
	env := newTestEnv(t, "incident_alert_rule")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: incidentRuleConfig(env, identifier, `
  name    = "Deliberately off"
  trigger = "incident_opened"
  enabled = false
  action  = { notify_email = true }`),
				Check: resource.TestCheckResourceAttr(incidentRuleRes, "enabled", "false"),
			},
			{
				// `enabled` is gone from the configuration and something else
				// changes. The rule must not come back on.
				Config: incidentRuleConfig(env, identifier, `
  name    = "Deliberately off, renamed"
  trigger = "incident_opened"
  action  = { notify_email = true }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(incidentRuleRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(incidentRuleRes, "name", "Deliberately off, renamed"),
					resource.TestCheckResourceAttr(incidentRuleRes, "enabled", "false"),
				),
			},
		},
	})
}

// The API half of the same guarantee: a rule disabled OUTSIDE Terraform must
// not be silently re-enabled by an unrelated apply. Needs the fake, because it
// turns the rule off behind the provider's back.
func TestIncidentAlertRule_omittedEnabledIsNotReset(t *testing.T) {
	env := newTestEnv(t, "incident_alert_rule")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: incidentRuleConfig(env, identifier, `
  name    = "Console toggled"
  trigger = "incident_opened"
  action = {
    notify_email = true
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(incidentRuleRes, "id", &id),
					resource.TestCheckResourceAttr(incidentRuleRes, "enabled", "true"),
				),
			},
			{
				// Someone disables it in the console, then Terraform changes
				// something else entirely.
				PreConfig: func() { env.fake.SetIncidentAlertRuleEnabled(mustInt(id), false) },
				Config: incidentRuleConfig(env, identifier, `
  name    = "Console toggled, renamed"
  trigger = "incident_opened"
  action = {
    notify_email = true
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(incidentRuleRes, "name", "Console toggled, renamed"),
					resource.TestCheckResourceAttr(incidentRuleRes, "enabled", "false"),
				),
			},
		},
	})
}

func TestIncidentAlertRule_validation(t *testing.T) {
	env := newTestEnv(t, "incident_alert_rule")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Error-rule triggers are not incident triggers.
				Config: incidentRuleConfig(env, identifier, `
  name    = "Wrong vocabulary"
  trigger = "new_group"
  action  = { notify_email = true }`),
				ExpectError: regexMust(`value must be one of`),
			},
			{
				Config: incidentRuleConfig(env, identifier, `
  name    = "No action"
  trigger = "incident_opened"
  action  = {}`),
				ExpectError: regexMust(`No action enabled`),
			},
			{
				Config: incidentRuleConfig(env, identifier, `
  name    = "Repeated without a count"
  trigger = "incident_repeated"
  action  = { notify_email = true }`),
				ExpectError: regexMust(`count required for incident_repeated`),
			},
			{
				Config: incidentRuleConfig(env, identifier, `
  name    = "Webhook without a URL"
  trigger = "incident_opened"
  action  = { notify_webhook = true }`),
				ExpectError: regexMust(`webhook_url required`),
			},
			{
				Config: incidentRuleConfig(env, identifier, `
  name    = "Window too short"
  trigger = "incident_repeated"
  condition = {
    count          = 2
    window_minutes = 1
  }
  action = { notify_email = true }`),
				ExpectError: regexMust(`(?s)window_minutes.*must be between 5`),
			},
			{
				Config: incidentRuleConfig(env, identifier, `
  name    = "Bad priority"
  trigger = "incident_opened"
  action = {
    notify_email = true
    priority_map = { critical = "screaming" }
  }`),
				ExpectError: regexMust(`value must be one of`),
			},
			{
				Config: incidentRuleConfig(env, identifier, `
  name    = "Bad severity key"
  trigger = "incident_opened"
  action = {
    notify_email = true
    priority_map = { fatal = "urgent" }
  }`),
				ExpectError: regexMust(`value must be one of`),
			},
		},
	})
}

// Creating a rule needs the project's incidents feature; the API refuses
// otherwise and the provider says which switch to flip.
func TestIncidentAlertRule_requiresIncidentsFeature(t *testing.T) {
	env := newTestEnv(t, "incident_alert_rule")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_project" "parent" {
  name       = "Parent %s"
  identifier = %q
  features = {
    incidents = false
  }
}

resource "flightdeck_incident_alert_rule" "test" {
  project_id = flightdeck_project.parent.id
  name       = "Too early"
  trigger    = "incident_opened"
  action     = { notify_email = true }
}
`, identifier, identifier),
				ExpectError: regexMust(`(?s)incidents feature is off|Incident management must be enabled`),
			},
		},
	})
}

func TestIncidentAlertRule_staleWriteIsReported(t *testing.T) {
	env := newTestEnv(t, "incident_alert_rule")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: incidentRuleConfig(env, identifier, `
  name    = "Racy"
  trigger = "incident_opened"
  action  = { notify_email = true }`),
				Check: captureAttr(incidentRuleRes, "id", &id),
			},
			{
				PreConfig: func() {
					env.fake.OnNextRequest("PATCH",
						fmt.Sprintf("/api/v1/projects/%d/incident-rules/%s", projectIDOf(env, identifier), id),
						func() { env.fake.TouchIncidentAlertRule(mustInt(id), "Someone else") })
				},
				Config: incidentRuleConfig(env, identifier, `
  name    = "Racy renamed"
  trigger = "incident_opened"
  action  = { notify_email = true }`),
				ExpectError: regexMust(`(?s)Incident alert rule "Racy" modified outside of Terraform`),
			},
		},
	})
}
