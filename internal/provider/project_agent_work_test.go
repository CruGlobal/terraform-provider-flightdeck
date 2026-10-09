package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Every test here that turns agent work on turns it off again before the
// project is destroyed, so nothing is ever left enabled. None of them links a
// GitHub repository, so even while agent work is on, Flightdeck has nothing
// it could send (no_github_repo is always a blocker).

func TestProjectAgentWork_settingsRoundTrip(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Agent work"
  agent_work = {
    enabled          = false
    kinds            = ["implement-work-item"]
    base_ref         = "develop"
    max_in_progress  = 2
    daily_budget_usd = 12.34
    task_max_usd     = 2.5
    task_max_minutes = 45
    queue_minutes    = 90
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "agent_work.enabled", "false"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.kinds.#", "1"),
					resource.TestCheckTypeSetElemAttr(projectRes, "agent_work.kinds.*", "implement-work-item"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.base_ref", "develop"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "2"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.daily_budget_usd", "12.34"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.task_max_usd", "2.5"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.task_max_minutes", "45"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.queue_minutes", "90"),
					// Unset settings come back with the server's defaults.
					resource.TestCheckResourceAttr(projectRes, "agent_work.runbook", "implement-work-item@1"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.accept_machine_labels", "false"),
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.label_id"),
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.agent_account_id"),
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.label_chosen_at"),
					// The first write that changes something creates the settings at 1.
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
				Config: projectConfig(env, identifier, `
  name = "Agent work"
  agent_work = {
    enabled          = false
    kinds            = ["implement-work-item"]
    base_ref         = "develop"
    max_in_progress  = 3
    daily_budget_usd = 12.34
    task_max_usd     = 2.5
    task_max_minutes = 45
    queue_minutes    = 90
  }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "3"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "2"),
				),
			},
			{
				// Dropping the block leaves the settings alone and plans nothing.
				Config: projectConfig(env, identifier, `  name = "Agent work"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "3"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.daily_budget_usd", "12.34"),
				),
			},
		},
	})
}

// A project nobody configured reads the defaults at version 0, and a project
// rename does not touch the agent work settings.
func TestProjectAgentWork_unconfiguredReadsTheDefaults(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `  name = "Defaults"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.enabled", "false"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.kinds.#", "0"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.base_ref", "main"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "1"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.daily_budget_usd", "10"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.task_max_usd", "5"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.task_max_minutes", "30"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.queue_minutes", "60"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "0"),
				),
			},
			{
				Config: projectConfig(env, identifier, `  name = "Defaults renamed"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "name", "Defaults renamed"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "0"),
				),
			},
		},
	})
	if env.live() {
		return
	}
	if patches := env.fake.RequestsMatching("PATCH", "/api/v1/projects/"); len(agentWorkRequests(patches)) != 0 {
		t.Fatalf("an unconfigured block must never be written, saw %d agent-work PATCHes", len(agentWorkRequests(patches)))
	}
}

