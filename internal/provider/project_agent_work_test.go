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
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Every test here that turns agent work on turns it off again before the
// project is destroyed, so nothing is ever left enabled. None of them that
// runs live chooses an agent label while agent work is on, or ticks research,
// so even then Flightdeck has nothing it could send (no_label is always a
// blocker).

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
			{
				Config:      cfg(`    kinds = ["fix-ci"]`),
				PlanOnly:    true,
				ExpectError: regexMust(`(?s)"implement-work-item"\s+"fix-error"\s+"research"`),
			},
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

// researchLabelConfig is the project with three labels of its own, the way a
// configuration gives a project labels without a dependency cycle (see
// TestProjectAgentWork_labelAndAccountAreNeverClearedByOmission): each label
// takes its project_id from a data source looked up by identifier, which only
// works once the project exists. A second project with a label of its own
// stands in for a label that is not this project's.
func researchLabelConfig(env *testEnv, identifier, other, body string) string {
	return env.providerConfig() + fmt.Sprintf(`
data "flightdeck_project" "self" {
  identifier = %q
}

resource "flightdeck_label" "agent" {
  project_id = data.flightdeck_project.self.id
  name       = "agent-ready"
}

resource "flightdeck_label" "research" {
  project_id = data.flightdeck_project.self.id
  name       = "research-first"
}

resource "flightdeck_label" "research_next" {
  project_id = data.flightdeck_project.self.id
  name       = "look-into-it"
}
%s
resource "flightdeck_project" "test" {
  identifier = %q
  name       = "Research label"
%s
}
`, identifier, otherProjectConfig(other), identifier, body)
}

// otherProjectConfig is a second project and a label in it, or nothing when
// other is empty.
func otherProjectConfig(other string) string {
	if other == "" {
		return ""
	}
	return fmt.Sprintf(`
resource "flightdeck_project" "other" {
  identifier = %q
  name       = "Another project"
}

resource "flightdeck_label" "foreign" {
  project_id = flightdeck_project.other.id
  name       = "research-elsewhere"
}
`, other)
}

