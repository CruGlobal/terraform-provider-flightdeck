package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const projectLookupDS = "data.flightdeck_project.lookup"

// withProjectLookup adds a data source that reads the test project by
// identifier, after it is written.
func withProjectLookup(config, identifier string) string {
	return config + fmt.Sprintf(`
data "flightdeck_project" "lookup" {
  identifier = %q
  depends_on = [flightdeck_project.test]
}
`, identifier)
}

// expectTerraformManagedPlanned checks the plan's terraform_managed.
func expectTerraformManagedPlanned(managed bool) plancheck.PlanCheck {
	return plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("terraform_managed"), knownvalue.Bool(managed))
}

// sentTerraformManaged returns the terraform_managed a recorded project write
// carried, and whether it carried the key at all.
func sentTerraformManaged(t *testing.T, r flightdecktest.RecordedRequest) (any, bool) {
	t.Helper()
	var body struct {
		Project map[string]any `json:"project"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatalf("%s %s: decoding the body: %v", r.Method, r.Path, err)
	}
	v, ok := body.Project["terraform_managed"]
	return v, ok
}

// projectWrites returns the recorded creates of projects and writes to one
// project (not its sub-resources).
func projectWrites(fake *flightdecktest.Server, id string) (creates, patches []flightdecktest.RecordedRequest) {
	for _, r := range fake.Requests() {
		switch {
		case r.Method == http.MethodPost && r.Path == "/api/v1/projects":
			creates = append(creates, r)
		case r.Method == http.MethodPatch && id != "" && r.Path == "/api/v1/projects/"+id:
			patches = append(patches, r)
		}
	}
	return creates, patches
}

// A project is marked terraform_managed unless the configuration says false,
// the flag reads back (resource, import and data source), and dropping false
// from the configuration turns it on again.
func TestProjectTerraformManaged_defaultsOnAndCanBeTurnedOff(t *testing.T) {
	env := newTestEnv(t, "project")
	identifier := randIdentifier()
	name := randName("Managed")
	config := func(body string) string {
		return withProjectLookup(projectConfig(env, identifier, fmt.Sprintf("  name = %q\n%s", name, body)), identifier)
	}
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config(""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{expectTerraformManagedPlanned(true)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "terraform_managed", "true"),
					resource.TestCheckResourceAttr(projectLookupDS, "terraform_managed", "true"),
				),
			},
			{
				// Import reads the stored flag back.
				ResourceName:            projectRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"features"},
			},
			{
				Config: config("  terraform_managed = false\n"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate),
						expectTerraformManagedPlanned(false),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "terraform_managed", "false"),
					resource.TestCheckResourceAttr(projectLookupDS, "terraform_managed", "false"),
				),
			},
			{
				Config: config("  terraform_managed = false\n"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Unset is the default, true, not "keep what the project has".
				Config: config(""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate),
						expectTerraformManagedPlanned(true),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "terraform_managed", "true"),
					resource.TestCheckResourceAttr(projectLookupDS, "terraform_managed", "true"),
				),
			},
			{
				// An update for something else sends the flag too, as stored.
				Config: config("  description = \"Touched\"\n"),
				Check:  resource.TestCheckResourceAttr(projectRes, "terraform_managed", "true"),
			},
		},
	})
	if env.live() {
		return
	}
	if got := env.fake.Project(mustInt(id)); got == nil || !got.TerraformManaged {
		t.Fatalf("stored project %+v, want terraform_managed true", got)
	}
	creates, patches := projectWrites(env.fake, id)
	if len(creates) != 1 {
		t.Fatalf("expected 1 create, got %d", len(creates))
	}
	if v, sent := sentTerraformManaged(t, creates[0]); v != true || !sent {
		t.Errorf("create sent terraform_managed %v (sent %t), want true", v, sent)
	}
	want := []any{false, true, true}
	if len(patches) != len(want) {
		t.Fatalf("expected %d project PATCHes, got %d", len(want), len(patches))
	}
	for i, r := range patches {
		if v, sent := sentTerraformManaged(t, r); v != want[i] || !sent {
			t.Errorf("PATCH %d sent terraform_managed %v (sent %t), want %v: %s", i+1, v, sent, want[i], r.Body)
		}
	}
}

// A configuration can opt out from the start: the create sends false, which
// is also what Flightdeck stores when it's told nothing.
func TestProjectTerraformManaged_createdOff(t *testing.T) {
	env := newTestEnv(t, "project")
	identifier := randIdentifier()
	config := projectConfig(env, identifier, fmt.Sprintf(`
  name              = %q
  terraform_managed = false`, randName("Unmanaged")))
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "terraform_managed", "false"),
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
	if env.live() {
		return
	}
	creates, _ := projectWrites(env.fake, id)
	if len(creates) != 1 {
		t.Fatalf("expected 1 create, got %d", len(creates))
	}
	if v, sent := sentTerraformManaged(t, creates[0]); v != false || !sent {
		t.Errorf("create sent terraform_managed %v (sent %t), want false", v, sent)
	}
}

// Turned off by another API caller (the app can't change it): the refresh
// sees it and the plan turns it back on.
func TestProjectTerraformManaged_driftIsPlannedBack(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	config := projectConfig(env, identifier, `  name = "Drift"`)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  captureAttr(projectRes, "id", &id),
			},
			{
				PreConfig: func() { env.fake.SetTerraformManagedOutOfBand(mustInt(id), false) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate),
						expectTerraformManagedPlanned(true),
					},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "terraform_managed", "true"),
			},
		},
	})
	if got := env.fake.Project(mustInt(id)); got == nil || !got.TerraformManaged {
		t.Fatalf("stored project %+v, want terraform_managed true again", got)
	}
}

// Importing a project that is not marked yet reads false, and the next plan
// turns it on.
func TestProjectTerraformManaged_importingAnUnmarkedProjectPlansTrue(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	p := env.fake.AddProject("Imported", identifier)
	config := projectConfig(env, identifier, `  name = "Imported"`)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			func() resource.TestStep {
				step := importStep(config, projectRes, func() string { return identifier }, nil)
				step.ImportStateCheck = func(states []*terraform.InstanceState) error {
					if got := states[0].Attributes["terraform_managed"]; got != "false" {
						return fmt.Errorf("imported terraform_managed = %q, want false", got)
					}
					return nil
				}
				return step
			}(),
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate),
						expectTerraformManagedPlanned(true),
					},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "terraform_managed", "true"),
			},
		},
	})
	if got := env.fake.Project(p.ID); got == nil || !got.TerraformManaged {
		t.Fatalf("stored project %+v, want terraform_managed true", got)
	}
}

// Setting or changing the flag needs a workspace owner or admin; sending back
// the stored value doesn't. A refused write saves nothing, the rest of it
// included.
func TestProjectTerraformManaged_needsAWorkspaceAdminToChange(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	env.fake.SetWorkspaceAdmin(false)
	identifier := randIdentifier()
	config := func(name, body string) string {
		return projectConfig(env, identifier, fmt.Sprintf("  name = %q\n%s", name, body))
	}
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Created with the default: refused, and nothing is created.
				Config: config("Member", ""),
				ExpectError: regexMust(`(?s)Changing terraform_managed requires a workspace owner or admin.*` +
					`turns\s+terraform_managed\s+on.*workspace\s+owner\s+or\s+admin.*` +
					"set\\s+`terraform_managed\\s+=\\s+false`.*Nothing\\s+was\\s+saved.*" +
					`Only a workspace owner or\s+admin can set or change a project's terraform_managed`),
			},
			{
				// Created with the flag off, the column's own default: allowed.
				Config: config("Member", "  terraform_managed = false\n"),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "terraform_managed", "false"),
				),
			},
			{
				// Sending false back with another change is fine.
				Config: config("Member renamed", "  terraform_managed = false\n"),
				Check:  resource.TestCheckResourceAttr(projectRes, "name", "Member renamed"),
			},
			{
				// Turning it on is refused, and the rename in the same write is
				// not saved either.
				Config:      config("Member renamed again", ""),
				ExpectError: regexMust(`(?s)Changing terraform_managed requires a workspace owner or admin.*turns\s+terraform_managed\s+on`),
			},
			{
				// An admin turns it on.
				PreConfig: func() { env.fake.SetWorkspaceAdmin(true) },
				Config:    config("Member renamed", ""),
				Check:     resource.TestCheckResourceAttr(projectRes, "terraform_managed", "true"),
			},
			{
				// Now a member's update sends back true, the stored value: fine.
				PreConfig: func() { env.fake.SetWorkspaceAdmin(false) },
				Config:    config("Renamed by a member", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "name", "Renamed by a member"),
					resource.TestCheckResourceAttr(projectRes, "terraform_managed", "true"),
				),
			},
			{
				// Turning it off is refused too, with the other way out.
				Config: config("Renamed by a member", "  terraform_managed = false\n"),
				ExpectError: regexMust(`(?s)Changing terraform_managed requires a workspace owner or admin.*` +
					"turns\\s+terraform_managed\\s+off.*remove\\s+`terraform_managed\\s+=\\s+false`"),
			},
			{
				// The refusal changed nothing: back at the default, nothing to do.
				Config: config("Renamed by a member", ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
	var refused, created int
	for _, r := range env.fake.Requests() {
		switch {
		case r.Method == http.MethodPost && r.Path == "/api/v1/projects" && r.Status == http.StatusForbidden:
			refused++
		case r.Method == http.MethodPost && r.Path == "/api/v1/projects" && r.Status == http.StatusCreated:
			created++
		}
	}
	if refused != 1 || created != 1 {
		t.Errorf("project creates: %d refused, %d created; want 1 and 1", refused, created)
	}
	// The refused rename in step 4 left the name as step 3 set it, and the
	// accepted member update in step 6 carried the stored value.
	_, patches := projectWrites(env.fake, id)
	var sawMemberResend bool
	for _, r := range patches {
		if r.Status == http.StatusForbidden && strings.Contains(string(r.Body), "Member renamed again") {
			continue
		}
		if r.Status == http.StatusOK && strings.Contains(string(r.Body), "Renamed by a member") {
			if v, sent := sentTerraformManaged(t, r); v != true || !sent {
				t.Errorf("the member's update sent terraform_managed %v (sent %t), want the stored true", v, sent)
			}
			sawMemberResend = true
		}
	}
	if !sawMemberResend {
		t.Error("no accepted member update to check")
	}
	if got := env.fake.Project(mustInt(id)); got == nil || got.Name != "Renamed by a member" || !got.TerraformManaged {
		t.Fatalf("stored project %+v", got)
	}
}

// A Flightdeck older than the setting (here, one rolled back below it): the
// reads lose the key, a project that exists keeps working without it, and
// anything that would make the provider send it says to upgrade.
func TestProjectTerraformManaged_olderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier, second := randIdentifier(), randIdentifier()
	config := func(name, body string) string {
		return withProjectLookup(projectConfig(env, identifier, fmt.Sprintf("  name = %q\n%s", name, body)), identifier)
	}
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config("Rolled back", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "terraform_managed", "true"),
				),
			},
			{
				// Rolled back: the refresh finds no flag, and the unset default
				// plans nothing rather than a change the server would refuse.
				PreConfig: func() { env.fake.SetProjectsLegacy(true) },
				Config:    config("Rolled back", ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(projectRes, "terraform_managed"),
					resource.TestCheckNoResourceAttr(projectLookupDS, "terraform_managed"),
				),
			},
			{
				// Other changes still apply, without the key.
				Config: config("Rolled back, renamed", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "name", "Rolled back, renamed"),
					resource.TestCheckNoResourceAttr(projectRes, "terraform_managed"),
				),
			},
			{
				// Asking for it explicitly is refused, and says why.
				Config: config("Rolled back, renamed", "  terraform_managed = true\n"),
				ExpectError: regexMust(`(?s)This Flightdeck does not support terraform_managed yet.*` +
					`older\s+than\s+the\s+version\s+that\s+added\s+it.*Upgrade\s+Flightdeck\s+first.*Nothing\s+was\s+saved.*` +
					`leave\s+terraform_managed\s+unset.*unknown key: terraform_managed`),
			},
			{
				Config: config("Rolled back, renamed", ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// A create always names it, so it needs the upgrade.
				Config: config("Rolled back, renamed", "") + fmt.Sprintf(`
resource "flightdeck_project" "second" {
  name       = "Second"
  identifier = %q
}
`, second),
				ExpectError: regexMust(`(?s)This Flightdeck does not support terraform_managed yet.*Nothing\s+was\s+created.*` +
					`sends\s+terraform_managed\s+on\s+every\s+project\s+create`),
			},
		},
	})
	_, patches := projectWrites(env.fake, id)
	var renames int
	for _, r := range patches {
		if r.Status != http.StatusOK || !strings.Contains(string(r.Body), "Rolled back, renamed") {
			continue
		}
		renames++
		if v, sent := sentTerraformManaged(t, r); sent {
			t.Errorf("the rename on the older Flightdeck sent terraform_managed %v: %s", v, r.Body)
		}
	}
	if renames != 1 {
		t.Errorf("expected 1 accepted rename on the older Flightdeck, saw %d", renames)
	}
	for _, p := range env.fake.AllProjectIDs() {
		if got := env.fake.Project(p); got != nil && got.Identifier == second {
			t.Errorf("the refused create made project %+v", got)
		}
	}
}

