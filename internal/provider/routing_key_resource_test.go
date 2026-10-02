package provider

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
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

// The API has no rotate route, so rotation is replacement. Terraform's default
// replacement destroys first: the old key is revoked, then a new one minted.
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

// Two keys in one project must each end up with a LIVE secret. The create's
// Idempotency-Key is a hash of the request body, and `name` is the only thing
// in that body, so before `name` was required two nameless declarations sent
// identical payloads: the second replayed the first, the replay carried no
// secret, and recovering from that once revoked the first resource's live key
// while the apply reported success. (Same-named keys are now refused; see
// TestRoutingKey_duplicateNameIsRefusedAndTheFirstKeyKept.)
func TestRoutingKey_siblingKeysAreIndependent(t *testing.T) {
	env := newTestEnv(t, "routing_key")
	env.requireFake(t)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectFixture(env, identifier) + `
resource "flightdeck_routing_key" "a" {
  project_id = flightdeck_project.parent.id
  name       = "Uptime monitor"
}

resource "flightdeck_routing_key" "b" {
  project_id = flightdeck_project.parent.id
  name       = "Synthetic checks"
}
`,
				Check: func(s *terraform.State) error {
					seen := map[string]bool{}
					for _, name := range []string{"flightdeck_routing_key.a", "flightdeck_routing_key.b"} {
						rs := s.RootModule().Resources[name].Primary
						row := env.fake.RoutingKey(mustInt(rs.ID))
						if row == nil {
							return fmt.Errorf("%s: routing key %s is not on the server", name, rs.ID)
						}
						if row.RevokedAt != nil {
							return fmt.Errorf("%s holds routing key %s, which is revoked on the server", name, rs.ID)
						}
						if secret := rs.Attributes["routing_key"]; secret == "" || seen[secret] {
							return fmt.Errorf("%s did not get a distinct live secret", name)
						} else {
							seen[secret] = true
						}
					}
					return nil
				},
			},
		},
	})
}

// A key with no name would share its payload, and so its Idempotency-Key, with
// every other nameless key in the project. The schema refuses it outright.
func TestRoutingKey_nameIsRequired(t *testing.T) {
	env := newTestEnv(t, "routing_key")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectFixture(env, identifier) + `
resource "flightdeck_routing_key" "unnamed" {
  project_id = flightdeck_project.parent.id
}
`,
				ExpectError: regexMust(`(?s)Missing required argument|"name" is required`),
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

// Two keys declared with the same name are one create to the API. The first
// key is live, so Flightdeck refuses the second create instead of replaying it
// without its secret, and the provider fails that resource without touching
// the first. Renaming the refused one is the fix, and leaves the first key as
// it was. An older Flightdeck replays the live key without its secret instead,
// and the provider reads that the same way.
func TestRoutingKey_duplicateNameIsRefusedAndTheFirstKeyKept(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			env := newTestEnv(t, "routing_key")
			if legacy {
				env.requireFake(t)
				env.fake.LegacySecretReplays(true)
			}
			identifier := randIdentifier()
			first := `
resource "flightdeck_routing_key" "a" {
  project_id = flightdeck_project.parent.id
  name       = "Same"
}
`
			second := func(name string) string {
				return fmt.Sprintf(`
resource "flightdeck_routing_key" "b" {
  project_id = flightdeck_project.parent.id
  name       = %q
}
`, name)
			}
			var id, secret string
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{
						Config: projectFixture(env, identifier) + first,
						Check: resource.ComposeAggregateTestCheckFunc(
							captureAttr("flightdeck_routing_key.a", "id", &id),
							captureAttr("flightdeck_routing_key.a", "routing_key", &secret),
						),
					},
					{
						Config:      projectFixture(env, identifier) + first + second("Same"),
						ExpectError: regexMust(`(?s)A routing key with this name already exists.*named "Same", it holds\s+that\s+key`),
					},
					{
						Config: projectFixture(env, identifier) + first + second("Other"),
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttrPtr("flightdeck_routing_key.a", "id", &id),
							resource.TestCheckResourceAttrPtr("flightdeck_routing_key.a", "routing_key", &secret),
							resource.TestCheckResourceAttrSet("flightdeck_routing_key.b", "routing_key"),
							func(s *terraform.State) error {
								if env.fake == nil {
									return nil
								}
								if row := env.fake.RoutingKey(mustInt(id)); row == nil || row.RevokedAt != nil {
									return fmt.Errorf("the first key %s is not live on the server: %+v", id, row)
								}
								for _, r := range env.fake.RequestsMatching("DELETE", "/api/v1/projects/") {
									if strings.Contains(r.Path, "/routing-keys/") {
										return fmt.Errorf("a routing key was revoked (%s): the refused create must not revoke anything", r.Path)
									}
								}
								return nil
							},
						),
					},
				},
			})
		})
	}
}

