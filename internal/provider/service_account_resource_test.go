package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const serviceAccountRes = "flightdeck_service_account.test"

// randBotEmail is an example.com address unique enough for a shared test
// workspace. Flightdeck never deletes a service account's user, only its
// place in a workspace, so every address a live run uses stays behind in its
// user table; the random part keeps one run from adding back another's.
func randBotEmail(prefix string) string {
	return fmt.Sprintf("%s-%s@machineusers.example.com", prefix, strings.ToLower(acctest.RandString(10)))
}

func serviceAccountConfig(env *testEnv, body string) string {
	return env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_service_account" "test" {
%s
}
`, body)
}

func TestServiceAccount_basicLifecycle(t *testing.T) {
	env := newTestEnv(t, "service_account")
	email := randBotEmail("deploy")
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: serviceAccountConfig(env, fmt.Sprintf(`
  email_address = %q
  name          = "Deploy Bot"`, email)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(serviceAccountRes, "id"),
					resource.TestCheckResourceAttr(serviceAccountRes, "email_address", email),
					resource.TestCheckResourceAttr(serviceAccountRes, "name", "Deploy Bot"),
					resource.TestCheckResourceAttr(serviceAccountRes, "role", "member"),
					resource.TestMatchResourceAttr(serviceAccountRes, "joined_at", regexMust(`^\d{4}-\d{2}-\d{2}T`)),
					captureAttr(serviceAccountRes, "id", &id),
				),
			},
			{
				ResourceName:      serviceAccountRes,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				Config: serviceAccountConfig(env, fmt.Sprintf(`
  email_address = %q
  name          = "Deploy Robot"`, email)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(serviceAccountRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(serviceAccountRes, "id", &id),
					resource.TestCheckResourceAttr(serviceAccountRes, "name", "Deploy Robot"),
					func(*terraform.State) error {
						if env.live() {
							return nil
						}
						if name, ok := env.fake.ServiceAccountInWorkspace(mustInt(id)); !ok || name != "Deploy Robot" {
							return fmt.Errorf("server has %q (in workspace: %v)", name, ok)
						}
						return nil
					},
				),
			},
			{
				Config: serviceAccountConfig(env, fmt.Sprintf(`
  email_address = %q
  name          = "Deploy Robot"`, email)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// A new address is a new service account.
				Config: serviceAccountConfig(env, fmt.Sprintf(`
  email_address = %q
  name          = "Deploy Robot"`, randBotEmail("other"))),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(serviceAccountRes, plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: func(s *terraform.State) error {
					if s.RootModule().Resources[serviceAccountRes].Primary.ID == id {
						return fmt.Errorf("a new address kept id %s", id)
					}
					return nil
				},
			},
		},
	})
	if !env.live() {
		if _, ok := env.fake.ServiceAccountInWorkspace(mustInt(id)); ok {
			t.Errorf("the replaced service account %s is still in the workspace", id)
		}
	}
}

// With no name, Flightdeck names the account after the part of its address
// before the @. Removing a name later keeps the one it has.
func TestServiceAccount_nameDefaultsToTheLocalPart(t *testing.T) {
	env := newTestEnv(t, "service_account")
	email := randBotEmail("ci")
	local := email[:strings.Index(email, "@")]
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: serviceAccountConfig(env, fmt.Sprintf(`  email_address = %q`, email)),
				Check:  resource.TestCheckResourceAttr(serviceAccountRes, "name", local),
			},
			{
				Config: serviceAccountConfig(env, fmt.Sprintf(`  email_address = %q`, email)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config: serviceAccountConfig(env, fmt.Sprintf(`
  email_address = %q
  name          = "CI Bot"`, email)),
				Check: resource.TestCheckResourceAttr(serviceAccountRes, "name", "CI Bot"),
			},
			{
				Config: serviceAccountConfig(env, fmt.Sprintf(`  email_address = %q`, email)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(serviceAccountRes, "name", "CI Bot"),
			},
		},
	})
}

// Flightdeck strips and lowercases an address, so one written any other way
// would show as a change on every plan. It fails the plan instead, saying
// how to write it. So does anything that isn't an address, and a name
// Flightdeck would not store as written.
func TestServiceAccount_planTimeValidation(t *testing.T) {
	env := newTestEnv(t, "service_account")
	step := func(body string, want string) resource.TestStep {
		return resource.TestStep{Config: serviceAccountConfig(env, body), ExpectError: regexMust(want)}
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			step(`  email_address = "Deploy-Bot@MachineUsers.Example.com"`,
				`(?s)Write the address as Flightdeck stores it.*Write\s+"deploy-bot@machineusers.example.com"\s+instead`),
			step(`  email_address = " deploy-bot@machineusers.example.com"`,
				`(?s)Write the address as Flightdeck stores it.*Write\s+"deploy-bot@machineusers.example.com"\s+instead`),
			step(`  email_address = "deploy-bot"`, `(?s)Not an email address.*exactly\s+one\s+.@.`),
			step(`  email_address = "deploy@bot@example.com"`, `(?s)Not an email address`),
			step(`  email_address = "deploy bot@example.com"`, `(?s)Not an email address`),
			step(`  email_address = ""`, `(?s)Not an email address`),
			step(`
  email_address = "deploy-bot@machineusers.example.com"
  name          = "  "`, `(?s)Blank value.*leave\s+.name.\s+out`),
			step(`
  email_address = "deploy-bot@machineusers.example.com"
  name          = "Deploy Bot "`, `(?s)Blank space at either end`),
			step(fmt.Sprintf(`
  email_address = "deploy-bot@machineusers.example.com"
  name          = %q`, strings.Repeat("n", 256)), `(?s)Value too long.*at most 255`),
			step(fmt.Sprintf(`  email_address = "%s@machineusers.example.com"`, strings.Repeat("a", 250)), `(?s)Value too long.*at most 255`),
		},
	})
}

// An address Flightdeck already has as a service account in no workspace at
// all (one removed earlier; the user is kept) comes back with the
// configured name, in the create itself, and the same id.
func TestServiceAccount_accountInNoWorkspaceComesBackWithTheConfiguredName(t *testing.T) {
	env := newTestEnv(t, "service_account")
	env.requireFake(t)
	email := randBotEmail("returning")
	existing := env.fake.AddServiceAccountElsewhere("Old Name", email, false)
	config := serviceAccountConfig(env, fmt.Sprintf(`
  email_address = %q
  name          = "New Name"`, email))
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(serviceAccountRes, "id", fmt.Sprint(existing)),
					resource.TestCheckResourceAttr(serviceAccountRes, "name", "New Name"),
					func(*terraform.State) error {
						if name, ok := env.fake.ServiceAccountInWorkspace(existing); !ok || name != "New Name" {
							return fmt.Errorf("server has %q (in workspace: %v)", name, ok)
						}
						if n := len(env.fake.RequestsMatching(http.MethodPatch, "/api/v1/service-accounts/")); n != 0 {
							return fmt.Errorf("PATCHes = %d, want none: the create gave it the name", n)
						}
						return nil
					},
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
}

// A create Flightdeck replays answers with the account as it is now. When
// it was renamed since, the provider renames it back to the configured
// name, so the apply ends as planned. Here the account leaves state without
// being deleted, is renamed outside Terraform, and is declared again within
// the day the API remembers the create.
func TestServiceAccount_replayOfARenamedAccountIsRenamedBack(t *testing.T) {
	env := newTestEnv(t, "service_account")
	email := randBotEmail("renamed")
	config := serviceAccountConfig(env, fmt.Sprintf(`
  email_address = %q
  name          = "Configured"`, email))
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config, Check: captureAttr(serviceAccountRes, "id", &id)},
			{
				Config: env.providerConfig() + `