// The research label is set, changed and imported like the agent label, and,
// like it, removing it from the configuration leaves the stored label alone:
// the API reads a null as "clear it", so the provider must never send one.
func TestProjectAgentWork_researchLabelSetChangeImportAndKeep(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	identifier := randIdentifier()
	cfg := func(body string) string { return researchLabelConfig(env, identifier, "", body) }
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// The project first, so the data source can find it.
				Config: projectConfig(env, identifier, `  name = "Research label"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.research_label_id"),
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.research_label_chosen_at"),
				),
			},
			{
				Config: cfg(`  agent_work = {
    label_id          = flightdeck_label.agent.id
    research_label_id = flightdeck_label.research.id
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(projectRes, "agent_work.label_id", "flightdeck_label.agent", "id"),
					resource.TestCheckResourceAttrPair(projectRes, "agent_work.research_label_id", "flightdeck_label.research", "id"),
					resource.TestCheckResourceAttrSet(projectRes, "agent_work.research_label_chosen_at"),
					resource.TestCheckResourceAttrSet(projectRes, "agent_work.label_chosen_at"),
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
				// Another research label; the agent label stays as it is.
				Config: cfg(`  agent_work = {
    label_id          = flightdeck_label.agent.id
    research_label_id = flightdeck_label.research_next.id
  }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(projectRes, "agent_work.research_label_id", "flightdeck_label.research_next", "id"),
					resource.TestCheckResourceAttrPair(projectRes, "agent_work.label_id", "flightdeck_label.agent", "id"),
					resource.TestCheckResourceAttrSet(projectRes, "agent_work.research_label_chosen_at"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "2"),
				),
			},
			{
				// Both labels leave the configuration; another setting changes.
				Config: cfg(`  agent_work = {
    max_in_progress = 2
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "2"),
					resource.TestCheckResourceAttrPair(projectRes, "agent_work.research_label_id", "flightdeck_label.research_next", "id"),
					resource.TestCheckResourceAttrSet(projectRes, "agent_work.research_label_chosen_at"),
					resource.TestCheckResourceAttrPair(projectRes, "agent_work.label_id", "flightdeck_label.agent", "id"),
					// The data source was read during this step's plan, after the
					// previous step chose research_next.
					resource.TestCheckResourceAttrPair("data.flightdeck_project.self", "agent_work.research_label_id",
						"flightdeck_label.research_next", "id"),
					resource.TestCheckResourceAttrSet("data.flightdeck_project.self", "agent_work.research_label_chosen_at"),
				),
			},
			{
				// Every kind Flightdeck knows. Agent work stays off, so nothing
				// is sent to AutoPilot.
				Config: cfg(`  agent_work = {
    kinds = ["implement-work-item", "fix-error", "research"]
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.enabled", "false"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.kinds.#", "3"),
					resource.TestCheckTypeSetElemAttr(projectRes, "agent_work.kinds.*", "implement-work-item"),
					resource.TestCheckTypeSetElemAttr(projectRes, "agent_work.kinds.*", "fix-error"),
					resource.TestCheckTypeSetElemAttr(projectRes, "agent_work.kinds.*", "research"),
					resource.TestCheckResourceAttrPair(projectRes, "agent_work.research_label_id", "flightdeck_label.research_next", "id"),
				),
			},
			{
				// Dropping the block leaves both labels alone and plans nothing.
				Config: cfg(""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttrPair(projectRes, "agent_work.research_label_id", "flightdeck_label.research_next", "id"),
			},
		},
	})
	if env.live() {
		return
	}
	writes := agentWorkRequests(env.fake.RequestsMatching("PATCH", "/api/v1/projects/"))
	if len(writes) != 4 {
		t.Fatalf("expected four agent-work PATCHes, got %d", len(writes))
	}
	for i, want := range []bool{true, true, false, false} {
		if _, sent := agentWorkBody(t, writes[i])["research_label_id"]; sent != want {
			t.Errorf("write %d: research_label_id sent = %t, want %t: %v", i+1, sent, want, agentWorkBody(t, writes[i]))
		}
	}
	if last := agentWorkBody(t, writes[2]); len(last) != 1 || last["max_in_progress"] != float64(2) {
		t.Errorf("the last write must send max_in_progress only, got %v", last)
	}
}

// Flightdeck's refusals of a research label land on the attribute: a label
// that isn't the project's, and one that is the agent label. A clash the
// configuration can see fails the plan; one with a stored label is refused
// by the apply, against whichever label the configuration changed. On create
// a refusal is a warning, so the project isn't tainted.
func TestProjectAgentWork_researchLabelRefusalsLandOnTheAttribute(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	identifier, other := randIdentifier(), randIdentifier()
	foreign := func() string {
		return env.providerConfig() + otherProjectConfig(other) + fmt.Sprintf(`
resource "flightdeck_project" "test" {
  identifier = %q
  name       = "Research label"
  agent_work = {
    research_label_id = flightdeck_label.foreign.id
  }
}
`, identifier)
	}
	cfg := func(body string) string { return researchLabelConfig(env, identifier, other, body) }
	notThisProjects := regexMust(`(?s)Flightdeck refused the research label.*Research\s+label\s+must\s+be\s+a\s+label\s+in\s+this\s+project`)
	sameAsAgent := regexMust(`(?s)Flightdeck refused the research label.*Research\s+label\s+must\s+be\s+different\s+from\s+the\s+agent\s+label`)
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Created with another project's label: a warning, not an error.
				Config:             foreign(),
				ExpectNonEmptyPlan: true,
			},
			{
				// The same, as an update: an error.
				Config:      foreign(),
				ExpectError: notThisProjects,
			},
			{Config: cfg(`  agent_work = {
    label_id = flightdeck_label.agent.id
  }`)},
			{
				// The stored agent label as the research label.
				Config: cfg(`  agent_work = {
    research_label_id = flightdeck_label.agent.id
  }`),
				ExpectError: sameAsAgent,
			},
			{
				Config: cfg(`  agent_work = {
    research_label_id = flightdeck_label.research.id
  }`),
				Check: resource.TestCheckResourceAttrPair(projectRes, "agent_work.research_label_id", "flightdeck_label.research", "id"),
			},
			{
				// The stored research label as the agent label.
				Config: cfg(`  agent_work = {
    label_id = flightdeck_label.research.id
  }`),
				ExpectError: sameAsAgent,
			},
			{
				// Both in the configuration: the plan sees it.
				Config: cfg(`  agent_work = {
    label_id          = flightdeck_label.agent.id
    research_label_id = flightdeck_label.agent.id
  }`),
				PlanOnly:    true,
				ExpectError: regexMust(`The research label must be a different label`),
			},
		},
	})
	at := func(name string) *tftypes.AttributePath {
		return tftypes.NewAttributePath().WithAttributeName("agent_work").WithAttributeName(name)
	}
	cases := []struct {
		what     string
		severity tfprotov6.DiagnosticSeverity
		summary  string
		path     *tftypes.AttributePath
		detail   string
	}{
		{"create, another project's label", tfprotov6.DiagnosticSeverityWarning, "Agent work settings were not saved",
			at("research_label_id"), "Research label must be a label in this project"},
		{"update, another project's label", tfprotov6.DiagnosticSeverityError, "Flightdeck refused the research label",
			at("research_label_id"), "Research label must be a label in this project"},
		{"the stored agent label as the research label", tfprotov6.DiagnosticSeverityError, "Flightdeck refused the research label",
			at("research_label_id"), "Research label must be different from the agent label"},
		{"the stored research label as the agent label", tfprotov6.DiagnosticSeverityError, "Flightdeck refused the research label",
			at("label_id"), "Research label must be different from the agent label"},
	}
	for _, tc := range cases {
		if !diagnosticAt(recorded.bySeverity(tc.severity), tc.summary, tc.path, tc.detail) {
			t.Errorf("%s: no %q at %s saying %q; recorded: %s", tc.what, tc.summary, tc.path, tc.detail,
				describeWithPaths(recorded.bySeverity(tc.severity)))
		}
	}
}

// A research label changed or cleared on the settings page shows in the next
// plan when the configuration sets one, and is set back. One chosen there
// while the configuration sets none is adopted, and plans nothing.
func TestProjectAgentWork_researchLabelDrift(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id, research, next string
	pid := func() int64 { return mustInt64(t, id) }
	label := func(s *string) *int64 { v := mustInt64(t, *s); return &v }
	configured := researchLabelConfig(env, identifier, "", `  agent_work = {
    research_label_id = flightdeck_label.research.id
  }`)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: projectConfig(env, identifier, `  name = "Research label"`), Check: captureAttr(projectRes, "id", &id)},
			{
				Config: configured,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr("flightdeck_label.research", "id", &research),
					captureAttr("flightdeck_label.research_next", "id", &next),
				),
			},
			{
				// Changed on the settings page.
				PreConfig: func() {
					env.fake.SetAgentWorkOutOfBand(pid(), func(row *flightdecktest.AgentWorkSetting) { row.ResearchLabelID = label(&next) })
				},
				Config: configured,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.TestCheckResourceAttrPair(projectRes, "agent_work.research_label_id", "flightdeck_label.research", "id"),
			},
			{
				// Cleared on the settings page.
				PreConfig: func() {
					env.fake.SetAgentWorkOutOfBand(pid(), func(row *flightdecktest.AgentWorkSetting) { row.ResearchLabelID = nil })
				},
				Config: configured,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(projectRes, "agent_work.research_label_id", "flightdeck_label.research", "id"),
					resource.TestCheckResourceAttrSet(projectRes, "agent_work.research_label_chosen_at"),
				),
			},
			{
				// Chosen on the settings page while the configuration sets none.
				PreConfig: func() {
					env.fake.SetAgentWorkOutOfBand(pid(), func(row *flightdecktest.AgentWorkSetting) { row.ResearchLabelID = label(&next) })
				},
				Config: researchLabelConfig(env, identifier, "", ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(projectRes, "agent_work.research_label_id", "flightdeck_label.research_next", "id"),
					// Checked here, before the test's destroy deletes the labels
					// (and with them, as the foreign key does, the reference).
					func(*terraform.State) error {
						got := env.fake.AgentWorkOf(pid())
						if got == nil || got.ResearchLabelID == nil || strconv.FormatInt(*got.ResearchLabelID, 10) != next {
							return fmt.Errorf("the label chosen on the settings page was not kept: %+v", got)
						}
						return nil
					},
				),
			},
		},
	})
}

// no_research_label is reported like any other blocker while agent work is
// on, and goes once a research label is chosen. With research the only kind
// ticked, neither the agent label nor a repository is needed, so neither
// no_label nor no_github_repo is listed.
func TestProjectAgentWork_noResearchLabelBlockerIsWarned(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: projectConfig(env, identifier, `  name = "Research label"`), Check: captureAttr(projectRes, "id", &id)},
			{
				// Turned on for research on the settings page; the refresh reports
				// it. Nothing about the project changes, so nothing applies to it.
				PreConfig: func() {
					env.fake.SetAgentWorkOutOfBand(mustInt64(t, id), func(row *flightdecktest.AgentWorkSetting) {
						row.Enabled = true
						row.Kinds = []string{"research"}
					})
				},
				Config: researchLabelConfig(env, identifier, "", ""),
			},
			{
				// Choosing the research label clears it.
				Config: researchLabelConfig(env, identifier, "", `  agent_work = {
    research_label_id = flightdeck_label.research.id
  }`),
				Check: resource.TestCheckResourceAttrPair(projectRes, "agent_work.research_label_id", "flightdeck_label.research", "id"),
			},
		},
	})
	summary := "Agent work cannot start on project " + identifier + " right now: "
	if !anyDiagnostic(recorded.readWarnings(), summary+"no_research_label", "No research label is chosen.") {
		t.Fatalf("no no_research_label blocker warning on refresh; refresh warnings: %s", describe(recorded.readWarnings()))
	}
	for _, code := range []string{"no_label", "no_github_repo"} {
		if anyDiagnostic(recorded.readWarnings(), summary+code) {
			t.Errorf("%s listed although research is the only kind ticked: %s", code, describe(recorded.readWarnings()))
		}
	}
	// The only apply to the project is the one that chose the label. It still
	// reports what blocks agent work (the fake is never connected to the
	// pool), so the missing no_research_label means it went.
	applied := recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning)
	if !anyDiagnostic(applied, summary+"pool_not_connected") {
		t.Fatalf("the apply reported no blockers at all, so it proves nothing: %s", describe(applied))
	}
	if anyDiagnostic(applied, summary+"no_research_label") {
		t.Fatalf("no_research_label still reported after the apply that chose the label: %s", describe(applied))
	}
}

// A label deleted outside Terraform takes the agent work reference with it:
// Flightdeck's foreign keys set label_id and research_label_id to null, and
// the read reports the chosen times as null too. The refresh sees that, and
// the plan makes the labels again and points both references at them.
func TestProjectAgentWork_deletedLabelIsReadAsNullAndSetBack(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	identifier := randIdentifier()
	var id, agentID, researchID string
	config := researchLabelConfig(env, identifier, "", `  agent_work = {
    label_id          = flightdeck_label.agent.id
    research_label_id = flightdeck_label.research.id
  }`)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: projectConfig(env, identifier, `  name = "Research label"`), Check: captureAttr(projectRes, "id", &id)},
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr("flightdeck_label.agent", "id", &agentID),
					captureAttr("flightdeck_label.research", "id", &researchID),
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "1"),
				),
			},
			{
				// Both labels are deleted outside Terraform.
				PreConfig: func() {
					c, err := client.New(env.endpoint, env.token)
					if err != nil {
						t.Fatal(err)
					}
					ctx := context.Background()
					for _, labelID := range []string{agentID, researchID} {
						l, err := c.GetLabel(ctx, mustInt64(t, labelID))
						if err != nil {
							t.Fatalf("reading label %s: %v", labelID, err)
						}
						if err := c.DeleteLabel(ctx, l.ID, l.LockVersion); err != nil {
							t.Fatalf("deleting label %s: %v", labelID, err)
						}
					}
					if env.live() {
						return
					}
					// Like the foreign key: the ids go, and nothing else moves.
					row := env.fake.AgentWorkOf(mustInt64(t, id))
					if row == nil || row.LabelID != nil || row.ResearchLabelID != nil || row.LockVersion != 1 {
						t.Fatalf("after the deletes the settings are %+v, want both ids null at lock_version 1", row)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("flightdeck_label.agent", plancheck.ResourceActionCreate),
						plancheck.ExpectResourceAction("flightdeck_label.research", plancheck.ResourceActionCreate),
						plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate),
						expectPriorAgentWork{key: "label_id", want: nil},
						expectPriorAgentWork{key: "research_label_id", want: nil},
						expectPriorAgentWork{key: "label_chosen_at", want: nil},
						expectPriorAgentWork{key: "research_label_chosen_at", want: nil},
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(projectRes, "agent_work.label_id", "flightdeck_label.agent", "id"),
					resource.TestCheckResourceAttrPair(projectRes, "agent_work.research_label_id", "flightdeck_label.research", "id"),
					resource.TestCheckResourceAttrSet(projectRes, "agent_work.label_chosen_at"),
					resource.TestCheckResourceAttrSet(projectRes, "agent_work.research_label_chosen_at"),
				),
			},
		},
	})
}

// expectPriorAgentWork checks the value an agent_work attribute of the project
// has before the planned change, which is what the refresh read.
type expectPriorAgentWork struct {
	key  string
	want any
}

func (e expectPriorAgentWork) CheckPlan(_ context.Context, req plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	for _, rc := range req.Plan.ResourceChanges {
		if rc.Address != projectRes {
			continue
		}
		before, _ := rc.Change.Before.(map[string]any)
		block, ok := before["agent_work"].(map[string]any)
		if !ok {
			resp.Error = fmt.Errorf("%s: the planned change starts from no agent_work block", projectRes)
			return
		}
		got, present := block[e.key]
		if !present || got != e.want {
			resp.Error = fmt.Errorf("%s: planned change starts from agent_work.%s = %v (present: %t), want %v",
				projectRes, e.key, got, present, e.want)
		}
		return
	}
	resp.Error = fmt.Errorf("%s is not in the plan", projectRes)
}

// Flightdeck stores kinds as sent and counts a reordered list as a change, as
// the fake does. So the provider sends kinds in the API's order (the order the
// settings page saves them in), and leaves them out of a write that doesn't
// change the set. Otherwise an update to anything else would rewrite the
// stored list in another order, move the settings' lock_version the plan said
// would stay, and fail with "Provider produced inconsistent result".
func TestProjectAgentWork_kindsOrderNeverCountsAsAChange(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	pid := func() int64 { return mustInt64(t, id) }
	config := func(name string) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  agent_work = {
    kinds = ["fix-error", "implement-work-item"]
  }`, name))
	}
	storedKinds := func(want ...string) resource.TestCheckFunc {
		return func(*terraform.State) error {
			got := env.fake.AgentWorkOf(pid())
			if got == nil || strings.Join(got.Kinds, ",") != strings.Join(want, ",") {
				return fmt.Errorf("stored kinds = %+v, want %v", got, want)
			}
			return nil
		}
	}
	// outOfBand saves the settings the way another client would: kinds in the
	// given order, and a budget change, which moves lock_version.
	outOfBand := func(budget float64, kinds ...string) func() {
		return func() {
			env.fake.SetAgentWorkOutOfBand(pid(), func(row *flightdecktest.AgentWorkSetting) {
				row.Kinds = kinds
				row.DailyBudgetUSD = budget
			})
		}
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Sent in the API's order, whatever order the configuration
				// lists them in, so a later save of the page changes nothing.
				Config: config("Kinds"),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					storedKinds("implement-work-item", "fix-error"),
				),
			},
			{
				// Another client stored the same kinds in name order. Renaming
				// the project must not resend them.
				PreConfig: outOfBand(20, "fix-error", "implement-work-item"),
				Config:    config("Kinds renamed"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "2"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.daily_budget_usd", "20"),
					storedKinds("fix-error", "implement-work-item"),
				),
			},
			{
				// The settings page saved them in its own order.
				PreConfig: outOfBand(30, "implement-work-item", "fix-error"),
				Config:    config("Kinds renamed again"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.lock_version", "3"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.daily_budget_usd", "30"),
					storedKinds("implement-work-item", "fix-error"),
				),
			},
		},
	})
}

