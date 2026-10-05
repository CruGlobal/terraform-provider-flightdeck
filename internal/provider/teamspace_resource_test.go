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

const teamspaceRes = "flightdeck_teamspace.test"

func teamspaceConfig(env *testEnv, body string) string {
	return env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_teamspace" "test" {
%s
}
`, body)
}

func TestTeamspace_basicLifecycle(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	lead := memberUserID(t, env)
	name := randName("Team")
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: teamspaceConfig(env, fmt.Sprintf(`  name = %q`, name)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(teamspaceRes, "id"),
					resource.TestCheckResourceAttr(teamspaceRes, "name", name),
					resource.TestCheckNoResourceAttr(teamspaceRes, "description"),
					resource.TestCheckNoResourceAttr(teamspaceRes, "lead_id"),
					resource.TestCheckResourceAttr(teamspaceRes, "lock_version", "0"),
					captureAttr(teamspaceRes, "id", &id),
				),
			},
			{
				ResourceName:      teamspaceRes,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				Config: teamspaceConfig(env, fmt.Sprintf(`
  name        = "%s renamed"
  description = "Pipelines and shared infrastructure"
  lead_id     = %d`, name, lead)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(teamspaceRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(teamspaceRes, "id", &id),
					resource.TestCheckResourceAttr(teamspaceRes, "name", name+" renamed"),
					resource.TestCheckResourceAttr(teamspaceRes, "description", "Pipelines and shared infrastructure"),
					resource.TestCheckResourceAttr(teamspaceRes, "lead_id", fmt.Sprint(lead)),
					resource.TestCheckResourceAttr(teamspaceRes, "lock_version", "1"),
				),
			},
			{
				// Removing the description and the lead clears them.
				Config: teamspaceConfig(env, fmt.Sprintf(`  name = "%s renamed"`, name)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(teamspaceRes, "description"),
					resource.TestCheckNoResourceAttr(teamspaceRes, "lead_id"),
					checkTeamspaceOnServer(env, &id, func(ts *client.Teamspace) error {
						if ts.Description != nil || ts.LeadID != nil {
							return fmt.Errorf("server still has description %v, lead %v", ts.Description, ts.LeadID)
						}
						return nil
					}),
				),
			},
			{
				Config: teamspaceConfig(env, fmt.Sprintf(`  name = "%s renamed"`, name)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

func TestTeamspace_planTimeValidation(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      teamspaceConfig(env, `  name = "   "`),
				ExpectError: regexMust(`(?s)Blank value.*give the team a name`),
			},
			{
				Config: teamspaceConfig(env, `
  name        = "Platform"
  description = ""`),
				ExpectError: regexMust(`(?s)Blank value.*leave\s+.description.\s+out`),
			},
			{
				Config: teamspaceConfig(env, `
  name    = "Platform"
  lead_id = 0`),
				ExpectError: regexMust(`must be at least 1`),
			},
		},
	})
}

// A lead must be a member of the workspace. The API answers anything else
// with its own 422, and the provider passes the reason on.
func TestTeamspace_leadOutsideTheWorkspaceIsRefused(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: teamspaceConfig(env, fmt.Sprintf(`
  name    = %q
  lead_id = 999999999`, randName("Team"))),
				ExpectError: regexMust(`(?s)Error creating Flightdeck teamspace.*HTTP 422.*Lead must be a member of\s+the\s+workspace`),
			},
		},
	})
}

// Names are not unique, and a create's idempotency key comes from its body,
// so two declarations of the same team are one create. This is the
// documented consequence: both resources manage one team.
func TestTeamspace_identicalDeclarationsShareOneTeam(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	name := randName("Twin")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_teamspace" "a" {
  name = %q
}

resource "flightdeck_teamspace" "b" {
  name = %q
}

resource "flightdeck_teamspace" "c" {
  name        = %q
  description = "a different body is a different team"
}
`, name, name, name),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("flightdeck_teamspace.a", "id", "flightdeck_teamspace.b", "id"),
					func(s *terraform.State) error {
						a := s.RootModule().Resources["flightdeck_teamspace.a"].Primary.ID
						c := s.RootModule().Resources["flightdeck_teamspace.c"].Primary.ID
						if a == c {
							return fmt.Errorf("teams a and c share id %s", a)
						}
						return nil
					},
				),
			},
		},
	})
}