// The create took effect but its response was lost, so the client's retry
// meets the key it made itself. Nobody holds that key's value, so the provider
// revokes it and mints another, and the apply succeeds quietly.
func TestRoutingKey_lostCreateResponseIsRecovered(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			env := newTestEnv(t, "routing_key")
			env.requireFake(t)
			env.fake.LegacySecretReplays(legacy)
			identifier := randIdentifier()
			recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
				Steps: []resource.TestStep{
					{
						PreConfig: func() { env.fake.DropNextResponse("POST", "/routing-keys") },
						Config:    routingKeyConfig(env, identifier, `  name = "Lost"`),
						Check: func(s *terraform.State) error {
							lost := lostCreateID(t, env, "/routing-keys")
							rs := s.RootModule().Resources[routingKeyRes].Primary
							if rs.ID == fmt.Sprint(lost) {
								return fmt.Errorf("state recorded the key whose response was lost (%d)", lost)
							}
							if rs.Attributes["routing_key"] == "" {
								return fmt.Errorf("the recovered key has no value")
							}
							if row := env.fake.RoutingKey(lost); row == nil || row.RevokedAt == nil {
								return fmt.Errorf("the key whose response was lost (%d) should be revoked: %+v", lost, row)
							}
							if row := env.fake.RoutingKey(mustInt(rs.ID)); row == nil || row.RevokedAt != nil {
								return fmt.Errorf("the recorded key %s is not live", rs.ID)
							}
							return nil
						},
					},
				},
			})
			for _, w := range recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning) {
				t.Errorf("unexpected warning: %s: %s", w.Summary, w.Detail)
			}
		})
	}
}

// lostCreateID is the id the first POST to a path ending in suffix created,
// read from the response the fake recorded and then withheld.
func lostCreateID(t *testing.T, env *testEnv, suffix string) int64 {
	t.Helper()
	for _, r := range env.fake.RequestsMatching("POST", "/api/v1/") {
		if !strings.HasSuffix(r.Path, suffix) {
			continue
		}
		var body struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(r.Response, &body); err != nil || body.ID == 0 {
			t.Fatalf("the first create's response %q has no id: %v", r.Response, err)
		}
		return body.ID
	}
	t.Fatalf("no POST to %s was recorded", suffix)
	return 0
}

// With create_before_destroy the replacement mints first, so within 24 hours
// of the old key's create it sends that same create again while the old key
// is live. Flightdeck refuses it, the provider says why, and the old key is
// kept.
func TestRoutingKey_createBeforeDestroyReplacementWithinTheWindowIsRefused(t *testing.T) {
	env := newTestEnv(t, "routing_key")
	identifier := randIdentifier()
	cfg := projectFixture(env, identifier) + `
resource "flightdeck_routing_key" "test" {
  project_id = flightdeck_project.parent.id
  name       = "Rotating"

  lifecycle {
    create_before_destroy = true
  }
}
`
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: cfg,
				Check:  captureAttr(routingKeyRes, "id", &id),
			},
			{
				Taint:       []string{routingKeyRes},
				Config:      cfg,
				ExpectError: regexMust(`(?s)A routing key with this name already exists.*create_before_destroy` + "`" + `\s+replacement`),
			},
			{
				// Nothing was revoked: the old key is still the one in state,
				// tainted, so a replacement is still planned.
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(routingKeyRes, "id", &id),
					resource.TestCheckResourceAttrSet(routingKeyRes, "routing_key"),
				),
			},
		},
	})
}
