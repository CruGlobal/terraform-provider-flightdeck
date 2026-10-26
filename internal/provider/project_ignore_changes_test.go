package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// The project's blocks under `lifecycle { ignore_changes = [...] }`.
// Terraform applies ignore_changes to the PLAN only, never to the
// configuration it hands the apply. So a block's write decides WHETHER to send
// a setting from the configuration (a setting it leaves unset is never sent),
// but sends the PLAN's value, which is the value in state for an ignored
// setting. Each test here changes a setting the way Flightdeck's settings page
// does, makes the project update for something else (a rename), and checks
// that the apply works, the edit survives on the server, and the next plan is
// empty. Before the fix, each apply sent the configuration's value,
// overwrote the edit, and failed with "Provider produced inconsistent result".

// ignoreChanges is a lifecycle block that ignores the given paths.
func ignoreChanges(paths ...string) string {
	return fmt.Sprintf("\n  lifecycle {\n    ignore_changes = [%s]\n  }", strings.Join(paths, ", "))
}

// lastAgentWorkWrite is the body of the last agent-work PATCH, after checking
// there were want of them.
func lastAgentWorkWrite(t *testing.T, env *testEnv, want int) map[string]any {
	t.Helper()
	writes := agentWorkRequests(env.fake.RequestsMatching("PATCH", "/api/v1/projects/"))
	if len(writes) != want {
		t.Fatalf("expected %d agent-work PATCHes, got %d", want, len(writes))
	}
	return agentWorkBody(t, writes[len(writes)-1])
}

// patchesTo returns the request bodies of every PATCH to one of a project's
// settings endpoints, decoded under root.
func patchesTo(t *testing.T, env *testEnv, suffix, root string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range env.fake.RequestsMatching("PATCH", "/api/v1/projects/") {
		if !strings.HasSuffix(r.Path, suffix) {
			continue
		}
		var body map[string]map[string]any
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatalf("decoding %s: %v", r.Body, err)
		}
		out = append(out, body[root])
	}
	return out
}

// webClient is a client for changes made outside Terraform, as Flightdeck's
// settings page makes them.
func webClient(t *testing.T, env *testEnv) *client.Client {
	t.Helper()
	c, err := client.New(env.endpoint, env.token)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestProjectAgentWork_ignoreChangesKeepsAWebEdit(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	config := func(name string) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  agent_work = {
    max_in_progress = 2
  }`, name)+ignoreChanges("agent_work"))
	}
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config("Ignored"),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "2"),
				),
			},
			{
				PreConfig: func() {
					env.fake.SetAgentWorkOutOfBand(mustInt(id), func(row *flightdecktest.AgentWorkSetting) { row.MaxInProgress = 5 })
				},
				Config: config("Ignored renamed"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("agent_work").AtMapKey("max_in_progress"), knownvalue.Int64Exact(5)),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "name", "Ignored renamed"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "5"),
					func(*terraform.State) error {
						if got := env.fake.AgentWorkOf(mustInt(id)).MaxInProgress; got != 5 {
							return fmt.Errorf("the update overwrote max_in_progress set on the settings page: %d", got)
						}
						return nil
					},
				),
			},
			{Config: config("Ignored renamed"), PlanOnly: true},
		},
	})
	if last := lastAgentWorkWrite(t, env, 2); last["max_in_progress"] != float64(5) {
		t.Errorf("the rename's write must send the value in state (5), got %v", last)
	}
}

// ignore_changes naming one setting of the block: that setting keeps its web
// edit through a rename, and a configured change to another setting in the
// same apply is still sent.
//
// Terraform (1.15.2) reads an index into an object differently from a dot.
// agent_work["max_in_progress"] and auto_promote["window"] ignore the whole
// block, and auto_promote.window["start"] the whole window, so that case here
// changes nothing else in the block. auto_promote["window"]["start"] ignores
// nothing at all: a web edit is reverted with no error.
func TestProjectAgentWork_ignoreChangesOnOneSettingKeepsItsWebEdit(t *testing.T) {
	for _, tc := range []struct {
		spelling string
		queue    int
	}{
		{`agent_work.max_in_progress`, 90},
		{`agent_work["max_in_progress"]`, 60},
	} {
		t.Run(tc.spelling, func(t *testing.T) {
			env := newTestEnv(t, "agent_work")
			env.requireFake(t)
			identifier := randIdentifier()
			config := func(name string, queue int) string {
				return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  agent_work = {
    max_in_progress = 2
    queue_minutes   = %d
  }`, name, queue)+ignoreChanges(tc.spelling))
			}
			var id string
			changed := config("One setting ignored renamed", tc.queue)
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{Config: config("One setting ignored", 60), Check: captureAttr(projectRes, "id", &id)},
					{
						PreConfig: func() {
							env.fake.SetAgentWorkOutOfBand(mustInt(id), func(row *flightdecktest.AgentWorkSetting) { row.MaxInProgress = 5 })
						},
						Config: changed,
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(projectRes, "name", "One setting ignored renamed"),
							resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "5"),
							resource.TestCheckResourceAttr(projectRes, "agent_work.queue_minutes", fmt.Sprint(tc.queue)),
							func(*terraform.State) error {
								row := env.fake.AgentWorkOf(mustInt(id))
								if row.MaxInProgress != 5 || row.QueueMinutes != int64(tc.queue) {
									return fmt.Errorf("want max_in_progress 5 (the web edit) and queue_minutes %d, got %d and %d",
										tc.queue, row.MaxInProgress, row.QueueMinutes)
								}
								return nil
							},
						),
					},
					{Config: changed, PlanOnly: true},
				},
			})
			last := lastAgentWorkWrite(t, env, 2)
			if last["max_in_progress"] != float64(5) || last["queue_minutes"] != float64(tc.queue) {
				t.Errorf("the second write must send max_in_progress 5 (state) and queue_minutes %d, got %v", tc.queue, last)
			}
		})
	}
}

