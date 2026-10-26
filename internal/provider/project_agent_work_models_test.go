package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// None of these tests turns agent work on, so a model they set never runs a
// task, live or not.

// modelsConfig is the project with the given agent_work settings.
func modelsConfig(env *testEnv, identifier, settings string) string {
	return projectConfig(env, identifier, `
  name = "Models"
  agent_work = {
`+settings+`
  }`)
}

// modelsLookup reads the same project through the data source.
const modelsLookup = `
data "flightdeck_project" "lookup" {
  id = flightdeck_project.test.id
}
`

// modelsPath is agent_work.models in a plan or in state.
var modelsPath = tfjsonpath.New("agent_work").AtMapKey("models")

// modelsAre checks agent_work.models in state, as a whole: exactly these
// kinds, with these models. An empty want is {}, which is not null.
func modelsAre(address string, want map[string]string) statecheck.StateCheck {
	checks := map[string]knownvalue.Check{}
	for kind, model := range want {
		checks[kind] = knownvalue.StringExact(model)
	}
	return statecheck.ExpectKnownValue(address, modelsPath, knownvalue.MapExact(checks))
}

// modelsNull checks that agent_work.models is null in state, as it reads on a
// Flightdeck older than the setting. A null map and an empty one look alike
// to TestCheckNoResourceAttr, so this asks the state's own value.
func modelsNull(address string) statecheck.StateCheck {
	return statecheck.ExpectKnownValue(address, modelsPath, knownvalue.Null())
}

// sentModels is what one agent-work PATCH sent as models, and whether it sent
// the key at all.
func sentModels(t *testing.T, r flightdecktestRequest) (map[string]string, bool) {
	t.Helper()
	raw, ok := agentWorkBody(t, r)["models"]
	if !ok {
		return nil, false
	}
	obj, isObj := raw.(map[string]any)
	if !isObj {
		t.Fatalf("models was sent as %#v, not an object", raw)
	}
	out := map[string]string{}
	for kind, model := range obj {
		name, isName := model.(string)
		if !isName {
			t.Fatalf("models.%s was sent as %#v, not a model name", kind, model)
		}
		out[kind] = name
	}
	return out, true
}

// models is set for one kind, changed, given a second kind, has a kind
// dropped (which the server then shows back at the default, so the next plan
// is empty), and is cleared with {}. It is imported, and the data source
// reports it. Each write sends the whole map and nothing else. This one runs
// live too: it creates and destroys its own project.
func TestProjectAgentWork_modelsRoundTrip(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	identifier := randIdentifier()
	update := resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// One kind.
				Config:            modelsConfig(env, identifier, `    models = { "implement-work-item" = "opus-5-5" }`),
				ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, map[string]string{"implement-work-item": "opus-5-5"})},
				Check: resource.ComposeAggregateTestCheckFunc(
					// models alone does not turn agent work on, and leaves the
					// kinds alone.
					resource.TestCheckResourceAttr(projectRes, "agent_work.enabled", "false"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.kinds.#", "0"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "1"),
				),
			},
			{
				ResourceName:            projectRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"features"},
			},
			{
				// Changed.
				Config:            modelsConfig(env, identifier, `    models = { "implement-work-item" = "sonnet-5-5" }`) + modelsLookup,
				ConfigPlanChecks:  update,
				ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, map[string]string{"implement-work-item": "sonnet-5-5"})},
				Check:             resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "2"),
			},
			{
				// A second kind.
				Config: modelsConfig(env, identifier, `    models = {
      "implement-work-item" = "sonnet-5-5"
      "research"            = "opus-5-5"
    }`) + modelsLookup,
				ConfigPlanChecks: update,
				ConfigStateChecks: []statecheck.StateCheck{
					modelsAre(projectRes, map[string]string{"implement-work-item": "sonnet-5-5", "research": "opus-5-5"}),
				},
				Check: resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "3"),
			},
			{
				// A kind dropped: Flightdeck puts it back on the default, which
				// is leaving it out of the map.
				Config:            modelsConfig(env, identifier, `    models = { "research" = "opus-5-5" }`) + modelsLookup,
				ConfigPlanChecks:  update,
				ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, map[string]string{"research": "opus-5-5"})},
				Check:             resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "4"),
			},
			{
				// So the next plan is empty, and the data source reads the same.
				Config: modelsConfig(env, identifier, `    models = { "research" = "opus-5-5" }`) + modelsLookup,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				ConfigStateChecks: []statecheck.StateCheck{
					modelsAre(projectRes, map[string]string{"research": "opus-5-5"}),
					modelsAre("data.flightdeck_project.lookup", map[string]string{"research": "opus-5-5"}),
				},
			},
			{
				// {} clears them all.
				Config:            modelsConfig(env, identifier, `    models = {}`),
				ConfigPlanChecks:  update,
				ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, map[string]string{})},
				Check:             resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "5"),
			},
			{Config: modelsConfig(env, identifier, `    models = {}`), PlanOnly: true},
		},
	})
	if env.live() {
		return
	}
	writes := agentWorkRequests(env.fake.RequestsMatching("PATCH", "/api/v1/projects/"))
	want := []map[string]string{
		{"implement-work-item": "opus-5-5"},
		{"implement-work-item": "sonnet-5-5"},
		{"implement-work-item": "sonnet-5-5", "research": "opus-5-5"},
		{"research": "opus-5-5"},
		{},
	}
	if len(writes) != len(want) {
		t.Fatalf("expected %d agent-work PATCHes, got %d", len(want), len(writes))
	}
	for i, r := range writes {
		got, sent := sentModels(t, r)
		if body := agentWorkBody(t, r); !sent || len(body) != 1 || fmt.Sprint(got) != fmt.Sprint(want[i]) {
			t.Errorf("write %d must send models = %v only, got %v", i+1, want[i], body)
		}
	}
}

