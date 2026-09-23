package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// Enabling a project's Slack channel asks Flightdeck to create a channel in
// the workspace's Slack and invite people into it, so every test that sets
// `enabled` runs against the fake only. The live legs exercise everything that
// changes configuration without provisioning anything: the notifications
// switch, the channel name, the event filter and the read applying back.

func TestProjectSlackChannel_configurationRoundTrip(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	identifier := randIdentifier()
	config := projectConfig(env, identifier, `
  name = "Slack channel"
  slack_channel = {
    notifications_enabled = false
    name                  = "team-web"
    event_filter = {
      logged = true
    }
  }`)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.notifications_enabled", "false"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.name", "team-web"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.basename", "team-web"),
					// The filter records only the category the configuration manages.
					resource.TestCheckResourceAttr(projectRes, "slack_channel.event_filter.%", "1"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.event_filter.logged", "true"),
					// Nothing was enabled, so nothing provisioned.
					resource.TestCheckResourceAttr(projectRes, "slack_channel.enabled", "false"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.linked", "false"),
					resource.TestCheckNoResourceAttr(projectRes, "slack_channel.channel_id"),
					resource.TestCheckNoResourceAttr(projectRes, "slack_channel.provision_status"),
				),
			},
			{
				// What a provider does on every plan: hold the read as state and
				// send it back as desired state. Nothing to reconcile.
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				ResourceName:      projectRes,
				ImportState:       true,
				ImportStateVerify: true,
				// features: an import records every settable key. event_filter:
				// an import has no configuration to defer to, so it manages no
				// categories rather than guessing at the five.
				ImportStateVerifyIgnore: []string{"features", "slack_channel.event_filter"},
			},
		},
	})
}

// longChannelName is 84 characters, so the 80-character cut the API applies
// when it DERIVES a channel name lands on the separator before "tail". The
// name itself is stored whole.
var longChannelName = strings.Repeat("a", 79) + " tail"

func TestProjectSlackChannel_nameIsStoredAsTypedAndResettable(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	identifier := randIdentifier()
	named := projectConfig(env, identifier, `
  name = "Slack naming"
  slack_channel = {
    name = "  My Team Channel!  "
  }`)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: named,
				Check: resource.ComposeAggregateTestCheckFunc(
					// Stored as typed, so the read is a fixed point of the write;
					// only the padding the server trims is absorbed here.
					resource.TestCheckResourceAttr(projectRes, "slack_channel.name", "  My Team Channel!  "),
					// The Slack-legal name is derived from it, and reported apart.
					resource.TestCheckResourceAttr(projectRes, "slack_channel.basename", "my-team-channel"),
				),
			},
			{
				// …and that difference is not a diff.
				Config:   named,
				PlanOnly: true,
			},
			{
				// Removing the attribute keeps whatever the project has…
				Config: projectConfig(env, identifier, `
  name = "Slack naming"
  slack_channel = {
    notifications_enabled = true
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "slack_channel.basename", "my-team-channel"),
			},
			{
				// …and writing the empty string clears the override, so the
				// effective name goes back to the project-name default.
				Config: projectConfig(env, identifier, `
  name = "Slack naming"
  slack_channel = {
    name = ""
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.name", ""),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.basename", "fd-slack-naming"),
				),
			},
			{
				// A name past the cap: it is stored whole, and only the derived
				// name is cut — here on a separator, so `basename` ends in `-`.
				Config: projectConfig(env, identifier, fmt.Sprintf(`
  name = "Slack naming"
  slack_channel = {
    name = %q
  }`, longChannelName)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.name", longChannelName),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.basename", strings.Repeat("a", 79)+"-"),
				),
			},
			{
				Config: projectConfig(env, identifier, fmt.Sprintf(`
  name = "Slack naming"
  slack_channel = {
    name = %q
  }`, longChannelName)),
				PlanOnly: true,
			},
		},
	})
}

func TestProjectSlackChannel_nameThatDerivesToNothing(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, randIdentifier(), `
  name = "Slack naming"
  slack_channel = {
    name = "!!!"
  }`),
				ExpectError: regexMust(`empty Slack channel name`),
			},
		},
	})
}

