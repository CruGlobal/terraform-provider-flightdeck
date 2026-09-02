package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const stateRes = "flightdeck_state.test"

// projectFixture renders a project the nested-resource tests hang off.
func projectFixture(env *testEnv, identifier string) string {
	return env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_project" "parent" {
  name       = "Parent %s"
  identifier = %q
}
`, identifier, identifier)
}

func stateConfig(env *testEnv, identifier, body string) string {
	return projectFixture(env, identifier) + fmt.Sprintf(`
resource "flightdeck_state" "test" {
  project_id = flightdeck_project.parent.id
%s
}
`, body)
}

func TestState_basicLifecycle(t *testing.T) {
	env := newTestEnv(t, "state")
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: stateConfig(env, identifier, `
  name  = "In Review"
  group = "started"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(stateRes, "id"),
					resource.TestCheckResourceAttrPair(stateRes, "project_id", "flightdeck_project.parent", "id"),
					resource.TestCheckResourceAttr(stateRes, "name", "In Review"),
					resource.TestCheckResourceAttr(stateRes, "group", "started"),
					resource.TestCheckResourceAttr(stateRes, "default", "false"),
					resource.TestCheckResourceAttrSet(stateRes, "color"),
					// Position is assigned by the server (end of the group).
					resource.TestCheckResourceAttrSet(stateRes, "position"),
					resource.TestCheckResourceAttrSet(stateRes, "lock_version"),
					captureAttr(stateRes, "id", &id),
				),
			},
			{
				ResourceName:      stateRes,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				Config: stateConfig(env, identifier, `
  name     = "Reviewing"
  group    = "started"
  color    = "#8b5cf6"
  position = 7`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(stateRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(stateRes, "id", &id),
					resource.TestCheckResourceAttr(stateRes, "name", "Reviewing"),
					resource.TestCheckResourceAttr(stateRes, "color", "#8b5cf6"),
					resource.TestCheckResourceAttr(stateRes, "position", "7"),
				),
			},
			{
				// Moving groups is an in-place update, not a replacement.
				Config: stateConfig(env, identifier, `
  name     = "Reviewing"
  group    = "completed"
  color    = "#8b5cf6"
  position = 7`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(stateRes, "id", &id),
					resource.TestCheckResourceAttr(stateRes, "group", "completed"),
				),
			},
		},
	})
}

func TestState_defaultMovesBetweenStates(t *testing.T) {
	env := newTestEnv(t, "state")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: stateConfig(env, identifier, `
  name    = "Triage"
  group   = "backlog"
  default = true`) + `
data "flightdeck_states" "all" {
  project_id = flightdeck_project.parent.id
  depends_on = [flightdeck_state.test]
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(stateRes, "default", "true"),
					// Exactly one default across the project: the seeded Backlog lost it.
					resource.TestCheckResourceAttr("data.flightdeck_states.all", "states.#", "6"),
					checkSingleDefault("data.flightdeck_states.all", "Triage"),
				),
			},
			{
				// Turning default off does not pick a new default; that is the
				// user's call, and the plan afterwards is still empty.
				Config: stateConfig(env, identifier, `
  name    = "Triage"
  group   = "backlog"
  default = false`),
				Check: resource.TestCheckResourceAttr(stateRes, "default", "false"),
			},
		},
	})
}

func TestState_deleteGuards(t *testing.T) {
	env := newTestEnv(t, "state")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: stateConfig(env, identifier, `
  name  = "Blocked"
  group = "started"`),
				Check: captureAttr(stateRes, "id", &id),
			},
			{
				// Work items sit in the state: the API refuses the delete and the
				// provider reports it instead of dropping the state silently.
				PreConfig:   func() { env.fake.MarkStateInUse(mustInt(id), true) },
				Config:      projectFixture(env, identifier),
				ExpectError: regexMust(`(?s)Error deleting Flightdeck state.*HTTP 422 \(validation_failed\).*dependent\s+work\s+items`),
			},
			{
				// Once the items are gone the delete goes through.
				PreConfig: func() { env.fake.MarkStateInUse(mustInt(id), false) },
				Config:    projectFixture(env, identifier),
			},
		},
	})
}

func TestState_staleWriteIsReported(t *testing.T) {
	env := newTestEnv(t, "state")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: stateConfig(env, identifier, `
  name  = "Racy"
  group = "started"`),
				Check: captureAttr(stateRes, "id", &id),
			},
			{
				PreConfig: func() {
					env.fake.OnNextRequest("PATCH", "/api/v1/states/"+id, func() { env.fake.TouchState(mustInt(id), "Someone else") })
				},
				Config: stateConfig(env, identifier, `
  name  = "Racy v2"
  group = "started"`),
				ExpectError: regexMust(`(?s)State "Racy" modified outside of Terraform.*Nothing was overwritten`),
			},
		},
	})
}

func TestState_validation(t *testing.T) {
	env := newTestEnv(t, "state")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: stateConfig(env, identifier, `
  name  = "x"
  group = "doing"`),
				ExpectError: regexMust(`value must be one of`),
			},
			{
				Config: stateConfig(env, identifier, `
  name  = "x"
  group = "started"
  color = "red"`),
				ExpectError: regexMust(`must be a hex color`),
			},
			{
				// Duplicate name within the project is a server-side 422.
				Config: stateConfig(env, identifier, `
  name  = "Backlog"
  group = "backlog"`),
				ExpectError: regexMust(`(?s)Error creating Flightdeck state.*HTTP 422 \(validation_failed\).*Name has already\s+been taken`),
			},
		},
	})
}

func TestState_projectChangeReplaces(t *testing.T) {
	env := newTestEnv(t, "state")
	a, b := randIdentifier(), randIdentifier()
	two := func(pid string) string {
		return env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_project" "a" {
  name       = "A"
  identifier = %q
}
resource "flightdeck_project" "b" {
  name       = "B"
  identifier = %q
}
resource "flightdeck_state" "test" {
  project_id = flightdeck_project.%s.id
  name       = "Moved"
  group      = "started"
}
`, a, b, pid)
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: two("a")},
			{
				Config: two("b"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(stateRes, plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: resource.TestCheckResourceAttrPair(stateRes, "project_id", "flightdeck_project.b", "id"),
			},
		},
	})
}

