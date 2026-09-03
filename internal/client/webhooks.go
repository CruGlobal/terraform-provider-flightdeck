package client

import (
	"context"
	"strconv"
)

// WebhookEvents mirrors Webhook::EVENTS — everything a webhook may subscribe to.
var WebhookEvents = []string{
	"work_item.created", "work_item.updated", "work_item.deleted",
	"work_item.state_changed", "work_item.assigned", "work_item.unassigned",
	"comment.created", "comment.updated", "comment.deleted",
	"cycle.created", "cycle.updated", "cycle.deleted",
	"module.created", "module.updated", "module.deleted",
	"intake.created", "intake.accepted", "intake.declined",
	"project.created", "project.updated",
}

// Webhook is an outbound webhook. Secret is present only in the response to
// the ORIGINAL create; a replayed create returns it empty with SecretAvailable
// false. ProjectID is nil for a workspace-wide webhook.
type Webhook struct {
	ID              int64    `json:"id"`
	WorkspaceID     int64    `json:"workspace_id"`
	ProjectID       *int64   `json:"project_id"`
	URL             string   `json:"url"`
	Events          []string `json:"events"`
	Active          bool     `json:"active"`
	LockVersion     int64    `json:"lock_version"`
	Secret          string   `json:"secret"`
	SecretAvailable *bool    `json:"secret_available"`
}

// ResourceID implements Identified.
func (w *Webhook) ResourceID() int64 { return w.ID }

// secret implements secretBearing.
func (w *Webhook) secret() string { return w.Secret }

const webhookRoot = "webhook"

func webhookPath(id int64) string { return "/webhooks/" + strconv.FormatInt(id, 10) }

// ListWebhooks returns the workspace's webhooks.
func (c *Client) ListWebhooks(ctx context.Context) ([]Webhook, error) {
	return ListResources[Webhook](ctx, c, "/webhooks", webhookRoot)
}

// GetWebhook fetches one webhook.
func (c *Client) GetWebhook(ctx context.Context, id int64) (*Webhook, error) {
	return GetResource[*Webhook](ctx, c, webhookPath(id), webhookRoot)
}

// CreateWebhook creates a webhook and guarantees the returned value carries the
// signing secret. A replayed create (secret redacted) is deleted and the
// webhook is created again under a fresh key. See CreateSecretResource.
func (c *Client) CreateWebhook(ctx context.Context, fields Fields, idempotencyKey string) (*Webhook, error) {
	return CreateSecretResource(ctx, c, "/webhooks", webhookRoot, fields, idempotencyKey,
		VerifyByGet(c.GetWebhook),
		func(ctx context.Context, replayed *Webhook) error {
			// Delete against the row's CURRENT version; a 404 (the predecessor
			// was already destroyed) is fine.
			current, err := c.GetWebhook(ctx, replayed.ID)
			if err != nil {
				if IsNotFound(err) {
					return nil
				}
				return err
			}
			return c.DeleteWebhook(ctx, current.ID, current.LockVersion)
		})
}

// UpdateWebhook PATCHes a webhook with an If-Match precondition.
func (c *Client) UpdateWebhook(ctx context.Context, id int64, fields Fields, lockVersion int64) (*Webhook, error) {
	return PatchResource[*Webhook](ctx, c, webhookPath(id), webhookRoot, fields, &lockVersion)
}

// DeleteWebhook deletes a webhook under an If-Match precondition; 404 is success.
func (c *Client) DeleteWebhook(ctx context.Context, id, lockVersion int64) error {
	err := c.Delete(ctx, webhookPath(id), nil, WithIfMatch(lockVersion))
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
