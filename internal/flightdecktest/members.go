package flightdecktest

import (
	"net/http"
	"sort"
)

var builtinProjectRoles = []string{"guest", "member", "admin", "commenter"}

// ProjectMember is the fake's stored membership row.
type ProjectMember struct {
	ID          int64
	ProjectID   int64
	UserID      int64
	Role        string
	LockVersion int64
}

type memberStore struct {
	byID     map[int64]*ProjectMember
	roleKeys []string // custom permission-scheme role keys, assignable like built-ins
	// requirePrecondition makes PATCH demand an If-Match (428 without one)
	// while the serializer omits lock_version — the API/provider mismatch the
	// member resource must diagnose clearly.
	requirePrecondition bool
}

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["members"] = &memberStore{byID: map[int64]*ProjectMember{}}
		// The creator is written in as project admin on every create.
		s.projectHooks = append(s.projectHooks, func(s *Server, p *Project) {
			m := &ProjectMember{ID: s.id(), ProjectID: p.ID, UserID: 1, Role: "admin"}
			s.projectMembers().byID[m.ID] = m
		})
		mux.HandleFunc("GET /api/v1/projects/{project_id}/members", s.listMembers)
		mux.HandleFunc("POST /api/v1/projects/{project_id}/members", s.createMember)
		mux.HandleFunc("PATCH /api/v1/projects/{project_id}/members/{user_id}", s.updateMember)
		mux.HandleFunc("DELETE /api/v1/projects/{project_id}/members/{user_id}", s.destroyMember)
	})
}

func (s *Server) projectMembers() *memberStore {
	store, _ := s.stores["members"].(*memberStore)
	return store
}

// RequireMemberPrecondition makes member PATCHes demand If-Match (428 when
// absent) while omitting lock_version from member rows.
func (s *Server) RequireMemberPrecondition(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projectMembers().requirePrecondition = on
}

// AddRoleKey makes a custom role key assignable (FD-230 permission schemes).
func (s *Server) AddRoleKey(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projectMembers().roleKeys = append(s.projectMembers().roleKeys, key)
}

// MembersOf returns a project's member rows ordered by user id.
func (s *Server) MembersOf(projectID int64) []ProjectMember {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ProjectMember
	for _, m := range s.projectMembers().byID {
		if m.ProjectID == projectID {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out
}

// TouchMember simulates an out-of-band role change that bumps lock_version.
func (s *Server) TouchMember(projectID, userID int64, role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.memberLocked(projectID, userID); m != nil {
		m.Role = role
		m.LockVersion++
	}
}

func (s *Server) memberLocked(projectID, userID int64) *ProjectMember {
	for _, m := range s.projectMembers().byID {
		if m.ProjectID == projectID && m.UserID == userID {
			return m
		}
	}
	return nil
}

func (s *Server) workspaceUser(userID int64) *User {
	for i := range s.members {
		if s.members[i].ID == userID {
			return &s.members[i]
		}
	}
	return nil
}

func (s *Server) roleAssignable(role string) bool {
	return contains(builtinProjectRoles, role) || contains(s.projectMembers().roleKeys, role)
}

func (s *Server) serializeMember(m *ProjectMember) map[string]any {
	out := map[string]any{
		"id": m.ID, "project_id": m.ProjectID, "user_id": m.UserID, "role": m.Role, "lock_version": m.LockVersion,
	}
	if s.projectMembers().requirePrecondition {
		delete(out, "lock_version")
	}
	if u := s.workspaceUser(m.UserID); u != nil {
		out["name"] = u.Name
		out["email"] = u.Email
	}
	return out
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request) {
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
	var rows []*ProjectMember
	for _, m := range s.projectMembers().byID {
		if m.ProjectID == pid {
			rows = append(rows, m)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].UserID < rows[j].UserID })
	items := make([]any, 0, len(rows))
	for _, m := range rows {
		items = append(items, s.serializeMember(m))
	}
	s.mu.Unlock()
	writeCollection(w, r, items)
}

func (s *Server) createMember(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "member")
	if !ok {
		return
	}
	s.withIdempotency(w, r, "member", func() (int, any) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.liveProject(pid) == nil {
			return http.StatusNotFound, map[string]any{"error": "Not found", "code": "not_found"}
		}
		userID, isNum := asInt64(attrs["user_id"])
		if !isNum || s.workspaceUser(userID) == nil {
			// A user outside the workspace is indistinguishable from a bad id.
			return http.StatusNotFound, map[string]any{"error": "Not found", "code": "not_found"}
		}
		role := asString(attrs["role"])
		if !s.roleAssignable(role) {
			return http.StatusUnprocessableEntity, map[string]any{
				"error": "'" + role + "' is not an assignable role", "code": "invalid_attribute"}
		}
		if s.memberLocked(pid, userID) != nil {
			return http.StatusUnprocessableEntity, map[string]any{
				"error": "User has already been taken", "code": "validation_failed"}
		}
		m := &ProjectMember{ID: s.id(), ProjectID: pid, UserID: userID, Role: role}
		s.projectMembers().byID[m.ID] = m
		return http.StatusCreated, s.serializeMember(m)
	})
}

func (s *Server) updateMember(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	uid, ok := pathID(w, r, "user_id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "member")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.liveProject(pid) == nil {
		notFound(w)
		return
	}
	m := s.memberLocked(pid, uid)
	if m == nil {
		notFound(w)
		return
	}
	if s.projectMembers().requirePrecondition && r.Header.Get("If-Match") == "" {
		writeError(w, http.StatusPreconditionRequired, "precondition_required", "If-Match header is required")
		return
	}
	if !checkIfMatch(w, r, m.LockVersion) {
		return
	}
	if v, has := attrs["role"]; has {
		role := asString(v)
		if !s.roleAssignable(role) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_attribute", "'"+role+"' is not an assignable role")
			return
		}
		m.Role = role
	}
	m.LockVersion++
	writeJSON(w, http.StatusOK, s.serializeMember(m))
}

func (s *Server) destroyMember(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	uid, ok := pathID(w, r, "user_id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.liveProject(pid) == nil {
		notFound(w)
		return
	}
	m := s.memberLocked(pid, uid)
	if m == nil {
		notFound(w)
		return
	}
	delete(s.projectMembers().byID, m.ID)
	w.WriteHeader(http.StatusNoContent)
}
