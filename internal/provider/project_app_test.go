package provider

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// randApp returns an app name unique enough for a shared workspace, in mixed
// case so a round trip also proves the API keeps the spelling it was sent.
func randApp() string {
	return "TfAcc-" + acctest.RandString(8) + ".App_x"
}

func TestProjectApp_setChangeAndKeep(t *testing.T) {
	env := newTestEnv(t, "project")
	identifier := randIdentifier()
	first, second := randApp(), randApp()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, fmt.Sprintf(`
  name = "App"
  app  = %q`, first)) + fmt.Sprintf(`
data "flightdeck_project" "by_identifier" {
  identifier = %q
  depends_on = [flightdeck_project.test]
}
`, identifier),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "app", first),
					// The data source looks the project up by listing, so this
					// also proves the list carries the app.
					resource.TestCheckResourceAttr("data.flightdeck_project.by_identifier", "app", first),
				),
			},
			{
				ResourceName:            projectRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"features"},
			},
			{
				// Import by identifier goes through the list, not the detail read.
				ResourceName:            projectRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"features"},
				ImportStateIdFunc:       func(*terraform.State) (string, error) { return identifier, nil },
			},
			{
				// A workspace admin may move the project to another app, in place.
				Config: projectConfig(env, identifier, fmt.Sprintf(`
  name = "App"
  app  = %q`, second)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "app", second),
				),
			},
			{
				// A change of case alone is still a change, and lands as sent.
				Config: projectConfig(env, identifier, fmt.Sprintf(`
  name = "App"
  app  = %q`, strings.ToLower(second))),
				Check: resource.TestCheckResourceAttr(projectRes, "app", strings.ToLower(second)),
			},
			{
				// Dropping app from configuration leaves it alone: the API cannot
				// clear it, and the provider does not try.
				Config: projectConfig(env, identifier, `  name = "App"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "app", strings.ToLower(second)),
			},
			{
				// ...and an unrelated update does not disturb it either.
				Config: projectConfig(env, identifier, `  name = "App renamed"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "name", "App renamed"),
					resource.TestCheckResourceAttr(projectRes, "app", strings.ToLower(second)),
				),
			},
		},
	})
}

// An app belongs to one project in a workspace, compared ignoring case.
func TestProjectApp_takenByAnotherProject(t *testing.T) {
	env := newTestEnv(t, "project")
	holder, claimant := randIdentifier(), randIdentifier()
	app := randApp()
	config := func(claimantApp string) string {
		return env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_project" "holder" {
  name       = "App holder"
  identifier = %q
  app        = %q
}

resource "flightdeck_project" "claimant" {
  name       = "App claimant"
  identifier = %q
%s
  depends_on = [flightdeck_project.holder]
}
`, holder, app, claimant, claimantApp)
	}
	// A third project, created (not updated) into the taken app.
	withLateComer := func(identifier string) string {
		return config("") + fmt.Sprintf(`
resource "flightdeck_project" "late_comer" {
  name       = "App late comer"
  identifier = %q
  app        = %q
  depends_on = [flightdeck_project.claimant]
}
`, identifier, strings.ToLower(app))
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config(""),
				Check:  resource.TestCheckResourceAttr("flightdeck_project.holder", "app", app),
			},
			{
				Config:      config(fmt.Sprintf("  app = %q", strings.ToUpper(app))),
				ExpectError: regexMust(`App already belongs to another project`),
			},
			{
				// The same on create; nothing is written.
				Config:      withLateComer(randIdentifier()),
				ExpectError: regexMust(`App already belongs to another project`),
			},
			{
				// A write that also fails another check is validation_failed
				// listing both, which is not blamed on the app alone.
				Config:      withLateComer(holder),
				ExpectError: regexMust(`(?is)Error\s+creating\s+Flightdeck\s+project.*validation_failed.*` + regexp.QuoteMeta(strings.ToLower(app))),
			},
		},
	})
}

// Replacing a project that has an app (destroy, then create with the same app)
// must not trip over the project being torn down.
func TestProjectApp_replaceKeepsTheApp(t *testing.T) {
	env := newTestEnv(t, "project")
	identifier := randIdentifier()
	app := randApp()
	config := projectConfig(env, identifier, fmt.Sprintf(`
  name = "Replaced"
  app  = %q`, app))
	var firstID string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  captureAttr(projectRes, "id", &firstID),
			},
			{
				Taint:  []string{projectRes},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "app", app),
					func(s *terraform.State) error {
						if s.RootModule().Resources[projectRes].Primary.ID == firstID {
							return fmt.Errorf("the project was not replaced")
						}
						return nil
					},
				),
			},
		},
	})
}

// Until an app is set, the first release event that names one binds it. An
// unset app is unmanaged, so the binding survives every later apply.
func TestProjectApp_pipelineBindingSurvivesAnApply(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `  name = "Bound later"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckNoResourceAttr(projectRes, "app"),
				),
			},
			{
				// Bound between applies: the refresh reads it, nothing to plan.
				PreConfig: func() { env.fake.BindApp(mustInt(id), "bound-app") },
				Config:    projectConfig(env, identifier, `  name = "Bound later"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "app", "bound-app"),
			},
			{
				Config: projectConfig(env, identifier, `  name = "Bound later, renamed"`),
				Check:  resource.TestCheckResourceAttr(projectRes, "app", "bound-app"),
			},
		},
	})
	patches := env.fake.RequestsMatching("PATCH", "/api/v1/projects/"+id)
	if len(patches) == 0 {
		t.Fatal("the rename sent no PATCH, so nothing was checked")
	}
	for _, r := range patches {
		if strings.Contains(string(r.Body), `"app"`) {
			t.Fatalf("a configuration without app sent one: %s", r.Body)
		}
	}
}

// The first binding bumps lock_version, so an apply that races it loses once
// with a lock conflict, and a re-plan clears it.
func TestProjectApp_bindingRacingAnApplyIsOneLockConflict(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `  name = "Race"`),
				Check:  captureAttr(projectRes, "id", &id),
			},
			{
				PreConfig: func() {
					env.fake.OnNextRequest(http.MethodPatch, "/api/v1/projects/"+id, func() { env.fake.BindApp(mustInt(id), "raced-app") })
				},
				Config:      projectConfig(env, identifier, `  name = "Race v2"`),
				ExpectError: regexMust(`modified outside of Terraform`),
			},
			{
				Config: projectConfig(env, identifier, `  name = "Race v2"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "name", "Race v2"),
					resource.TestCheckResourceAttr(projectRes, "app", "raced-app"),
				),
			},
		},
	})
}

