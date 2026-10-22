package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// Every test here leaves auto-promote off, or destroys the project it turned
// it on for, and none of them turns auto-rollback on outside the fake. No
// Flightdeck can promote anything for these test projects: they have no
// releases.

// autoPromoteConfig is the project with the given auto_promote settings.
func autoPromoteConfig(env *testEnv, identifier, settings string) string {
	return projectConfig(env, identifier, `
  name = "Auto-promote"
  auto_promote = {
`+settings+`
  }`)
}

// selfHealingPatches returns the recorded PATCHes to the self-healing
// endpoint, with each one's settings.
func selfHealingPatches(t *testing.T, env *testEnv) ([]flightdecktestRequest, []map[string]any) {
	t.Helper()
	var reqs []flightdecktestRequest
	var bodies []map[string]any
	for _, r := range env.fake.RequestsMatching("PATCH", "/api/v1/projects/") {
		if !strings.HasSuffix(r.Path, "/self-healing") {
			continue
		}
		var body map[string]map[string]any
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		reqs = append(reqs, r)
		bodies = append(bodies, body["self_healing"])
	}
	return reqs, bodies
}

// The settings are set, imported, changed one part at a time and dropped
// from the configuration, and the data source reports them. This one runs
// live too: it creates and destroys its own project and leaves the mode off.
func TestProjectAutoPromote_roundTrip(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	identifier := randIdentifier()
	var id string
	lookup := `
data "flightdeck_project" "lookup" {
  id = flightdeck_project.test.id
}
`
	full := `    mode           = "off"
    soak_hours     = 48
    heads_up_hours = 6
    freeze_dates   = ["2030-12-25", "2030-12-24"]
    window = {
      days      = ["wed", "mon"]
      start     = "08:00"
      end       = "17:30"
      time_zone = "Europe/London"
    }`
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: autoPromoteConfig(env, identifier, full),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "auto_promote.mode", "off"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "48"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.heads_up_hours", "6"),
					resource.TestCheckTypeSetElemAttr(projectRes, "auto_promote.freeze_dates.*", "2030-12-24"),
					resource.TestCheckTypeSetElemAttr(projectRes, "auto_promote.freeze_dates.*", "2030-12-25"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.window.days.#", "2"),
					resource.TestCheckTypeSetElemAttr(projectRes, "auto_promote.window.days.*", "mon"),
					resource.TestCheckTypeSetElemAttr(projectRes, "auto_promote.window.days.*", "wed"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.window.start", "08:00"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.window.end", "17:30"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.window.time_zone", "Europe/London"),
					// self_healing is read from the same answer, untouched.
					resource.TestCheckResourceAttr(projectRes, "self_healing.rollback", "report"),
				),
			},
			{
				ResourceName:            projectRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"features"},
			},
			{
				// Only the start changes; the rest of the window stays.
				Config: autoPromoteConfig(env, identifier, strings.Replace(full, `"08:00"`, `"07:15"`, 1)) + lookup,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "auto_promote.window.start", "07:15"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.window.end", "17:30"),
				),
			},
			{
				// Dropping the attribute keeps the stored settings and plans
				// nothing; the data source reads them.
				Config: projectConfig(env, identifier, `  name = "Auto-promote"`) + lookup,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "48"),
					resource.TestCheckResourceAttr("data.flightdeck_project.lookup", "auto_promote.mode", "off"),
					resource.TestCheckResourceAttr("data.flightdeck_project.lookup", "auto_promote.soak_hours", "48"),
					resource.TestCheckResourceAttr("data.flightdeck_project.lookup", "auto_promote.window.start", "07:15"),
					resource.TestCheckResourceAttr("data.flightdeck_project.lookup", "auto_promote.window.time_zone", "Europe/London"),
					resource.TestCheckResourceAttr("data.flightdeck_project.lookup", "auto_promote.freeze_dates.#", "2"),
				),
			},
			{
				// Turned on, which warns, and then off again before the destroy.
				Config: autoPromoteConfig(env, identifier, `    mode = "deps_only"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.mode", "deps_only"),
				),
			},
			{
				// [] clears the freeze dates.
				Config: autoPromoteConfig(env, identifier, "    mode         = \"off\"\n    freeze_dates = []"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "auto_promote.mode", "off"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.freeze_dates.#", "0"),
					func(*terraform.State) error {
						if env.live() {
							return nil
						}
						row := env.fake.AutoPromoteOf(mustInt(id))
						if dates, _ := row["freeze_dates"].([]string); row["mode"] != "off" || dates == nil || len(dates) != 0 {
							return fmt.Errorf("want the mode off and no freeze dates stored, got %v", row)
						}
						return nil
					},
				),
			},
		},
	})
	if env.live() {
		return
	}
	_, bodies := selfHealingPatches(t, env)
	if len(bodies) != 4 {
		t.Fatalf("expected four self-healing PATCHes, got %d: %v", len(bodies), bodies)
	}
	// The create sends every configured setting, days in week order and the
	// dates sorted, under the API's names.
	first, _ := json.Marshal(bodies[0])
	want := `{"auto_promote":"off","freeze_dates":["2030-12-24","2030-12-25"],"heads_up_hours":6,` +
		`"promote_window":{"days":["mon","wed"],"end":"17:30","start":"08:00","time_zone":"Europe/London"},"soak_hours":48}`
	if string(first) != want {
		t.Errorf("the create's write:\n got %s\nwant %s", first, want)
	}
	if got, _ := json.Marshal(bodies[3]); string(got) != `{"auto_promote":"off","freeze_dates":[]}` {
		t.Errorf("the last write must send [] to clear the dates, got %s", got)
	}
}

// One apply that changes self_healing and auto_promote sends them in ONE
// request, at the project's lock_version, and Flightdeck checks it whole:
// a refusal of either part saves neither.
func TestProjectAutoPromote_oneWriteWithSelfHealing(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	config := func(rollback, autoPromote string) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = "One write"
  self_healing = {
    rollback = %q
  }
  auto_promote = {
%s
  }`, rollback, autoPromote))
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config("report", `    soak_hours = 12`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					// Created at 0, bumped once by the one write.
					resource.TestCheckResourceAttr(projectRes, "lock_version", "1"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "12"),
				),
			},
			{
				// Both change: auto-rollback and auto-promote, together.
				Config: config("auto", "    soak_hours = 12\n    mode       = \"all\""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "self_healing.rollback", "auto"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.mode", "all"),
					// The project PATCH (2), then one self-healing PATCH (3).
					resource.TestCheckResourceAttr(projectRes, "lock_version", "3"),
				),
			},
			{
				// The window refused, with rollback riding along: neither saves.
				Config:      config("report", "    mode = \"off\"\n    window = {\n      start = \"08:00\"\n      end   = \"08:00\"\n    }"),
				ExpectError: regexMust(`The promote window must start before it ends`),
			},
			{
				// A refusal only Flightdeck can see (the end the project
				// already has): the plan warns, the apply fails on the attribute,
				// and nothing is saved, rollback included.
				Config: config("report", "    mode = \"off\"\n    window = {\n      start = \"16:30\"\n    }"),
				ExpectError: regexMust(errorMatching("Flightdeck refused auto_promote.window.start",
					"none of the self_healing or auto_promote settings in this apply were saved",
					"promote_window.start (16:30) must be before promote_window.end (16:00)")),
			},
			{
				PreConfig: func() {
					sh := env.fake.AutoPromoteOf(mustInt(id))
					if sh["mode"] != "all" {
						t.Errorf("the refused write must leave the mode, got %v", sh)
					}
				},
				// Both back off, together.
				Config: config("report", `    mode = "off"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "self_healing.rollback", "report"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.mode", "off"),
				),
			},
		},
	})
	reqs, bodies := selfHealingPatches(t, env)
	// create, both on, the refused one, both off.
	if len(reqs) != 4 {
		t.Fatalf("expected four self-healing PATCHes, got %d: %v", len(reqs), bodies)
	}
	if got, _ := json.Marshal(bodies[1]); string(got) != `{"auto_promote":"all","rollback":"auto","soak_hours":12}` {
		t.Errorf("the second write must carry both blocks' settings, got %s", got)
	}
	if got := reqs[1].Header.Get("If-Match"); got != `"2"` {
		t.Errorf("the write must pin the project's lock_version, got If-Match %s", got)
	}
	if got, _ := json.Marshal(bodies[2]); string(got) != `{"auto_promote":"off","promote_window":{"start":"16:30"},"rollback":"report"}` {
		t.Errorf("the refused write must send only the configured start, got %s", got)
	}
}

// The plan warns before an apply that a merged window would be refused.
func TestProjectAutoPromote_mergedWindowWarns(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	identifier := randIdentifier()
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: autoPromoteConfig(env, identifier, `    soak_hours = 24`)},
			{
				Config:      autoPromoteConfig(env, identifier, "    window = {\n      end = \"08:30\"\n    }"),
				ExpectError: regexMust(`Flightdeck refused auto_promote\.window\.end`),
			},
		},
	})
	at := tftypes.NewAttributePath().WithAttributeName("auto_promote").WithAttributeName("window").WithAttributeName("end")
	if !diagnosticAt(recorded.planWarnings(), "The promote window will not be coherent", at,
		"sets window.end to 08:30, and the project's stored window.start is 09:00") {
		t.Fatalf("no merged-window warning; plan warnings: %s", describeWithPaths(recorded.planWarnings()))
	}
}

// A partial window sends only the parts configured, and keeps the rest.
func TestProjectAutoPromote_partialWindow(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: autoPromoteConfig(env, identifier, "    window = {\n      days = [\"sat\", \"sun\"]\n    }"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "auto_promote.window.days.#", "2"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.window.start", "09:00"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.window.end", "16:00"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.window.time_zone", "America/New_York"),
				),
			},
			{
				Config: autoPromoteConfig(env, identifier, "    window = {\n      days      = [\"sat\", \"sun\"]\n      time_zone = \"UTC\"\n    }"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("auto_promote").AtMapKey("window").AtMapKey("start"), knownvalue.StringExact("09:00")),
						plancheck.ExpectKnownValue(projectRes, tfjsonpath.New("auto_promote").AtMapKey("soak_hours"), knownvalue.Int64Exact(24)),
					},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "auto_promote.window.time_zone", "UTC"),
			},
			{Config: autoPromoteConfig(env, identifier, "    window = {\n      days      = [\"sun\", \"sat\"]\n      time_zone = \"UTC\"\n    }"), PlanOnly: true},
		},
	})
	_, bodies := selfHealingPatches(t, env)
	if len(bodies) != 2 {
		t.Fatalf("expected two self-healing PATCHes, got %d", len(bodies))
	}
	for i, want := range []string{`{"promote_window":{"days":["sat","sun"]}}`, `{"promote_window":{"days":["sat","sun"],"time_zone":"UTC"}}`} {
		if got, _ := json.Marshal(bodies[i]); string(got) != want {
			t.Errorf("write %d: got %s, want %s", i+1, got, want)
		}
	}
}

// Unset, nothing is sent: state reads whatever Flightdeck holds, including a
// change made on the settings page, and plans nothing for it. Configured, a
// change on the page shows in the next plan and is set back.
func TestProjectAutoPromote_unsetIsNeverSentAndDriftIsSetBack(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, "  name = \"Drift\"\n  self_healing = {\n    bake_minutes = 30\n  }"),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.mode", "off"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "24"),
				),
			},
			{
				PreConfig: func() {
					env.fake.SetAutoPromoteOutOfBand(mustInt(id), func(m map[string]any) { m["soak_hours"] = int64(36) })
				},
				Config: projectConfig(env, identifier, "  name = \"Drift\"\n  self_healing = {\n    bake_minutes = 35\n  }"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "36"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "35"),
				),
			},
			{
				Config: projectConfig(env, identifier, "  name = \"Drift\"\n  auto_promote = {\n    soak_hours = 30\n  }"),
				Check:  resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "30"),
			},
			{
				PreConfig: func() {
					env.fake.SetAutoPromoteOutOfBand(mustInt(id), func(m map[string]any) { m["soak_hours"] = int64(40) })
				},
				Config: projectConfig(env, identifier, "  name = \"Drift\"\n  auto_promote = {\n    soak_hours = 30\n  }"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "30"),
			},
		},
	})
	_, bodies := selfHealingPatches(t, env)
	for i, body := range bodies[:2] {
		for _, key := range client.SelfHealingAutoPromoteKeys {
			if _, sent := body[key]; sent {
				t.Errorf("write %d sent %s, which the configuration leaves unset: %v", i+1, key, body)
			}
		}
	}
}

// While the mode is on, each blocker is a warning, on refresh and after
// apply, and none is stored. While it is off, the same blockers warn of
// nothing. Moving the mode away from "off" warns at plan time.
func TestProjectAutoPromote_blockersAndThePlanWarning(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	identifier := randIdentifier()
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: autoPromoteConfig(env, identifier, `    mode = "off"`)},
			{
				Config: autoPromoteConfig(env, identifier, `    mode = "all"`),
				Check:  resource.TestCheckResourceAttr(projectRes, "auto_promote.mode", "all"),
			},
			// A refresh and an empty plan.
			{Config: autoPromoteConfig(env, identifier, `    mode = "all"`), PlanOnly: true},
			{Config: autoPromoteConfig(env, identifier, `    mode = "off"`)},
		},
	})
	project := "project " + identifier
	plans := recorded.planWarnings()
	if !anyDiagnostic(plans, "This apply turns on auto-promote for "+project,
		`sets auto_promote.mode for `+project+` to "all"`,
		"promote this project's release candidates to production by itself once every check passes",
		"lists not_yet_available for every project, and the setting takes effect once promoting ships") {
		t.Fatalf("no plan-time warning for turning auto-promote on; plan warnings: %s", describe(plans))
	}
	// Only the step that moved it away from off warns: not the create at
	// "off", not the empty plan at "all", not turning it off.
	turnsOn := 0
	for _, w := range plans {
		if strings.HasPrefix(w.Summary, "This apply turns on auto-promote") {
			turnsOn++
		}
	}
	// Terraform plans each step more than once; each plan of that one step
	// warns, the others never do.
	if turnsOn == 0 || anyDiagnostic(plans, `to "off"`) {
		t.Fatalf("unexpected plan warnings: %s", describe(plans))
	}
	for _, code := range []string{"rollback_not_auto", "no_current_release", "not_yet_available"} {
		summary := "Auto-promote cannot act on " + project + " right now: " + code
		if !anyDiagnostic(recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning), summary) {
			t.Errorf("no %s warning after apply: %s", code, describe(recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning)))
		}
		if !anyDiagnostic(recorded.readWarnings(), summary) {
			t.Errorf("no %s warning on refresh: %s", code, describe(recorded.readWarnings()))
		}
	}
}

// The blockers are reported only while the mode is on.
func TestProjectAutoPromote_noBlockerWarningsWhileOff(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	identifier := randIdentifier()
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: autoPromoteConfig(env, identifier, `    soak_hours = 30`)},
			{Config: autoPromoteConfig(env, identifier, `    soak_hours = 30`), PlanOnly: true},
		},
	})
	all := append(append(recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning), recorded.readWarnings()...), recorded.planWarnings()...)
	if anyDiagnostic(all, "auto-promote") || anyDiagnostic(all, "Auto-promote") {
		t.Fatalf("auto-promote is off, so nothing may warn about it: %s", describe(all))
	}
}

// A Flightdeck from before auto-promote: a project that leaves it unset works
// as before, and reads null. Setting it fails on the attribute, saying the
// server is too old, and saves nothing, the self_healing change in the same
// write included. Once Flightdeck is upgraded, the same configuration applies.
func TestProjectAutoPromote_olderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	env.fake.SetAutoPromoteSupported(false)
	identifier := randIdentifier()
	var id string
	withBoth := projectConfig(env, identifier, `
  name = "Older"
  self_healing = {
    bake_minutes = 40
  }
  auto_promote = {
    soak_hours = 30
  }`)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, "  name = \"Older\"\n  self_healing = {\n    bake_minutes = 30\n  }"),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckNoResourceAttr(projectRes, "auto_promote.%"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "30"),
				),
			},
			{
				Config: withBoth,
				ExpectError: regexMust(errorMatching("This Flightdeck does not support auto_promote yet",
					"older than the release that added auto-promote",
					"none of the self_healing or auto_promote settings in this apply were saved",
					"Upgrade Flightdeck first", "unknown self-healing setting: soak_hours")),
			},
			{
				// Nothing was saved: not the auto-promote setting, and not the
				// threshold that went in the same write.
				PreConfig: func() {
					if row := env.fake.AutoPromoteOf(mustInt(id)); row != nil {
						t.Errorf("nothing may be saved, got %v", row)
					}
					c, err := client.New(env.endpoint, env.token)
					if err != nil {
						t.Fatal(err)
					}
					if sh, err := c.GetSelfHealing(context.Background(), mustInt(id)); err != nil || sh.Config.BakeMinutes != 30 {
						t.Errorf("the threshold in the refused write must not be saved: %v, %v", sh, err)
					}
				},
				Config:             withBoth,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				PreConfig: func() { env.fake.SetAutoPromoteSupported(true) },
				Config:    withBoth,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "30"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "40"),
				),
			},
			{Config: withBoth, PlanOnly: true},
		},
	})
}

// The error lands on the attribute, with the upgrade hint, when only
// auto_promote was sent.
func TestProjectAutoPromote_olderFlightdeckErrorIsOnTheAttribute(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	env.fake.SetAutoPromoteSupported(false)
	identifier := randIdentifier()
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: projectConfig(env, identifier, `  name = "Older"`)},
			{
				Config:      autoPromoteConfig(env, identifier, `    mode = "off"`),
				ExpectError: regexMust(`This Flightdeck does not support auto_promote yet`),
			},
		},
	})
	errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError)
	at := tftypes.NewAttributePath().WithAttributeName("auto_promote")
	if len(errs) != 1 || !diagnosticAt(errs, "This Flightdeck does not support auto_promote yet", at, "Upgrade Flightdeck first") {
		t.Fatalf("want one error on auto_promote, got %s", describeWithPaths(errs))
	}
	if !strings.Contains(errs[0].Detail, "None of the auto_promote settings in this apply were saved.") ||
		strings.Contains(errs[0].Detail, "self_healing") {
		t.Errorf("only auto_promote settings were sent, so only they were lost: %s", errs[0].Detail)
	}
}

// Flightdeck is upgraded to auto-promote, or rolled back below it, between
// the plan and the apply, as a saved plan applied later can see. With
// auto_promote unset nothing is sent for it, so the apply keeps what the plan
// promised rather than fail with "Provider produced inconsistent result", and
// the next refresh adopts what Flightdeck reports without planning anything.
func TestProjectAutoPromote_unsetSurvivesAServerChangeDuringApply(t *testing.T) {
	for _, before := range []bool{false, true} {
		t.Run(fmt.Sprintf("Flightdeck has auto-promote at plan time: %t", before), func(t *testing.T) {
			env := newTestEnv(t, "auto_promote")
			env.requireFake(t)
			env.fake.SetAutoPromoteSupported(before)
			identifier := randIdentifier()
			var id string
			config := func(minutes int) string {
				return projectConfig(env, identifier, fmt.Sprintf("  name = \"Server change\"\n  self_healing = {\n    bake_minutes = %d\n  }", minutes))
			}
			check := func(supported bool) resource.TestCheckFunc {
				if supported {
					return resource.TestCheckResourceAttr(projectRes, "auto_promote.mode", "off")
				}
				return resource.TestCheckNoResourceAttr(projectRes, "auto_promote.%")
			}
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{Config: config(30), Check: resource.ComposeAggregateTestCheckFunc(captureAttr(projectRes, "id", &id), check(before))},
					{
						PreConfig: func() {
							env.fake.OnNextRequest("PATCH", fmt.Sprintf("/api/v1/projects/%s/self-healing", id), func() {
								env.fake.SetAutoPromoteSupported(!before)
							})
						},
						Config: config(35),
						Check:  check(before),
					},
					{
						Config: config(35),
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
						},
						Check: check(!before),
					},
				},
			})
		})
	}
}

// Values the plan refuses, before anything is sent.
func TestProjectAutoPromote_planValidation(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	identifier := randIdentifier()
	cases := []struct{ settings, want string }{
		{`    mode = "ALL"`, `value must be one of`},
		{`    mode = "on"`, `value must be one of`},
		{`    soak_hours = 3`, `between 4 and 720`},
		{`    soak_hours = 721`, `between 4 and 720`},
		{`    heads_up_hours = 0`, `between 1 and 72`},
		{`    heads_up_hours = 73`, `between 1 and 72`},
		{"    window = {\n      days = []\n    }", `at least 1 elements`},
		{"    window = {\n      days = [\"Mon\"]\n    }", `value must be one of`},
		{"    window = {\n      start = \"9:00\"\n    }", `09:00, not 9:00`},
		{"    window = {\n      start = \"24:00\"\n    }", `from 00:00 to 23:59`},
		{"    window = {\n      end = \"24:01\"\n    }", `from 00:00 to 24:00`},
		{"    window = {\n      time_zone = \"Eastern Time (US & Canada)\"\n    }", `IANA time zone name`},
		{"    window = {\n      time_zone = \" UTC\"\n    }", `IANA time zone name`},
		{"    window = {\n      start = \"12:00\"\n      end   = \"11:00\"\n    }", `must be before window.end`},
		{`    freeze_dates = ["2030-02-30"]`, `not a real calendar date`},
		{`    freeze_dates = ["2030-1-5"]`, `two-digit month and day`},
		{`    freeze_dates = ["0000-01-01"]`, `year 0`},
		{`    freeze_dates = ["2031-01-01", ` + manyDates(100) + `]`, `at most 100`},
	}
	steps := make([]resource.TestStep, 0, len(cases))
	for _, tc := range cases {
		steps = append(steps, resource.TestStep{
			Config:      autoPromoteConfig(env, identifier, tc.settings),
			PlanOnly:    true,
			ExpectError: regexMust(`(?s)` + regexpQuote(tc.want)),
		})
	}
	runTest(t, resource.TestCase{Steps: steps})
}

// manyDates is n distinct dates in 2032, quoted and comma-separated.
func manyDates(n int) string {
	dates := make([]string, 0, n)
	for i := 0; i < n; i++ {
		dates = append(dates, fmt.Sprintf(`"2032-%02d-%02d"`, i/28+1, i%28+1))
	}
	return strings.Join(dates, ", ")
}

// regexpQuote escapes a plain string for regexMust, letting any run of
// white space match a line break the CLI wraps in.
func regexpQuote(s string) string {
	parts := strings.Fields(s)
	for i, p := range parts {
		parts[i] = strings.NewReplacer(`(`, `\(`, `)`, `\)`, `.`, `\.`, `[`, `\[`, `]`, `\]`, `+`, `\+`).Replace(p)
	}
	return strings.Join(parts, `\s+`)
}

// Flightdeck's own refusal of a value the plan lets through (here, a time
// zone written in the wrong case) lands on that attribute.
func TestProjectAutoPromote_refusalLandsOnTheAttribute(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	identifier := randIdentifier()
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      autoPromoteConfig(env, identifier, "    window = {\n      time_zone = \"america/new_york\"\n    }"),
				ExpectError: regexMust(`Flightdeck refused auto_promote\.window\.time_zone`),
			},
		},
	})
	if env.live() {
		return
	}
	at := tftypes.NewAttributePath().WithAttributeName("auto_promote").WithAttributeName("window").WithAttributeName("time_zone")
	if !diagnosticAt(recorded.bySeverity(tfprotov6.DiagnosticSeverityError), "Flightdeck refused auto_promote.window.time_zone", at,
		"Did you mean America/New_York?") {
		t.Fatalf("the refusal is not on the attribute: %s", describeWithPaths(recorded.bySeverity(tfprotov6.DiagnosticSeverityError)))
	}
}

// A token that is not a workspace owner or admin reads no auto-promote
// settings, and setting them is a clear 403 on the attribute.
func TestProjectAutoPromote_nonAdminToken(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	env.fake.SetWorkspaceAdmin(false)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, "  name = \"Non-admin\"\n  terraform_managed = false"),
				Check:  resource.TestCheckNoResourceAttr(projectRes, "auto_promote.%"),
			},
			{
				Config:      projectConfig(env, identifier, "  name = \"Non-admin\"\n  terraform_managed = false\n  auto_promote = {\n    mode = \"off\"\n  }"),
				ExpectError: regexMust(errorMatching("Auto-promote settings require a workspace admin", "auto-promote settings")),
			},
		},
	})
}

// fullAutoPromoteBlock is an auto_promote block with every setting set.
func fullAutoPromoteBlock(t *testing.T) types.Object {
	t.Helper()
	return autoPromoteObj(t, autoPromoteModel{
		Mode: types.StringValue("all"), SoakHours: types.Int64Value(24), HeadsUpHours: types.Int64Value(4),
		FreezeDates: stringSet([]string{"2030-12-24"}),
		Window: windowObj(t, promoteWindowModel{Days: stringSet([]string{"mon"}), Start: types.StringValue("09:00"),
			End: types.StringValue("16:00"), TimeZone: types.StringValue("UTC")}),
	})
}

func autoPromoteObj(t *testing.T, m autoPromoteModel) types.Object {
	t.Helper()
	nullIfZero(&m)
	obj, d := types.ObjectValueFrom(context.Background(), autoPromoteAttrTypes, m)
	if d.HasError() {
		t.Fatalf("building auto_promote: %v", d)
	}
	return obj
}

func windowObj(t *testing.T, w promoteWindowModel) types.Object {
	t.Helper()
	if w.Days.IsNull() && w.Days.ElementType(context.Background()) == nil {
		w.Days = types.SetNull(types.StringType)
	}
	obj, d := types.ObjectValueFrom(context.Background(), promoteWindowAttrTypes, w)
	if d.HasError() {
		t.Fatalf("building window: %v", d)
	}
	return obj
}

// nullIfZero turns the zero values of a model built in a test into nulls of
// the right type.
func nullIfZero(m *autoPromoteModel) {
	if m.Window.IsNull() && len(m.Window.AttributeTypes(context.Background())) == 0 {
		m.Window = types.ObjectNull(promoteWindowAttrTypes)
	}
	if m.FreezeDates.IsNull() && m.FreezeDates.ElementType(context.Background()) == nil {
		m.FreezeDates = types.SetNull(types.StringType)
	}
}

func TestAutoPromoteFields(t *testing.T) {
	ctx := context.Background()
	unknownSet := types.SetUnknown(types.StringType)
	cases := []struct {
		name  string
		block types.Object
		want  string
	}{
		{"no block", types.ObjectNull(autoPromoteAttrTypes), "null"},
		{"nothing configured", autoPromoteObj(t, autoPromoteModel{}), "{}"},
		{"every setting", fullAutoPromoteBlock(t),
			`{"auto_promote":"all","freeze_dates":["2030-12-24"],"heads_up_hours":4,"promote_window":{"days":["mon"],"end":"16:00","start":"09:00","time_zone":"UTC"},"soak_hours":24}`},
		{"[] is sent, to clear", autoPromoteObj(t, autoPromoteModel{FreezeDates: stringSet(nil)}), `{"freeze_dates":[]}`},
		{"dates sorted, days in week order", autoPromoteObj(t, autoPromoteModel{
			FreezeDates: stringSet([]string{"2031-01-02", "2030-12-31", "2031-01-01"}),
			Window:      windowObj(t, promoteWindowModel{Days: stringSet([]string{"sun", "fri", "mon"})}),
		}), `{"freeze_dates":["2030-12-31","2031-01-01","2031-01-02"],"promote_window":{"days":["mon","fri","sun"]}}`},
		{"one window part", autoPromoteObj(t, autoPromoteModel{Window: windowObj(t, promoteWindowModel{End: types.StringValue("24:00")})}),
			`{"promote_window":{"end":"24:00"}}`},
		{"an empty window names nothing", autoPromoteObj(t, autoPromoteModel{Window: windowObj(t, promoteWindowModel{})}), "{}"},
		{"values not known yet are left out", autoPromoteObj(t, autoPromoteModel{
			Mode: types.StringUnknown(), SoakHours: types.Int64Unknown(), FreezeDates: unknownSet,
			Window: windowObj(t, promoteWindowModel{Days: unknownSet, Start: types.StringUnknown()}),
		}), "{}"},
		{"a set with an element not known yet is left out", autoPromoteObj(t, autoPromoteModel{
			FreezeDates: types.SetValueMust(types.StringType, []attr.Value{types.StringValue("2030-12-24"), types.StringUnknown()}),
		}), "{}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			fields := autoPromoteFields(ctx, tc.block, &diags)
			if diags.HasError() {
				t.Fatalf("errors: %v", diags.Errors())
			}
			got := "null"
			if fields != nil {
				b, _ := json.Marshal(fields)
				got = string(b)
			}
			if got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestAutoPromoteToObject(t *testing.T) {
	str := func(s string) *string { return &s }
	n := func(i int64) *int64 { return &i }
	var diags diag.Diagnostics
	if got := autoPromoteToObject(nil, &diags); !got.IsNull() {
		t.Errorf("no answer must be null, got %v", got)
	}
	if got := autoPromoteToObject(&client.SelfHealing{Rollback: str("report")}, &diags); !got.IsNull() {
		t.Errorf("an older Flightdeck's answer must be null, got %v", got)
	}
	got := autoPromoteToObject(&client.SelfHealing{AutoPromote: str("deps_only"), SoakHours: n(24), HeadsUpHours: n(4),
		PromoteWindow: &client.PromoteWindow{Days: []string{"mon", "tue"}, Start: "09:00", End: "16:00", TimeZone: "UTC"},
		FreezeDates:   []string{}}, &diags)
	if diags.HasError() {
		t.Fatalf("errors: %v", diags.Errors())
	}
	var m autoPromoteModel
	diags.Append(got.As(context.Background(), &m, objectAsOptions)...)
	var w promoteWindowModel
	diags.Append(m.Window.As(context.Background(), &w, objectAsOptions)...)
	if diags.HasError() || m.Mode.ValueString() != "deps_only" || m.SoakHours.ValueInt64() != 24 || len(m.FreezeDates.Elements()) != 0 ||
		m.FreezeDates.IsNull() || len(w.Days.Elements()) != 2 || w.TimeZone.ValueString() != "UTC" {
		t.Fatalf("got %+v, window %+v (%v)", m, w, diags)
	}
}

// keepUnconfiguredAsPlanned keeps the planned value of whatever the
// configuration leaves null, part by part inside a nested object, and takes
// everything else as Flightdeck reports it.
func TestKeepUnconfiguredAsPlanned(t *testing.T) {
	ctx := context.Background()
	str, n := types.StringValue, types.Int64Value
	window := func(start, end types.String) types.Object {
		return windowObj(t, promoteWindowModel{Start: start, End: end, TimeZone: types.StringNull()})
	}
	null := types.ObjectNull(autoPromoteAttrTypes)
	planned := autoPromoteObj(t, autoPromoteModel{Mode: str("off"), SoakHours: n(24), HeadsUpHours: n(4),
		Window: window(str("09:00"), str("16:00")), FreezeDates: stringSet(nil)})
	// What Flightdeck reported: a soak default changed, and the window's end.
	result := autoPromoteObj(t, autoPromoteModel{Mode: str("off"), SoakHours: n(48), HeadsUpHours: n(6),
		Window: window(str("09:00"), str("17:00")), FreezeDates: stringSet(nil)})
	// A create plans a configured object that sets nothing as a known object
	// whose every part is unknown. A window the configuration leaves null is
	// unknown whole, and one it writes as {} is a known object whose every
	// part is unknown.
	plannedOnCreate := func(soak types.Int64, window types.Object) types.Object {
		return autoPromoteObj(t, autoPromoteModel{Mode: types.StringUnknown(), SoakHours: soak, HeadsUpHours: types.Int64Unknown(),
			FreezeDates: types.SetUnknown(types.StringType), Window: window})
	}
	unknownWindowParts := windowObj(t, promoteWindowModel{Days: types.SetUnknown(types.StringType), Start: types.StringUnknown(),
		End: types.StringUnknown(), TimeZone: types.StringUnknown()})
	cases := []struct {
		name    string
		config  types.Object
		planned types.Object
		result  types.Object
		want    types.Object
	}{
		{"not configured, the plan knew: the plan's", null, planned, result, planned},
		{"not configured, the plan had null: null", null, null, result, null},
		{"not configured, the plan did not know: Flightdeck's", null, types.ObjectUnknown(autoPromoteAttrTypes), result, result},
		{"configured but naming nothing: the plan's", autoPromoteObj(t, autoPromoteModel{Window: window(types.StringNull(), types.StringNull())}),
			planned, result, planned},
		{"configured but naming nothing, Flightdeck dropped it: the plan's", autoPromoteObj(t, autoPromoteModel{}), planned, null, planned},
		{"configured but naming nothing, on a create: Flightdeck's, since the plan knew none of it",
			autoPromoteObj(t, autoPromoteModel{}), plannedOnCreate(types.Int64Unknown(), types.ObjectUnknown(promoteWindowAttrTypes)),
			result, result},
		{"an empty window in a block that sets something, on a create: Flightdeck's window",
			autoPromoteObj(t, autoPromoteModel{SoakHours: n(48), Window: window(types.StringNull(), types.StringNull())}),
			plannedOnCreate(n(48), unknownWindowParts), result, result},
		{"configured but naming nothing, the plan knew only some of it: the plan's where it knew, Flightdeck's elsewhere",
			autoPromoteObj(t, autoPromoteModel{}),
			autoPromoteObj(t, autoPromoteModel{Mode: str("off"), SoakHours: types.Int64Unknown(), HeadsUpHours: n(4),
				Window: window(str("09:00"), str("16:00")), FreezeDates: stringSet(nil)}),
			result,
			autoPromoteObj(t, autoPromoteModel{Mode: str("off"), SoakHours: n(48), HeadsUpHours: n(4),
				Window: window(str("09:00"), str("16:00")), FreezeDates: stringSet(nil)})},
		{"a part the configuration leaves null whose plan holds something unknown: merged part by part",
			autoPromoteObj(t, autoPromoteModel{HeadsUpHours: n(6)}),
			autoPromoteObj(t, autoPromoteModel{Mode: str("off"), SoakHours: n(24), HeadsUpHours: n(4),
				Window: window(str("09:00"), types.StringUnknown()), FreezeDates: stringSet(nil)}),
			result,
			autoPromoteObj(t, autoPromoteModel{Mode: str("off"), SoakHours: n(24), HeadsUpHours: n(6),
				Window: window(str("09:00"), str("17:00")), FreezeDates: stringSet(nil)})},
		{"each setting the configuration leaves null: the plan's, the rest Flightdeck's",
			autoPromoteObj(t, autoPromoteModel{HeadsUpHours: n(6)}), planned, result,
			autoPromoteObj(t, autoPromoteModel{Mode: str("off"), SoakHours: n(24), HeadsUpHours: n(6),
				Window: window(str("09:00"), str("16:00")), FreezeDates: stringSet(nil)})},
		{"each window part the configuration leaves null: the plan's",
			autoPromoteObj(t, autoPromoteModel{Window: window(str("09:00"), types.StringNull())}), planned, result,
			autoPromoteObj(t, autoPromoteModel{Mode: str("off"), SoakHours: n(24), HeadsUpHours: n(4),
				Window: window(str("09:00"), str("16:00")), FreezeDates: stringSet(nil)})},
		{"a configured value Flightdeck did not honor: Flightdeck's, so the apply fails",
			autoPromoteObj(t, autoPromoteModel{SoakHours: n(24)}), planned, result,
			autoPromoteObj(t, autoPromoteModel{Mode: str("off"), SoakHours: n(48), HeadsUpHours: n(4),
				Window: window(str("09:00"), str("16:00")), FreezeDates: stringSet(nil)})},
		{"configured, Flightdeck reports nothing: null, so the apply fails",
			autoPromoteObj(t, autoPromoteModel{SoakHours: n(24)}), planned, null, null},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			got := keepUnconfiguredAsPlanned(ctx, tc.config, tc.planned, tc.result, &diags)
			if diags.HasError() {
				t.Fatalf("errors: %v", diags.Errors())
			}
			if !got.Equal(tc.want) {
				t.Fatalf("got  %v\nwant %v", got, tc.want)
			}
		})
	}
	// A planned part not known until apply is taken as Flightdeck reports it.
	unknownSoak := autoPromoteObj(t, autoPromoteModel{Mode: str("off"), SoakHours: types.Int64Unknown(), HeadsUpHours: n(4),
		Window: window(str("09:00"), str("16:00")), FreezeDates: stringSet(nil)})
	var diags diag.Diagnostics
	got := keepUnconfiguredAsPlanned(ctx, autoPromoteObj(t, autoPromoteModel{Mode: str("off")}), unknownSoak, result, &diags)
	var m autoPromoteModel
	diags.Append(got.As(ctx, &m, objectAsOptions)...)
	if diags.HasError() || m.SoakHours.ValueInt64() != 48 || m.HeadsUpHours.ValueInt64() != 4 {
		t.Fatalf("got %+v (%v)", m, diags)
	}
}

func TestWarnAutoPromote(t *testing.T) {
	ctx := context.Background()
	mode := func(m types.String) types.Object { return autoPromoteObj(t, autoPromoteModel{Mode: m}) }
	off, all, deps := types.StringValue("off"), types.StringValue("all"), types.StringValue("deps_only")
	null := types.ObjectNull(autoPromoteAttrTypes)
	cases := []struct {
		name                  string
		config, prior, plan   types.Object
		wantSummary, wantText string
	}{
		{name: "off to all", config: mode(all), prior: mode(off), plan: mode(all),
			wantSummary: "This apply turns on auto-promote for project APP", wantText: `to "all"`},
		{name: "created at deps_only", config: mode(deps), prior: null, plan: mode(deps),
			wantSummary: "This apply turns on auto-promote for project APP", wantText: "when every change waiting for production is a Dependabot update"},
		{name: "an older Flightdeck's null to all", config: mode(all), prior: null, plan: mode(all),
			wantSummary: "This apply turns on auto-promote for project APP"},
		{name: "not known until apply", config: mode(types.StringUnknown()), prior: mode(off), plan: mode(types.StringUnknown()),
			wantSummary: "This apply may turn on auto-promote for project APP", wantText: "not known until apply"},
		{name: "already on", config: mode(all), prior: mode(deps), plan: mode(all)},
		{name: "turned off", config: mode(off), prior: mode(all), plan: mode(off)},
		{name: "stays off", config: mode(off), prior: mode(off), plan: mode(off)},
		{name: "unset on create", config: null, prior: null, plan: types.ObjectUnknown(autoPromoteAttrTypes)},
		{name: "unset, on in state", config: null, prior: mode(all), plan: mode(all)},
		{name: "mode unset in a configured block", config: mode(types.StringNull()), prior: mode(off), plan: mode(off)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			warnAutoPromote(ctx, types.StringValue("APP"), tc.config, tc.prior, tc.plan, &diags)
			if diags.HasError() {
				t.Fatalf("errors: %v", diags.Errors())
			}
			if tc.wantSummary == "" {
				if diags.WarningsCount() != 0 {
					t.Fatalf("want no warning, got %v", diags.Warnings())
				}
				return
			}
			if diags.WarningsCount() != 1 {
				t.Fatalf("want one warning, got %v", diags.Warnings())
			}
			w := diags.Warnings()[0]
			if w.Summary() != tc.wantSummary || !strings.Contains(w.Detail(), tc.wantText) ||
				!strings.Contains(w.Detail(), "not_yet_available") {
				t.Fatalf("got %q / %q", w.Summary(), w.Detail())
			}
			if withPath, ok := w.(diag.DiagnosticWithPath); !ok || !withPath.Path().Equal(path.Root("auto_promote").AtName("mode")) {
				t.Fatalf("the warning is not on auto_promote.mode")
			}
		})
	}
}

func TestWarnAutoPromoteBlockers(t *testing.T) {
	str := func(s string) *string { return &s }
	blockers := []client.AutoPromoteBlocker{
		{Code: "rollback_not_auto", Message: "Turn on auto-rollback first."},
		{Code: "not_rollback_safe", Message: "Mark the live release safe to roll back."},
		{Code: "not_rollback_safe", Message: "And the one before it."},
		{Code: "not_yet_available", Message: "Flightdeck doesn't promote on its own yet."},
	}
	for _, tc := range []struct {
		name string
		sh   *client.SelfHealing
		want int
	}{
		{name: "no answer", sh: nil},
		{name: "an older Flightdeck", sh: &client.SelfHealing{}},
		{name: "off lists blockers but warns of none", sh: &client.SelfHealing{AutoPromote: str("off"), AutoPromoteBlockers: blockers}},
		{name: "on with nothing in the way", sh: &client.SelfHealing{AutoPromote: str("all")}},
		{name: "on with blockers", sh: &client.SelfHealing{AutoPromote: str("deps_only"), AutoPromoteBlockers: blockers}, want: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			warnAutoPromoteBlockers(tc.sh, "APP", &diags)
			if diags.WarningsCount() != tc.want {
				t.Fatalf("expected %d warnings, got %v", tc.want, diags.Warnings())
			}
			seen := map[string]bool{}
			for i, w := range diags.Warnings() {
				want := "Auto-promote cannot act on project APP right now: " + blockers[i].Code
				if i == 2 {
					want += " (2)"
				}
				if w.Summary() != want || !strings.Contains(w.Detail(), blockers[i].Message) || seen[w.Summary()] {
					t.Fatalf("warning %d = %q / %q, want %q", i, w.Summary(), w.Detail(), want)
				}
				seen[w.Summary()] = true
			}
		})
	}
}

func TestAutoPromoteRefusedAt(t *testing.T) {
	refusal := func(code, msg string) error {
		return &client.Error{Method: "PATCH", Path: "/projects/4/self-healing", Status: 422, Code: code, Message: msg}
	}
	root := path.Root("auto_promote")
	window := root.AtName("window")
	sent := client.Fields{
		"auto_promote": "all", "soak_hours": int64(3), "heads_up_hours": int64(80),
		"freeze_dates":   []string{"2030-12-24", "2030-12-25"},
		"promote_window": map[string]any{"start": "08:00", "time_zone": "x"},
	}
	cases := []struct {
		name string
		sent client.Fields
		err  error
		want path.Path
		ok   bool
	}{
		{"the mode", sent, refusal(client.CodeInvalidAttribute, `auto_promote must be "off", "deps_only" or "all", got true`), root.AtName("mode"), true},
		{"soak hours", sent, refusal(client.CodeInvalidAttribute, "soak_hours must be a whole number of hours from 4 to 720, got 3"), root.AtName("soak_hours"), true},
		{"heads-up hours", sent, refusal(client.CodeInvalidAttribute, "heads_up_hours must be a whole number of hours from 1 to 72, got 80"), root.AtName("heads_up_hours"), true},
		{"one freeze date", sent, refusal(client.CodeInvalidAttribute, `freeze_dates[1] must be an ISO 8601 date (YYYY-MM-DD), got "2030-12-25"`),
			root.AtName("freeze_dates").AtSetValue(types.StringValue("2030-12-25")), true},
		{"a freeze date past the list", sent, refusal(client.CodeInvalidAttribute, `freeze_dates[9] must be an ISO 8601 date`), root.AtName("freeze_dates"), true},
		{"the freeze dates", sent, refusal(client.CodeInvalidAttribute, "freeze_dates lists 2030-12-24 more than once"), root.AtName("freeze_dates"), true},
		{"a window part", sent, refusal(client.CodeInvalidAttribute, "promote_window.time_zone must be an IANA time zone name, such as America/New_York, got \"x\""), window.AtName("time_zone"), true},
		{"the window whole", sent, refusal(client.CodeInvalidAttribute, "promote_window must be an object"), window, true},
		{"start after a stored end, start sent", sent, refusal(client.CodeInvalidAttribute, "promote_window.start (08:00) must be before promote_window.end (07:00)"), window.AtName("start"), true},
		{"start after a stored end, end sent", client.Fields{"promote_window": map[string]any{"end": "07:00"}},
			refusal(client.CodeInvalidAttribute, "promote_window.start (08:00) must be before promote_window.end (07:00)"), window.AtName("end"), true},
		{"not sent", client.Fields{"bake_minutes": int64(30)}, refusal(client.CodeInvalidAttribute, "soak_hours must be a whole number"), path.Empty(), false},
		{"a threshold", sent, refusal(client.CodeInvalidAttribute, "bake_minutes must be between 1 and 1440"), path.Empty(), false},
		{"an unknown key", sent, refusal(client.CodeInvalidAttribute, "unknown self-healing setting: soak_hours (settable: ...)"), path.Empty(), false},
		{"a longer key", client.Fields{"auto_promote": "all"}, refusal(client.CodeInvalidAttribute, "auto_promote_blockers is read-only"), path.Empty(), false},
		{"not invalid_attribute", sent, refusal(client.CodeValidationFailed, "soak_hours must be a whole number"), path.Empty(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, ok := autoPromoteRefusedAt(tc.sent, tc.err)
			if ok != tc.ok || !got.Equal(tc.want) {
				t.Fatalf("got %s (%t), want %s (%t)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestIsoDateProblem(t *testing.T) {
	for date, ok := range map[string]bool{
		"2030-12-24": true, "2028-02-29": true, "0001-01-01": true, "9999-12-31": true,
		"2030-02-29": false, "2030-13-01": false, "2030-12-32": false, "2030-1-05": false, "30-12-24": false,
		" 2030-12-24": false, "2030-12-24T00:00:00Z": false, "0000-01-01": false, "": false,
	} {
		if got := isoDateProblem(date) == ""; got != ok {
			t.Errorf("isoDateProblem(%q) accepts = %t, want %t", date, got, ok)
		}
	}
}

func TestDaysInWeekOrder(t *testing.T) {
	got := daysInWeekOrder([]string{"sun", "mon", "xyz", "wed", "abc"})
	if strings.Join(got, ",") != "mon,wed,sun,abc,xyz" {
		t.Fatalf("got %v", got)
	}
}

// The fake and the client agree on the API's keys, so the provider's list is
// what holds the writable-settings drift test to an exact match.
func TestSelfHealingAutoPromoteKeys_matchTheFake(t *testing.T) {
	got := append([]string{}, flightdecktest.AutoPromoteWritable...)
	want := append([]string{}, client.SelfHealingAutoPromoteKeys...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("fake %v, client %v", got, want)
	}
}

// errorMatching is a pattern for an error the CLI prints, which wraps long
// lines: each fragment is matched with any white space standing for a space,
// and the fragments in order with anything between them.
func errorMatching(fragments ...string) string {
	quoted := make([]string, 0, len(fragments))
	for _, f := range fragments {
		quoted = append(quoted, regexpQuote(f))
	}
	return "(?s)" + strings.Join(quoted, ".*")
}

// While both are on, a rollback blocker is warned once, as a rollback
// blocker that stops auto-promote too, and not again as an auto-promote one.
// Auto-promote's own codes are still warned.
func TestWarnBlockers_notRepeated(t *testing.T) {
	str := func(s string) *string { return &s }
	sh := &client.SelfHealing{
		Rollback:         str("auto"),
		RollbackBlockers: []client.RollbackBlocker{{Code: "not_rollback_safe", Message: "Mark the live release safe to roll back."}},
		AutoPromote:      str("all"),
		AutoPromoteBlockers: []client.AutoPromoteBlocker{
			{Code: "not_rollback_safe", Message: "Mark the live release safe to roll back."},
			{Code: "not_yet_available", Message: "Flightdeck doesn't promote on its own yet."},
		},
	}
	var diags diag.Diagnostics
	warnRollbackBlockers(sh, "APP", &diags)
	warnAutoPromoteBlockers(sh, "APP", &diags)
	var summaries []string
	for _, w := range diags.Warnings() {
		summaries = append(summaries, w.Summary())
	}
	want := "Auto-rollback cannot act on project APP right now: not_rollback_safe|" +
		"Auto-promote cannot act on project APP right now: not_yet_available"
	if got := strings.Join(summaries, "|"); got != want {
		t.Fatalf("warnings:\n got %s\nwant %s", got, want)
	}
	if !strings.Contains(diags.Warnings()[0].Detail(), "It stops auto-promote too") {
		t.Errorf("the rollback blocker must say it stops auto-promote too: %s", diags.Warnings()[0].Detail())
	}
	// With auto-rollback in report only, the rollback blockers are not warned
	// of as such, so auto-promote's copy of them is.
	sh.Rollback = str("report")
	diags = nil
	warnRollbackBlockers(sh, "APP", &diags)
	warnAutoPromoteBlockers(sh, "APP", &diags)
	if diags.WarningsCount() != 2 || !strings.HasSuffix(diags.Warnings()[0].Summary(), "right now: not_rollback_safe") ||
		!strings.HasPrefix(diags.Warnings()[0].Summary(), "Auto-promote") {
		t.Fatalf("want both auto-promote blockers, got %v", diags.Warnings())
	}
	// With auto-promote off, the rollback warning does not mention it.
	sh.Rollback, sh.AutoPromote = str("auto"), str("off")
	diags = nil
	warnRollbackBlockers(sh, "APP", &diags)
	if diags.WarningsCount() != 1 || strings.Contains(diags.Warnings()[0].Detail(), "auto-promote") {
		t.Fatalf("got %v", diags.Warnings())
	}
}

// The same, end to end, with auto-rollback and auto-promote turned on in one
// apply.
func TestProjectAutoPromote_blockersNotRepeatedEndToEnd(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	identifier := randIdentifier()
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: projectConfig(env, identifier, `
  name = "Not repeated"
  self_healing = {
    feature_enabled = true
    rollback        = "auto"
  }
  auto_promote = {
    mode = "all"
  }`)},
		},
	})
	warnings := recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning)
	project := "project " + identifier
	if !anyDiagnostic(warnings, "Auto-rollback cannot act on "+project+" right now: no_current_release", "It stops auto-promote too") {
		t.Fatalf("no rollback blocker warning naming auto-promote: %s", describe(warnings))
	}
	if anyDiagnostic(warnings, "Auto-promote cannot act on "+project+" right now: no_current_release") {
		t.Fatalf("the rollback blocker is warned twice: %s", describe(warnings))
	}
	for _, code := range []string{"no_github_repository", "not_yet_available"} {
		if !anyDiagnostic(warnings, "Auto-promote cannot act on "+project+" right now: "+code) {
			t.Errorf("auto-promote's own %s is missing: %s", code, describe(warnings))
		}
	}
}

// A new project's partial window is merged into Flightdeck's default window
// (09:00 to 16:00). One that cannot fit it fails the PLAN, before the project
// is created; refused after the create instead, the project would be tainted
// and replaced on every later apply.
func TestProjectAutoPromote_createPartialWindowFailsThePlan(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	identifier := randIdentifier()
	config := func(window string) string {
		return projectConfig(env, identifier, `
  name = "Create partial"
  self_healing = {
    bake_minutes = 33
  }
  auto_promote = {
    window = {
`+window+`
    }
  }`)
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config(`      start = "16:30"`),
				ExpectError: regexMust(errorMatching("The promote window must start before it ends",
					"sets only window.start, to 16:30", "whose end is 16:00", "Set window.end too")),
			},
			{
				Config: config(`      end = "08:00"`),
				ExpectError: regexMust(errorMatching("The promote window must start before it ends",
					"sets only window.end, to 08:00", "whose start is 09:00", "Set window.start too")),
			},
			{
				// One that fits the default window creates the project.
				Config: config(`      start = "10:00"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "auto_promote.window.start", "10:00"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.window.end", "16:00"),
				),
			},
		},
	})
	if creates := env.fake.RequestsMatching("POST", "/api/v1/projects"); len(creates) != 1 {
		t.Fatalf("only the window that fits may create a project, got %d creates", len(creates))
	}
}