// Turning agent work on warns at plan time, and while it is on, what stops it
// comes back as warnings after the apply and on refresh, not as state.
func TestProjectAgentWork_turningOnWarnsAndReportsBlockers(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	identifier := randIdentifier()
	config := func(enabled bool) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = "Agents on"
  agent_work = {
    enabled = %t
    kinds   = ["implement-work-item"]
  }`, enabled))
	}
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config(false),
				Check:  resource.TestCheckResourceAttr(projectRes, "agent_work.enabled", "false"),
			},
			{
				Config: config(true),
				Check:  resource.TestCheckResourceAttr(projectRes, "agent_work.enabled", "true"),
			},
			{
				// Off again, which is also where the test must end.
				Config: config(false),
				Check:  resource.TestCheckResourceAttr(projectRes, "agent_work.enabled", "false"),
			},
		},
	})
	if !anyDiagnostic(recorded.planWarnings(), "This apply turns on agent work for project "+identifier,
		"lets AutoPilot agents take project "+identifier+"'s labelled work items and open pull requests") {
		t.Fatalf("no plan-time warning for turning agent work on; plan warnings: %s", describe(recorded.planWarnings()))
	}
	// No label is ever chosen here, so no_label is always one of the blockers.
	summary := "Agent work cannot start on project " + identifier + " right now: no_label"
	if !anyDiagnostic(recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning), summary) {
		t.Fatalf("no no_label blocker warning after apply; apply warnings: %s", describe(recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning)))
	}
	if !anyDiagnostic(recorded.readWarnings(), summary) {
		t.Fatalf("no no_label blocker warning on refresh; refresh warnings: %s", describe(recorded.readWarnings()))
	}
}

// While agent work is off, its blockers are not news: "disabled" is always
// one of them, and nothing else matters until it is on.
func TestProjectAgentWork_offReportsNoBlockers(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	identifier := randIdentifier()
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Agents off"
  agent_work = {
    enabled = false
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "agent_work.enabled", "false"),
			},
		},
	})
	for _, set := range [][]*tfprotov6.Diagnostic{recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning), recorded.readWarnings(), recorded.planWarnings()} {
		if anyDiagnostic(set, "Agent work cannot start") || anyDiagnostic(set, "turns on agent work") {
			t.Fatalf("agent work is off, so nothing should warn: %s", describe(set))
		}
	}
}

// The configuration this block needs for a label, without a dependency cycle:
// a flightdeck_label depends on its project, so the label's project_id comes
// from a data source looked up by identifier, which only works once the
// project exists. Removing label_id and agent_account_id from configuration
// afterwards must leave both where they are: the API reads a null for either
// as "clear it", so the provider must never send one.
func TestProjectAgentWork_labelAndAccountAreNeverClearedByOmission(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	identifier := randIdentifier()
	// A service account only exists on the fake; live, the label alone is tested.
	account, accountLine := "", ""
	if !env.live() {
		account = `
data "flightdeck_workspace_member" "agent" {
  email = "deploy-bot@example.com"
}
`
		accountLine = "    agent_account_id = data.flightdeck_workspace_member.agent.id\n"
	}
	project := func(body string) string {
		return env.providerConfig() + fmt.Sprintf(`
data "flightdeck_project" "self" {
  identifier = %q
}

resource "flightdeck_label" "agent" {
  project_id = data.flightdeck_project.self.id
  name       = "agent-ready"
}
%s
resource "flightdeck_project" "test" {
  identifier = %q
  name       = "Agent label"
%s
}
`, identifier, account, identifier, body)
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// The project first, so the data source can find it.
				Config: projectConfig(env, identifier, `  name = "Agent label"`),
			},
			{
				Config: project(`  agent_work = {
    label_id         = flightdeck_label.agent.id
` + accountLine + `  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(projectRes, "agent_work.label_id", "flightdeck_label.agent", "id"),
					resource.TestCheckResourceAttrSet(projectRes, "agent_work.label_chosen_at"),
					func(s *terraform.State) error {
						if env.live() {
							return nil
						}
						return resource.TestCheckResourceAttr(projectRes, "agent_work.agent_account_id", "4")(s)
					},
				),
			},
			{
				// Both references leave the configuration; another setting changes.
				Config: project(`  agent_work = {
    max_in_progress = 2
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "2"),
					resource.TestCheckResourceAttrPair(projectRes, "agent_work.label_id", "flightdeck_label.agent", "id"),
					resource.TestCheckResourceAttrSet(projectRes, "agent_work.label_chosen_at"),
					func(s *terraform.State) error {
						if env.live() {
							return nil
						}
						return resource.TestCheckResourceAttr(projectRes, "agent_work.agent_account_id", "4")(s)
					},
				),
			},
		},
	})
	if env.live() {
		return
	}
	writes := agentWorkRequests(env.fake.RequestsMatching("PATCH", "/api/v1/projects/"))
	if len(writes) != 2 {
		t.Fatalf("expected two agent-work PATCHes, got %d", len(writes))
	}
	first, last := agentWorkBody(t, writes[0]), agentWorkBody(t, writes[1])
	if _, ok := first["label_id"]; !ok {
		t.Errorf("the write that set the label did not send label_id: %v", first)
	}
	for _, key := range []string{"label_id", "agent_account_id"} {
		if _, sent := last[key]; sent {
			t.Errorf("%s was sent although the configuration no longer sets it (the API would clear it): %v", key, last)
		}
	}
	if len(last) != 1 || last["max_in_progress"] != float64(2) {
		t.Errorf("the second write must send max_in_progress only, got %v", last)
	}
}

// The money rules are checked at plan time: Flightdeck refuses an amount with
// a third decimal place rather than rounding it.
func TestProjectAgentWork_moneyIsValidatedAtPlanTime(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	identifier := randIdentifier()
	cfg := func(settings string) string {
		return projectConfig(env, identifier, `
  name = "Money"
  agent_work = {
`+settings+`
  }`)
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: cfg(`    daily_budget_usd = 10.005`), PlanOnly: true, ExpectError: regexMust(`(?s)10\.005\s+has\s+more\s+than\s+two\s+decimal\s+places`)},
			{Config: cfg(`    task_max_usd = 1.999`), PlanOnly: true, ExpectError: regexMust(`(?s)1\.999\s+has\s+more\s+than\s+two\s+decimal\s+places`)},
			{Config: cfg(`    task_max_usd = 0`), PlanOnly: true, ExpectError: regexMust(`must\s+be\s+more\s+than\s+0`)},
			{Config: cfg(`    daily_budget_usd = 1000.01`), PlanOnly: true, ExpectError: regexMust(`must\s+be\s+at\s+most\s+1000`)},
			{Config: cfg(`    task_max_usd = 101`), PlanOnly: true, ExpectError: regexMust(`must\s+be\s+at\s+most\s+100`)},
			{
				Config:      cfg("    task_max_usd     = 20\n    daily_budget_usd = 15"),
				PlanOnly:    true,
				ExpectError: regexMust(`(?s)task_max_usd\s+\(20\)\s+cannot\s+be\s+more\s+than\s+daily_budget_usd\s+\(15\)`),
			},
			{Config: cfg(`    max_in_progress = 11`), PlanOnly: true, ExpectError: regexMust(`between\s+1\s+and\s+10`)},
			{Config: cfg(`    base_ref = ""`), PlanOnly: true, ExpectError: regexMust(`Invalid Attribute Value`)},
			{Config: cfg(`    kinds = ["fix-everything"]`), PlanOnly: true, ExpectError: regexMust(`implement-work-item`)},
		},
	})
}

// Raising the per-task limit past a stored daily budget the configuration
// does not mention warns at plan time, and the API still refuses it.
func TestProjectAgentWork_mergedBudgetWarnsAndFailsAtApply(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	identifier := randIdentifier()
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Budget"
  agent_work = {
    daily_budget_usd = 10
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "agent_work.daily_budget_usd", "10"),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Budget"
  agent_work = {
    task_max_usd = 20
  }`),
				ExpectError: regexMust(`(?s)Most\s+a\s+task\s+may\s+cost\s+can't\s+be\s+more\s+than\s+the\s+daily\s+budget`),
			},
		},
	})
	if !anyDiagnostic(recorded.planWarnings(), "Agent work limits will not be coherent", "task_max_usd to 20", "daily_budget_usd is 10") {
		t.Fatalf("no merged-budget warning; plan warnings: %s", describe(recorded.planWarnings()))
	}
}

