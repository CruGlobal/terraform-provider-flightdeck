package client

import (
	"context"
	"strconv"
)

// SlackChannel is the per-project Slack channel configuration at
// GET/PATCH /api/v1/projects/:project_id/slack-channel. Like self-healing it
// lives on the project row, so LockVersion is the PROJECT's lock_version: the
// same token PATCH /api/v1/projects/:id uses, and a write here bumps it.
//
// Provisioning is asynchronous. A write saves the configuration and enqueues
// the job; ChannelID, ProvisionStatus and ProvisionNote are filled in
// afterwards, so the write's response reports the enqueue, never a finished
// channel.
type SlackChannel struct {
	ProjectID int64 `json:"project_id"`
	// ChannelAvailable is false when the workspace has no connected Slack
	// integration, and ScopesSufficient is false when the connection predates
	// the channel scopes. Either one means nothing will provision, however
	// valid the configuration is.
	ChannelAvailable     bool            `json:"slack_channel_available"`
	ScopesSufficient     bool            `json:"slack_scopes_sufficient"`
	ChannelEnabled       bool            `json:"slack_channel_enabled"`
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
}

// SlackChannelWritableKeys are the settable keys, spelled as the read emits
// them so a read applies straight back. Everything else on the shape is
// server-owned and refused by name.
var SlackChannelWritableKeys = []string{
	"slack_channel_enabled", "slack_notifications_enabled",
	"slack_channel_name", "slack_event_filter",
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
// PROJECT's lock_version. Only the submitted keys change.
func (c *Client) UpdateSlackChannel(ctx context.Context, projectID int64, settings Fields, projectLockVersion int64) (*SlackChannel, error) {
	return PatchResource[*SlackChannel](ctx, c, slackChannelPath(projectID), slackChannelRoot, settings, &projectLockVersion)
}