removed {
  from = flightdeck_service_account.test
  lifecycle {
    destroy = false
  }
}
`,
			},
			{
				PreConfig: func() {
					if _, err := webClient(t, env).UpdateServiceAccount(context.Background(), mustInt(id),
						client.Fields{"name": "Renamed elsewhere"}); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(serviceAccountRes, "id", &id),
					resource.TestCheckResourceAttr(serviceAccountRes, "name", "Configured"),
					func(*terraform.State) error {
						if env.live() {
							return nil
						}
						if name, _ := env.fake.ServiceAccountInWorkspace(mustInt(id)); name != "Configured" {
							return fmt.Errorf("server has %q", name)
						}
						// One rename from the test, one from the provider.
						if n := len(env.fake.RequestsMatching(http.MethodPatch, "/api/v1/service-accounts/"+id)); n != 2 {
							return fmt.Errorf("PATCHes = %d, want 2", n)
						}
						return nil
					},
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
}

// An account that is also in another workspace keeps its name everywhere,
// and Flightdeck refuses to rename it here. A create naming another name
// fails, takes the account back out of this workspace and says which name
// to use; either fix then works. A later rename fails the same way, and
// changes nothing.
func TestServiceAccount_accountSharedWithAnotherWorkspaceKeepsItsName(t *testing.T) {
	env := newTestEnv(t, "service_account")
	env.requireFake(t)
	email := randBotEmail("shared")
	shared := env.fake.AddServiceAccountElsewhere("Shared Bot", email, true)
	withName := func(name string) string {
		return serviceAccountConfig(env, fmt.Sprintf(`
  email_address = %q
  name          = %q`, email, name))
	}
	notHere := func() error {
		if _, ok := env.fake.ServiceAccountInWorkspace(shared); ok {
			return fmt.Errorf("service account %d was left in the workspace", shared)
		}
		return nil
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: withName("Mine"),
				ExpectError: regexMust(`(?s)This address belongs to a service account in another workspace.*` +
					email + `\s+already\s+belongs\s+to\s+a\s+service\s+account\s+named\s+"Shared Bot".*` +
					`Set\s+.name\s+=\s+"Shared Bot".*leave\s+.name.\s+out.*taken\s+back\s+out\s+of\s+this\s+workspace`),
			},
			{
				PreConfig: func() {
					if err := notHere(); err != nil {
						t.Fatal(err)
					}
				},
				Config: withName("Shared Bot"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(serviceAccountRes, "id", fmt.Sprint(shared)),
					resource.TestCheckResourceAttr(serviceAccountRes, "name", "Shared Bot"),
				),
			},
			{
				Config: withName("Renamed"),
				ExpectError: regexMust(`(?s)This service account's name can't be changed here.*another\s+workspace.*` +
					`Set\s+.name\s+=\s+"Shared Bot".*Nothing\s+was\s+changed`),
			},
			{
				// Out of Terraform, then back with no name: it takes the name it has.
				Config: env.providerConfig(),
			},
			{
				Config: serviceAccountConfig(env, fmt.Sprintf(`  email_address = %q`, email)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(serviceAccountRes, "id", fmt.Sprint(shared)),
					resource.TestCheckResourceAttr(serviceAccountRes, "name", "Shared Bot"),
				),
			},
		},
	})
	if name, _ := env.fake.ServiceAccountInWorkspace(shared); name == "Renamed" {
		t.Errorf("the shared account was renamed")
	}
}

// replayOfAnAccountInUse creates a service account, lets it leave state
// without being deleted (a handover to another configuration, or lost
// state), renames it and gives it a token outside Terraform, and then
// declares it again within the day Flightdeck replays the create. The replay
// answers with the original 201, so this apply did not add the account: it
// was already here, and in use. arrange runs just before that last apply,
// with the account's id, to make the rename back fail; wantErr is the error
// that apply must end with. It returns the account's id and the token's.
func replayOfAnAccountInUse(t *testing.T, env *testEnv, arrange func(id int64), wantErr string) (string, int64, *applyDiagnostics) {
	t.Helper()
	config := serviceAccountConfig(env, fmt.Sprintf(`
  email_address = %q
  name          = "Configured"`, randBotEmail("inuse")))
	var id string
	var tokenID int64
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config, Check: captureAttr(serviceAccountRes, "id", &id)},
			{
				Config: env.providerConfig() + `
