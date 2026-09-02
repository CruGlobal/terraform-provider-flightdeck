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

// Webhook is an outbound webhook. Secret is present only in the create
// response. ProjectID is nil for a workspace-wide webhook.
type Webhook struct {
	ID          int64    `json:"id"`
	ProjectID   *int64   `json:"project_id"`
	URL         string   `json:"url"`
	Events      []string `json:"events"`
	Secret      string   `json:"secret"`
	Active      bool     `json:"active"`
	LockVersion int64    `json:"lock_version"`
}

// ListWebhooks returns the workspace's webhooks.
func (c *Client) ListWebhooks(ctx context.Context) ([]Webhook, error) {
	return List[Webhook](ctx, c, "/webhooks")
}

// GetWebhook fetches one webhook.
func (c *Client) GetWebhook(ctx context.Context, id int64) (*Webhook, error) {
	var w Webhook
	if err := c.Get(ctx, "/webhooks/"+strconv.FormatInt(id, 10), &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// CreateWebhook creates a webhook, guarded like CreateProject.
func (c *Client) CreateWebhook(ctx context.Context, fields Fields, idempotencyKey string) (*Webhook, error) {
	return CreateWithReplayGuard(ctx, idempotencyKey,
		func(ctx context.Context, key string) (*Webhook, error) {
			var w Webhook
			if err := c.Post(ctx, "/webhooks", map[string]any{"webhook": fields}, &w, WithIdempotencyKey(key)); err != nil {
				return nil, err
			}
			return &w, nil
		},
		func(ctx context.Context, created *Webhook) error {
			_, err := c.GetWebhook(ctx, created.ID)
			return err
		})
}

// UpdateWebhook PATCHes a webhook with an If-Match precondition.
func (c *Client) UpdateWebhook(ctx context.Context, id int64, fields Fields, lockVersion int64) (*Webhook, error) {
	var w Webhook
	if err := c.Patch(ctx, "/webhooks/"+strconv.FormatInt(id, 10), map[string]any{"webhook": fields}, &w, WithIfMatch(lockVersion)); err != nil {
		return nil, err
	}
	return &w, nil
}

// DeleteWebhook deletes a webhook; 404 is success.
func (c *Client) DeleteWebhook(ctx context.Context, id int64) error {
	err := c.Delete(ctx, "/webhooks/"+strconv.FormatInt(id, 10), nil)
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