func TestProjectSlackChannel_eventFilterMerges(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	identifier := randIdentifier()
	dataSource := fmt.Sprintf(`
data "flightdeck_project" "x" {
  identifier = %q
  depends_on = [flightdeck_project.test]
}
`, identifier)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Slack events"
  slack_channel = {
    event_filter = {
      logged = true
    }
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "slack_channel.event_filter.logged", "true"),
			},
			{
				// The API merges the filter, so the earlier override survives a
				// write that does not name it. State records only what this
				// configuration manages; the data source reports every category.
				Config: projectConfig(env, identifier, `
  name = "Slack events"
  slack_channel = {
    event_filter = {
      created = false
    }
  }`) + dataSource,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.event_filter.%", "1"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.event_filter.created", "false"),
					resource.TestCheckResourceAttr("data.flightdeck_project.x", "slack_channel.event_filter.created", "false"),
					resource.TestCheckResourceAttr("data.flightdeck_project.x", "slack_channel.event_filter.logged", "true"),
					// Categories nobody has touched keep their defaults.
					resource.TestCheckResourceAttr("data.flightdeck_project.x", "slack_channel.event_filter.state_changed", "true"),
					resource.TestCheckResourceAttr("data.flightdeck_project.x", "slack_channel.event_filter.field_changed", "false"),
					resource.TestCheckResourceAttr("data.flightdeck_project.x", "slack_channel.event_filter.assigned", "true"),
				),
			},
		},
	})
}

func TestProjectSlackChannel_unknownEventCategory(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, randIdentifier(), `
  name = "Slack events"
  slack_channel = {
    event_filter = {
      commented = true
    }
  }`),
				ExpectError: regexMust(`value must be one of`),
			},
		},
	})
}

// The notifications master switch is the project's `slack` feature, and this
// block is the only door onto it.
func TestProjectSlackChannel_notificationsSwitchIsTheSlackFeature(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Slack feature"
  features = {
    slack = false
  }`),
				ExpectError: regexMust(`value must be one of`),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Slack feature"
  slack_channel = {
    notifications_enabled = false
  }`) + fmt.Sprintf(`
data "flightdeck_project" "x" {
  identifier = %q
  depends_on = [flightdeck_project.test]
}
`, identifier),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.notifications_enabled", "false"),
					// Same state, seen through the project's feature map.
					resource.TestCheckResourceAttr("data.flightdeck_project.x", "features.slack", "false"),
					// The switch merges into the features jsonb: other keys survive.
					resource.TestCheckResourceAttr("data.flightdeck_project.x", "features.cycles", "true"),
					// No channel-name override spells itself as the empty string.
					resource.TestCheckResourceAttr(projectRes, "slack_channel.name", ""),
				),
			},
		},
	})
}

func TestProjectDataSource_slackChannel(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Slack DS"
  slack_channel = {
    name = "ds-channel"
  }`) + fmt.Sprintf(`
data "flightdeck_project" "x" {
  identifier = %q
  depends_on = [flightdeck_project.test]
}
`, identifier),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.flightdeck_project.x", "slack_channel.name", "ds-channel"),
					resource.TestCheckResourceAttr("data.flightdeck_project.x", "slack_channel.basename", "ds-channel"),
					resource.TestCheckResourceAttr("data.flightdeck_project.x", "slack_channel.enabled", "false"),
					// Every category, resolved against its default.
					resource.TestCheckResourceAttr("data.flightdeck_project.x", "slack_channel.event_filter.%", "5"),
				),
			},
		},
	})
}

// --- fake only: anything that provisions, faults or inspects requests -------

func TestProjectSlackChannel_provisioningIsAsynchronous(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t) // enabling asks Slack for a real channel
	identifier := randIdentifier()
	enabled := projectConfig(env, identifier, `
  name = "Slack async"
  slack_channel = {
    enabled = true
  }`)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: enabled,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					// The write reports the enqueue, not a finished channel.
					resource.TestCheckResourceAttr(projectRes, "slack_channel.enabled", "true"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.provision_status", "queued"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.linked", "false"),
					resource.TestCheckNoResourceAttr(projectRes, "slack_channel.channel_id"),
				),
			},
			{
				// The job lands between applies. The refresh picks the channel
				// up and none of it produces a diff.
				PreConfig: func() { env.fake.CompleteSlackProvision(mustInt(id), "C0TEST") },
				Config:    enabled,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.channel_id", "C0TEST"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.linked", "true"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.provision_status", "linked"),
				),
			},
			{
				// Renaming the channel drops the link so the next provision
				// re-links; the old channel is left alone on Slack.
				Config: projectConfig(env, identifier, `
  name = "Slack async"
  slack_channel = {
    enabled = true
    name    = "somewhere-else"
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.basename", "somewhere-else"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.linked", "false"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.provision_status", "queued"),
					resource.TestCheckNoResourceAttr(projectRes, "slack_channel.channel_id"),
				),
			},
		},
	})
}