// A new project's partial window that fits Flightdeck's default window,
// including at its edges, creates the project.
func TestProjectAutoPromote_createPartialWindowsThatFit(t *testing.T) {
	for _, window := range []string{
		`days = ["sat"]`, `time_zone = "UTC"`, `end = "24:00"`, `start = "15:59"`, `end = "09:01"`, `start = "00:00"`,
	} {
		t.Run(window, func(t *testing.T) {
			env := newTestEnv(t, "auto_promote")
			env.requireFake(t)
			identifier := randIdentifier()
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{Config: autoPromoteConfig(env, identifier, "    window = {\n      "+window+"\n    }")},
				},
			})
		})
	}
}

// A window bound set from a value not known until apply is still set: the
// plan does not check it against the default window, which Flightdeck would
// not use for it. A window whose bounds are both set is checked as a whole
// once both are known, and a lone bound not known yet has nothing to check.
func TestProjectAutoPromote_createWindowBoundNotKnownUntilApply(t *testing.T) {
	for _, tc := range []struct {
		name, value, window, start, end string
	}{
		{name: "start", value: "07:00", window: "start = terraform_data.bound.output\n      end   = \"08:00\"", start: "07:00", end: "08:00"},
		{name: "end", value: "18:00", window: "start = \"17:00\"\n      end   = terraform_data.bound.output", start: "17:00", end: "18:00"},
		{name: "start alone", value: "10:00", window: "start = terraform_data.bound.output", start: "10:00", end: "16:00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, "auto_promote")
			env.requireFake(t)
			identifier := randIdentifier()
			config := env.providerConfig() + fmt.Sprintf(`
resource "terraform_data" "bound" {
  input = %q
}

resource "flightdeck_project" "test" {
  name       = "Bound from elsewhere"
  identifier = %q
  auto_promote = {
    window = {
      %s
    }
  }
}
`, tc.value, identifier, tc.window)
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{
						Config: config,
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(projectRes, "auto_promote.window.start", tc.start),
							resource.TestCheckResourceAttr(projectRes, "auto_promote.window.end", tc.end),
						),
					},
				},
			})
		})
	}
}

