package flightdecktest

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

var repoFullNameForm = regexp.MustCompile(`^[^/\s]+/[^/\s]+$`)

// GithubIntegration is the fake's stored project<->repository link. SecretSet
// records whether the caller supplied the secret (caller-managed webhook) or
// Flightdeck generated it and registered the webhook itself.
type GithubIntegration struct {
	ID                int64
	ProjectID         int64
	RepoFullName      string
	Enabled           bool
	WebhookRegistered bool
	SecretSet         bool
	LockVersion       int64
	CreatedAt         time.Time
}

type githubIntegrationStore struct {
	byID map[int64]*GithubIntegration
	// unreachable repositories: the GitHub App cannot see them.
	unreachable map[string]bool
	// hookPresent repositories already have a webhook targeting the receiver;
	// Flightdeck leaves it alone and does not claim it (webhook_registered false).
	hookPresent map[string]bool
}

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["github_integrations"] = &githubIntegrationStore{byID: map[int64]*GithubIntegration{}, unreachable: map[string]bool{}, hookPresent: map[string]bool{}}
		mux.HandleFunc("GET /api/v1/projects/{project_id}/github-integrations", s.listGithubIntegrations)
		mux.HandleFunc("POST /api/v1/projects/{project_id}/github-integrations", s.createGithubIntegration)
		mux.HandleFunc("GET /api/v1/github-integrations/{id}", s.showGithubIntegration)
		mux.HandleFunc("PATCH /api/v1/github-integrations/{id}", s.updateGithubIntegration)
		mux.HandleFunc("DELETE /api/v1/github-integrations/{id}", s.destroyGithubIntegration)
	})
}

func (s *Server) githubIntegrations() *githubIntegrationStore {
	store, _ := s.stores["github_integrations"].(*githubIntegrationStore)
	return store
}

// MarkRepoUnreachable makes the GitHub App unable to reach a repository, so a
// create for it is a 422 repo_unreachable with no row created.
func (s *Server) MarkRepoUnreachable(repo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.githubIntegrations().unreachable[strings.ToLower(repo)] = true
}

// MarkHookAlreadyPresent makes a repository already carry a webhook aimed at
// the receiver, so a managed create succeeds without registering (and without
// claiming) one: webhook_registered is false.
func (s *Server) MarkHookAlreadyPresent(repo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.githubIntegrations().hookPresent[strings.ToLower(repo)] = true
}

// GithubIntegration returns a stored integration or nil.
func (s *Server) GithubIntegration(id int64) *GithubIntegration {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.githubIntegrations().byID[id]
	if g == nil {
		return nil
	}
	cp := *g
	return &cp
}

// TouchGithubIntegration simulates an out-of-band edit that bumps lock_version.
func (s *Server) TouchGithubIntegration(id int64, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.githubIntegrations().byID[id]; g != nil {
		g.Enabled = enabled
		g.LockVersion++
	}
}

func (s *Server) liveGithubIntegration(id int64) *GithubIntegration {
	g := s.githubIntegrations().byID[id]
	if g == nil || s.liveProject(g.ProjectID) == nil {
		return nil
	}
	return g
}

func serializeGithubIntegration(g *GithubIntegration) map[string]any {
	return map[string]any{
		"id": g.ID, "project_id": g.ProjectID, "repo_full_name": g.RepoFullName,
		"enabled": g.Enabled, "webhook_registered": g.WebhookRegistered,
		"lock_version": g.LockVersion, "created_at": iso(g.CreatedAt), "updated_at": iso(g.CreatedAt),
	}
}

// repoLinked mirrors the controller's guard_already_linked!: ANY row for the
// repository (case-insensitive), enabled or not, blocks a new link.
func (s *Server) repoLinked(repo string) bool {
	for _, other := range s.githubIntegrations().byID {
		if strings.EqualFold(other.RepoFullName, repo) {
			return true
		}
	}
	return false
}

// otherEnabledOnProject mirrors GithubIntegration#only_one_enabled_per_project.
func (s *Server) otherEnabledOnProject(projectID, exceptID int64) bool {
	for _, other := range s.githubIntegrations().byID {
		if other.ID != exceptID && other.ProjectID == projectID && other.Enabled {
			return true
		}
	}
	return false
}

