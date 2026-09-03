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
}

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["github_integrations"] = &githubIntegrationStore{byID: map[int64]*GithubIntegration{}, unreachable: map[string]bool{}}
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

// enabledLinkExists mirrors GithubIntegration.for_repo: one ENABLED
// integration per repository (case-insensitive) in the workspace.
func (s *Server) enabledLinkExists(repo string, exceptID int64) bool {
	for _, other := range s.githubIntegrations().byID {
		if other.ID != exceptID && other.Enabled && strings.EqualFold(other.RepoFullName, repo) && s.liveProject(other.ProjectID) != nil {
			return true
		}
	}
	return false
}

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
			return http.StatusNotFound, map[string]any{"error": "Not found", "code": "not_found"}
		}
		repo := strings.TrimSpace(asString(attrs["repo_full_name"]))
		if !repoFullNameForm.MatchString(repo) {
			return http.StatusUnprocessableEntity, map[string]any{"error": "repo_full_name must be in owner/repo form", "code": "invalid_attribute"}
		}
		g := &GithubIntegration{ID: s.id(), ProjectID: pid, RepoFullName: repo, Enabled: true, CreatedAt: time.Now()}
		if v, has := attrs["enabled"]; has && v != nil {
			g.Enabled = truthy(v)
		}
		if v, has := attrs["secret"]; has && strings.TrimSpace(asString(v)) != "" {
			// Caller-managed: the secret is stored, nothing happens on GitHub.
			g.SecretSet = true
		} else {
			// Flightdeck-managed: generate the secret and register the webhook
			// through the GitHub App, which must be able to reach the repo.
			if s.githubIntegrations().unreachable[strings.ToLower(repo)] {
				return http.StatusUnprocessableEntity, map[string]any{
					"error": "The GitHub App cannot reach " + repo + "; install it on the repository (or supply a secret and register the webhook yourself)",
					"code":  "repo_unreachable"}
			}
			g.WebhookRegistered = true
		}
		if g.Enabled && s.enabledLinkExists(repo, g.ID) {
			return http.StatusConflict, map[string]any{
				"error": repo + " is already linked to an enabled integration in this workspace", "code": "repo_already_linked"}
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
	if !checkIfMatch(w, r, g.LockVersion) {
		return
	}
	if _, has := attrs["repo_full_name"]; has {
		writeError(w, http.StatusUnprocessableEntity, "invalid_attribute",
			"repo_full_name cannot be changed on an existing integration — unlink it and link the new repository")
		return
	}
	if _, has := attrs["secret"]; has {
		writeError(w, http.StatusUnprocessableEntity, "invalid_attribute",
			"secret cannot be changed on an existing integration — unlink it and link again")
		return
	}
	if v, has := attrs["enabled"]; has && v != nil {
		enabled := truthy(v)
		if enabled && !g.Enabled && s.enabledLinkExists(g.RepoFullName, g.ID) {
			writeError(w, http.StatusConflict, "repo_already_linked",
				g.RepoFullName+" is already linked to an enabled integration in this workspace")
			return
		}
		g.Enabled = enabled
	}
	g.LockVersion++
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
