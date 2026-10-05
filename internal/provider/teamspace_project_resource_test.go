package provider

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const teamspaceProjectRes = "flightdeck_teamspace_project.test"

// teamspaceProjectConfig declares a project, a team and the link between them.
func teamspaceProjectConfig(env *testEnv, identifier, teamName string) string {
	return projectFixture(env, identifier) + fmt.Sprintf(`
resource "flightdeck_teamspace" "team" {
  name = %q
}

resource "flightdeck_teamspace_project" "test" {
  teamspace_id = flightdeck_teamspace.team.id
  project_id   = flightdeck_project.parent.id
}
`, teamName)
}

func TestTeamspaceProject_basicLifecycle(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	identifier := randIdentifier()
	team := randName("Owners")
	var teamID, projectID string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: teamspaceProjectConfig(env, identifier, team),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr("flightdeck_teamspace.team", "id", &teamID),
					captureAttr("flightdeck_project.parent", "id", &projectID),
					resource.TestCheckResourceAttrPair(teamspaceProjectRes, "teamspace_id", "flightdeck_teamspace.team", "id"),
					resource.TestCheckResourceAttrPair(teamspaceProjectRes, "project_id", "flightdeck_project.parent", "id"),
					func(s *terraform.State) error {
						want := teamID + "/" + projectID
						if got := s.RootModule().Resources[teamspaceProjectRes].Primary.Attributes["id"]; got != want {
							return fmt.Errorf("id = %q, want %q", got, want)
						}
						return nil
					},
				),
			},
			{
				ResourceName:      teamspaceProjectRes,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				// Removing the link unlinks the project and leaves both alone.
				Config: projectFixture(env, identifier) + fmt.Sprintf(`
resource "flightdeck_teamspace" "team" {
  name = %q
}
`, team),
				Check: func(*terraform.State) error {
					c, err := client.New(env.endpoint, env.token)
					if err != nil {
						return err
					}
					links, err := c.ListTeamspaceProjects(context.Background(), mustID(t, teamID))
					if err != nil {
						return err
					}
					if len(links) != 0 {
						return fmt.Errorf("team still owns %+v", links)
					}
					_, err = c.GetProject(context.Background(), mustID(t, projectID))
					return err
				},
			},
		},
	})
}

// A project id that names nothing is refused with the same 422 a project the
// token cannot see gets; the error says it cannot tell the two apart.
func TestTeamspaceProject_unknownProjectIsRefused(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_teamspace" "team" {
  name = %q
}

resource "flightdeck_teamspace_project" "test" {
  teamspace_id = flightdeck_teamspace.team.id
  project_id   = 999999999
}
`, randName("Owners")),
				ExpectError: regexMust(`(?s)Cannot link project 999999999.*same\s+answer.*cannot\s+see.*not a project in\s+this\s+workspace`),
			},
		},
	})
}

func TestTeamspaceProject_hiddenProjectIsRefusedLikeAMissingOne(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	hidden := env.fake.AddProject("Private", randIdentifier())
	env.fake.SetProjectAccess(hidden.ID, flightdecktest.ProjectAccessHidden)
	teamID := env.fake.AddTeamspace("Owners")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_teamspace_project" "test" {
  teamspace_id = %d
  project_id   = %d
}
`, teamID, hidden.ID),
				ExpectError: regexMust(fmt.Sprintf(`(?s)Cannot link project %d.*not a project in\s+this\s+workspace`, hidden.ID)),
			},
		},
	})
}