// The newer settings, models among them, follow the same rule.
func TestProjectAgentWork_ignoreChangesKeepsAWebEditOfNewerSettings(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	config := func(name string) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  agent_work = {
    models      = { "implement-work-item" = "sonnet-5-5" }
    send_errors = false
  }`, name)+ignoreChanges("agent_work"))
	}
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:            config("Newer settings"),
				Check:             captureAttr(projectRes, "id", &id),
				ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, map[string]string{"implement-work-item": "sonnet-5-5"})},
			},
			{
				PreConfig: func() {
					env.fake.SetAgentWorkOutOfBand(mustInt(id), func(row *flightdecktest.AgentWorkSetting) {
						row.Models = map[string]string{"implement-work-item": "opus-5-5"}
						row.SendErrors = true
					})
				},
				Config:            config("Newer settings renamed"),
				ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, map[string]string{"implement-work-item": "opus-5-5"})},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.send_errors", "true"),
					func(*terraform.State) error {
						row := env.fake.AgentWorkOf(mustInt(id))
						if row.Models["implement-work-item"] != "opus-5-5" || !row.SendErrors {
							return fmt.Errorf("the update overwrote the settings page's edit: models %v, send_errors %t", row.Models, row.SendErrors)
						}
						return nil
					},
				),
			},
			{Config: config("Newer settings renamed"), PlanOnly: true},
		},
	})
	last := lastAgentWorkWrite(t, env, 2)
	if models, _ := last["models"].(map[string]any); models["implement-work-item"] != "opus-5-5" || last["send_errors"] != true {
		t.Errorf("the rename's write must send the values in state, got %v", last)
	}
}

// label_id, which the API clears on a null, keeps a label chosen on the
// settings page, and its label_chosen_at with it.
func TestProjectAgentWork_ignoreChangesKeepsAWebEditedLabel(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	project := func(name, lifecycle string) string {
		return env.providerConfig() + fmt.Sprintf(`
data "flightdeck_project" "self" {
  identifier = %q
}

resource "flightdeck_label" "agent" {
  project_id = data.flightdeck_project.self.id
  name       = "agent-ready"
}

resource "flightdeck_label" "other" {
  project_id = data.flightdeck_project.self.id
  name       = "agent-other"
}

resource "flightdeck_project" "test" {
  identifier = %q
  name       = %q
  agent_work = {
    label_id = flightdeck_label.agent.id
  }%s
}
`, identifier, identifier, name, lifecycle)
	}
	var id, other string
	var chosenAt time.Time
	ignored := project("Label renamed", ignoreChanges("agent_work"))
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// The project first, so the data source can find it.
				Config: projectConfig(env, identifier, `  name = "Label"`),
				Check:  captureAttr(projectRes, "id", &id),
			},
			{
				Config: project("Label", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(projectRes, "agent_work.label_id", "flightdeck_label.agent", "id"),
					captureAttr("flightdeck_label.other", "id", &other),
				),
			},
			{
				PreConfig: func() {
					env.fake.SetAgentWorkOutOfBand(mustInt(id), func(row *flightdecktest.AgentWorkSetting) {
						label := mustInt(other)
						row.LabelID = &label
					})
					chosenAt = *env.fake.AgentWorkOf(mustInt(id)).LabelChosenAt
				},
				Config: ignored,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(projectRes, "agent_work.label_id", "flightdeck_label.other", "id"),
					func(s *terraform.State) error {
						row := env.fake.AgentWorkOf(mustInt(id))
						if row.LabelID == nil || *row.LabelID != mustInt(other) {
							return fmt.Errorf("the update overwrote the label chosen on the settings page: %v", row.LabelID)
						}
						if !row.LabelChosenAt.Equal(chosenAt) {
							return fmt.Errorf("label_chosen_at moved from %s to %s, so the label was changed", chosenAt, row.LabelChosenAt)
						}
						return nil
					},
				),
			},
			{Config: ignored, PlanOnly: true},
		},
	})
	if last := lastAgentWorkWrite(t, env, 2); last["label_id"] != float64(mustInt(other)) {
		t.Errorf("the rename's write must send the label in state (%s), got %v", other, last)
	}
}

// A configured newer setting ignored while state holds it as null (read from
// a Flightdeck older than the setting) is not sent, so the update works. With
// Flightdeck upgraded to the setting between the plan and the apply, the apply
// keeps the plan's null rather than fail, and the next refresh adopts what
// Flightdeck reports without planning anything.
func TestProjectAgentWork_ignoreChangesOverANullNewerSettingSurvivesAnUpgrade(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	env.fake.SetAgentWorkModelsSupported(false)
	identifier := randIdentifier()
	ignored := projectConfig(env, identifier, `
  name = "Older renamed"
  agent_work = {
    max_in_progress = 2
    models          = { "implement-work-item" = "opus-5-5" }
  }`+ignoreChanges("agent_work"))
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Older"
  agent_work = {
    max_in_progress = 2
  }`),
				Check:             captureAttr(projectRes, "id", &id),
				ConfigStateChecks: []statecheck.StateCheck{modelsNull(projectRes)},
			},
			{
				PreConfig: func() {
					env.fake.OnNextRequest("PATCH", "/api/v1/projects/"+id+"/agent-work", func() {
						env.fake.SetAgentWorkModelsSupported(true)
					})
				},
				Config:            ignored,
				ConfigStateChecks: []statecheck.StateCheck{modelsNull(projectRes)},
				Check:             resource.TestCheckResourceAttr(projectRes, "name", "Older renamed"),
			},
			{
				Config: ignored,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, map[string]string{})},
			},
		},
	})
	for i, w := range agentWorkRequests(env.fake.RequestsMatching("PATCH", "/api/v1/projects/")) {
		if _, sent := agentWorkBody(t, w)["models"]; sent {
			t.Errorf("write %d sent models, which the plan holds as null: %v", i+1, agentWorkBody(t, w))
		}
	}
}

