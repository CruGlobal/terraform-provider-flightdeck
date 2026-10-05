package provider

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// Label and state names are at most 255 characters, counted as characters
// rather than bytes, so a name in a non-Latin script at the limit is
// accepted and one character more fails the plan.
func TestNameLength_labelsAndStates(t *testing.T) {
	env := newTestEnv(t, "label")
	identifier := randIdentifier()
	atLimit := strings.Repeat("é", 255)
	overLimit := strings.Repeat("é", 256)
	config := func(name string) string {
		return projectFixture(env, identifier) + fmt.Sprintf(`
resource "flightdeck_label" "test" {
  project_id = flightdeck_project.parent.id
  name       = %q
}

resource "flightdeck_state" "test" {
  project_id = flightdeck_project.parent.id
  name       = %q
  group      = "started"
}
`, name, name)
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      config(overLimit),
				ExpectError: regexMust(`(?s)Value too long.*256\s+characters\s+long;\s+Flightdeck\s+accepts\s+at\s+most\s+255`),
			},
			{
				Config: config(atLimit),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(labelRes, "name", atLimit),
					resource.TestCheckResourceAttr(stateRes, "name", atLimit),
				),
			},
		},
	})
}

// A repository name is at most 140 characters: an owner of up to 39, a
// slash, and a repository of up to 100.
func TestNameLength_githubRepository(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	identifier := randIdentifier()
	atLimit := "o" + strings.ToLower(identifier) + strings.Repeat("o", 38-len(identifier)) + "/" + strings.Repeat("r", 100)
	config := func(repo string) string {
		return githubIntegrationConfig(env, identifier, fmt.Sprintf(`
  repo_full_name = %q
  secret         = "mine-is-long-enough-0123456789"`, repo))
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      config(atLimit + "r"),
				ExpectError: regexMust(`(?s)Value too long.*141\s+characters\s+long;\s+Flightdeck\s+accepts\s+at\s+most\s+140`),
			},
			{
				// Caller-managed, so no GitHub access is needed.
				Config: config(atLimit),
				Check:  resource.TestCheckResourceAttr(ghRes, "repo_full_name", atLimit),
			},
		},
	})
}
