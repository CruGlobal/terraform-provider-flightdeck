package client

import (
	"context"
	"strconv"
)

// AgentWork is a project's agent work settings at GET/PATCH
// /api/v1/projects/:project_id/agent-work: whether Flightdeck's AI agents may
// take the project's labelled work items, which label marks an item for them,
// the service account they work as, and how much they may spend and run.
//
// Unlike self-healing, the settings are their own row, so LockVersion is that
// row's, not the project's: a write here never conflicts with a project
// update, and does not bump the project's lock_version. A project whose
// settings were never saved reads the defaults at version 0 (UpdatedAt null),
// and the first write that changes something creates the row at version 1.
// A write that changes nothing creates nothing.
type AgentWork struct {
	ProjectID           int64    `json:"project_id"`
	Enabled             bool     `json:"enabled"`
	Kinds               []string `json:"kinds"`
	LabelID             *int64   `json:"label_id"`
	LabelChosenAt       *string  `json:"label_chosen_at"`
	AcceptMachineLabels bool     `json:"accept_machine_labels"`
	AgentAccountID      *int64   `json:"agent_account_id"`
	BaseRef             string   `json:"base_ref"`
	MaxInProgress       int64    `json:"max_in_progress"`
	DailyBudgetUSD      float64  `json:"daily_budget_usd"`
	TaskMaxUSD          float64  `json:"task_max_usd"`
	TaskMaxMinutes      int64    `json:"task_max_minutes"`
	QueueMinutes        int64    `json:"queue_minutes"`
	Runbook             string   `json:"runbook"`
	// Blockers lists what would stop Flightdeck sending this project's work
	// right now, empty when nothing would. It is worked out on every read and
	// changes as the project does (a label deleted, the day's budget spent),
	// so it is read-only and never stored.
	Blockers    []AgentWorkBlocker `json:"blockers"`
	LockVersion int64              `json:"lock_version"`
	UpdatedAt   *string            `json:"updated_at"`
}

// AgentWorkBlocker is one reason Flightdeck would not send a project's work
// right now. Code is a stable slug; Message says what is in the way.
type AgentWorkBlocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// AgentWorkKinds are the kinds of work the API accepts in `kinds`.
var AgentWorkKinds = []string{"implement-work-item"}

// AgentWorkRunbooks are the runbooks the API accepts in `runbook`.
var AgentWorkRunbooks = []string{"implement-work-item@1"}

// AgentWorkSettingKeys are the writable settings, in the order the API
// documents them. Every other key the read reports (project_id, blockers,
// label_chosen_at, updated_at) is refused on a write with a 422.
//
// A key sent as null or a blank string is "no opinion" and leaves the stored
// value alone, EXCEPT label_id and agent_account_id, where a blank clears the
// choice. So a client must leave an unmanaged key out of the write rather
// than send it empty.
var AgentWorkSettingKeys = []string{
	"enabled", "kinds", "label_id", "accept_machine_labels", "agent_account_id", "base_ref",
	"max_in_progress", "daily_budget_usd", "task_max_usd", "task_max_minutes", "queue_minutes", "runbook",
}

const agentWorkRoot = "agent_work"

func agentWorkPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/agent-work"
}

// GetAgentWork reads a project's agent work settings, or the defaults when
// none were ever saved (reading never creates anything). Workspace owners and
// admins only: other tokens get a 403. A Flightdeck without the endpoint 404s.
func (c *Client) GetAgentWork(ctx context.Context, projectID int64) (*AgentWork, error) {
	return GetResource[*AgentWork](ctx, c, agentWorkPath(projectID), agentWorkRoot)
}

// UpdateAgentWork PATCHes the given settings under an If-Match carrying the
// SETTINGS row's lock_version (0 for a project whose settings were never
// saved). Only the keys present change. A stale version is a 409
// stale_object; a value of the wrong JSON type is a 422 invalid_attribute,
// and one that breaks a rule (a range, more than two decimal places on a
// money field, a task costing more than the day) a 422 validation_failed.
// Either way nothing is saved.
func (c *Client) UpdateAgentWork(ctx context.Context, projectID int64, settings Fields, lockVersion int64) (*AgentWork, error) {
	return PatchResource[*AgentWork](ctx, c, agentWorkPath(projectID), agentWorkRoot, settings, &lockVersion)
}