// Unset, models is never sent: state reads whatever the server holds,
// including models chosen on the settings page, an import reads them too, and
// nothing is planned for them. Once the configuration has owned models,
// leaving them out again keeps what is stored.
func TestProjectAgentWork_modelsUnsetIsNeverSent(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	page := map[string]string{"research": "opus-5-5", "fix-error": "sonnet-5-5"}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: modelsConfig(env, identifier, `    max_in_progress = 2`),
				Check:  captureAttr(projectRes, "id", &id),
				// The server's default: no kind has a model.
				ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, map[string]string{})},
			},
			{
				// Chosen on the settings page: adopted, nothing planned.
				PreConfig: func() {
					env.fake.SetAgentWorkOutOfBand(mustInt64(t, id), func(row *flightdecktest.AgentWorkSetting) {
						row.Models = map[string]string{"research": "opus-5-5", "fix-error": "sonnet-5-5"}
					})
				},
				Config: modelsConfig(env, identifier, `    max_in_progress = 2`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, page)},
			},
			{
				// An import reads them.
				ResourceName:            projectRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"features"},
			},
			{
				// Another setting changes; the models stay as the page left them.
				Config:            modelsConfig(env, identifier, `    max_in_progress = 3`),
				ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, page)},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "3"),
					func(*terraform.State) error {
						if row := env.fake.AgentWorkOf(mustInt64(t, id)); row == nil || fmt.Sprint(row.Models) != fmt.Sprint(page) {
							return fmt.Errorf("the write changed the stored models: %+v", row)
						}
						return nil
					},
				),
			},
			{
				// Owned for a while...
				Config:            modelsConfig(env, identifier, "    max_in_progress = 3\n    models = { \"research\" = \"sonnet-5-5\" }"),
				ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, map[string]string{"research": "sonnet-5-5"})},
			},
			{
				// ...then left out again: what is stored stays, and nothing is
				// planned.
				Config: modelsConfig(env, identifier, `    max_in_progress = 3`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, map[string]string{"research": "sonnet-5-5"})},
			},
		},
	})
	writes := agentWorkRequests(env.fake.RequestsMatching("PATCH", "/api/v1/projects/"))
	if len(writes) != 3 {
		t.Fatalf("expected three agent-work PATCHes, got %d", len(writes))
	}
	for i, r := range writes[:2] {
		if body := agentWorkBody(t, r); len(body) != 1 || body["max_in_progress"] == nil {
			t.Errorf("write %d must send max_in_progress only, never models: %v", i+1, body)
		}
	}
}

// Configured, models changed on the settings page show in the next plan and
// are set back, whichever way they changed: a model switched, a kind added,
// or the map cleared.
func TestProjectAgentWork_modelsDrift(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	config := modelsConfig(env, identifier, `    models = { "research" = "opus-5-5" }`)
	want := []statecheck.StateCheck{modelsAre(projectRes, map[string]string{"research": "opus-5-5"})}
	outOfBand := func(models map[string]string) func() {
		return func() {
			env.fake.SetAgentWorkOutOfBand(mustInt64(t, id), func(row *flightdecktest.AgentWorkSetting) { row.Models = models })
		}
	}
	update := resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config, Check: captureAttr(projectRes, "id", &id)},
			{PreConfig: outOfBand(map[string]string{"research": "sonnet-5-5"}), Config: config, ConfigPlanChecks: update, ConfigStateChecks: want},
			{
				PreConfig: outOfBand(map[string]string{"research": "opus-5-5", "implement-work-item": "sonnet-5-5"}),
				Config:    config, ConfigPlanChecks: update, ConfigStateChecks: want,
			},
			{PreConfig: outOfBand(map[string]string{}), Config: config, ConfigPlanChecks: update, ConfigStateChecks: want},
			{Config: config, PlanOnly: true},
		},
	})
}

// The plan checks models' keys against the kinds of work, and fails on a
// null or blank model, before anything is created. It never checks a model's
// name: one the provider has never heard of plans fine, and it is
// Flightdeck's to accept or refuse.
func TestProjectAgentWork_modelsKeysAreCheckedAtPlanTime(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: modelsConfig(env, identifier, `    models = { "fix-ci" = "opus-5-5" }`),
				ExpectError: regexMust(`(?s)Unknown kind of work in agent_work\.models.*"fix-ci" is not a kind of\s+work.*` +
					`implement-work-item,\s+fix-error,\s+research`),
			},
			{
				// A kind spelled another way is not a kind either.
				Config:      modelsConfig(env, identifier, `    models = { "Research" = "opus-5-5" }`),
				ExpectError: regexMust(`"Research" is not a kind of\s+work`),
			},
			{
				Config:      modelsConfig(env, identifier, `    models = { "research" = "" }`),
				ExpectError: regexMust(`(?s)No model named for research.*leave it\s+out\s+of\s+models`),
			},
			{
				Config:      modelsConfig(env, identifier, `    models = { "research" = "  " }`),
				ExpectError: regexMust(`No model named for research`),
			},
			{
				Config:      modelsConfig(env, identifier, `    models = { "fix-error" = null }`),
				ExpectError: regexMust(`No model named for fix-error`),
			},
			{
				// A model nobody has heard of yet is the server's to judge.
				Config:             modelsConfig(env, identifier, `    models = { "research" = "sonnet-9-9" }`),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
	if env.live() {
		return
	}
	if creates := env.fake.RequestsMatching("POST", "/api/v1/projects"); len(creates) != 0 {
		t.Fatalf("a plan that fails must create nothing, saw %d project creates", len(creates))
	}
}

// Flightdeck's refusal of a model it doesn't offer reaches the user in its own
// words, on agent_work.models, and saves nothing (not even the block's other
// changes). Fixing the configuration then applies.
func TestProjectAgentWork_modelsUnknownModelRefusalSurfaces(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	bad := modelsConfig(env, identifier, "    max_in_progress = 3\n    models = { \"research\" = \"opus-9-9\" }")
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: modelsConfig(env, identifier, "    max_in_progress = 2\n    models = { \"research\" = \"opus-5-5\" }"),
				Check:  captureAttr(projectRes, "id", &id),
			},
			{
				Config: bad,
				ExpectError: regexMust(`(?s)Flightdeck refused agent_work\.models.*Nothing was saved.*` +
					`models\.research\s+must\s+be\s+one\s+of:\s+sonnet-5-5,\s+opus-5-5,\s+got\s+"opus-9-9"`),
			},
			{
				PreConfig: func() {
					if row := env.fake.AgentWorkOf(mustInt64(t, id)); row == nil || row.Models["research"] != "opus-5-5" || row.MaxInProgress != 2 {
						t.Errorf("a refused write must save nothing, but the settings are %+v", row)
					}
				},
				Config:             bad,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config:            modelsConfig(env, identifier, "    max_in_progress = 3\n    models = { \"research\" = \"sonnet-5-5\" }"),
				ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, map[string]string{"research": "sonnet-5-5"})},
				Check:             resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "3"),
			},
		},
	})
	errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError)
	at := tftypes.NewAttributePath().WithAttributeName("agent_work").WithAttributeName("models")
	if !diagnosticAt(errs, "Flightdeck refused agent_work.models", at,
		`The API said: models.research must be one of: sonnet-5-5, opus-5-5, got "opus-9-9"`) {
		t.Fatalf("the refusal is not reported on agent_work.models with the API's message; apply errors: %s", describeWithPaths(errs))
	}
	for _, e := range errs {
		if strings.Contains(e.Detail, "older than the release") {
			t.Errorf("a Flightdeck that knows models must not be told to upgrade: %s", e.Detail)
		}
	}
}

