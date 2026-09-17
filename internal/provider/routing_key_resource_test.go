package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const routingKeyRes = "flightdeck_routing_key.test"

func routingKeyConfig(env *testEnv, identifier, body string) string {
	return projectFixture(env, identifier) + fmt.Sprintf(`
resource "flightdeck_routing_key" "test" {
  project_id = flightdeck_project.parent.id
%s
}
`, body)
}

func TestRoutingKey_basicLifecycle(t *testing.T) {
	env := newTestEnv(t, "routing_key")
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: routingKeyConfig(env, identifier, `  name = "Uptime monitor"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(routingKeyRes, "id", &id),
					resource.TestCheckResourceAttr(routingKeyRes, "name", "Uptime monitor"),
					resource.TestCheckResourceAttrSet(routingKeyRes, "routing_key"),
					resource.TestCheckResourceAttrSet(routingKeyRes, "last_four"),
					resource.TestCheckResourceAttrSet(routingKeyRes, "masked"),
					// A key pages nobody until someone attaches a policy in the console.
					resource.TestCheckNoResourceAttr(routingKeyRes, "escalation_policy_id"),
				),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectSensitiveValue(routingKeyRes, tfjsonPath("routing_key"))},
				},
			},
			{
				// The secret survives a refresh: it is never re-read, so state
				// is the only copy.
				RefreshState: true,
				Check:        resource.TestCheckResourceAttrSet(routingKeyRes, "routing_key"),
			},
			{
				ResourceName:      routingKeyRes,
				ImportState:       true,
				ImportStateVerify: true,
				// The API returns a key's value only on create.
				ImportStateVerifyIgnore: []string{"routing_key"},
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs := s.RootModule().Resources[routingKeyRes].Primary
					return rs.Attributes["project_id"] + "/" + rs.ID, nil
				},
			},
			{
				// The name is editable in place; the key is not touched.
				Config: routingKeyConfig(env, identifier, `  name = "Uptime monitor (renamed)"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(routingKeyRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(routingKeyRes, "id", &id),
					resource.TestCheckResourceAttr(routingKeyRes, "name", "Uptime monitor (renamed)"),
				),
			},
		},
	})
}

// The API has no rotate route, so rotation is replacement: a new key is minted
// and the old one revoked.
func TestRoutingKey_rotationIsReplacement(t *testing.T) {
	env := newTestEnv(t, "routing_key")
	env.requireFake(t)
	identifier := randIdentifier()
	var id, secret string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: routingKeyConfig(env, identifier, `  name = "Rotating"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(routingKeyRes, "id", &id),
					captureAttr(routingKeyRes, "routing_key", &secret),
				),
			},
			{
				Taint:  []string{routingKeyRes},
				Config: routingKeyConfig(env, identifier, `  name = "Rotating"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(routingKeyRes, plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: func(s *terraform.State) error {
					rs := s.RootModule().Resources[routingKeyRes].Primary
					if rs.ID == id {
						return fmt.Errorf("expected a new routing key, still %s", rs.ID)
					}
					if rs.Attributes["routing_key"] == secret {
						return fmt.Errorf("expected a new secret after rotation")
					}
					// The old key is revoked, not deleted: its row survives.
					old := env.fake.RoutingKey(mustInt(id))
					if old == nil {
						return fmt.Errorf("the old routing key row was deleted; it should be kept as history")
					}
					if old.RevokedAt == nil {
						return fmt.Errorf("the old routing key was not revoked")
					}
					return nil
				},
			},
		},
	})
}

// A key revoked in the console is gone as a credential even though its row
// remains, so the provider must plan a replacement rather than keep using it.
func TestRoutingKey_revokedOutOfBandIsRecreated(t *testing.T) {
	env := newTestEnv(t, "routing_key")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: routingKeyConfig(env, identifier, `  name = "Revoked elsewhere"`),
				Check:  captureAttr(routingKeyRes, "id", &id),
			},
			{
				PreConfig: func() { env.fake.RevokeRoutingKeyOutOfBand(mustInt(id)) },
				Config:    routingKeyConfig(env, identifier, `  name = "Revoked elsewhere"`),
				Check: func(s *terraform.State) error {
					if rs := s.RootModule().Resources[routingKeyRes].Primary; rs.ID == id {
						return fmt.Errorf("expected the revoked key to be replaced, still %s", rs.ID)
					}
					return nil
				},
			},
		},
	})
}

// escalation_policy_id is reported but never written: attaching a policy is a
// console operation, and this provider does not manage on-call.
func TestRoutingKey_escalationPolicyIsReadOnly(t *testing.T) {
	env := newTestEnv(t, "routing_key")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: routingKeyConfig(env, identifier, `
  name                 = "Paging"
  escalation_policy_id = 7`),
				ExpectError: regexMust(`(?s)Read-only|read-only`),
			},
		},
	})
}

func TestRoutingKey_reportsAConsoleAttachedPolicy(t *testing.T) {
	env := newTestEnv(t, "routing_key")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: routingKeyConfig(env, identifier, `  name = "Paging"`),
				Check:  captureAttr(routingKeyRes, "id", &id),
			},
			{
				PreConfig:    func() { env.fake.AttachRoutingKeyEscalationPolicy(mustInt(id), 99) },
				RefreshState: true,
				Check:        resource.TestCheckResourceAttr(routingKeyRes, "escalation_policy_id", "99"),
			},
		},
	})
}

func TestRoutingKey_staleWriteIsReported(t *testing.T) {
	env := newTestEnv(t, "routing_key")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: routingKeyConfig(env, identifier, `  name = "Racy"`),
				Check:  captureAttr(routingKeyRes, "id", &id),
			},
			{
				PreConfig: func() {
					env.fake.OnNextRequest("PATCH",
						fmt.Sprintf("/api/v1/projects/%d/routing-keys/%s", projectIDOf(env, identifier), id),
						func() { env.fake.TouchRoutingKey(mustInt(id), "Someone else") })
				},
				Config:      routingKeyConfig(env, identifier, `  name = "Racy renamed"`),
				ExpectError: regexMust(`(?s)Routing key "Racy" modified outside of Terraform`),
			},
		},
	})
}