// With the block ignored while it reads as null (here, a token that is no
// longer a workspace admin), nothing is sent: the plan holds null, so the
// rename works and leaves the settings alone.
func TestProjectAgentWork_ignoreChangesOnANullBlockSendsNothing(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	config := func(name string) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  agent_work = {
    max_in_progress = 2
  }`, name)+ignoreChanges("agent_work", "terraform_managed"))
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config("Null block")},
			{
				PreConfig: func() { env.fake.SetWorkspaceAdmin(false) },
				Config:    config("Null block renamed"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "name", "Null block renamed"),
					resource.TestCheckNoResourceAttr(projectRes, "agent_work.max_in_progress"),
				),
			},
			{Config: config("Null block renamed"), PlanOnly: true},
			{
				// An admin again, so the project can be torn down as usual.
				PreConfig: func() { env.fake.SetWorkspaceAdmin(true) },
				Config:    config("Null block renamed"),
			},
		},
	})
	lastAgentWorkWrite(t, env, 1)
}

// Without ignore_changes nothing changes: a configured setting changed on the
// settings page is planned back and sent.
func TestProjectAgentWork_withoutIgnoreChangesAWebEditIsUndone(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	config := func(name string) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  agent_work = {
    max_in_progress = 2
  }`, name))
	}
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config("Owned"), Check: captureAttr(projectRes, "id", &id)},
			{
				PreConfig: func() {
					env.fake.SetAgentWorkOutOfBand(mustInt(id), func(row *flightdecktest.AgentWorkSetting) { row.MaxInProgress = 5 })
				},
				Config: config("Owned renamed"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("agent_work").AtMapKey("max_in_progress"), knownvalue.Int64Exact(2)),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "2"),
					func(*terraform.State) error {
						if got := env.fake.AgentWorkOf(mustInt(id)).MaxInProgress; got != 2 {
							return fmt.Errorf("the configured value was not sent: %d", got)
						}
						return nil
					},
				),
			},
			{Config: config("Owned renamed"), PlanOnly: true},
		},
	})
	if last := lastAgentWorkWrite(t, env, 2); last["max_in_progress"] != float64(2) {
		t.Errorf("the write must send the configured value (2), got %v", last)
	}
}