func TestProjectSlackChannel_partialWriteLeavesOtherKeysAlone(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t) // enabling asks Slack for a real channel
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Slack partial"
  slack_channel = {
    enabled = true
    name    = "keep-me"
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "slack_channel.enabled", "true"),
			},
			{
				// `enabled` is no longer configured, so it is not sent and the
				// project keeps it. Only the submitted keys change.
				Config: projectConfig(env, identifier, `
  name = "Slack partial"
  slack_channel = {
    name                  = "keep-me"
    notifications_enabled = false
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.enabled", "true"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.notifications_enabled", "false"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.basename", "keep-me"),
				),
			},
		},
	})
}

// A write to this endpoint re-enqueues provisioning, so an apply that changes
// nothing here must not make one.
func TestProjectSlackChannel_unrelatedUpdateDoesNotRewrite(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Slack untouched"
  slack_channel = {
    name = "untouched"
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "slack_channel.basename", "untouched"),
			},
			{
				// The project is renamed and the block is left exactly as it
				// was: the PATCH must carry the rename only.
				Config: projectConfig(env, identifier, `
  name = "Slack untouched renamed"
  slack_channel = {
    name = "untouched"
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "name", "Slack untouched renamed"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.basename", "untouched"),
				),
			},
			{
				// Dropping the block entirely leaves the configuration alone
				// and plans nothing.
				Config: projectConfig(env, identifier, `  name = "Slack untouched renamed"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "slack_channel.basename", "untouched"),
			},
		},
	})
	var projectPatches, slackPatches int
	for _, r := range env.fake.RequestsMatching("PATCH", "/api/v1/projects/") {
		if strings.HasSuffix(r.Path, "/slack-channel") {
			slackPatches++
		} else {
			projectPatches++
		}
	}
	if projectPatches != 1 {
		t.Fatalf("expected exactly one project PATCH (the rename), got %d", projectPatches)
	}
	if slackPatches != 1 {
		t.Fatalf("expected the rename to leave the Slack channel alone, saw %d slack-channel PATCHes", slackPatches)
	}
}

func TestProjectSlackChannel_writesGoToTheOwnEndpointWithTheProjectLockVersion(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Slack transport"
  slack_channel = {
    notifications_enabled = false
    event_filter = {
      logged = true
    }
  }`),
				// Created at 0, bumped once by the Slack channel write.
				Check: resource.TestCheckResourceAttr(projectRes, "lock_version", "1"),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Slack transport"
  slack_channel = {
    notifications_enabled = true
    event_filter = {
      logged = true
    }
  }`),
				// Project PATCH (2) then the Slack channel PATCH (3).
				Check: resource.TestCheckResourceAttr(projectRes, "lock_version", "3"),
			},
		},
	})
	var patches []flightdecktestRequest
	for _, r := range env.fake.RequestsMatching("PATCH", "/api/v1/projects/") {
		if strings.HasSuffix(r.Path, "/slack-channel") {
			patches = append(patches, r)
		}
	}
	if len(patches) != 2 {
		t.Fatalf("expected 2 slack-channel PATCHes, got %d", len(patches))
	}
	var body map[string]map[string]any
	if err := json.Unmarshal(patches[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	settings := body["slack_channel"]
	if settings["slack_notifications_enabled"] != false {
		t.Errorf("slack-channel PATCH body = %s", patches[0].Body)
	}
	// An unset attribute is not sent at all: the API changes only what it is given.
	if raw, sent := settings["slack_channel_name"]; sent {
		t.Errorf("an unset name must not be sent, got %v (%s)", raw, patches[0].Body)
	}
	for key := range settings {
		if !slices.Contains(client.SlackChannelWritableKeys, key) {
			t.Errorf("slack-channel PATCH must send writable keys only, got %q", key)
		}
	}
	if patches[0].Header.Get("If-Match") != `"0"` || patches[1].Header.Get("If-Match") != `"2"` {
		t.Errorf("slack-channel If-Match must be the project's current lock_version: %q, %q",
			patches[0].Header.Get("If-Match"), patches[1].Header.Get("If-Match"))
	}
}

// Two states that look like success and provision nothing. Both store the
// configuration, so both are warnings rather than errors — the fix is an
// interactive authorization the API cannot perform.
func TestProjectSlackChannel_nothingProvisionsWithoutSlack(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t)
	env.fake.SetSlackIntegration(false, false)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Slack absent"
  slack_channel = {
    enabled = true
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					// The configuration is stored…
					resource.TestCheckResourceAttr(projectRes, "slack_channel.enabled", "true"),
					// …and this is how a client sees why nothing happened.
					resource.TestCheckResourceAttr(projectRes, "slack_channel.available", "false"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.scopes_sufficient", "false"),
					resource.TestCheckNoResourceAttr(projectRes, "slack_channel.provision_status"),
				),
			},
		},
	})
}

func TestProjectSlackChannel_insufficientScopes(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t)
	env.fake.SetSlackIntegration(true, false)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Slack scopes"
  slack_channel = {
    enabled = true
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.available", "true"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.scopes_sufficient", "false"),
					// Enqueued all the same; the job is what will fail.
					resource.TestCheckResourceAttr(projectRes, "slack_channel.provision_status", "queued"),
				),
			},
		},
	})
}

func TestProjectSlackChannel_warningsForTheNoProvisionStates(t *testing.T) {
	failed, linked := "failed", "linked"
	note := "Invite @example-bot to #example-channel"
	blank := "  "
	healthy := client.SlackChannel{ChannelEnabled: true, ChannelAvailable: true, ScopesSufficient: true}
	withStatus := func(sc client.SlackChannel, status, note *string) client.SlackChannel {
		sc.ProvisionStatus, sc.ProvisionNote = status, note
		return sc
	}
	cases := []struct {
		name    string
		channel client.SlackChannel
		want    string
		detail  string
	}{
		{
			name:    "no integration",
			channel: client.SlackChannel{ChannelEnabled: true},
			want:    "no Slack integration",
		},
		{
			name:    "scopes too narrow",
			channel: client.SlackChannel{ChannelEnabled: true, ChannelAvailable: true},
			want:    "lacks the channel scopes",
		},
		{
			name:    "healthy",
			channel: healthy,
		},
		{
			name:    "linked",
			channel: withStatus(healthy, &linked, nil),
		},
		{
			name:    "provision failed",
			channel: withStatus(healthy, &failed, &note),
			want:    "provisioning failed",
			detail:  note,
		},
		{
			name:    "provision failed without a note",
			channel: withStatus(healthy, &failed, &blank),
			want:    "provisioning failed",
			detail:  "did not say why",
		},
		{
			// A failure left over from before the channel was switched off is
			// not worth a warning.
			name:    "failed, but the channel is off",
			channel: withStatus(client.SlackChannel{ChannelAvailable: true, ScopesSufficient: true}, &failed, &note),
		},
		{
			name:    "channel off",
			channel: client.SlackChannel{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			slackChannelProvisioningWarnings(&tc.channel, &diags)
			if diags.HasError() {
				t.Fatalf("a stored configuration must not be an error: %v", diags.Errors())
			}
			warnings := diags.Warnings()
			if tc.want == "" {
				if len(warnings) != 0 {
					t.Fatalf("expected no warning, got %v", warnings)
				}
				return
			}
			if len(warnings) != 1 {
				t.Fatalf("expected exactly one warning, got %v", warnings)
			}
			if !strings.Contains(warnings[0].Summary(), tc.want) {
				t.Errorf("warning %q does not mention %q", warnings[0].Summary(), tc.want)
			}
			if !strings.Contains(warnings[0].Detail(), tc.detail) {
				t.Errorf("warning detail does not carry %q:\n%s", tc.detail, warnings[0].Detail())
			}
		})
	}
}

func TestProjectSlackChannel_nonAdminToken(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t)
	env.fake.SetProjectAdmin(false)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Not reported to this token: the block is null and nothing is sent.
				Config: projectConfig(env, identifier, `  name = "Slack non-admin"`),
				Check:  resource.TestCheckNoResourceAttr(projectRes, "slack_channel.%"),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Slack non-admin"
  slack_channel = {
    notifications_enabled = false
  }`),
				ExpectError: regexMust(`Slack channel configuration requires a project admin`),
			},
		},
	})
}

