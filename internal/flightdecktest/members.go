package flightdecktest

import (
	"net/http"
	"sort"
)

var builtinProjectRoles = []string{"guest", "member", "admin", "commenter"}

// ProjectMember is the fake's stored membership row. Role is the effective
// role key (custom key or built-in); BuiltinRole is the built-in enum.
type ProjectMember struct {
	ID          int64
	ProjectID   int64
	UserID      int64
	Role        string
	BuiltinRole string
	LockVersion int64
}

type memberStore struct {
	byID     map[int64]*ProjectMember
	roleKeys []string // custom permission-scheme role keys, assignable like built-ins
}

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["members"] = &memberStore{byID: map[int64]*ProjectMember{}}
		// The creator is written in as project admin on every create.
		s.projectHooks = append(s.projectHooks, func(s *Server, p *Project) {
			m := &ProjectMember{ID: s.id(), ProjectID: p.ID, UserID: 1, Role: "admin", BuiltinRole: "admin"}
			s.projectMembers().byID[m.ID] = m
		})
		mux.HandleFunc("GET /api/v1/projects/{project_id}/members", s.listMembers)
		mux.HandleFunc("POST /api/v1/projects/{project_id}/members", s.createMember)
		mux.HandleFunc("GET /api/v1/projects/{project_id}/members/{id}", s.showMember)
		mux.HandleFunc("PATCH /api/v1/projects/{project_id}/members/{id}", s.updateMember)
		mux.HandleFunc("DELETE /api/v1/projects/{project_id}/members/{id}", s.destroyMember)
	})
}

func (s *Server) projectMembers() *memberStore {
	store, _ := s.stores["members"].(*memberStore)
	return store
}

// AddRoleKey makes a custom permission-scheme role key assignable.
func (s *Server) AddRoleKey(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projectMembers().roleKeys = append(s.projectMembers().roleKeys, key)
}

// MembersOf returns a project's member rows ordered by id.
func (s *Server) MembersOf(projectID int64) []ProjectMember {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ProjectMember
	for _, m := range s.projectMembers().byID {
		if m.ProjectID == projectID {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// TouchMember simulates an out-of-band role change that bumps lock_version.
func (s *Server) TouchMember(membershipID int64, role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.projectMembers().byID[membershipID]; m != nil {
		m.Role = role
		m.BuiltinRole = role
		m.LockVersion++
	}
}

// RemoveMemberOutOfBand deletes a membership row the way the members UI would.
func (s *Server) RemoveMemberOutOfBand(membershipID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.projectMembers().byID, membershipID)
}

func (s *Server) memberForUserLocked(projectID, userID int64) *ProjectMember {
	for _, m := range s.projectMembers().byID {
		if m.ProjectID == projectID && m.UserID == userID {
			return m
		}
	}
	return nil
}

// liveMember resolves a membership row within a live project: find(:id)
// scoped to @project.project_members.
func (s *Server) liveMember(projectID, id int64) *ProjectMember {
	m := s.projectMembers().byID[id]
	if m == nil || m.ProjectID != projectID || s.liveProject(projectID) == nil {
		return nil
	}
	return m
}

func (s *Server) workspaceUser(userID int64) *User {
	for i := range s.members {
		if s.members[i].ID == userID {
			return &s.members[i]
		}
	}
	return nil
}

// applyRole resolves a role the way the API does: a built-in, else a custom key.
func (s *Server) applyRole(m *ProjectMember, role string) bool {
	if contains(builtinProjectRoles, role) {
		m.Role, m.BuiltinRole = role, role
		return true
	}
	if contains(s.projectMembers().roleKeys, role) {
		m.Role = role
		if m.BuiltinRole == "" {
			m.BuiltinRole = "member"
		}
		return true
	}
	return false
}

func serializeMember(m *ProjectMember) map[string]any {
	return map[string]any{
		"id": m.ID, "project_id": m.ProjectID, "user_id": m.UserID, "role": m.Role,
		"builtin_role": m.BuiltinRole, "lock_version": m.LockVersion,
	}
}

// Every member action is :administer_project, reads included; the fake's
// token owner administers every project it created.
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
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	items := make([]any, 0, len(rows))
	for _, m := range rows {
		items = append(items, serializeMember(m))
	}
	s.mu.Unlock()
	writeCollection(w, r, items)
}

func (s *Server) showMember(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.liveMember(pid, id)
	if m == nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, serializeMember(m))
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
	s.withIdempotency(w, r, "project_member", func() (int, any) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.liveProject(pid) == nil {
			return http.StatusNotFound, errorBody("Not found", "not_found")
		}
		userID, isNum := asInt64(attrs["user_id"])
		if !isNum || s.workspaceUser(userID) == nil {
			// A user outside the workspace is indistinguishable from a bad id.
			return http.StatusNotFound, errorBody("Not found", "not_found")
		}
		m := &ProjectMember{ID: s.id(), ProjectID: pid, UserID: userID, Role: "member", BuiltinRole: "member"}
		if v, has := attrs["role"]; has {
			if !s.applyRole(m, asString(v)) {
				return http.StatusUnprocessableEntity, map[string]any{
					"error": "unknown role: " + asString(v), "code": "invalid_attribute"}
			}
		}
		if s.memberForUserLocked(pid, userID) != nil {
			return http.StatusUnprocessableEntity, map[string]any{
				"error": "User has already been taken", "code": "validation_failed"}
		}
		s.projectMembers().byID[m.ID] = m
		return http.StatusCreated, serializeMember(m)
	})
}

func (s *Server) updateMember(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "member")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.liveMember(pid, id)
	if m == nil {
		notFound(w)
		return
	}
	if !checkIfMatch(w, r, m.LockVersion) {
		return
	}
	if v, has := attrs["user_id"]; has {
		if requested, _ := asInt64(v); requested != m.UserID {
			writeError(w, http.StatusUnprocessableEntity, "invalid_attribute",
				"user_id cannot be changed on an existing membership — DELETE it and POST a new one")
			return
		}
	}
	if v, has := attrs["role"]; has {
		if !s.applyRole(m, asString(v)) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_attribute", "unknown role: "+asString(v))
			return
		}
	}
	m.LockVersion++
	writeJSON(w, http.StatusOK, serializeMember(m))
}

func (s *Server) destroyMember(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.liveMember(pid, id)
	if m == nil {
		notFound(w)
		return
	}
	if !checkIfMatch(w, r, m.LockVersion) {
		return
	}
	delete(s.projectMembers().byID, m.ID)
	w.WriteHeader(http.StatusNoContent)
}
