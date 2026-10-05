package flightdecktest

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Teamspace is the fake's stored team. Description and LeadID are nil when
// unset. Names are not unique.
type Teamspace struct {
	ID          int64
	Name        string
	Description *string
	LeadID      *int64
	LockVersion int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ProjectAccess is what the token's user may do with one project, as far as
// the teamspace routes are concerned. Linking or unlinking a project needs
// both read and administer on it.
type ProjectAccess int

const (
	// ProjectAccessAdmin reads and administers the project (the default).
	ProjectAccessAdmin ProjectAccess = iota
	// ProjectAccessHidden cannot see the project at all: it answers as if
	// it did not exist (a 404, or the 422 a made-up id gets).
	ProjectAccessHidden
	// ProjectAccessReadOnly reads the project but may not administer it.
	ProjectAccessReadOnly
	// ProjectAccessAdminNoRead administers the project but may not read it,
	// which a custom permission scheme can grant.
	ProjectAccessAdminNoRead
)

func (a ProjectAccess) visible() bool  { return a != ProjectAccessHidden }
func (a ProjectAccess) readable() bool { return a == ProjectAccessAdmin || a == ProjectAccessReadOnly }

// missingLinkCapability is the first capability the link rule finds missing,
// in the API's order (read, then administer), or "" when it allows the link.
func (a ProjectAccess) missingLinkCapability() string {
	switch a {
	case ProjectAccessHidden, ProjectAccessAdminNoRead:
		return "react"
	case ProjectAccessReadOnly:
		return "administer_project"
	}
	return ""
}

type teamspaceLink struct{ createdAt time.Time }

type teamspaceStore struct {
	byID map[int64]*Teamspace
	// members[teamspace][user] and projects[teamspace][project].
	members  map[int64]map[int64]teamspaceLink
	projects map[int64]map[int64]teamspaceLink
	access   map[int64]ProjectAccess
	// guest makes the token's user a workspace guest on these routes: it may
	// read, and every write is refused.
	guest bool
}

const (
	guestRefusal      = "Workspace guests can't do this. Ask a workspace admin to make you a member."
	capabilityRefusal = "Your project role does not permit this action (missing capability: %s)."
)

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["teamspaces"] = &teamspaceStore{
			byID: map[int64]*Teamspace{}, members: map[int64]map[int64]teamspaceLink{},
			projects: map[int64]map[int64]teamspaceLink{}, access: map[int64]ProjectAccess{},
		}
		mux.HandleFunc("GET /api/v1/teamspaces", s.listTeamspaces)
		mux.HandleFunc("POST /api/v1/teamspaces", s.createTeamspace)
		mux.HandleFunc("GET /api/v1/teamspaces/{id}", s.showTeamspace)
		mux.HandleFunc("PATCH /api/v1/teamspaces/{id}", s.updateTeamspace)
		mux.HandleFunc("DELETE /api/v1/teamspaces/{id}", s.destroyTeamspace)
		mux.HandleFunc("GET /api/v1/teamspaces/{id}/members", s.listTeamspaceMembers)
		mux.HandleFunc("POST /api/v1/teamspaces/{id}/members", s.addTeamspaceMember)
		mux.HandleFunc("GET /api/v1/teamspaces/{id}/members/{user_id}", s.showTeamspaceMember)
		mux.HandleFunc("DELETE /api/v1/teamspaces/{id}/members/{user_id}", s.removeTeamspaceMember)
		mux.HandleFunc("GET /api/v1/teamspaces/{id}/projects", s.listTeamspaceProjects)
		mux.HandleFunc("POST /api/v1/teamspaces/{id}/projects", s.linkTeamspaceProject)
		mux.HandleFunc("GET /api/v1/teamspaces/{id}/projects/{project_id}", s.showTeamspaceProject)
		mux.HandleFunc("DELETE /api/v1/teamspaces/{id}/projects/{project_id}", s.unlinkTeamspaceProject)
	})
}

func (s *Server) teamspaces() *teamspaceStore {
	store, _ := s.stores["teamspaces"].(*teamspaceStore)
	return store
}

