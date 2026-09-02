package flightdecktest

import (
	"net/http"
	"sort"
	"strings"
)

// defaultLabels mirrors Project::DEFAULT_LABELS, seeded on every project create.
var defaultLabels = []struct{ name, color string }{
	{"Bug", "#ef5974"}, {"Feature", "#3b82f6"}, {"Enhancement", "#14b8a6"},
	{"Documentation", "#8b5cf6"}, {"Tech debt", "#f5b82e"},
}

// Label is the fake's stored label.
type Label struct {
	ID          int64
	ProjectID   int64
	Name        string
	Color       string
	LockVersion int64
}

type labelStore struct{ byID map[int64]*Label }

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["labels"] = &labelStore{byID: map[int64]*Label{}}
		s.projectHooks = append(s.projectHooks, seedDefaultLabels)
		mux.HandleFunc("GET /api/v1/projects/{project_id}/labels", s.listLabels)
		mux.HandleFunc("POST /api/v1/projects/{project_id}/labels", s.createLabel)
		mux.HandleFunc("GET /api/v1/labels/{id}", s.showLabel)
		mux.HandleFunc("PATCH /api/v1/labels/{id}", s.updateLabel)
		mux.HandleFunc("DELETE /api/v1/labels/{id}", s.destroyLabel)
	})
}

func (s *Server) labels() *labelStore {
	store, _ := s.stores["labels"].(*labelStore)
	return store
}

func seedDefaultLabels(s *Server, p *Project) {
	for _, d := range defaultLabels {
		l := &Label{ID: s.id(), ProjectID: p.ID, Name: d.name, Color: d.color}
		s.labels().byID[l.ID] = l
	}
}

// LabelsOf returns a project's labels ordered by id.
func (s *Server) LabelsOf(projectID int64) []Label {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Label
	for _, l := range s.labels().byID {
		if l.ProjectID == projectID {
			out = append(out, *l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Server) liveLabel(id int64) *Label {
	l := s.labels().byID[id]
	if l == nil || s.liveProject(l.ProjectID) == nil {
		return nil
	}
	return l
}

func serializeLabel(l *Label) map[string]any {
	return map[string]any{
		"id": l.ID, "name": l.Name, "color": l.Color, "project_id": l.ProjectID, "lock_version": l.LockVersion,
	}
}

func (s *Server) listLabels(w http.ResponseWriter, r *http.Request) {
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
	var ordered []*Label
	for _, l := range s.labels().byID {
		if l.ProjectID == pid {
			ordered = append(ordered, l)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	items := make([]any, 0, len(ordered))
	for _, l := range ordered {
		items = append(items, serializeLabel(l))
	}
	s.mu.Unlock()
	writeCollection(w, r, items)
}

func (s *Server) showLabel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	l := s.liveLabel(id)
	var body map[string]any
	if l != nil {
		body = serializeLabel(l)
	}
	s.mu.Unlock()
	if body == nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) applyLabelAttrs(l *Label, attrs map[string]any) (int, string, string) {
	if v, ok := attrs["name"]; ok {
		l.Name = asString(v)
	}
	if v, ok := attrs["color"]; ok && v != nil {
		l.Color = asString(v)
	}
	if strings.TrimSpace(l.Name) == "" {
		return http.StatusUnprocessableEntity, "validation_failed", "Name can't be blank"
	}
	if l.Color != "" && !hexColor.MatchString(l.Color) {
		return http.StatusUnprocessableEntity, "validation_failed", "Color is invalid"
	}
	for _, other := range s.labels().byID {
		if other.ID != l.ID && other.ProjectID == l.ProjectID && other.Name == l.Name {
			return http.StatusUnprocessableEntity, "validation_failed", "Name has already been taken"
		}
	}
	return 0, "", ""
}

func (s *Server) createLabel(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "label")
	if !ok {
		return
	}
	s.withIdempotency(w, r, "label", func() (int, any) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.liveProject(pid) == nil {
			return http.StatusNotFound, map[string]any{"error": "Not found", "code": "not_found"}
		}
		l := &Label{ID: s.id(), ProjectID: pid, Color: "#6b7280"}
		if status, code, msg := s.applyLabelAttrs(l, attrs); status != 0 {
			return status, map[string]any{"error": msg, "code": code}
		}
		s.labels().byID[l.ID] = l
		return http.StatusCreated, serializeLabel(l)
	})
}

func (s *Server) updateLabel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "label")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.liveLabel(id)
	if l == nil {
		notFound(w)
		return
	}
	if !checkIfMatch(w, r, l.LockVersion) {
		return
	}
	candidate := *l
	if status, code, msg := s.applyLabelAttrs(&candidate, attrs); status != 0 {
		writeError(w, status, code, msg)
		return
	}
	candidate.LockVersion++
	*l = candidate
	writeJSON(w, http.StatusOK, serializeLabel(l))
}

func (s *Server) destroyLabel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.liveLabel(id) == nil {
		notFound(w)
		return
	}
	delete(s.labels().byID, id)
	w.WriteHeader(http.StatusNoContent)
}
