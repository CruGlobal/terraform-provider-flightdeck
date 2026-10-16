package provider

import (
	"context"
	"encoding/json"
	"fmt"
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

// The channel mode (slack_channel.mode). Setting a mode while the channel is
// off asks nothing of Slack, so the live-capable tests set and change the mode
// on a channel that stays off. Anything that turns the channel on, and so
// would create or link a real Slack channel, runs against the fake only.

// slackModeDataSource reads the project back through the data source.
func slackModeDataSource(identifier string) string {
	return fmt.Sprintf(`
data "flightdeck_project" "x" {
  identifier = %q
  depends_on = [flightdeck_project.test]
}
`, identifier)
}

// setSlackChannelModeElsewhere changes the mode the way someone on the
// settings page would, outside Terraform. It works live and against the fake.
func setSlackChannelModeElsewhere(t *testing.T, env *testEnv, projectID int64, mode string) {
	t.Helper()
	c, err := client.New(env.endpoint, env.token)
	if err != nil {
		t.Fatal(err)
	}
	current, err := c.GetSlackChannel(t.Context(), projectID)
	if err != nil {
		t.Fatalf("reading the Slack channel: %v", err)
	}
	if _, err := c.UpdateSlackChannel(t.Context(), projectID, client.Fields{"slack_channel_mode": mode}, current.LockVersion); err != nil {
		t.Fatalf("setting the mode to %q outside Terraform: %v", mode, err)
	}
}

// slackChannelPatches returns the slack-channel PATCH bodies, in order.
func slackChannelPatches(t *testing.T, env *testEnv) []map[string]any {
	t.Helper()
	var bodies []map[string]any
	for _, r := range env.fake.RequestsMatching("PATCH", "/api/v1/projects/") {
		if !strings.HasSuffix(r.Path, "/slack-channel") {
			continue
		}
		var body map[string]map[string]any
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatalf("slack-channel PATCH body %s: %v", r.Body, err)
		}
		bodies = append(bodies, body["slack_channel"])
	}
	return bodies
}

func TestProjectSlackChannel_modeRoundTrip(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	identifier := randIdentifier()
	withMode := func(mode string) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = "Slack mode"
  slack_channel = {
    mode = %q
  }`, mode)) + slackModeDataSource(identifier)
	}
	ds := "data.flightdeck_project.x"
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// The channel stays off, so Flightdeck checks nothing in Slack.
				Config: withMode("link"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.mode", "link"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.enabled", "false"),
					resource.TestCheckResourceAttr(ds, "slack_channel.mode", "link"),
				),
			},
			{
				Config: withMode("link"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config: withMode("create"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(projectRes,
							tfjsonpath.New("slack_channel").AtMapKey("mode"), knownvalue.StringExact("create")),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.mode", "create"),
					resource.TestCheckResourceAttr(ds, "slack_channel.mode", "create"),
				),
			},
			{
				// Removing the attribute keeps the stored mode and plans nothing:
				// there is no going back to null.
				Config: projectConfig(env, identifier, `
  name = "Slack mode"
  slack_channel = {}`) + slackModeDataSource(identifier),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.mode", "create"),
					resource.TestCheckResourceAttr(ds, "slack_channel.mode", "create"),
				),
			},
			{
				// Imported with the mode, which a configuration that leaves it
				// unset then keeps.
				ResourceName:            projectRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"features", "slack_channel.event_filter"},
			},
		},
	})
}

// A new project has no mode until someone chooses one, and the read says so
// with a null, on the resource and the data source alike.
func TestProjectSlackChannel_modeIsNullUntilChosen(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Slack no mode"
  slack_channel = {
    notifications_enabled = false
  }`) + slackModeDataSource(identifier),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.notifications_enabled", "false"),
					resource.TestCheckNoResourceAttr(projectRes, "slack_channel.mode"),
					resource.TestCheckNoResourceAttr("data.flightdeck_project.x", "slack_channel.mode"),
				),
			},
		},
	})
}

// A mode changed outside Terraform is drift: put back when the configuration
// sets the mode, and kept when it leaves the mode unset.
func TestProjectSlackChannel_modeDrift(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	identifier := randIdentifier()
	linked := projectConfig(env, identifier, `
  name = "Slack mode drift"
  slack_channel = {
    mode = "link"
  }`)
	unset := projectConfig(env, identifier, `
  name = "Slack mode drift"
  slack_channel = {}`)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: linked,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.mode", "link"),
				),
			},
			{
				PreConfig: func() { setSlackChannelModeElsewhere(t, env, mustInt(id), "create") },
				Config:    linked,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(projectRes,
							tfjsonpath.New("slack_channel").AtMapKey("mode"), knownvalue.StringExact("link")),
					},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "slack_channel.mode", "link"),
			},
			{
				PreConfig: func() { setSlackChannelModeElsewhere(t, env, mustInt(id), "create") },
				Config:    unset,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "slack_channel.mode", "create"),
			},
		},
	})
}