removed {
  from = flightdeck_service_account.test
  lifecycle {
    destroy = false
  }
}
`,
			},
			{
				PreConfig: func() {
					c := webClient(t, env)
					if _, err := c.UpdateServiceAccount(context.Background(), mustInt(id),
						client.Fields{"name": "Renamed elsewhere"}); err != nil {
						t.Fatal(err)
					}
					tok, err := c.CreateServiceAccountAPIToken(context.Background(), mustInt(id),
						client.Fields{"name": "used by an app"}, client.RandomIdempotencyKey())
					if err != nil {
						t.Fatal(err)
					}
					tokenID = tok.ID
					arrange(mustInt(id))
				},
				Config:      config,
				ExpectError: regexMust(wantErr),
			},
		},
	})
	return id, tokenID, recorded
}

// assertServiceAccountKept fails the test when the apply sent a DELETE for
// the service account, or when it, or its token, is gone.
func assertServiceAccountKept(t *testing.T, env *testEnv, id string, name string, tokenID int64) {
	t.Helper()
	for _, r := range env.fake.RequestsMatching(http.MethodDelete, "/api/v1/service-accounts/"+id) {
		if r.Path == "/api/v1/service-accounts/"+id {
			t.Errorf("the apply sent DELETE %s (status %d) for an account it must keep", r.Path, r.Status)
		}
	}
	if got, ok := env.fake.ServiceAccountInWorkspace(mustInt(id)); !ok || got != name {
		t.Errorf("service account %s: in workspace = %v, name = %q, want in the workspace as %q", id, ok, got, name)
	}
	if tokenID != 0 && env.fake.ServiceAccountAPIToken(tokenID) == nil {
		t.Errorf("token %d of service account %s was deleted", tokenID, id)
	}
}

// A replayed create of an account that was already here, renamed since and
// now also in another workspace, can't rename it back. The account was not
// added by this apply, so it stays, with its token, and the error gives the
// id to import it with.
func TestServiceAccount_replayWhoseRenameIsRefusedAsSharedKeepsTheAccount(t *testing.T) {
	env := newTestEnv(t, "service_account")
	env.requireFake(t)
	id, tokenID, recorded := replayOfAnAccountInUse(t, env,
		func(id int64) { env.fake.JoinServiceAccountToAnotherWorkspace(id) },
		`(?s)This service account's name can't be changed here.*already\s+in\s+this\s+workspace.*`+
			`also\s+in\s+another\s+workspace.*left\s+in\s+this\s+workspace`)
	assertServiceAccountKept(t, env, id, "Renamed elsewhere", tokenID)
	errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError)
	if !anyDiagnostic(errs, "Service account "+id+" (", "terraform import <resource address> "+id,
		"id = \""+id+"\"", "set `name = \"Renamed elsewhere\"`") {
		t.Errorf("no error names id %s to import and the name to set; errors:\n%s", id, describe(errs))
	}
	if anyDiagnostic(errs, "taken back out") {
		t.Errorf("an error claims the account was taken out of the workspace; errors:\n%s", describe(errs))
	}
}

