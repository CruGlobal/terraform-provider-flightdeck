package provider

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const teamspaceMemberRes = "flightdeck_teamspace_member.test"

func teamspaceMemberConfig(env *testEnv, teamName string, userID int64) string {
	return env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_teamspace" "team" {
  name = %q
}

resource "flightdeck_teamspace_member" "test" {
  teamspace_id = flightdeck_teamspace.team.id
  user_id      = %d
}
`, teamName, userID)
}

func TestTeamspaceMember_basicLifecycle(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	user := memberUserID(t, env)
	team := randName("Members")
	var teamID string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: teamspaceMemberConfig(env, team, user),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr("flightdeck_teamspace.team", "id", &teamID),
					resource.TestCheckResourceAttrPair(teamspaceMemberRes, "teamspace_id", "flightdeck_teamspace.team", "id"),
					resource.TestCheckResourceAttr(teamspaceMemberRes, "user_id", fmt.Sprint(user)),
					func(s *terraform.State) error {
						want := fmt.Sprintf("%s/%d", teamID, user)
						if got := s.RootModule().Resources[teamspaceMemberRes].Primary.Attributes["id"]; got != want {
							return fmt.Errorf("id = %q, want %q", got, want)
						}
						return nil
					},
				),
			},
			{
				ResourceName:      teamspaceMemberRes,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				// Removing the resource takes the user off the team.
				Config: env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_teamspace" "team" {
  name = %q
}
`, team),
				Check: func(*terraform.State) error {
					c, err := client.New(env.endpoint, env.token)
					if err != nil {
						return err
					}
					members, err := c.ListTeamspaceMembers(context.Background(), mustID(t, teamID))
					if err != nil {
						return err
					}
					if len(members) != 0 {
						return fmt.Errorf("team still has members %+v", members)
					}
					return nil
				},
			},
		},
	})
}

func TestTeamspaceMember_userOutsideTheWorkspaceIsRefused(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: teamspaceMemberConfig(env, randName("Members"), 999999999),
				ExpectError: regexMust(`(?s)Cannot add this user to the\s+teamspace.*Only members of the workspace` +
					`.*not a member of this\s+workspace`),
			},
		},
	})
}

func TestTeamspaceMember_importIDs(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: teamspaceMemberConfig(env, "Imports", 2)},
			{
				ResourceName:  teamspaceMemberRes,
				ImportState:   true,
				ImportStateId: "12",
				ExpectError:   regexMust(`Expected <teamspace_id>/<user_id>`),
			},
			{
				ResourceName:  teamspaceMemberRes,
				ImportState:   true,
				ImportStateId: "12/x",
				ExpectError:   regexMust(`Invalid import id`),
			},
			{
				ResourceName:  teamspaceMemberRes,
				ImportState:   true,
				ImportStateId: "999/2",
				ExpectError:   regexMust(`(?s)Error importing Flightdeck teamspace member.*HTTP 404`),
			},
		},
	})
}

// A user already on the team (added in the app, say) is adopted: the API
// answers 200 with the existing row, and nothing fails.
func TestTeamspaceMember_existingMembershipIsAdopted(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	teamID := env.fake.AddTeamspace("Existing")
	env.fake.SetTeamspaceMemberOutOfBand(teamID, 2, true)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_teamspace_member" "test" {
  teamspace_id = %d
  user_id      = 2
}
`, teamID),
				Check: resource.TestCheckResourceAttr(teamspaceMemberRes, "id", fmt.Sprintf("%d/2", teamID)),
			},
		},
	})
	posts := env.fake.RequestsMatching(http.MethodPost, fmt.Sprintf("/api/v1/teamspaces/%d/members", teamID))
	if len(posts) != 1 || posts[0].Status != http.StatusOK {
		t.Fatalf("member POSTs = %+v, want one answered 200", posts)
	}
	if posts[0].Header.Get("Idempotency-Key") != "" {
		t.Error("a member add carried an Idempotency-Key; the API ignores one and the add needs none")
	}
}

// A member add is safe to repeat (the API answers a repeat with the existing
// row), so one whose response is lost is sent again rather than failing.
func TestTeamspaceMember_lostResponseIsRetried(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	teamID := env.fake.AddTeamspace("Flaky")
	env.fake.DropNextResponse(http.MethodPost, "/members")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_teamspace_member" "test" {
  teamspace_id = %d
  user_id      = 2
}
`, teamID),
				Check: resource.TestCheckResourceAttr(teamspaceMemberRes, "user_id", "2"),
			},
		},
	})
	posts := env.fake.RequestsMatching(http.MethodPost, fmt.Sprintf("/api/v1/teamspaces/%d/members", teamID))
	if len(posts) != 2 || posts[0].Status != http.StatusCreated || posts[1].Status != http.StatusOK {
		t.Fatalf("member POSTs = %d, want a lost 201 then a 200", len(posts))
	}
}

func TestTeamspaceMember_removedOutsideTerraformIsAddedAgain(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	var teamID string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: teamspaceMemberConfig(env, "Drift", 2),
				Check:  captureAttr("flightdeck_teamspace.team", "id", &teamID),
			},
			{
				PreConfig: func() { env.fake.SetTeamspaceMemberOutOfBand(mustID(t, teamID), 2, false) },
				Config:    teamspaceMemberConfig(env, "Drift", 2),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(teamspaceMemberRes, plancheck.ResourceActionCreate)},
				},
				Check: func(*terraform.State) error {
					if got := env.fake.TeamspaceMemberIDs(mustID(t, teamID)); len(got) != 1 || got[0] != 2 {
						return fmt.Errorf("members = %v", got)
					}
					return nil
				},
			},
		},
	})
}

// A member who left the team between refresh and destroy is already gone:
// the API's 404 on the DELETE is success.
func TestTeamspaceMember_destroyOfAMemberAlreadyRemovedSucceeds(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	var teamID string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: teamspaceMemberConfig(env, "Leaving", 2),
				Check:  captureAttr("flightdeck_teamspace.team", "id", &teamID),
			},
			{
				PreConfig: func() {
					path := fmt.Sprintf("/api/v1/teamspaces/%s/members/2", teamID)
					env.fake.OnNextRequest(http.MethodDelete, path, func() {
						env.fake.SetTeamspaceMemberOutOfBand(mustID(t, teamID), 2, false)
					})
				},
				Config: env.providerConfig() + `
resource "flightdeck_teamspace" "team" {
  name = "Leaving"
}
`,
			},
		},
	})
}

// Adding a member while the team is deleted at the same moment is a lost
// race (409 stale_object). It is reported, not retried.
func TestTeamspaceMember_addThatLostARaceIsReported(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	teamID := env.fake.AddTeamspace("Racing")
	env.fake.RefuseNext(http.MethodPost, fmt.Sprintf("/teamspaces/%d/members", teamID), http.StatusConflict,
		"stale_object", flightdecktest.LostRaceWriteMessage)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_teamspace_member" "test" {
  teamspace_id = %d
  user_id      = 2
}
`, teamID),
				ExpectError: regexMust(`(?s)Error adding Flightdeck teamspace member.*HTTP 409 \(stale_object\).*lost\s+a\s+race`),
			},
		},
	})
	if got := countRequests(env, http.MethodPost, fmt.Sprintf("/api/v1/teamspaces/%d/members", teamID)); got != 1 {
		t.Errorf("member POSTs = %d; a lost race must not be retried", got)
	}
}