// State is not always what Flightdeck holds. When a create's settings are
// refused, state keeps the planned kinds while Flightdeck holds none, and a
// plan made with -refresh=false never looks. The next apply must still send
// kinds, because the server doesn't hold them, or it saves the other settings
// and fails with "Provider produced inconsistent result" on kinds.
func TestProjectAgentWork_kindsRefusedOnCreateAreSentWithoutARefresh(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	identifier := randIdentifier()
	var id string
	config := func(budget string) string {
		return projectConfig(env, identifier, `
  name = "Refused kinds"
  agent_work = {
    kinds        = ["research"]
    task_max_usd = 20
`+budget+`
  }`)
	}
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		// Every plan is made with -refresh=false, as the reviewer's apply was.
		AdditionalCLIOptions: &resource.AdditionalCLIOptions{Plan: resource.PlanOptions{NoRefresh: true}},
		Steps: []resource.TestStep{
			{
				// A task may not cost more than the stored daily budget (10), so
				// Flightdeck refuses every setting here, kinds included.
				Config: config(""),
				Check:  captureAttr(projectRes, "id", &id),
			},
			{
				Config: config("    daily_budget_usd = 50"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.kinds.#", "1"),
					resource.TestCheckTypeSetElemAttr(projectRes, "agent_work.kinds.*", "research"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.enabled", "false"),
					func(*terraform.State) error {
						c, err := client.New(env.endpoint, env.token)
						if err != nil {
							return err
						}
						aw, err := c.GetAgentWork(context.Background(), mustInt64(t, id))
						if err != nil {
							return err
						}
						if strings.Join(aw.Kinds, ",") != "research" || aw.TaskMaxUSD != 20 || aw.DailyBudgetUSD != 50 {
							return fmt.Errorf("Flightdeck holds kinds %v, task_max_usd %v, daily_budget_usd %v; want [research], 20, 50",
								aw.Kinds, aw.TaskMaxUSD, aw.DailyBudgetUSD)
						}
						return nil
					},
				),
			},
		},
	})
	// The case only means something if the create was refused.
	if !anyDiagnostic(recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning), "Agent work settings were not saved") {
		t.Fatalf("the create's settings were not refused; apply warnings: %s", describe(recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning)))
	}
}

