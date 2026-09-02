package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const webhookRes = "flightdeck_webhook.test"

func webhookConfig(env *testEnv, body string) string {
	return env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_webhook" "test" {
%s
}
`, body)
}

func TestWebhook_basicLifecycle(t *testing.T) {
	env := newTestEnv(t, "webhook")
	identifier := randIdentifier()
	var id, secret string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: webhookConfig(env, `
  url    = "https://ci.example.com/hooks/flightdeck"
  events = ["work_item.created", "work_item.state_changed"]`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(webhookRes, "id"),
					resource.TestCheckNoResourceAttr(webhookRes, "project_id"),
					resource.TestCheckResourceAttr(webhookRes, "url", "https://ci.example.com/hooks/flightdeck"),
					resource.TestCheckResourceAttr(webhookRes, "events.#", "2"),
					resource.TestCheckTypeSetElemAttr(webhookRes, "events.*", "work_item.created"),
					resource.TestCheckTypeSetElemAttr(webhookRes, "events.*", "work_item.state_changed"),
					resource.TestCheckResourceAttr(webhookRes, "active", "true"),
					resource.TestCheckResourceAttrSet(webhookRes, "secret"),
					resource.TestCheckResourceAttrSet(webhookRes, "lock_version"),
					captureAttr(webhookRes, "id", &id),
					captureAttr(webhookRes, "secret", &secret),
				),
			},
			{
				// The generated secret survives a refresh even though reads omit it.
				RefreshState: true,
				Check:        resource.TestCheckResourceAttrPtr(webhookRes, "secret", &secret),
			},
			{
				ResourceName:            webhookRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"secret"},
			},
			{
				// Scope to a project, change events and url, deactivate — in place.
				Config: projectFixture(env, identifier) + `
resource "flightdeck_webhook" "test" {
  project_id = flightdeck_project.parent.id
  url        = "https://ci.example.com/hooks/flightdeck-v2"
  events     = ["intake.created", "intake.accepted", "intake.declined"]
  active     = false
}
`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(webhookRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(webhookRes, "id", &id),
					resource.TestCheckResourceAttrPair(webhookRes, "project_id", "flightdeck_project.parent", "id"),
					resource.TestCheckResourceAttr(webhookRes, "url", "https://ci.example.com/hooks/flightdeck-v2"),
					resource.TestCheckResourceAttr(webhookRes, "events.#", "3"),
					resource.TestCheckResourceAttr(webhookRes, "active", "false"),
					resource.TestCheckResourceAttrPtr(webhookRes, "secret", &secret),
				),
			},
			{
				// Dropping project_id widens back to the workspace.
				Config: projectFixture(env, identifier) + webhookConfig(env, `
  url    = "https://ci.example.com/hooks/flightdeck-v2"
  events = ["intake.created", "intake.accepted", "intake.declined"]
  active = false`)[len(env.providerConfig()):],
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(webhookRes, "id", &id),
					resource.TestCheckNoResourceAttr(webhookRes, "project_id"),
				),
			},
		},
	})
}

func TestWebhook_userSuppliedSecretReplacesOnChange(t *testing.T) {
	env := newTestEnv(t, "webhook")
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: webhookConfig(env, `
  url    = "https://ci.example.com/hooks/a"
  events = ["project.updated"]
  secret = "shhh-one"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(webhookRes, "secret", "shhh-one"),
					captureAttr(webhookRes, "id", &id),
				),
			},
			{
				Config: webhookConfig(env, `
  url    = "https://ci.example.com/hooks/a"
  events = ["project.updated"]
  secret = "shhh-two"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(webhookRes, plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(webhookRes, "secret", "shhh-two"),
					func(s *terraform.State) error {
						if s.RootModule().Resources[webhookRes].Primary.ID == id {
							return fmt.Errorf("secret change should have replaced the webhook")
						}
						return nil
					},
				),
			},
		},
	})
	if !env.live() {
		if w := env.fake.Webhook(mustInt(id)); w != nil {
			t.Fatalf("original webhook %s should have been deleted on replace", id)
		}
	}
}

func TestWebhook_validation(t *testing.T) {
	env := newTestEnv(t, "webhook")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: webhookConfig(env, `
  url    = "ftp://ci.example.com"
  events = ["project.updated"]`),
				ExpectError: regexMust(`must be an http\(s\) URL`),
			},
			{
				Config: webhookConfig(env, `
  url    = "https://ci.example.com"
  events = []`),
				ExpectError: regexMust(`set must contain at least 1 elements`),
			},
			{
				Config: webhookConfig(env, `
  url    = "https://ci.example.com"
  events = ["project.exploded"]`),
				ExpectError: regexMust(`value must be one of`),
			},
			{
				// Internal targets are refused by the API's SSRF screen.
				Config: webhookConfig(env, `
  url    = "http://localhost:9000/hook"
  events = ["project.updated"]`),
				ExpectError: regexMust(`(?s)HTTP 422 \(validation_failed\).*internal or private`),
			},
		},
	})
}

func TestWebhook_requiresWorkspaceAdmin(t *testing.T) {
	env := newTestEnv(t, "webhook")
	env.requireFake(t)
	env.fake.SetWorkspaceAdmin(false)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: webhookConfig(env, `
  url    = "https://ci.example.com/hooks/a"
  events = ["project.updated"]`),
				ExpectError: regexMust(`(?s)Error creating Flightdeck webhook.*HTTP 403 \(forbidden\).*lacks the project or workspace role`),
			},
		},
	})
}

func TestWebhook_staleWriteAndHeaders(t *testing.T) {
	env := newTestEnv(t, "webhook")
	env.requireFake(t)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: webhookConfig(env, `
  url    = "https://ci.example.com/hooks/a"
  events = ["project.updated"]`),
				Check: captureAttr(webhookRes, "id", &id),
			},
			{
				PreConfig: func() {
					env.fake.OnNextRequest("PATCH", "/api/v1/webhooks/"+id, func() { env.fake.TouchWebhook(mustInt(id), false) })
				},
				Config: webhookConfig(env, `
  url    = "https://ci.example.com/hooks/b"
  events = ["project.updated"]`),
				ExpectError: regexMust(`(?s)Webhook \d+ modified outside of Terraform.*state\s+has lock_version 0, the server now has 1`),
			},
		},
	})
	posts := env.fake.RequestsMatching("POST", "/api/v1/webhooks")
	if len(posts) != 1 || posts[0].Header.Get("Idempotency-Key") == "" {
		t.Errorf("webhook POSTs = %d, key present = %v", len(posts), len(posts) > 0 && posts[0].Header.Get("Idempotency-Key") != "")
	}
	patches := env.fake.RequestsMatching("PATCH", "/api/v1/webhooks/")
	if len(patches) != 1 || patches[0].Header.Get("If-Match") != `"0"` {
		t.Errorf("webhook PATCHes = %+v", patches)
	}
}

func TestWebhook_inactiveSurvivesImportWithoutADefault(t *testing.T) {
	env := newTestEnv(t, "webhook")
	paused := webhookConfig(env, `
  url    = "https://ci.example.com/hooks/paused"
  events = ["project.updated"]
  active = false`)
	unspecified := webhookConfig(env, `
  url    = "https://ci.example.com/hooks/paused"
  events = ["project.updated"]`)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: paused, Check: resource.TestCheckResourceAttr(webhookRes, "active", "false")},
			{ResourceName: webhookRes, ImportState: true, ImportStateVerify: true, ImportStateVerifyIgnore: []string{"secret"}},
			{
				Config: unspecified,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(webhookRes, "active", "false"),
			},
		},
	})
}