// Linking needs both read and administer on the project. Either one missing
// is a 403 naming the capability, and the error says what is needed.
func TestTeamspaceProject_linkRuleIsExplained(t *testing.T) {
	for _, tc := range []struct {
		access  flightdecktest.ProjectAccess
		missing string
	}{
		{flightdecktest.ProjectAccessReadOnly, "administer_project"},
		{flightdecktest.ProjectAccessAdminNoRead, "react"},
	} {
		t.Run(tc.missing, func(t *testing.T) {
			env := newTestEnv(t, "teamspace")
			env.requireFake(t)
			p := env.fake.AddProject("Someone else's", randIdentifier())
			env.fake.SetProjectAccess(p.ID, tc.access)
			teamID := env.fake.AddTeamspace("Owners")
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{
						Config: env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_teamspace_project" "test" {
  teamspace_id = %d
  project_id   = %d
}
`, teamID, p.ID),
						ExpectError: regexMust(fmt.Sprintf(`(?s)Not allowed to link project %d.*both\s+read\s+and\s+administer`+
							`.*missing\s+capability:\s+%s`, p.ID, tc.missing)),
					},
				},
			})
		})
	}
}

// A guest is refused a link even on a project it administers.
func TestTeamspaceProject_guestIsRefused(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	p := env.fake.AddProject("Guest project", randIdentifier())
	teamID := env.fake.AddTeamspace("Owners")
	env.fake.SetWorkspaceGuest(true)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_teamspace_project" "test" {
  teamspace_id = %d
  project_id   = %d
}
`, teamID, p.ID),
				ExpectError: regexMust(`(?s)Not allowed to link project.*Workspace guests can't do this`),
			},
		},
	})
}

// A token that can see a project but not read it cannot read the link
// either. That is neither "gone" nor "exists": the refresh fails and says why,
// rather than dropping the link from state.
func TestTeamspaceProject_unreadableLinkIsAnErrorNotADrop(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	identifier := randIdentifier()
	var projectID string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: teamspaceProjectConfig(env, identifier, "Owners"),
				Check:  captureAttr("flightdeck_project.parent", "id", &projectID),
			},
			{
				PreConfig: func() {
					env.fake.SetProjectAccess(mustID(t, projectID), flightdecktest.ProjectAccessAdminNoRead)
				},
				Config:      teamspaceProjectConfig(env, identifier, "Owners"),
				ExpectError: regexMust(`(?s)Cannot read teamspace \d+'s link to project \d+.*can see the project but may not\s+read\s+it`),
			},
			{
				// With read access back, everything reconciles and destroys.
				PreConfig: func() {
					env.fake.SetProjectAccess(mustID(t, projectID), flightdecktest.ProjectAccessAdmin)
				},
				Config: teamspaceProjectConfig(env, identifier, "Owners"),
			},
		},
	})
}

// A linked project that is deleted elsewhere takes its link with it: both
// leave state, and the next apply creates the project and links it again.
func TestTeamspaceProject_linkToAProjectThatGoesAwayLeavesState(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	identifier := randIdentifier()
	var teamID, projectID string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: teamspaceProjectConfig(env, identifier, "Owners"),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr("flightdeck_teamspace.team", "id", &teamID),
					captureAttr("flightdeck_project.parent", "id", &projectID),
				),
			},
			{
				// The project is deleted elsewhere: the project and its link
				// both leave state and are created again.
				PreConfig: func() { env.fake.DeleteProjectOutOfBand(mustID(t, projectID)) },
				Config:    teamspaceProjectConfig(env, identifier, "Owners"),
				Check: func(s *terraform.State) error {
					newProject := s.RootModule().Resources["flightdeck_project.parent"].Primary.ID
					if newProject == projectID {
						return fmt.Errorf("project %s was not recreated", projectID)
					}
					if got := env.fake.TeamspaceProjectIDs(mustID(t, teamID)); len(got) < 1 || got[len(got)-1] != mustID(t, newProject) {
						return fmt.Errorf("team owns %v, want the new project %s", got, newProject)
					}
					return nil
				},
			},
		},
	})
}