// SetWorkspaceGuest makes the token's user a workspace guest on the teamspace
// routes: reads work, and creating, editing or deleting a team, or changing
// its members or projects, is a 403.
func (s *Server) SetWorkspaceGuest(guest bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.teamspaces().guest = guest
}

// SetProjectAccess sets what the token's user may do with a project on the
// teamspace routes.
func (s *Server) SetProjectAccess(projectID int64, access ProjectAccess) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.teamspaces().access[projectID] = access
}

// AddTeamspace creates a team directly, the way the web form would.
func (s *Server) AddTeamspace(name string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	t := &Teamspace{ID: s.id(), Name: name, CreatedAt: now, UpdatedAt: now}
	s.teamspaces().byID[t.ID] = t
	return t.ID
}

// Teamspace returns a copy of a stored team, or nil.
func (s *Server) Teamspace(id int64) *Teamspace {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.teamspaces().byID[id]; t != nil {
		out := *t
		return &out
	}
	return nil
}

// EditTeamspaceOutOfBand changes a team the way the web form would, bumping
// lock_version.
func (s *Server) EditTeamspaceOutOfBand(id int64, edit func(*Teamspace)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.teamspaces().byID[id]; t != nil {
		edit(t)
		t.LockVersion++
		t.UpdatedAt = time.Now()
	}
}

// DeleteTeamspaceOutOfBand deletes a team and its links, as the web would.
func (s *Server) DeleteTeamspaceOutOfBand(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.teamspaces()
	delete(st.byID, id)
	delete(st.members, id)
	delete(st.projects, id)
}

// TeamspaceMemberIDs returns the user ids on a team, sorted.
func (s *Server) TeamspaceMemberIDs(teamspaceID int64) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sortedKeys(s.teamspaces().members[teamspaceID])
}

// TeamspaceProjectIDs returns the project ids a team owns, sorted, whatever
// the token may see.
func (s *Server) TeamspaceProjectIDs(teamspaceID int64) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sortedKeys(s.teamspaces().projects[teamspaceID])
}

// SetTeamspaceMemberOutOfBand adds (on) or removes a member the way the web would.
func (s *Server) SetTeamspaceMemberOutOfBand(teamspaceID, userID int64, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	setLink(s.teamspaces().members, teamspaceID, userID, on)
}

// SetTeamspaceProjectOutOfBand links (on) or unlinks a project the way the
// web would, without asking the link rule.
func (s *Server) SetTeamspaceProjectOutOfBand(teamspaceID, projectID int64, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	setLink(s.teamspaces().projects, teamspaceID, projectID, on)
}

func setLink(links map[int64]map[int64]teamspaceLink, teamspaceID, otherID int64, on bool) {
	if !on {
		delete(links[teamspaceID], otherID)
		return
	}
	if links[teamspaceID] == nil {
		links[teamspaceID] = map[int64]teamspaceLink{}
	}
	if _, ok := links[teamspaceID][otherID]; !ok {
		links[teamspaceID][otherID] = teamspaceLink{createdAt: time.Now()}
	}
}

func sortedKeys(m map[int64]teamspaceLink) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func serializeTeamspace(t *Teamspace) map[string]any {
	return map[string]any{
		"id": t.ID, "name": t.Name, "description": t.Description, "lead_id": t.LeadID,
		"lock_version": t.LockVersion, "created_at": iso(t.CreatedAt), "updated_at": iso(t.UpdatedAt),
	}
}

// projectAccess is the token's access to a project. Called with s.mu held.
func (s *Server) projectAccess(projectID int64) ProjectAccess {
	return s.teamspaces().access[projectID]
}

// visibleProject is a project the token can see and that is not being
// deleted, or nil. Called with s.mu held.
func (s *Server) visibleProject(projectID int64) *Project {
	p := s.liveProject(projectID)
	if p == nil || !s.projectAccess(projectID).visible() {
		return nil
	}
	return p
}

