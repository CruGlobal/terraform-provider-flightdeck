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

// ResourceID implements Identified.
func (l *Label) ResourceID() int64 { return l.ID }

const labelRoot = "label"

func labelPath(id int64) string { return "/labels/" + strconv.FormatInt(id, 10) }

// ListLabels returns every label of a project.
func (c *Client) ListLabels(ctx context.Context, projectID int64) ([]Label, error) {
	return ListResources[Label](ctx, c, "/projects/"+strconv.FormatInt(projectID, 10)+"/labels", labelRoot)
}

// GetLabel fetches one label by id.
func (c *Client) GetLabel(ctx context.Context, id int64) (*Label, error) {
	return GetResource[*Label](ctx, c, labelPath(id), labelRoot)
}

// CreateLabel creates a label in a project through the verified create path.
func (c *Client) CreateLabel(ctx context.Context, projectID int64, fields Fields, idempotencyKey string) (*Label, error) {
	path := "/projects/" + strconv.FormatInt(projectID, 10) + "/labels"
	return CreateResource(ctx, c, path, labelRoot, fields, idempotencyKey, VerifyByGet(c.GetLabel))
}

// UpdateLabel PATCHes a label with an If-Match precondition.
func (c *Client) UpdateLabel(ctx context.Context, id int64, fields Fields, lockVersion int64) (*Label, error) {
	return PatchResource[*Label](ctx, c, labelPath(id), labelRoot, fields, &lockVersion)
}

// DeleteLabel deletes a label under an If-Match precondition; an already-gone
// 404 is success.
func (c *Client) DeleteLabel(ctx context.Context, id, lockVersion int64) error {
	err := c.Delete(ctx, labelPath(id), nil, WithIfMatch(lockVersion))
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