// Imported from a Flightdeck that predates the setting, a project plans
// nothing for it; once Flightdeck is upgraded, the next plan turns it on.
// Upgraded between a plan and its apply, the write (which sent no flag) keeps
// it null rather than failing the apply on a value nobody planned.
func TestProjectTerraformManaged_olderFlightdeckThenUpgraded(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	env.fake.SetProjectsLegacy(true)
	identifier := randIdentifier()
	p := env.fake.AddProject("Upgraded", identifier)
	id := fmt.Sprint(p.ID)
	config := func(name string) string { return projectConfig(env, identifier, fmt.Sprintf("  name = %q", name)) }
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			func() resource.TestStep {
				step := importStep(config("Upgraded"), projectRes, func() string { return id }, nil)
				step.ImportStateCheck = func(states []*terraform.InstanceState) error {
					if got, ok := states[0].Attributes["terraform_managed"]; ok && got != "" {
						return fmt.Errorf("imported terraform_managed = %q from a Flightdeck that has none", got)
					}
					return nil
				}
				return step
			}(),
			{
				// Import lists every feature toggle, so this first apply stops
				// tracking the ones the configuration leaves out. The flag stays
				// null, and is not sent.
				Config: config("Upgraded"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("terraform_managed"), knownvalue.Null()),
					},
				},
				Check: resource.TestCheckNoResourceAttr(projectRes, "terraform_managed"),
			},
			{
				Config: config("Upgraded"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Planned against the older Flightdeck, applied against the
				// upgraded one.
				PreConfig: func() {
					env.fake.OnNextRequest(http.MethodPatch, "/api/v1/projects/"+id, func() { env.fake.SetProjectsLegacy(false) })
				},
				Config: config("Upgraded, renamed"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "name", "Upgraded, renamed"),
					resource.TestCheckNoResourceAttr(projectRes, "terraform_managed"),
				),
				// The refresh now reads the stored false, so the default diffs.
				ExpectNonEmptyPlan: true,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PostApplyPostRefresh: []plancheck.PlanCheck{expectTerraformManagedPlanned(true)},
				},
			},
			{
				Config: config("Upgraded, renamed"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate),
						expectTerraformManagedPlanned(true),
					},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "terraform_managed", "true"),
			},
		},
	})
	if got := env.fake.Project(p.ID); got == nil || !got.TerraformManaged {
		t.Fatalf("stored project %+v, want terraform_managed true", got)
	}
	// Only the last write, made after the upgrade was read, names the flag.
	_, patches := projectWrites(env.fake, id)
	if len(patches) != 3 {
		t.Fatalf("expected 3 project PATCHes (features, rename, flag), got %d", len(patches))
	}
	for i, r := range patches {
		v, sent := sentTerraformManaged(t, r)
		if wantSent := i == 2; sent != wantSent || (sent && v != true) {
			t.Errorf("PATCH %d sent terraform_managed %v (sent %t): %s", i+1, v, sent, r.Body)
		}
	}
}