// Splitting a team: rename it, then declare a new team with its old name.
// The new create's body is the old one's, so the API replays the old
// create, but the team it names no longer has that name. It is somebody
// else's team now, and a new one is made.
func TestTeamspace_reusingARenamedTeamsNameMakesANewTeam(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	name := randName("Split")
	original := func(n string) string {
		return fmt.Sprintf(`
resource "flightdeck_teamspace" "a" {
  name = %q
}
`, n)
	}
	var first string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: env.providerConfig() + original(name),
				Check:  captureAttr("flightdeck_teamspace.a", "id", &first),
			},
			{
				Config: env.providerConfig() + original(name+" core"),
			},
			{
				Config: env.providerConfig() + original(name+" core") + fmt.Sprintf(`
resource "flightdeck_teamspace" "b" {
  name = %q
}
`, name),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr("flightdeck_teamspace.a", "id", &first),
					resource.TestCheckResourceAttr("flightdeck_teamspace.a", "name", name+" core"),
					resource.TestCheckResourceAttr("flightdeck_teamspace.b", "name", name),
					func(s *terraform.State) error {
						if b := s.RootModule().Resources["flightdeck_teamspace.b"].Primary.ID; b == first {
							return fmt.Errorf("the new team took over the renamed team %s", first)
						}
						return nil
					},
				),
			},
		},
	})
}

// A team saved from the web form can hold an empty description where the
// API would store none. Both read as no description, so the plan is empty.
func TestTeamspace_emptyDescriptionFromTheWebReadsAsNone(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	var id string
	config := teamspaceConfig(env, `  name = "Web edited"`) + `
data "flightdeck_teamspace" "read" {
  id = flightdeck_teamspace.test.id
}
`
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  captureAttr(teamspaceRes, "id", &id),
			},
			{
				PreConfig: func() {
					empty := ""
					env.fake.EditTeamspaceOutOfBand(mustID(t, id), func(ts *flightdecktest.Teamspace) { ts.Description = &empty })
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckNoResourceAttr("data.flightdeck_teamspace.read", "description"),
			},
		},
	})
}

// Flightdeck refuses a create whose Idempotency-Key it already used for a
// different body. The error says what happened and how to get past it.
func TestTeamspace_reusedIdempotencyKeyIsExplained(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	env.fake.RefuseNext(http.MethodPost, "/teamspaces", http.StatusConflict, "idempotency_key_reused",
		"This Idempotency-Key was already used for a create with different attributes.")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: teamspaceConfig(env, `  name = "Platform"`),
				ExpectError: regexMust(`(?s)refused this teamspace create's idempotency key.*made\s+nothing.*Change\s+any\s+argument` +
					`.*already\s+used\s+for\s+a\s+create\s+with\s+different\s+attributes`),
			},
		},
	})
}

func TestTeamspace_staleWriteIsReported(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: teamspaceConfig(env, `  name = "Before"`),
				Check:  captureAttr(teamspaceRes, "id", &id),
			},
			{
				// Someone edits the team between this plan and its apply.
				PreConfig: func() {
					env.fake.OnNextRequest(http.MethodPatch, "/api/v1/teamspaces/"+id, func() {
						env.fake.EditTeamspaceOutOfBand(mustID(t, id), func(ts *flightdecktest.Teamspace) { ts.Name = "Theirs" })
					})
				},
				Config:      teamspaceConfig(env, `  name = "After"`),
				ExpectError: regexMust(`(?s)modified outside of Terraform.*lock_version 0.*the server now has 1`),
			},
		},
	})
}

