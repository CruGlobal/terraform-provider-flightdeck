package client

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"
)

// A read says three different things about the mode, and the provider needs
// all three: a mode was chosen, nobody chose (null), or the Flightdeck is
// older than the mode (no key at all). Mode alone cannot tell the last two
// apart; ReportsMode can.
func TestSlackChannel_decodesTheModeAndWhetherItWasReported(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		mode     string // "" for nil
		reported bool
	}{
		{
			name:     "chosen, wrapped in the root key",
			body:     `{"slack_channel": {"project_id": 7, "slack_channel_mode": "link", "slack_channel_basename": "fd-x", "lock_version": 3}}`,
			mode:     SlackChannelModeLink,
			reported: true,
		},
		{
			name:     "chosen, flat",
			body:     `{"project_id": 7, "slack_channel_mode": "create", "slack_channel_basename": "fd-x", "lock_version": 3}`,
			mode:     SlackChannelModeCreate,
			reported: true,
		},
		{
			name:     "nobody chose",
			body:     `{"project_id": 7, "slack_channel_mode": null, "slack_channel_basename": "fd-x", "lock_version": 3}`,
			reported: true,
		},
		{
			name: "older Flightdeck",
			body: `{"project_id": 7, "slack_channel_basename": "fd-x", "lock_version": 3}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, err := DecodeResource[*SlackChannel](json.RawMessage(tc.body), slackChannelRoot)
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if sc.Mode != nil {
				got = *sc.Mode
			}
			if got != tc.mode {
				t.Errorf("Mode = %q, want %q", got, tc.mode)
			}
			if sc.ReportsMode() != tc.reported {
				t.Errorf("ReportsMode() = %t, want %t", sc.ReportsMode(), tc.reported)
			}
			// The rest of the shape still decodes around the custom step.
			if sc.ProjectID != 7 || sc.ChannelBasename != "fd-x" || sc.LockVersion != 3 {
				t.Errorf("other fields lost: %+v", sc)
			}
		})
	}
}

// The fake's mode follows the API's contract: null and blank leave it alone,
// only the two exact spellings are accepted, create refuses a taken name and
// link a missing one with slack_channel_unusable (saving nothing), and an
// older Flightdeck neither reports the key nor accepts it. The provider's
// tests lean on all of this, so it is checked here directly.
func TestSlackChannel_modeContract(t *testing.T) {
	ctx := context.Background()
	fake := flightdecktest.New(t)
	c, err := New(fake.URL, fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	fields := Fields{"name": "Mode contract", "identifier": "MODE"}
	p, err := c.CreateProject(ctx, fields, PayloadKey("project", "", fields))
	if err != nil {
		t.Fatal(err)
	}
	patch := func(settings Fields) (*SlackChannel, error) {
		t.Helper()
		current, err := c.GetSlackChannel(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		return c.UpdateSlackChannel(ctx, p.ID, settings, current.LockVersion)
	}
	modeOf := func(sc *SlackChannel) string {
		if sc == nil {
			return "<no response>"
		}
		if sc.Mode == nil {
			return "<null>"
		}
		return *sc.Mode
	}

	sc, err := c.GetSlackChannel(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sc.ReportsMode() || sc.Mode != nil {
		t.Fatalf("a new project must report the key with nobody's choice (null), got reported=%t mode=%s", sc.ReportsMode(), modeOf(sc))
	}

	if sc, err = patch(Fields{"slack_channel_mode": "link"}); err != nil || modeOf(sc) != "link" {
		t.Fatalf("setting link: mode=%v err=%v", sc, err)
	}
	before := fake.Project(p.ID).LockVersion
	for _, blank := range []any{nil, "", "   "} {
		if sc, err = patch(Fields{"slack_channel_mode": blank}); err != nil || modeOf(sc) != "link" {
			t.Fatalf("a blank (%#v) must leave the mode alone: mode=%s err=%v", blank, modeOf(sc), err)
		}
	}
	if after := fake.Project(p.ID).LockVersion; after != before {
		t.Errorf("a blank mode is no change, but lock_version moved from %d to %d", before, after)
	}
	for _, bad := range []any{"Create", " link", "both", true} {
		_, err := patch(Fields{"slack_channel_mode": bad})
		if !HasCode(err, CodeInvalidAttribute) || !strings.Contains(err.Error(), `slack_channel_mode must be "create" or "link"`) {
			t.Errorf("mode %#v: want 422 invalid_attribute naming the two values, got %v", bad, err)
		}
	}

	// Link needs a channel that exists. Nothing is checked while the channel
	// is off, so the refusal comes when it is turned on.
	if _, err := patch(Fields{"slack_channel_name": "team-web"}); err != nil {
		t.Fatal(err)
	}
	before = fake.Project(p.ID).LockVersion
	_, err = patch(Fields{"slack_channel_enabled": true})
	if !HasCode(err, CodeSlackChannelUnusable) || !strings.Contains(err.Error(), "No Slack channel named #team-web.") {
		t.Fatalf("link with no such channel: want 422 slack_channel_unusable, got %v", err)
	}
	if row := fake.Project(p.ID); row.SlackChannelEnabled || row.LockVersion != before {
		t.Errorf("a refused write must save nothing: enabled=%t lock_version %d -> %d", row.SlackChannelEnabled, before, row.LockVersion)
	}

	// Create refuses a name another channel already has.
	fake.SetSlackChannelExists("team-web", true)
	_, err = patch(Fields{"slack_channel_enabled": true, "slack_channel_mode": "create"})
	if !HasCode(err, CodeSlackChannelUnusable) || !strings.Contains(err.Error(), "A Slack channel named #team-web already exists.") {
		t.Fatalf("create with a taken name: want 422 slack_channel_unusable, got %v", err)
	}
	if row := fake.Project(p.ID); row.SlackChannelEnabled || row.SlackChannelMode != "link" {
		t.Errorf("a refused write must save nothing: enabled=%t mode=%q", row.SlackChannelEnabled, row.SlackChannelMode)
	}

	// Link takes the channel that exists.
	if sc, err = patch(Fields{"slack_channel_enabled": true}); err != nil || !sc.ChannelEnabled || modeOf(sc) != "link" {
		t.Fatalf("link with the channel there: sc=%+v err=%v", sc, err)
	}

	// Only a write aimed at the channel is checked. The channel goes away
	// before the job links it: a mute is still saved, and a rename is checked.
	fake.SetSlackChannelExists("team-web", false)
	if _, err := patch(Fields{"slack_notifications_enabled": false}); err != nil {
		t.Errorf("a mute is not aimed at the channel and must not be checked: %v", err)
	}
	_, err = patch(Fields{"slack_channel_name": "team-ops"})
	if !HasCode(err, CodeSlackChannelUnusable) || !strings.Contains(err.Error(), "No Slack channel named #team-ops.") {
		t.Errorf("a rename is aimed at the channel and must be checked: got %v", err)
	}

	// An older Flightdeck: no key on a read, and the key refused as unknown
	// whatever its value, null included.
	fake.SetSlackChannelModeSupported(false)
	if sc, err = c.GetSlackChannel(ctx, p.ID); err != nil || sc.ReportsMode() || sc.Mode != nil {
		t.Fatalf("an older Flightdeck must not report the key: sc=%+v err=%v", sc, err)
	}
	for _, value := range []any{"link", nil} {
		_, err := patch(Fields{"slack_channel_mode": value})
		if !HasCode(err, CodeInvalidAttribute) || !strings.Contains(err.Error(), "unknown key: slack_channel_mode") {
			t.Errorf("older Flightdeck, mode %#v: want 422 unknown key, got %v", value, err)
		}
	}
	if _, err := patch(Fields{"slack_notifications_enabled": false}); err != nil {
		t.Errorf("an older Flightdeck must still take a write without the mode: %v", err)
	}
}

// The fake refuses a channel the way Flightdeck does, in each mode, with
// Flightdeck's words: an archived channel is "archived" whatever the mode, a
// private one Flightdeck cannot see is the bot message with no mode, taken in
// create mode and missing in link mode, and a live public one is linked
// except in create mode. The provider decides where a refusal points from
// these words, so they are checked here as Flightdeck writes them.
func TestSlackChannel_refusalsReadAsFlightdeckWritesThem(t *testing.T) {
	const (
		archived = "#team-web is archived. Unarchive it in Slack or choose a different channel name, then try again."
		bot      = "#team-web exists, but Flightdeck can't post to it: it's private and " + flightdecktest.SlackBotHandle +
			" isn't a member, or it's archived. Invite " + flightdecktest.SlackBotHandle +
			" to #team-web (or unarchive it), then try again."
		exists  = "A Slack channel named #team-web already exists. To use it, link it instead of creating a new one."
		missing = "No Slack channel named #team-web. Check the name, or create a new channel instead. " +
			"If it's private, invite the Flightdeck app to it first."
	)
	channels := map[string]func(*flightdecktest.Server){
		"archived":     func(f *flightdecktest.Server) { f.SetSlackChannelArchived("team-web", true) },
		"private":      func(f *flightdecktest.Server) { f.SetSlackChannelUnusable("team-web", true) },
		"public":       func(f *flightdecktest.Server) { f.SetSlackChannelExists("team-web", true) },
		"no such name": func(*flightdecktest.Server) {},
	}
	want := map[string]map[string]string{ // channel -> mode -> message, "" when it passes
		"archived":     {"": archived, "create": archived, "link": archived},
		"private":      {"": bot, "create": exists, "link": missing},
		"public":       {"": "", "create": exists, "link": ""},
		"no such name": {"": "", "create": "", "link": missing},
	}
	ctx := context.Background()
	for channel, setup := range channels {
		for _, mode := range []string{"", "create", "link"} {
			t.Run(channel+", mode "+mode, func(t *testing.T) {
				fake := flightdecktest.New(t)
				setup(fake)
				c, err := New(fake.URL, fake.Token())
				if err != nil {
					t.Fatal(err)
				}
				fields := Fields{"name": "Refusals", "identifier": "REF"}
				p, err := c.CreateProject(ctx, fields, PayloadKey("project", "", fields))
				if err != nil {
					t.Fatal(err)
				}
				settings := Fields{"slack_channel_name": "team-web"}
				if mode != "" {
					settings["slack_channel_mode"] = mode
				}
				sc, err := c.UpdateSlackChannel(ctx, p.ID, settings, p.LockVersion)
				if err != nil {
					t.Fatalf("choosing the channel with it off checks nothing, got %v", err)
				}
				_, err = c.UpdateSlackChannel(ctx, p.ID, Fields{"slack_channel_enabled": true}, sc.LockVersion)
				expected := want[channel][mode]
				switch {
				case expected == "" && err != nil:
					t.Errorf("want the channel accepted, got %v", err)
				case expected != "" && !HasCode(err, CodeSlackChannelUnusable):
					t.Errorf("want 422 slack_channel_unusable %q, got %v", expected, err)
				case expected != "":
					if apiErr, _ := AsError(err); apiErr == nil || apiErr.Message != expected {
						t.Errorf("message = %q\nwant      %q", apiErr.Message, expected)
					}
				}
			})
		}
	}
}

// Changing the mode on a channel that is on but not linked yet is checked
// again under the new mode, as Flightdeck's classify_write does for a change
// of mode while the channel is enabled.
func TestSlackChannel_modeChangeOnAnUnlinkedChannelIsChecked(t *testing.T) {
	ctx := context.Background()
	fake := flightdecktest.New(t)
	c, err := New(fake.URL, fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	fields := Fields{"name": "Recheck", "identifier": "RECHECK"}
	p, err := c.CreateProject(ctx, fields, PayloadKey("project", "", fields))
	if err != nil {
		t.Fatal(err)
	}
	sc, err := c.UpdateSlackChannel(ctx, p.ID, Fields{"slack_channel_enabled": true, "slack_channel_name": "team-web"}, p.LockVersion)
	if err != nil || sc.ChannelLinked {
		t.Fatalf("turning on a channel with a free name: linked=%v err=%v", sc != nil && sc.ChannelLinked, err)
	}
	_, err = c.UpdateSlackChannel(ctx, p.ID, Fields{"slack_channel_mode": "link"}, sc.LockVersion)
	if !HasCode(err, CodeSlackChannelUnusable) || !strings.Contains(err.Error(), "No Slack channel named #team-web.") {
		t.Fatalf("a mode change on an enabled, unlinked channel must be checked: got %v", err)
	}
	if row := fake.Project(p.ID); row.SlackChannelMode != "" {
		t.Errorf("the refused mode change saved mode %q", row.SlackChannelMode)
	}
}
