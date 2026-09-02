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

// ListStates returns every state of a project, in the API's order (group,
// position, id).
func (c *Client) ListStates(ctx context.Context, projectID int64) ([]State, error) {
	return List[State](ctx, c, "/projects/"+strconv.FormatInt(projectID, 10)+"/states")
}

// GetState fetches one state by id.
func (c *Client) GetState(ctx context.Context, id int64) (*State, error) {
	var s State
	if err := c.Get(ctx, "/states/"+strconv.FormatInt(id, 10), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// CreateState creates a state in a project, guarded like CreateProject.
func (c *Client) CreateState(ctx context.Context, projectID int64, fields Fields, idempotencyKey string) (*State, error) {
	path := "/projects/" + strconv.FormatInt(projectID, 10) + "/states"
	return CreateWithReplayGuard(ctx, idempotencyKey,
		func(ctx context.Context, key string) (*State, error) {
			var s State
			if err := c.Post(ctx, path, map[string]any{"state": fields}, &s, WithIdempotencyKey(key)); err != nil {
				return nil, err
			}
			return &s, nil
		},
		func(ctx context.Context, created *State) error {
			_, err := c.GetState(ctx, created.ID)
			return err
		})
}

// UpdateState PATCHes a state with an If-Match precondition.
func (c *Client) UpdateState(ctx context.Context, id int64, fields Fields, lockVersion int64) (*State, error) {
	var s State
	if err := c.Patch(ctx, "/states/"+strconv.FormatInt(id, 10), map[string]any{"state": fields}, &s, WithIfMatch(lockVersion)); err != nil {
		return nil, err
	}
	return &s, nil
}

// DeleteState deletes a state. The API refuses (422) to delete a state that
// still has work items or is the project default; that error is returned.
// An already-gone 404 is success.
func (c *Client) DeleteState(ctx context.Context, id int64) error {
	err := c.Delete(ctx, "/states/"+strconv.FormatInt(id, 10), nil)
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
