package client

import (
	"context"
	"strconv"
)

// StateGroups are the fixed workflow-state groups (State.group enum).
var StateGroups = []string{"backlog", "unstarted", "started", "completed", "cancelled"}

// State is a project's workflow state.
type State struct {
	ID          int64  `json:"id"`
	ProjectID   int64  `json:"project_id"`
	Name        string `json:"name"`
	Group       string `json:"group"`
	Color       string `json:"color"`
	Default     bool   `json:"default"`
	Position    int64  `json:"position"`
	LockVersion int64  `json:"lock_version"`
}

// ResourceID implements Identified.
func (s *State) ResourceID() int64 { return s.ID }

const stateRoot = "state"

func statePath(id int64) string { return "/states/" + strconv.FormatInt(id, 10) }

// ListStates returns every state of a project, in the API's order (group,
// position, id).
func (c *Client) ListStates(ctx context.Context, projectID int64) ([]State, error) {
	return ListResources[State](ctx, c, "/projects/"+strconv.FormatInt(projectID, 10)+"/states", stateRoot)
}

// GetState fetches one state by id.
func (c *Client) GetState(ctx context.Context, id int64) (*State, error) {
	return GetResource[*State](ctx, c, statePath(id), stateRoot)
}

// CreateState creates a state in a project through the verified create path.
func (c *Client) CreateState(ctx context.Context, projectID int64, fields Fields, idempotencyKey string) (*State, error) {
	path := "/projects/" + strconv.FormatInt(projectID, 10) + "/states"
	return CreateResource(ctx, c, path, stateRoot, fields, idempotencyKey, VerifyByGet(c.GetState))
}

// UpdateState PATCHes a state with an If-Match precondition.
func (c *Client) UpdateState(ctx context.Context, id int64, fields Fields, lockVersion int64) (*State, error) {
	return PatchResource[*State](ctx, c, statePath(id), stateRoot, fields, &lockVersion)
}

// DeleteState deletes a state under an If-Match precondition. The API refuses
// (422) to delete a state that still has work items (state_in_use), is the
// project default (state_is_default) or is the project's last state
// (last_state); those errors are returned. An already-gone 404 is success.
func (c *Client) DeleteState(ctx context.Context, id, lockVersion int64) error {
	err := c.Delete(ctx, statePath(id), nil, WithIfMatch(lockVersion))
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