// An auto_promote block the configuration names, even one that sets nothing,
// fails the apply when Flightdeck cannot answer for it, rather than reading
// as null and planning it again on every run. Left out, it stays silent.
func TestProjectAutoPromote_namedBlockFlightdeckCannotRead(t *testing.T) {
	testNamedBlockUnreadable(t, []unreadableBlock{
		{
			reason: "not an admin", block: "auto_promote", nonAdmin: true,
			setup:   func(env *testEnv) { env.fake.SetWorkspaceAdmin(false) },
			summary: "Auto-promote settings require a workspace admin",
			detail: "Only a workspace owner or admin may read or write a project's auto-promote settings, which ride " +
				"its self-healing endpoint.",
		},
		{
			reason: "no endpoint", block: "auto_promote",
			setup:   func(env *testEnv) { env.fake.SetSelfHealingEndpoint(false) },
			summary: "Auto-promote settings are not available on this Flightdeck",
			detail:  "which carries the auto-promote settings, answered 404",
		},
		{
			reason: "older Flightdeck", block: "auto_promote",
			setup:   func(env *testEnv) { env.fake.SetAutoPromoteSupported(false) },
			summary: "Auto-promote settings are not available on this Flightdeck",
			detail: "which carries the auto-promote settings, does not report them, so this Flightdeck version does " +
				"not expose them over the API yet. Remove auto_promote or upgrade Flightdeck.",
		},
	})
}