func TestProjectSelfHealing_ignoreChangesKeepsAWebEdit(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	env.requireFake(t)
	identifier := randIdentifier()
	config := func(name string) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  self_healing = {
    bake_minutes = 30
  }`, name)+ignoreChanges("self_healing"))
	}
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config("Self-healing"), Check: captureAttr(projectRes, "id", &id)},
			{
				PreConfig: func() {
					pid := mustInt(id)
					if _, err := webClient(t, env).UpdateSelfHealing(context.Background(), pid,
						client.Fields{"bake_minutes": 45}, env.fake.Project(pid).LockVersion); err != nil {
						t.Fatal(err)
					}
				},
				Config: config("Self-healing renamed"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "name", "Self-healing renamed"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "45"),
				),
			},
			{Config: config("Self-healing renamed"), PlanOnly: true},
		},
	})
	// The create's write, the web edit, and the rename's.
	writes := patchesTo(t, env, "/self-healing", "self_healing")
	if len(writes) != 3 {
		t.Fatalf("expected 3 self-healing PATCHes, got %d: %v", len(writes), writes)
	}
	if last := writes[2]; last["bake_minutes"] != float64(45) {
		t.Errorf("the rename's write must send the value in state (45), got %v", last)
	}
}

func TestProjectAutoPromote_ignoreChangesKeepsAWebEdit(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	identifier := randIdentifier()
	config := func(name string) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  auto_promote = {
    soak_hours = 30
    window = {
      start = "09:00"
    }
  }`, name)+ignoreChanges("auto_promote"))
	}
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config("Auto-promote"), Check: captureAttr(projectRes, "id", &id)},
			{
				PreConfig: func() {
					env.fake.SetAutoPromoteOutOfBand(mustInt(id), func(m map[string]any) {
						m["soak_hours"] = int64(36)
						// The stored window is the whole window.
						m["promote_window"] = map[string]any{
							"days": []string{"mon", "tue", "wed", "thu", "fri"}, "start": "10:00", "end": "16:00",
							"time_zone": "America/New_York",
						}
					})
				},
				Config: config("Auto-promote renamed"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "name", "Auto-promote renamed"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "36"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.window.start", "10:00"),
				),
			},
			{Config: config("Auto-promote renamed"), PlanOnly: true},
		},
	})
	// The create's write and the rename's: the web edit made no request.
	writes := patchesTo(t, env, "/self-healing", "self_healing")
	if len(writes) != 2 {
		t.Fatalf("expected 2 self-healing PATCHes, got %d: %v", len(writes), writes)
	}
	last := writes[1]
	window, _ := last["promote_window"].(map[string]any)
	if last["soak_hours"] != float64(36) || window["start"] != "10:00" {
		t.Errorf("the rename's write must send the values in state (36, 10:00), got %v", last)
	}
}

