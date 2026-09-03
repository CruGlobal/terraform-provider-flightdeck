package client

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
)

// ProjectMember is a user's role on a project — one membership row. ID is the
// MEMBERSHIP id, which is what every by-id route takes; UserID is the user.
// Role is the effective role key: a built-in (guest, member, admin, commenter)
// or a custom role key defined by the workspace's permission scheme.
// BuiltinRole is the built-in the row rests on even when a custom key applies.
type ProjectMember struct {
	ID          int64  `json:"id"`
	ProjectID   int64  `json:"project_id"`
	UserID      int64  `json:"user_id"`
	Role        string `json:"role"`
	BuiltinRole string `json:"builtin_role"`
	LockVersion int64  `json:"lock_version"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// ResourceID implements Identified.
func (m *ProjectMember) ResourceID() int64 { return m.ID }

// ProjectMemberRoles are the built-in roles; custom role keys are also valid.
var ProjectMemberRoles = []string{"guest", "member", "admin", "commenter"}

const memberRoot = "member"

func membersPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/members"
}

func memberPath(projectID, membershipID int64) string {
	return membersPath(projectID) + "/" + strconv.FormatInt(membershipID, 10)
}

// ListProjectMembers returns a project's membership rows.
func (c *Client) ListProjectMembers(ctx context.Context, projectID int64) ([]ProjectMember, error) {
	return ListResources[ProjectMember](ctx, c, membersPath(projectID), memberRoot)
}

// GetProjectMember fetches one membership row by its id.
func (c *Client) GetProjectMember(ctx context.Context, projectID, membershipID int64) (*ProjectMember, error) {
	return GetResource[*ProjectMember](ctx, c, memberPath(projectID, membershipID), memberRoot)
}

// FindProjectMemberByUser returns the membership row for userID, or a 404
// *Error. An import convenience; the resource itself is keyed by membership id.
func (c *Client) FindProjectMemberByUser(ctx context.Context, projectID, userID int64) (*ProjectMember, error) {
	members, err := c.ListProjectMembers(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for i := range members {
		if members[i].UserID == userID {
			return &members[i], nil
		}
	}
	return nil, &Error{
		Method: http.MethodGet, Path: membersPath(projectID), Status: http.StatusNotFound, Code: CodeNotFound,
		Message: fmt.Sprintf("User %d is not a member of project %d", userID, projectID),
	}
}

// AddProjectMember adds a user to a project with a role, verified through the
// membership's show route.
func (c *Client) AddProjectMember(ctx context.Context, projectID int64, fields Fields, idempotencyKey string) (*ProjectMember, error) {
	return CreateResource(ctx, c, membersPath(projectID), memberRoot, fields, idempotencyKey,
		VerifyByGet(func(ctx context.Context, id int64) (*ProjectMember, error) {
			return c.GetProjectMember(ctx, projectID, id)
		}))
}

// UpdateProjectMember changes a membership's role under an If-Match precondition.
func (c *Client) UpdateProjectMember(ctx context.Context, projectID, membershipID int64, fields Fields, lockVersion int64) (*ProjectMember, error) {
	return PatchResource[*ProjectMember](ctx, c, memberPath(projectID, membershipID), memberRoot, fields, &lockVersion)
}

// RemoveProjectMember deletes a membership under an If-Match precondition; 404
// is success.
func (c *Client) RemoveProjectMember(ctx context.Context, projectID, membershipID, lockVersion int64) error {
	err := c.Delete(ctx, memberPath(projectID, membershipID), nil, WithIfMatch(lockVersion))
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