func TestProjectSlackChannel_endpointAbsent(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t)
	env.fake.SetSlackChannelEndpoint(false)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// A Flightdeck without the endpoint: the block reads as null.
				Config: projectConfig(env, identifier, `  name = "Slack old server"`),
				Check:  resource.TestCheckNoResourceAttr(projectRes, "slack_channel.%"),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Slack old server"
  slack_channel = {
    notifications_enabled = false
  }`),
				ExpectError: regexMust(`Slack channel configuration is not available on this Flightdeck`),
			},
		},
	})
}

// A channel that exists but that Flightdeck cannot post to is refused before
// anything is saved. The apply fails with the API's message, the next plan
// still shows the change, and once the bot is invited the same apply works.
func TestProjectSlackChannel_unusableChannelIsRefusedUntilFixed(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t) // enabling asks Slack for a real channel
	env.fake.SetSlackChannelUnusable("example-channel", true)
	identifier := randIdentifier()
	enabled := projectConfig(env, identifier, `
  name = "Slack unusable"
  slack_channel = {
    enabled = true
    name    = "example-channel"
  }`)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// Naming the channel without enabling it checks nothing.
				Config: projectConfig(env, identifier, `
  name = "Slack unusable"
  slack_channel = {
    name = "example-channel"
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(projectRes, "id", &id),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.enabled", "false"),
					resource.TestCheckResourceAttr(projectRes, "lock_version", "1"),
				),
			},
			{
				// Enabling it runs the check, which refuses the channel. The API's
				// message names the channel and the bot to invite.
				Config: enabled,
				ExpectError: regexMust(`(?s)Slack channel cannot be used.*Invite\s+` + flightdecktest.SlackBotHandle +
					`\s+to\s+#example-channel.*saved\s+none`),
			},
			{
				// Nothing was saved, so the change is still pending. The project
				// PATCH that runs before the block's write bumped lock_version to 2;
				// the refused write left it there.
				PreConfig: func() {
					p := env.fake.Project(mustInt(id))
					if p.SlackChannelEnabled || p.SlackProvisionStatus != "" || p.LockVersion != 2 {
						t.Errorf("a refused write must save nothing: enabled=%t status=%q lock_version=%d",
							p.SlackChannelEnabled, p.SlackProvisionStatus, p.LockVersion)
					}
				},
				Config:             enabled,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// The bot is invited. The same configuration now applies in place.
				PreConfig: func() { env.fake.SetSlackChannelUnusable("example-channel", false) },
				Config:    enabled,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(projectRes,
							tfjsonpath.New("slack_channel").AtMapKey("enabled"), knownvalue.Bool(true)),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "slack_channel.enabled", "true"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.basename", "example-channel"),
					resource.TestCheckResourceAttr(projectRes, "slack_channel.provision_status", "queued"),
					// Project PATCH (3), then the Slack channel PATCH (4).
					resource.TestCheckResourceAttr(projectRes, "lock_version", "4"),
				),
			},
			{
				Config:   enabled,
				PlanOnly: true,
			},
		},
	})
}

