package flightdecktest

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
)

var hexColor = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// canonicalColor stores colours the way a normalising model would: lower-case,
// six digits. Deliberately stricter than the current model (which stores what
// it is given) so the provider is proven tolerant of either behaviour.
func canonicalColor(c string) string {
	if !hexColor.MatchString(c) {
		return c
	}
	c = strings.ToLower(c)
	if len(c) == 4 {
		return "#" + strings.Repeat(string(c[1]), 2) + strings.Repeat(string(c[2]), 2) + strings.Repeat(string(c[3]), 2)
	}
	return c
}

var stateGroups = []string{"backlog", "unstarted", "started", "completed", "cancelled"}

// defaultStates mirrors Project::DEFAULT_STATES, seeded on every project create.
var defaultStates = []struct {
	name, group, color string
	def                bool
}{
	{"Backlog", "backlog", "#9aa0a6", true},
	{"To Do", "unstarted", "#3b6fe0", false},
	{"In Progress", "started", "#d99a2b", false},
	{"Done", "completed", "#3f9e6b", false},
	{"Cancelled", "cancelled", "#8a9197", false},
}

// State is the fake's stored workflow state.
type State struct {
	ID          int64
	ProjectID   int64
	Name        string
	Group       string
	Color       string
	Default     bool
	Position    int64
	LockVersion int64
	// InUse simulates has_many :work_items, dependent: :restrict_with_error.
	InUse bool
}

type stateStore struct{ byID map[int64]*State }

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["states"] = &stateStore{byID: map[int64]*State{}}
		s.projectHooks = append(s.projectHooks, seedDefaultStates)
		mux.HandleFunc("GET /api/v1/projects/{project_id}/states", s.listStates)
		mux.HandleFunc("POST /api/v1/projects/{project_id}/states", s.createState)
		mux.HandleFunc("GET /api/v1/states/{id}", s.showState)
		mux.HandleFunc("PATCH /api/v1/states/{id}", s.updateState)
		mux.HandleFunc("DELETE /api/v1/states/{id}", s.destroyState)
	})
}

func (s *Server) states() *stateStore {
	store, _ := s.stores["states"].(*stateStore)
	return store
}

func seedDefaultStates(s *Server, p *Project) {
	for i, d := range defaultStates {
		st := &State{ID: s.id(), ProjectID: p.ID, Name: d.name, Group: d.group, Color: d.color, Default: d.def, Position: int64(i)}
		s.states().byID[st.ID] = st
	}
}

// StatesOf returns a project's states in API order (group, position, id).
func (s *Server) StatesOf(projectID int64) []State {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []State
	for _, st := range s.projectStatesLocked(projectID) {
		out = append(out, *st)
	}
	return out
}

// RemoveOtherStates deletes every state of the project except keepID, so the
// survivor is the project's last state.
func (s *Server) RemoveOtherStates(projectID, keepID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, st := range s.states().byID {
		if st.ProjectID == projectID && id != keepID {
			delete(s.states().byID, id)
		}
	}
}

// AddState seeds a state directly (no HTTP).
func (s *Server) AddState(projectID int64, name, group string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &State{ID: s.id(), ProjectID: projectID, Name: name, Group: group, Color: "#9ca3af"}
	s.states().byID[st.ID] = st
	return st.ID
}

// MarkStateInUse simulates work items sitting in the state, which blocks delete.
func (s *Server) MarkStateInUse(id int64, inUse bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.states().byID[id]; st != nil {
		st.InUse = inUse
	}
}

// TouchState simulates an out-of-band edit that bumps lock_version.
func (s *Server) TouchState(id int64, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.states().byID[id]; st != nil {
		st.Name = name
		st.LockVersion++
	}
}

func (s *Server) projectStatesLocked(projectID int64) []*State {
	var out []*State
	for _, st := range s.states().byID {
		if st.ProjectID == projectID {
			out = append(out, st)
		}
	}
	groupRank := func(g string) int {
		for i, x := range stateGroups {
			if x == g {
				return i
			}
		}
		return 99
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if groupRank(a.Group) != groupRank(b.Group) {
			return groupRank(a.Group) < groupRank(b.Group)
		}
		if a.Position != b.Position {
			return a.Position < b.Position
		}
		return a.ID < b.ID
	})
	return out
}

// liveState resolves a state whose project is live (the API scopes every
// lookup to the token's workspace's non-deleting projects).
func (s *Server) liveState(id int64) *State {
	st := s.states().byID[id]
	if st == nil || s.liveProject(st.ProjectID) == nil {
		return nil
	}
	return st
}

func serializeState(st *State) map[string]any {
	return map[string]any{
		"id": st.ID, "name": st.Name, "group": st.Group, "color": st.Color,
		"default": st.Default, "position": st.Position, "project_id": st.ProjectID,
		"lock_version": st.LockVersion,
	}
}