// Both blocks named and neither readable: one error naming both, in the words
// of a refused write that carried both. And self_healing settings written to a
// Flightdeck without auto-promote are saved, while the auto_promote beside
// them that sets nothing fails the apply.
func TestProjectAutoPromote_namedWithSelfHealingFlightdeckCannotRead(t *testing.T) {
	t.Run("both blocks, not an admin", func(t *testing.T) {
		env := newTestEnv(t, "auto_promote")
		env.requireFake(t)
		env.fake.SetWorkspaceAdmin(false)
		identifier := randIdentifier()
		base := "  name = \"Both\"\n  terraform_managed = false\n"
		summary := "Self-healing and auto-promote settings require a workspace admin"
		detail := "Only a workspace owner or admin may read or write a project's self-healing thresholds and its " +
			"auto-promote settings, which ride the same endpoint."
		recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
			Steps: []resource.TestStep{
				{Config: projectConfig(env, identifier, base)},
				{
					Config:      projectConfig(env, identifier, base+"  self_healing = {}\n  auto_promote = {}"),
					ExpectError: regexMust(errorMatching(summary, detail)),
				},
			},
		})
		errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError)
		if len(errs) != 1 || !anyDiagnostic(errs, summary, detail) || strings.Contains(errs[0].Detail, "saved") {
			t.Fatalf("want one error naming both, saying nothing of what was saved; got: %s", describe(errs))
		}
	})
	t.Run("self_healing settings, auto_promote {}, older Flightdeck", func(t *testing.T) {
		env := newTestEnv(t, "auto_promote")
		env.requireFake(t)
		env.fake.SetAutoPromoteSupported(false)
		identifier := randIdentifier()
		selfHealing := "  name = \"Mixed\"\n  self_healing = {\n    bake_minutes = 30\n  }\n"
		runTest(t, resource.TestCase{
			Steps: []resource.TestStep{
				{Config: projectConfig(env, identifier, `  name = "Mixed"`)},
				{
					Config: projectConfig(env, identifier, selfHealing+"  auto_promote = {}"),
					ExpectError: regexMust(errorMatching("Auto-promote settings are not available on this Flightdeck",
						"does not report them")),
				},
				{
					// The setting was saved: the refresh reads it back, so with
					// auto_promote left out there is nothing to plan.
					Config: projectConfig(env, identifier, selfHealing),
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
					},
					Check: resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "30"),
				},
			},
		})
		_, bodies := selfHealingPatches(t, env)
		if len(bodies) != 1 {
			t.Fatalf("expected one self-healing PATCH, got %d: %v", len(bodies), bodies)
		}
		if got, _ := json.Marshal(bodies[0]); string(got) != `{"bake_minutes":30}` {
			t.Errorf("the write must carry only the self_healing setting, got %s", got)
		}
	})
}

