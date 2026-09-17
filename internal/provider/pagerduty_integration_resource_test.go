package provider

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const pagerDutyRes = "flightdeck_pagerduty_integration.test"

// Fabricated keys. They only have to be non-blank and free of whitespace: the
// API deliberately does not check a routing key's shape.
const (
	pdKeyA = "pdtestkeyaaaa1111bbbb2222cccc3333"
	pdKeyB = "pdtestkeybbbb2222cccc3333dddd4444"
	// Shares its last four characters with pdKeyA, which is exactly what the
	// last-four comparison cannot see.
	pdKeyCollides = "pdtestkeyzzzz9999yyyy8888wwww3333"
)

func pagerDutyConfig(env *testEnv, identifier, body string) string {
	return projectFixture(env, identifier) + fmt.Sprintf(`
resource "flightdeck_pagerduty_integration" "test" {
  project_id = flightdeck_project.parent.id
%s
}
`, body)
}

func TestPagerDutyIntegration_basicLifecycle(t *testing.T) {
	env := newTestEnv(t, "pagerduty_integration")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: pagerDutyConfig(env, identifier, fmt.Sprintf(`
  routing_key  = %q
  min_severity = "error"`, pdKeyA)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(pagerDutyRes, "enabled", "true"),
					resource.TestCheckResourceAttr(pagerDutyRes, "min_severity", "error"),
					resource.TestCheckResourceAttr(pagerDutyRes, "routing_key_last_four", "3333"),
					resource.TestCheckResourceAttr(pagerDutyRes, "routing_key_masked", "…3333"),
					// A write-only argument is never persisted, so state has no
					// value for it at all.
					resource.TestCheckNoResourceAttr(pagerDutyRes, "routing_key"),
					resource.TestCheckResourceAttrSet(pagerDutyRes, "lock_version"),
				),
			},
			{
				// Re-applying the same key plans nothing: the last four match.
				Config: pagerDutyConfig(env, identifier, fmt.Sprintf(`
  routing_key  = %q
  min_severity = "error"`, pdKeyA)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config: pagerDutyConfig(env, identifier, fmt.Sprintf(`
  routing_key  = %q
  min_severity = "critical"
  enabled      = false
  service_id   = "PABC123"
  service_url  = "https://example.pagerduty.com/service-directory/PABC123"`, pdKeyA)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(pagerDutyRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(pagerDutyRes, "min_severity", "critical"),
					resource.TestCheckResourceAttr(pagerDutyRes, "enabled", "false"),
					resource.TestCheckResourceAttr(pagerDutyRes, "service_id", "PABC123"),
					// The credential is untouched by an unrelated update.
					resource.TestCheckResourceAttr(pagerDutyRes, "routing_key_last_four", "3333"),
				),
			},
			{
				// Dropping a display label clears it, rather than leaving it
				// behind on a merging endpoint.
				Config: pagerDutyConfig(env, identifier, fmt.Sprintf(`
  routing_key  = %q
  min_severity = "critical"
  enabled      = false`, pdKeyA)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(pagerDutyRes, "service_id"),
					resource.TestCheckNoResourceAttr(pagerDutyRes, "service_url"),
				),
			},
		},
	})
}

// Changing the key in configuration is invisible to Terraform on its own — a
// write-only value is null in the plan — so the provider compares the last
// four characters and plans the rotation itself.
func TestPagerDutyIntegration_rotationByLastFour(t *testing.T) {
	env := newTestEnv(t, "pagerduty_integration")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: pagerDutyConfig(env, identifier, fmt.Sprintf(`  routing_key = %q`, pdKeyA)),
				Check:  resource.TestCheckResourceAttr(pagerDutyRes, "routing_key_last_four", "3333"),
			},
			{
				Config: pagerDutyConfig(env, identifier, fmt.Sprintf(`  routing_key = %q`, pdKeyB)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(pagerDutyRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.TestCheckResourceAttr(pagerDutyRes, "routing_key_last_four", "4444"),
			},
		},
	})
}

// The blind spot, stated honestly: a new key sharing the old one's last four
// characters plans nothing until routing_key_version is bumped.
func TestPagerDutyIntegration_versionForcesAnInvisibleRotation(t *testing.T) {
	env := newTestEnv(t, "pagerduty_integration")
	env.requireFake(t)
	identifier := randIdentifier()
	var projectID int64
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: pagerDutyConfig(env, identifier, fmt.Sprintf(`
  routing_key         = %q
  routing_key_version = "1"`, pdKeyA)),
				Check: func(s *terraform.State) error {
					projectID = projectIDOf(env, identifier)
					if got := env.fake.PagerDutyLink(projectID).Plaintext; got != pdKeyA {
						return fmt.Errorf("stored key is %q, want the first key", got)
					}
					return nil
				},
			},
			{
				// Same last four, different key, version unchanged: nothing happens.
				Config: pagerDutyConfig(env, identifier, fmt.Sprintf(`
  routing_key         = %q
  routing_key_version = "1"`, pdKeyCollides)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Bumping the version forces the write through.
				Config: pagerDutyConfig(env, identifier, fmt.Sprintf(`
  routing_key         = %q
  routing_key_version = "2"`, pdKeyCollides)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(pagerDutyRes, plancheck.ResourceActionUpdate)},
				},
				Check: func(s *terraform.State) error {
					if got := env.fake.PagerDutyLink(projectID).Plaintext; got != pdKeyCollides {
						return fmt.Errorf("stored key is %q, want the rotated key", got)
					}
					return nil
				},
			},
		},
	})
}