func TestProjectApp_needsAWorkspaceAdminToChange(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier, other := randIdentifier(), randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Set by an admin.
				Config: projectConfig(env, identifier, `
  name = "Admin set"
  app  = "admin-set"`),
				Check: resource.TestCheckResourceAttr(projectRes, "app", "admin-set"),
			},
			{
				// A token that is not a workspace admin can still make an
				// unrelated update with the app configured: an unchanged app is
				// not sent.
				PreConfig: func() { env.fake.SetWorkspaceAdmin(false) },
				Config: projectConfig(env, identifier, `
  name = "Admin set, renamed"
  app  = "admin-set"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "name", "Admin set, renamed"),
					resource.TestCheckResourceAttr(projectRes, "app", "admin-set"),
				),
			},
			{
				// Changing it is refused, and says which token is needed.
				Config: projectConfig(env, identifier, `
  name = "Admin set, renamed"
  app  = "changed-by-member"`),
				ExpectError: regexMust(`(?s)Setting a project's app requires a workspace\s+owner\s+or\s+admin`),
			},
			{
				// So is naming one on create (with terraform_managed off, which is
				// the stored default, so the only bar is the app's).
				Config: projectConfig(env, identifier, `
  name = "Admin set, renamed"
  app  = "admin-set"`) + fmt.Sprintf(`
resource "flightdeck_project" "other" {
  name              = "Member create"
  identifier        = %q
  app               = "member-create"
  terraform_managed = false
}
`, other),
				ExpectError: regexMust(`(?s)Setting a project's app requires a workspace\s+owner\s+or\s+admin`),
			},
		},
	})
	// The accepted rename in step 2 must not have named the app at all: the
	// API would take the stored value back, so only the request log shows it.
	var accepted int
	for _, r := range env.fake.RequestsMatching("PATCH", "/api/v1/projects/") {
		if r.Status != http.StatusOK {
			continue
		}
		accepted++
		if strings.Contains(string(r.Body), `"app"`) {
			t.Fatalf("an update that did not change the app named it: %s", r.Body)
		}
	}
	if accepted == 0 {
		t.Fatal("no accepted project PATCH, so nothing was checked")
	}
}

func TestProjectApp_validation(t *testing.T) {
	env := newTestEnv(t, "project")
	identifier := randIdentifier()
	var steps []resource.TestStep
	for _, bad := range []string{"", " padded", "has space", "slash/name", strings.Repeat("a", 101)} {
		steps = append(steps, resource.TestStep{
			Config: projectConfig(env, identifier, fmt.Sprintf(`
  name = "Bad app"
  app  = %q`, bad)),
			ExpectError: regexMust(`must be 1-100 letters, digits, '\.', '_' or '-'`),
		})
	}
	runTest(t, resource.TestCase{Steps: steps})
}

