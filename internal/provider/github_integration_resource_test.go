package provider

import (
	"fmt"
	"os"
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

// managedRepo returns a repository the managed mode can register a webhook on:
// any name against the fake; against a live instance the one named by
// FLIGHTDECK_ACC_GITHUB_REPO, or the test skips (the workspace's GitHub App
// must be able to reach it).
func managedRepo(t *testing.T, env *testEnv, fallback string) string {
	t.Helper()
	if !env.live() {
		return fallback
	}
	repo := os.Getenv(envAccGithubRepo)
	if repo == "" {
		t.Skipf("%s not set: the workspace needs a GitHub App credential and a reachable repository for the managed mode", envAccGithubRepo)
	}
	return repo
}

func TestGithubIntegration_flightdeckManaged(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	identifier := randIdentifier()
	repo := managedRepo(t, env, "example-org/"+strings.ToLower(identifier))
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
					// A new integration files nothing until the action is chosen.
					resource.TestCheckResourceAttr(ghRes, "ci_failure_action", "off"),
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
					// Two writes so far: the create, and recording the id of the
					// webhook it registered. This one makes three.
					resource.TestCheckResourceAttr(ghRes, "lock_version", "2"),
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

func TestGithubIntegration_repoChangeReplaces(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t) // a second reachable repository is not assumed live
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
				// A new repository replaces the integration and re-points the project.
				Config: githubIntegrationConfig(env, identifier, fmt.Sprintf(`  repo_full_name = %q`, repo+"-v2")),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(ghRes, plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(ghRes, "repo_full_name", repo+"-v2"),
					resource.TestCheckResourceAttr("data.flightdeck_project.linked", "github_repo_full_name", repo+"-v2"),
					func(s *terraform.State) error {
						if s.RootModule().Resources[ghRes].Primary.ID == id {
							return fmt.Errorf("repository change should have replaced the integration")
						}
						return nil
					},
				),
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
  repo_full_name    = %q
  secret            = "caller-managed-secret-0123456789"
  ci_failure_action = "file_intake"`, repo)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(ghRes, "webhook_registered", "false"),
					// Honoured on create, and read back by the refresh and import below.
					resource.TestCheckResourceAttr(ghRes, "ci_failure_action", "file_intake"),
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
				// The API creates every integration enabled; a planned false is
				// applied by an immediate follow-up update.
				Config: githubIntegrationConfig(env, identifier, fmt.Sprintf(`
  repo_full_name = %q
  enabled        = false`, repo)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(ghRes, "enabled", "false"),
					// The create, the hook-id write-back, then the follow-up that
					// applies the planned `enabled = false`.
					resource.TestCheckResourceAttr(ghRes, "lock_version", "2"),
				),
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

func TestGithubIntegration_createNeverSendsEnabled(t *testing.T) {
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
		},
	})
	var creates, patches int
	for _, r := range env.fake.Requests() {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.Path, "/github-integrations"):
			creates++
			if strings.Contains(string(r.Body), `"enabled"`) {
				t.Errorf("create body must not carry enabled (the API ignores it): %s", r.Body)
			}
			if strings.Contains(string(r.Body), `"ci_failure_action"`) {
				t.Errorf("an unconfigured action must not be sent, so the server default stands: %s", r.Body)
			}
		case r.Method == "PATCH" && strings.Contains(r.Path, "/github-integrations/"):
			patches++
			if !strings.Contains(string(r.Body), `"enabled":false`) {
				t.Errorf("follow-up PATCH should disable: %s", r.Body)
			}
		}
	}
	if creates != 1 || patches != 1 {
		t.Fatalf("expected 1 create + 1 disabling PATCH, got %d/%d", creates, patches)
	}
}

func TestGithubIntegration_existingHookIsNotClaimed(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	env.fake.MarkHookAlreadyPresent(repo)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Managed mode, but a hook already targets Flightdeck: the link is
				// made, nothing is registered or claimed, webhook_registered=false.
				Config: githubIntegrationConfig(env, identifier, fmt.Sprintf(`  repo_full_name = %q`, repo)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(ghRes, "enabled", "true"),
					resource.TestCheckResourceAttr(ghRes, "webhook_registered", "false"),
					resource.TestCheckResourceAttr("data.flightdeck_project.linked", "github_repo_full_name", repo),
				),
			},
		},
	})
}

// ciActionConfig omits the linked-project data source so a step can assert an
// empty plan without a deferred data read showing up in it.
func ciActionConfig(env *testEnv, identifier, repo, body string) string {
	return projectFixture(env, identifier) + fmt.Sprintf(`
resource "flightdeck_github_integration" "test" {
  project_id     = flightdeck_project.parent.id
  repo_full_name = %q
%s
}
`, repo, body)
}

func TestGithubIntegration_ciFailureActionIsSetOnCreateAndKeptWhenUnset(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t) // the console toggle and the request bodies need the fake
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	fileIntake := ciActionConfig(env, identifier, repo, `  ci_failure_action = "file_intake"`)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Plan-time enum check: nothing is linked.
				Config:      ciActionConfig(env, identifier, repo, `  ci_failure_action = "always"`),
				ExpectError: regexMust(`value must be one of`),
			},
			{
				// Read on create, unlike `enabled`: no follow-up update, so the
				// create and the hook-id write-back are the only two writes.
				Config: fileIntake,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(ghRes, "ci_failure_action", "file_intake"),
					resource.TestCheckResourceAttr(ghRes, "lock_version", "1"),
					captureAttr(ghRes, "id", &id),
				),
			},
			{
				// A toggle in the console is corrected while the attribute is
				// configured, because every update re-asserts it.
				PreConfig: func() { env.fake.SetGithubCIFailureAction(mustInt(id), "off") },
				Config:    fileIntake,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(ghRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.TestCheckResourceAttr(ghRes, "ci_failure_action", "file_intake"),
			},
			{
				// Dropping the line keeps the current value: the API merges, so
				// there is no reset to the default to plan.
				Config: ciActionConfig(env, identifier, repo, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(ghRes, "ci_failure_action", "file_intake"),
			},
			{
				// Switching it off is written, never implied.
				Config: ciActionConfig(env, identifier, repo, `  ci_failure_action = "off"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(ghRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.TestCheckResourceAttr(ghRes, "ci_failure_action", "off"),
			},
		},
	})
	var creates, patches int
	for _, r := range env.fake.Requests() {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.Path, "/github-integrations"):
			creates++
			if !strings.Contains(string(r.Body), `"ci_failure_action":"file_intake"`) {
				t.Errorf("create body should carry the configured action: %s", r.Body)
			}
		case r.Method == "PATCH" && strings.Contains(r.Path, "/github-integrations/"):
			patches++
		}
	}
	if creates != 1 {
		t.Errorf("expected 1 create, got %d", creates)
	}
	if patches != 2 {
		t.Errorf("expected 2 updates (the console correction and the switch off), got %d", patches)
	}
}