func TestTeamspace_deletedOutsideTerraformIsRecreated(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: teamspaceConfig(env, `  name = "Gone soon"`),
				Check:  captureAttr(teamspaceRes, "id", &id),
			},
			{
				PreConfig: func() { env.fake.DeleteTeamspaceOutOfBand(mustID(t, id)) },
				Config:    teamspaceConfig(env, `  name = "Gone soon"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(teamspaceRes, plancheck.ResourceActionCreate)},
				},
				Check: func(s *terraform.State) error {
					if got := s.RootModule().Resources[teamspaceRes].Primary.ID; got == id {
						return fmt.Errorf("the deleted team's id %s came back; the replayed create was not detected", id)
					}
					return nil
				},
			},
		},
	})
}

func TestTeamspace_guestIsRefused(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	env.fake.SetWorkspaceGuest(true)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      teamspaceConfig(env, `  name = "Guest's team"`),
				ExpectError: regexMust(`(?s)Workspace guests cannot create or change\s+teamspaces.*Workspace guests can't do this`),
			},
		},
	})
}

// Deleting a team unlinks every project it owns, so the API refuses it
// unless the token may unlink each one, and nothing is removed. Here a
// project linked outside Terraform, which the token may read but not
// administer, blocks the delete until the token is given that right.
func TestTeamspace_deleteBlockedByAProjectTheTokenCannotUnlink(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	blocker := env.fake.AddProject("Not mine", randIdentifier())
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: teamspaceConfig(env, `  name = "Owns something"`),
				Check:  captureAttr(teamspaceRes, "id", &id),
			},
			{
				PreConfig: func() {
					env.fake.SetTeamspaceProjectOutOfBand(mustID(t, id), blocker.ID, true)
					env.fake.SetProjectAccess(blocker.ID, flightdecktest.ProjectAccessReadOnly)
				},
				Config: env.providerConfig(),
				ExpectError: regexMust(`(?s)cannot be deleted with this token.*Links made outside\s+Terraform\s+count\s+too` +
					`.*owns 1 project that you cannot\s+unlink`),
			},
			{
				PreConfig: func() { env.fake.SetProjectAccess(blocker.ID, flightdecktest.ProjectAccessAdmin) },
				Config:    env.providerConfig(),
				Check: func(*terraform.State) error {
					if env.fake.Teamspace(mustID(t, id)) != nil {
						return fmt.Errorf("teamspace %s still exists", id)
					}
					return nil
				},
			},
		},
	})
}

// A delete that loses a race to a member being added at the same moment is
// refused with 409 stale_object; the provider re-reads and deletes once more.
func TestTeamspace_deleteThatLostARaceIsSentAgain(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: teamspaceConfig(env, `  name = "Raced"`),
				Check:  captureAttr(teamspaceRes, "id", &id),
			},
			{
				PreConfig: func() {
					env.fake.RefuseNext(http.MethodDelete, "/teamspaces/"+id, http.StatusConflict, "stale_object",
						flightdecktest.LostRaceDeleteMessage)
				},
				Config: env.providerConfig(),
				Check: func(*terraform.State) error {
					if got := countRequests(env, http.MethodDelete, "/api/v1/teamspaces/"+id); got != 2 {
						return fmt.Errorf("DELETEs = %d, want 2", got)
					}
					if env.fake.Teamspace(mustID(t, id)) != nil {
						return fmt.Errorf("teamspace %s still exists", id)
					}
					return nil
				},
			},
		},
	})
}

// checkTeamspaceOnServer reads the team straight from the API, which works
// against the fake and a live Flightdeck alike.
func checkTeamspaceOnServer(env *testEnv, id *string, check func(*client.Teamspace) error) resource.TestCheckFunc {
	return func(*terraform.State) error {
		c, err := client.New(env.endpoint, env.token)
		if err != nil {
			return err
		}
		var tid int64
		if _, err := fmt.Sscan(*id, &tid); err != nil {
			return err
		}
		ts, err := c.GetTeamspace(context.Background(), tid)
		if err != nil {
			return err
		}
		return check(ts)
	}
}