func (s *Server) listStates(w http.ResponseWriter, r *http.Request) {
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
	var items []any
	for _, st := range s.projectStatesLocked(pid) {
		items = append(items, serializeState(st))
	}
	s.mu.Unlock()
	if items == nil {
		items = []any{}
	}
	writeCollection(w, r, items)
}

func (s *Server) showState(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	st := s.liveState(id)
	var body map[string]any
	if st != nil {
		body = serializeState(st)
	}
	s.mu.Unlock()
	if body == nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// applyStateAttrs mirrors the model: name required + unique per project, group
// a fixed enum, color a hex value. Returns (status, code, message); 0 = ok.
func (s *Server) applyStateAttrs(st *State, attrs map[string]any) (int, string, string) {
	if v, ok := attrs["name"]; ok {
		st.Name = asString(v)
	}
	if v, ok := attrs["group"]; ok {
		g := asString(v)
		if !contains(stateGroups, g) {
			return http.StatusUnprocessableEntity, "invalid_attribute",
				"'" + g + "' is not a valid group (backlog, unstarted, started, completed, cancelled)"
		}
		st.Group = g
	}
	if v, ok := attrs["color"]; ok && v != nil {
		st.Color = canonicalColor(asString(v))
	}
	if v, ok := attrs["default"]; ok {
		st.Default = truthy(v)
	}
	if v, ok := attrs["position"]; ok && v != nil {
		if p, isNum := asInt64(v); isNum {
			st.Position = p
		}
	}
	if strings.TrimSpace(st.Name) == "" {
		return http.StatusUnprocessableEntity, "validation_failed", "Name can't be blank"
	}
	if !hexColor.MatchString(st.Color) {
		return http.StatusUnprocessableEntity, "validation_failed", "Color is invalid"
	}
	for _, other := range s.states().byID {
		if other.ID != st.ID && other.ProjectID == st.ProjectID && other.Name == st.Name {
			return http.StatusUnprocessableEntity, "validation_failed", "Name has already been taken"
		}
	}
	return 0, "", ""
}

// ensureSingleDefault mirrors State#ensure_single_default.
func (s *Server) ensureSingleDefault(st *State) {
	if !st.Default {
		return
	}
	for _, other := range s.states().byID {
		if other.ID != st.ID && other.ProjectID == st.ProjectID && other.Default {
			other.Default = false
			other.LockVersion++
		}
	}
}

func (s *Server) createState(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "state")
	if !ok {
		return
	}
	s.withIdempotency(w, r, "state", func() (int, any) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.liveProject(pid) == nil {
			return http.StatusNotFound, map[string]any{"error": "Not found", "code": "not_found"}
		}
		st := &State{ID: s.id(), ProjectID: pid, Color: "#9ca3af", Group: "backlog"}
		// Append to the end of the group, as StatesController#create does.
		if status, code, msg := s.applyStateAttrs(st, attrs); status != 0 {
			return status, map[string]any{"error": msg, "code": code}
		}
		if _, explicit := attrs["position"]; !explicit {
			st.Position = 0
			for _, other := range s.states().byID {
				if other.ProjectID == pid && other.Group == st.Group && other.Position >= st.Position {
					st.Position = other.Position + 1
				}
			}
		}
		s.states().byID[st.ID] = st
		s.ensureSingleDefault(st)
		return http.StatusCreated, serializeState(st)
	})
}

func (s *Server) updateState(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "state")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.liveState(id)
	if st == nil {
		notFound(w)
		return
	}
	if !checkIfMatch(w, r, st.LockVersion) {
		return
	}
	candidate := *st
	if status, code, msg := s.applyStateAttrs(&candidate, attrs); status != 0 {
		writeError(w, status, code, msg)
		return
	}
	candidate.LockVersion++
	*st = candidate
	s.ensureSingleDefault(st)
	writeJSON(w, http.StatusOK, serializeState(st))
}

func (s *Server) destroyState(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.liveState(id)
	if st == nil {
		notFound(w)
		return
	}
	if !checkIfMatch(w, r, st.LockVersion) {
		return
	}
	// The three guards, in the API's order and with its codes.
	if len(s.projectStatesLocked(st.ProjectID)) == 1 {
		writeError(w, http.StatusUnprocessableEntity, "last_state",
			"A project must keep at least one state — this is the last one. Create another state before deleting it.")
		return
	}
	if st.Default {
		writeError(w, http.StatusUnprocessableEntity, "state_is_default",
			"This is the project's default state. Make another state the default (PATCH it with default: true) before deleting this one.")
		return
	}
	if st.InUse {
		writeError(w, http.StatusUnprocessableEntity, "state_in_use",
			"Cannot delete record because dependent work items exist. Move or delete the work items in this state first, or PATCH them to another state.")
		return
	}
	delete(s.states().byID, id)
	w.WriteHeader(http.StatusNoContent)
}