// Somebody changes the credential in the Flightdeck console. The next plan
// must notice and put the configured key back.
func TestPagerDutyIntegration_consoleRotationIsCorrected(t *testing.T) {
	env := newTestEnv(t, "pagerduty_integration")
	env.requireFake(t)
	identifier := randIdentifier()
	var projectID int64
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: pagerDutyConfig(env, identifier, fmt.Sprintf(`  routing_key = %q`, pdKeyA)),
				Check: func(s *terraform.State) error {
					projectID = projectIDOf(env, identifier)
					return nil
				},
			},
			{
				PreConfig: func() { env.fake.RotatePagerDutyKeyOutOfBand(projectID, pdKeyB) },
				Config:    pagerDutyConfig(env, identifier, fmt.Sprintf(`  routing_key = %q`, pdKeyA)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(pagerDutyRes, plancheck.ResourceActionUpdate)},
				},
				Check: func(s *terraform.State) error {
					if got := env.fake.PagerDutyLink(projectID).Plaintext; got != pdKeyA {
						return fmt.Errorf("stored key is %q, want the configured key restored", got)
					}
					return nil
				},
			},
		},
	})
}

// The credential must never reach the state file, in any form.
func TestPagerDutyIntegration_keyIsNeverPersisted(t *testing.T) {
	env := newTestEnv(t, "pagerduty_integration")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: pagerDutyConfig(env, identifier, fmt.Sprintf(`  routing_key = %q`, pdKeyA)),
				Check: func(s *terraform.State) error {
					for name, rs := range s.RootModule().Resources {
						for attr, value := range rs.Primary.Attributes {
							if strings.Contains(value, pdKeyA) {
								return fmt.Errorf("the routing key was written to state at %s.%s", name, attr)
							}
						}
					}
					return nil
				},
			},
		},
	})
}

func TestPagerDutyIntegration_importCarriesNoKey(t *testing.T) {
	env := newTestEnv(t, "pagerduty_integration")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: pagerDutyConfig(env, identifier, fmt.Sprintf(`
  routing_key  = %q
  min_severity = "warning"`, pdKeyA)),
			},
			{
				ResourceName:      pagerDutyRes,
				ImportState:       true,
				ImportStateVerify: true,
				// The link is a singleton on its project and has no id of its
				// own, so the project is what identifies it.
				ImportStateVerifyIdentifierAttribute: "project_id",
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					return s.RootModule().Resources[pagerDutyRes].Primary.Attributes["project_id"], nil
				},
			},
		},
	})
}

func TestPagerDutyIntegration_validation(t *testing.T) {
	env := newTestEnv(t, "pagerduty_integration")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: pagerDutyConfig(env, identifier, fmt.Sprintf(`
  routing_key  = %q
  min_severity = "catastrophic"`, pdKeyA)),
				ExpectError: regexMust(`value must be one of`),
			},
			{
				Config: pagerDutyConfig(env, identifier, fmt.Sprintf(`
  routing_key = %q
  service_url = "ftp://example.com"`, pdKeyA)),
				ExpectError: regexMust(`must be an http\(s\) URL`),
			},
			{
				Config:      pagerDutyConfig(env, identifier, `  routing_key = ""`),
				ExpectError: regexMust(`(?s)string length must be at least 1`),
			},
		},
	})
}

// One credential per project: a second link is refused, and the message says
// to import instead.
func TestPagerDutyIntegration_secondLinkIsRefused(t *testing.T) {
	env := newTestEnv(t, "pagerduty_integration")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectFixture(env, identifier) + fmt.Sprintf(`
resource "flightdeck_pagerduty_integration" "test" {
  project_id  = flightdeck_project.parent.id
  routing_key = %q
}

resource "flightdeck_pagerduty_integration" "second" {
  project_id  = flightdeck_project.parent.id
  routing_key = %q
}
`, pdKeyA, pdKeyB),
				ExpectError: regexMust(`(?s)already has a PagerDuty link|already has a PagerDuty integration`),
			},
		},
	})
}