// On the apply that creates a project, Flightdeck's refusal of a model is a
// warning on agent_work.models, carrying its words, so the new project isn't
// tainted. The next apply is an update of the same project.
func TestProjectAgentWork_modelsUnknownModelAtCreateWarns(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:             modelsConfig(env, identifier, `    models = { "implement-work-item" = "opus-9-9" }`),
				ExpectNonEmptyPlan: true,
				Check:              captureAttr(projectRes, "id", &id),
			},
			{
				Config: modelsConfig(env, identifier, `    models = { "implement-work-item" = "opus-5-5" }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, map[string]string{"implement-work-item": "opus-5-5"})},
				Check:             resource.TestCheckResourceAttrPtr(projectRes, "id", &id),
			},
		},
	})
	if errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError); len(errs) != 0 {
		t.Errorf("no apply may fail, got %d error(s): %s", len(errs), describeWithPaths(errs))
	}
	warnings := recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning)
	at := tftypes.NewAttributePath().WithAttributeName("agent_work").WithAttributeName("models")
	if !diagnosticAt(warnings, "Agent work settings were not saved", at, "The project was created",
		`models.implement-work-item must be one of: sonnet-5-5, opus-5-5, got "opus-9-9"`) {
		t.Fatalf("no create-time warning on agent_work.models; apply warnings: %s", describeWithPaths(warnings))
	}
}

// A model Flightdeck has taken off its list stays stored for the kind it was
// set for. The read shows it, the next plan is empty, and an apply that
// changes something else sends it back unchanged, which Flightdeck accepts.
// Setting it for another kind is refused, in Flightdeck's words, naming only
// the models it still offers.
func TestProjectAgentWork_modelsRetiredModelRoundTrips(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	stored := map[string]string{"implement-work-item": "opus-5-5", "research": "sonnet-5-5"}
	config := func(n int, extra string) string {
		return modelsConfig(env, identifier, fmt.Sprintf(`    max_in_progress = %d
    models = {
      "implement-work-item" = "opus-5-5"
      "research"            = "sonnet-5-5"
%s    }`, n, extra))
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config(2, ""), ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, stored)}},
			{
				// opus-5-5 comes off the list. Nothing changes, so nothing is
				// planned.
				PreConfig: func() { env.fake.SetAgentWorkModelsOffered("sonnet-5-5") },
				Config:    config(2, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, stored)},
			},
			{
				// Another setting changes. The write sends the whole map,
				// opus-5-5 included, and Flightdeck takes it.
				Config:            config(3, ""),
				ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, stored)},
				Check:             resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "3"),
			},
			{Config: config(3, ""), PlanOnly: true},
			{
				// The same model for another kind is a change, and refused.
				Config: config(3, "      \"fix-error\"           = \"opus-5-5\"\n"),
				ExpectError: regexMust(`(?s)Flightdeck refused agent_work\.models.*` +
					`models\.fix-error\s+must\s+be\s+one\s+of:\s+sonnet-5-5,\s+got\s+"opus-5-5"`),
			},
		},
	})
	writes := agentWorkRequests(env.fake.RequestsMatching("PATCH", "/api/v1/projects/"))
	if len(writes) != 3 {
		t.Fatalf("expected three agent-work PATCHes, got %d", len(writes))
	}
	if got, sent := sentModels(t, writes[1]); !sent || fmt.Sprint(got) != fmt.Sprint(stored) {
		t.Errorf("the second write must send the stored models back, the retired one included, got %v", agentWorkBody(t, writes[1]))
	}
}

// A Flightdeck older than models (but with the other four newer settings,
// the release before it): a project whose configuration leaves models unset
// works as before (it reads null and is never sent). Setting it fails on the
// attribute, saying the server is too old, saves nothing, and applies once
// Flightdeck is upgraded.
func TestProjectAgentWork_modelsOnAnOlderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkModelsSupported(false)
	identifier := randIdentifier()
	var id string
	withModels := modelsConfig(env, identifier, "    max_in_progress = 3\n    send_errors     = true\n    models          = { \"research\" = \"opus-5-5\" }")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: modelsConfig(env, identifier, `    max_in_progress = 2`) + modelsLookup,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "2"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "false"),
				),
				ConfigStateChecks: []statecheck.StateCheck{modelsNull(projectRes), modelsNull("data.flightdeck_project.lookup")},
			},
			{
				// The error names models, not send_errors, which this
				// Flightdeck knows.
				Config: withModels,
				ExpectError: regexMust(`(?s)This Flightdeck does not support agent_work\.models yet.*` +
					`older\s+than\s+the\s+release\s+that\s+added\s+the\s+choice\s+of\s+AI\s+model\s+for\s+each\s+kind\s+of\s+work.*` +
					`Upgrade\s+Flightdeck\s+first.*unknown key: models`),
			},
			{
				// Nothing was saved, max_in_progress and send_errors included.
				PreConfig: func() {
					if row := env.fake.AgentWorkOf(mustInt64(t, id)); row == nil || row.MaxInProgress != 2 || row.SendErrors || len(row.Models) != 0 {
						t.Errorf("a refused write must save nothing, but the settings are %+v", row)
					}
				},
				Config:             withModels,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Flightdeck is upgraded; the same configuration applies.
				PreConfig:         func() { env.fake.SetAgentWorkModelsSupported(true) },
				Config:            withModels,
				ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, map[string]string{"research": "opus-5-5"})},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "true"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "3"),
				),
			},
			{Config: withModels, PlanOnly: true},
		},
	})
	writes := agentWorkRequests(env.fake.RequestsMatching("PATCH", "/api/v1/projects/"))
	if len(writes) == 0 {
		t.Fatal("no agent-work PATCH was recorded")
	}
	if _, sent := agentWorkBody(t, writes[0])["models"]; sent {
		t.Errorf("an unset models must not be sent to an older Flightdeck: %v", agentWorkBody(t, writes[0]))
	}
}

// On the apply that creates a project, an older Flightdeck's refusal of models
// is a warning on the attribute: an error would taint the new project. The
// next apply is an update of the same project.
func TestProjectAgentWork_modelsOnAnOlderFlightdeckAtCreateWarns(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkModelsSupported(false)
	identifier := randIdentifier()
	config := modelsConfig(env, identifier, "    max_in_progress = 2\n    models          = { \"fix-error\" = \"opus-5-5\" }")
	var id string
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:             config,
				ExpectNonEmptyPlan: true,
				Check:              captureAttr(projectRes, "id", &id),
			},
			{
				PreConfig: func() {
					if row := env.fake.AgentWorkOf(mustInt64(t, id)); row != nil {
						t.Errorf("a refused write must save nothing, but the settings were stored: %+v", row)
					}
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// An update, not a replace: the create did not taint the project.
				PreConfig: func() { env.fake.SetAgentWorkModelsSupported(true) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, map[string]string{"fix-error": "opus-5-5"})},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "2"),
				),
			},
			{Config: config, PlanOnly: true},
		},
	})
	if errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError); len(errs) != 0 {
		t.Errorf("no apply may fail, got %d error(s): %s", len(errs), errs[0].Summary)
	}
	warnings := recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning)
	at := tftypes.NewAttributePath().WithAttributeName("agent_work").WithAttributeName("models")
	if !diagnosticAt(warnings, "This Flightdeck does not support agent_work.models yet", at,
		"The project was created", "unknown key: models") {
		t.Fatalf("no create-time warning on agent_work.models; apply warnings: %s", describeWithPaths(warnings))
	}
	for _, w := range warnings {
		for _, key := range []string{"auto_merge", "send_ci_failures", "send_errors", "wait_for_checks"} {
			if strings.Contains(w.Summary, key) {
				t.Errorf("only models was sent, so nothing may warn about %s: %s", key, w.Summary)
			}
		}
	}
}

// A project imported from a Flightdeck that has send_errors but not models
// reads models as null and matches the state the apply left. A later change
// elsewhere plans it as that null, not "known after apply", and keeps the
// settings' lock_version known too.
func TestProjectAgentWork_modelsImportedFromAnOlderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkWithSendErrors)
	identifier := randIdentifier()
	config := func(name string) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  agent_work = {
    send_errors = true
  }`, name))
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config("Imported"), ConfigStateChecks: []statecheck.StateCheck{modelsNull(projectRes)}},
			{
				ResourceName:            projectRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"features"},
			},
			{
				Config: config("Imported renamed"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(projectRes, modelsPath, knownvalue.Null()),
						plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("agent_work").AtMapKey("send_errors"), knownvalue.Bool(true)),
						plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("agent_work").AtMapKey("lock_version"), knownvalue.Int64Exact(1)),
					},
				},
			},
		},
	})
}

