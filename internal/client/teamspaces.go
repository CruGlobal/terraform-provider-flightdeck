package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Teamspace is a workspace-level team: a name, a description and an optional
// lead. A project can belong to several teams. Names are NOT unique, so the
// id is the only reference that cannot become ambiguous. The team's members
// and the projects it owns are separate link resources (TeamspaceMember,
// TeamspaceProject), each addressed by what it points at.
//
// Description is null when unset: the API stores every blank spelling as
// null, though a team saved from the web form can hold "". LeadID is null
// when the team has no lead.
type Teamspace struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	LeadID      *int64  `json:"lead_id"`
	LockVersion int64   `json:"lock_version"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

// ResourceID implements Identified.
func (t *Teamspace) ResourceID() int64 { return t.ID }

// TeamspaceNameFilterMaxLength is the longest `?name=` the API accepts, not
// counting blank space at either end. A longer one is a 422, so a team with a
// longer name can only be found by its id.
const TeamspaceNameFilterMaxLength = 255

const (
	teamspaceRoot        = "teamspace"
	teamspaceMemberRoot  = "member"
	teamspaceProjectRoot = "project"
	teamspacesPath       = "/teamspaces"
)

func teamspacePath(id int64) string { return teamspacesPath + "/" + strconv.FormatInt(id, 10) }

// ListTeamspaces returns every teamspace in the workspace, ordered by name
// and then id.
func (c *Client) ListTeamspaces(ctx context.Context) ([]Teamspace, error) {
	return ListResources[Teamspace](ctx, c, teamspacesPath, teamspaceRoot)
}

// FindTeamspacesByName returns every teamspace whose whole name is exactly
// name, ignoring letter case. Names are not unique, so this can return
// several, and a caller resolving one team must refuse anything but exactly
// one rather than pick.
//
// A blank name is refused here: the API reads a blank `?name=` as no filter
// and would return every team. Every row that comes back is also checked
// against name, and one that does not carry it is an error rather than
// something to leave out, so whatever the server does with the filter, a
// lookup never narrows several teams down to one by itself.
func (c *Client) FindTeamspacesByName(ctx context.Context, name string) ([]Teamspace, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("a teamspace name to look up must not be blank")
	}
	q := url.Values{}
	q.Set("name", name)
	rows, err := ListResources[Teamspace](ctx, c, teamspacesPath, teamspaceRoot, WithQuery(q))
	if err != nil {
		return nil, err
	}
	var strays []string
	for _, t := range rows {
		if !teamspaceNamesMatch(t.Name, name) {
			strays = append(strays, fmt.Sprintf("%d (%q)", t.ID, t.Name))
		}
	}
	if len(strays) > 0 {
		return nil, fmt.Errorf("asked Flightdeck for the teamspaces named %q, and it also returned teamspace %s, "+
			"whose name does not match; refusing to choose among them. Look the team up by id",
			name, strings.Join(strays, ", "))
	}
	return rows, nil
}

// teamspaceNamesMatch is the API's name filter: the whole name, ignoring
// letter case. The API lowercases both sides in SQL, so two names match when
// they are equal under Unicode case folding or once both are lowercased; the
// two differ for a few letters, such as a dotted capital I.
func teamspaceNamesMatch(a, b string) bool {
	lowerA, lowerB := strings.ToLower(a), strings.ToLower(b)
	return lowerA == lowerB || strings.EqualFold(a, b)
}

// GetTeamspace fetches one teamspace. Another workspace's id is a 404.
func (c *Client) GetTeamspace(ctx context.Context, id int64) (*Teamspace, error) {
	return GetResource[*Teamspace](ctx, c, teamspacePath(id), teamspaceRoot)
}

// CreateTeamspace creates a teamspace through the verified create path, and
// returns the team as it reads back now.
//
// The stable key makes a retried create replay the first one, for 24 hours,
// whatever has happened to that team since. Names are not unique, so a
// replay is only taken as this create's own team while the team still has
// the name, description and lead this create asks for. One that has been
// renamed (say, to make room for a new team with its old name) or otherwise
// changed is somebody else's now, and the create runs again under a fresh
// key, the way a replay of a deleted team does. Two declarations with the
// same arguments still share one team; see PayloadKey.
func (c *Client) CreateTeamspace(ctx context.Context, fields Fields, idempotencyKey string) (*Teamspace, error) {
	var current *Teamspace
	verify := func(ctx context.Context, created *Teamspace) (Verdict, error) {
		t, err := c.GetTeamspace(ctx, created.ID)
		if err != nil {
			if IsNotFound(err) {
				return VerifiedGone, nil
			}
			return VerifiedUnknown, err
		}
		if !teamspaceHas(t, fields) {
			return VerifiedGone, nil
		}
		current = t
		return VerifiedPresent, nil
	}
	if _, err := CreateResource(ctx, c, teamspacesPath, teamspaceRoot, fields, idempotencyKey, verify); err != nil {
		return nil, err
	}
	return current, nil
}

// teamspaceHas reports whether a team has exactly what a create's fields ask
// for: the name, and the description and lead, or none where a field is left
// out.
func teamspaceHas(t *Teamspace, fields Fields) bool {
	if name, _ := fields["name"].(string); t.Name != name {
		return false
	}
	description, _ := fields["description"].(string)
	if (t.Description == nil) != (description == "") || (t.Description != nil && *t.Description != description) {
		return false
	}
	lead, hasLead := fields["lead_id"].(int64)
	if (t.LeadID == nil) == hasLead || (t.LeadID != nil && *t.LeadID != lead) {
		return false
	}
	return true
}

// UpdateTeamspace PATCHes a teamspace under an If-Match precondition.
func (c *Client) UpdateTeamspace(ctx context.Context, id int64, fields Fields, lockVersion int64) (*Teamspace, error) {
	return PatchResource[*Teamspace](ctx, c, teamspacePath(id), teamspaceRoot, fields, &lockVersion)
}

// DeleteTeamspace deletes a teamspace, and with it every member and project
// link it holds (the people and projects themselves are untouched), under an
// If-Match precondition. An already-gone 404 is success.
//
// Deleting a team unlinks every project it owns, so the API asks for the
// right to unlink each one: without it the answer is a 403 that gives a
// count, never the projects' names.
func (c *Client) DeleteTeamspace(ctx context.Context, id, lockVersion int64) error {
	err := c.Delete(ctx, teamspacePath(id), nil, WithIfMatch(lockVersion))
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

// TeamspaceMember is a user's place on a team. It has no id of its own: it is
// addressed by the team and the user.
type TeamspaceMember struct {
	TeamspaceID int64  `json:"teamspace_id"`
	UserID      int64  `json:"user_id"`
	CreatedAt   string `json:"created_at"`
}

// TeamspaceProject says a team owns a project. Like a member, it is addressed
// by the team and the project.
type TeamspaceProject struct {
	TeamspaceID int64  `json:"teamspace_id"`
	ProjectID   int64  `json:"project_id"`
	CreatedAt   string `json:"created_at"`
}

func teamspaceMembersPath(teamspaceID int64) string {
	return teamspacePath(teamspaceID) + "/members"
}

func teamspaceMemberPath(teamspaceID, userID int64) string {
	return teamspaceMembersPath(teamspaceID) + "/" + strconv.FormatInt(userID, 10)
}

func teamspaceProjectsPath(teamspaceID int64) string {
	return teamspacePath(teamspaceID) + "/projects"
}

func teamspaceProjectPath(teamspaceID, projectID int64) string {
	return teamspaceProjectsPath(teamspaceID) + "/" + strconv.FormatInt(projectID, 10)
}

// ListTeamspaceMembers returns a team's members, ordered by user id.
func (c *Client) ListTeamspaceMembers(ctx context.Context, teamspaceID int64) ([]TeamspaceMember, error) {
	return ListResources[TeamspaceMember](ctx, c, teamspaceMembersPath(teamspaceID), teamspaceMemberRoot)
}

// GetTeamspaceMember reads user userID's place on a team. A 404 means the user
// is not on it, or the team is gone.
func (c *Client) GetTeamspaceMember(ctx context.Context, teamspaceID, userID int64) (*TeamspaceMember, error) {
	return GetResource[*TeamspaceMember](ctx, c, teamspaceMemberPath(teamspaceID, userID), teamspaceMemberRoot)
}

// AddTeamspaceMember puts a workspace member on a team. The API answers 201
// for a new member and 200 carrying the existing row when the user is already
// on the team, so the request is safe to repeat and carries no
// Idempotency-Key. A user outside the workspace is a 422 invalid_attribute.
func (c *Client) AddTeamspaceMember(ctx context.Context, teamspaceID, userID int64) (*TeamspaceMember, error) {
	path := teamspaceMembersPath(teamspaceID)
	got, err := postLink[TeamspaceMember](ctx, c, path, teamspaceMemberRoot, Fields{"user_id": userID})
	if err != nil {
		return nil, err
	}
	if got.TeamspaceID != teamspaceID || got.UserID != userID {
		return nil, linkMismatch(path, "member", fmt.Sprintf("user %d on teamspace %d", userID, teamspaceID),
			fmt.Sprintf("user %d on teamspace %d", got.UserID, got.TeamspaceID))
	}
	return got, nil
}

// RemoveTeamspaceMember takes a user off a team. A 404 (not on the team, or
// the team is gone) is success, and a delete that loses a race is sent once
// more.
func (c *Client) RemoveTeamspaceMember(ctx context.Context, teamspaceID, userID int64) error {
	return c.deleteGone(ctx, teamspaceMemberPath(teamspaceID, userID))
}

// ListTeamspaceProjects returns the links to the projects a team owns that the
// token may read, ordered by project id. A link to a project the token cannot
// read is left out.
func (c *Client) ListTeamspaceProjects(ctx context.Context, teamspaceID int64) ([]TeamspaceProject, error) {
	return ListResources[TeamspaceProject](ctx, c, teamspaceProjectsPath(teamspaceID), teamspaceProjectRoot)
}

// GetTeamspaceProject reads a team's link to a project. A 404 means there is
// no such link, the team is gone, or the token cannot see the project (or it
// is being deleted); a 403 means the token can see the project but may not
// read it.
func (c *Client) GetTeamspaceProject(ctx context.Context, teamspaceID, projectID int64) (*TeamspaceProject, error) {
	return GetResource[*TeamspaceProject](ctx, c, teamspaceProjectPath(teamspaceID, projectID), teamspaceProjectRoot)
}

// LinkTeamspaceProject says a team owns a project. Like a member, a link that
// already exists is a 200 carrying it, so the request is safe to repeat and
// carries no Idempotency-Key. The token needs both read and administer on the
// project (a 403 otherwise), and a project it cannot see, or one being
// deleted, is the same 422 invalid_attribute an id that names nothing gets.
func (c *Client) LinkTeamspaceProject(ctx context.Context, teamspaceID, projectID int64) (*TeamspaceProject, error) {
	path := teamspaceProjectsPath(teamspaceID)
	got, err := postLink[TeamspaceProject](ctx, c, path, teamspaceProjectRoot, Fields{"project_id": projectID})
	if err != nil {
		return nil, err
	}
	if got.TeamspaceID != teamspaceID || got.ProjectID != projectID {
		return nil, linkMismatch(path, "project link", fmt.Sprintf("project %d on teamspace %d", projectID, teamspaceID),
			fmt.Sprintf("project %d on teamspace %d", got.ProjectID, got.TeamspaceID))
	}
	return got, nil
}

// UnlinkTeamspaceProject removes a team's link to a project. A 404 (no such
// link, the team is gone, or the project cannot be seen) is success, and a
// delete that loses a race is sent once more.
func (c *Client) UnlinkTeamspaceProject(ctx context.Context, teamspaceID, projectID int64) error {
	return c.deleteGone(ctx, teamspaceProjectPath(teamspaceID, projectID))
}

// postLink POSTs {rootKey: fields} to a link collection that answers a repeat
// with the existing row, and decodes the link. Because a repeat is an answer
// rather than a duplicate, the request may be retried after a dropped
// connection or a gateway error without an Idempotency-Key.
func postLink[T any](ctx context.Context, c *Client, path, rootKey string, fields Fields) (*T, error) {
	var raw json.RawMessage
	if err := c.Post(ctx, path, map[string]any{rootKey: fields}, &raw, safeToRepeat()); err != nil {
		return nil, err
	}
	got, err := DecodeResource[*T](raw, rootKey)
	if err != nil || got == nil {
		return nil, &Error{Method: http.MethodPost, Path: path, Status: http.StatusCreated, Err: err,
			Message: fmt.Sprintf("response could not be decoded as a %s (%s)", rootKey, shapeOf(raw))}
	}
	return got, nil
}

func linkMismatch(path, what, asked, got string) *Error {
	return &Error{Method: http.MethodPost, Path: path, Status: http.StatusCreated,
		Message: fmt.Sprintf("asked for %s but the API answered with %s; refusing to record a %s it did not ask for. "+
			"Check the deployed Flightdeck API version", asked, got, what)}
}
