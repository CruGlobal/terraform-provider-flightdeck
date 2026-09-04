package provider

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"

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
					resource.TestCheckResourceAttrSet(projectRes, "lead_id"),
					resource.TestCheckResourceAttr(projectRes, "network", "public_project"),
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
  name        = %q
  description = "Renamed"
  emoji       = "🚀"
  archived    = true
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
				// Read-only over the API; the plan-time error points at the resource
				// that manages the link.
				Config: projectConfig(env, "OK", `
  name                  = "x"
  github_repo_full_name = "example-org/app"`),
				ExpectError: regexMust(`(?s)Read-only attribute.*flightdeck_github_integration`),
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

func TestProject_archivedSurvivesImportWithoutADefault(t *testing.T) {
	env := newTestEnv(t, "project")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name     = "Archived"
  archived = true`),
				Check: resource.TestCheckResourceAttr(projectRes, "archived", "true"),
			},
			{
				ResourceName:            projectRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"features"},
			},
			{
				// An imported (or existing) archived project whose configuration
				// says nothing about `archived` must not be unarchived by a plan.
				Config: projectConfig(env, identifier, `  name = "Archived"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "archived", "true"),
			},
			{
				Config: projectConfig(env, identifier, `
  name     = "Archived"
  archived = false`),
				Check: resource.TestCheckResourceAttr(projectRes, "archived", "false"),
			},
		},
	})
}

func TestProject_featureOverrideIsADiffNotAnError(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	// The server keeps `incidents` off for every project (a plan-gated toggle).
	env.fake.ForceFeature("incidents", false)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Gated"
  features = {
    intake    = true
    incidents = true
  }`),
				// The apply succeeds (state records the request) with a warning,
				// and the next plan shows the server's override as a diff.
				ExpectNonEmptyPlan: true,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "features.intake", "true"),
					resource.TestCheckResourceAttr(projectRes, "features.incidents", "true"),
				),
			},
		},
	})
}

func TestProject_staleDiagnosticQuotesTheServer(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `  name = "Quoted"`),
				Check:  captureAttr(projectRes, "id", &id),
			},
			{
				PreConfig: func() {
					env.fake.OnNextRequest("PATCH", "/api/v1/projects/"+id, func() {
						env.fake.TouchProject(mustInt(id), "Won by partner")
					})
				},
				Config:      projectConfig(env, identifier, `  name = "Quoted v2"`),
				ExpectError: regexMust(`(?s)The API said:.*modified by someone else`),
			},
		},
	})
}

func TestProject_readOnlyFieldsReflectTheServer(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `  name = "Linked"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckNoResourceAttr(projectRes, "github_repo_full_name"),
					resource.TestCheckResourceAttr(projectRes, "network", "public_project"),
				),
			},
			{
				// Linked and made private from the web UI: both show up on refresh
				// as computed values, with nothing to reconcile.
				PreConfig: func() {
					env.fake.LinkGithubRepo(mustInt(id), "example-org/app")
					env.fake.SetNetwork(mustInt(id), "private_project")
				},
				Config: projectConfig(env, identifier, `  name = "Linked"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "github_repo_full_name", "example-org/app"),
					resource.TestCheckResourceAttr(projectRes, "network", "private_project"),
				),
			},
		},
	})
	// The project PATCH body must never carry the read-only key, the block that
	// lives on its own endpoint, or an unconfigured network.
	for _, r := range env.fake.RequestsMatching("PATCH", "/api/v1/projects/") {
		for _, k := range []string{"github_repo_full_name", "network", "self_healing"} {
			if strings.Contains(string(r.Body), `"`+k+`"`) {
				t.Errorf("PATCH body carried read-only key %s: %s", k, r.Body)
			}
		}
	}
}

func TestProject_leadIDIsSettableAndDefaultsToTheCreator(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	other := env.fake.Members()[1].ID
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `  name = "Led"`),
				Check:  resource.TestCheckResourceAttr(projectRes, "lead_id", "1"),
			},
			{
				Config: projectConfig(env, identifier, fmt.Sprintf(`
  name    = "Led"
  lead_id = %d`, other)),
				Check: resource.TestCheckResourceAttr(projectRes, "lead_id", fmt.Sprint(other)),
			},
			{
				// Unset keeps the current lead.
				Config: projectConfig(env, identifier, `  name = "Led"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config: projectConfig(env, identifier, `
  name    = "Led"
  lead_id = 999999`),
				ExpectError: regexMust(`HTTP 404 \(not_found\)`),
			},
		},
	})
}

