package provider

import (
	"fmt"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const memberRes = "flightdeck_project_member.test"

// memberEmail is the workspace member the tests grant access to: a seeded
// user of the fake, or FLIGHTDECK_ACC_MEMBER_EMAIL against a live instance.
func memberEmail(t *testing.T, env *testEnv) string {
	t.Helper()
	if env.live() {
		email := os.Getenv(envAccMemberEmail)
		if email == "" {
			t.Skipf("%s must be set for project member tests against a live instance", envAccMemberEmail)
		}
		return email
	}
	return env.fake.Members()[1].Email
}

func memberConfig(env *testEnv, identifier, email, role string) string {
	return projectFixture(env, identifier) + fmt.Sprintf(`
data "flightdeck_workspace_member" "who" {
  email = %q
}

resource "flightdeck_project_member" "test" {
  project_id = flightdeck_project.parent.id
  user_id    = data.flightdeck_workspace_member.who.id
  role       = %q
}
`, email, role)
}

func TestProjectMember_basicLifecycle(t *testing.T) {
	env := newTestEnv(t, "project_member")
	identifier := randIdentifier()
	email := memberEmail(t, env)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: memberConfig(env, identifier, email, "member"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(memberRes, "id"),
					resource.TestCheckResourceAttrPair(memberRes, "project_id", "flightdeck_project.parent", "id"),
					resource.TestCheckResourceAttrPair(memberRes, "user_id", "data.flightdeck_workspace_member.who", "id"),
					resource.TestCheckResourceAttr(memberRes, "role", "member"),
				),
			},
			{
				ResourceName:      memberRes,
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs := s.RootModule().Resources[memberRes].Primary
					return rs.Attributes["project_id"] + "/" + rs.Attributes["user_id"], nil
				},
			},
			{
				Config: memberConfig(env, identifier, email, "admin"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(memberRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.TestCheckResourceAttr(memberRes, "role", "admin"),
			},
		},
	})
}

func TestProjectMember_customRoleKeyAndRejectedRole(t *testing.T) {
	env := newTestEnv(t, "project_member")
	env.requireFake(t)
	env.fake.AddRoleKey("release-manager")
	identifier := randIdentifier()
	email := memberEmail(t, env)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: memberConfig(env, identifier, email, "release-manager"),
				Check:  resource.TestCheckResourceAttr(memberRes, "role", "release-manager"),
			},
			{
				Config:      memberConfig(env, identifier, email, "overlord"),
				ExpectError: regexMust(`(?s)HTTP 422 \(invalid_attribute\).*not an assignable role`),
			},
		},
	})
}

func TestProjectMember_removedOutOfBandIsRecreated(t *testing.T) {
	env := newTestEnv(t, "project_member")
	env.requireFake(t)
	identifier := randIdentifier()
	email := memberEmail(t, env)
	var projectID, userID string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: memberConfig(env, identifier, email, "member"),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(memberRes, "project_id", &projectID),
					captureAttr(memberRes, "user_id", &userID),
				),
			},
			{
				PreConfig: func() {
					// Remove the row the way the members UI would.
					for _, m := range env.fake.MembersOf(mustInt(projectID)) {
						if m.UserID == mustInt(userID) {
							env.fake.TouchMember(mustInt(projectID), mustInt(userID), "guest")
						}
					}
				},
				Config: memberConfig(env, identifier, email, "member"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(memberRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.TestCheckResourceAttr(memberRes, "role", "member"),
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
				Config: projectFixture(env, identifier) + `
resource "flightdeck_project_member" "test" {
  project_id = flightdeck_project.parent.id
  user_id    = 999999999
  role       = "member"
}
`,
				ExpectError: regexMust(`(?s)Error adding Flightdeck project member.*HTTP 404 \(not_found\)`),
			},
		},
	})
}

func TestWorkspaceMemberDataSource(t *testing.T) {
	env := newTestEnv(t, "workspace_member")
	email := memberEmail(t, env)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: env.providerConfig() + fmt.Sprintf(`
data "flightdeck_workspace_member" "who" {
  email = %q
}
`, email),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("data.flightdeck_workspace_member.who", "id"),
					resource.TestCheckResourceAttrSet("data.flightdeck_workspace_member.who", "name"),
					resource.TestCheckResourceAttr("data.flightdeck_workspace_member.who", "email", email),
				),
			},
			{
				Config: env.providerConfig() + `
data "flightdeck_workspace_member" "who" {
  email = "nobody-here@example.invalid"
}
`,
				ExpectError: regexMust(`(?s)No workspace member with email\s+"nobody-here@example.invalid"`),
			},
		},
	})
}

func TestWorkspaceMemberDataSource_caseInsensitive(t *testing.T) {
	env := newTestEnv(t, "workspace_member")
	env.requireFake(t)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: env.providerConfig() + `
data "flightdeck_workspace_member" "who" {
  email = "ALEX@Example.com"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.flightdeck_workspace_member.who", "id", "2"),
					resource.TestCheckResourceAttr("data.flightdeck_workspace_member.who", "email", "alex@example.com"),
					resource.TestCheckResourceAttr("data.flightdeck_workspace_member.who", "role", "member"),
				),
			},
		},
	})
}

func TestProjectMember_preconditionRequiredWithoutLockVersionIsDiagnosed(t *testing.T) {
	env := newTestEnv(t, "project_member")
	env.requireFake(t)
	env.fake.RequireMemberPrecondition(true)
	identifier := randIdentifier()
	email := memberEmail(t, env)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: memberConfig(env, identifier, email, "member"),
				Check:  resource.TestCheckNoResourceAttr(memberRes, "lock_version"),
			},
			{
				Config:      memberConfig(env, identifier, email, "admin"),
				ExpectError: regexMust(`(?s)requires a precondition on member updates.*did not\s+return a lock_version`),
			},
		},
	})
}
