package client

import (
	"context"
	"encoding/json"
	"strconv"
)

// SlackChannel is the per-project Slack channel configuration at
// GET/PATCH /api/v1/projects/:project_id/slack-channel. Like self-healing it
// lives on the project row, so LockVersion is the PROJECT's lock_version: the
// same token PATCH /api/v1/projects/:id uses, and a write here bumps it.
//
// Provisioning is mostly asynchronous. A write that leaves the channel enabled
// and not yet linked is first checked against Slack: a channel that exists but
// cannot be posted to is refused with a 422 (CodeSlackChannelUnusable) and
// nothing is saved. The same 422 refuses a channel the Mode rules out: in
// create mode a name another channel already has, in link mode a name no
// channel has. Otherwise the write saves the configuration and enqueues
// the job, so its response may already be linked but is usually queued, with
// ChannelID, ProvisionStatus and ProvisionNote filled in afterwards. When
// Slack cannot be reached for the check, the write is saved and queued as if
// it had passed.
type SlackChannel struct {
	ProjectID int64 `json:"project_id"`
	// ChannelAvailable is false when the workspace has no connected Slack
	// integration, and ScopesSufficient is false when the connection predates
	// the channel scopes. Either one means nothing will provision, however
	// valid the configuration is.
	ChannelAvailable bool `json:"slack_channel_available"`
	ScopesSufficient bool `json:"slack_scopes_sufficient"`
	ChannelEnabled   bool `json:"slack_channel_enabled"`
	// Mode is how Flightdeck gets the channel: SlackChannelModeCreate,
	// SlackChannelModeLink, or nil when nobody has chosen (link a channel with
	// the name if there is one, else create it, and invite the members either
	// way). A null or blank on a write leaves the mode alone, and there is no
	// way to go back to nil. A Flightdeck older than the mode has no such key
	// on a read and refuses a write naming it as an unknown key, so nil alone
	// cannot tell the two apart: ReportsMode can.
	Mode                 *string         `json:"slack_channel_mode"`
	NotificationsEnabled bool            `json:"slack_notifications_enabled"`
	ChannelLinked        bool            `json:"slack_channel_linked"`
	ChannelID            *string         `json:"slack_channel_id"`
	ChannelName          *string         `json:"slack_channel_name"`
	ChannelBasename      string          `json:"slack_channel_basename"`
	ProvisionStatus      *string         `json:"slack_provision_status"`
	ProvisionNote        *string         `json:"slack_provision_note"`
	InvitesSkipped       int64           `json:"slack_invites_skipped"`
	EventFilter          map[string]bool `json:"slack_event_filter"`
	LockVersion          int64           `json:"lock_version"`

	// modeReported is whether the read carried slack_channel_mode at all,
	// null included.
	modeReported bool
}

// UnmarshalJSON decodes the shape as usual and also notes whether the
// slack_channel_mode key was there, which a null value alone cannot say.
func (s *SlackChannel) UnmarshalJSON(data []byte) error {
	type plain SlackChannel
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return err
	}
	*s = SlackChannel(decoded)
	_, s.modeReported = keys["slack_channel_mode"]
	return nil
}

// ReportsMode reports whether the Flightdeck that answered knows the Slack
// channel mode: a read from one that does always has the key, even when its
// value is null because nobody has chosen.
func (s *SlackChannel) ReportsMode() bool { return s.modeReported }

// The values slack_channel_mode takes, spelled exactly; the API neither trims
// nor folds case. Create: Flightdeck makes the channel and invites the
// project's members, and refuses a name another channel already has. Link:
// Flightdeck uses a channel that already exists and never invites anyone, and
// refuses a name no channel has (a private channel it is not in counts).
const (
	SlackChannelModeCreate = "create"
	SlackChannelModeLink   = "link"
)

// SlackChannelModes are the values slack_channel_mode accepts.
var SlackChannelModes = []string{SlackChannelModeCreate, SlackChannelModeLink}

// The provision outcomes slack_provision_status reports that need someone to
// act. Failed: the job, or a check of a linked channel, could not finish (the
// note says why). NeedsInvite: the channel is linked, but Flightdeck is not in
// it and cannot join it by itself, so no member can be invited until someone
// adds the Flightdeck app to the channel.
const (
	SlackProvisionFailed      = "failed"
	SlackProvisionNeedsInvite = "needs_invite"
)

// SlackChannelWritableKeys are the settable keys, spelled as the read emits
// them so a read applies straight back. Everything else on the shape is
// server-owned and refused by name.
var SlackChannelWritableKeys = []string{
	"slack_channel_enabled", "slack_notifications_enabled",
	"slack_channel_name", "slack_event_filter", "slack_channel_mode",
}

// SlackEventCategories are the activity categories slack_event_filter accepts,
// in the order the API documents them. The filter is MERGED server-side: only
// the categories a write names change.
var SlackEventCategories = []string{"created", "state_changed", "field_changed", "logged", "assigned"}

const slackChannelRoot = "slack_channel"

func slackChannelPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/slack-channel"
}

// GetSlackChannel reads a project's Slack channel configuration. Project
// admins only: other tokens get a 403. A deployment without the endpoint 404s.
func (c *Client) GetSlackChannel(ctx context.Context, projectID int64) (*SlackChannel, error) {
	return GetResource[*SlackChannel](ctx, c, slackChannelPath(projectID), slackChannelRoot)
}

// UpdateSlackChannel PATCHes the writable keys under an If-Match carrying the
// PROJECT's lock_version. Only the submitted keys change. A channel Flightdeck
// cannot post to comes back as an *Error with CodeSlackChannelUnusable.
func (c *Client) UpdateSlackChannel(ctx context.Context, projectID int64, settings Fields, projectLockVersion int64) (*SlackChannel, error) {
	return PatchResource[*SlackChannel](ctx, c, slackChannelPath(projectID), slackChannelRoot, settings, &projectLockVersion)
}
