package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func TestStatesDataSource_listsSeededStatesInWorkflowOrder(t *testing.T) {
	env := newTestEnv(t, "state")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectFixture(env, identifier) + `
data "flightdeck_states" "all" {
  project_id = flightdeck_project.parent.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("data.flightdeck_states.all", "project_id", "flightdeck_project.parent", "id"),
					resource.TestCheckResourceAttr("data.flightdeck_states.all", "states.#", "5"),
					resource.TestCheckResourceAttr("data.flightdeck_states.all", "states.0.name", "Backlog"),
					resource.TestCheckResourceAttr("data.flightdeck_states.all", "states.0.group", "backlog"),
					resource.TestCheckResourceAttr("data.flightdeck_states.all", "states.0.default", "true"),
					resource.TestCheckResourceAttr("data.flightdeck_states.all", "states.1.name", "To Do"),
					resource.TestCheckResourceAttr("data.flightdeck_states.all", "states.2.name", "In Progress"),
					resource.TestCheckResourceAttr("data.flightdeck_states.all", "states.3.name", "Done"),
					resource.TestCheckResourceAttr("data.flightdeck_states.all", "states.4.name", "Cancelled"),
					resource.TestCheckResourceAttr("data.flightdeck_states.all", "states.4.group", "cancelled"),
					resource.TestCheckResourceAttrSet("data.flightdeck_states.all", "states.0.id"),
					resource.TestCheckResourceAttrSet("data.flightdeck_states.all", "states.0.color"),
					checkSingleDefault("Backlog"),
				),
			},
		},
	})
}

func TestStatesDataSource_unknownProject(t *testing.T) {
	env := newTestEnv(t, "state")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: env.providerConfig() + `
data "flightdeck_states" "x" {
  project_id = 999999999
}
`,
				ExpectError: regexMust(`HTTP 404 \(not_found\)`),
			},
		},
	})
}

// statesDS is the data source the state tests read the project's states through.
const statesDS = "data.flightdeck_states.all"

// checkSingleDefault asserts exactly one state in statesDS is the default, and
// that it is the named one.
func checkSingleDefault(wantName string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[statesDS]
		if !ok {
			return fmt.Errorf("%s not in state", statesDS)
		}
		var defaults []string
		for i := 0; ; i++ {
			name, ok := rs.Primary.Attributes[fmt.Sprintf("states.%d.name", i)]
			if !ok {
				break
			}
			if rs.Primary.Attributes[fmt.Sprintf("states.%d.default", i)] == "true" {
				defaults = append(defaults, name)
			}
		}
		if len(defaults) != 1 || defaults[0] != wantName {
			return fmt.Errorf("expected exactly one default state %q, got %v", wantName, defaults)
		}
		return nil
	}
}
