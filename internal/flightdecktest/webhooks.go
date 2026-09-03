package flightdecktest

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
)

// WebhookEvents are the events a webhook may subscribe to.
var WebhookEvents = []string{
	"work_item.created", "work_item.updated", "work_item.deleted",
	"work_item.state_changed", "work_item.assigned", "work_item.unassigned",
	"comment.created", "comment.updated", "comment.deleted",
	"cycle.created", "cycle.updated", "cycle.deleted",
	"module.created", "module.updated", "module.deleted",
	"intake.created", "intake.accepted", "intake.declined",
	"project.created", "project.updated",
}

// Webhook is the fake's stored webhook.
type Webhook struct {
	ID          int64
	ProjectID   *int64
	URL         string
	Events      []string
	Secret      string
	Active      bool
	LockVersion int64
}

type webhookStore struct{ byID map[int64]*Webhook }

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["webhooks"] = &webhookStore{byID: map[int64]*Webhook{}}
		mux.HandleFunc("GET /api/v1/webhooks", s.listWebhooks)
		mux.HandleFunc("POST /api/v1/webhooks", s.createWebhook)
		mux.HandleFunc("GET /api/v1/webhooks/{id}", s.showWebhook)
		mux.HandleFunc("PATCH /api/v1/webhooks/{id}", s.updateWebhook)
		mux.HandleFunc("DELETE /api/v1/webhooks/{id}", s.destroyWebhook)
	})
}

func (s *Server) webhooks() *webhookStore {
	store, _ := s.stores["webhooks"].(*webhookStore)
	return store
}

// Webhook returns a stored webhook or nil.
func (s *Server) Webhook(id int64) *Webhook {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.webhooks().byID[id]
	if w == nil {
		return nil
	}
	cp := *w
	return &cp
}

// TouchWebhook simulates an out-of-band edit that bumps lock_version.
func (s *Server) TouchWebhook(id int64, active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.webhooks().byID[id]; w != nil {
		w.Active = active
		w.LockVersion++
	}
}

// requireWorkspaceAdmin mirrors WebhooksController's require_workspace_admin.
func (s *Server) requireWorkspaceAdmin(w http.ResponseWriter) bool {
	if s.workspaceAdmin {
		return true
	}
	writeError(w, http.StatusForbidden, "forbidden", "Workspace admin role required to manage webhooks.")
	return false
}

func serializeWebhook(h *Webhook, withSecret bool) map[string]any {
	events := append([]string(nil), h.Events...)
	sort.Strings(events)
	out := map[string]any{
		"id": h.ID, "workspace_id": 1, "project_id": h.ProjectID, "url": h.URL, "events": events,
		"active": h.Active, "lock_version": h.LockVersion,
	}
	if withSecret {
		out["secret"] = h.Secret
	}
	return out
}

func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if !s.requireWorkspaceAdmin(w) {
		s.mu.Unlock()
		return
	}
	var rows []*Webhook
	for _, h := range s.webhooks().byID {
		rows = append(rows, h)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID > rows[j].ID })
	items := make([]any, 0, len(rows))
	for _, h := range rows {
		items = append(items, serializeWebhook(h, false))
	}
	s.mu.Unlock()
	writeCollection(w, r, items)
}

func (s *Server) showWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.requireWorkspaceAdmin(w) {
		return
	}
	h := s.webhooks().byID[id]
	if h == nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, serializeWebhook(h, false))
}

func (s *Server) applyWebhookAttrs(h *Webhook, attrs map[string]any) (int, string, string) {
	if v, has := attrs["url"]; has {
		h.URL = asString(v)
	}
	if v, has := attrs["events"]; has {
		list, isList := v.([]any)
		if !isList {
			return http.StatusUnprocessableEntity, "invalid_attribute", "events must be an array"
		}
		h.Events = nil
		for _, e := range list {
			h.Events = append(h.Events, asString(e))
		}
	}
	if v, has := attrs["active"]; has {
		h.Active = truthy(v)
	}
	if v, has := attrs["project_id"]; has {
		if v == nil {
			h.ProjectID = nil
		} else if pid, isNum := asInt64(v); isNum {
			if s.liveProject(pid) == nil {
				return http.StatusUnprocessableEntity, "validation_failed", "Project must belong to this workspace"
			}
			h.ProjectID = &pid
		}
	}
	if !httpURL.MatchString(h.URL) {
		return http.StatusUnprocessableEntity, "validation_failed", "Url must be an http(s) URL"
	}
	lower := strings.ToLower(h.URL)
	if strings.Contains(lower, "localhost") || strings.Contains(lower, "127.0.0.1") || strings.Contains(lower, ".internal") {
		return http.StatusUnprocessableEntity, "validation_failed", "Url must not point to an internal or private address"
	}
	var bad []string
	for _, e := range h.Events {
		if !contains(WebhookEvents, e) {
			bad = append(bad, e)
		}
	}
	if len(bad) > 0 {
		return http.StatusUnprocessableEntity, "validation_failed", "Events contains unknown events: " + strings.Join(bad, ", ")
	}
	return 0, "", ""
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) {
	attrs, ok := decodeBody(w, r, "webhook")
	if !ok {
		return
	}
	s.mu.Lock()
	if !s.requireWorkspaceAdmin(w) {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.withIdempotencyRedacted(w, r, "webhook", func() (int, any, any) {
		s.mu.Lock()
		defer s.mu.Unlock()
		h := &Webhook{ID: s.id(), Active: true}
		if status, code, msg := s.applyWebhookAttrs(h, attrs); status != 0 {
			return status, errorBody(msg, code), nil
		}
		// The signing secret is always generated server-side; a submitted
		// `secret` is ignored, as the real API ignores it.
		raw := make([]byte, 24)
		_, _ = rand.Read(raw)
		h.Secret = hex.EncodeToString(raw)
		s.webhooks().byID[h.ID] = h
		redacted := serializeWebhook(h, false)
		redacted["secret"] = nil
		redacted["secret_available"] = false
		redacted["message"] = "Replay of a previously-used Idempotency-Key. The signing secret is returned only by the original create."
		return http.StatusCreated, serializeWebhook(h, true), redacted
	})
}

func (s *Server) updateWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "webhook")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.requireWorkspaceAdmin(w) {
		return
	}
	h := s.webhooks().byID[id]
	if h == nil {
		notFound(w)
		return
	}
	if !checkIfMatch(w, r, h.LockVersion) {
		return
	}
	candidate := *h
	candidate.Events = append([]string(nil), h.Events...)
	if status, code, msg := s.applyWebhookAttrs(&candidate, attrs); status != 0 {
		writeError(w, status, code, msg)
		return
	}
	candidate.LockVersion++
	*h = candidate
	writeJSON(w, http.StatusOK, serializeWebhook(h, false))
}

func (s *Server) destroyWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.requireWorkspaceAdmin(w) {
		return
	}
	h := s.webhooks().byID[id]
	if h == nil {
		notFound(w)
		return
	}
	if !checkIfMatch(w, r, h.LockVersion) {
		return
	}
	delete(s.webhooks().byID, id)
	w.WriteHeader(http.StatusNoContent)
}