// An auto_promote block that sets nothing creates the project, whether it is
// written as {} or has every setting null (a module passing through an
// optional variable that defaults to null). Nothing is sent, and the plan
// knew none of the settings, so each one is taken as Flightdeck reports it.
// The next plan is empty.
func TestProjectAutoPromote_blockThatSetsNothingOnCreate(t *testing.T) {
	for _, tc := range []struct{ name, variable, settings string }{
		{name: "empty"},
		{
			name: "every setting null",
			variable: `
variable "mode" {
  type    = string
  default = null
}
`,
			settings: "    mode = var.mode",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, "auto_promote")
			env.requireFake(t)
			identifier := randIdentifier()
			config := tc.variable + autoPromoteConfig(env, identifier, tc.settings)
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{
						Config: config,
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(projectRes, "auto_promote.mode", "off"),
							resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "24"),
							resource.TestCheckResourceAttr(projectRes, "auto_promote.heads_up_hours", "4"),
							resource.TestCheckResourceAttr(projectRes, "auto_promote.freeze_dates.#", "0"),
							resource.TestCheckResourceAttr(projectRes, "auto_promote.window.days.#", "5"),
							resource.TestCheckResourceAttr(projectRes, "auto_promote.window.start", "09:00"),
							resource.TestCheckResourceAttr(projectRes, "auto_promote.window.end", "16:00"),
							resource.TestCheckResourceAttr(projectRes, "auto_promote.window.time_zone", "America/New_York"),
						),
					},
					{Config: config, PlanOnly: true},
				},
			})
			if reqs, bodies := selfHealingPatches(t, env); len(reqs) != 0 {
				t.Fatalf("a block that sets nothing must send nothing, got %v", bodies)
			}
		})
	}
}