// The same replay, with the account only in this workspace, meets a gateway
// error on the rename back. A rename that failed for a reason other than a
// shared name says nothing about the account, so it stays, with its token.
func TestServiceAccount_replayWhoseRenameFailsKeepsTheAccount(t *testing.T) {
	env := newTestEnv(t, "service_account")
	env.requireFake(t)
	id, tokenID, recorded := replayOfAnAccountInUse(t, env,
		func(id int64) {
			env.fake.RefuseNext(http.MethodPatch, fmt.Sprintf("/api/v1/service-accounts/%d", id), http.StatusBadGateway, "", "Bad Gateway")
		},
		`(?s)Error renaming Flightdeck service account.*already\s+in\s+this\s+workspace.*left\s+in\s+this\s+workspace`)
	assertServiceAccountKept(t, env, id, "Renamed elsewhere", tokenID)
	errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError)
	if !anyDiagnostic(errs, "Service account "+id+" (", "terraform import <resource address> "+id) {
		t.Errorf("no error names id %s to import; errors:\n%s", id, describe(errs))
	}
}

// A create that joins a service account from another workspace, with its
// shared name, is undone only when the rename is refused because the name is
// shared. When the rename fails for another reason, the account stays, and
// the error gives the id to import it with.
func TestServiceAccount_sharedJoinWhoseRenameFailsForAnotherReasonKeepsTheAccount(t *testing.T) {
	env := newTestEnv(t, "service_account")
	env.requireFake(t)
	email := randBotEmail("sharedfail")
	shared := env.fake.AddServiceAccountElsewhere("Shared Bot", email, true)
	id := fmt.Sprint(shared)
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					env.fake.RefuseNext(http.MethodPatch, "/api/v1/service-accounts/"+id, http.StatusBadGateway, "", "Bad Gateway")
				},
				Config: serviceAccountConfig(env, fmt.Sprintf(`
  email_address = %q
  name          = "Mine"`, email)),
				ExpectError: regexMust(`(?s)Error renaming Flightdeck service account.*added\s+service\s+account.*` +
					`with\s+the\s+name\s+it\s+already\s+had.*left\s+in\s+this\s+workspace`),
			},
		},
	})
	assertServiceAccountKept(t, env, id, "Shared Bot", 0)
	errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError)
	if !anyDiagnostic(errs, "terraform import <resource address> "+id) {
		t.Errorf("no error names id %s to import; errors:\n%s", id, describe(errs))
	}
}