// teamspaceFromPath resolves the {id} segment; anything else is a 404, asked
// before any other check. Called with s.mu held.
func (s *Server) teamspaceFromPath(w http.ResponseWriter, r *http.Request) *Teamspace {
	id, ok := pathID(w, r, "id")
	if !ok {
		return nil
	}
	t := s.teamspaces().byID[id]
	if t == nil {
		notFound(w)
	}
	return t
}

// refuseGuest answers the workspace guest floor. Called with s.mu held.
func (s *Server) refuseGuest(w http.ResponseWriter) bool {
	if s.teamspaces().guest {
		writeError(w, http.StatusForbidden, "forbidden", guestRefusal)
		return true
	}
	return false
}

func (s *Server) isWorkspaceUser(id int64) bool {
	for _, m := range s.members {
		if m.ID == id {
			return true
		}
	}
	return false
}

// decodeTeamspaceBody is decodeBody, plus the API's reading of an empty root
// object as a missing one: a 400, before anything else looks at it.
func decodeTeamspaceBody(w http.ResponseWriter, r *http.Request, root string) (map[string]any, bool) {
	attrs, ok := decodeBody(w, r, root)
	if ok && len(attrs) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "param is missing or the value is empty or invalid: "+root)
		return nil, false
	}
	return attrs, ok
}

// --- teamspaces ----------------------------------------------------------------

func (s *Server) listTeamspaces(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for key := range q {
		switch {
		case key == "name[]" || len(q["name"]) > 1:
			writeError(w, http.StatusUnprocessableEntity, "invalid_attribute", "name must be a string, got a list")
			return
		case strings.HasPrefix(key, "name["):
			writeError(w, http.StatusUnprocessableEntity, "invalid_attribute", "name must be a string, got an object")
			return
		}
	}
	name := q.Get("name")
	filtered := strings.Trim(name, apiBlank) != ""
	if filtered && utf8.RuneCountInString(strings.Trim(name, apiBlank)) > 255 {
		writeError(w, http.StatusUnprocessableEntity, "invalid_attribute", "name must be 255 characters or fewer")
		return
	}
	s.mu.Lock()
	var rows []*Teamspace
	for _, t := range s.teamspaces().byID {
		if !filtered || sqlLowerEqual(t.Name, name) {
			rows = append(rows, t)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].ID < rows[j].ID
	})
	items := make([]any, 0, len(rows))
	for _, t := range rows {
		items = append(items, serializeTeamspace(t))
	}
	s.mu.Unlock()
	writeCollection(w, r, items)
}

func (s *Server) showTeamspace(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.teamspaceFromPath(w, r); t != nil {
		writeJSON(w, http.StatusOK, serializeTeamspace(t))
	}
}

// sqlLowerEqual is the API's `LOWER(name) = LOWER(?)`: equal once both are
// lowercased, which a dotted capital I passes and Unicode case folding does
// not, or under case folding.
func sqlLowerEqual(a, b string) bool {
	lowerA, lowerB := strings.ToLower(a), strings.ToLower(b)
	return lowerA == lowerB || strings.EqualFold(a, b)
}

