package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/types"
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
					resource.TestCheckResourceAttr(ruleRes, "condition.%", "5"),
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

// expectPriorCondition asserts the value a planned change to a rule's
// condition starts from: the left side of the arrow in `terraform plan`.
type expectPriorCondition struct {
	res, key string
	want     any
}

func (e expectPriorCondition) CheckPlan(_ context.Context, req plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	for _, rc := range req.Plan.ResourceChanges {
		if rc.Address != e.res {
			continue
		}
		before, _ := rc.Change.Before.(map[string]any)
		condition, _ := before["condition"].(map[string]any)
		if got := condition[e.key]; got != e.want {
			resp.Error = fmt.Errorf("%s: planned change starts from condition.%s = %v, want %v", e.res, e.key, got, e.want)
		}
		return
	}
	resp.Error = fmt.Errorf("%s is not in the plan", e.res)
}

// storedRule reads the rule in state straight from the API, so a test can
// assert what Flightdeck stored rather than what the provider recorded.
func storedRule(env *testEnv, s *terraform.State) (*client.ErrorAlertRule, error) {
	rs, ok := s.RootModule().Resources[ruleRes]
	if !ok {
		return nil, fmt.Errorf("%s not in state", ruleRes)
	}
	c, err := client.New(env.endpoint, env.token)
	if err != nil {
		return nil, err
	}
	projectID, err := strconv.ParseInt(rs.Primary.Attributes["project_id"], 10, 64)
	if err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(rs.Primary.ID, 10, 64)
	if err != nil {
		return nil, err
	}
	return c.GetErrorAlertRule(context.Background(), projectID, id)
}

// checkStoredBrowserErrors asserts the stored condition.count_browser_errors:
// want is "true" or "false", or "" for a key the API did not store.
func checkStoredBrowserErrors(env *testEnv, want string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rule, err := storedRule(env, s)
		if err != nil {
			return err
		}
		got, stored := rule.Condition["count_browser_errors"]
		switch {
		case want == "" && stored:
			return fmt.Errorf("the API stored condition.count_browser_errors = %s, want the key absent", got)
		case want != "" && string(got) != want:
			return fmt.Errorf("the API stored condition.count_browser_errors = %q, want %s", got, want)
		}
		return nil
	}
}