// Only the two exact spellings are accepted, and the plan says so before
// anything reaches Flightdeck. A blank is refused too: Flightdeck would read
// it as "no change", which a configuration can say by leaving it out.
func TestProjectSlackChannel_modeMustBeCreateOrLink(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	for _, mode := range []string{"Create", "", "both"} {
		t.Run(fmt.Sprintf("%q", mode), func(t *testing.T) {
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{
						Config: projectConfig(env, randIdentifier(), fmt.Sprintf(`
  name = "Slack bad mode"
  slack_channel = {
    mode = %q
  }`, mode)),
						ExpectError: regexMust(`(?s)slack_channel\.mode.*value must be one of`),
					},
				},
			})
		})
	}
}

// expectSlackModeCarriedOver checks that a plan leaves slack_channel.mode as
// it is in state: the same value before and after, and not "known after
// apply". A plan that only shows a non-empty diff cannot tell those apart.
type expectSlackModeCarriedOver struct{ want string }

func (e expectSlackModeCarriedOver) CheckPlan(_ context.Context, req plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	for _, rc := range req.Plan.ResourceChanges {
		if rc.Address != projectRes {
			continue
		}
		modeIn := func(side any) (any, bool) {
			sides, _ := side.(map[string]any)
			block, _ := sides["slack_channel"].(map[string]any)
			value, present := block["mode"]
			return value, present
		}
		before, _ := modeIn(rc.Change.Before)
		after, _ := modeIn(rc.Change.After)
		if before != e.want || after != e.want {
			resp.Error = fmt.Errorf("%s: slack_channel.mode planned %v -> %v, want %q carried over unchanged",
				projectRes, before, after, e.want)
			return
		}
		unknowns, _ := rc.Change.AfterUnknown.(map[string]any)
		if whole, _ := unknowns["slack_channel"].(bool); whole {
			resp.Error = fmt.Errorf("%s: the whole slack_channel block is planned as known after apply", projectRes)
			return
		}
		if unknown, _ := modeIn(rc.Change.AfterUnknown); unknown == true {
			resp.Error = fmt.Errorf("%s: slack_channel.mode is planned as known after apply, want %q", projectRes, e.want)
		}
		return
	}
	resp.Error = fmt.Errorf("%s is not in the plan", projectRes)
}

// A change to something else on the project leaves the mode as it is in the
// plan, whether the configuration sets the mode or leaves it unset. The
// second case is the one that needs the plan modifier: an unset Optional +
// Computed value is otherwise planned as known after apply on every change.
func TestProjectSlackChannel_unrelatedChangeLeavesTheModeAlone(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	identifier := randIdentifier()
	config := func(name, block string) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  slack_channel = %s`, name, block))
	}
	set := `{
    mode = "create"
  }`
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config("Slack mode quiet", set),
				Check:  resource.TestCheckResourceAttr(projectRes, "slack_channel.mode", "create"),
			},
			{
				Config: config("Slack mode quiet two", set),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate),
						expectSlackModeCarriedOver{want: "create"},
					},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "slack_channel.mode", "create"),
			},
			{
				// The mode is no longer configured, and the project is renamed.
				Config: config("Slack mode quiet three", `{}`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate),
						expectSlackModeCarriedOver{want: "create"},
					},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "slack_channel.mode", "create"),
			},
		},
	})
}

// --- fake only: anything that turns the channel on, faults or inspects requests

// Changing the mode on a channel that is on but not linked yet points it at
// the channel again, so Flightdeck checks it again under the new mode, as it
// does for turning the channel on or renaming it.
func TestProjectSlackChannel_modeChangeOnAnUnlinkedChannelChecksAgain(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t) // turning the channel on asks Slack for a real channel
	identifier := randIdentifier()
	config := func(mode string) string {
		line := ""
		if mode != "" {
			line = fmt.Sprintf("\n    mode    = %q", mode)
		}
		return projectConfig(env, identifier, `
  name = "Slack mode recheck"
  slack_channel = {
    enabled = true
    name    = "team-web"`+line+`
  }`)
	}
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// No channel has the name, so with no mode chosen it passes the
				// check and waits for the job to create it.
				Config: config(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.linked", "false"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.provision_status", "queued"),
				),
			},
			{
				// Same name, still on, still not linked: only the mode changes,
				// and link has no channel to use.
				Config:      config("link"),
				ExpectError: regexMust(`(?s)Slack channel cannot be used.*No Slack channel named #team-web\.`),
			},
			{
				PreConfig: func() {
					if p := env.fake.Project(mustInt(id)); p.SlackChannelMode != "" {
						t.Errorf("the refused mode change must save nothing, but the mode is %q", p.SlackChannelMode)
					}
				},
				Config:             config("link"),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Create passes the same check: the name is free.
				Config: config("create"),
				Check:  resource.TestCheckResourceAttr(projectRes, "slack_channel.mode", "create"),
			},
			{Config: config("create"), PlanOnly: true},
		},
	})
}