func TestProjectFields_appSentOnlyWhenItChanges(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		app   types.String
		prior *projectModel
		want  any
	}{
		{name: "set on create", app: types.StringValue("my-app"), want: "my-app"},
		{name: "unset on create", app: types.StringUnknown()},
		{name: "set where none was", app: types.StringValue("my-app"), prior: &projectModel{App: types.StringNull()}, want: "my-app"},
		{name: "changed", app: types.StringValue("new-app"), prior: &projectModel{App: types.StringValue("old-app")}, want: "new-app"},
		{name: "changed case only", app: types.StringValue("My-App"), prior: &projectModel{App: types.StringValue("my-app")}, want: "My-App"},
		{name: "unchanged", app: types.StringValue("my-app"), prior: &projectModel{App: types.StringValue("my-app")}},
		// Unset in configuration plans the prior value, which is never sent.
		{name: "unset on update", app: types.StringValue("bound-app"), prior: &projectModel{App: types.StringValue("bound-app")}},
		{name: "unset and never bound", app: types.StringNull(), prior: &projectModel{App: types.StringNull()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			plan := projectModel{Name: types.StringValue("n"), Identifier: types.StringValue("ID"), App: tc.app}
			fields := projectFields(ctx, &plan, tc.prior, &diags)
			got, sent := fields["app"]
			if tc.want == nil {
				if sent {
					t.Fatalf("app sent as %v when %s", got, tc.name)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("app = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAddProjectWriteError_appRefusals(t *testing.T) {
	apiErr := func(status int, code, message string) error {
		return &client.Error{Method: http.MethodPatch, Path: "/projects/1", Status: status, Code: code, Message: message}
	}
	for _, tc := range []struct {
		name        string
		changingApp bool
		err         error
		wantSummary string
		// wantOnApp is whether the diagnostic points at the app attribute.
		wantOnApp  bool
		wantDetail []string
	}{
		{
			name:        "app taken",
			changingApp: true,
			err:         apiErr(422, client.CodeAppTaken, "my-app already belongs to the Web project."),
			wantSummary: "App already belongs to another project",
			wantOnApp:   true,
			wantDetail:  []string{"my-app already belongs to the Web project.", "(app_taken)", "Nothing was written"},
		},
		{
			name:        "403 while changing the app",
			changingApp: true,
			err:         apiErr(403, client.CodeForbidden, "Only a workspace owner or admin can set a project's app."),
			wantSummary: "Setting a project's app requires a workspace owner or admin",
			wantOnApp:   true,
			wantDetail:  []string{"workspace owner or admin token", "Only a workspace owner or admin can set a project's app."},
		},
		{
			// A 403 from the read that verifies a create is not the app's bar.
			name:        "403 from the verifying read",
			changingApp: true,
			err:         &client.Error{Method: http.MethodGet, Path: "/projects/1", Status: 403, Code: client.CodeForbidden, Message: "Your project role does not permit this action."},
			wantSummary: "Error updating Flightdeck project",
			wantDetail:  []string{"lacks the project or workspace role"},
		},
		{
			// With the app unchanged, a 403 is the project role, not the app.
			name:        "403 with the app unchanged",
			changingApp: false,
			err:         apiErr(403, client.CodeForbidden, "Your project role does not permit this action."),
			wantSummary: "Error updating Flightdeck project",
			wantDetail:  []string{"lacks the project or workspace role"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			addProjectWriteError(&diags, "Error updating Flightdeck project", projectAdminChanges{app: tc.changingApp}, tc.err)
			if diags.ErrorsCount() != 1 {
				t.Fatalf("expected one error, got %v", diags)
			}
			d := diags.Errors()[0]
			if d.Summary() != tc.wantSummary {
				t.Fatalf("summary = %q, want %q", d.Summary(), tc.wantSummary)
			}
			withPath, onAttr := d.(diag.DiagnosticWithPath)
			onApp := onAttr && withPath.Path().Equal(path.Root("app"))
			if onApp != tc.wantOnApp {
				t.Fatalf("diagnostic on the app attribute = %t, want %t: %v", onApp, tc.wantOnApp, d)
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(d.Detail(), want) {
					t.Fatalf("detail does not say %q:\n%s", want, d.Detail())
				}
			}
		})
	}
}