func TestProjectAgentWork_writesUseTheSettingsOwnLockVersion(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Transport"
  agent_work = {
    max_in_progress = 2
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "1"),
					// The settings are their own row: the project's version is untouched.
					resource.TestCheckResourceAttr(projectRes, "lock_version", "0"),
				),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Transport"
  agent_work = {
    max_in_progress = 4
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "2"),
					// The update's own project PATCH bumps it once; the agent
					// work write would have made it 2 if the two shared a version.
					resource.TestCheckResourceAttr(projectRes, "lock_version", "1"),
				),
			},
		},
	})
	writes := agentWorkRequests(env.fake.RequestsMatching("PATCH", "/api/v1/projects/"))
	if len(writes) != 2 {
		t.Fatalf("expected 2 agent-work PATCHes, got %d", len(writes))
	}
	if got := writes[0].Header.Get("If-Match"); got != `"0"` {
		t.Errorf("the first write must pin version 0 (no settings saved yet), sent %q", got)
	}
	if got := writes[1].Header.Get("If-Match"); got != `"1"` {
		t.Errorf("the second write must pin the settings' own version 1, sent %q", got)
	}
	body := agentWorkBody(t, writes[0])
	if len(body) != 1 || body["max_in_progress"] != float64(2) {
		t.Errorf("the write must send the configured setting only, got %v", body)
	}
}