// A change elsewhere in the project plans an unset models as the value in
// state, not "known after apply", and so leaves the settings' lock_version
// known too. On an older Flightdeck that value is null.
func TestProjectAgentWork_modelsUnsetStaysKnownOnARename(t *testing.T) {
	for _, supported := range []bool{true, false} {
		t.Run(fmt.Sprintf("Flightdeck has models: %t", supported), func(t *testing.T) {
			env := newTestEnv(t, "agent_work")
			env.requireFake(t)
			env.fake.SetAgentWorkModelsSupported(supported)
			identifier := randIdentifier()
			config := func(name string) string {
				return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  agent_work = {
    max_in_progress = 2
  }`, name))
			}
			var want knownvalue.Check = knownvalue.Null()
			if supported {
				want = knownvalue.MapExact(map[string]knownvalue.Check{})
			}
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{Config: config("Known")},
					{
						Config: config("Known renamed"),
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{
								plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate),
								plancheck.ExpectKnownValue(projectRes, modelsPath, want),
								plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("agent_work").AtMapKey("lock_version"), knownvalue.Int64Exact(1)),
							},
						},
					},
				},
			})
		})
	}
}

// Flightdeck is upgraded to models, or rolled back below it, between the plan
// and the apply, as a saved plan applied later can see. An unset models is
// never sent, so the apply keeps the value the plan promised rather than fail
// with "Provider produced inconsistent result", and the next refresh adopts
// the server's value without planning anything.
func TestProjectAgentWork_modelsUnsetSurvivesAServerChangeDuringApply(t *testing.T) {
	cases := []struct {
		name string
		// before is whether Flightdeck has models at plan time.
		before bool
		// block is whether the configuration has an agent_work block (one
		// that leaves models unset) or none at all.
		block bool
	}{
		{name: "upgraded, block without models", before: false, block: true},
		{name: "rolled back, block without models", before: true, block: true},
		{name: "upgraded, no block", before: false, block: false},
		{name: "rolled back, no block", before: true, block: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, "agent_work")
			env.requireFake(t)
			env.fake.SetAgentWorkModelsSupported(tc.before)
			identifier := randIdentifier()
			var id string
			config := func(name string, n int) string {
				body := fmt.Sprintf("  name = %q", name)
				if tc.block {
					body += fmt.Sprintf("\n  agent_work = {\n    max_in_progress = %d\n  }", n)
				}
				return projectConfig(env, identifier, body)
			}
			// The value state holds before the change, and the one Flightdeck
			// reports after it.
			check := func(supported bool) []statecheck.StateCheck {
				if supported {
					return []statecheck.StateCheck{modelsAre(projectRes, map[string]string{})}
				}
				return []statecheck.StateCheck{modelsNull(projectRes)}
			}
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{Config: config("Server change", 2), Check: captureAttr(projectRes, "id", &id), ConfigStateChecks: check(tc.before)},
					{
						// The change lands after the plan, just before the write
						// that touches the project: its agent-work PATCH, or with
						// no block, the project PATCH ahead of the agent-work read.
						PreConfig: func() {
							route := fmt.Sprintf("/api/v1/projects/%s", id)
							if tc.block {
								route += "/agent-work"
							}
							env.fake.OnNextRequest("PATCH", route, func() { env.fake.SetAgentWorkModelsSupported(!tc.before) })
						},
						Config: config("Server change renamed", 3),
						// The plan's value, kept.
						ConfigStateChecks: check(tc.before),
					},
					{
						// The refresh adopts what Flightdeck now reports, and
						// plans nothing for it.
						Config: config("Server change renamed", 3),
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
						},
						ConfigStateChecks: check(!tc.before),
					},
				},
			})
		})
	}
}

// A configured models gets no such allowance: a Flightdeck rolled back below
// the setting between the plan and the apply refuses the write, and the apply
// fails saying so.
func TestProjectAgentWork_modelsConfiguredMeetsARollbackDuringApply(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: modelsConfig(env, identifier, `    models = {}`), Check: captureAttr(projectRes, "id", &id)},
			{
				PreConfig: func() {
					env.fake.OnNextRequest("PATCH", fmt.Sprintf("/api/v1/projects/%s/agent-work", id), func() {
						env.fake.SetAgentWorkModelsSupported(false)
					})
				},
				Config:      modelsConfig(env, identifier, `    models = { "research" = "opus-5-5" }`),
				ExpectError: regexMust(`This Flightdeck does not support agent_work\.models yet`),
			},
		},
	})
}

// Creating a project that sets all five newer settings on a Flightdeck older
// than all of them gives five warnings, one on each attribute, and no error,
// so the new project isn't tainted. Once Flightdeck is upgraded to the newest
// release, the next apply is an update of the same project.
func TestProjectAgentWork_everyNewerSettingWithModelsAtCreateOnAFlightdeckOlderThanAll(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkBeforeAutoMerge)
	identifier := randIdentifier()
	config := modelsConfig(env, identifier, "    max_in_progress  = 2\n    models           = { \"research\" = \"opus-5-5\" }\n"+
		"    auto_merge       = true\n    send_ci_failures = true\n    send_errors      = true\n    wait_for_checks  = false")
	keys := []string{"models", "auto_merge", "send_ci_failures", "send_errors", "wait_for_checks"}
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config, ExpectNonEmptyPlan: true},
			{
				PreConfig: func() { env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkWithModels) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, map[string]string{"research": "opus-5-5"})},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "true"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "true"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "true"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "false"),
				),
			},
			{Config: config, PlanOnly: true},
		},
	})
	if errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError); len(errs) != 0 {
		t.Errorf("no apply may fail, got %d error(s): %s", len(errs), describeWithPaths(errs))
	}
	warnings := recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning)
	for _, key := range keys {
		at := tftypes.NewAttributePath().WithAttributeName("agent_work").WithAttributeName(key)
		if !diagnosticAt(warnings, "This Flightdeck does not support agent_work."+key+" yet", at, "The project was created") {
			t.Errorf("no create-time warning on agent_work.%s: %s", key, describeWithPaths(warnings))
		}
	}
	unsupported := 0
	for _, w := range warnings {
		if strings.HasPrefix(w.Summary, "This Flightdeck does not support") {
			unsupported++
		}
	}
	if unsupported != len(keys) {
		t.Errorf("want %d warnings about unsupported settings, got %d: %s", len(keys), unsupported, describeWithPaths(warnings))
	}
}

// Flightdeck is upgraded from below auto_merge to the newest release, or
// rolled back from the newest to below auto_merge, between the plan and the
// apply: all five newer settings change at once. Left unset, each keeps the
// value the plan promised, and the next refresh adopts the server's for all
// of them without planning anything.
func TestProjectAgentWork_everyNewerSettingWithModelsUnsetSurvivesAServerChangeDuringApply(t *testing.T) {
	release := func(all bool) flightdecktest.AgentWorkRelease {
		if all {
			return flightdecktest.AgentWorkWithModels
		}
		return flightdecktest.AgentWorkBeforeAutoMerge
	}
	for _, before := range []bool{false, true} {
		t.Run(fmt.Sprintf("Flightdeck has them at plan time: %t", before), func(t *testing.T) {
			env := newTestEnv(t, "agent_work")
			env.requireFake(t)
			env.fake.SetAgentWorkRelease(release(before))
			identifier := randIdentifier()
			var id string
			config := func(name string, n int) string {
				return projectConfig(env, identifier, fmt.Sprintf("  name = %q\n  agent_work = {\n    max_in_progress = %d\n  }", name, n))
			}
			check := func(all bool) ([]statecheck.StateCheck, resource.TestCheckFunc) {
				if all {
					return []statecheck.StateCheck{modelsAre(projectRes, map[string]string{})}, resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(projectRes, "agent_work.auto_merge", "false"),
						resource.TestCheckResourceAttr(projectRes, "agent_work.send_ci_failures", "false"),
						resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "false"),
						resource.TestCheckResourceAttr(projectRes, "agent_work.wait_for_checks", "true"),
					)
				}
				return []statecheck.StateCheck{modelsNull(projectRes)}, resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.auto_merge"),
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.send_ci_failures"),
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.send_errors"),
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.wait_for_checks"),
				)
			}
			beforeState, beforeCheck := check(before)
			afterState, afterCheck := check(!before)
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{
						Config:            config("Every change", 2),
						ConfigStateChecks: beforeState,
						Check:             resource.ComposeAggregateTestCheckFunc(captureAttr(projectRes, "id", &id), beforeCheck),
					},
					{
						PreConfig: func() {
							env.fake.OnNextRequest("PATCH", fmt.Sprintf("/api/v1/projects/%s/agent-work", id), func() {
								env.fake.SetAgentWorkRelease(release(!before))
							})
						},
						Config: config("Every change renamed", 3),
						// The plan's values, kept.
						ConfigStateChecks: beforeState,
						Check:             beforeCheck,
					},
					{
						Config: config("Every change renamed", 3),
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
						},
						ConfigStateChecks: afterState,
						Check:             afterCheck,
					},
				},
			})
		})
	}
}

// How writeAgentWork reports a refused update that sent models. The read
// after the refusal, not the API's wording, decides whether the server is too
// old for it, and each setting it is too old for is reported on its own
// attribute, in the API's order, which puts models first although it came
// last. When the read fails, the refusal is reported as it came, with a note
// for each newer setting it names as unknown. A Flightdeck that knows models
// and refuses one of them has its words put on agent_work.models, with no
// upgrade hint; a refusal of anything else stays on the block.
func TestWriteAgentWork_modelsRefusals(t *testing.T) {
	modelsAt := path.Root("agent_work").AtName("models")
	blockPath := path.Root("agent_work")
	newProject := func(t *testing.T, env *testEnv) (*client.Client, int64) {
		t.Helper()
		c, err := client.New(env.fake.URL, env.fake.Token())
		if err != nil {
			t.Fatal(err)
		}
		fields := client.Fields{"name": "Models refusal", "identifier": randIdentifier()}
		p, err := c.CreateProject(t.Context(), fields, client.PayloadKey("project", "", fields))
		if err != nil {
			t.Fatal(err)
		}
		return c, p.ID
	}
	// readFailsAfterRefusal makes the read that follows the next agent-work
	// PATCH fail, as if the endpoint went away in between.
	readFailsAfterRefusal := func(env *testEnv, id int64) {
		route := fmt.Sprintf("/api/v1/projects/%d/agent-work", id)
		env.fake.OnNextRequest("PATCH", route, func() {
			env.fake.OnNextRequest("GET", route, func() { env.fake.SetAgentWorkEndpoint(false) })
		})
	}
	at := func(d diag.Diagnostic) path.Path {
		if withPath, ok := d.(diag.DiagnosticWithPath); ok {
			return withPath.Path()
		}
		return path.Empty()
	}
	models := func(m map[string]string) types.Map {
		values := map[string]attr.Value{}
		for kind, model := range m {
			values[kind] = types.StringValue(model)
		}
		return types.MapValueMust(types.StringType, values)
	}
	write := func(t *testing.T, c *client.Client, projectID int64, m agentWorkModel) diag.Diagnostics {
		t.Helper()
		var diags diag.Diagnostics
		block := agentWorkObj(t, &m)
		writeAgentWork(t.Context(), c, projectID, "MOD", block, types.ObjectNull(agentWorkAttrTypes),
			types.ObjectUnknown(agentWorkAttrTypes), agentWorkOnUpdate, &diags)
		return diags
	}
	wantUnsupported := func(t *testing.T, d diag.Diagnostic, key, added string) {
		t.Helper()
		if got := d.Summary(); got != "This Flightdeck does not support agent_work."+key+" yet" {
			t.Errorf("summary = %q", got)
		}
		if got, want := at(d), path.Root("agent_work").AtName(key); !got.Equal(want) {
			t.Errorf("reported at %s, want %s", got, want)
		}
		for _, want := range []string{"older than the release that added " + added, "Upgrade Flightdeck first",
			"leave `" + key + "` out", "unknown key"} {
			if !strings.Contains(d.Detail(), want) {
				t.Errorf("detail does not carry %q:\n%s", want, d.Detail())
			}
		}
	}

	// A Flightdeck with the other four newer settings refuses the write for
	// models alone; send_errors is not blamed.
	t.Run("older Flightdeck, sent with send_errors", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkModelsSupported(false)
		c, id := newProject(t, env)
		diags := write(t, c, id, agentWorkModel{Models: models(map[string]string{"research": "opus-5-5"}), SendErrors: types.BoolValue(true)})
		errs := diags.Errors()
		if len(errs) != 1 {
			t.Fatalf("expected exactly one error, got %v", diags)
		}
		wantUnsupported(t, errs[0], "models", "the choice of AI model for each kind of work")
		if !strings.Contains(errs[0].Detail(), "unknown key: models") {
			t.Errorf("detail does not carry the API's refusal:\n%s", errs[0].Detail())
		}
		if row := env.fake.AgentWorkOf(id); row != nil {
			t.Errorf("a refused write must save nothing, send_errors included, got %+v", row)
		}
	})

	// A Flightdeck older than all five refuses all five, each on its own
	// attribute, in the API's order.
	t.Run("Flightdeck older than all five, all sent", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkRelease(flightdecktest.AgentWorkBeforeAutoMerge)
		c, id := newProject(t, env)
		diags := write(t, c, id, agentWorkModel{Models: models(map[string]string{}), AutoMerge: types.BoolValue(false),
			SendCIFailures: types.BoolValue(false), SendErrors: types.BoolValue(false), WaitForChecks: types.BoolValue(true)})
		errs := diags.Errors()
		if len(errs) != 5 {
			t.Fatalf("expected exactly five errors, got %v", diags)
		}
		wantUnsupported(t, errs[0], "models", "the choice of AI model for each kind of work")
		wantUnsupported(t, errs[1], "auto_merge", "auto-merge for agent pull requests")
		wantUnsupported(t, errs[2], "send_ci_failures", "sending CI failures to agents")
		wantUnsupported(t, errs[3], "send_errors", "sending errors to agents")
		wantUnsupported(t, errs[4], "wait_for_checks", "the choice of whether agents wait for CI")
		for _, e := range errs {
			if !strings.Contains(e.Detail(), "unknown keys: auto_merge, models, send_ci_failures, send_errors, wait_for_checks") {
				t.Errorf("detail does not carry the API's refusal:\n%s", e.Detail())
			}
		}
	})

	t.Run("older Flightdeck, the read fails", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		env.fake.SetAgentWorkModelsSupported(false)
		c, id := newProject(t, env)
		readFailsAfterRefusal(env, id)
		diags := write(t, c, id, agentWorkModel{Models: models(map[string]string{"research": "opus-5-5"})})
		errs := diags.Errors()
		if len(errs) != 1 {
			t.Fatalf("expected exactly one error, got %v", diags)
		}
		if got := errs[0].Summary(); got != "Flightdeck refused the agent work settings" {
			t.Errorf("summary = %q", got)
		}
		if got := at(errs[0]); !got.Equal(blockPath) {
			t.Errorf("reported at %s, want %s", got, blockPath)
		}
		detail := errs[0].Detail()
		for _, want := range []string{"unknown key: models", agentWorkModelsUnknownNote} {
			if !strings.Contains(detail, want) {
				t.Errorf("detail does not carry %q:\n%s", want, detail)
			}
		}
		// The other four are in the settable list, not refused.
		for _, note := range []string{agentWorkAutoMergeUnknownNote, agentWorkSendCIFailuresUnknownNote, agentWorkSendErrorsUnknownNote,
			agentWorkWaitForChecksUnknownNote} {
			if strings.Contains(detail, note) {
				t.Errorf("a refusal that lists a setting as settable must not carry its note:\n%s", detail)
			}
		}
	})

	for _, tc := range []struct {
		name   string
		models map[string]string
		words  string
	}{
		{"a model it doesn't offer", map[string]string{"research": "opus-9-9"},
			`models.research must be one of: sonnet-5-5, opus-5-5, got "opus-9-9"`},
		{"a kind it doesn't know", map[string]string{"fix-ci": "opus-5-5"},
			`models has an unknown kind: "fix-ci" (valid: implement-work-item, fix-error, research)`},
	} {
		t.Run("a Flightdeck with models refuses "+tc.name, func(t *testing.T) {
			env := newTestEnv(t, "agent_work")
			env.requireFake(t)
			c, id := newProject(t, env)
			diags := write(t, c, id, agentWorkModel{Models: models(tc.models), MaxInProgress: types.Int64Value(3)})
			errs := diags.Errors()
			if len(errs) != 1 {
				t.Fatalf("expected exactly one error, got %v", diags)
			}
			if got := errs[0].Summary(); got != "Flightdeck refused agent_work.models" {
				t.Errorf("summary = %q", got)
			}
			if got := at(errs[0]); !got.Equal(modelsAt) {
				t.Errorf("reported at %s, want %s", got, modelsAt)
			}
			detail := errs[0].Detail()
			for _, want := range []string{"Nothing was saved.", "The API said: " + tc.words} {
				if !strings.Contains(detail, want) {
					t.Errorf("detail does not carry %q:\n%s", want, detail)
				}
			}
			if strings.Contains(detail, "older than the release") || strings.Contains(strings.ToLower(detail), "upgrade") {
				t.Errorf("a Flightdeck that knows models must not be told to upgrade:\n%s", detail)
			}
			if row := env.fake.AgentWorkOf(id); row != nil {
				t.Errorf("a refused write must save nothing, got %+v", row)
			}
		})
	}

	t.Run("a Flightdeck with models refuses something else", func(t *testing.T) {
		env := newTestEnv(t, "agent_work")
		env.requireFake(t)
		c, id := newProject(t, env)
		// More than the stored daily budget (10), which Flightdeck refuses.
		diags := write(t, c, id, agentWorkModel{Models: models(map[string]string{"research": "opus-5-5"}), TaskMaxUSD: types.Float64Value(20)})
		errs := diags.Errors()
		if len(errs) != 1 {
			t.Fatalf("expected exactly one error, got %v", diags)
		}
		if got := errs[0].Summary(); got != "Flightdeck refused the agent work settings" {
			t.Errorf("summary = %q", got)
		}
		if got := at(errs[0]); got.Equal(modelsAt) {
			t.Errorf("a refusal of another setting must not land on models")
		}
		if !strings.Contains(errs[0].Detail(), "can't be more than the daily budget") {
			t.Errorf("detail does not carry the API's refusal:\n%s", errs[0].Detail())
		}
	})
}

// agentWorkModelsRefused reads Flightdeck's refusal of a models it was sent:
// only a 422 invalid_attribute whose message starts with the key, to a write
// that sent models. An older Flightdeck's refusal of the key is an unknown-key
// refusal, which the newer settings explain, and a refusal of anything else is
// not about models even when the write sent them.
func TestAgentWorkModelsRefused(t *testing.T) {
	refusal := func(code, msg string) error {
		return &client.Error{Method: "PATCH", Path: "/projects/4/agent-work", Status: 422, Code: code, Message: msg}
	}
	sent := client.Fields{"models": map[string]string{"research": "opus-9-9"}}
	cases := []struct {
		name string
		sent client.Fields
		err  error
		want bool
	}{
		{"a model not offered", sent, refusal(client.CodeInvalidAttribute,
			`models.research must be one of: sonnet-5-5, opus-5-5, got "opus-9-9"`), true},
		{"an unknown kind", sent, refusal(client.CodeInvalidAttribute,
			`models has an unknown kind: "fix-ci" (valid: implement-work-item, fix-error, research)`), true},
		{"not an object", sent, refusal(client.CodeInvalidAttribute,
			`models must be an object of kinds of work to model names, got ["opus-5-5"]`), true},
		{"models not sent", client.Fields{"max_in_progress": int64(2)}, refusal(client.CodeInvalidAttribute,
			`models.research must be one of: sonnet-5-5, opus-5-5, got "opus-9-9"`), false},
		{"an older Flightdeck's unknown key", sent, refusal(client.CodeInvalidAttribute,
			"unknown key: models (settable: enabled, kinds, auto_merge, send_ci_failures, send_errors, wait_for_checks)"), false},
		{"another setting refused", sent, refusal(client.CodeInvalidAttribute, "max_in_progress must be a whole number, got \"2\""), false},
		{"a rule, not a shape", sent, refusal(client.CodeValidationFailed, "Most a task may cost can't be more than the daily budget"), false},
		{"not a 422", sent, &client.Error{Status: 409, Code: client.CodeStaleObject, Message: "models changed"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentWorkModelsRefused(tc.sent, tc.err); got != tc.want {
				t.Fatalf("agentWorkModelsRefused = %t, want %t", got, tc.want)
			}
		})
	}
}

// refusesAsUnknown and agentWorkUnknownNotes read the keys an unknown-key
// refusal names, and give models' note first, in the API's order.
func TestRefusesAsUnknown_models(t *testing.T) {
	refusal := func(code, msg string) error {
		return &client.Error{Method: "PATCH", Path: "/projects/4/agent-work", Status: 422, Code: code, Message: msg}
	}
	settable := " (settable: enabled, kinds, models, auto_merge, send_ci_failures, send_errors, wait_for_checks, label_id)"
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"one key", refusal(client.CodeInvalidAttribute, "unknown key: models"+settable), "\n\n" + agentWorkModelsUnknownNote},
		{"all five", refusal(client.CodeInvalidAttribute, "unknown keys: auto_merge, models, send_ci_failures, send_errors, wait_for_checks"+settable),
			"\n\n" + agentWorkModelsUnknownNote + "\n\n" + agentWorkAutoMergeUnknownNote + "\n\n" + agentWorkSendCIFailuresUnknownNote +
				"\n\n" + agentWorkSendErrorsUnknownNote + "\n\n" + agentWorkWaitForChecksUnknownNote},
		{"several, some left out", refusal(client.CodeInvalidAttribute, "unknown keys: alpha, models and 4 more"+settable),
			"\n\n" + agentWorkModelsUnknownNote},
		{"another key, models settable", refusal(client.CodeInvalidAttribute, "unknown key: zebra"+settable), ""},
		{"models' own refusal", refusal(client.CodeInvalidAttribute, `models.research must be one of: sonnet-5-5, got "opus-5-5"`), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentWorkUnknownNotes(tc.err); got != tc.want {
				t.Errorf("agentWorkUnknownNotes = %q, want %q", got, tc.want)
			}
		})
	}
}

// agentWorkSettingRefused decides from the read after a refusal, never from
// the API's wording: only a 422 to a write that sent models, from a Flightdeck
// whose read has no models, is the server being too old for it.
func TestAgentWorkModelsSettingRefused(t *testing.T) {
	on := true
	knowsAll := &client.AgentWork{Models: map[string]string{}, AutoMerge: &on, SendCIFailures: &on, WaitForChecks: &on, SendErrors: &on}
	knowsFour := &client.AgentWork{AutoMerge: &on, SendCIFailures: &on, WaitForChecks: &on, SendErrors: &on}
	unknownKey := &client.Error{Method: "PATCH", Path: "/projects/4/agent-work", Status: 422, Code: client.CodeInvalidAttribute,
		Message: "unknown key: models (settable: enabled, kinds, auto_merge, send_ci_failures, send_errors, wait_for_checks)"}
	sent := client.Fields{"models": map[string]string{}}
	cases := []struct {
		name    string
		current *client.AgentWork
		sent    client.Fields
		err     error
		want    bool
		refused []string
	}{
		{"older Flightdeck, sent", knowsFour, sent, unknownKey, true, []string{"models"}},
		{"older Flightdeck, sent with send_errors", knowsFour, client.Fields{"models": map[string]string{}, "send_errors": true}, unknownKey,
			true, []string{"models"}},
		{"older than all, models and auto_merge sent", &client.AgentWork{}, client.Fields{"models": map[string]string{}, "auto_merge": true},
			unknownKey, true, []string{"models", "auto_merge"}},
		{"a Flightdeck that knows it", knowsAll, sent, unknownKey, false, nil},
		{"not sent", knowsFour, client.Fields{"max_in_progress": int64(2)}, unknownKey, false, nil},
		{"not a 422", knowsFour, sent, &client.Error{Status: 409, Code: client.CodeStaleObject}, false, nil},
		{"no read", nil, sent, unknownKey, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentWorkSettingRefused(agentWorkModels, tc.current, tc.sent, tc.err); got != tc.want {
				t.Fatalf("agentWorkSettingRefused = %t, want %t", got, tc.want)
			}
			var keys []string
			for _, setting := range agentWorkRefusedSettings(tc.current, tc.sent, tc.err) {
				keys = append(keys, setting.key)
			}
			if strings.Join(keys, ",") != strings.Join(tc.refused, ",") {
				t.Fatalf("agentWorkRefusedSettings = %v, want %v", keys, tc.refused)
			}
		})
	}
}

// The plan-time check of models: every key a kind of work, every model named.
// Model names themselves are never judged, and a null or unknown map, or a
// model not known until apply, is not the check's to refuse.
func TestAgentWorkModelsValidator(t *testing.T) {
	ctx := context.Background()
	at := path.Root("agent_work").AtName("models")
	validate := func(v types.Map) diag.Diagnostics {
		t.Helper()
		resp := &validator.MapResponse{}
		agentWorkModelsValidator{}.ValidateMap(ctx, validator.MapRequest{Path: at, ConfigValue: v}, resp)
		return resp.Diagnostics
	}
	of := func(values map[string]attr.Value) types.Map { return types.MapValueMust(types.StringType, values) }
	model := types.StringValue
	for name, v := range map[string]types.Map{
		"one kind":                  of(map[string]attr.Value{"implement-work-item": model("opus-5-5")}),
		"every kind":                of(map[string]attr.Value{"implement-work-item": model("opus-5-5"), "fix-error": model("sonnet-5-5"), "research": model("opus-5-5")}),
		"none":                      of(map[string]attr.Value{}),
		"a model nobody offers yet": of(map[string]attr.Value{"research": model("sonnet-9-9")}),
		"a model with odd spelling": of(map[string]attr.Value{"research": model("Opus 5.5")}),
		"a model not known yet":     of(map[string]attr.Value{"research": types.StringUnknown()}),
		"null":                      types.MapNull(types.StringType),
		"unknown":                   types.MapUnknown(types.StringType),
	} {
		if diags := validate(v); diags.HasError() {
			t.Errorf("%s refused: %v", name, diags.Errors())
		}
	}

	for name, tc := range map[string]struct {
		value   types.Map
		summary []string
		paths   []path.Path
	}{
		"an unknown kind": {of(map[string]attr.Value{"fix-ci": model("opus-5-5"), "research": model("opus-5-5")}),
			[]string{"Unknown kind of work in agent_work.models"}, []path.Path{at.AtMapKey("fix-ci")}},
		"two unknown kinds, in name order": {of(map[string]attr.Value{"zeta": model("opus-5-5"), "alpha": model("opus-5-5")}),
			[]string{"Unknown kind of work in agent_work.models", "Unknown kind of work in agent_work.models"},
			[]path.Path{at.AtMapKey("alpha"), at.AtMapKey("zeta")}},
		"an empty model": {of(map[string]attr.Value{"research": model("")}),
			[]string{"No model named for research"}, []path.Path{at.AtMapKey("research")}},
		"a blank model": {of(map[string]attr.Value{"fix-error": model(" \t")}),
			[]string{"No model named for fix-error"}, []path.Path{at.AtMapKey("fix-error")}},
		"a null model": {of(map[string]attr.Value{"implement-work-item": types.StringNull()}),
			[]string{"No model named for implement-work-item"}, []path.Path{at.AtMapKey("implement-work-item")}},
	} {
		t.Run(name, func(t *testing.T) {
			errs := validate(tc.value).Errors()
			if len(errs) != len(tc.summary) {
				t.Fatalf("expected %d error(s), got %v", len(tc.summary), errs)
			}
			for i, e := range errs {
				if e.Summary() != tc.summary[i] {
					t.Errorf("error %d summary = %q, want %q", i, e.Summary(), tc.summary[i])
				}
				withPath, ok := e.(diag.DiagnosticWithPath)
				if !ok || !withPath.Path().Equal(tc.paths[i]) {
					t.Errorf("error %d is not reported at %s: %#v", i, tc.paths[i], e)
				}
			}
			if strings.HasPrefix(errs[0].Summary(), "Unknown kind") {
				for _, kind := range client.AgentWorkKinds {
					if !strings.Contains(errs[0].Detail(), kind) {
						t.Errorf("the refusal does not name the kind %s: %s", kind, errs[0].Detail())
					}
				}
			}
		})
	}
}

// keepUnsetAsPlanned changes models in the result only when the configuration
// leaves it unset and the plan knew its value, and never touches the other
// newer settings.
func TestKeepUnsetModelsAsPlanned(t *testing.T) {
	ctx := t.Context()
	on := types.BoolValue(true)
	empty := types.MapValueMust(types.StringType, map[string]attr.Value{})
	set := types.MapValueMust(types.StringType, map[string]attr.Value{"research": types.StringValue("opus-5-5")})
	obj := func(v types.Map) types.Object {
		return agentWorkObj(t, &agentWorkModel{Models: v, SendErrors: on, MaxInProgress: types.Int64Value(3)})
	}
	cases := []struct {
		name                   string
		config, planned, after types.Object
		want                   types.Map
	}{
		{"unset, the plan had null, the server says {}", obj(types.MapNull(types.StringType)), obj(types.MapNull(types.StringType)), obj(empty), types.MapNull(types.StringType)},
		{"unset, the plan had a map, the server says null", obj(types.MapNull(types.StringType)), obj(set), obj(types.MapNull(types.StringType)), set},
		{"no block, the plan had null", types.ObjectNull(agentWorkAttrTypes), obj(types.MapNull(types.StringType)), obj(empty), types.MapNull(types.StringType)},
		{"unset, the plan did not know", obj(types.MapNull(types.StringType)), obj(types.MapUnknown(types.StringType)), obj(empty), empty},
		{"configured, the server says null", obj(set), obj(set), obj(types.MapNull(types.StringType)), types.MapNull(types.StringType)},
		{"configured {}, the server says a map", obj(empty), obj(empty), obj(set), set},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			out := keepUnsetAsPlanned("models", tc.config, tc.planned, tc.after, &diags)
			if diags.HasError() {
				t.Fatalf("errors: %v", diags.Errors())
			}
			var got agentWorkModel
			diags.Append(out.As(ctx, &got, objectAsOptions)...)
			if !got.Models.Equal(tc.want) {
				t.Errorf("models = %v, want %v", got.Models, tc.want)
			}
			if got.MaxInProgress.ValueInt64() != 3 || !got.SendErrors.Equal(on) {
				t.Errorf("the rest of the block changed: %+v", got)
			}
		})
	}
}
