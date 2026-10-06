package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// These tests cover the ids a write names that Flightdeck won't accept: a
// project or team lead, a project member's user and a webhook's project. Each
// is a bad argument, reported against the attribute, and never a sign that
// the resource is gone.

// addRefusedID picks the attribute from the field name a refusal starts with,
// and leaves anything else to the generic error.
func TestAddRefusedID(t *testing.T) {
	refusal := &client.Error{Method: "POST", Path: "/projects/4/members", Status: 422, Code: client.CodeInvalidAttribute,
		Message: "user_id 7 is not a member of this workspace; only workspace members can be added to a project"}
	var diags diag.Diagnostics
	if !addRefusedID(&diags, "user_id", "Cannot add this user", "Only members. Nothing was added.", refusal) {
		t.Fatal("a refusal naming user_id was not reported")
	}
	if len(diags) != 1 || diags[0].Severity() != diag.SeverityError {
		t.Fatalf("want one error, got %v", diags)
	}
	withPath, ok := diags[0].(diag.DiagnosticWithPath)
	if !ok || !withPath.Path().Equal(path.Root("user_id")) {
		t.Errorf("the error is not reported against user_id: %#v", diags[0])
	}
	if detail := diags[0].Detail(); !strings.HasPrefix(detail, "Only members. Nothing was added.") ||
		!strings.Contains(detail, "The API said: user_id 7 is not a member of this workspace") {
		t.Errorf("detail = %q", detail)
	}

	for name, err := range map[string]error{
		"another field's refusal":     &client.Error{Status: 422, Code: client.CodeInvalidAttribute, Message: "unknown role: boss"},
		"a longer field name":         &client.Error{Status: 422, Code: client.CodeInvalidAttribute, Message: "user_ids must be a list"},
		"no code, from an older API":  &client.Error{Status: 422, Message: "user_id 7 is not a member of this workspace"},
		"a validation_failed":         &client.Error{Status: 422, Code: client.CodeValidationFailed, Message: "user_id 7 is taken"},
		"a 404":                       &client.Error{Status: 404, Code: client.CodeNotFound, Message: "Not found"},
		"an error that isn't the API": errors.New("user_id 7 is not a member of this workspace"),
	} {
		var diags diag.Diagnostics
		if addRefusedID(&diags, "user_id", "Cannot add this user", "Only members.", err) || diags.HasError() {
			t.Errorf("%s: reported as a refused user_id: %v", name, diags)
		}
	}
}

// An id below 1 names nobody, so it fails the plan instead of the apply.
func TestRefusedIDs_belowOneFailsThePlan(t *testing.T) {
	env := newTestEnv(t, "project")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name    = "Lead zero"
  lead_id = 0`),
				ExpectError: regexMust(`(?s)lead_id\s+value\s+must\s+be\s+at\s+least\s+1`),
			},
			{
				Config:      memberConfig(env, identifier, 0, "member"),
				ExpectError: regexMust(`(?s)user_id\s+value\s+must\s+be\s+at\s+least\s+1`),
			},
			{
				Config: webhookConfig(env, `
  project_id = -1
  url        = "https://ci.example.com/hooks/ids"
  events     = ["project.updated"]`),
				ExpectError: regexMust(`(?s)project_id\s+value\s+must\s+be\s+at\s+least\s+1`),
			},
		},
	})
}

// A workspace guest can't be made a project's lead. Flightdeck checks the
// role, which the provider can't see, so the apply fails against lead_id
// with the API's reason, on a create and on an update, and the project is
// left as it was. A guest who already leads the project stays its lead:
// sending them back, or leaving lead_id unset, is accepted.
func TestProject_guestLead(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	const guest = 3 // the fake seeds user 3 as a workspace guest
	guestRefused := regexMust(`(?s)Flightdeck\s+refused\s+this\s+project\s+lead.*not\s+a\s+guest` +
		`.*lead_id\s+3\s+is\s+a\s+guest\s+in\s+this\s+workspace;\s+guests\s+can't\s+lead\s+a\s+project`)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, fmt.Sprintf(`
  name    = "Guest led"
  lead_id = %d`, guest)),
				ExpectError: guestRefused,
			},
			{
				Config: projectConfig(env, identifier, `  name = "Guest led"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "lead_id", "1"),
				),
			},
			{
				Config: projectConfig(env, identifier, fmt.Sprintf(`
  name    = "Guest led"
  lead_id = %d`, guest)),
				ExpectError: guestRefused,
			},
			{
				// Still in state, unchanged.
				Config: projectConfig(env, identifier, `  name = "Guest led"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// A guest who led the project before Flightdeck refused guests
				// can be sent back with another change.
				PreConfig: func() { env.fake.SetProjectLeadOutOfBand(mustInt(id), guest) },
				Config: projectConfig(env, identifier, fmt.Sprintf(`
  name    = "Guest led, renamed"
  lead_id = %d`, guest)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "name", "Guest led, renamed"),
					resource.TestCheckResourceAttr(projectRes, "lead_id", "3"),
				),
			},
			{
				// Leaving lead_id unset keeps them too.
				Config: projectConfig(env, identifier, `  name = "Guest led, renamed again"`),
				Check:  resource.TestCheckResourceAttr(projectRes, "lead_id", "3"),
			},
		},
	})
}

// A lead who has since left the workspace stays the project's lead. An apply
// that changes something else sends them back unchanged, and Flightdeck
// accepts that.
func TestProject_leadWhoLeftTheWorkspaceStays(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	lead := env.fake.AddMember("Leaving Lead", "leaving-lead@example.com")
	config := func(name string) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name    = %q
  lead_id = %d`, name, lead.ID))
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config("Led by a leaver")},
			{
				PreConfig: func() { env.fake.RemoveWorkspaceMember(lead.ID) },
				Config:    config("Led by a leaver, renamed"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "name", "Led by a leaver, renamed"),
					resource.TestCheckResourceAttr(projectRes, "lead_id", fmt.Sprint(lead.ID)),
				),
			},
		},
	})
}