// applyTeamspaceAttrs mirrors the API's applier: read-only keys and unknown
// keys are refused by name; a blank name is no opinion; a blank description
// and a blank or null lead clear them; a lead must be a workspace member.
// Called with s.mu held.
func (s *Server) applyTeamspaceAttrs(t *Teamspace, attrs map[string]any) (int, string, string) {
	for _, key := range []string{"id", "created_at", "updated_at"} {
		if _, has := attrs[key]; has {
			return http.StatusUnprocessableEntity, "invalid_attribute", key + " is read-only over the API"
		}
	}
	for key := range attrs {
		if key != "name" && key != "description" && key != "lead_id" && key != "lock_version" {
			return http.StatusUnprocessableEntity, "invalid_attribute",
				"unknown key: " + key + " (settable: name, description, lead_id)"
		}
	}
	if v, has := attrs["name"]; has && v != nil {
		str, isStr := v.(string)
		if !isStr {
			return http.StatusUnprocessableEntity, "invalid_attribute", fmt.Sprintf("name must be a string, got %T", v)
		}
		if strings.Trim(str, apiBlank) != "" {
			t.Name = str
		}
	}
	if v, has := attrs["description"]; has {
		switch d := v.(type) {
		case nil:
			t.Description = nil
		case string:
			if strings.Trim(d, apiBlank) == "" {
				t.Description = nil
			} else {
				t.Description = &d
			}
		default:
			return http.StatusUnprocessableEntity, "invalid_attribute", fmt.Sprintf("description must be a string, got %T", v)
		}
	}
	if v, has := attrs["lead_id"]; has {
		if blankValue(v) {
			t.LeadID = nil
		} else {
			id, refusal := idParam("lead_id", v)
			if refusal != "" {
				return http.StatusUnprocessableEntity, "invalid_attribute", refusal
			}
			t.LeadID = &id
		}
	}
	if strings.TrimSpace(t.Name) == "" {
		return http.StatusUnprocessableEntity, "validation_failed", "Name can't be blank"
	}
	if t.LeadID != nil && !s.isWorkspaceUser(*t.LeadID) {
		return http.StatusUnprocessableEntity, "validation_failed", "Lead must be a member of the workspace"
	}
	return 0, "", ""
}

func sameTeamspace(a, b *Teamspace) bool {
	return a.Name == b.Name && equalPtr(a.Description, b.Description) && equalPtr(a.LeadID, b.LeadID)
}

func equalPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (s *Server) createTeamspace(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	refused := s.refuseGuest(w)
	s.mu.Unlock()
	if refused {
		return
	}
	attrs, ok := decodeTeamspaceBody(w, r, "teamspace")
	if !ok {
		return
	}
	// Fingerprinted, like the API: the same key with a different body is a
	// 409 idempotency_key_reused, and nothing is created. lock_version, which
	// a create never reads, is left out of the fingerprint.
	fingerprinted := map[string]any{}
	for k, v := range attrs {
		if k != "lock_version" {
			fingerprinted[k] = v
		}
	}
	s.withIdempotencyFingerprint(w, r, "teamspace", fingerprintOf(fingerprinted), func() (int, any) {
		s.mu.Lock()
		defer s.mu.Unlock()
		now := time.Now()
		t := &Teamspace{ID: s.id(), CreatedAt: now, UpdatedAt: now}
		if status, code, msg := s.applyTeamspaceAttrs(t, attrs); status != 0 {
			return status, errorBody(msg, code)
		}
		s.teamspaces().byID[t.ID] = t
		return http.StatusCreated, serializeTeamspace(t)
	})
}

func (s *Server) updateTeamspace(w http.ResponseWriter, r *http.Request) {
	attrs, ok := decodeTeamspaceBody(w, r, "teamspace")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.teamspaceFromPath(w, r)
	if t == nil || s.refuseGuest(w) || !checkIfMatch(w, r, t.LockVersion) {
		return
	}
	candidate := *t
	if status, code, msg := s.applyTeamspaceAttrs(&candidate, attrs); status != 0 {
		writeError(w, status, code, msg)
		return
	}
	// Like the API, a write that changes nothing leaves lock_version and
	// updated_at alone.
	if !sameTeamspace(&candidate, t) {
		candidate.LockVersion++
		candidate.UpdatedAt = time.Now()
	}
	*t = candidate
	writeJSON(w, http.StatusOK, serializeTeamspace(t))
}

// destroyTeamspace removes the team and its links. Deleting a team unlinks
// every project it owns that is not being deleted, so the link rule is asked
// of each one first, and the refusal gives a count, never names.
func (s *Server) destroyTeamspace(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.teamspaceFromPath(w, r)
	if t == nil || s.refuseGuest(w) {
		return
	}
	blocking := 0
	for pid := range s.teamspaces().projects[t.ID] {
		if s.liveProject(pid) != nil && s.projectAccess(pid).missingLinkCapability() != "" {
			blocking++
		}
	}
	if blocking > 0 {
		noun, pronoun := "projects", "them"
		if blocking == 1 {
			noun, pronoun = "project", "it"
		}
		writeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf(
			"This teamspace owns %d %s that you cannot unlink, and deleting the teamspace would unlink %s. A workspace "+
				"owner or admin, or someone who administers every project the teamspace owns, can delete it.",
			blocking, noun, pronoun))
		return
	}
	if !checkIfMatch(w, r, t.LockVersion) {
		return
	}
	st := s.teamspaces()
	delete(st.byID, t.ID)
	delete(st.members, t.ID)
	delete(st.projects, t.ID)
	w.WriteHeader(http.StatusNoContent)
}