func TestProjectSlackChannel_ignoreChangesKeepsAWebEdit(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t)
	identifier := randIdentifier()
	config := func(name string) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  slack_channel = {
    notifications_enabled = true
    name                  = "team-web"
  }`, name)+ignoreChanges("slack_channel"))
	}
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config("Slack"), Check: captureAttr(projectRes, "id", &id)},
			{
				PreConfig: func() {
					pid := mustInt(id)
					if _, err := webClient(t, env).UpdateSlackChannel(context.Background(), pid,
						client.Fields{"slack_notifications_enabled": false, "slack_channel_name": "team-ops"},
						env.fake.Project(pid).LockVersion); err != nil {
						t.Fatal(err)
					}
				},
				Config: config("Slack renamed"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "name", "Slack renamed"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.notifications_enabled", "false"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.name", "team-ops"),
					func(*terraform.State) error {
						if p := env.fake.Project(mustInt(id)); p.SlackChannelName != "team-ops" {
							return fmt.Errorf("the update overwrote the channel name set on the settings page: %q", p.SlackChannelName)
						}
						return nil
					},
				),
			},
			{Config: config("Slack renamed"), PlanOnly: true},
		},
	})
	// The web edit is one PATCH; Terraform's own are the create's and nothing
	// since, because the rename asks nothing of the channel that it lacks.
	if writes := patchesTo(t, env, "/slack-channel", "slack_channel"); len(writes) != 2 {
		t.Errorf("want the create's write and the web edit only, got %d: %v", len(writes), writes)
	}
}

// Dropping event_filter from the configuration leaves the categories it
// managed tracked at their current values, as a refresh does, so a write the
// same apply makes for another setting records them too. Before, that write
// recorded no categories against a plan that still held them, and the apply
// failed with "Provider produced inconsistent result".
func TestProjectSlackChannel_droppingEventFilterThenWritingKeepsItsCategories(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	identifier := randIdentifier()
	dropped := projectConfig(env, identifier, `
  name = "Drop filter"
  slack_channel = {
    notifications_enabled = false
  }`)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Drop filter"
  slack_channel = {
    notifications_enabled = true
    event_filter = {
      logged = true
    }
  }`),
			},
			{
				Config: dropped,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.notifications_enabled", "false"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.event_filter.%", "1"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.event_filter.logged", "true"),
				),
			},
			{Config: dropped, PlanOnly: true},
		},
	})
}