// With no project to read, a create refused by an older Flightdeck can't be
// told apart from any other refusal, so it gets the generic error, which
// quotes the API's own "unknown key".
func TestProjectTerraformManaged_olderFlightdeckWithNoProjectsToAsk(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	env.fake.SetProjectsLegacy(true)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      projectConfig(env, randIdentifier(), `  name = "First"`),
				ExpectError: regexMust(`(?s)Error creating Flightdeck project.*HTTP 422 \(invalid_attribute\).*unknown key: terraform_managed`),
			},
		},
	})
}

func TestProjectFields_terraformManagedSentWheneverPlanned(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		managed types.Bool
		prior   *projectModel
		want    any
	}{
		{name: "on, on create", managed: types.BoolValue(true), want: true},
		{name: "off, on create", managed: types.BoolValue(false), want: false},
		{name: "unchanged on update", managed: types.BoolValue(true), prior: &projectModel{TerraformManaged: types.BoolValue(true)}, want: true},
		{name: "changed on update", managed: types.BoolValue(false), prior: &projectModel{TerraformManaged: types.BoolValue(true)}, want: false},
		// A Flightdeck that never reported it: planned null, never sent.
		{name: "unreported", managed: types.BoolNull(), prior: &projectModel{TerraformManaged: types.BoolNull()}},
		{name: "unknown", managed: types.BoolUnknown()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			plan := projectModel{Name: types.StringValue("n"), Identifier: types.StringValue("ID"), TerraformManaged: tc.managed}
			fields := projectFields(ctx, &plan, tc.prior, &diags)
			got, sent := fields["terraform_managed"]
			if tc.want == nil {
				if sent {
					t.Fatalf("terraform_managed sent as %v", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("terraform_managed = %v (sent %t), want %v", got, sent, tc.want)
			}
		})
	}
}

