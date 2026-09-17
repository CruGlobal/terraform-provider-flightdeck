package client

import (
	"context"
	"encoding/json"
	"strconv"
)

// Enumerations the API accepts for incident alert rules. They overlap the
// error-rule vocabulary without matching it: the triggers are incident
// lifecycle events, the condition gates on incident severity rather than error
// level, and the action has no `open_incident` (the incident already exists)
// and no `escalation_policy_id`.
var (
	IncidentAlertTriggers      = []string{"incident_opened", "incident_repeated"}
	IncidentAlertConditionKeys = []string{"min_severity", "count", "window_minutes"}
	IncidentAlertActionKeys    = []string{"notify_slack", "notify_email", "create_work_item", "file_intake", "notify_webhook"}
	IncidentSeverities         = []string{"critical", "error", "warning", "info"}
	IncidentPriorities         = []string{"none", "low", "medium", "high", "urgent"}
)

// IncidentAlertRule is a trigger -> conditions -> action rule that fires on an
// incident's lifecycle. Condition and Action are the raw JSON objects the API
// stores (allowlisted keys only); a false action flag is stored by omission,
// so a key absent from Action means false.
//
// PriorityMap is the EFFECTIVE severity-to-priority table, defaults included.
// The overrides actually stored live under Action["priority_map"], and the API
// keeps only the rows that differ from the default — so a row set to its own
// default is dropped there but still reported here.
type IncidentAlertRule struct {
	ID          int64                      `json:"id"`
	ProjectID   int64                      `json:"project_id"`
	Name        string                     `json:"name"`
	Enabled     bool                       `json:"enabled"`
	Trigger     string                     `json:"trigger"`
	Condition   map[string]json.RawMessage `json:"condition"`
	Action      map[string]json.RawMessage `json:"action"`
	PriorityMap map[string]string          `json:"priority_map"`
	LockVersion int64                      `json:"lock_version"`
}

// ResourceID implements Identified.
func (r *IncidentAlertRule) ResourceID() int64 { return r.ID }

const incidentAlertRuleRoot = "incident_alert_rule"

// Rules are nested under the project all the way:
// .../projects/:project_id/incident-rules/:id.
func incidentAlertRulesPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/incident-rules"
}

func incidentAlertRulePath(projectID, id int64) string {
	return incidentAlertRulesPath(projectID) + "/" + strconv.FormatInt(id, 10)
}

// ListIncidentAlertRules returns a project's rules.
func (c *Client) ListIncidentAlertRules(ctx context.Context, projectID int64) ([]IncidentAlertRule, error) {
	return ListResources[IncidentAlertRule](ctx, c, incidentAlertRulesPath(projectID), incidentAlertRuleRoot)
}

// GetIncidentAlertRule fetches one rule.
func (c *Client) GetIncidentAlertRule(ctx context.Context, projectID, id int64) (*IncidentAlertRule, error) {
	return GetResource[*IncidentAlertRule](ctx, c, incidentAlertRulePath(projectID, id), incidentAlertRuleRoot)
}

// CreateIncidentAlertRule creates a rule through the verified create path. The
// project's `incidents` feature must be on, or the create is a 422
// validation_failed; the gate applies to creates only, so an existing rule
// keeps working after the feature is switched off.
func (c *Client) CreateIncidentAlertRule(ctx context.Context, projectID int64, fields Fields, idempotencyKey string) (*IncidentAlertRule, error) {
	return CreateResource(ctx, c, incidentAlertRulesPath(projectID), incidentAlertRuleRoot, fields, idempotencyKey,
		VerifyByGet(func(ctx context.Context, id int64) (*IncidentAlertRule, error) {
			return c.GetIncidentAlertRule(ctx, projectID, id)
		}))
}

// UpdateIncidentAlertRule PATCHes a rule with an If-Match precondition.
//
// The two levels behave differently, and both matter. A top-level key that is
// absent keeps its stored value, so `enabled` has to be re-sent to be managed.
// `condition` and `action`, by contrast, REPLACE what is stored whenever they
// are sent: a partial action object drops every flag it does not name. Send
// both objects whole.
func (c *Client) UpdateIncidentAlertRule(ctx context.Context, projectID, id int64, fields Fields, lockVersion int64) (*IncidentAlertRule, error) {
	return PatchResource[*IncidentAlertRule](ctx, c, incidentAlertRulePath(projectID, id), incidentAlertRuleRoot, fields, &lockVersion)
}

// DeleteIncidentAlertRule deletes a rule under an If-Match precondition; 404 is success.
func (c *Client) DeleteIncidentAlertRule(ctx context.Context, projectID, id, lockVersion int64) error {
	err := c.Delete(ctx, incidentAlertRulePath(projectID, id), nil, WithIfMatch(lockVersion))
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
