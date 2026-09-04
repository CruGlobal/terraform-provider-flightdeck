package client

import (
	"context"
	"encoding/json"
	"strconv"
)

// Enumerations the API accepts for rules and error levels.
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

// ResourceID implements Identified.
func (r *ErrorAlertRule) ResourceID() int64 { return r.ID }

const errorAlertRuleRoot = "error_alert_rule"

// Rules are nested under the project all the way: …/projects/:project_id/error-rules/:id.
func errorAlertRulesPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/error-rules"
}

func errorAlertRulePath(projectID, id int64) string {
	return errorAlertRulesPath(projectID) + "/" + strconv.FormatInt(id, 10)
}

// ListErrorAlertRules returns a project's rules.
func (c *Client) ListErrorAlertRules(ctx context.Context, projectID int64) ([]ErrorAlertRule, error) {
	return ListResources[ErrorAlertRule](ctx, c, errorAlertRulesPath(projectID), errorAlertRuleRoot)
}

// GetErrorAlertRule fetches one rule.
func (c *Client) GetErrorAlertRule(ctx context.Context, projectID, id int64) (*ErrorAlertRule, error) {
	return GetResource[*ErrorAlertRule](ctx, c, errorAlertRulePath(projectID, id), errorAlertRuleRoot)
}

// CreateErrorAlertRule creates a rule through the verified create path.
func (c *Client) CreateErrorAlertRule(ctx context.Context, projectID int64, fields Fields, idempotencyKey string) (*ErrorAlertRule, error) {
	return CreateResource(ctx, c, errorAlertRulesPath(projectID), errorAlertRuleRoot, fields, idempotencyKey,
		VerifyByGet(func(ctx context.Context, id int64) (*ErrorAlertRule, error) {
			return c.GetErrorAlertRule(ctx, projectID, id)
		}))
}

// UpdateErrorAlertRule PATCHes a rule with an If-Match precondition.
func (c *Client) UpdateErrorAlertRule(ctx context.Context, projectID, id int64, fields Fields, lockVersion int64) (*ErrorAlertRule, error) {
	return PatchResource[*ErrorAlertRule](ctx, c, errorAlertRulePath(projectID, id), errorAlertRuleRoot, fields, &lockVersion)
}

// DeleteErrorAlertRule deletes a rule under an If-Match precondition; 404 is success.
func (c *Client) DeleteErrorAlertRule(ctx context.Context, projectID, id, lockVersion int64) error {
	err := c.Delete(ctx, errorAlertRulePath(projectID, id), nil, WithIfMatch(lockVersion))
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