// An empty window inside a block that sets something else names no part of
// the window, so none is sent. On a create the plan knew none of the window,
// and it is taken as Flightdeck reports it. On an update the plan's window is
// kept, even when Flightdeck's default window changes during the apply, and
// the next refresh adopts Flightdeck's without planning anything.
func TestProjectAutoPromote_emptyWindowInABlockThatSetsSomething(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	config := func(soak int) string {
		return autoPromoteConfig(env, identifier, fmt.Sprintf("    soak_hours = %d\n    window = {}", soak))
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config(30),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "30"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.window.days.#", "5"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.window.start", "09:00"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.window.end", "16:00"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.window.time_zone", "America/New_York"),
				),
			},
			{
				PreConfig: func() {
					env.fake.OnNextRequest("PATCH", "/api/v1/projects/"+id+"/self-healing", func() {
						env.fake.SetAutoPromoteDefault("promote_window", map[string]any{
							"days": []string{"mon", "tue", "wed", "thu", "fri"}, "start": "09:00", "end": "17:00", "time_zone": "America/New_York",
						})
					})
				},
				Config: config(31),
				// The plan's window, kept.
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "31"),
					resource.TestCheckResourceAttr(projectRes, "auto_promote.window.end", "16:00"),
				),
			},
			{
				Config: config(31),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "auto_promote.window.end", "17:00"),
			},
		},
	})
	_, bodies := selfHealingPatches(t, env)
	if len(bodies) != 2 {
		t.Fatalf("expected two self-healing PATCHes, got %d: %v", len(bodies), bodies)
	}
	for i, want := range []string{`{"soak_hours":30}`, `{"soak_hours":31}`} {
		if got, _ := json.Marshal(bodies[i]); string(got) != want {
			t.Errorf("write %d: got %s, want %s", i+1, got, want)
		}
	}
}

