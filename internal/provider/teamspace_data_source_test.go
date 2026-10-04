package provider

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestTeamspaceDataSource_byIDAndByName(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	name := randName("Lookup")
	config := env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_teamspace" "test" {
  name        = %q
  description = "Found by name"
}

data "flightdeck_teamspace" "by_id" {
  id = flightdeck_teamspace.test.id
}

# The match ignores letter case.
data "flightdeck_teamspace" "by_name" {
  name       = %q
  depends_on = [flightdeck_teamspace.test]
}
`, name, strings.ToUpper(name))
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("data.flightdeck_teamspace.by_id", "name", teamspaceRes, "name"),
					resource.TestCheckResourceAttr("data.flightdeck_teamspace.by_id", "description", "Found by name"),
					resource.TestCheckResourceAttrPair("data.flightdeck_teamspace.by_name", "id", teamspaceRes, "id"),
					// The argument comes back as configured.
					resource.TestCheckResourceAttr("data.flightdeck_teamspace.by_name", "name", strings.ToUpper(name)),
					resource.TestCheckResourceAttr("data.flightdeck_teamspace.by_name", "description", "Found by name"),
					resource.TestCheckNoResourceAttr("data.flightdeck_teamspace.by_name", "lead_id"),
				),
			},
		},
	})
	if env.fake != nil {
		var filtered bool
		for _, r := range env.fake.RequestsMatching(http.MethodGet, "/api/v1/teamspaces") {
			q, _ := url.ParseQuery(r.Query)
			if q.Get("name") == strings.ToUpper(name) {
				filtered = true
			}
		}
		if !filtered {
			t.Error("the name lookup did not send ?name=")
		}
	}
}

// Names are not unique. A lookup by name that matches two teams fails and
// names both ids, rather than choosing one.
func TestTeamspaceDataSource_ambiguousNameIsRefused(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	name := randName("Shared")
	teams := env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_teamspace" "one" {
  name        = %q
  description = "first"
}

resource "flightdeck_teamspace" "two" {
  name        = %q
  description = "second"
}
`, name, strings.ToLower(name))
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: teams},
			{
				// The error names both ids, so the right one can be chosen.
				Config: teams + fmt.Sprintf(`
data "flightdeck_teamspace" "pick" {
  name = %q
}
`, name),
				ExpectError: regexMust(`(?s)Ambiguous teamspace name.*2 teamspaces in this workspace are named.*\(ids\s+\d+,\s+\d+\).*refuses\s+to\s+pick\s+one`),
			},
		},
	})
}

func TestTeamspaceDataSource_noMatchAndBadArguments(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	lookup := func(body string) string {
		return env.providerConfig() + fmt.Sprintf(`
data "flightdeck_teamspace" "pick" {
%s
}
`, body)
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      lookup(fmt.Sprintf(`  name = %q`, randName("Nobody"))),
				ExpectError: regexMust(`(?s)No such teamspace.*whole\s+name`),
			},
			{
				Config:      lookup(`  id = 999999999`),
				ExpectError: regexMust(`(?s)No such teamspace.*no teamspace with id 999999999`),
			},
			{
				// A blank name would be no filter at all on the API.
				Config:      lookup(`  name = "  "`),
				ExpectError: regexMust(`Blank value`),
			},
			{
				Config:      lookup(fmt.Sprintf(`  name = %q`, strings.Repeat("é", 256))),
				ExpectError: regexMust(`(?s)256\s+characters\s+long.*at\s+most\s+255`),
			},
			{
				Config: lookup(`
  id   = 1
  name = "Platform"`),
				ExpectError: regexMust(`(?s)Invalid Attribute Combination`),
			},
		},
	})
}

// 255 characters is the cap once blank space at either end is left out, as
// the API counts it, so a padded name at the cap is still looked up.
func TestTeamspaceDataSource_nameCapIgnoresBlankEnds(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	name := strings.Repeat("t", 255)
	env.fake.AddTeamspace(name)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: env.providerConfig() + fmt.Sprintf(`
data "flightdeck_teamspace" "pick" {
  name = %q
}
`, strings.ToUpper(name)),
				Check: resource.TestCheckResourceAttrSet("data.flightdeck_teamspace.pick", "id"),
			},
			{
				// Padded, it passes the length check but does not match: blank
				// space counts in the match itself.
				Config: env.providerConfig() + fmt.Sprintf(`
data "flightdeck_teamspace" "pick" {
  name = %q
}
`, "  "+name+"  "),
				ExpectError: regexMust(`No such teamspace`),
			},
		},
	})
}