// slackChannelBlock builds a slack_channel object with the given attributes
// set and everything else null, the way a configuration that sets only those
// attributes decodes.
func slackChannelBlock(t *testing.T, set map[string]attr.Value) types.Object {
	t.Helper()
	ctx := t.Context()
	attrs := map[string]attr.Value{}
	for name, typ := range slackChannelAttrTypes {
		null, err := typ.ValueFromTerraform(ctx, tftypes.NewValue(typ.TerraformType(ctx), nil))
		if err != nil {
			t.Fatalf("null %s: %v", name, err)
		}
		attrs[name] = null
	}
	for name, value := range set {
		attrs[name] = value
	}
	obj, diags := types.ObjectValue(slackChannelAttrTypes, attrs)
	if diags.HasError() {
		t.Fatalf("building a slack_channel block: %v", diags.Errors())
	}
	return obj
}

func slackChannelTestProject(t *testing.T, env *testEnv, name string) (*client.Client, int64) {
	t.Helper()
	c, err := client.New(env.fake.URL, env.fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	fields := client.Fields{"name": name, "identifier": randIdentifier()}
	p, err := c.CreateProject(t.Context(), fields, client.PayloadKey("project", "", fields))
	if err != nil {
		t.Fatal(err)
	}
	return c, p.ID
}

// The refusal as writeSlackChannel reports it: an error on the attribute that
// picked the channel, the API's message as the detail, and nothing recorded
// for the block, since nothing was saved.
func TestWriteSlackChannel_unusableChannelRecordsNothing(t *testing.T) {
	cases := []struct {
		name    string
		config  map[string]attr.Value
		channel string
		at      path.Path
	}{
		{
			name: "named channel",
			config: map[string]attr.Value{
				"enabled": types.BoolValue(true),
				"name":    slackChannelNameValue{StringValue: types.StringValue("example-channel")},
			},
			channel: "example-channel",
			at:      path.Root("slack_channel").AtName("name"),
		},
		{
			// No name: the channel comes from the project name, so the error
			// points at the block rather than at an attribute nobody set.
			name:    "project-name default",
			config:  map[string]attr.Value{"enabled": types.BoolValue(true)},
			channel: "fd-slack-default",
			at:      path.Root("slack_channel"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, "slack_channel")
			env.requireFake(t)
			env.fake.SetSlackChannelUnusable(tc.channel, true)
			c, id := slackChannelTestProject(t, env, "Slack default")
			ctx := t.Context()

			var diags diag.Diagnostics
			prior := readSlackChannel(ctx, c, id, types.MapNull(types.BoolType), slackEventsManaged, &diags)
			if diags.HasError() {
				t.Fatalf("reading the block: %v", diags.Errors())
			}
			block, lockVersion := writeSlackChannel(ctx, c, id, slackChannelBlock(t, tc.config),
				types.ObjectUnknown(slackChannelAttrTypes), prior, 0, &diags)

			if !block.IsNull() {
				t.Errorf("a refused write must not record the block, got %s", block)
			}
			if lockVersion != 0 {
				t.Errorf("a refused write saves nothing, so lock_version must stay 0, got %d", lockVersion)
			}
			if len(diags.Warnings()) != 0 {
				t.Errorf("expected no warnings, got %v", diags.Warnings())
			}
			errs := diags.Errors()
			if len(errs) != 1 {
				t.Fatalf("expected exactly one error, got %v", errs)
			}
			withPath, ok := errs[0].(diag.DiagnosticWithPath)
			if !ok || !withPath.Path().Equal(tc.at) {
				t.Errorf("error should point at %s, got %#v", tc.at, errs[0])
			}
			if got := errs[0].Summary(); got != "Slack channel cannot be used" {
				t.Errorf("summary = %q", got)
			}
			detail := errs[0].Detail()
			for _, want := range []string{"Invite " + flightdecktest.SlackBotHandle + " to #" + tc.channel, "saved none"} {
				if !strings.Contains(detail, want) {
					t.Errorf("detail does not carry %q:\n%s", want, detail)
				}
			}
			if p := env.fake.Project(id); p.SlackChannelEnabled || p.LockVersion != 0 {
				t.Errorf("the fake saved a refused write: enabled=%t lock_version=%d", p.SlackChannelEnabled, p.LockVersion)
			}
		})
	}
}

