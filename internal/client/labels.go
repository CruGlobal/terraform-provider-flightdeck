package client

import (
	"context"
	"strconv"
)

// Label is a project label.
type Label struct {
	ID          int64  `json:"id"`
	ProjectID   int64  `json:"project_id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	LockVersion int64  `json:"lock_version"`
}

// ListLabels returns every label of a project.
func (c *Client) ListLabels(ctx context.Context, projectID int64) ([]Label, error) {
	return List[Label](ctx, c, "/projects/"+strconv.FormatInt(projectID, 10)+"/labels")
}

// GetLabel fetches one label by id.
func (c *Client) GetLabel(ctx context.Context, id int64) (*Label, error) {
	var l Label
	if err := c.Get(ctx, "/labels/"+strconv.FormatInt(id, 10), &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// CreateLabel creates a label in a project, guarded like CreateProject.
func (c *Client) CreateLabel(ctx context.Context, projectID int64, fields Fields, idempotencyKey string) (*Label, error) {
	path := "/projects/" + strconv.FormatInt(projectID, 10) + "/labels"
	return CreateWithReplayGuard(ctx, idempotencyKey,
		func(ctx context.Context, key string) (*Label, error) {
			var l Label
			if err := c.Post(ctx, path, map[string]any{"label": fields}, &l, WithIdempotencyKey(key)); err != nil {
				return nil, err
			}
			return &l, nil
		},
		func(ctx context.Context, created *Label) error {
			_, err := c.GetLabel(ctx, created.ID)
			return err
		})
}

// UpdateLabel PATCHes a label with an If-Match precondition.
func (c *Client) UpdateLabel(ctx context.Context, id int64, fields Fields, lockVersion int64) (*Label, error) {
	var l Label
	if err := c.Patch(ctx, "/labels/"+strconv.FormatInt(id, 10), map[string]any{"label": fields}, &l, WithIfMatch(lockVersion)); err != nil {
		return nil, err
	}
	return &l, nil
}

// DeleteLabel deletes a label; an already-gone 404 is success.
func (c *Client) DeleteLabel(ctx context.Context, id int64) error {
	err := c.Delete(ctx, "/labels/"+strconv.FormatInt(id, 10), nil)
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
