package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
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