// plannedWhereConfigured takes the plan's value of each setting the
// configuration sets, part by part in a nested object, and null for the rest,
// including a setting the configuration sets that the plan holds as null.
func TestPlannedWhereConfigured(t *testing.T) {
	ctx := context.Background()
	config := autoPromoteObj(t, autoPromoteModel{
		SoakHours:    types.Int64Value(30),
		HeadsUpHours: types.Int64Value(2),
		Window:       windowObj(t, promoteWindowModel{Start: types.StringValue("09:00")}),
	})
	// What ignore_changes plans: the values in state, with heads_up_hours null
	// as a Flightdeck older than it reads.
	planned := autoPromoteObj(t, autoPromoteModel{
		Mode: types.StringValue("off"), SoakHours: types.Int64Value(36), FreezeDates: stringSet(nil),
		Window: windowObj(t, promoteWindowModel{Days: stringSet([]string{"mon"}), Start: types.StringValue("10:00"),
			End: types.StringValue("16:00"), TimeZone: types.StringValue("UTC")}),
	})
	want := autoPromoteObj(t, autoPromoteModel{
		SoakHours: types.Int64Value(36),
		Window:    windowObj(t, promoteWindowModel{Start: types.StringValue("10:00")}),
	})
	var diags diag.Diagnostics
	if got := plannedWhereConfigured(ctx, config, planned, &diags); diags.HasError() || !got.Equal(want) {
		t.Errorf("got %v (%v), want %v", got, diags, want)
	}
	// Nothing configured sends nothing, and neither does a block the plan
	// holds as null.
	if got := plannedWhereConfigured(ctx, types.ObjectNull(autoPromoteAttrTypes), planned, &diags); !got.IsNull() {
		t.Errorf("an unconfigured block must stay null, got %v", got)
	}
	if got := plannedWhereConfigured(ctx, config, types.ObjectNull(autoPromoteAttrTypes), &diags); !got.IsNull() {
		t.Errorf("a block planned as null must send nothing, got %v", got)
	}
	// What the plan does not know is sent from the configuration, as before.
	if got := plannedWhereConfigured(ctx, config, types.ObjectUnknown(autoPromoteAttrTypes), &diags); !got.Equal(config) {
		t.Errorf("a block planned as unknown must send the configuration, got %v", got)
	}
	partly := autoPromoteObj(t, autoPromoteModel{
		Mode: types.StringValue("off"), SoakHours: types.Int64Unknown(), HeadsUpHours: types.Int64Unknown(),
		FreezeDates: stringSet(nil), Window: types.ObjectUnknown(promoteWindowAttrTypes),
	})
	if got := plannedWhereConfigured(ctx, config, partly, &diags); !got.Equal(config) {
		t.Errorf("settings planned as unknown must be sent from the configuration, got %v", got)
	}
	// A null of a custom type keeps its type.
	slack := func(name attr.Value) types.Object {
		values := map[string]attr.Value{}
		for key, typ := range slackChannelAttrTypes {
			values[key] = nullOf(ctx, typ, &diags)
		}
		values["name"] = name
		obj, d := types.ObjectValue(slackChannelAttrTypes, values)
		if d.HasError() {
			t.Fatalf("building slack_channel: %v", d)
		}
		return obj
	}
	got := plannedWhereConfigured(ctx, slack(slackChannelNameValue{StringValue: types.StringValue("team-web")}),
		slack(slackChannelNameValue{StringValue: types.StringNull()}), &diags)
	if name, ok := got.Attributes()["name"].(slackChannelNameValue); diags.HasError() || !ok || !name.IsNull() {
		t.Errorf("want a null slackChannelNameValue, got %#v (%v)", got.Attributes()["name"], diags)
	}
}