// A write that comes back with provision_status `failed` is saved, so it is
// recorded and warned about, never turned into an error.
func TestWriteSlackChannel_failedProvisionWarns(t *testing.T) {
	env := newTestEnv(t, "slack_channel")
	env.requireFake(t)
	c, id := slackChannelTestProject(t, env, "Slack failed")
	ctx := t.Context()
	enabled, err := c.UpdateSlackChannel(ctx, id, client.Fields{"slack_channel_enabled": true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	const note = "Could not post to #fd-slack-failed. Invite " + flightdecktest.SlackBotHandle + " to the channel."
	env.fake.FailSlackProvision(id, note)

	// The block was never read (state null), so it is written even though the
	// project already has it enabled. The API saves nothing new and answers
	// with the job's failure.
	var diags diag.Diagnostics
	block, lockVersion := writeSlackChannel(ctx, c, id,
		slackChannelBlock(t, map[string]attr.Value{"enabled": types.BoolValue(true)}),
		types.ObjectUnknown(slackChannelAttrTypes), types.ObjectNull(slackChannelAttrTypes), enabled.LockVersion, &diags)

	if diags.HasError() {
		t.Fatalf("a saved configuration must not be an error: %v", diags.Errors())
	}
	warnings := diags.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("expected exactly one warning, got %v", warnings)
	}
	if !strings.Contains(warnings[0].Summary(), "provisioning failed") {
		t.Errorf("summary = %q", warnings[0].Summary())
	}
	if !strings.Contains(warnings[0].Detail(), note) {
		t.Errorf("detail does not carry the provision note:\n%s", warnings[0].Detail())
	}
	if lockVersion != enabled.LockVersion {
		t.Errorf("lock_version = %d, want %d", lockVersion, enabled.LockVersion)
	}
	if block.IsNull() {
		t.Fatal("the saved block must be recorded")
	}
	if got := block.Attributes()["provision_status"]; !got.Equal(types.StringValue("failed")) {
		t.Errorf("provision_status = %s, want failed", got)
	}
}

func TestSlackChannelNameDerivesToNothing(t *testing.T) {
	cases := map[string]bool{
		"Release Eng":          false,
		"team-web":             false,
		"7":                    false,
		"  My Team Channel!  ": false,
		"!!!":                  true,
		"---":                  true,
		"":                     true,
		"   ":                  true,
		// Not ASCII alphanumeric once lower-cased, so the API derives nothing
		// from it either.
		"ÉÉÉ": true,
	}
	for input, want := range cases {
		if got := slackChannelNameDerivesToNothing(input); got != want {
			t.Errorf("slackChannelNameDerivesToNothing(%q) = %t, want %t", input, got, want)
		}
	}
}

func TestSlackChannelNameSemanticEquality(t *testing.T) {
	name := func(s string) slackChannelNameValue {
		return slackChannelNameValue{StringValue: types.StringValue(s)}
	}
	cases := []struct {
		a, b slackChannelNameValue
		want bool
	}{
		// The trim is the only liberty the server takes with a name…
		{name("  Release Eng  "), name("Release Eng"), true},
		{slackChannelNameValue{StringValue: types.StringNull()}, name("  "), true},
		// …so two spellings that derive to one channel are still two names.
		{name("Release Eng"), name("release-eng"), false},
		{name("STAY"), name("stay"), false},
		{name("team-web"), name("team-ops"), false},
		{slackChannelNameValue{StringValue: types.StringNull()}, name("team-web"), false},
	}
	for _, tc := range cases {
		got, diags := tc.a.StringSemanticEquals(context.Background(), tc.b)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags.Errors())
		}
		if got != tc.want {
			t.Errorf("%s == %s: got %t, want %t", tc.a, tc.b, got, tc.want)
		}
	}
}
