package provider

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const projectRes = "flightdeck_project.test"

func projectConfig(env *testEnv, identifier, body string) string {
	return env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_project" "test" {
  identifier = %q
%s
}
`, identifier, body)
}

func TestProject_basicLifecycle(t *testing.T) {
	env := newTestEnv(t, "project")
	identifier := randIdentifier()
	name := randName("Project")
	var firstID string

	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Create with a subset of features.
				Config: projectConfig(env, identifier, fmt.Sprintf(`
  name        = %q
  description = "Created by the provider test suite"
  features = {
    intake = true
  }
`, name)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(projectRes, "id"),
					resource.TestCheckResourceAttr(projectRes, "name", name),
					resource.TestCheckResourceAttr(projectRes, "identifier", identifier),
					resource.TestCheckResourceAttr(projectRes, "description", "Created by the provider test suite"),
					resource.TestCheckResourceAttr(projectRes, "archived", "false"),
					resource.TestCheckResourceAttrSet(projectRes, "emoji"),
					resource.TestCheckResourceAttr(projectRes, "features.%", "1"),
					resource.TestCheckResourceAttr(projectRes, "features.intake", "true"),
					resource.TestCheckNoResourceAttr(projectRes, "github_repo_full_name"),
					resource.TestCheckResourceAttrSet(projectRes, "lock_version"),
					captureAttr(projectRes, "id", &firstID),
				),
			},
			{
				// Import by id: everything round-trips except features, which
				// import as the full settable set rather than the managed subset.
				ResourceName:            projectRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"features"},
			},
			{
				// Import by identifier (listing + match).
				ResourceName:            projectRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"features"},
				ImportStateIdFunc: func(*terraform.State) (string, error) {
					return identifier, nil
				},
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("expected 1 imported state, got %d", len(states))
					}
					if got := states[0].Attributes["features.%"]; got != strconv.Itoa(len(toggleableFeatures)) {
						return fmt.Errorf("import should populate every settable feature key, got %s keys", got)
					}
					if _, leaked := states[0].Attributes["features.self_healing"]; leaked {
						return fmt.Errorf("import must not include the read-only self_healing feature key")
					}
					return nil
				},
			},
			{
				// Update every mutable attribute in place (same id).
				Config: projectConfig(env, identifier, fmt.Sprintf(`
  name                  = %q
  description           = "Renamed"
  emoji                 = "🚀"
  archived              = true
  github_repo_full_name = "example-org/mobile-app"
  features = {
    intake = false
    errors = true
  }
`, name+" v2")),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(projectRes, "id", &firstID),
					resource.TestCheckResourceAttr(projectRes, "name", name+" v2"),
					resource.TestCheckResourceAttr(projectRes, "description", "Renamed"),
					resource.TestCheckResourceAttr(projectRes, "emoji", "🚀"),
					resource.TestCheckResourceAttr(projectRes, "archived", "true"),
					resource.TestCheckResourceAttr(projectRes, "github_repo_full_name", "example-org/mobile-app"),
					resource.TestCheckResourceAttr(projectRes, "features.%", "2"),
					resource.TestCheckResourceAttr(projectRes, "features.intake", "false"),
					resource.TestCheckResourceAttr(projectRes, "features.errors", "true"),
				),
			},
			{
				// Removing optional attributes clears them server-side; dropping
				// the features block stops managing features without touching them.
				Config: projectConfig(env, identifier, fmt.Sprintf(`
  name     = %q
  archived = false
`, name+" v2")),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(projectRes, "id", &firstID),
					resource.TestCheckNoResourceAttr(projectRes, "description"),
					resource.TestCheckNoResourceAttr(projectRes, "github_repo_full_name"),
					resource.TestCheckResourceAttr(projectRes, "emoji", "🚀"),
					resource.TestCheckResourceAttr(projectRes, "archived", "false"),
					resource.TestCheckNoResourceAttr(projectRes, "features.%"),
				),
			},
		},
	})
}

func TestProject_changingIdentifierUpdatesInPlace(t *testing.T) {
	env := newTestEnv(t, "project")
	a, b := randIdentifier(), randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, a, `  name = "Identifier change"`),
				Check:  captureAttr(projectRes, "id", &id),
			},
			{
				Config: projectConfig(env, b, `  name = "Identifier change"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "identifier", b),
				),
			},
		},
	})
}

func TestProject_validation(t *testing.T) {
	env := newTestEnv(t, "project")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      projectConfig(env, "lowercase", `  name = "x"`),
				ExpectError: regexMust(`must be 1-10 uppercase letters/digits`),
			},
			{
				Config:      projectConfig(env, "TOOLONGIDENT", `  name = "x"`),
				ExpectError: regexMust(`must be 1-10 uppercase letters/digits`),
			},
			{
				Config: projectConfig(env, "OK", `
  name = "x"
  features = {
    not_a_feature = true
  }`),
				ExpectError: regexMust(`not_a_feature`),
			},
			{
				Config: projectConfig(env, "OK", `
  name                  = "x"
  github_repo_full_name = "no-slash"`),
				ExpectError: regexMust(`owner/repo`),
			},
		},
	})
}

func TestProject_duplicateIdentifierIsAServerValidationError(t *testing.T) {
	env := newTestEnv(t, "project")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `  name = "first"`),
			},
			{
				Config: projectConfig(env, identifier, `  name = "first"`) + fmt.Sprintf(`
resource "flightdeck_project" "dupe" {
  identifier = %q
  name       = "second"
}
`, identifier),
				ExpectError: regexMust(`(?s)Error creating Flightdeck project.*HTTP 422 \(validation_failed\).*Identifier`),
			},
		},
	})
}

