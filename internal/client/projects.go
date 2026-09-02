package client

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// projectRoot is the request/response root key for projects.
const projectRoot = "project"

// Project is the API's project shape (the flat `project` serializer; the
// single-project `project_detail` shape adds `urls`, which the provider does
// not manage and ignores).
type Project struct {
	ID                 int64           `json:"id"`
	Name               string          `json:"name"`
	Identifier         string          `json:"identifier"`
	Description        *string         `json:"description"`
	Emoji              *string         `json:"emoji"`
	Archived           bool            `json:"archived"`
	Features           map[string]bool `json:"features"`
	GithubRepoFullName *string         `json:"github_repo_full_name"`
	LockVersion        int64           `json:"lock_version"`
	CreatedAt          string          `json:"created_at"`
	UpdatedAt          string          `json:"updated_at"`
}

// ResourceID implements Identified.
func (p *Project) ResourceID() int64 { return p.ID }

func projectPath(id int64) string { return "/projects/" + strconv.FormatInt(id, 10) }

// ListProjects returns every project the token may read.
func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	return ListResources[Project](ctx, c, "/projects", projectRoot)
}

// GetProject fetches one project. A 404 covers unknown ids, ids in another
// workspace, and projects mid-teardown.
func (c *Client) GetProject(ctx context.Context, id int64) (*Project, error) {
	return GetResource[*Project](ctx, c, projectPath(id), projectRoot)
}

// FindProjectByIdentifier lists projects and returns the one with the given
// identifier (case-insensitive, as the model upcases it). Absence is reported
// as a *Error with Status 404 so callers can treat it like GetProject.
func (c *Client) FindProjectByIdentifier(ctx context.Context, identifier string) (*Project, error) {
	projects, err := c.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	for i := range projects {
		if strings.EqualFold(projects[i].Identifier, identifier) {
			return &projects[i], nil
		}
	}
	return nil, &Error{
		Method: http.MethodGet, Path: "/projects", Status: http.StatusNotFound, Code: CodeNotFound,
		Message: fmt.Sprintf("No project with identifier %q", identifier),
	}
}

// CreateProject creates a project under the stable idempotency key (see
// PayloadKey) and verifies it by reading the returned id back.
func (c *Client) CreateProject(ctx context.Context, fields Fields, idempotencyKey string) (*Project, error) {
	return CreateResource(ctx, c, "/projects", projectRoot, fields, idempotencyKey, VerifyByGet(c.GetProject))
}

// UpdateProject PATCHes the given fields with an If-Match precondition on
// lockVersion. A lost race is a *Error for which IsStale is true.
func (c *Client) UpdateProject(ctx context.Context, id int64, fields Fields, lockVersion int64) (*Project, error) {
	return PatchResource[*Project](ctx, c, projectPath(id), projectRoot, fields, &lockVersion)
}

// DeleteProject asks the API to delete a project. The API soft-marks the
// project and tears it down asynchronously (202 Accepted); from that moment
// it is unreachable, so an already-gone 404 is also success.
func (c *Client) DeleteProject(ctx context.Context, id int64) error {
	err := c.Delete(ctx, projectPath(id), nil)
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