// --- members --------------------------------------------------------------------

func serializeTeamspaceLink(teamspaceID int64, key string, otherID int64, l teamspaceLink) map[string]any {
	return map[string]any{"teamspace_id": teamspaceID, key: otherID, "created_at": iso(l.createdAt)}
}

func (s *Server) listTeamspaceMembers(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	t := s.teamspaceFromPath(w, r)
	if t == nil {
		s.mu.Unlock()
		return
	}
	links := s.teamspaces().members[t.ID]
	items := make([]any, 0, len(links))
	for _, uid := range sortedKeys(links) {
		items = append(items, serializeTeamspaceLink(t.ID, "user_id", uid, links[uid]))
	}
	s.mu.Unlock()
	writeCollection(w, r, items)
}

func (s *Server) showTeamspaceMember(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.teamspaceFromPath(w, r)
	if t == nil {
		return
	}
	uid, ok := pathID(w, r, "user_id")
	if !ok {
		return
	}
	link, ok := s.teamspaces().members[t.ID][uid]
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, serializeTeamspaceLink(t.ID, "user_id", uid, link))
}

// addTeamspaceMember answers 201 for a new member and 200 carrying the row
// for one already on the team; a user outside the workspace is a 422.
func (s *Server) addTeamspaceMember(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	t := s.teamspaceFromPath(w, r)
	refused := t == nil || s.refuseGuest(w)
	s.mu.Unlock()
	if refused {
		return
	}
	attrs, ok := decodeTeamspaceBody(w, r, "member")
	if !ok {
		return
	}
	uid, status, msg := linkTargetID(attrs, "user_id")
	if status != 0 {
		writeError(w, status, "invalid_attribute", msg)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.teamspaces().byID[t.ID] == nil {
		// Deleted between the lookup and the write: the API's lost race.
		writeError(w, http.StatusConflict, "stale_object", LostRaceWriteMessage)
		return
	}
	if !s.isWorkspaceUser(uid) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_attribute",
			fmt.Sprintf("user_id %d is not a member of this workspace; only workspace members can join a teamspace", uid))
		return
	}
	links := s.teamspaces().members
	status = http.StatusOK
	if _, exists := links[t.ID][uid]; !exists {
		setLink(links, t.ID, uid, true)
		status = http.StatusCreated
	}
	writeJSON(w, status, serializeTeamspaceLink(t.ID, "user_id", uid, links[t.ID][uid]))
}

func (s *Server) removeTeamspaceMember(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.teamspaceFromPath(w, r)
	if t == nil || s.refuseGuest(w) {
		return
	}
	uid, ok := pathID(w, r, "user_id")
	if !ok {
		return
	}
	if _, exists := s.teamspaces().members[t.ID][uid]; !exists {
		notFound(w)
		return
	}
	setLink(s.teamspaces().members, t.ID, uid, false)
	w.WriteHeader(http.StatusNoContent)
}

// linkTargetID reads the one key a link create takes, the way the API's
// resolver does: required, and an integer id.
func linkTargetID(attrs map[string]any, key string) (int64, int, string) {
	for k := range attrs {
		if k == "teamspace_id" || k == "created_at" {
			return 0, http.StatusUnprocessableEntity, k + " is read-only over the API"
		}
		if k != key {
			return 0, http.StatusUnprocessableEntity, fmt.Sprintf("unknown key: %s (settable: %s)", k, key)
		}
	}
	v, has := attrs[key]
	if !has || blankValue(v) {
		return 0, http.StatusUnprocessableEntity, key + " is required"
	}
	id, refusal := idParam(key, v)
	if refusal != "" {
		return 0, http.StatusUnprocessableEntity, refusal
	}
	return id, 0, ""
}