// A team whose lead has left the workspace can still be changed: the update
// sends the stored lead back, and Flightdeck accepts it.
func TestTeamspace_leadWhoLeftTheWorkspaceStays(t *testing.T) {
	env := newTestEnv(t, "teamspace")
	env.requireFake(t)
	lead := env.fake.AddMember("Leaving Lead", "leaving-lead@example.com")
	name := randName("Team")
	config := func(name string) string {
		return teamspaceConfig(env, fmt.Sprintf(`
  name    = %q
  lead_id = %d`, name, lead.ID))
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config(name)},
			{
				PreConfig: func() { env.fake.RemoveWorkspaceMember(lead.ID) },
				Config:    config(name + " renamed"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(teamspaceRes, "name", name+" renamed"),
					resource.TestCheckResourceAttr(teamspaceRes, "lead_id", fmt.Sprint(lead.ID)),
				),
			},
		},
	})
}

// A webhook's project must be one the token can see. Flightdeck answers any
// other id with a 422 naming project_id, worded the same for a project that
// doesn't exist and one the token can't see, and the provider reports it
// against project_id, on a create and on an update. Versions before that
// answered 404, which gets the generic error. Either way the webhook stays in
// state, unchanged.
func TestWebhook_projectYouCannotSeeIsRefused(t *testing.T) {
	env := newTestEnv(t, "webhook")
	url := "https://ci.example.com/hooks/" + strings.ToLower(randIdentifier())
	config := func(projectLine string) string {
		return webhookConfig(env, fmt.Sprintf(`%s
  url    = %q
  events = ["project.updated"]`, projectLine, url))
	}
	refused := func(verb string) string {
		return `(?s)(Error\s+` + verb + `\s+Flightdeck\s+webhook.*HTTP\s+404\s+\(not_found\)|` +
			`Cannot\s+scope\s+the\s+webhook\s+to\s+this\s+project.*project_id\s+999999999\s+is\s+not\s+a\s+project\s+you\s+can\s+see)`
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      config(`  project_id = 999999999`),
				ExpectError: regexMust(refused("creating")),
			},
			{Config: config("")},
			{
				Config:      config(`  project_id = 999999999`),
				ExpectError: regexMust(refused("updating")),
			},
			{
				Config: config(""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// The fake reads user_id on an existing membership the way the API does: a
// blank leaves the membership alone, the stored id in any spelling of an id
// is accepted back, a value that is not an id is refused as on a create, and
// any other id is a change, which is refused. On a create, a blank user_id is
// a missing one, which only the save refuses, after the role is read.
func TestFake_readsMemberUserIDLikeTheAPI(t *testing.T) {
	env := newTestEnv(t, "project_member")
	env.requireFake(t)
	ctx := context.Background()
	c, err := client.New(env.endpoint, env.token)
	if err != nil {
		t.Fatal(err)
	}
	p := env.fake.AddProject("Member ids", randIdentifier())
	add := func(fields client.Fields) (*client.ProjectMember, error) {
		return c.AddProjectMember(ctx, p.ID, fields, client.RandomIdempotencyKey())
	}
	if _, err := add(client.Fields{"user_id": "", "role": "member"}); !client.HasCode(err, client.CodeValidationFailed) ||
		apiMessage(err) != "User must exist" {
		t.Errorf("a blank user_id on a create: err = %v, want 422 validation_failed \"User must exist\"", err)
	}
	if _, err := add(client.Fields{"user_id": nil, "role": "boss"}); !client.HasCode(err, client.CodeInvalidAttribute) ||
		apiMessage(err) != "unknown role: boss" {
		t.Errorf("a blank user_id and a bad role: err = %v, want the role refused first", err)
	}
	m, err := add(client.Fields{"user_id": 2, "role": "member"})
	if err != nil {
		t.Fatal(err)
	}
	patch := func(user any) (*client.ProjectMember, error) {
		fresh, err := c.GetProjectMember(ctx, p.ID, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		return c.UpdateProjectMember(ctx, p.ID, m.ID, client.Fields{"user_id": user, "role": "commenter"}, fresh.LockVersion)
	}
	for _, kept := range []any{nil, "", "  ", 2, "2", " 2 "} {
		if got, err := patch(kept); err != nil || got.UserID != 2 || got.Role != "commenter" {
			t.Errorf("user_id %#v on an update: got %+v, %v; want the membership kept and the role changed", kept, got, err)
		}
	}
	if _, err := patch("2a"); !client.HasCode(err, client.CodeInvalidAttribute) ||
		apiMessage(err) != `user_id must be an integer id of 0 or more, got "2a"` {
		t.Errorf("a user_id that is not an id on an update: err = %v", err)
	}
	if _, err := patch(1); !client.HasCode(err, client.CodeInvalidAttribute) ||
		!strings.HasPrefix(apiMessage(err), "user_id cannot be changed on an existing membership") {
		t.Errorf("another user_id on an update: err = %v", err)
	}
}

// The fake reads a webhook's project_id the way the API does: false, a list
// and an object are refused by name rather than read as "all projects", a
// blank means all projects, and the project the webhook already has is
// accepted back even while it is being deleted, though a change to a project
// being deleted is refused.
func TestFake_readsWebhookProjectIDLikeTheAPI(t *testing.T) {
	env := newTestEnv(t, "webhook")
	env.requireFake(t)
	ctx := context.Background()
	c, err := client.New(env.endpoint, env.token)
	if err != nil {
		t.Fatal(err)
	}
	create := func(project any) (*client.Webhook, error) {
		return c.CreateWebhook(ctx, client.Fields{"url": "https://ci.example.com/hooks/" + strings.ToLower(randIdentifier()),
			"events": []string{"project.updated"}, "project_id": project}, client.RandomIdempotencyKey())
	}
	for _, notAnID := range []any{false, []any{}, map[string]any{}} {
		if _, err := create(notAnID); !client.HasCode(err, client.CodeInvalidAttribute) ||
			!strings.HasPrefix(apiMessage(err), "project_id must be") {
			t.Errorf("project_id %#v: err = %v, want 422 invalid_attribute naming project_id", notAnID, err)
		}
	}
	if h, err := create(" "); err != nil || h.ProjectID != nil {
		t.Errorf("a blank project_id: got %+v, %v; want a workspace-wide webhook", h, err)
	}

	dying := env.fake.AddProject("Dying", randIdentifier())
	other := env.fake.AddProject("Also dying", randIdentifier())
	h, err := create(dying.ID)
	if err != nil {
		t.Fatal(err)
	}
	env.fake.DeleteProjectOutOfBand(dying.ID)
	env.fake.DeleteProjectOutOfBand(other.ID)
	patch := func(project int64) (*client.Webhook, error) {
		fresh, err := c.GetWebhook(ctx, h.ID)
		if err != nil {
			t.Fatal(err)
		}
		return c.UpdateWebhook(ctx, h.ID, client.Fields{"project_id": project, "active": false}, fresh.LockVersion)
	}
	if got, err := patch(dying.ID); err != nil || got.Active || got.ProjectID == nil || *got.ProjectID != dying.ID {
		t.Errorf("sending back a project being deleted: got %+v, %v; want the webhook paused", got, err)
	}
	if _, err := patch(other.ID); !client.HasCode(err, client.CodeInvalidAttribute) ||
		apiMessage(err) != fmt.Sprintf("project_id %d is not a project you can see in this workspace", other.ID) {
		t.Errorf("a change to a project being deleted: err = %v", err)
	}
}