func TestState_colorSpellingIsSemantic(t *testing.T) {
	env := newTestEnv(t, "state")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Upper-case shorthand in configuration; the fake stores lower-case
				// six-digit (#aabbcc). Without semantic equality that read-back
				// would fail the apply as an inconsistent result; with it the
				// configured spelling is kept and the plan afterwards is empty.
				Config: stateConfig(env, identifier, `
  name  = "Shorthand"
  group = "started"
  color = "#ABC"`),
				Check: resource.TestCheckResourceAttr(stateRes, "color", "#ABC"),
			},
			{
				RefreshState: true,
				Check:        resource.TestCheckResourceAttr(stateRes, "color", "#ABC"),
			},
		},
	})
}

func TestState_importedDefaultStateIsNotUnsetByAPlan(t *testing.T) {
	env := newTestEnv(t, "state")
	identifier := randIdentifier()
	backlogConfig := projectFixture(env, identifier) + `
data "flightdeck_states" "all" {
  project_id = flightdeck_project.parent.id
}

resource "flightdeck_state" "test" {
  project_id = flightdeck_project.parent.id
  name       = "Backlog"
  group      = "backlog"
}
`
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectFixture(env, identifier) + `
data "flightdeck_states" "all" {
  project_id = flightdeck_project.parent.id
}
`,
				Check: checkSingleDefault("data.flightdeck_states.all", "Backlog"),
			},
			{
				// Import the seeded default state under a configuration that says
				// nothing about `default`.
				ResourceName:       stateRes,
				ImportState:        true,
				ImportStatePersist: true,
				Config:             backlogConfig,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					ds := s.RootModule().Resources["data.flightdeck_states.all"].Primary.Attributes
					for i := 0; ; i++ {
						name, ok := ds[fmt.Sprintf("states.%d.name", i)]
						if !ok {
							return "", fmt.Errorf("no Backlog state in %v", ds)
						}
						if name == "Backlog" {
							return ds[fmt.Sprintf("states.%d.id", i)], nil
						}
					}
				},
			},
			{
				// The first plan after import must be empty: `default` stays true.
				Config: backlogConfig,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(stateRes, "default", "true"),
					checkSingleDefault("data.flightdeck_states.all", "Backlog"),
				),
			},
		},
	})
}

func TestState_defaultHandOverRacesTheOldDefaultsEdit(t *testing.T) {
	env := newTestEnv(t, "state")
	identifier := randIdentifier()
	// Step 1: two managed states, "old" is the default.
	step1 := projectFixture(env, identifier) + `
resource "flightdeck_state" "old" {
  project_id = flightdeck_project.parent.id
  name       = "Old default"
  group      = "backlog"
  default    = true
}

resource "flightdeck_state" "new" {
  project_id = flightdeck_project.parent.id
  name       = "New default"
  group      = "unstarted"
  depends_on = [flightdeck_state.old]
}
`
	// Step 2: hand the default to "new" and, in the same apply, rename "old".
	// depends_on forces the hand-over to apply first, which bumps "old"'s
	// lock_version on the server; "old"'s edit then carries a stale If-Match.
	step2 := projectFixture(env, identifier) + `
resource "flightdeck_state" "new" {
  project_id = flightdeck_project.parent.id
  name       = "New default"
  group      = "unstarted"
  default    = true
}

resource "flightdeck_state" "old" {
  project_id = flightdeck_project.parent.id
  name       = "Old default (renamed)"
  group      = "backlog"
  default    = false
  depends_on = [flightdeck_state.new]
}
`
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: step1,
				Check:  resource.TestCheckResourceAttr("flightdeck_state.old", "default", "true"),
			},
			{
				Config:      step2,
				ExpectError: regexMust(`(?s)State "Old default" modified outside of Terraform`),
			},
			{
				// The next plan refreshes "old" (default now false, new lock_version)
				// and the rename goes through.
				Config: step2,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("flightdeck_state.new", "default", "true"),
					resource.TestCheckResourceAttr("flightdeck_state.old", "default", "false"),
					resource.TestCheckResourceAttr("flightdeck_state.old", "name", "Old default (renamed)"),
				),
			},
		},
	})
}

func TestState_destroyingTheDefaultStateLeavesItInPlace(t *testing.T) {
	env := newTestEnv(t, "state")
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: stateConfig(env, identifier, `
  name    = "Triage"
  group   = "backlog"
  default = true`),
				Check: captureAttr(stateRes, "id", &id),
			},
			{
				// Removing the resource: the API refuses to delete the project's
				// default state, so the provider drops it from Terraform state
				// with a warning rather than failing the destroy.
				Config: projectFixture(env, identifier),
			},
		},
	})
	if !env.live() {
		if stateProjectID(env, mustInt(id)) == 0 {
			t.Fatalf("default state %s should still exist on the server", id)
		}
	}
}

// stateProjectID finds the project owning a state in the fake (0 if unknown).
func stateProjectID(env *testEnv, stateID int64) int64 {
	for _, p := range env.fake.AllProjectIDs() {
		for _, st := range env.fake.StatesOf(p) {
			if st.ID == stateID {
				return p
			}
		}
	}
	return 0
}
