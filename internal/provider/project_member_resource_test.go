package provider

import (
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const memberRes = "flightdeck_project_member.test"

// memberUserID is the workspace member the tests grant access to: a seeded
// user of the fake, or FLIGHTDECK_ACC_MEMBER_USER_ID against a live instance
// (the API has no directory route to resolve an email).
func memberUserID(t *testing.T, env *testEnv) int64 {
	t.Helper()
	if env.live() {
		raw := os.Getenv(envAccMemberUserID)
		if raw == "" {
			t.Skipf("%s must be set for project member tests against a live instance", envAccMemberUserID)
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			t.Fatalf("%s must be a numeric user id: %v", envAccMemberUserID, err)
		}
		return id
	}
	return env.fake.Members()[1].ID
}

func memberConfig(env *testEnv, identifier string, userID int64, role string) string {
	return projectFixture(env, identifier) + fmt.Sprintf(`
resource "flightdeck_project_member" "test" {
  project_id = flightdeck_project.parent.id
  user_id    = %d
  role       = %q
}
`, userID, role)
}

func TestProjectMember_basicLifecycle(t *testing.T) {
	env := newTestEnv(t, "project_member")
	identifier := randIdentifier()
	userID := memberUserID(t, env)
	var membershipID string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: memberConfig(env, identifier, userID, "member"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(memberRes, "id"),
					resource.TestCheckResourceAttrPair(memberRes, "project_id", "flightdeck_project.parent", "id"),
					resource.TestCheckResourceAttr(memberRes, "user_id", fmt.Sprint(userID)),
					resource.TestCheckResourceAttr(memberRes, "role", "member"),
					resource.TestCheckResourceAttr(memberRes, "builtin_role", "member"),
					resource.TestCheckResourceAttrSet(memberRes, "lock_version"),
					captureAttr(memberRes, "id", &membershipID),
					func(s *terraform.State) error {
						rs := s.RootModule().Resources[memberRes].Primary
						if rs.ID == rs.Attributes["user_id"] {
							return fmt.Errorf("resource id must be the membership id, not the user id")
						}
						return nil
					},
				),
			},
			{
				// Import by <project_id>/<membership_id>.
				ResourceName:      memberRes,
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs := s.RootModule().Resources[memberRes].Primary
					return rs.Attributes["project_id"] + "/" + rs.ID, nil
				},
			},
			{
				// Import by <project_id>/user:<user_id>.
				ResourceName:      memberRes,
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs := s.RootModule().Resources[memberRes].Primary
					return rs.Attributes["project_id"] + "/user:" + rs.Attributes["user_id"], nil
				},
			},
			{
				Config: memberConfig(env, identifier, userID, "admin"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(memberRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(memberRes, "id", &membershipID),
					resource.TestCheckResourceAttr(memberRes, "role", "admin"),
					resource.TestCheckResourceAttr(memberRes, "lock_version", "1"),
				),
			},
		},
	})
}

func TestProjectMember_requestsUseMembershipIdAndIfMatch(t *testing.T) {
	env := newTestEnv(t, "project_member")
	env.requireFake(t)
	identifier := randIdentifier()
	userID := memberUserID(t, env)
	var membershipID string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: memberConfig(env, identifier, userID, "member"), Check: captureAttr(memberRes, "id", &membershipID)},
			{Config: memberConfig(env, identifier, userID, "admin")},
			{Config: projectFixture(env, identifier)},
		},
	})
	patches := env.fake.RequestsMatching("PATCH", "/api/v1/projects/")
	deletes := env.fake.RequestsMatching("DELETE", "/api/v1/projects/")
	var memberPatch, memberDelete *flightdecktestRequest
	for i := range patches {
		if len(patches[i].Path) > 0 && patches[i].Path[len(patches[i].Path)-len(membershipID):] == membershipID {
			memberPatch = &patches[i]
		}
	}
	for i := range deletes {
		if deletes[i].Path[len(deletes[i].Path)-len(membershipID):] == membershipID {
			memberDelete = &deletes[i]
		}
	}
	if memberPatch == nil || memberDelete == nil {
		t.Fatalf("expected PATCH and DELETE on .../members/%s; patches=%v deletes=%v", membershipID, patches, deletes)
	}
	if memberPatch.Header.Get("If-Match") != `"0"` {
		t.Errorf("member PATCH If-Match = %q", memberPatch.Header.Get("If-Match"))
	}
	if memberDelete.Header.Get("If-Match") != `"1"` {
		t.Errorf("member DELETE If-Match = %q, want the version after the role change", memberDelete.Header.Get("If-Match"))
	}
}

func TestProjectMember_customRoleKeyAndRejectedRole(t *testing.T) {
	env := newTestEnv(t, "project_member")
	env.requireFake(t)
	env.fake.AddRoleKey("release-manager")
	identifier := randIdentifier()
	userID := memberUserID(t, env)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: memberConfig(env, identifier, userID, "release-manager"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(memberRes, "role", "release-manager"),
					resource.TestCheckResourceAttr(memberRes, "builtin_role", "member"),
				),
			},
			{
				Config:      memberConfig(env, identifier, userID, "overlord"),
				ExpectError: regexMust(`(?s)HTTP 422 \(invalid_attribute\).*unknown\s+role`),
			},
		},
	})
}

func TestProjectMember_removedOutOfBandIsRecreated(t *testing.T) {
	env := newTestEnv(t, "project_member")
	env.requireFake(t)
	identifier := randIdentifier()
	userID := memberUserID(t, env)
	var membershipID string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: memberConfig(env, identifier, userID, "member"), Check: captureAttr(memberRes, "id", &membershipID)},
			{
				PreConfig: func() { env.fake.RemoveMemberOutOfBand(mustInt(membershipID)) },
				Config:    memberConfig(env, identifier, userID, "member"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(memberRes, plancheck.ResourceActionCreate)},
				},
				Check: func(s *terraform.State) error {
					if s.RootModule().Resources[memberRes].Primary.ID == membershipID {
						return fmt.Errorf("expected a new membership id after out-of-band removal")
					}
					return nil
				},
			},
			{
				// A role changed from the UI is a stale write here, reported not overwritten.
				PreConfig: func() {
					id := env.fake.MembersOf(projectIDOf(env, identifier))[1].ID
					env.fake.OnNextRequest("PATCH", fmt.Sprintf("/api/v1/projects/%d/members/%d", projectIDOf(env, identifier), id),
						func() { env.fake.TouchMember(id, "guest") })
				},
				Config:      memberConfig(env, identifier, userID, "admin"),
				ExpectError: regexMust(`(?s)Membership \d+ .* modified outside of Terraform`),
			},
		},
	})
}

func TestProjectMember_unknownUserIs404(t *testing.T) {
	env := newTestEnv(t, "project_member")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      memberConfig(env, identifier, 999999999, "member"),
				ExpectError: regexMust(`(?s)Error adding Flightdeck project member.*HTTP 404 \(not_found\)`),
			},
		},
	})
}

// projectIDOf finds the fake project created by projectFixture for identifier.
func projectIDOf(env *testEnv, identifier string) int64 {
	for _, id := range env.fake.AllProjectIDs() {
		if p := env.fake.Project(id); p != nil && p.Identifier == identifier && !p.Deleting {
			return id
		}
	}
	return 0
}