func TestProject_networkIsWritableAndSentOnlyWhenChanged(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Exact enum spellings only; the API refuses anything else too.
				Config: projectConfig(env, identifier, `
  name    = "x"
  network = "private"`),
				ExpectError: regexMust(`value must be one of`),
			},
			{
				Config: projectConfig(env, identifier, `
  name    = "Private"
  network = "private_project"`),
				Check: resource.TestCheckResourceAttr(projectRes, "network", "private_project"),
			},
			{
				// A rename with network unchanged: the PATCH must not carry network
				// (re-sending private_project re-runs the server's membership guard).
				Config: projectConfig(env, identifier, `
  name    = "Private renamed"
  network = "private_project"`),
				Check: resource.TestCheckResourceAttr(projectRes, "network", "private_project"),
			},
			{
				Config: projectConfig(env, identifier, `
  name    = "Private renamed"
  network = "public_project"`),
				Check: resource.TestCheckResourceAttr(projectRes, "network", "public_project"),
			},
			{
				// Unset keeps the current value.
				Config: projectConfig(env, identifier, `  name = "Private renamed"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
	var creates, patches []flightdecktestRequest
	for _, r := range env.fake.Requests() {
		switch {
		case r.Method == "POST" && r.Path == "/api/v1/projects":
			creates = append(creates, r)
		case r.Method == "PATCH" && strings.HasPrefix(r.Path, "/api/v1/projects/") && !strings.HasSuffix(r.Path, "/self-healing"):
			patches = append(patches, r)
		}
	}
	if len(creates) != 1 || !strings.Contains(string(creates[0].Body), `"network":"private_project"`) {
		t.Fatalf("create body should carry network: %v", creates)
	}
	if len(patches) != 2 {
		t.Fatalf("expected 2 project PATCHes (rename, then visibility), got %d", len(patches))
	}
	if strings.Contains(string(patches[0].Body), `"network"`) {
		t.Errorf("rename PATCH re-sent network: %s", patches[0].Body)
	}
	if !strings.Contains(string(patches[1].Body), `"network":"public_project"`) {
		t.Errorf("visibility PATCH lacks network: %s", patches[1].Body)
	}
}

func TestProject_idempotencyKeyReusedIsAClearConflict(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	// Two declarations that would derive the same key never happen through the
	// provider (the key covers the payload), so drive the client directly.
	c, err := client.New(env.fake.URL, env.fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	fields := client.Fields{"name": "Fingerprinted", "identifier": randIdentifier()}
	if _, err := c.CreateProject(ctx, fields, "shared-key"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	fields["network"] = "private_project"
	_, err = c.CreateProject(ctx, fields, "shared-key")
	if !client.HasCode(err, client.CodeIdempotencyKeyReused) {
		t.Fatalf("expected idempotency_key_reused, got %v", err)
	}
	if apiErr, _ := client.AsError(err); apiErr.Retryable() {
		t.Error("idempotency_key_reused must not be retried")
	}
}

func TestProject_worksAgainstADeploymentWithoutErrorCodes(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	env.fake.OmitErrorCodes(true)
	identifier := randIdentifier()
	var id string
	// The in-flight replay window answers a keyed create with an uncoded 409;
	// the client retries it once from the prose.
	env.fake.InFlightNext(1)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `  name = "No codes"`),
				Check:  captureAttr(projectRes, "id", &id),
			},
			{
				// An uncoded 409 on an If-Match update is still reported as stale.
				PreConfig: func() {
					env.fake.OnNextRequest("PATCH", "/api/v1/projects/"+id, func() { env.fake.TouchProject(mustInt(id), "Elsewhere") })
				},
				Config:      projectConfig(env, identifier, `  name = "No codes v2"`),
				ExpectError: regexMust(`(?s)modified outside of Terraform.*Nothing\s+was\s+overwritten`),
			},
			{
				Config: projectConfig(env, identifier, `  name = "No codes v2"`),
			},
			{
				// A 404 without a code is still "gone".
				PreConfig: func() { env.fake.DeleteProjectOutOfBand(mustInt(id)) },
				Config:    projectConfig(env, identifier, `  name = "No codes v2"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionCreate)},
				},
			},
		},
	})
	var errors int
	for _, r := range env.fake.Requests() {
		if r.Status >= 400 {
			errors++
			if strings.Contains(string(r.Response), `"code"`) {
				t.Errorf("error body carried a code with OmitErrorCodes on: %s", r.Response)
			}
		}
	}
	if errors < 3 {
		t.Errorf("expected the 409 in-flight, the 409 stale and the 404 to have been served, saw %d error responses", errors)
	}
}