// The mode goes out only when configured. It is never sent as null, which an
// older Flightdeck would refuse as an unknown key.
func TestProjectSlackChannel_modeIsSentOnlyWhenConfigured(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t)
	identifier := randIdentifier()
	config := func(body string) string {
		return projectConfig(env, identifier, `
  name = "Slack mode sent"
  slack_channel = {
`+body+`
  }`)
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config(`    notifications_enabled = false`),
				Check:  resource.TestCheckNoResourceAttr(projectRes, "slack_channel.mode"),
			},
			{
				Config: config(`    notifications_enabled = false
    mode                  = "link"`),
				Check: resource.TestCheckResourceAttr(projectRes, "slack_channel.mode", "link"),
			},
			{
				// No longer configured: kept, and not sent with the next write.
				Config: config(`    notifications_enabled = true`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.mode", "link"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.notifications_enabled", "true"),
				),
			},
			{
				Config:   config(`    notifications_enabled = true`),
				PlanOnly: true,
			},
		},
	})
	patches := slackChannelPatches(t, env)
	if len(patches) != 3 {
		t.Fatalf("expected 3 slack-channel PATCHes, got %d: %v", len(patches), patches)
	}
	for i, wantSent := range []bool{false, true, false} {
		value, sent := patches[i]["slack_channel_mode"]
		switch {
		case sent != wantSent:
			t.Errorf("PATCH %d: slack_channel_mode sent=%t, want %t (body %v)", i+1, sent, wantSent, patches[i])
		case sent && value != "link":
			t.Errorf("PATCH %d: slack_channel_mode = %#v, want \"link\"", i+1, value)
		}
	}
	if p := env.fake.Project(env.fake.AllProjectIDs()[0]); p.SlackChannelMode != "link" {
		t.Errorf("the fake stored mode %q, want link", p.SlackChannelMode)
	}
}

// Create mode refuses a name another channel already has. On an update the
// apply fails on `mode` with Flightdeck's message and saves nothing; the same
// configuration is refused again, and switching to link applies.
func TestProjectSlackChannel_createModeRefusesATakenName(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t) // turning the channel on asks Slack for a real channel
	env.fake.SetSlackChannelExists("team-web", true)
	identifier := randIdentifier()
	config := func(enabled bool, mode string) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = "Slack create taken"
  slack_channel = {
    enabled = %t
    name    = "team-web"
    mode    = %q
  }`, enabled, mode))
	}
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Choosing the mode with the channel off checks nothing.
				Config: config(false, "create"),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.mode", "create"),
					resource.TestCheckResourceAttr(projectRes, "lock_version", "1"),
				),
			},
			{
				Config: config(true, "create"),
				ExpectError: regexMust(`(?s)Slack channel cannot be used.*A Slack channel named #team-web already exists\..*` +
					`change\s+` + "`mode`"),
			},
			{
				// Nothing was saved, so the change is still pending. The project
				// PATCH that runs before the block's write bumped lock_version to
				// 2; the refused write left it there.
				PreConfig: func() {
					if p := env.fake.Project(mustInt(id)); p.SlackChannelEnabled || p.SlackChannelMode != "create" || p.LockVersion != 2 {
						t.Errorf("a refused write must save nothing: enabled=%t lock_version=%d", p.SlackChannelEnabled, p.LockVersion)
					}
				},
				Config:             config(true, "create"),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: config(true, "link"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.enabled", "true"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.mode", "link"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.provision_status", "queued"),
				),
			},
			{
				Config:   config(true, "link"),
				PlanOnly: true,
			},
		},
	})
}