func TestProjectAgentWork_consoleChangeShowsInPlan(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	config := projectConfig(env, identifier, `
  name = "Console"
  agent_work = {
    max_in_progress = 2
  }`)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config, Check: captureAttr(projectRes, "id", &id)},
			{
				PreConfig: func() {
					env.fake.SetAgentWorkOutOfBand(mustInt64(t, id), func(row *flightdecktest.AgentWorkSetting) { row.MaxInProgress = 5 })
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "2"),
					// Created at 1, moved by the console to 2, written back at 3.
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "3"),
				),
			},
		},
	})
}

func TestProjectAgentWork_staleWriteIsReported(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Stale"
  agent_work = {
    max_in_progress = 2
  }`),
				Check: captureAttr(projectRes, "id", &id),
			},
			{
				// Someone saves the settings page between the plan and the write.
				PreConfig: func() {
					pid := mustInt64(t, id)
					env.fake.OnNextRequest("PATCH", fmt.Sprintf("/api/v1/projects/%d/agent-work", pid), func() {
						env.fake.SetAgentWorkOutOfBand(pid, func(row *flightdecktest.AgentWorkSetting) { row.QueueMinutes = 120 })
					})
				},
				Config: projectConfig(env, identifier, `
  name = "Stale"
  agent_work = {
    max_in_progress = 3
  }`),
				ExpectError: regexMust(`(?s)agent work settings modified outside of Terraform.*lock_version 1, the server now has 2`),
			},
		},
	})
}

func TestProjectAgentWork_requiresWorkspaceAdmin(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetWorkspaceAdmin(false)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Unconfigured, the block is simply null for a non-admin, who can
				// only create a project with terraform_managed off.
				Config: projectConfig(env, identifier, `
  name              = "Not an admin"
  terraform_managed = false`),
				Check: resource.TestCheckNoResourceAttr(projectRes, "agent_work.%"),
			},
			{
				Config: projectConfig(env, identifier, `
  name              = "Not an admin"
  terraform_managed = false
  agent_work = {
    max_in_progress = 2
  }`),
				ExpectError: regexMust(`Agent work settings require a workspace owner or admin`),
			},
		},
	})
}

func TestProjectAgentWork_endpointAbsent(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkEndpoint(false)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `  name = "Old server"`),
				Check:  resource.TestCheckNoResourceAttr(projectRes, "agent_work.%"),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Old server"
  agent_work = {
    enabled = false
  }`),
				ExpectError: regexMust(`Agent work settings are not available on this Flightdeck`),
			},
		},
	})
}

// A refusal while creating the project is a warning, not an error: an error
// would taint the new project, and the next apply would replace it.
func TestProjectAgentWork_refusalOnCreateDoesNotTaintTheProject(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// User 2 is a person, not a service account.
				Config: projectConfig(env, identifier, `
  name = "Refused"
  agent_work = {
    agent_account_id = 2
  }`),
				Check:              captureAttr(projectRes, "id", &id),
				ExpectNonEmptyPlan: true,
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Refused"
  agent_work = {
    agent_account_id = 4
  }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "agent_work.agent_account_id", "4"),
				),
			},
		},
	})
	if !anyDiagnostic(recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning), "Agent work settings were not saved", "must be a service account") {
		t.Fatalf("no create-time warning for the refused settings; apply warnings: %s", describe(recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning)))
	}
}

func TestProjectAgentWork_dataSourceReportsTheSettings(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Lookup"
  agent_work = {
    max_in_progress = 2
  }`) + `
data "flightdeck_project" "lookup" {
  id = flightdeck_project.test.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.flightdeck_project.lookup", "agent_work.max_in_progress", "2"),
					resource.TestCheckResourceAttr("data.flightdeck_project.lookup", "agent_work.enabled", "false"),
					resource.TestCheckResourceAttr("data.flightdeck_project.lookup", "agent_work.lock_version", "1"),
				),
			},
		},
	})
}

