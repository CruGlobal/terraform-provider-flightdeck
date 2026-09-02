package client

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// WorkspaceMember is a user of the token's workspace (the workspace member
// directory used to map people by email).
type WorkspaceMember struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

// ListWorkspaceMembers returns the workspace's member directory.
func (c *Client) ListWorkspaceMembers(ctx context.Context) ([]WorkspaceMember, error) {
	return List[WorkspaceMember](ctx, c, "/workspace_members")
}

// FindWorkspaceMemberByEmail resolves a member by email (case-insensitive).
// Absence is a *Error with Status 404.
func (c *Client) FindWorkspaceMemberByEmail(ctx context.Context, email string) (*WorkspaceMember, error) {
	members, err := c.ListWorkspaceMembers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range members {
		if strings.EqualFold(members[i].Email, email) {
			return &members[i], nil
		}
	}
	return nil, &Error{
		Method: http.MethodGet, Path: "/workspace_members", Status: http.StatusNotFound, Code: CodeNotFound,
		Message: fmt.Sprintf("No workspace member with email %q", email),
	}
}

// ProjectMember is a user's role on a project. Role is the effective role key:
// a built-in (guest, member, admin, commenter) or a custom role key defined by
// the workspace's permission scheme.
type ProjectMember struct {
	ID          int64  `json:"id"`
	ProjectID   int64  `json:"project_id"`
	UserID      int64  `json:"user_id"`
	Role        string `json:"role"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	LockVersion *int64 `json:"lock_version"`
}

// ProjectMemberRoles are the built-in roles; custom role keys are also valid.
var ProjectMemberRoles = []string{"guest", "member", "admin", "commenter"}

func membersPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/members"
}

// ListProjectMembers returns a project's explicit member rows.
func (c *Client) ListProjectMembers(ctx context.Context, projectID int64) ([]ProjectMember, error) {
	return List[ProjectMember](ctx, c, membersPath(projectID))
}

// FindProjectMember returns the member row for userID, or a 404 *Error.
func (c *Client) FindProjectMember(ctx context.Context, projectID, userID int64) (*ProjectMember, error) {
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

// AddProjectMember adds a user to a project with a role.
func (c *Client) AddProjectMember(ctx context.Context, projectID int64, fields Fields, idempotencyKey string) (*ProjectMember, error) {
	return CreateWithReplayGuard(ctx, idempotencyKey,
		func(ctx context.Context, key string) (*ProjectMember, error) {
			var m ProjectMember
			if err := c.Post(ctx, membersPath(projectID), map[string]any{"member": fields}, &m, WithIdempotencyKey(key)); err != nil {
				return nil, err
			}
			return &m, nil
		},
		func(ctx context.Context, created *ProjectMember) error {
			_, err := c.FindProjectMember(ctx, projectID, created.UserID)
			return err
		})
}

// UpdateProjectMember changes a member's role. lockVersion is sent as If-Match
// when non-nil (the API may or may not version membership rows).
func (c *Client) UpdateProjectMember(ctx context.Context, projectID, userID int64, fields Fields, lockVersion *int64) (*ProjectMember, error) {
	var m ProjectMember
	var opts []RequestOption
	if lockVersion != nil {
		opts = append(opts, WithIfMatch(*lockVersion))
	}
	err := c.Patch(ctx, membersPath(projectID)+"/"+strconv.FormatInt(userID, 10), map[string]any{"member": fields}, &m, opts...)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// RemoveProjectMember removes a user from a project; 404 is success.
func (c *Client) RemoveProjectMember(ctx context.Context, projectID, userID int64) error {
	err := c.Delete(ctx, membersPath(projectID)+"/"+strconv.FormatInt(userID, 10), nil)
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
