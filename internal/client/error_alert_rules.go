package client

import (
	"context"
	"encoding/json"
	"strconv"
)

// Enumerations from ErrorAlertRule and ErrorGroup.
var (
	ErrorAlertTriggers      = []string{"new_group", "regression", "occurrence_threshold"}
	ErrorAlertConditionKeys = []string{"min_level", "environment", "count", "window_minutes"}
	ErrorAlertActionKeys    = []string{"notify_slack", "notify_email", "create_work_item", "file_intake", "notify_webhook", "open_incident"}
	ErrorLevels             = []string{"debug", "info", "warning", "error", "critical"}
)

// ErrorAlertRule is a trigger → conditions → action alert rule. Condition and
// Action are the raw JSON objects the API stores (whitelisted keys only).
type ErrorAlertRule struct {
	ID          int64                      `json:"id"`
	ProjectID   int64                      `json:"project_id"`
	Name        string                     `json:"name"`
	Enabled     bool                       `json:"enabled"`
	Trigger     string                     `json:"trigger"`
	Condition   map[string]json.RawMessage `json:"condition"`
	Action      map[string]json.RawMessage `json:"action"`
	LockVersion int64                      `json:"lock_version"`
}

func errorAlertRulesPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/error_alert_rules"
}

// ListErrorAlertRules returns a project's rules.
func (c *Client) ListErrorAlertRules(ctx context.Context, projectID int64) ([]ErrorAlertRule, error) {
	return List[ErrorAlertRule](ctx, c, errorAlertRulesPath(projectID))
}

// GetErrorAlertRule fetches one rule.
func (c *Client) GetErrorAlertRule(ctx context.Context, id int64) (*ErrorAlertRule, error) {
	var r ErrorAlertRule
	if err := c.Get(ctx, "/error_alert_rules/"+strconv.FormatInt(id, 10), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// CreateErrorAlertRule creates a rule, guarded like CreateProject.
func (c *Client) CreateErrorAlertRule(ctx context.Context, projectID int64, fields Fields, idempotencyKey string) (*ErrorAlertRule, error) {
	return CreateWithReplayGuard(ctx, idempotencyKey,
		func(ctx context.Context, key string) (*ErrorAlertRule, error) {
			var r ErrorAlertRule
			if err := c.Post(ctx, errorAlertRulesPath(projectID), map[string]any{"error_alert_rule": fields}, &r, WithIdempotencyKey(key)); err != nil {
				return nil, err
			}
			return &r, nil
		},
		func(ctx context.Context, created *ErrorAlertRule) error {
			_, err := c.GetErrorAlertRule(ctx, created.ID)
			return err
		})
}

// UpdateErrorAlertRule PATCHes a rule with an If-Match precondition.
func (c *Client) UpdateErrorAlertRule(ctx context.Context, id int64, fields Fields, lockVersion int64) (*ErrorAlertRule, error) {
	var r ErrorAlertRule
	err := c.Patch(ctx, "/error_alert_rules/"+strconv.FormatInt(id, 10), map[string]any{"error_alert_rule": fields}, &r, WithIfMatch(lockVersion))
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// DeleteErrorAlertRule deletes a rule; 404 is success.
func (c *Client) DeleteErrorAlertRule(ctx context.Context, id int64) error {
	err := c.Delete(ctx, "/error_alert_rules/"+strconv.FormatInt(id, 10), nil)
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