// AgentWorkSettingKeys documents the endpoint's writable set, and
// agentWorkFields writes its own list by hand; this holds them together.
func TestAgentWorkFields_writePathMatchesSettingKeys(t *testing.T) {
	ctx := context.Background()
	kinds, _ := types.SetValueFrom(ctx, types.StringType, []string{"implement-work-item"})
	fields := mustAgentWorkFields(t, agentWorkModel{
		Enabled: types.BoolValue(true), Kinds: kinds, LabelID: types.Int64Value(7),
		LabelChosenAt: types.StringValue("2026-10-01T00:00:00Z"), AcceptMachineLabels: types.BoolValue(false),
		AgentAccountID: types.Int64Value(8), BaseRef: types.StringValue("main"), MaxInProgress: types.Int64Value(1),
		DailyBudgetUSD: types.Float64Value(10), TaskMaxUSD: types.Float64Value(5), TaskMaxMinutes: types.Int64Value(30),
		QueueMinutes: types.Int64Value(60), Runbook: types.StringValue("implement-work-item@1"), LockVersion: types.Int64Value(3),
	})
	got := make([]string, 0, len(fields))
	for k := range fields {
		got = append(got, k)
	}
	want := append([]string{}, client.AgentWorkSettingKeys...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the write path and AgentWorkSettingKeys have drifted:\n  sends: %v\n   list: %v", got, want)
	}
	// Read-only keys are a 422 on the API, so they must never be sent.
	for _, k := range []string{"label_chosen_at", "lock_version", "blockers", "project_id"} {
		if _, sent := fields[k]; sent {
			t.Errorf("agentWorkFields sent the read-only %q", k)
		}
	}
}

// The rule the item exists for: only configured settings are sent, and an
// unset label_id or agent_account_id is left out, never sent as null, because
// the API reads a null for either as "clear it".
func TestAgentWorkFields_sendsOnlyConfiguredSettings(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		model agentWorkModel
		want  map[string]any
	}{
		{
			name:  "unset references are left out",
			model: agentWorkModel{Enabled: types.BoolValue(true), MaxInProgress: types.Int64Value(2)},
			want:  map[string]any{"enabled": true, "max_in_progress": int64(2)},
		},
		{
			name:  "references not known until apply are left out too",
			model: agentWorkModel{LabelID: types.Int64Unknown(), AgentAccountID: types.Int64Unknown(), BaseRef: types.StringValue("develop")},
			want:  map[string]any{"base_ref": "develop"},
		},
		{
			name:  "configured references are sent",
			model: agentWorkModel{LabelID: types.Int64Value(11), AgentAccountID: types.Int64Value(12)},
			want:  map[string]any{"label_id": int64(11), "agent_account_id": int64(12)},
		},
		{
			name:  "false and an empty kinds list are values",
			model: agentWorkModel{Enabled: types.BoolValue(false), AcceptMachineLabels: types.BoolValue(false), Kinds: types.SetValueMust(types.StringType, nil)},
			want:  map[string]any{"enabled": false, "accept_machine_labels": false, "kinds": []string{}},
		},
		{
			name:  "money is sent as a number",
			model: agentWorkModel{DailyBudgetUSD: types.Float64Value(12.34), TaskMaxUSD: types.Float64Value(2.5)},
			want:  map[string]any{"daily_budget_usd": 12.34, "task_max_usd": 2.5},
		},
		{
			name:  "nothing configured sends nothing",
			model: agentWorkModel{},
			want:  map[string]any{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := mustAgentWorkFields(t, tc.model)
			if fmt.Sprint(map[string]any(fields)) != fmt.Sprint(tc.want) {
				t.Fatalf("fields = %v, want %v", fields, tc.want)
			}
			for _, key := range []string{"label_id", "agent_account_id"} {
				if v, sent := fields[key]; sent && v == nil {
					t.Fatalf("%s was sent as null, which the API reads as clearing it", key)
				}
			}
		})
	}
	// No block at all: nothing to send, and not an empty write.
	var diags diag.Diagnostics
	if fields := agentWorkFields(ctx, types.ObjectNull(agentWorkAttrTypes), &diags); fields != nil {
		t.Fatalf("a null block must send nothing, got %v", fields)
	}
}