// The other way in: no web edit, but the configuration changes an ignored
// setting in the same apply as a rename. The plan keeps the value in state,
// so that is what is sent, and the configured change is not.
func TestProjectAgentWork_ignoreChangesDropsAConfigEdit(t *testing.T) {
	env := newTestEnv(t, "agent_work")
	env.requireFake(t)
	identifier := randIdentifier()
	config := func(name string, inProgress int) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  agent_work = {
    max_in_progress = %d
  }`, name, inProgress)+ignoreChanges("agent_work.max_in_progress"))
	}
	var id string
	changed := config("Config edit renamed", 3)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config("Config edit", 2), Check: captureAttr(projectRes, "id", &id)},
			{
				Config: changed,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "name", "Config edit renamed"),
					resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "2"),
					func(*terraform.State) error {
						if got := env.fake.AgentWorkOf(mustInt(id)).MaxInProgress; got != 2 {
							return fmt.Errorf("the ignored configuration change was sent: max_in_progress is %d", got)
						}
						return nil
					},
				),
			},
			{Config: changed, PlanOnly: true},
		},
	})
	if last := lastAgentWorkWrite(t, env, 2); last["max_in_progress"] != float64(2) {
		t.Errorf("the rename's write must send the value in state (2), got %v", last)
	}
}

// ignore_changes never applies to a create, so every block is sent then,
// whether the lifecycle names the blocks or says all.
func TestProject_createUnderIgnoreChangesSendsEveryBlock(t *testing.T) {
	for _, ignored := range []string{"[agent_work, self_healing, auto_promote, slack_channel]", "all"} {
		t.Run(ignored, func(t *testing.T) {
			env := newTestEnv(t, "agent_work")
			env.requireFake(t)
			identifier := randIdentifier()
			config := projectConfig(env, identifier, `
  name = "Create ignored"
  agent_work = {
    max_in_progress = 2
    models          = { "implement-work-item" = "opus-5-5" }
  }
  self_healing = {
    bake_minutes = 31
  }
  auto_promote = {
    soak_hours = 33
    window = {
      start = "10:00"
    }
  }
  slack_channel = {
    notifications_enabled = false
    name                  = "team-web"
    event_filter = {
      logged = true
    }
  }
  lifecycle {
    ignore_changes = `+ignored+`
  }`)
			var id string
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{
						Config:            config,
						ConfigStateChecks: []statecheck.StateCheck{modelsAre(projectRes, map[string]string{"implement-work-item": "opus-5-5"})},
						Check: resource.ComposeAggregateTestCheckFunc(
							captureAttr(projectRes, "id", &id),
							resource.TestCheckResourceAttr(projectRes, "agent_work.max_in_progress", "2"),
							resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "31"),
							resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "33"),
							resource.TestCheckResourceAttr(projectRes, "auto_promote.window.start", "10:00"),
							resource.TestCheckResourceAttr(projectRes, "slack_channel.notifications_enabled", "false"),
							resource.TestCheckResourceAttr(projectRes, "slack_channel.name", "team-web"),
							resource.TestCheckResourceAttr(projectRes, "slack_channel.event_filter.logged", "true"),
							func(*terraform.State) error {
								pid := mustInt(id)
								aw, ap, p := env.fake.AgentWorkOf(pid), env.fake.AutoPromoteOf(pid), env.fake.Project(pid)
								window, _ := ap["promote_window"].(map[string]any)
								switch {
								case aw == nil || aw.MaxInProgress != 2 || aw.Models["implement-work-item"] != "opus-5-5":
									return fmt.Errorf("agent_work was not sent on create: %+v", aw)
								case ap["soak_hours"] != int64(33) || window["start"] != "10:00":
									return fmt.Errorf("auto_promote was not sent on create: %v", ap)
								case p.SlackChannelName != "team-web" || !p.SlackEventFilter["logged"]:
									return fmt.Errorf("slack_channel was not sent on create: name %q, filter %v", p.SlackChannelName, p.SlackEventFilter)
								}
								return nil
							},
						),
					},
					{Config: config, PlanOnly: true},
				},
			})
			if writes := patchesTo(t, env, "/self-healing", "self_healing"); len(writes) != 1 || writes[0]["bake_minutes"] != float64(31) {
				t.Errorf("self_healing was not sent on create: %v", writes)
			}
		})
	}
}

// ignore_changes naming one self_healing setting keeps its web edit, while a
// configured change to another setting in the same apply is sent.
func TestProjectSelfHealing_ignoreChangesOnOneSettingKeepsItsWebEdit(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	env.requireFake(t)
	identifier := randIdentifier()
	config := func(name string, cooldown int) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  self_healing = {
    bake_minutes     = 30
    cooldown_minutes = %d
  }`, name, cooldown)+ignoreChanges("self_healing.bake_minutes"))
	}
	var id string
	changed := config("One threshold renamed", 20)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config("One threshold", 10), Check: captureAttr(projectRes, "id", &id)},
			{
				PreConfig: func() {
					pid := mustInt(id)
					if _, err := webClient(t, env).UpdateSelfHealing(context.Background(), pid,
						client.Fields{"bake_minutes": 45}, env.fake.Project(pid).LockVersion); err != nil {
						t.Fatal(err)
					}
				},
				Config: changed,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "45"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.cooldown_minutes", "20"),
				),
			},
			{Config: changed, PlanOnly: true},
		},
	})
	// The create's write, the web edit, and the rename's.
	writes := patchesTo(t, env, "/self-healing", "self_healing")
	if len(writes) != 3 {
		t.Fatalf("expected 3 self-healing PATCHes, got %d: %v", len(writes), writes)
	}
	if last := writes[2]; last["bake_minutes"] != float64(45) || last["cooldown_minutes"] != float64(20) {
		t.Errorf("the rename's write must send bake_minutes 45 (state) and cooldown_minutes 20 (configuration), got %v", last)
	}
}