// The read that decides whether kinds can be left out never replaces the
// version the write pins. When the settings moved on between the plan and the
// apply, the write still meets the usual stale-settings error rather than
// handing back settings the plan didn't promise.
func TestProjectAgentWork_unchangedKindsStillMeetAStaleVersion(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	config := func(name string) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  agent_work = {
    kinds = ["implement-work-item"]
  }`, name))
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config("Stale kinds"), Check: captureAttr(projectRes, "id", &id)},
			{
				// Someone saves the settings page after the plan, while the
				// project itself is being written.
				PreConfig: func() {
					pid := mustInt64(t, id)
					env.fake.OnNextRequest("PATCH", fmt.Sprintf("/api/v1/projects/%d", pid), func() {
						env.fake.SetAgentWorkOutOfBand(pid, func(row *flightdecktest.AgentWorkSetting) { row.QueueMinutes = 120 })
					})
				},
				Config:      config("Stale kinds renamed"),
				ExpectError: regexMust(`(?s)agent work settings modified outside of Terraform.*lock_version 1, the server now has 2`),
			},
		},
	})
}

// The kinds validator is the schema's own: each kind Flightdeck knows passes,
// together or alone, and anything else fails naming the valid ones.
func TestAgentWorkKinds_validation(t *testing.T) {
	ctx := context.Background()
	block, ok := agentWorkSchema().(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("agent_work is not a single nested attribute")
	}
	kindsAttr, ok := block.Attributes["kinds"].(schema.SetAttribute)
	if !ok {
		t.Fatal("agent_work.kinds is not a set attribute")
	}
	validate := func(kinds ...string) diag.Diagnostics {
		t.Helper()
		set, d := types.SetValueFrom(ctx, types.StringType, kinds)
		if d.HasError() {
			t.Fatalf("building the set: %v", d)
		}
		resp := &validator.SetResponse{}
		for _, v := range kindsAttr.Validators {
			v.ValidateSet(ctx, validator.SetRequest{Path: path.Root("agent_work").AtName("kinds"), ConfigValue: set}, resp)
		}
		return resp.Diagnostics
	}
	for _, kinds := range [][]string{
		{"implement-work-item"}, {"fix-error"}, {"research"},
		{"implement-work-item", "fix-error", "research"}, {},
	} {
		if diags := validate(kinds...); diags.HasError() {
			t.Errorf("%v refused: %v", kinds, diags.Errors())
		}
	}
	diags := validate("research", "fix-ci")
	if !diags.HasError() {
		t.Fatal("fix-ci passed validation")
	}
	for _, kind := range []string{"implement-work-item", "fix-error", "research"} {
		if !strings.Contains(diags.Errors()[0].Detail(), `"`+kind+`"`) {
			t.Errorf("the refusal of fix-ci does not name %s: %s", kind, diags.Errors()[0].Detail())
		}
	}
}

// agentWorkRefusedAt points a refusal about the research label at the
// attribute the write sent, and leaves every other refusal on the block.
func TestAgentWorkRefusedAt(t *testing.T) {
	block := path.Root("agent_work")
	refusal := func(code, msg string) error {
		return &client.Error{Method: "PATCH", Path: "/projects/4/agent-work", Status: 422, Code: code, Message: msg}
	}
	notThisProjects := refusal(client.CodeValidationFailed, "Research label must be a label in this project")
	clash := refusal(client.CodeValidationFailed, "Research label must be different from the agent label")
	cases := []struct {
		name string
		sent client.Fields
		err  error
		want path.Path
		ok   bool
	}{
		{"not this project's, sent", client.Fields{"research_label_id": int64(9)}, notThisProjects, block.AtName("research_label_id"), true},
		{"a clash, research label sent", client.Fields{"research_label_id": int64(9), "label_id": int64(9)}, clash, block.AtName("research_label_id"), true},
		{"a clash, only the agent label sent", client.Fields{"label_id": int64(9), "max_in_progress": int64(2)}, clash, block.AtName("label_id"), true},
		{"a clash, neither sent", client.Fields{"max_in_progress": int64(2)}, clash, block, false},
		// An older Flightdeck lists the agent label's key among the settable
		// ones; that is not a refusal of the agent label.
		{"the key refused by an older Flightdeck", client.Fields{"research_label_id": int64(9), "label_id": int64(8)},
			refusal(client.CodeInvalidAttribute, "unknown key: research_label_id (settable: enabled, kinds, label_id, "+
				"accept_machine_labels, agent_account_id, base_ref, max_in_progress, daily_budget_usd, task_max_usd, "+
				"task_max_minutes, queue_minutes, runbook)"), block.AtName("research_label_id"), true},
		{"another setting refused", client.Fields{"research_label_id": int64(9), "task_max_usd": 20.0},
			refusal(client.CodeValidationFailed, "Most a task may cost can't be more than the daily budget"), block, false},
		{"the agent label refused", client.Fields{"label_id": int64(9)},
			refusal(client.CodeValidationFailed, "Agent label must be a label in this project"), block, false},
		{"both labels refused", client.Fields{"label_id": int64(9), "research_label_id": int64(10)},
			refusal(client.CodeValidationFailed, "Agent label must be a label in this project and Research label must be a label in this project"),
			block, false},
		{"a clash named in the research label's words", client.Fields{"label_id": int64(9), "research_label_id": int64(9)},
			refusal(client.CodeValidationFailed, "Agent label must be a label in this project and Research label must be different from the agent label"),
			block, false},
		{"not a 422", client.Fields{"research_label_id": int64(9)},
			&client.Error{Status: 409, Code: client.CodeStaleObject, Message: "Research label changed"}, block, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := agentWorkRefusedAt(tc.sent, tc.err)
			if ok != tc.ok || !got.Equal(tc.want) {
				t.Fatalf("got %s (%t), want %s (%t)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// diagnosticAt reports whether diags holds one with this summary, reported
// against the attribute at, whose detail contains every one of details.
func diagnosticAt(diags []*tfprotov6.Diagnostic, summary string, at *tftypes.AttributePath, details ...string) bool {
	for _, d := range diags {
		if d.Summary != summary || d.Attribute == nil || !d.Attribute.Equal(at) {
			continue
		}
		all := true
		for _, s := range details {
			all = all && strings.Contains(d.Detail, s)
		}
		if all {
			return true
		}
	}
	return false
}

// describeWithPaths is describe with each diagnostic's attribute path.
func describeWithPaths(diags []*tfprotov6.Diagnostic) string {
	parts := make([]string, 0, len(diags))
	for _, d := range diags {
		parts = append(parts, fmt.Sprintf("[%s at %v] %s", d.Summary, d.Attribute, d.Detail))
	}
	return strings.Join(parts, "\n")
}

// AgentWorkSettingKeys documents the endpoint's writable set, and
// agentWorkFields writes its own list by hand; this holds them together.
func TestAgentWorkFields_writePathMatchesSettingKeys(t *testing.T) {
	ctx := context.Background()
	kinds, _ := types.SetValueFrom(ctx, types.StringType, []string{"implement-work-item"})
	fields := mustAgentWorkFields(t, agentWorkModel{
		Enabled: types.BoolValue(true), Kinds: kinds, LabelID: types.Int64Value(7),
		LabelChosenAt: types.StringValue("2026-10-01T00:00:00Z"), AcceptMachineLabels: types.BoolValue(false),
		ResearchLabelID: types.Int64Value(9), ResearchLabelChosenAt: types.StringValue("2026-10-02T00:00:00Z"),
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
	for _, k := range []string{"label_chosen_at", "research_label_chosen_at", "lock_version", "blockers", "project_id"} {
		if _, sent := fields[k]; sent {
			t.Errorf("agentWorkFields sent the read-only %q", k)
		}
	}
}

// The rule the block exists for: only configured settings are sent, and an
// unset label_id, research_label_id or agent_account_id is left out, never
// sent as null, because the API reads a null for any of them as "clear it".
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
			name: "references not known until apply are left out too",
			model: agentWorkModel{LabelID: types.Int64Unknown(), ResearchLabelID: types.Int64Unknown(),
				AgentAccountID: types.Int64Unknown(), BaseRef: types.StringValue("develop")},
			want: map[string]any{"base_ref": "develop"},
		},
		{
			name:  "configured references are sent",
			model: agentWorkModel{LabelID: types.Int64Value(11), ResearchLabelID: types.Int64Value(13), AgentAccountID: types.Int64Value(12)},
			want:  map[string]any{"label_id": int64(11), "research_label_id": int64(13), "agent_account_id": int64(12)},
		},
		{
			name:  "a research label alone is sent alone",
			model: agentWorkModel{ResearchLabelID: types.Int64Value(13)},
			want:  map[string]any{"research_label_id": int64(13)},
		},
		{
			name: "the research label's chosen time is never sent",
			model: agentWorkModel{ResearchLabelChosenAt: types.StringValue("2026-10-02T00:00:00Z"),
				LabelChosenAt: types.StringValue("2026-10-01T00:00:00Z"), MaxInProgress: types.Int64Value(1)},
			want: map[string]any{"max_in_progress": int64(1)},
		},
		{
			name:  "false and an empty kinds list are values",
			model: agentWorkModel{Enabled: types.BoolValue(false), AcceptMachineLabels: types.BoolValue(false), Kinds: types.SetValueMust(types.StringType, nil)},
			want:  map[string]any{"enabled": false, "accept_machine_labels": false, "kinds": []string{}},
		},
		{
			name:  "kinds are sent in the API's order",
			model: agentWorkModel{Kinds: types.SetValueMust(types.StringType, []attr.Value{types.StringValue("research"), types.StringValue("fix-error"), types.StringValue("implement-work-item")})},
			want:  map[string]any{"kinds": []string{"implement-work-item", "fix-error", "research"}},
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
			for _, key := range []string{"label_id", "research_label_id", "agent_account_id"} {
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
		{Code: "no_research_label", Message: "No research label is chosen."},
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
		"Agent work cannot start on project APP right now: no_research_label",
		"Agent work cannot start on project APP right now: no_label (2)",
	}
	if strings.Join(summaries, "|") != strings.Join(want, "|") {
		t.Fatalf("summaries = %v, want %v (each distinct, so Terraform does not fold them)", summaries, want)
	}
	if !strings.Contains(on.Warnings()[0].Detail(), "No agent label is chosen.") {
		t.Fatalf("the warning must carry the API's message: %s", on.Warnings()[0].Detail())
	}
	if !strings.Contains(on.Warnings()[2].Detail(), "No research label is chosen.") {
		t.Fatalf("the no_research_label warning must carry the API's message: %s", on.Warnings()[2].Detail())
	}
}

func TestPlanAgentWorkComputed(t *testing.T) {
	ctx := context.Background()
	prior := &agentWorkModel{Enabled: types.BoolValue(false), LabelID: types.Int64Value(7), MaxInProgress: types.Int64Value(1),
		DailyBudgetUSD: types.Float64Value(12.34), LabelChosenAt: types.StringValue("2026-10-01T00:00:00Z"),
		ResearchLabelID: types.Int64Value(9), ResearchLabelChosenAt: types.StringValue("2026-10-02T00:00:00Z"), LockVersion: types.Int64Value(3)}
	cases := []struct {
		name                                 string
		plan                                 agentWorkModel
		wantVersion, wantLabel, wantResearch bool // whether each keeps its prior value
	}{
		{name: "nothing changes", plan: *prior, wantVersion: true, wantLabel: true, wantResearch: true},
		{name: "a setting changes", plan: with(*prior, func(m *agentWorkModel) { m.MaxInProgress = types.Int64Value(2) }), wantLabel: true, wantResearch: true},
		{name: "the label changes", plan: with(*prior, func(m *agentWorkModel) { m.LabelID = types.Int64Value(8) }), wantResearch: true},
		{name: "the label is not known yet", plan: with(*prior, func(m *agentWorkModel) { m.LabelID = types.Int64Unknown() }), wantResearch: true},
		{name: "the research label changes", plan: with(*prior, func(m *agentWorkModel) { m.ResearchLabelID = types.Int64Value(10) }), wantLabel: true},
		{name: "the research label is not known yet", plan: with(*prior, func(m *agentWorkModel) { m.ResearchLabelID = types.Int64Unknown() }), wantLabel: true},
		// A configured amount carries more precision than the API's answer;
		// the same float64 is the same setting.
		{name: "money equal as sent", plan: with(*prior, func(m *agentWorkModel) { m.DailyBudgetUSD = types.Float64Value(12.34) }), wantVersion: true, wantLabel: true, wantResearch: true},
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
			if tc.wantResearch != !got.ResearchLabelChosenAt.IsUnknown() {
				t.Errorf("research_label_chosen_at = %v, want kept=%t", got.ResearchLabelChosenAt, tc.wantResearch)
			}
		})
	}
	// On create there is no prior: all three are unknown.
	var diags diag.Diagnostics
	out := planAgentWorkComputed(ctx, types.ObjectNull(agentWorkAttrTypes), agentWorkObj(t, prior), &diags)
	var got agentWorkModel
	diags.Append(out.As(ctx, &got, objectAsOptions)...)
	if !got.LockVersion.IsUnknown() || !got.LabelChosenAt.IsUnknown() || !got.ResearchLabelChosenAt.IsUnknown() {
		t.Errorf("on create all three must be unknown, got %v, %v and %v", got.LockVersion, got.LabelChosenAt, got.ResearchLabelChosenAt)
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

	// The same label for both kinds of work fails the plan, on the research
	// label, which is where Flightdeck puts it too.
	diags = nil
	validateAgentWorkConfig(ctx, agentWorkObj(t, &agentWorkModel{LabelID: types.Int64Value(7), ResearchLabelID: types.Int64Value(7)}), &diags)
	if !diags.HasError() {
		t.Fatal("the same label as both the agent label and the research label must fail the plan")
	}
	if withPath, ok := diags.Errors()[0].(diag.DiagnosticWithPath); !ok ||
		!withPath.Path().Equal(path.Root("agent_work").AtName("research_label_id")) {
		t.Errorf("the clash is not reported against research_label_id: %#v", diags.Errors()[0])
	}
	for name, m := range map[string]*agentWorkModel{
		"different labels":           {LabelID: types.Int64Value(7), ResearchLabelID: types.Int64Value(8)},
		"only the research label":    {ResearchLabelID: types.Int64Value(7)},
		"a label not known yet":      {LabelID: types.Int64Unknown(), ResearchLabelID: types.Int64Value(7)},
		"a research label not known": {LabelID: types.Int64Value(7), ResearchLabelID: types.Int64Unknown()},
	} {
		diags = nil
		validateAgentWorkConfig(ctx, agentWorkObj(t, m), &diags)
		if diags.HasError() {
			t.Errorf("%s: refused at plan time: %v", name, diags.Errors())
		}
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