func TestProject_driftDetection(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `  name = "Drift"`),
				Check:  captureAttr(projectRes, "id", &id),
			},
			{
				// Out-of-band rename: refresh picks up the new name and plans an update.
				PreConfig: func() {
					pid, _ := strconv.ParseInt(id, 10, 64)
					env.fake.TouchProject(pid, "Renamed elsewhere")
				},
				Config: projectConfig(env, identifier, `  name = "Drift"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "name", "Drift"),
			},
			{
				// Out-of-band delete (the web UI's soft-mark): the API 404s, the
				// resource leaves state, and the plan recreates it.
				PreConfig: func() {
					pid, _ := strconv.ParseInt(id, 10, 64)
					env.fake.DeleteProjectOutOfBand(pid)
				},
				Config: projectConfig(env, identifier, `  name = "Drift"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionCreate)},
				},
				Check: func(s *terraform.State) error {
					rs := s.RootModule().Resources[projectRes]
					if rs.Primary.ID == id {
						return fmt.Errorf("expected a new project id after out-of-band delete, still %s", id)
					}
					return nil
				},
			},
		},
	})
}

func TestProject_staleLockVersionIsReportedNotOverwritten(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `  name = "Stale"`),
				Check:  captureAttr(projectRes, "id", &id),
			},
			{
				// The plan is computed against a fresh read, then someone else
				// writes before our apply lands. The fake exposes that race via
				// a hook that fires after refresh but before apply.
				PreConfig: func() {
					pid, _ := strconv.ParseInt(id, 10, 64)
					env.fake.OnNextRequest("PATCH", "/api/v1/projects/"+id, func() {
						env.fake.TouchProject(pid, "Won by partner")
					})
				},
				Config:      projectConfig(env, identifier, `  name = "Stale v2"`),
				ExpectError: regexMust(`(?s)modified outside of Terraform.*lock_version\s+0,\s+the\s+server\s+now\s+has\s+1.*Nothing\s+was\s+overwritten`),
			},
			{
				// Re-planning picks up the other writer's change and succeeds.
				Config: projectConfig(env, identifier, `  name = "Stale v2"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "name", "Stale v2"),
					resource.TestCheckResourceAttr(projectRes, "lock_version", "2"),
				),
			},
		},
	})
	if got := env.fake.Project(mustInt(id)); got == nil || got.Name != "Stale v2" {
		t.Fatalf("final project state %+v", got)
	}
}

func TestProject_createSendsIdempotencyKeyAndSurvivesThrottling(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	// Two 429s, then an in-flight 409, before the create lands.
	env.fake.ThrottleNext(2, 0)
	env.fake.InFlightNext(1)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `  name = "Throttled"`),
				Check:  resource.TestCheckResourceAttr(projectRes, "name", "Throttled"),
			},
		},
	})
	posts := env.fake.RequestsMatching("POST", "/api/v1/projects")
	if len(posts) != 4 {
		t.Fatalf("expected 4 POST attempts (2x429, 1x409 in-flight, 1 success), got %d", len(posts))
	}
	key := posts[0].Header.Get("Idempotency-Key")
	if key == "" {
		t.Fatal("create sent no Idempotency-Key")
	}
	for i, p := range posts {
		if p.Header.Get("Idempotency-Key") != key {
			t.Errorf("attempt %d used a different Idempotency-Key: %q vs %q", i, p.Header.Get("Idempotency-Key"), key)
		}
	}
	// One project, not four.
	var live int
	for _, r := range env.fake.RequestsMatching("POST", "/api/v1/projects") {
		if r.Status == 201 {
			live++
		}
	}
	if live != 1 {
		t.Errorf("expected exactly one 201, got %d", live)
	}
}

func TestProject_updateSendsIfMatch(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: projectConfig(env, identifier, `  name = "v1"`)},
			{Config: projectConfig(env, identifier, `  name = "v2"`)},
		},
	})
	patches := env.fake.RequestsMatching("PATCH", "/api/v1/projects/")
	if len(patches) != 1 {
		t.Fatalf("expected 1 PATCH, got %d", len(patches))
	}
	if got := patches[0].Header.Get("If-Match"); got != `"0"` {
		t.Errorf("If-Match = %q, want the state's lock_version (\"0\")", got)
	}
}

func TestProject_destroyAndRecreateInsideIdempotencyWindow(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	var first string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `  name = "Replay"`),
				Check:  captureAttr(projectRes, "id", &first),
			},
			{
				// Destroy: the fake now holds a cached 201 for this identifier's key.
				Config: env.providerConfig(),
			},
			{
				// Recreate with the identical declaration. The stable key replays
				// the deleted project; the replay guard notices the 404 and creates
				// a fresh one instead of leaving a dead id in state.
				Config: projectConfig(env, identifier, `  name = "Replay"`),
				Check: func(s *terraform.State) error {
					got := s.RootModule().Resources[projectRes].Primary.ID
					if got == first {
						return fmt.Errorf("recreate returned the replayed, deleted project id %s", got)
					}
					return nil
				},
			},
		},
	})
}

// captureAttr stores an attribute's value for use in later steps.
func captureAttr(res, attr string, into *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[res]
		if !ok {
			return fmt.Errorf("%s not in state", res)
		}
		v, ok := rs.Primary.Attributes[attr]
		if !ok {
			return fmt.Errorf("%s has no attribute %s", res, attr)
		}
		*into = v
		return nil
	}
}

func mustInt(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}