func TestErrorAlertRule_countBrowserErrors(t *testing.T) {
	env := newTestEnv(t, "error_alert_rule")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// The recommended shape: a rule that opens incidents leaves browser errors out.
				Config: ruleConfig(env, identifier, `
  name    = "Page on server errors"
  trigger = "new_group"
  condition = {
    count_browser_errors = false
  }
  action = {
    open_incident = true
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(ruleRes, "condition.count_browser_errors", "false"),
					resource.TestCheckNoResourceAttr(ruleRes, "condition.min_level"),
					checkStoredBrowserErrors(env, "false"),
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
				// An explicit true is stored and reads back as true, not as unset.
				Config: ruleConfig(env, identifier, `
  name    = "Page on server errors"
  trigger = "new_group"
  condition = {
    min_level            = "error"
    count_browser_errors = true
  }
  action = {
    open_incident = true
  }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(ruleRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(ruleRes, "condition.count_browser_errors", "true"),
					resource.TestCheckResourceAttr(ruleRes, "condition.min_level", "error"),
					checkStoredBrowserErrors(env, "true"),
				),
			},
			{
				// Removing the line stops sending the key, so the rule counts every error.
				Config: ruleConfig(env, identifier, `
  name    = "Page on server errors"
  trigger = "new_group"
  condition = {
    min_level = "error"
  }
  action = {
    open_incident = true
  }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(ruleRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(ruleRes, "condition.count_browser_errors"),
					resource.TestCheckResourceAttr(ruleRes, "condition.min_level", "error"),
					checkStoredBrowserErrors(env, ""),
				),
			},
		},
	})
}

// A false set outside Terraform (in the console, or over the API) used to be
// invisible to the plan and silently cleared by the next update. It now reads
// back into state, so the plan shows the change before it is made.
func TestErrorAlertRule_countBrowserErrorsSetOutsideTerraformShowsInPlan(t *testing.T) {
	env := newTestEnv(t, "error_alert_rule")
	identifier := randIdentifier()
	config := ruleConfig(env, identifier, `
  name    = "Set in the console"
  trigger = "new_group"
  condition = {
    min_level = "error"
  }
  action = {
    notify_slack = true
  }`)
	var projectID, id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(ruleRes, "condition.count_browser_errors"),
					captureAttr(ruleRes, "project_id", &projectID),
					captureAttr(ruleRes, "id", &id),
				),
			},
			{
				PreConfig: func() {
					c, err := client.New(env.endpoint, env.token)
					if err != nil {
						t.Fatal(err)
					}
					ctx := context.Background()
					rule, err := c.GetErrorAlertRule(ctx, mustInt(projectID), mustInt(id))
					if err != nil {
						t.Fatal(err)
					}
					if _, err := c.UpdateErrorAlertRule(ctx, rule.ProjectID, rule.ID, client.Fields{
						"condition": map[string]any{"min_level": "error", "count_browser_errors": false},
					}, rule.LockVersion); err != nil {
						t.Fatalf("out-of-band update: %v", err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(ruleRes, plancheck.ResourceActionUpdate),
						// The planned value is always the configured null, so what
						// proves the drift was read is the value it changes FROM.
						expectPriorCondition{res: ruleRes, key: "count_browser_errors", want: false},
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(ruleRes, "condition.count_browser_errors"),
					resource.TestCheckResourceAttr(ruleRes, "condition.min_level", "error"),
					checkStoredBrowserErrors(env, ""),
				),
			},
		},
	})
}

// A Flightdeck from before the setting refuses the key as unknown. The
// provider sends it only when it is set, so every other rule keeps working.
func TestErrorAlertRule_countBrowserErrorsOnOlderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "error_alert_rule")
	env.requireFake(t)
	env.fake.SetErrorAlertRulesLegacy(true)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: ruleConfig(env, identifier, `
  name    = "Older server"
  trigger = "new_group"
  condition = {
    min_level = "error"
  }
  action = {
    notify_slack = true
  }`),
				Check: resource.TestCheckNoResourceAttr(ruleRes, "condition.count_browser_errors"),
			},
			{
				Config: ruleConfig(env, identifier, `
  name    = "Older server"
  trigger = "new_group"
  condition = {
    min_level            = "error"
    count_browser_errors = false
  }
  action = {
    notify_slack = true
  }`),
				ExpectError: regexMust(`(?s)HTTP 422 \(invalid_attribute\).*unknown\s+condition\s+key:\s+count_browser_errors`),
			},
		},
	})
}

func TestRawCountBrowserErrors(t *testing.T) {
	cases := []struct {
		raw  string
		want types.Bool
	}{
		{"", types.BoolNull()},
		{"null", types.BoolNull()},
		{`""`, types.BoolNull()},
		{`"  "`, types.BoolNull()},
		{"true", types.BoolValue(true)},
		{"false", types.BoolValue(false)},
		{`"false"`, types.BoolValue(false)},
		{`" OFF "`, types.BoolValue(false)},
		{`"0"`, types.BoolValue(false)},
		{`"f"`, types.BoolValue(false)},
		{`"true"`, types.BoolValue(true)},
		{`"on"`, types.BoolValue(true)},
		// The API counts browser errors for anything it cannot read as false,
		// including a JSON 0 (only the string "0" is a false spelling).
		{`"yes"`, types.BoolValue(true)},
		// Only ASCII whitespace is trimmed, so a no-break space leaves no
		// false reading, and the API counts browser errors.
		{`"\u00a0false"`, types.BoolValue(true)},
		{`"false\u0000"`, types.BoolValue(false)},
		{"0", types.BoolValue(true)},
		{"[]", types.BoolValue(true)},
	}
	for _, tc := range cases {
		var raw json.RawMessage
		if tc.raw != "" {
			raw = json.RawMessage(tc.raw)
		}
		if got := rawCountBrowserErrors(raw); !got.Equal(tc.want) {
			t.Errorf("rawCountBrowserErrors(%s) = %s, want %s", tc.raw, got, tc.want)
		}
	}
}