// Link mode refuses a name no channel has. Once the channel exists in Slack,
// the same configuration applies.
func TestProjectSlackChannel_linkModeRefusesAMissingChannel(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t) // turning the channel on asks Slack for a real channel
	identifier := randIdentifier()
	config := projectConfig(env, identifier, `
  name = "Slack link missing"
  slack_channel = {
    enabled = true
    name    = "nowhere"
    mode    = "link"
  }`)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: projectConfig(env, identifier, `  name = "Slack link missing"`), Check: captureAttr(projectRes, "id", &id)},
			{
				Config: config,
				ExpectError: regexMust(`(?s)Slack channel cannot be used.*No Slack channel named #nowhere\..*` +
					`invite the Flightdeck app to it first`),
			},
			{
				PreConfig: func() {
					if p := env.fake.Project(mustInt(id)); p.SlackChannelEnabled || p.SlackChannelMode != "" || p.SlackChannelName != "" {
						t.Errorf("a refused write must save nothing: enabled=%t mode=%q name=%q",
							p.SlackChannelEnabled, p.SlackChannelMode, p.SlackChannelName)
					}
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				PreConfig: func() { env.fake.SetSlackChannelExists("nowhere", true) },
				Config:    config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.enabled", "true"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.mode", "link"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.basename", "nowhere"),
				),
			},
			{Config: config, PlanOnly: true},
		},
	})
}

// On the apply that creates the project, a mode refusal is a warning on
// `mode`, like the bot check's: an error would taint the new project. The
// next plan shows the change again, and a fixed configuration applies as an
// ordinary update of the same project.
func TestProjectSlackChannel_modeRefusalOnCreateWarnsAndRetries(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t) // turning the channel on asks Slack for a real channel
	env.fake.SetSlackChannelExists("team-web", true)
	identifier := randIdentifier()
	config := func(mode string) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = "Slack create mode"
  slack_channel = {
    enabled = true
    name    = "team-web"
    mode    = %q
  }`, mode))
	}
	var id string
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:             config("create"),
				ExpectNonEmptyPlan: true,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					checkNotTainted(projectRes),
				),
			},
			{
				PreConfig: func() {
					p := env.fake.Project(mustInt(id))
					if p.SlackChannelEnabled || p.SlackChannelMode != "" || p.LockVersion != 0 {
						t.Errorf("a refused write must save nothing: enabled=%t mode=%q lock_version=%d",
							p.SlackChannelEnabled, p.SlackChannelMode, p.LockVersion)
					}
				},
				Config:             config("create"),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: config("link"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					func(*terraform.State) error {
						if got := env.fake.AllProjectIDs(); len(got) != 1 {
							return fmt.Errorf("expected the one project to be kept, the fake has %v", got)
						}
						return nil
					},
					resource.TestCheckResourceAttrPtr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.enabled", "true"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.mode", "link"),
				),
			},
			{Config: config("link"), PlanOnly: true},
		},
	})

	if errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError); len(errs) != 0 {
		t.Errorf("no apply may fail, got %d error(s): %s", len(errs), errs[0].Summary)
	}
	warnings := recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning)
	if len(warnings) != 1 {
		t.Fatalf("expected exactly one warning, from the create, got %d", len(warnings))
	}
	warning := warnings[0]
	if warning.Summary != "Slack channel cannot be used" {
		t.Errorf("summary = %q", warning.Summary)
	}
	for _, want := range []string{"A Slack channel named #team-web already exists.", "The project was created", "change `mode`"} {
		if !strings.Contains(warning.Detail, want) {
			t.Errorf("warning detail does not carry %q:\n%s", want, warning.Detail)
		}
	}
	if want := tftypes.NewAttributePath().WithAttributeName("slack_channel").WithAttributeName("mode"); !warning.Attribute.Equal(want) {
		t.Errorf("warning should point at %s, got %s", want, warning.Attribute)
	}
}

// Switching a linked channel to create checks it again, and refuses it when
// Flightdeck did not make it. A mode change never drops the link: the refused
// switch keeps it, and a switch to link keeps the same channel.
func TestProjectSlackChannel_linkedChannelSwitchedToCreateIsCheckedAgain(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t) // turning the channel on asks Slack for a real channel
	env.fake.SetSlackChannelExists("team-web", true)
	identifier := randIdentifier()
	config := func(mode string) string {
		line := ""
		if mode != "" {
			line = fmt.Sprintf("\n    mode    = %q", mode)
		}
		return projectConfig(env, identifier, `
  name = "Slack recheck"
  slack_channel = {
    enabled = true
    name    = "team-web"`+line+`
  }`)
	}
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Nobody chose a mode, so the existing channel is linked.
				Config: config(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckNoResourceAttr(projectRes, "slack_channel.mode"),
				),
			},
			{
				PreConfig:   func() { env.fake.CompleteSlackProvision(mustInt(id), "C0TEAMWEB") },
				Config:      config("create"),
				ExpectError: regexMust(`(?s)Slack channel cannot be used.*A Slack channel named #team-web already exists\.`),
			},
			{
				PreConfig: func() {
					p := env.fake.Project(mustInt(id))
					if p.SlackChannelMode != "" || p.SlackChannelID != "C0TEAMWEB" {
						t.Errorf("the refused switch must save nothing and keep the link: mode=%q channel=%q",
							p.SlackChannelMode, p.SlackChannelID)
					}
				},
				Config:             config("create"),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: config("link"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.mode", "link"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.linked", "true"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.channel_id", "C0TEAMWEB"),
				),
			},
			{Config: config("link"), PlanOnly: true},
		},
	})
}