// The same for a part of auto_promote's nested window: window.start keeps its
// web edit, and a configured change to soak_hours is sent.
func TestProjectAutoPromote_ignoreChangesOnTheWindowStartKeepsItsWebEdit(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	identifier := randIdentifier()
	config := func(name string, soak int) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  auto_promote = {
    soak_hours = %d
    window = {
      start = "09:00"
    }
  }`, name, soak)+ignoreChanges("auto_promote.window.start"))
	}
	var id string
	changed := config("Window start renamed", 40)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config("Window start", 30), Check: captureAttr(projectRes, "id", &id)},
			{
				PreConfig: func() {
					env.fake.SetAutoPromoteOutOfBand(mustInt(id), func(m map[string]any) {
						m["soak_hours"] = int64(36)
						// The stored window is the whole window.
						m["promote_window"] = map[string]any{
							"days": []string{"mon", "tue", "wed", "thu", "fri"}, "start": "10:00", "end": "16:00",
							"time_zone": "America/New_York",
						}
					})
				},
				Config: changed,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "40"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.window.start", "10:00"),
				),
			},
			{Config: changed, PlanOnly: true},
		},
	})
	writes := patchesTo(t, env, "/self-healing", "self_healing")
	if len(writes) != 2 {
		t.Fatalf("expected 2 self-healing PATCHes, got %d: %v", len(writes), writes)
	}
	window, _ := writes[1]["promote_window"].(map[string]any)
	if writes[1]["soak_hours"] != float64(40) || window["start"] != "10:00" || len(window) != 1 {
		t.Errorf("the rename's write must send soak_hours 40 (configuration) and only window.start 10:00 (state), got %v", writes[1])
	}
}

// And for one slack_channel setting: the channel name keeps its web edit, and
// a configured change to notifications_enabled in the same apply is sent.
func TestProjectSlackChannel_ignoreChangesOnTheNameKeepsItsWebEdit(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t)
	identifier := randIdentifier()
	config := func(name string, notify bool) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  slack_channel = {
    enabled               = true
    notifications_enabled = %t
    name                  = "team-web"
  }`, name, notify)+ignoreChanges("slack_channel.name"))
	}
	var id string
	changed := config("Slack name renamed", false)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config("Slack name", true), Check: captureAttr(projectRes, "id", &id)},
			{
				PreConfig: func() {
					pid := mustInt(id)
					if _, err := webClient(t, env).UpdateSlackChannel(context.Background(), pid,
						client.Fields{"slack_channel_name": "team-ops"}, env.fake.Project(pid).LockVersion); err != nil {
						t.Fatal(err)
					}
				},
				Config: changed,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.name", "team-ops"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.notifications_enabled", "false"),
					func(*terraform.State) error {
						if p := env.fake.Project(mustInt(id)); p.SlackChannelName != "team-ops" {
							return fmt.Errorf("the update overwrote the channel name set on the settings page: %q", p.SlackChannelName)
						}
						return nil
					},
				),
			},
			{Config: changed, PlanOnly: true},
		},
	})
	// The create's write, the web edit, and the rename's.
	writes := patchesTo(t, env, "/slack-channel", "slack_channel")
	if len(writes) != 3 {
		t.Fatalf("expected 3 slack-channel PATCHes, got %d: %v", len(writes), writes)
	}
	if last := writes[2]; last["slack_channel_name"] != "team-ops" || last["slack_notifications_enabled"] != false {
		t.Errorf("the rename's write must send the name in state (team-ops) and notifications_enabled false, got %v", last)
	}
}