// Unlinking a project the token may read but not administer is refused, and
// says why. The link stays.
func TestTeamspaceProject_unlinkRuleIsExplained(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	identifier := randIdentifier()
	var projectID string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: teamspaceProjectConfig(env, identifier, "Owners"),
				Check:  captureAttr("flightdeck_project.parent", "id", &projectID),
			},
			{
				PreConfig: func() {
					env.fake.SetProjectAccess(mustID(t, projectID), flightdecktest.ProjectAccessReadOnly)
				},
				Config: projectFixture(env, identifier) + `
resource "flightdeck_teamspace" "team" {
  name = "Owners"
}
`,
				ExpectError: regexMust(`(?s)Not allowed to unlink project \d+.*both\s+read\s+and\s+administer.*missing\s+capability:\s+administer_project`),
			},
			{
				PreConfig: func() {
					env.fake.SetProjectAccess(mustID(t, projectID), flightdecktest.ProjectAccessAdmin)
				},
				Config: projectFixture(env, identifier) + `
resource "flightdeck_teamspace" "team" {
  name = "Owners"
}
`,
			},
		},
	})
}

// An unlink that loses a race is sent once more; one answered 404 (already
// unlinked) is success.
func TestTeamspaceProject_unlinkRaces(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	identifier := randIdentifier()
	var teamID, projectID string
	unlinked := projectFixture(env, identifier) + `
resource "flightdeck_teamspace" "team" {
  name = "Owners"
}
`
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: teamspaceProjectConfig(env, identifier, "Owners"),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr("flightdeck_teamspace.team", "id", &teamID),
					captureAttr("flightdeck_project.parent", "id", &projectID),
				),
			},
			{
				PreConfig: func() {
					env.fake.RefuseNext(http.MethodDelete, "/teamspaces/"+teamID+"/projects/"+projectID,
						http.StatusConflict, "stale_object", flightdecktest.LostRaceDeleteMessage)
				},
				Config: unlinked,
				Check: func(*terraform.State) error {
					path := "/api/v1/teamspaces/" + teamID + "/projects/" + projectID
					if got := countRequests(env, http.MethodDelete, path); got != 2 {
						return fmt.Errorf("DELETEs = %d, want 2", got)
					}
					if got := env.fake.TeamspaceProjectIDs(mustID(t, teamID)); len(got) != 0 {
						return fmt.Errorf("team still owns %v", got)
					}
					return nil
				},
			},
			{
				Config: teamspaceProjectConfig(env, identifier, "Owners"),
			},
			{
				PreConfig: func() {
					path := "/api/v1/teamspaces/" + teamID + "/projects/" + projectID
					env.fake.OnNextRequest(http.MethodDelete, path, func() {
						env.fake.SetTeamspaceProjectOutOfBand(mustID(t, teamID), mustID(t, projectID), false)
					})
				},
				Config: unlinked,
			},
		},
	})
}

// A project the token can no longer see reads as a missing link, so the link
// leaves state. Linking it again is the API's answer for a project it cannot
// see; once the token can see the project again, the link it never lost is
// adopted.
func TestTeamspaceProject_linkToAProjectTheTokenCanNoLongerSee(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	identifier := randIdentifier()
	var teamID, projectID string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: teamspaceProjectConfig(env, identifier, "Owners"),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr("flightdeck_teamspace.team", "id", &teamID),
					captureAttr("flightdeck_project.parent", "id", &projectID),
				),
			},
			{
				PreConfig: func() {
					env.fake.SetProjectAccess(mustID(t, projectID), flightdecktest.ProjectAccessHidden)
				},
				Config:      teamspaceProjectConfig(env, identifier, "Owners"),
				ExpectError: regexMust(`(?s)Cannot link project \d+.*cannot\s+see`),
			},
			{
				PreConfig: func() {
					env.fake.SetProjectAccess(mustID(t, projectID), flightdecktest.ProjectAccessAdmin)
				},
				Config: teamspaceProjectConfig(env, identifier, "Owners"),
				Check: func(*terraform.State) error {
					if got := env.fake.TeamspaceProjectIDs(mustID(t, teamID)); len(got) != 1 || got[0] != mustID(t, projectID) {
						return fmt.Errorf("team owns %v, want only project %s", got, projectID)
					}
					return nil
				},
			},
		},
	})
}
