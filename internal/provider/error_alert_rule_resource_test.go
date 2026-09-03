package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const ruleRes = "flightdeck_error_alert_rule.test"

func ruleConfig(env *testEnv, identifier, body string) string {
	return env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_project" "parent" {
  name       = "Parent %s"
  identifier = %q
  features = {
    errors    = true
    incidents = true
  }
}

resource "flightdeck_error_alert_rule" "test" {
  project_id = flightdeck_project.parent.id
%s
}
`, identifier, identifier, body)
}

func TestErrorAlertRule_basicLifecycle(t *testing.T) {
	env := newTestEnv(t, "error_alert_rule")
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: ruleConfig(env, identifier, `
  name    = "New production errors"
  trigger = "new_group"
  condition = {
    min_level   = "error"
    environment = "production"
  }
  action = {
    notify_slack     = true
    create_work_item = true
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(ruleRes, "id"),
					resource.TestCheckResourceAttr(ruleRes, "name", "New production errors"),
					resource.TestCheckResourceAttr(ruleRes, "enabled", "true"),
					resource.TestCheckResourceAttr(ruleRes, "trigger", "new_group"),
					resource.TestCheckResourceAttr(ruleRes, "condition.min_level", "error"),
					resource.TestCheckResourceAttr(ruleRes, "condition.environment", "production"),
					resource.TestCheckNoResourceAttr(ruleRes, "condition.count"),
					resource.TestCheckResourceAttr(ruleRes, "action.notify_slack", "true"),
					resource.TestCheckResourceAttr(ruleRes, "action.create_work_item", "true"),
					resource.TestCheckResourceAttr(ruleRes, "action.notify_email", "false"),
					resource.TestCheckResourceAttr(ruleRes, "action.open_incident", "false"),
					resource.TestCheckNoResourceAttr(ruleRes, "action.webhook_url"),
					resource.TestCheckResourceAttrSet(ruleRes, "lock_version"),
					captureAttr(ruleRes, "id", &id),
				),
			},
			{
				ResourceName:      ruleRes,
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs := s.RootModule().Resources[ruleRes].Primary
					return rs.Attributes["project_id"] + "/" + rs.ID, nil
				},
			},
			{
				// Switch trigger, drop the condition, swap actions, disable.
				Config: ruleConfig(env, identifier, `
  name    = "Error storm"
  enabled = false
  trigger = "occurrence_threshold"
  condition = {
    count          = 50
    window_minutes = 10
  }
  action = {
    notify_webhook = true
    webhook_url    = "https://alerts.example.com/hooks/flightdeck"
  }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(ruleRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(ruleRes, "id", &id),
					resource.TestCheckResourceAttr(ruleRes, "enabled", "false"),
					resource.TestCheckResourceAttr(ruleRes, "trigger", "occurrence_threshold"),
					resource.TestCheckNoResourceAttr(ruleRes, "condition.min_level"),
					resource.TestCheckResourceAttr(ruleRes, "condition.count", "50"),
					resource.TestCheckResourceAttr(ruleRes, "condition.window_minutes", "10"),
					resource.TestCheckResourceAttr(ruleRes, "action.notify_slack", "false"),
					resource.TestCheckResourceAttr(ruleRes, "action.notify_webhook", "true"),
					resource.TestCheckResourceAttr(ruleRes, "action.webhook_url", "https://alerts.example.com/hooks/flightdeck"),
				),
			},
			{
				// No condition block at all: an empty condition, and no diff afterwards.
				Config: ruleConfig(env, identifier, `
  name    = "Error storm"
  trigger = "regression"
  action = {
    notify_email = true
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(ruleRes, "condition.%", "4"),
					resource.TestCheckNoResourceAttr(ruleRes, "condition.count"),
					resource.TestCheckResourceAttr(ruleRes, "action.notify_email", "true"),
					resource.TestCheckResourceAttr(ruleRes, "action.notify_webhook", "false"),
				),
			},
		},
	})
}

