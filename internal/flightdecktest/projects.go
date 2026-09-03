package flightdecktest

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// DefaultFeatures mirrors Project::DEFAULT_FEATURES — what a read reports for
// a key the project has never stored.
var DefaultFeatures = map[string]bool{
	"cycles": true, "modules": true, "milestones": true, "views": true, "pages": true,
	"meeting_notes": true, "decisions": true, "intake": false, "estimates": true,
	"errors": false, "incidents": false, "self_healing": false, "slack": true,
}

// ToggleableFeatures mirrors Project::TOGGLEABLE_FEATURES — the keys the API
// accepts on write. self_healing and slack are deliberately absent.
var ToggleableFeatures = []string{
	"cycles", "modules", "milestones", "views", "pages", "meeting_notes",
	"decisions", "intake", "errors", "incidents", "estimates",
}

var identifierFormat = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}$`)

// Project is the fake's stored project.
type Project struct {
	ID                 int64
	Name               string
	Identifier         string
	Description        *string
	Emoji              string
	Archived           bool
	Features           map[string]bool // stored overrides only
	GithubRepoFullName *string
	LockVersion        int64
	LeadID             int64
	Network            string
	// SelfHealing holds the stored jsonb overrides; reads resolve defaults.
	SelfHealing map[string]any
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// Deleting mirrors projects.deleting_at: the row still exists but every
	// /api/v1 lookup goes through .not_deleting and 404s.
	Deleting bool
}

type projectStore struct {
	byID map[int64]*Project
	// forcedFeatures are toggles the server reports at a fixed value
	// whatever a client writes, like a plan-gated feature.
	forcedFeatures map[string]bool
}

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["projects"] = &projectStore{byID: map[int64]*Project{}, forcedFeatures: map[string]bool{}}
		mux.HandleFunc("GET /api/v1/projects", s.listProjects)
		mux.HandleFunc("POST /api/v1/projects", s.createProject)
		mux.HandleFunc("GET /api/v1/projects/{id}", s.showProject)
		mux.HandleFunc("PATCH /api/v1/projects/{id}", s.updateProject)
		mux.HandleFunc("DELETE /api/v1/projects/{id}", s.destroyProject)
	})
}

func (s *Server) projects() *projectStore {
	store, _ := s.stores["projects"].(*projectStore)
	return store
}

// AddProject seeds a project directly (no HTTP), returning its id.
func (s *Server) AddProject(name, identifier string) *Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addProjectLocked(map[string]any{"name": name, "identifier": identifier})
}

// ForceFeature makes the fake report a feature toggle at a fixed value for
// every project, whatever clients write to it.
func (s *Server) ForceFeature(key string, value bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projects().forcedFeatures[key] = value
}

// LinkGithubRepo sets the read-only repository mapping the way Settings ->
// Integrations would.
func (s *Server) LinkGithubRepo(projectID int64, repo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.projects().byID[projectID]; p != nil {
		p.GithubRepoFullName = &repo
		p.LockVersion++
	}
}

// SetNetwork flips the read-only visibility the way Settings -> Members would.
func (s *Server) SetNetwork(projectID int64, network string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.projects().byID[projectID]; p != nil {
		p.Network = network
		p.LockVersion++
	}
}

// AllProjectIDs returns every stored project id, deleting ones included.
func (s *Server) AllProjectIDs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []int64
	for id := range s.projects().byID {
		ids = append(ids, id)
	}
	return ids
}

// Project returns a stored project by id (nil if absent), including ones
// marked for deletion.
func (s *Server) Project(id int64) *Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.projects().byID[id]
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// TouchProject simulates an out-of-band edit: it renames the project and bumps
// lock_version, so a provider write carrying the old version must 409.
func (s *Server) TouchProject(id int64, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.projects().byID[id]; p != nil {
		p.Name = name
		p.LockVersion++
		p.UpdatedAt = time.Now()
	}
}

// DeleteProjectOutOfBand marks a project deleting, as the web UI's delete does.
func (s *Server) DeleteProjectOutOfBand(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.projects().byID[id]; p != nil {
		p.Deleting = true
	}
}

// liveProject resolves a project the way the API does: .not_deleting.find.
func (s *Server) liveProject(id int64) *Project {
	p := s.projects().byID[id]
	if p == nil || p.Deleting {
		return nil
	}
	return p
}

func (s *Server) serializeProject(p *Project, detail bool) map[string]any {
	features := map[string]bool{}
	for k, v := range DefaultFeatures {
		features[k] = v
	}
	for k, v := range p.Features {
		features[k] = v
	}
	for k, v := range s.projects().forcedFeatures {
		features[k] = v
	}
	out := map[string]any{
		"id": p.ID, "name": p.Name, "identifier": p.Identifier,
		"description": p.Description, "emoji": p.Emoji, "archived": p.Archived,
		"features": features, "github_repo_full_name": p.GithubRepoFullName,
		"lead_id": p.LeadID, "network": p.Network,
		"lock_version": p.LockVersion,
		"created_at":   iso(p.CreatedAt), "updated_at": iso(p.UpdatedAt),
	}
	if detail {
		out["urls"] = []any{}
	}
	return out
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	var live []*Project
	for _, p := range s.projects().byID {
		if !p.Deleting {
			live = append(live, p)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Name < live[j].Name })
	items := make([]any, 0, len(live))
	for _, p := range live {
		items = append(items, s.serializeProject(p, false))
	}
	s.mu.Unlock()
	writeCollection(w, r, items)
}

func (s *Server) showProject(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	p := s.liveProject(id)
	var body map[string]any
	if p != nil {
		body = s.serializeProject(p, true)
	}
	s.mu.Unlock()
	if body == nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// applyProjectAttrs mirrors Api::ProjectAttributes.apply!. It returns an
// (status, code, message) triple on rejection; status 0 means accepted.
func (s *Server) applyProjectAttrs(p *Project, attrs map[string]any) (int, string, string) {
	// Api::ProjectAttributes::READ_ONLY: named in a write -> 422 invalid_attribute.
	var readOnly []string
	for _, k := range []string{"github_repo_full_name", "network"} {
		if _, named := attrs[k]; named {
			readOnly = append(readOnly, k+" is read-only over the API")
		}
	}
	if len(readOnly) > 0 {
		return http.StatusUnprocessableEntity, "invalid_attribute", strings.Join(readOnly, "; ")
	}
	if v, ok := attrs["lead_id"]; ok {
		if v == nil {
			p.LeadID = 0
		} else if id, isNum := asInt64(v); isNum && s.workspaceUser(id) != nil {
			p.LeadID = id
		} else {
			return http.StatusNotFound, "not_found", "Not found"
		}
	}
	if v, ok := attrs["name"]; ok {
		p.Name = asString(v)
	}
	if v, ok := attrs["description"]; ok {
		if v == nil {
			p.Description = nil
		} else {
			str := asString(v)
			p.Description = &str
		}
	}
	if v, ok := attrs["identifier"]; ok {
		p.Identifier = strings.ToUpper(strings.TrimSpace(asString(v)))
	}
	if v, ok := attrs["emoji"]; ok && v != nil {
		p.Emoji = asString(v)
	}
	if v, ok := attrs["archived"]; ok {
		p.Archived = truthy(v)
	}
	if v, ok := attrs["features"]; ok {
		submitted, isMap := v.(map[string]any)
		if !isMap {
			return http.StatusUnprocessableEntity, "invalid_attribute", "features must be an object of key => boolean"
		}
		var unknown []string
		for k := range submitted {
			if !contains(ToggleableFeatures, k) {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			return http.StatusUnprocessableEntity, "invalid_attribute",
				"unknown feature: " + strings.Join(unknown, ", ") + " (settable: " + strings.Join(ToggleableFeatures, ", ") + ")"
		}
		if p.Features == nil {
			p.Features = map[string]bool{}
		}
		for k, val := range submitted {
			p.Features[k] = truthy(val)
		}
	}
	// Model validations.
	if strings.TrimSpace(p.Name) == "" {
		return http.StatusUnprocessableEntity, "validation_failed", "Name can't be blank"
	}
	if !identifierFormat.MatchString(p.Identifier) {
		return http.StatusUnprocessableEntity, "validation_failed", "Identifier must be 1-10 uppercase letters/numbers"
	}
	for _, other := range s.projects().byID {
		if other.ID != p.ID && !other.Deleting && other.Identifier == p.Identifier {
			return http.StatusUnprocessableEntity, "validation_failed", "Identifier has already been taken"
		}
	}
	return 0, "", ""
}

func (s *Server) addProjectLocked(attrs map[string]any) *Project {
	now := time.Now()
	p := &Project{ID: s.id(), Emoji: "📁", Features: map[string]bool{}, LeadID: 1, Network: "public_project", CreatedAt: now, UpdatedAt: now}
	if status, _, _ := s.applyProjectAttrs(p, attrs); status != 0 {
		return nil
	}
	s.projects().byID[p.ID] = p
	for _, hook := range s.projectHooks {
		hook(s, p)
	}
	return p
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	attrs, ok := decodeBody(w, r, "project")
	if !ok {
		return
	}
	s.withIdempotency(w, r, "project", func() (int, any) {
		s.mu.Lock()
		defer s.mu.Unlock()
		now := time.Now()
		p := &Project{ID: s.id(), Emoji: "📁", Features: map[string]bool{}, LeadID: 1, Network: "public_project", CreatedAt: now, UpdatedAt: now}
		if status, code, msg := s.applyProjectAttrs(p, attrs); status != 0 {
			return status, map[string]any{"error": msg, "code": code}
		}
		s.projects().byID[p.ID] = p
		for _, hook := range s.projectHooks {
			hook(s, p)
		}
		return http.StatusCreated, s.serializeProject(p, true)
	})
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "project")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.liveProject(id)
	if p == nil {
		notFound(w)
		return
	}
	if !checkIfMatch(w, r, p.LockVersion) {
		return
	}
	candidate := *p
	if candidate.Features != nil {
		candidate.Features = map[string]bool{}
		for k, v := range p.Features {
			candidate.Features[k] = v
		}
	}
	if status, code, msg := s.applyProjectAttrs(&candidate, attrs); status != 0 {
		writeError(w, status, code, msg)
		return
	}
	candidate.LockVersion++
	candidate.UpdatedAt = time.Now()
	*p = candidate
	writeJSON(w, http.StatusOK, s.serializeProject(p, true))
}

func (s *Server) destroyProject(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	p := s.liveProject(id)
	if p != nil {
		p.Deleting = true
	}
	s.mu.Unlock()
	if p == nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id": id, "status": "deleting",
		"message": "Project marked for deletion and hidden immediately; its rows are being torn down in the background. It is already unreachable through the API.",
	})
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