func TestProjectAdminChangesOf(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name    string
		fields  client.Fields
		prior   *projectModel
		app     bool
		managed *bool
	}{
		{name: "create turning it on", fields: client.Fields{"terraform_managed": true}, managed: &on},
		// false is what a create stores anyway.
		{name: "create leaving it off", fields: client.Fields{"terraform_managed": false}},
		{name: "create setting the app", fields: client.Fields{"app": "a", "terraform_managed": false}, app: true},
		{name: "update sending it back", fields: client.Fields{"terraform_managed": true},
			prior: &projectModel{TerraformManaged: types.BoolValue(true)}},
		{name: "update turning it off", fields: client.Fields{"terraform_managed": false},
			prior: &projectModel{TerraformManaged: types.BoolValue(true)}, managed: &off},
		{name: "update turning it on", fields: client.Fields{"terraform_managed": true},
			prior: &projectModel{TerraformManaged: types.BoolValue(false)}, managed: &on},
		// Never reported, so it can't be known to be unchanged.
		{name: "update where none was reported", fields: client.Fields{"terraform_managed": true},
			prior: &projectModel{TerraformManaged: types.BoolNull()}, managed: &on},
		{name: "update not sending it", fields: client.Fields{"name": "n"}, prior: &projectModel{TerraformManaged: types.BoolNull()}},
		{name: "both", fields: client.Fields{"app": "a", "terraform_managed": true}, app: true, managed: &on},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := projectAdminChangesOf(tc.fields, tc.prior)
			if got.app != tc.app {
				t.Errorf("app = %t, want %t", got.app, tc.app)
			}
			switch {
			case tc.managed == nil && got.terraformManaged != nil:
				t.Errorf("terraformManaged = %t, want none", *got.terraformManaged)
			case tc.managed != nil && (got.terraformManaged == nil || *got.terraformManaged != *tc.managed):
				t.Errorf("terraformManaged = %v, want %t", got.terraformManaged, *tc.managed)
			}
		})
	}
}