func TestMoneyValidator(t *testing.T) {
	cases := []struct {
		value float64
		max   float64
		want  string
	}{
		{10, 1000, ""},
		{10.5, 1000, ""},
		{12.34, 1000, ""},
		{0.01, 100, ""},
		{1000, 1000, ""},
		{100, 100, ""},
		{10.005, 1000, "more than two decimal places"},
		{1.999, 100, "more than two decimal places"},
		{0.001, 100, "more than two decimal places"},
		{0, 100, "must be more than 0"},
		{-5, 100, "must be more than 0"},
		{1000.01, 1000, "must be at most 1000"},
		{100.5, 100, "must be at most 100"},
	}
	for _, tc := range cases {
		t.Run(strconv.FormatFloat(tc.value, 'f', -1, 64), func(t *testing.T) {
			resp := &validator.Float64Response{}
			moneyValidator{max: tc.max}.ValidateFloat64(context.Background(), validator.Float64Request{
				Path: path.Root("agent_work").AtName("daily_budget_usd"), ConfigValue: types.Float64Value(tc.value),
			}, resp)
			if tc.want == "" {
				if resp.Diagnostics.HasError() {
					t.Fatalf("%v refused: %v", tc.value, resp.Diagnostics.Errors())
				}
				return
			}
			if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), tc.want) {
				t.Fatalf("%v: want an error saying %q, got %v", tc.value, tc.want, resp.Diagnostics.Errors())
			}
		})
	}
	// Null and unknown are not the validator's to judge.
	for _, v := range []types.Float64{types.Float64Null(), types.Float64Unknown()} {
		resp := &validator.Float64Response{}
		moneyValidator{max: 100}.ValidateFloat64(context.Background(), validator.Float64Request{ConfigValue: v}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("%v refused: %v", v, resp.Diagnostics.Errors())
		}
	}
}

func TestWarnAgentWorkEnabled(t *testing.T) {
	ctx := context.Background()
	on, off := types.BoolValue(true), types.BoolValue(false)
	cases := []struct {
		name                 string
		config, prior, plan  *agentWorkModel
		wantSummary, wantNot string
	}{
		{name: "create with it on", config: &agentWorkModel{Enabled: on}, plan: &agentWorkModel{Enabled: on},
			wantSummary: "This apply turns on agent work for project APP"},
		{name: "off to on", config: &agentWorkModel{Enabled: on}, prior: &agentWorkModel{Enabled: off}, plan: &agentWorkModel{Enabled: on},
			wantSummary: "This apply turns on agent work for project APP"},
		{name: "already on", config: &agentWorkModel{Enabled: on}, prior: &agentWorkModel{Enabled: on}, plan: &agentWorkModel{Enabled: on}},
		{name: "turning it off", config: &agentWorkModel{Enabled: off}, prior: &agentWorkModel{Enabled: on}, plan: &agentWorkModel{Enabled: off}},
		{name: "staying off", config: &agentWorkModel{Enabled: off}, prior: &agentWorkModel{Enabled: off}, plan: &agentWorkModel{Enabled: off}},
		{name: "not known until apply", config: &agentWorkModel{Enabled: types.BoolUnknown()}, prior: &agentWorkModel{Enabled: off},
			plan: &agentWorkModel{Enabled: types.BoolUnknown()}, wantSummary: "This apply may turn on agent work for project APP"},
		{name: "unconfigured, kept on from state", prior: &agentWorkModel{Enabled: on}, plan: &agentWorkModel{Enabled: on}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			warnAgentWorkEnabled(ctx, types.StringValue("APP"), agentWorkObj(t, tc.config), agentWorkObj(t, tc.prior), agentWorkObj(t, tc.plan), &diags)
			if diags.HasError() {
				t.Fatalf("errors: %v", diags.Errors())
			}
			if tc.wantSummary == "" {
				if diags.WarningsCount() != 0 {
					t.Fatalf("expected no warning, got %v", diags.Warnings())
				}
				return
			}
			if diags.WarningsCount() != 1 || diags.Warnings()[0].Summary() != tc.wantSummary {
				t.Fatalf("want one warning %q, got %v", tc.wantSummary, diags.Warnings())
			}
			if !strings.Contains(diags.Warnings()[0].Detail(), "AutoPilot agents") {
				t.Fatalf("the warning must say what agents will do: %s", diags.Warnings()[0].Detail())
			}
		})
	}
}

