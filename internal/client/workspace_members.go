package client

import (
	"context"
	"net/url"
)

// WorkspaceMember is a person (or service account) in the token's workspace,
// from GET /api/v1/workspace-members. It exists so a client can resolve a user
// id from an email address: everything else on the API that names a user —
// project memberships, a project's lead — takes the numeric id.
//
// Kind is "human" or "service". Service accounts are visible only to a
// workspace admin: to any other token they are simply absent from the
// directory rather than refused, and a service account's own token is capped
// at the member role, so it never resolves another one.
type WorkspaceMember struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Kind  string `json:"kind"`
}

const workspaceMembersPath = "/workspace-members"

// FindWorkspaceMembersByEmail returns the members whose email address is
// exactly email. The filter is an exact match — the API normalizes case and
// surrounding whitespace, but does not match on a prefix or a fragment — so
// this resolves to one row or none, and an unknown address is an empty slice
// rather than an error.
func (c *Client) FindWorkspaceMembersByEmail(ctx context.Context, email string) ([]WorkspaceMember, error) {
	q := url.Values{}
	q.Set("email", email)
	return List[WorkspaceMember](ctx, c, workspaceMembersPath, WithQuery(q))
}
