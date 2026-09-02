package provider

import (
	"fmt"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestProjectDataSource_byIdAndIdentifier(t *testing.T) {
	env := newTestEnv(t, "project")
	identifier := randIdentifier()
	name := randName("Lookup")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, fmt.Sprintf(`  name = %q`, name)) + `
data "flightdeck_project" "by_identifier" {
  identifier = flightdeck_project.test.identifier
}

data "flightdeck_project" "by_id" {
  id = flightdeck_project.test.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("data.flightdeck_project.by_identifier", "id", projectRes, "id"),
					resource.TestCheckResourceAttr("data.flightdeck_project.by_identifier", "name", name),
					resource.TestCheckResourceAttr("data.flightdeck_project.by_identifier", "identifier", identifier),
					resource.TestCheckResourceAttr("data.flightdeck_project.by_identifier", "archived", "false"),
					resource.TestCheckResourceAttrSet("data.flightdeck_project.by_identifier", "lock_version"),
					// The data source reports the full effective map, read-only keys included.
					resource.TestCheckResourceAttr("data.flightdeck_project.by_identifier", "features.cycles", "true"),
					resource.TestCheckResourceAttr("data.flightdeck_project.by_identifier", "features.intake", "false"),
					resource.TestCheckResourceAttr("data.flightdeck_project.by_identifier", "features.self_healing", "false"),
					resource.TestCheckResourceAttrPair("data.flightdeck_project.by_id", "identifier", projectRes, "identifier"),
					resource.TestCheckResourceAttr("data.flightdeck_project.by_id", "name", name),
				),
			},
		},
	})
}

func TestProjectDataSource_identifierIsCaseInsensitive(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	p := env.fake.AddProject("Cased", "CASED")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: env.providerConfig() + `
data "flightdeck_project" "x" {
  identifier = "cased"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.flightdeck_project.x", "id", fmt.Sprint(p.ID)),
					resource.TestCheckResourceAttr("data.flightdeck_project.x", "identifier", "CASED"),
				),
			},
		},
	})
}

func TestProjectDataSource_paginatesWhenMatchingIdentifier(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	// More projects than one page (100) holds, target on the last page.
	for i := 0; i < 120; i++ {
		env.fake.AddProject(fmt.Sprintf("Filler %03d", i), fmt.Sprintf("F%d", i))
	}
	target := env.fake.AddProject("Zzz Target", "ZZZ")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: env.providerConfig() + `
data "flightdeck_project" "x" {
  identifier = "ZZZ"
}
`,
				Check: resource.TestCheckResourceAttr("data.flightdeck_project.x", "id", fmt.Sprint(target.ID)),
			},
		},
	})
	if got := len(env.fake.RequestsMatching("GET", "/api/v1/projects")); got < 2 {
		t.Errorf("expected the lookup to walk at least 2 pages, saw %d list requests", got)
	}
}

func TestProjectDataSource_errors(t *testing.T) {
	env := newTestEnv(t, "project")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: env.providerConfig() + `
data "flightdeck_project" "x" {
  identifier = "NOSUCHPROJ"
}
`,
				ExpectError: regexMust(`No project with identifier "NOSUCHPROJ"`),
			},
			{
				Config: env.providerConfig() + `
data "flightdeck_project" "x" {
  id = 999999999
}
`,
				ExpectError: regexMust(`HTTP 404 \(not_found\)`),
			},
			{
				Config: env.providerConfig() + `
data "flightdeck_project" "x" {
  id         = 1
  identifier = "APP"
}
`,
				ExpectError: regexMust(`Exactly one of these attributes must be configured`),
			},
			{
				Config: env.providerConfig() + `
data "flightdeck_project" "x" {}
`,
				ExpectError: regexMust(`Exactly one of these attributes must be configured`),
			},
		},
	})
}

// Provider-level behaviour that needs a data source to drive Configure.

func TestProvider_EnvironmentFallback(t *testing.T) {
	fake := flightdecktest.New(t)
	p := fake.AddProject("Env", "ENV")
	t.Setenv(envEndpoint, fake.URL)
	t.Setenv(envToken, fake.Token())
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: `
provider "flightdeck" {}

data "flightdeck_project" "x" {
  identifier = "ENV"
}
`,
				Check: resource.TestCheckResourceAttr("data.flightdeck_project.x", "id", fmt.Sprint(p.ID)),
			},
		},
	})
}

func TestProvider_MissingConfigurationIsAnError(t *testing.T) {
	t.Setenv(envEndpoint, "")
	t.Setenv(envToken, "")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: `
provider "flightdeck" {}

data "flightdeck_project" "x" {
  identifier = "WEB"
}
`,
				ExpectError: regexMust(`Missing Flightdeck (endpoint|token)`),
			},
		},
	})
}

func TestProvider_InvalidTokenIsUnauthorized(t *testing.T) {
	fake := flightdecktest.New(t)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
provider "flightdeck" {
  endpoint = %q
  token    = "fd_pat_wrong"
}

data "flightdeck_project" "x" {
  identifier = "WEB"
}
`, fake.URL),
				ExpectError: regexMust(`HTTP 401 \(unauthorized\)`),
			},
		},
	})
}