// A Flightdeck older than the mode: a project whose configuration leaves the
// mode unset works as before (the mode reads null and is never sent). Setting
// it fails on the attribute with an upgrade hint, saves nothing, and applies
// once Flightdeck is upgraded.
func TestProjectSlackChannel_modeOnAnOlderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t)
	env.fake.SetSlackChannelModeSupported(false)
	identifier := randIdentifier()
	withMode := projectConfig(env, identifier, `
  name = "Slack old mode"
  slack_channel = {
    notifications_enabled = true
    mode                  = "link"
  }`)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Slack old mode"
  slack_channel = {
    notifications_enabled = false
  }`) + slackModeDataSource(identifier),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.notifications_enabled", "false"),
					resource.TestCheckNoResourceAttr(projectRes, "slack_channel.mode"),
					resource.TestCheckNoResourceAttr("data.flightdeck_project.x", "slack_channel.mode"),
				),
			},
			{
				Config: withMode,
				ExpectError: regexMust(`(?s)This Flightdeck does not support slack_channel\.mode yet.*` +
					`Upgrade\s+Flightdeck\s+first.*unknown key: slack_channel_mode`),
			},
			{
				// Nothing was saved, the notifications switch included.
				PreConfig: func() {
					if p := env.fake.Project(mustInt(id)); p.Features["slack"] {
						t.Errorf("a refused write must save nothing, but the notifications switch is on")
					}
				},
				Config:             withMode,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Flightdeck is upgraded; the same configuration applies.
				PreConfig: func() { env.fake.SetSlackChannelModeSupported(true) },
				Config:    withMode,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.mode", "link"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.notifications_enabled", "true"),
				),
			},
			{Config: withMode, PlanOnly: true},
		},
	})
	patches := slackChannelPatches(t, env)
	if len(patches) == 0 {
		t.Fatal("no slack-channel PATCH was recorded")
	}
	if _, sent := patches[0]["slack_channel_mode"]; sent {
		t.Errorf("an unset mode must not be sent to an older Flightdeck: %v", patches[0])
	}
}

// On the apply that creates a project, an older Flightdeck's refusal of the
// mode is a warning, for the same reason as a refused channel: an error would
// taint the new project. The next apply is an update of the same project.
func TestProjectSlackChannel_modeOnAnOlderFlightdeckAtCreateWarns(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t)
	env.fake.SetSlackChannelModeSupported(false)
	identifier := randIdentifier()
	config := projectConfig(env, identifier, `
  name = "Slack old create"
  slack_channel = {
    notifications_enabled = false
    mode                  = "link"
  }`)
	var id string
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:             config,
				ExpectNonEmptyPlan: true,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					checkNotTainted(projectRes),
				),
			},
			{
				PreConfig: func() {
					if _, stored := env.fake.Project(mustInt(id)).Features["slack"]; stored {
						t.Errorf("a refused write must save nothing, but the notifications switch was stored")
					}
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				PreConfig: func() { env.fake.SetSlackChannelModeSupported(true) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.mode", "link"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.notifications_enabled", "false"),
				),
			},
			{Config: config, PlanOnly: true},
		},
	})

	if errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError); len(errs) != 0 {
		t.Errorf("no apply may fail, got %d error(s): %s", len(errs), errs[0].Summary)
	}
	warnings := recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning)
	if len(warnings) != 1 {
		t.Fatalf("expected exactly one warning, from the create, got %d", len(warnings))
	}
	warning := warnings[0]
	if warning.Summary != "This Flightdeck does not support slack_channel.mode yet" {
		t.Errorf("summary = %q", warning.Summary)
	}
	for _, want := range []string{"The project was created", "unknown key: slack_channel_mode"} {
		if !strings.Contains(warning.Detail, want) {
			t.Errorf("warning detail does not carry %q:\n%s", want, warning.Detail)
		}
	}
	if want := tftypes.NewAttributePath().WithAttributeName("slack_channel").WithAttributeName("mode"); !warning.Attribute.Equal(want) {
		t.Errorf("warning should point at %s, got %s", want, warning.Attribute)
	}
}

// Where writeSlackChannel points a mode refusal on an update: at `mode` when
// the configuration sets it, otherwise where the bot check's refusal goes (the
// configured name, or the block). Nothing is recorded, since nothing was saved.
func TestWriteSlackChannel_modeRefusalPointsAtMode(t *testing.T) {
	name := func(s string) attr.Value { return slackChannelNameValue{StringValue: types.StringValue(s)} }
	cases := []struct {
		name     string
		stored   string // a mode chosen outside Terraform before the write
		exists   bool
		archived bool
		config   map[string]attr.Value
		at       path.Path
		message  string
		// slackFix: the refusal is about the channel in Slack, not the mode,
		// so the advice is to fix the channel.
		slackFix bool
	}{
		{
			name:    "create configured, the name is taken",
			exists:  true,
			config:  map[string]attr.Value{"enabled": types.BoolValue(true), "name": name("team-web"), "mode": types.StringValue("create")},
			at:      path.Root("slack_channel").AtName("mode"),
			message: "A Slack channel named #team-web already exists.",
		},
		{
			name:    "link configured, no such channel",
			config:  map[string]attr.Value{"enabled": types.BoolValue(true), "name": name("team-web"), "mode": types.StringValue("link")},
			at:      path.Root("slack_channel").AtName("mode"),
			message: "No Slack channel named #team-web.",
		},
		{
			name:    "link chosen in Flightdeck, the name configured",
			stored:  "link",
			config:  map[string]attr.Value{"enabled": types.BoolValue(true), "name": name("team-web")},
			at:      path.Root("slack_channel").AtName("name"),
			message: "No Slack channel named #team-web.",
		},
		{
			name:    "link chosen in Flightdeck, the project-name default",
			stored:  "link",
			config:  map[string]attr.Value{"enabled": types.BoolValue(true)},
			at:      path.Root("slack_channel"),
			message: "No Slack channel named #fd-slack-mode.",
		},
		{
			// An archived channel is refused as archived in either mode, and
			// that is about the channel, so it points at the name.
			name:     "create configured, the channel is archived",
			archived: true,
			config:   map[string]attr.Value{"enabled": types.BoolValue(true), "name": name("team-web"), "mode": types.StringValue("create")},
			at:       path.Root("slack_channel").AtName("name"),
			message:  "#team-web is archived. Unarchive it in Slack or choose a different channel name, then try again.",
			slackFix: true,
		},
		{
			name:     "link configured, the channel is archived",
			archived: true,
			config:   map[string]attr.Value{"enabled": types.BoolValue(true), "name": name("team-web"), "mode": types.StringValue("link")},
			at:       path.Root("slack_channel").AtName("name"),
			message:  "#team-web is archived. Unarchive it in Slack or choose a different channel name, then try again.",
			slackFix: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, "slack_channel")
			env.requireFake(t)
			if tc.exists {
				env.fake.SetSlackChannelExists("team-web", true)
			}
			if tc.archived {
				env.fake.SetSlackChannelArchived("team-web", true)
			}
			c, id := slackChannelTestProject(t, env, "Slack mode")
			ctx := t.Context()
			if tc.stored != "" {
				env.fake.SetSlackChannelMode(id, tc.stored)
			}
			var diags diag.Diagnostics
			prior := readSlackChannel(ctx, c, id, types.MapNull(types.BoolType), slackEventsManaged, &diags)
			if diags.HasError() {
				t.Fatalf("reading the block: %v", diags.Errors())
			}
			before := env.fake.Project(id).LockVersion
			block, lockVersion := writeSlackChannel(ctx, c, id, slackChannelBlock(t, tc.config),
				types.ObjectUnknown(slackChannelAttrTypes), prior, before, slackChannelOnUpdate, &diags)

			if !block.IsNull() {
				t.Errorf("a refused write must not record the block, got %s", block)
			}
			if lockVersion != before {
				t.Errorf("a refused write saves nothing, so lock_version must stay %d, got %d", before, lockVersion)
			}
			errs := diags.Errors()
			if len(errs) != 1 || len(diags.Warnings()) != 0 {
				t.Fatalf("expected exactly one error and no warning, got %v", diags)
			}
			withPath, ok := errs[0].(diag.DiagnosticWithPath)
			if !ok || !withPath.Path().Equal(tc.at) {
				t.Errorf("error should point at %s, got %#v", tc.at, errs[0])
			}
			if got := errs[0].Summary(); got != "Slack channel cannot be used" {
				t.Errorf("summary = %q", got)
			}
			detail := errs[0].Detail()
			advice, notAdvice := "change `mode`", "Fix the channel in Slack"
			if tc.slackFix {
				advice, notAdvice = notAdvice, advice
			}
			if !strings.HasPrefix(detail, tc.message) {
				t.Errorf("detail should lead with Flightdeck's message %q:\n%s", tc.message, detail)
			}
			for _, want := range []string{advice, "saved none"} {
				if !strings.Contains(detail, want) {
					t.Errorf("detail does not carry %q:\n%s", want, detail)
				}
			}
			if strings.Contains(detail, notAdvice) {
				t.Errorf("detail gives the wrong advice (%q):\n%s", notAdvice, detail)
			}
			if p := env.fake.Project(id); p.SlackChannelEnabled || p.LockVersion != before {
				t.Errorf("the fake saved a refused write: enabled=%t lock_version=%d", p.SlackChannelEnabled, p.LockVersion)
			}
		})
	}
}

// Only a mode refusal points at `mode`. The bot check's refusal (a channel
// Flightdeck cannot post to) keeps pointing at the name, with its own advice,
// even when the configuration sets a mode.
func TestAddSlackChannelUnusable_onlyAModeRefusalPointsAtMode(t *testing.T) {
	config := slackChannelBlock(t, map[string]attr.Value{
		"enabled": types.BoolValue(true),
		"name":    slackChannelNameValue{StringValue: types.StringValue("example-channel")},
		"mode":    types.StringValue("link"),
	})
	refusal := func(message string) error {
		return &client.Error{Method: "PATCH", Path: "/api/v1/projects/1/slack-channel", Status: 422,
			Code: client.CodeSlackChannelUnusable, Message: message}
	}
	cases := []struct {
		message string
		at      path.Path
		advice  string
	}{
		{
			message: "#example-channel is archived. Unarchive it in Slack or choose a different channel name, then try again.",
			at:      path.Root("slack_channel").AtName("name"),
			advice:  "Fix the channel in Slack",
		},
		{
			message: "No Slack channel named #example-channel. Check the name, or create a new channel instead. " +
				"If it's private, invite the Flightdeck app to it first.",
			at:     path.Root("slack_channel").AtName("mode"),
			advice: "change `mode`",
		},
	}
	for _, tc := range cases {
		var diags diag.Diagnostics
		addSlackChannelUnusable(t.Context(), config, refusal(tc.message), slackChannelOnUpdate, &diags)
		if len(diags.Errors()) != 1 {
			t.Fatalf("%q: expected one error, got %v", tc.message, diags)
		}
		d := diags.Errors()[0]
		withPath, ok := d.(diag.DiagnosticWithPath)
		if !ok || !withPath.Path().Equal(tc.at) {
			t.Errorf("%q: should point at %s, got %#v", tc.message, tc.at, d)
		}
		if !strings.HasPrefix(d.Detail(), tc.message) || !strings.Contains(d.Detail(), tc.advice) {
			t.Errorf("%q: detail should lead with the API's message and say %q:\n%s", tc.message, tc.advice, d.Detail())
		}
	}
}

// When the read that would tell whether Flightdeck knows the mode fails, the
// refusal is reported as it came, closed with a note on what an unknown key
// means. A refusal from a Flightdeck that does know the mode gets no such
// hint: the read, not the wording, decides.
func TestWriteSlackChannel_modeUpgradeHintNeedsTheReadToSaySo(t *testing.T) {
	t.Run("older Flightdeck, the read fails", func(t *testing.T) {
		env := newTestEnv(t, "slack_channel")
		env.requireFake(t)
		env.fake.SetSlackChannelModeSupported(false)
		c, id := slackChannelTestProject(t, env, "Slack old read")
		route := fmt.Sprintf("/api/v1/projects/%d/slack-channel", id)
		env.fake.OnNextRequest("GET", route, func() { env.fake.SetSlackChannelEndpoint(false) })

		var diags diag.Diagnostics
		writeSlackChannel(t.Context(), c, id, slackChannelBlock(t, map[string]attr.Value{"mode": types.StringValue("link")}),
			types.ObjectUnknown(slackChannelAttrTypes), types.ObjectNull(slackChannelAttrTypes), 0, slackChannelOnUpdate, &diags)

		errs := diags.Errors()
		if len(errs) != 1 {
			t.Fatalf("expected exactly one error, got %v", diags)
		}
		if got := errs[0].Summary(); got != "Error updating Flightdeck Slack channel configuration" {
			t.Errorf("summary = %q", got)
		}
		for _, want := range []string{"unknown key: slack_channel_mode", slackChannelModeUnknownNote} {
			if !strings.Contains(errs[0].Detail(), want) {
				t.Errorf("detail does not carry %q:\n%s", want, errs[0].Detail())
			}
		}
	})
	t.Run("a Flightdeck with the mode refuses something else", func(t *testing.T) {
		env := newTestEnv(t, "slack_channel")
		env.requireFake(t)
		c, id := slackChannelTestProject(t, env, "Slack other refusal")
		// A name with no letter or digit, which the plan would refuse, built
		// here directly so Flightdeck refuses it instead.
		config := slackChannelBlock(t, map[string]attr.Value{
			"mode": types.StringValue("link"),
			"name": slackChannelNameValue{StringValue: types.StringValue("!!!")},
		})
		var diags diag.Diagnostics
		writeSlackChannel(t.Context(), c, id, config, types.ObjectUnknown(slackChannelAttrTypes),
			types.ObjectNull(slackChannelAttrTypes), 0, slackChannelOnUpdate, &diags)

		errs := diags.Errors()
		if len(errs) != 1 {
			t.Fatalf("expected exactly one error, got %v", diags)
		}
		if got := errs[0].Summary(); got != "Error updating Flightdeck Slack channel configuration" {
			t.Errorf("summary = %q", got)
		}
		detail := errs[0].Detail()
		if !strings.Contains(detail, "normalizes to an empty Slack channel name") {
			t.Errorf("detail does not carry the API's refusal:\n%s", detail)
		}
		if strings.Contains(detail, "older than the release") || strings.Contains(strings.ToLower(detail), "upgrade") {
			t.Errorf("a Flightdeck that knows the mode must not be told to upgrade:\n%s", detail)
		}
	})
}

// The mode carries through the read mapping as the API reports it: a chosen
// mode, or null for nobody's choice and for an older Flightdeck alike.
func TestSlackChannelToObject_mode(t *testing.T) {
	link := client.SlackChannelModeLink
	for _, tc := range []struct {
		mode *string
		want types.String
	}{
		{mode: &link, want: types.StringValue("link")},
		{mode: nil, want: types.StringNull()},
	} {
		var diags diag.Diagnostics
		obj := slackChannelToObject(t.Context(), &client.SlackChannel{Mode: tc.mode}, types.MapNull(types.BoolType), slackEventsManaged, &diags)
		if diags.HasError() {
			t.Fatal(diags.Errors())
		}
		if got := obj.Attributes()["mode"]; !got.Equal(tc.want) {
			t.Errorf("mode = %s, want %s", got, tc.want)
		}
	}
}

// slackChannelWriteNeeded: a configured mode that differs from the stored one
// (null included) needs a write; an unset one never does.
func TestSlackChannelWriteNeeded_mode(t *testing.T) {
	stored := func(mode types.String) types.Object {
		return slackChannelBlock(t, map[string]attr.Value{
			"enabled":               types.BoolValue(false),
			"notifications_enabled": types.BoolValue(true),
			"name":                  slackChannelNameValue{StringValue: types.StringValue("")},
			"mode":                  mode,
		})
	}
	configured := func(mode types.String) types.Object {
		return slackChannelBlock(t, map[string]attr.Value{"mode": mode})
	}
	cases := []struct {
		name          string
		config, state types.Object
		want          bool
	}{
		{"same mode", configured(types.StringValue("link")), stored(types.StringValue("link")), false},
		{"other mode", configured(types.StringValue("create")), stored(types.StringValue("link")), true},
		{"nobody chose yet", configured(types.StringValue("link")), stored(types.StringNull()), true},
		{"unset keeps the stored mode", configured(types.StringNull()), stored(types.StringValue("link")), false},
		{"unset, nobody chose", configured(types.StringNull()), stored(types.StringNull()), false},
		{"not known until apply", configured(types.StringUnknown()), stored(types.StringValue("link")), false},
	}
	for _, tc := range cases {
		var diags diag.Diagnostics
		if got := slackChannelWriteNeeded(t.Context(), tc.config, tc.state, &diags); got != tc.want {
			t.Errorf("%s: slackChannelWriteNeeded = %t, want %t", tc.name, got, tc.want)
		}
		if diags.HasError() {
			t.Errorf("%s: %v", tc.name, diags.Errors())
		}
	}
}

// The two mode refusals match; Flightdeck's bot and archived refusals, worded
// as Flightdeck words them, do not.
func TestIsSlackChannelModeRefusal(t *testing.T) {
	cases := map[string]bool{
		"A Slack channel named #team-web already exists. To use it, link it instead of creating a new one.": true,
		"No Slack channel named #team-web. Check the name, or create a new channel instead. " +
			"If it's private, invite the Flightdeck app to it first.": true,
		"#team-web exists, but Flightdeck can't post to it: it's private and " + flightdecktest.SlackBotHandle +
			" isn't a member, or it's archived. Invite " + flightdecktest.SlackBotHandle +
			" to #team-web (or unarchive it), then try again.": false,
		"#team-web is archived. Unarchive it in Slack or choose a different channel name, then try again.": false,
		"": false,
	}
	for message, want := range cases {
		if got := isSlackChannelModeRefusal(message); got != want {
			t.Errorf("isSlackChannelModeRefusal(%q) = %t, want %t", message, got, want)
		}
	}
}