func TestWarnAgentWorkBlockers(t *testing.T) {
	blockers := []client.AgentWorkBlocker{
		{Code: "no_label", Message: "No agent label is chosen."},
		{Code: "no_github_repo", Message: "The project has no linked GitHub repository."},
		{Code: "no_label", Message: "A second one with the same code."},
	}
	var off diag.Diagnostics
	warnAgentWorkBlockers(&client.AgentWork{Enabled: false, Blockers: blockers}, "APP", &off)
	if off.WarningsCount() != 0 {
		t.Fatalf("agent work is off, so no blocker is news: %v", off.Warnings())
	}
	var none diag.Diagnostics
	warnAgentWorkBlockers(nil, "APP", &none)
	warnAgentWorkBlockers(&client.AgentWork{Enabled: true}, "APP", &none)
	if none.WarningsCount() != 0 {
		t.Fatalf("nothing to report, got %v", none.Warnings())
	}
	var on diag.Diagnostics
	warnAgentWorkBlockers(&client.AgentWork{Enabled: true, Blockers: blockers}, "APP", &on)
	var summaries []string
	for _, w := range on.Warnings() {
		summaries = append(summaries, w.Summary())
	}
	want := []string{
		"Agent work cannot start on project APP right now: no_label",
		"Agent work cannot start on project APP right now: no_github_repo",
		"Agent work cannot start on project APP right now: no_label (2)",
	}
	if strings.Join(summaries, "|") != strings.Join(want, "|") {
		t.Fatalf("summaries = %v, want %v (each distinct, so Terraform does not fold them)", summaries, want)
	}
	if !strings.Contains(on.Warnings()[0].Detail(), "No agent label is chosen.") {
		t.Fatalf("the warning must carry the API's message: %s", on.Warnings()[0].Detail())
	}
}

func TestPlanAgentWorkComputed(t *testing.T) {
	ctx := context.Background()
	prior := &agentWorkModel{Enabled: types.BoolValue(false), LabelID: types.Int64Value(7), MaxInProgress: types.Int64Value(1),
		DailyBudgetUSD: types.Float64Value(12.34), LabelChosenAt: types.StringValue("2026-10-01T00:00:00Z"), LockVersion: types.Int64Value(3)}
	cases := []struct {
		name                   string
		plan                   agentWorkModel
		wantVersion, wantLabel bool // whether each keeps its prior value
	}{
		{name: "nothing changes", plan: *prior, wantVersion: true, wantLabel: true},
		{name: "a setting changes", plan: with(*prior, func(m *agentWorkModel) { m.MaxInProgress = types.Int64Value(2) }), wantLabel: true},
		{name: "the label changes", plan: with(*prior, func(m *agentWorkModel) { m.LabelID = types.Int64Value(8) })},
		{name: "the label is not known yet", plan: with(*prior, func(m *agentWorkModel) { m.LabelID = types.Int64Unknown() })},
		// A configured amount carries more precision than the API's answer;
		// the same float64 is the same setting.
		{name: "money equal as sent", plan: with(*prior, func(m *agentWorkModel) { m.DailyBudgetUSD = types.Float64Value(12.34) }), wantVersion: true, wantLabel: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			out := planAgentWorkComputed(ctx, agentWorkObj(t, prior), agentWorkObj(t, &tc.plan), &diags)
			if diags.HasError() {
				t.Fatalf("errors: %v", diags.Errors())
			}
			var got agentWorkModel
			diags.Append(out.As(ctx, &got, objectAsOptions)...)
			if tc.wantVersion != !got.LockVersion.IsUnknown() {
				t.Errorf("lock_version = %v, want kept=%t", got.LockVersion, tc.wantVersion)
			}
			if tc.wantLabel != !got.LabelChosenAt.IsUnknown() {
				t.Errorf("label_chosen_at = %v, want kept=%t", got.LabelChosenAt, tc.wantLabel)
			}
		})
	}
	// On create there is no prior: both are unknown.
	var diags diag.Diagnostics
	out := planAgentWorkComputed(ctx, types.ObjectNull(agentWorkAttrTypes), agentWorkObj(t, prior), &diags)
	var got agentWorkModel
	diags.Append(out.As(ctx, &got, objectAsOptions)...)
	if !got.LockVersion.IsUnknown() || !got.LabelChosenAt.IsUnknown() {
		t.Errorf("on create both must be unknown, got %v and %v", got.LockVersion, got.LabelChosenAt)
	}
}

