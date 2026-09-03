package provider

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const ghRes = "flightdeck_github_integration.test"

func githubIntegrationConfig(env *testEnv, identifier, body string) string {
	return projectFixture(env, identifier) + fmt.Sprintf(`
resource "flightdeck_github_integration" "test" {
  project_id = flightdeck_project.parent.id
%s
}

data "flightdeck_project" "linked" {
  id         = flightdeck_project.parent.id
  depends_on = [flightdeck_github_integration.test]
}
`, body)
}

func TestGithubIntegration_flightdeckManaged(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// No secret: Flightdeck generates it and registers the webhook.
				Config: githubIntegrationConfig(env, identifier, fmt.Sprintf(`  repo_full_name = %q`, repo)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(ghRes, "id"),
					resource.TestCheckResourceAttr(ghRes, "repo_full_name", repo),
					resource.TestCheckResourceAttr(ghRes, "enabled", "true"),
					resource.TestCheckResourceAttr(ghRes, "webhook_registered", "true"),
					resource.TestCheckNoResourceAttr(ghRes, "secret"),
					resource.TestCheckResourceAttrSet(ghRes, "lock_version"),
					// Linking records the repository on the project.
					resource.TestCheckResourceAttr("data.flightdeck_project.linked", "github_repo_full_name", repo),
					captureAttr(ghRes, "id", &id),
				),
			},
			{
				ResourceName:      ghRes,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				// Disable in place.
				Config: githubIntegrationConfig(env, identifier, fmt.Sprintf(`
  repo_full_name = %q
  enabled        = false`, repo)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(ghRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(ghRes, "id", &id),
					resource.TestCheckResourceAttr(ghRes, "enabled", "false"),
					resource.TestCheckResourceAttr(ghRes, "lock_version", "1"),
				),
			},
			{
				// A new repository replaces the integration.
				Config: githubIntegrationConfig(env, identifier, fmt.Sprintf(`  repo_full_name = %q`, repo+"-v2")),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(ghRes, plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(ghRes, "repo_full_name", repo+"-v2"),
					resource.TestCheckResourceAttr("data.flightdeck_project.linked", "github_repo_full_name", repo+"-v2"),
				),
			},
			{
				// Unlink.
				Config: projectFixture(env, identifier),
			},
			{
				// Re-read the project: the API cleared the mapping on unlink, with
				// no provider-side action.
				Config: projectFixture(env, identifier) + `
data "flightdeck_project" "linked" {
  id = flightdeck_project.parent.id
}
`,
				Check: resource.TestCheckNoResourceAttr("data.flightdeck_project.linked", "github_repo_full_name"),
			},
		},
	})
}

func TestGithubIntegration_callerManagedSecretIsWriteOnly(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: githubIntegrationConfig(env, identifier, fmt.Sprintf(`
  repo_full_name = %q
  secret         = "caller-managed-secret-0123456789"`, repo)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(ghRes, "webhook_registered", "false"),
					resource.TestCheckResourceAttr(ghRes, "secret", "caller-managed-secret-0123456789"),
					captureAttr(ghRes, "id", &id),
				),
			},
			{
				// The secret is never read back: state keeps the configured value.
				RefreshState: true,
				Check:        resource.TestCheckResourceAttr(ghRes, "secret", "caller-managed-secret-0123456789"),
			},
			{
				// Import cannot recover it.
				ResourceName:            ghRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"secret"},
			},
			{
				// A new secret replaces the integration.
				Config: githubIntegrationConfig(env, identifier, fmt.Sprintf(`
  repo_full_name = %q
  secret         = "rotated-secret-0123456789abcdef"`, repo)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(ghRes, plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: func(s *terraform.State) error {
					if s.RootModule().Resources[ghRes].Primary.ID == id {
						return fmt.Errorf("secret change should have replaced the integration")
					}
					return nil
				},
			},
		},
	})
	if !env.live() {
		// The secret travelled only in create bodies, never in a PATCH.
		for _, r := range env.fake.Requests() {
			if r.Method == "PATCH" && strings.Contains(string(r.Body), "secret") {
				t.Errorf("secret sent on update: %s", r.Body)
			}
		}
		var posts int
		for _, r := range env.fake.Requests() {
			if r.Method == "POST" && strings.HasSuffix(r.Path, "/github-integrations") {
				posts++
				if !strings.Contains(string(r.Body), `"secret"`) {
					t.Errorf("caller-managed create lacked the secret: %s", r.Body)
				}
			}
		}
		if posts != 2 {
			t.Errorf("expected 2 creates, got %d", posts)
		}
	}
}