// Workspace admin for every verb (existence is checked first so a non-admin
// cannot tell project ids apart by 403 vs 404).
func (s *Server) listGithubIntegrations(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	s.mu.Lock()
	if s.liveProject(pid) == nil {
		s.mu.Unlock()
		notFound(w)
		return
	}
	if !s.requireWorkspaceAdmin(w) {
		s.mu.Unlock()
		return
	}
	var rows []*GithubIntegration
	for _, g := range s.githubIntegrations().byID {
		if g.ProjectID == pid {
			rows = append(rows, g)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	items := make([]any, 0, len(rows))
	for _, g := range rows {
		items = append(items, serializeGithubIntegration(g))
	}
	s.mu.Unlock()
	writeCollection(w, r, items)
}

func (s *Server) showGithubIntegration(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.liveGithubIntegration(id)
	if g == nil {
		notFound(w)
		return
	}
	if !s.requireWorkspaceAdmin(w) {
		return
	}
	writeJSON(w, http.StatusOK, serializeGithubIntegration(g))
}

func (s *Server) createGithubIntegration(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "github_integration")
	if !ok {
		return
	}
	s.withIdempotency(w, r, "github_integration", func() (int, any) {
		s.mu.Lock()
		defer s.mu.Unlock()
		project := s.liveProject(pid)
		if project == nil {
			return http.StatusNotFound, errorBody("Not found", "not_found")
		}
		if !s.workspaceAdmin {
			return http.StatusForbidden, errorBody("This action requires workspace owner or admin rights.", "forbidden")
		}
		repo := strings.TrimSpace(asString(attrs["repo_full_name"]))
		if !repoFullNameForm.MatchString(repo) {
			return http.StatusUnprocessableEntity, errorBody("repo_full_name is required, in owner/repo form", "invalid_attribute")
		}
		// `enabled` is not read on create: a new integration is always enabled.
		g := &GithubIntegration{ID: s.id(), ProjectID: pid, RepoFullName: repo, Enabled: true, CreatedAt: time.Now()}
		// A blank secret counts as absent (managed mode); a short one is refused.
		secret := asString(attrs["secret"])
		if strings.TrimSpace(secret) != "" {
			if len(secret) < 16 {
				return http.StatusUnprocessableEntity, map[string]any{
					"error": "secret must be at least 16 characters (omit it entirely to have Flightdeck generate one and register the webhook for you)",
					"code":  "invalid_attribute"}
			}
			// Caller-managed: the secret is stored, GitHub is never called.
			g.SecretSet = true
		}
		if s.repoLinked(repo) {
			return http.StatusUnprocessableEntity, map[string]any{
				"error": repo + " is already linked to a Flightdeck project. Delete that integration first, or link a different repository.",
				"code":  "repo_already_linked"}
		}
		if s.otherEnabledOnProject(pid, g.ID) {
			return http.StatusUnprocessableEntity, map[string]any{
				"error": "Project this project already has an enabled GitHub integration — disable or remove it first", "code": "validation_failed"}
		}
		if !g.SecretSet {
			// Flightdeck-managed: register the webhook through the GitHub App,
			// inside the same transaction as the insert: unreachable = no row.
			if s.githubIntegrations().unreachable[strings.ToLower(repo)] {
				return http.StatusUnprocessableEntity, map[string]any{
					"error": "Flightdeck's GitHub App cannot reach " + repo + ". Install the GitHub App in workspace settings, or supply your own `secret` and register the webhook on GitHub yourself.",
					"code":  "repo_unreachable"}
			}
			// A hook already aimed at the receiver is skipped and not claimed.
			g.WebhookRegistered = !s.githubIntegrations().hookPresent[strings.ToLower(repo)]
		}
		s.githubIntegrations().byID[g.ID] = g
		// The column side effect: the project records the mapping.
		linked := g.RepoFullName
		project.GithubRepoFullName = &linked
		project.LockVersion++
		return http.StatusCreated, serializeGithubIntegration(g)
	})
}

func (s *Server) updateGithubIntegration(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "github_integration")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.liveGithubIntegration(id)
	if g == nil {
		notFound(w)
		return
	}
	if !s.requireWorkspaceAdmin(w) {
		return
	}
	if !checkIfMatch(w, r, g.LockVersion) {
		return
	}
	// PATCH accepts only `enabled`; repo_full_name may be re-sent unchanged.
	if v, has := attrs["repo_full_name"]; has && !strings.EqualFold(strings.TrimSpace(asString(v)), g.RepoFullName) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_attribute",
			"repo_full_name cannot be changed on an existing integration — DELETE it and POST a new one")
		return
	}
	if _, has := attrs["secret"]; has {
		writeError(w, http.StatusUnprocessableEntity, "invalid_attribute",
			"secret cannot be changed over the API — rotate it in Settings -> Integrations, or DELETE this integration and POST a new one")
		return
	}
	wasEnabled := g.Enabled
	if v, has := attrs["enabled"]; has && v != nil {
		enabled := truthy(v)
		if enabled && s.otherEnabledOnProject(g.ProjectID, g.ID) {
			writeError(w, http.StatusUnprocessableEntity, "validation_failed",
				"Project this project already has an enabled GitHub integration — disable or remove it first")
			return
		}
		g.Enabled = enabled
	}
	g.LockVersion++
	if g.Enabled && !wasEnabled {
		// Re-enabling mirrors the column again.
		if p := s.projects().byID[g.ProjectID]; p != nil {
			linked := g.RepoFullName
			p.GithubRepoFullName = &linked
			p.LockVersion++
		}
	}
	writeJSON(w, http.StatusOK, serializeGithubIntegration(g))
}

func (s *Server) destroyGithubIntegration(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.liveGithubIntegration(id)
	if g == nil {
		notFound(w)
		return
	}
	if !s.requireWorkspaceAdmin(w) {
		return
	}
	if !checkIfMatch(w, r, g.LockVersion) {
		return
	}
	delete(s.githubIntegrations().byID, id)
	// Unlink clears the project's mapping when it still points at this repo.
	if p := s.projects().byID[g.ProjectID]; p != nil && p.GithubRepoFullName != nil && strings.EqualFold(*p.GithubRepoFullName, g.RepoFullName) {
		p.GithubRepoFullName = nil
		p.LockVersion++
	}
	w.WriteHeader(http.StatusNoContent)
}
