package provider

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

const wsMemberDS = "data.flightdeck_workspace_member.test"

func workspaceMemberConfig(env *testEnv, email string) string {
	return env.providerConfig() + fmt.Sprintf(`
data "flightdeck_workspace_member" "test" {
  email = %q
}
`, email)
}

// The fake seeds a known roster; a live workspace's addresses are its own, so
// the tests that need a particular one resolve it from a membership instead
// (see TestWorkspaceMember_resolvesAMembershipsAddress).
func TestWorkspaceMember_resolvesByEmail(t *testing.T) {
	env := newTestEnv(t, "workspace_member")
	env.requireFake(t)
	member := env.fake.Members()[1]
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: workspaceMemberConfig(env, member.Email),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(wsMemberDS, "id", fmt.Sprint(member.ID)),
					resource.TestCheckResourceAttr(wsMemberDS, "name", member.Name),
					resource.TestCheckResourceAttr(wsMemberDS, "email", member.Email),
					resource.TestCheckResourceAttr(wsMemberDS, "kind", "human"),
				),
			},
			{
				// Case and surrounding whitespace are the server's to ignore.
				Config: workspaceMemberConfig(env, "  "+strings.ToUpper(member.Email)+"  "),
				Check:  resource.TestCheckResourceAttr(wsMemberDS, "id", fmt.Sprint(member.ID)),
			},
		},
	})
}

// An address nobody holds fails the plan: an empty id would only fail later,
// somewhere less obvious.
func TestWorkspaceMember_unknownEmailFailsThePlan(t *testing.T) {
	env := newTestEnv(t, "workspace_member")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      workspaceMemberConfig(env, randName("nobody")+"@example.com"),
				ExpectError: regexMust(`No such workspace member`),
			},
			{
				// …and the message offers the other explanation, because a
				// lookup a token may not see is indistinguishable from a miss.
				Config:      workspaceMemberConfig(env, randName("nobody")+"@example.com"),
				ExpectError: regexMust(`service\s+account`),
			},
		},
	})
}

// The exact-match filter cannot match a prefix or a fragment, so a partial
// address is a miss rather than a lucky hit.
func TestWorkspaceMember_partialAddressDoesNotMatch(t *testing.T) {
	env := newTestEnv(t, "workspace_member")
	env.requireFake(t)
	email := env.fake.Members()[1].Email
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      workspaceMemberConfig(env, email[:len(email)-4]),
				ExpectError: regexMust(`No such workspace member`),
			},
		},
	})
}

func TestWorkspaceMember_serviceAccountResolvesForAWorkspaceAdmin(t *testing.T) {
	env := newTestEnv(t, "workspace_member")
	env.requireFake(t)
	bot := env.fake.AddServiceAccount("Release Bot", "release-bot@example.com")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: workspaceMemberConfig(env, bot.Email),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(wsMemberDS, "id", fmt.Sprint(bot.ID)),
					// What tells a bot apart from a person with a similar address.
					resource.TestCheckResourceAttr(wsMemberDS, "kind", "service"),
				),
			},
		},
	})
}

// Without the workspace-admin role a service account is absent from the
// directory rather than refused, so the lookup misses.
func TestWorkspaceMember_serviceAccountIsInvisibleToANonAdmin(t *testing.T) {
	env := newTestEnv(t, "workspace_member")
	env.requireFake(t)
	bot := env.fake.AddServiceAccount("Release Bot", "release-bot@example.com")
	env.fake.SetWorkspaceAdmin(false)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      workspaceMemberConfig(env, bot.Email),
				ExpectError: regexMust(`No such workspace member`),
			},
		},
	})
}

// The filter resolves at most one row, so more than one is the API breaking
// its contract: refuse rather than pick.
func TestWorkspaceMember_ambiguousLookupIsRefused(t *testing.T) {
	env := newTestEnv(t, "workspace_member")
	env.requireFake(t)
	env.fake.AddMember("Twin One", "twin@example.com")
	env.fake.AddMember("Twin Two", "twin@example.com")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      workspaceMemberConfig(env, "twin@example.com"),
				ExpectError: regexMust(`Ambiguous workspace member lookup`),
			},
		},
	})
}

// The pattern this exists for, and the one a live workspace can run without
// knowing any of its addresses in advance: a membership reports who it is
// for, and that address resolves back to the same user id.
func TestWorkspaceMember_resolvesAMembershipsAddress(t *testing.T) {
	env := newTestEnv(t, "workspace_member")
	identifier := randIdentifier()
	userID := memberUserID(t, env)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: memberConfig(env, identifier, userID, "member") + `
data "flightdeck_workspace_member" "test" {
  email = flightdeck_project_member.test.email
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					// The membership says who it is for…
					resource.TestCheckResourceAttrSet(memberRes, "email"),
					resource.TestCheckResourceAttrSet(memberRes, "name"),
					// …and that address resolves to the id the membership was
					// written with.
					resource.TestCheckResourceAttr(wsMemberDS, "id", fmt.Sprint(userID)),
					resource.TestCheckResourceAttrPair(wsMemberDS, "id", memberRes, "user_id"),
					resource.TestCheckResourceAttrPair(wsMemberDS, "name", memberRes, "name"),
					resource.TestCheckResourceAttrPair(wsMemberDS, "email", memberRes, "email"),
				),
			},
		},
	})
}