func TestGithubIntegration_apiRefusals(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	env.fake.MarkRepoUnreachable("example-org/unreachable")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Plan-time format check.
				Config:      githubIntegrationConfig(env, identifier, `  repo_full_name = "not-a-repo"`),
				ExpectError: regexMust(`must be "owner/repo"`),
			},
			{
				// The App cannot reach the repository: no row, a pointed error.
				Config:      githubIntegrationConfig(env, identifier, `  repo_full_name = "example-org/unreachable"`),
				ExpectError: regexMust(`(?s)GitHub App cannot reach the repository.*supply.*secret`),
			},
			{
				// A caller-managed secret must be at least 16 characters.
				Config: githubIntegrationConfig(env, identifier, `
  repo_full_name = "example-org/unreachable"
  secret         = "short"`),
				ExpectError: regexMust(`Secret too short`),
			},
			{
				// Caller-managed mode does not need the App to reach the repo.
				Config: githubIntegrationConfig(env, identifier, `
  repo_full_name = "example-org/unreachable"
  secret         = "mine-is-long-enough-0123456789"`),
				Check: resource.TestCheckResourceAttr(ghRes, "webhook_registered", "false"),
			},
			{
				// A blank secret selects the managed mode (an unset variable arrives
				// as ""); replacing the caller-managed link above.
				Config: githubIntegrationConfig(env, identifier, fmt.Sprintf(`
  repo_full_name = %q
  secret         = ""`, repo)),
				Check: resource.TestCheckResourceAttr(ghRes, "webhook_registered", "true"),
			},
			{
				// A repository is linked once across the workspace, enabled or not.
				Config: githubIntegrationConfig(env, identifier, fmt.Sprintf(`  repo_full_name = %q`, repo)) + fmt.Sprintf(`
resource "flightdeck_project" "other" {
  name       = "Other"
  identifier = %q
}

resource "flightdeck_github_integration" "dupe" {
  project_id     = flightdeck_project.other.id
  repo_full_name = %q
  depends_on     = [flightdeck_github_integration.test]
}
`, randIdentifier(), repo),
				ExpectError: regexMust(`(?s)Repository already linked.*already linked to a\s+Flightdeck project`),
			},
			{
				// One enabled integration per project.
				Config: githubIntegrationConfig(env, identifier, fmt.Sprintf(`  repo_full_name = %q`, repo)) + fmt.Sprintf(`
resource "flightdeck_github_integration" "second" {
  project_id     = flightdeck_project.parent.id
  repo_full_name = %q
  depends_on     = [flightdeck_github_integration.test]
}
`, repo+"-second"),
				ExpectError: regexMust(`(?s)HTTP 422 \(validation_failed\).*already has an enabled GitHub\s+integration`),
			},
		},
	})
	for _, r := range env.fake.Requests() {
		if r.Method == "POST" && strings.Contains(string(r.Body), "unreachable") && r.Status == 201 && !strings.Contains(string(r.Body), "secret") {
			t.Errorf("an unreachable Flightdeck-managed create must not create a row: %+v", r)
		}
	}
}

func TestGithubIntegration_staleWriteIsReported(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: githubIntegrationConfig(env, identifier, fmt.Sprintf(`  repo_full_name = %q`, repo)),
				Check:  captureAttr(ghRes, "id", &id),
			},
			{
				PreConfig: func() {
					env.fake.OnNextRequest("PATCH", "/api/v1/github-integrations/"+id, func() { env.fake.TouchGithubIntegration(mustInt(id), false) })
				},
				Config: githubIntegrationConfig(env, identifier, fmt.Sprintf(`
  repo_full_name = %q
  enabled        = false`, repo)),
				ExpectError: regexMust(`(?s)GitHub integration for .* modified outside of Terraform`),
			},
		},
	})
}

func TestGithubIntegration_requiresWorkspaceAdmin(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	env.fake.SetWorkspaceAdmin(false)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      githubIntegrationConfig(env, identifier, `  repo_full_name = "example-org/app"`),
				ExpectError: regexMust(`Linking a GitHub repository requires a workspace admin`),
			},
		},
	})
}

func TestGithubIntegration_reEnablingMirrorsTheProjectColumn(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: githubIntegrationConfig(env, identifier, fmt.Sprintf(`
  repo_full_name = %q
  enabled        = false`, repo)),
				Check: resource.TestCheckResourceAttr(ghRes, "enabled", "false"),
			},
			{
				Config: githubIntegrationConfig(env, identifier, fmt.Sprintf(`
  repo_full_name = %q
  enabled        = true`, repo)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(ghRes, "enabled", "true"),
					resource.TestCheckResourceAttr("data.flightdeck_project.linked", "github_repo_full_name", repo),
				),
			},
		},
	})
}