// --- projects -------------------------------------------------------------------

// listTeamspaceProjects carries only links whose project the token may read
// and that is not being deleted.
func (s *Server) listTeamspaceProjects(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	t := s.teamspaceFromPath(w, r)
	if t == nil {
		s.mu.Unlock()
		return
	}
	links := s.teamspaces().projects[t.ID]
	items := make([]any, 0, len(links))
	for _, pid := range sortedKeys(links) {
		if s.liveProject(pid) != nil && s.projectAccess(pid).readable() {
			items = append(items, serializeTeamspaceLink(t.ID, "project_id", pid, links[pid]))
		}
	}
	s.mu.Unlock()
	writeCollection(w, r, items)
}

// visibleLink finds a link among the projects the token can see: a link to a
// project it cannot see, or one being deleted, is the 404 a missing link is.
// Called with s.mu held.
func (s *Server) visibleLink(w http.ResponseWriter, r *http.Request, t *Teamspace) (int64, teamspaceLink, bool) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return 0, teamspaceLink{}, false
	}
	link, linked := s.teamspaces().projects[t.ID][pid]
	if !linked || s.visibleProject(pid) == nil {
		notFound(w)
		return 0, teamspaceLink{}, false
	}
	return pid, link, true
}

func (s *Server) showTeamspaceProject(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.teamspaceFromPath(w, r)
	if t == nil {
		return
	}
	pid, link, ok := s.visibleLink(w, r, t)
	if !ok {
		return
	}
	if !s.projectAccess(pid).readable() {
		writeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf(capabilityRefusal, "react"))
		return
	}
	writeJSON(w, http.StatusOK, serializeTeamspaceLink(t.ID, "project_id", pid, link))
}

// linkTeamspaceProject: the 404 for the team, the guest floor, the 422 for a
// project the token cannot see (worded as for an id that names nothing), the
// link rule's 403, then 201 or 200.
func (s *Server) linkTeamspaceProject(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	t := s.teamspaceFromPath(w, r)
	refused := t == nil || s.refuseGuest(w)
	s.mu.Unlock()
	if refused {
		return
	}
	attrs, ok := decodeTeamspaceBody(w, r, "project")
	if !ok {
		return
	}
	pid, status, msg := linkTargetID(attrs, "project_id")
	if status != 0 {
		writeError(w, status, "invalid_attribute", msg)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.visibleProject(pid) == nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_attribute",
			fmt.Sprintf("project_id %d is not a project in this workspace, or it is being deleted", pid))
		return
	}
	if missing := s.projectAccess(pid).missingLinkCapability(); missing != "" {
		writeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf(capabilityRefusal, missing))
		return
	}
	if s.teamspaces().byID[t.ID] == nil {
		writeError(w, http.StatusConflict, "stale_object", LostRaceWriteMessage)
		return
	}
	links := s.teamspaces().projects
	status = http.StatusOK
	if _, exists := links[t.ID][pid]; !exists {
		setLink(links, t.ID, pid, true)
		status = http.StatusCreated
	}
	writeJSON(w, status, serializeTeamspaceLink(t.ID, "project_id", pid, links[t.ID][pid]))
}

// unlinkTeamspaceProject finds the link among visible projects first, so one
// the token cannot see is a 404 even for a guest; then the guest floor and
// the link rule.
func (s *Server) unlinkTeamspaceProject(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.teamspaceFromPath(w, r)
	if t == nil {
		return
	}
	pid, _, ok := s.visibleLink(w, r, t)
	if !ok || s.refuseGuest(w) {
		return
	}
	if missing := s.projectAccess(pid).missingLinkCapability(); missing != "" {
		writeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf(capabilityRefusal, missing))
		return
	}
	setLink(s.teamspaces().projects, t.ID, pid, false)
	w.WriteHeader(http.StatusNoContent)
}
