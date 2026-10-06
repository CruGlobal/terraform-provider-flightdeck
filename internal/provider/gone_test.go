package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// hiddenProjectConfig is a project with a label and a member in it, and a
// teamspace that owns it.
func hiddenProjectConfig(env *testEnv, identifier string) string {
	return projectFixture(env, identifier) + fmt.Sprintf(`
resource "flightdeck_label" "test" {
  project_id = flightdeck_project.parent.id
  name       = "Hidden"
}

resource "flightdeck_project_member" "test" {
  project_id = flightdeck_project.parent.id
  user_id    = %d
  role       = "member"
}

resource "flightdeck_teamspace" "owner" {
  name = "Owner of %s"
}

resource "flightdeck_teamspace_project" "test" {
  teamspace_id = flightdeck_teamspace.owner.id
  project_id   = flightdeck_project.parent.id
}
`, env.fake.Members()[1].ID, identifier)
}

// When the token's user loses access to a project, Flightdeck answers 404
// for it and for everything in it, the same as for a deleted project. The
// refresh removes them all from state, and each removal warns, naming the
// resource and its import id; the teamspace that owns the project is not in
// it and stays. Applying the plan that follows would fail, because the
// hidden project still holds its identifier. The documented way back works:
// restore access, import, and nothing is created again.
func TestHiddenProject_resourcesLeaveStateWithAWarning(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	config := hiddenProjectConfig(env, identifier)
	const parentRes, ownerRes, linkRes = "flightdeck_project.parent", "flightdeck_teamspace.owner", teamspaceProjectRes
	var projectID, labelID, memberID, teamspaceID string
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(parentRes, "id", &projectID),
					captureAttr(labelRes, "id", &labelID),
					captureAttr(memberRes, "id", &memberID),
					captureAttr(ownerRes, "id", &teamspaceID),
				),
			},
			{
				PreConfig:          func() { env.fake.HideProjectFromToken(mustInt(projectID), true) },
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check: func(s *terraform.State) error {
					var left []string
					for address := range s.RootModule().Resources {
						left = append(left, address)
					}
					if len(left) != 1 || left[0] != ownerRes {
						return fmt.Errorf("want only %s left in state after the refresh, have %v", ownerRes, left)
					}
					return nil
				},
			},
			{
				// The plan offers to create everything again, and applying it fails.
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(parentRes, plancheck.ResourceActionCreate),
						plancheck.ExpectResourceAction(labelRes, plancheck.ResourceActionCreate),
						plancheck.ExpectResourceAction(memberRes, plancheck.ResourceActionCreate),
						plancheck.ExpectResourceAction(linkRes, plancheck.ResourceActionCreate),
						plancheck.ExpectResourceAction(ownerRes, plancheck.ResourceActionNoop),
					},
				},
				ExpectError: regexMust(`Identifier\s+has\s+already\s+been\s+taken`),
			},
			importStep(config, parentRes, func() string { return projectID }, func() { env.fake.HideProjectFromToken(mustInt(projectID), false) }),
			importStep(config, labelRes, func() string { return labelID }, nil),
			importStep(config, memberRes, func() string { return projectID + "/" + memberID }, nil),
			importStep(config, linkRes, func() string { return teamspaceID + "/" + projectID }, nil),
			{
				// Nothing is created again. An imported project lists every
				// feature toggle, so a configuration that names none plans an
				// in-place update that stops tracking them, as after any import.
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(parentRes, plancheck.ResourceActionUpdate),
						plancheck.ExpectResourceAction(labelRes, plancheck.ResourceActionNoop),
						plancheck.ExpectResourceAction(memberRes, plancheck.ResourceActionNoop),
						plancheck.ExpectResourceAction(linkRes, plancheck.ResourceActionNoop),
						plancheck.ExpectResourceAction(ownerRes, plancheck.ResourceActionNoop),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(parentRes, "id", &projectID),
					resource.TestCheckResourceAttrPtr(labelRes, "id", &labelID),
					resource.TestCheckResourceAttrPtr(memberRes, "id", &memberID),
				),
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
	if n := len(env.fake.AllProjectIDs()); n != 1 {
		t.Errorf("want the one project, the fake holds %d", n)
	}
	warnings := recorded.readWarnings()
	for what, want := range map[string][]string{
		"the project": {fmt.Sprintf("404 for project %s (id %s)", identifier, projectID), "Its import id is `" + projectID + "`"},
		"the label": {fmt.Sprintf("404 for label %s, and for project %s as well", labelID, projectID),
			"Its import id is `" + labelID + "`"},
		"the member": {fmt.Sprintf("404 for membership %s (user %d), and for project %s as well", memberID, env.fake.Members()[1].ID, projectID),
			"Its import id is `" + projectID + "/" + memberID + "`"},
		"the teamspace link": {fmt.Sprintf("404 for teamspace %s's link to project %s, and for project %s as well", teamspaceID, projectID, projectID),
			"Its import id is `" + teamspaceID + "/" + projectID + "`"},
	} {
		if !anyDiagnostic(warnings, append([]string{hiddenProjectSummary, "Restore the token user's access",
			"`terraform import`"}, want...)...) {
			t.Errorf("no warning for %s; refresh warnings:\n%s", what, describe(warnings))
		}
	}
}

// importStep imports one resource into state with the id importID returns.
func importStep(config, address string, importID func() string, before func()) resource.TestStep {
	return resource.TestStep{
		PreConfig:          before,
		Config:             config,
		ResourceName:       address,
		ImportState:        true,
		ImportStatePersist: true,
		ImportStateIdFunc:  func(*terraform.State) (string, error) { return importID(), nil },
	}
}

// A resource deleted from a project the token can still see is removed from
// state as before, with no warning: the project reads back, so the 404 meant
// the resource itself was deleted.
func TestHiddenProject_noWarningWhenOnlyTheResourceIsGone(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	config := hiddenProjectConfig(env, identifier)
	var memberID string
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  captureAttr(memberRes, "id", &memberID),
			},
			{
				PreConfig:          func() { env.fake.RemoveMemberOutOfBand(mustInt(memberID)) },
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check: func(s *terraform.State) error {
					if _, ok := s.RootModule().Resources[memberRes]; ok {
						return fmt.Errorf("%s should have left state", memberRes)
					}
					return nil
				},
			},
			{
				// The plan adds the member back, and that is right.
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(memberRes, plancheck.ResourceActionCreate)},
				},
			},
		},
	})
	if anyDiagnostic(recorded.readWarnings(), hiddenProjectSummary) {
		t.Errorf("a resource deleted from a visible project warned:\n%s", describe(recorded.readWarnings()))
	}
}