// A setting the configuration leaves unset that changes in Flightdeck between
// the plan and the apply, with no lock_version moving (a Flightdeck release
// changing a default), keeps the planned value, and the next refresh adopts
// Flightdeck's without planning anything. That holds for an auto_promote
// setting, a window part, and a self_healing threshold, and with or without
// the attribute in configuration.
func TestProjectAutoPromote_defaultChangesBetweenPlanAndApply(t *testing.T) {
	cases := []struct {
		name string
		// body is the project's settings at the given heads-up hours.
		body func(hours int) string
		// change changes a default; check reads the result.
		change func(env *testEnv)
		before resource.TestCheckFunc
		after  resource.TestCheckFunc
		// trigger is the request the change lands just before.
		trigger func(id string) (string, string)
	}{
		{
			name: "an auto_promote setting",
			body: func(hours int) string {
				return fmt.Sprintf("  auto_promote = {\n    mode           = \"off\"\n    heads_up_hours = %d\n  }", hours)
			},
			change: func(env *testEnv) { env.fake.SetAutoPromoteDefault("soak_hours", int64(48)) },
			before: resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "24"),
			after:  resource.TestCheckResourceAttr(projectRes, "auto_promote.soak_hours", "48"),
			trigger: func(id string) (string, string) {
				return "PATCH", "/api/v1/projects/" + id + "/self-healing"
			},
		},
		{
			// A start equal to the default stores nothing, so the window still
			// follows the default; once a write changes any part, Flightdeck
			// stores the whole merged window and a default no longer reaches it.
			name: "a window part",
			body: func(hours int) string {
				return fmt.Sprintf("  auto_promote = {\n    heads_up_hours = %d\n    window = {\n      start = \"09:00\"\n    }\n  }", hours)
			},
			change: func(env *testEnv) {
				env.fake.SetAutoPromoteDefault("promote_window", map[string]any{
					"days": []string{"mon", "tue", "wed", "thu", "fri"}, "start": "09:00", "end": "17:00", "time_zone": "America/New_York",
				})
			},
			before: resource.TestCheckResourceAttr(projectRes, "auto_promote.window.end", "16:00"),
			after:  resource.TestCheckResourceAttr(projectRes, "auto_promote.window.end", "17:00"),
			trigger: func(id string) (string, string) {
				return "PATCH", "/api/v1/projects/" + id + "/self-healing"
			},
		},
		{
			name: "auto_promote not configured, read after a rename",
			body: func(hours int) string { return fmt.Sprintf("  description = \"rename %d\"", hours) },
			change: func(env *testEnv) {
				env.fake.SetAutoPromoteDefault("heads_up_hours", int64(8))
			},
			before: resource.TestCheckResourceAttr(projectRes, "auto_promote.heads_up_hours", "4"),
			after:  resource.TestCheckResourceAttr(projectRes, "auto_promote.heads_up_hours", "8"),
			trigger: func(id string) (string, string) {
				return "PATCH", "/api/v1/projects/" + id
			},
		},
		{
			name: "a self_healing threshold",
			body: func(hours int) string {
				return fmt.Sprintf("  self_healing = {\n    bake_minutes = %d\n  }", 20+hours)
			},
			change: func(env *testEnv) { env.fake.SetSelfHealingDefault("sustain_count", int64(4)) },
			before: resource.TestCheckResourceAttr(projectRes, "self_healing.sustain_count", "3"),
			after:  resource.TestCheckResourceAttr(projectRes, "self_healing.sustain_count", "4"),
			trigger: func(id string) (string, string) {
				return "PATCH", "/api/v1/projects/" + id + "/self-healing"
			},
		},
		{
			name: "self_healing not configured, read after a rename",
			body: func(hours int) string { return fmt.Sprintf("  description = \"rename %d\"", hours) },
			change: func(env *testEnv) {
				env.fake.SetSelfHealingDefault("cooldown_minutes", int64(45))
			},
			before: resource.TestCheckResourceAttr(projectRes, "self_healing.cooldown_minutes", "30"),
			after:  resource.TestCheckResourceAttr(projectRes, "self_healing.cooldown_minutes", "45"),
			trigger: func(id string) (string, string) {
				return "PATCH", "/api/v1/projects/" + id
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, "auto_promote")
			env.requireFake(t)
			identifier := randIdentifier()
			var id string
			config := func(hours int) string {
				return projectConfig(env, identifier, "  name = \"Default change\"\n"+tc.body(hours))
			}
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{Config: config(5), Check: resource.ComposeAggregateTestCheckFunc(captureAttr(projectRes, "id", &id), tc.before)},
					{
						PreConfig: func() {
							method, route := tc.trigger(id)
							env.fake.OnNextRequest(method, route, func() { tc.change(env) })
						},
						Config: config(6),
						// The plan's value, kept.
						Check: tc.before,
					},
					{
						Config: config(6),
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
						},
						Check: tc.after,
					},
				},
			})
		})
	}
}