// An address that is already a service account in this workspace can't be
// created again. The error names its id and the import command. The
// account that holds the address is left alone.
func TestServiceAccount_alreadyInTheWorkspaceNamesTheIDToImport(t *testing.T) {
	env := newTestEnv(t, "service_account")
	email := randBotEmail("twin")
	first := env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_service_account" "first" {
  email_address = %q
  name          = "First"
}
`, email)
	second := fmt.Sprintf(`
resource "flightdeck_service_account" "second" {
  email_address = %q
  name          = "Second"
  depends_on    = [flightdeck_service_account.first]
}
`, email)
	var id string
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: first,
				Check:  captureAttr("flightdeck_service_account.first", "id", &id),
			},
			{
				Config:      first + second,
				ExpectError: regexMust(`(?s)This service account is already in the workspace.*terraform\s+import\s+<resource\s+address>`),
			},
			{
				Config: first,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr("flightdeck_service_account.first", "id", &id),
					resource.TestCheckResourceAttr("flightdeck_service_account.first", "name", "First"),
				),
			},
		},
	})
	if !anyDiagnostic(recorded.bySeverity(tfprotov6.DiagnosticSeverityError),
		"(id "+id+", named \"First\")", "terraform import <resource address> "+id, "id = \""+id+"\"") {
		t.Errorf("no error names id %s to import; errors:\n%s", id, describe(recorded.bySeverity(tfprotov6.DiagnosticSeverityError)))
	}
}

// A service account removed from the workspace outside Terraform leaves
// state on the next refresh. Creating it again adds the same account back:
// Flightdeck never deletes the account itself.
func TestServiceAccount_removedOutsideTerraformIsAddedBack(t *testing.T) {
	env := newTestEnv(t, "service_account")
	email := randBotEmail("removed")
	config := serviceAccountConfig(env, fmt.Sprintf(`
  email_address = %q
  name          = "Removed Bot"`, email))
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  captureAttr(serviceAccountRes, "id", &id),
			},
			{
				PreConfig: func() {
					if err := webClient(t, env).DeleteServiceAccount(context.Background(), mustInt(id)); err != nil {
						t.Fatal(err)
					}
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check: func(s *terraform.State) error {
					if _, ok := s.RootModule().Resources[serviceAccountRes]; ok {
						return fmt.Errorf("%s is still in state", serviceAccountRes)
					}
					return nil
				},
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(serviceAccountRes, plancheck.ResourceActionCreate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(serviceAccountRes, "id", &id),
					resource.TestCheckResourceAttr(serviceAccountRes, "name", "Removed Bot"),
				),
			},
		},
	})
}

// Destroying a service account that something else removed a moment
// before succeeds: the DELETE's 404 means it is already gone.
func TestServiceAccount_destroyOfAnAccountAlreadyRemovedSucceeds(t *testing.T) {
	env := newTestEnv(t, "service_account")
	env.requireFake(t)
	email := randBotEmail("gone")
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: serviceAccountConfig(env, fmt.Sprintf(`  email_address = %q`, email)),
				Check:  captureAttr(serviceAccountRes, "id", &id),
			},
			{
				PreConfig: func() {
					env.fake.OnNextRequest(http.MethodDelete, "/api/v1/service-accounts/"+id, func() {
						env.fake.RemoveServiceAccountOutOfBand(mustInt(id))
					})
				},
				Config: env.providerConfig(),
				Check: func(*terraform.State) error {
					var statuses []int
					for _, r := range env.fake.RequestsMatching(http.MethodDelete, "/api/v1/service-accounts/"+id) {
						statuses = append(statuses, r.Status)
					}
					if len(statuses) != 1 || statuses[0] != http.StatusNotFound {
						return fmt.Errorf("DELETE statuses = %v, want one 404", statuses)
					}
					return nil
				},
			},
		},
	})
}

// Destroying a service account and declaring it again within the day the
// API remembers a create replays the first create, whose account has left
// the workspace. The provider creates it again under a fresh key, which adds
// the same account back.
func TestServiceAccount_recreatedWithinADayAddsTheSameAccountBack(t *testing.T) {
	env := newTestEnv(t, "service_account")
	email := randBotEmail("again")
	config := serviceAccountConfig(env, fmt.Sprintf(`
  email_address = %q
  name          = "Again"`, email))
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config, Check: captureAttr(serviceAccountRes, "id", &id)},
			{Config: env.providerConfig()},
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(serviceAccountRes, "id", &id),
					resource.TestCheckResourceAttr(serviceAccountRes, "name", "Again"),
				),
			},
		},
	})
	if env.live() {
		return
	}
	var keys []string
	for _, r := range env.fake.RequestsMatching(http.MethodPost, "/api/v1/service-accounts") {
		if r.Path == "/api/v1/service-accounts" {
			keys = append(keys, r.Header.Get("Idempotency-Key"))
		}
	}
	// The first create, its replay, and the fresh-key create.
	if len(keys) != 3 || keys[0] != keys[1] || keys[2] == keys[0] {
		t.Fatalf("create keys = %v", keys)
	}
}

// Every service-account route needs a workspace owner or admin, and the
// error says so.
func TestServiceAccount_needsAWorkspaceAdmin(t *testing.T) {
	env := newTestEnv(t, "service_account")
	env.requireFake(t)
	env.fake.SetWorkspaceAdmin(false)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: serviceAccountConfig(env, fmt.Sprintf(`  email_address = %q`, randBotEmail("denied"))),
				ExpectError: regexMust(`(?s)Error creating Flightdeck service account.*HTTP 403.*` +
					`only\s+be\s+read\s+or\s+changed\s+with\s+a\s+token\s+whose\s+user\s+is\s+a\s+workspace\s+owner\s+or\s+admin`),
			},
		},
	})
}

// The documented setup: a service account, a project it may work in, and a
// token for the application. The membership takes the account's id as its
// user, and reports the account's address.
func TestServiceAccount_withAProjectMembershipAndAToken(t *testing.T) {
	env := newTestEnv(t, "service_account")
	identifier := randIdentifier()
	email := randBotEmail("app")
	config := projectFixture(env, identifier) + fmt.Sprintf(`
resource "flightdeck_service_account" "deploy" {
  email_address = %q
  name          = "Deploy Bot"
}

resource "flightdeck_project_member" "deploy" {
  project_id = flightdeck_project.parent.id
  user_id    = flightdeck_service_account.deploy.id
  role       = "member"
}

resource "flightdeck_service_account_api_token" "deploy" {
  service_account_id = flightdeck_service_account.deploy.id
  name               = "Deploy production"
}
`, email)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("flightdeck_project_member.deploy", "user_id", "flightdeck_service_account.deploy", "id"),
					resource.TestCheckResourceAttr("flightdeck_project_member.deploy", "email", email),
					resource.TestCheckResourceAttrPair("flightdeck_service_account_api_token.deploy", "service_account_id",
						"flightdeck_service_account.deploy", "id"),
					resource.TestMatchResourceAttr("flightdeck_service_account_api_token.deploy", "token", regexMust(`^fd_pat_`)),
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
}