func TestAddProjectWriteError_terraformManagedRefusals(t *testing.T) {
	on, off := true, false
	forbidden := func(method, message string) error {
		return &client.Error{Method: method, Path: "/projects/1", Status: http.StatusForbidden, Code: client.CodeForbidden, Message: message}
	}
	const tmRefusal = "Only a workspace owner or admin can set or change a project's terraform_managed."
	type want struct {
		path    path.Path
		summary string
		detail  []string
	}
	tmOn := want{path.Root("terraform_managed"), "Changing terraform_managed requires a workspace owner or admin",
		[]string{"turns terraform_managed on", "must belong to a workspace owner or admin", "set `terraform_managed = false`", "Nothing was saved", tmRefusal}}
	for _, tc := range []struct {
		name    string
		changes projectAdminChanges
		err     error
		want    []want
	}{
		{name: "turning it on", changes: projectAdminChanges{terraformManaged: &on}, err: forbidden(http.MethodPost, tmRefusal), want: []want{tmOn}},
		{
			name: "turning it off", changes: projectAdminChanges{terraformManaged: &off}, err: forbidden(http.MethodPatch, tmRefusal),
			want: []want{{path.Root("terraform_managed"), "Changing terraform_managed requires a workspace owner or admin",
				[]string{"turns terraform_managed off", "remove `terraform_managed = false`", tmRefusal}}},
		},
		{
			// Both need the bar, so both are named, whichever Flightdeck checked.
			name: "changing it and the app", changes: projectAdminChanges{app: true, terraformManaged: &on},
			err: forbidden(http.MethodPatch, "Only a workspace owner or admin can set or change a project's app."),
			want: []want{
				{path.Root("app"), "Setting a project's app requires a workspace owner or admin", []string{"sets or changes `app`"}},
				{path.Root("terraform_managed"), "Changing terraform_managed requires a workspace owner or admin", []string{"turns terraform_managed on"}},
			},
		},
		{
			// A 403 from the read that verifies a create is the project role.
			name: "403 from the verifying read", changes: projectAdminChanges{terraformManaged: &on},
			err:  forbidden(http.MethodGet, "Your project role does not permit this action."),
			want: []want{{path.Empty(), "Error updating Flightdeck project", []string{"lacks the project or workspace role"}}},
		},
		{
			name: "403 sending it back unchanged", changes: projectAdminChanges{},
			err:  forbidden(http.MethodPatch, "Your project role does not permit this action."),
			want: []want{{path.Empty(), "Error updating Flightdeck project", []string{"lacks the project or workspace role"}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			addProjectWriteError(&diags, "Error updating Flightdeck project", tc.changes, tc.err)
			if diags.ErrorsCount() != len(tc.want) {
				t.Fatalf("expected %d errors, got %v", len(tc.want), diags)
			}
			for i, w := range tc.want {
				d := diags.Errors()[i]
				if d.Summary() != w.summary {
					t.Errorf("error %d summary = %q, want %q", i, d.Summary(), w.summary)
				}
				var at path.Path
				if withPath, ok := d.(diag.DiagnosticWithPath); ok {
					at = withPath.Path()
				}
				if at.String() != w.path.String() {
					t.Errorf("error %d is on %q, want %q", i, at, w.path)
				}
				for _, s := range w.detail {
					if !strings.Contains(d.Detail(), s) {
						t.Errorf("error %d detail does not say %q:\n%s", i, s, d.Detail())
					}
				}
			}
		})
	}
}

// The fake reads terraform_managed the way the API does: a strict boolean, a
// blank as no opinion, the stored value accepted back from anyone, a change
// only from a workspace owner or admin, and on an older Flightdeck an unknown
// key whatever its value.
func TestFake_readsTerraformManagedLikeTheAPI(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	ctx := context.Background()
	c, err := client.New(env.endpoint, env.token)
	if err != nil {
		t.Fatal(err)
	}
	p := env.fake.AddProject("Flagged", randIdentifier())
	patch := func(fields client.Fields) (*client.Project, error) {
		t.Helper()
		fresh, err := c.GetProject(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		return c.UpdateProject(ctx, p.ID, fields, fresh.LockVersion)
	}
	managed := func(got *client.Project) string {
		if got == nil || got.TerraformManaged == nil {
			return "none"
		}
		return fmt.Sprint(*got.TerraformManaged)
	}

	if got, err := c.GetProject(ctx, p.ID); err != nil || managed(got) != "false" {
		t.Fatalf("a new project reads terraform_managed %s (%v), want false", managed(got), err)
	}
	for _, bad := range []any{1, 0, "yes", []any{true}, map[string]any{"on": true}} {
		_, err := patch(client.Fields{"terraform_managed": bad})
		if !client.HasCode(err, client.CodeInvalidAttribute) || !strings.Contains(apiMessage(err), "terraform_managed must be true or false") {
			t.Errorf("terraform_managed %#v: err = %v, want 422 invalid_attribute naming it", bad, err)
		}
	}
	for _, blank := range []any{nil, "", "  "} {
		if got, err := patch(client.Fields{"terraform_managed": blank}); err != nil || managed(got) != "false" {
			t.Errorf("terraform_managed %#v: got %s, %v; want no opinion", blank, managed(got), err)
		}
	}

	env.fake.SetWorkspaceAdmin(false)
	if got, err := patch(client.Fields{"terraform_managed": false, "name": "Sent back"}); err != nil || got.Name != "Sent back" {
		t.Errorf("a member sending back the stored value: %+v, %v", got, err)
	}
	_, err = patch(client.Fields{"terraform_managed": true, "name": "Not saved"})
	if !client.IsForbidden(err) || !client.HasCode(err, client.CodeForbidden) ||
		apiMessage(err) != "Only a workspace owner or admin can set or change a project's terraform_managed." {
		t.Errorf("a member turning it on: err = %v, want the 403", err)
	}
	if got := env.fake.Project(p.ID); got.Name != "Sent back" || got.TerraformManaged {
		t.Errorf("a refused write saved something: %+v", got)
	}

	env.fake.SetWorkspaceAdmin(true)
	if got, err := patch(client.Fields{"terraform_managed": " ON "}); err != nil || managed(got) != "true" {
		t.Errorf("an admin's form-string \" ON \": got %s, %v", managed(got), err)
	}

	env.fake.SetProjectsLegacy(true)
	for _, v := range []any{nil, true} {
		_, err := patch(client.Fields{"terraform_managed": v})
		if !client.HasCode(err, client.CodeInvalidAttribute) || !strings.HasPrefix(apiMessage(err), "unknown key: terraform_managed (settable: ") {
			t.Errorf("an older Flightdeck given terraform_managed %#v: err = %v, want the unknown key", v, err)
		}
	}
	if got, err := c.GetProject(ctx, p.ID); err != nil || got.ReportsTerraformManaged() {
		t.Errorf("an older Flightdeck's read reports terraform_managed %s (%v)", managed(got), err)
	}
	if list, err := c.ListProjects(ctx); err != nil || len(list) != 1 || list[0].ReportsTerraformManaged() {
		t.Errorf("an older Flightdeck's list: %+v, %v", list, err)
	}
}