// Leaving mode out while auto-promote is on keeps it on, with an empty plan,
// so the plan warns. Leaving it out while it is off does not.
func TestProjectAutoPromote_modeLeftUnsetWhileOnWarns(t *testing.T) {
	env := newTestEnv(t, "auto_promote")
	env.requireFake(t)
	identifier := randIdentifier()
	project := "project " + identifier
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: autoPromoteConfig(env, identifier, `    mode = "all"`)},
			{
				// The whole block removed.
				Config: projectConfig(env, identifier, `  name = "Auto-promote"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "auto_promote.mode", "all"),
			},
		},
	})
	at := tftypes.NewAttributePath().WithAttributeName("auto_promote").WithAttributeName("mode")
	if !diagnosticAt(recorded.planWarnings(), "Auto-promote stays on for "+project, at,
		`auto_promote.mode is not set in this configuration, so Flightdeck keeps it at "all"`,
		`Leaving mode out does not turn auto-promote off.`, `Set mode = "off" to turn it off, or set mode = "all" to keep it on`) {
		t.Fatalf("no warning for removing the block; plan warnings: %s", describeWithPaths(recorded.planWarnings()))
	}

	// Only mode removed, the block kept.
	identifier = randIdentifier()
	recorded = runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: autoPromoteConfig(env, identifier, "    mode       = \"deps_only\"\n    soak_hours = 30")},
			{Config: autoPromoteConfig(env, identifier, `    soak_hours = 30`), PlanOnly: true},
		},
	})
	if !anyDiagnostic(recorded.planWarnings(), "Auto-promote stays on for project "+identifier, `keeps it at "deps_only"`) {
		t.Fatalf("no warning for removing mode; plan warnings: %s", describe(recorded.planWarnings()))
	}

	// Off, then removed: nothing to warn about.
	identifier = randIdentifier()
	recorded = runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: autoPromoteConfig(env, identifier, `    mode = "off"`)},
			{Config: projectConfig(env, identifier, `  name = "Auto-promote"`), PlanOnly: true},
		},
	})
	if anyDiagnostic(recorded.planWarnings(), "stays on") {
		t.Fatalf("auto-promote is off, so nothing may warn: %s", describe(recorded.planWarnings()))
	}
}

// A refused write names what it carried: self_healing settings, auto_promote
// settings, or both, and says that only those were not saved.
func TestProjectAutoPromote_refusalsNameWhatWasSent(t *testing.T) {
	both := func(env *testEnv, identifier string, soak int) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name              = "Both"
  terraform_managed = false
  self_healing = {
    bake_minutes = %d
  }
  auto_promote = {
    soak_hours = %d
  }`, soak, soak))
	}
	t.Run("403 for both", func(t *testing.T) {
		env := newTestEnv(t, "auto_promote")
		env.requireFake(t)
		identifier := randIdentifier()
		runTest(t, resource.TestCase{
			Steps: []resource.TestStep{
				{Config: projectConfig(env, identifier, "  name = \"Both\"\n  terraform_managed = false")},
				{
					PreConfig: func() { env.fake.SetWorkspaceAdmin(false) },
					Config:    both(env, identifier, 30),
					ExpectError: regexMust(errorMatching("Self-healing and auto-promote settings require a workspace admin",
						"none of the self_healing or auto_promote settings in this apply were saved")),
				},
			},
		})
	})
	t.Run("404 for both", func(t *testing.T) {
		env := newTestEnv(t, "auto_promote")
		env.requireFake(t)
		identifier := randIdentifier()
		runTest(t, resource.TestCase{
			Steps: []resource.TestStep{
				{Config: projectConfig(env, identifier, "  name = \"Both\"\n  terraform_managed = false")},
				{
					PreConfig: func() { env.fake.SetSelfHealingEndpoint(false) },
					Config:    both(env, identifier, 30),
					ExpectError: regexMust(errorMatching("Self-healing and auto-promote settings are not available on this Flightdeck",
						"carries both the self_healing and the auto_promote settings", "Remove both")),
				},
			},
		})
	})
	for _, tc := range []struct {
		name, config, want string
	}{
		{"409 for both", "both", "Project self-healing and auto-promote settings changed outside of Terraform"},
		{"409 for auto_promote", "auto_promote", "Project auto-promote settings changed outside of Terraform"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, "auto_promote")
			env.requireFake(t)
			identifier := randIdentifier()
			var id string
			config := func(soak int) string {
				if tc.config == "both" {
					return both(env, identifier, soak)
				}
				return autoPromoteConfig(env, identifier, fmt.Sprintf("    soak_hours = %d", soak))
			}
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{Config: config(30), Check: captureAttr(projectRes, "id", &id)},
					{
						// Something else writes the project between the project
						// PATCH and the self-healing one.
						PreConfig: func() {
							env.fake.OnNextRequest("PATCH", "/api/v1/projects/"+id+"/self-healing", func() {
								env.fake.SetAutoPromoteOutOfBand(mustInt(id), func(m map[string]any) { m["heads_up_hours"] = int64(9) })
							})
						},
						Config:      config(31),
						ExpectError: regexMust(regexpQuote(tc.want)),
					},
				},
			})
		})
	}
}