func TestErrorAlertRule_openIncidentWithEscalationPolicy(t *testing.T) {
	env := newTestEnv(t, "error_alert_rule")
	env.requireFake(t)
	identifier := randIdentifier()
	var projectID string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectFixture(env, identifier),
				Check:  captureAttr("flightdeck_project.parent", "id", &projectID),
			},
			{
				PreConfig: func() { env.fake.AddEscalationPolicy(mustInt(projectID), 77) },
				Config: ruleConfig(env, identifier, `
  name    = "Page on-call"
  trigger = "new_group"
  action = {
    open_incident        = true
    escalation_policy_id = 77
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(ruleRes, "action.open_incident", "true"),
					resource.TestCheckResourceAttr(ruleRes, "action.escalation_policy_id", "77"),
				),
			},
			{
				// A policy from another project is rejected server-side.
				Config: ruleConfig(env, identifier, `
  name    = "Page on-call"
  trigger = "new_group"
  action = {
    open_incident        = true
    escalation_policy_id = 78
  }`),
				ExpectError: regexMust(`(?s)HTTP 422 \(validation_failed\).*escalation policy`),
			},
		},
	})
}

func TestErrorAlertRule_planTimeValidation(t *testing.T) {
	env := newTestEnv(t, "error_alert_rule")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: ruleConfig(env, identifier, `
  name    = "x"
  trigger = "new_group"
  action  = {}`),
				ExpectError: regexMust(`No action enabled`),
			},
			{
				Config: ruleConfig(env, identifier, `
  name    = "x"
  trigger = "new_group"
  action = {
    notify_webhook = true
  }`),
				ExpectError: regexMust(`webhook_url required`),
			},
			{
				Config: ruleConfig(env, identifier, `
  name    = "x"
  trigger = "new_group"
  action = {
    notify_slack         = true
    escalation_policy_id = 1
  }`),
				ExpectError: regexMust(`escalation_policy_id requires open_incident`),
			},
			{
				Config: ruleConfig(env, identifier, `
  name    = "x"
  trigger = "sometimes"
  action = {
    notify_slack = true
  }`),
				ExpectError: regexMust(`value must be one of`),
			},
			{
				Config: ruleConfig(env, identifier, `
  name    = "x"
  trigger = "new_group"
  condition = {
    min_level = "loud"
  }
  action = {
    notify_slack = true
  }`),
				ExpectError: regexMust(`value must be one of`),
			},
		},
	})
}

func TestErrorAlertRule_openIncidentNeedsIncidentsFeature(t *testing.T) {
	env := newTestEnv(t, "error_alert_rule")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectFixture(env, identifier) + `
resource "flightdeck_error_alert_rule" "test" {
  project_id = flightdeck_project.parent.id
  name       = "x"
  trigger    = "new_group"
  action = {
    open_incident = true
  }
}
`,
				ExpectError: regexMust(`(?s)HTTP 422 \(validation_failed\).*Incident\s+management`),
			},
		},
	})
}

func TestErrorAlertRule_staleWriteIsReported(t *testing.T) {
	env := newTestEnv(t, "error_alert_rule")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	base := `
  name    = "Racy"
  trigger = "new_group"
  action = {
    notify_slack = true
  }`
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: ruleConfig(env, identifier, base), Check: captureAttr(ruleRes, "id", &id)},
			{
				PreConfig: func() {
					env.fake.OnNextRequest("PATCH", fmt.Sprintf("/api/v1/projects/%d/error-rules/%s", projectIDOf(env, identifier), id),
						func() { env.fake.TouchErrorAlertRule(mustInt(id), "Someone else") })
				},
				Config: ruleConfig(env, identifier, `
  name    = "Racy v2"
  trigger = "new_group"
  action = {
    notify_slack = true
  }`),
				ExpectError: regexMust(`(?s)Error alert rule "Racy" modified outside of Terraform`),
			},
		},
	})
}

func TestErrorAlertRule_disabledSurvivesImportWithoutADefault(t *testing.T) {
	env := newTestEnv(t, "error_alert_rule")
	identifier := randIdentifier()
	disabled := ruleConfig(env, identifier, `
  name    = "Paused"
  enabled = false
  trigger = "new_group"
  action = {
    notify_slack = true
  }`)
	unspecified := ruleConfig(env, identifier, `
  name    = "Paused"
  trigger = "new_group"
  action = {
    notify_slack = true
  }`)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: disabled, Check: resource.TestCheckResourceAttr(ruleRes, "enabled", "false")},
			{
				ResourceName: ruleRes, ImportState: true, ImportStateVerify: true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs := s.RootModule().Resources[ruleRes].Primary
					return rs.Attributes["project_id"] + "/" + rs.ID, nil
				},
			},
			{
				Config: unspecified,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(ruleRes, "enabled", "false"),
			},
		},
	})
}

func TestErrorAlertRule_webhookURLWithoutNotifyWebhookIsRejected(t *testing.T) {
	env := newTestEnv(t, "error_alert_rule")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: ruleConfig(env, identifier, `
  name    = "x"
  trigger = "new_group"
  action = {
    notify_slack = true
    webhook_url  = "https://alerts.example.com/hook"
  }`),
				ExpectError: regexMust(`webhook_url requires notify_webhook`),
			},
		},
	})
}