func TestValidateAgentWorkConfigAndBudgetWarning(t *testing.T) {
	ctx := context.Background()
	var diags diag.Diagnostics
	validateAgentWorkConfig(ctx, agentWorkObj(t, &agentWorkModel{TaskMaxUSD: types.Float64Value(20), DailyBudgetUSD: types.Float64Value(15)}), &diags)
	if !diags.HasError() {
		t.Fatal("a task costing more than the day, both configured, must fail the plan")
	}
	diags = nil
	validateAgentWorkConfig(ctx, agentWorkObj(t, &agentWorkModel{TaskMaxUSD: types.Float64Value(15), DailyBudgetUSD: types.Float64Value(15)}), &diags)
	if diags.HasError() {
		t.Fatalf("a task may cost the whole day: %v", diags.Errors())
	}

	// One side configured: the plan's merged pair decides, as a warning.
	diags = nil
	warnAgentWorkBudget(ctx,
		agentWorkObj(t, &agentWorkModel{DailyBudgetUSD: types.Float64Value(3)}),
		agentWorkObj(t, &agentWorkModel{DailyBudgetUSD: types.Float64Value(3), TaskMaxUSD: types.Float64Value(5)}), &diags)
	if diags.WarningsCount() != 1 || !strings.Contains(diags.Warnings()[0].Detail(), "sets daily_budget_usd to 3, and the project's stored task_max_usd is 5") {
		t.Fatalf("want the merged-budget warning naming each value with its own key, got %v", diags.Warnings())
	}
	diags = nil
	warnAgentWorkBudget(ctx,
		agentWorkObj(t, &agentWorkModel{DailyBudgetUSD: types.Float64Value(30)}),
		agentWorkObj(t, &agentWorkModel{DailyBudgetUSD: types.Float64Value(30), TaskMaxUSD: types.Float64Value(5)}), &diags)
	if diags.WarningsCount() != 0 {
		t.Fatalf("a coherent pair must not warn: %v", diags.Warnings())
	}
}

// --- helpers -----------------------------------------------------------------

// agentWorkObj builds a block object from a model, null for nil. A zero Set
// has no element type, so an unset kinds is made a typed null.
func agentWorkObj(t *testing.T, m *agentWorkModel) types.Object {
	t.Helper()
	if m == nil {
		return types.ObjectNull(agentWorkAttrTypes)
	}
	cp := *m
	if cp.Kinds.ElementType(context.Background()) == nil {
		cp.Kinds = types.SetNull(types.StringType)
	}
	obj, d := types.ObjectValueFrom(context.Background(), agentWorkAttrTypes, cp)
	if d.HasError() {
		t.Fatalf("building the block: %v", d)
	}
	return obj
}

func mustAgentWorkFields(t *testing.T, m agentWorkModel) client.Fields {
	t.Helper()
	var diags diag.Diagnostics
	fields := agentWorkFields(context.Background(), agentWorkObj(t, &m), &diags)
	if diags.HasError() {
		t.Fatalf("agentWorkFields: %v", diags.Errors())
	}
	return fields
}

func with(m agentWorkModel, fn func(*agentWorkModel)) agentWorkModel {
	fn(&m)
	return m
}

func agentWorkRequests(reqs []flightdecktestRequest) []flightdecktestRequest {
	var out []flightdecktestRequest
	for _, r := range reqs {
		if strings.HasSuffix(r.Path, "/agent-work") {
			out = append(out, r)
		}
	}
	return out
}

func agentWorkBody(t *testing.T, r flightdecktestRequest) map[string]any {
	t.Helper()
	var body map[string]map[string]any
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatalf("decoding %s: %v", r.Body, err)
	}
	return body["agent_work"]
}

func mustInt64(t *testing.T, s string) int64 {
	t.Helper()
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return v
}
