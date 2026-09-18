package flightdecktest

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sort"
	"strings"
	"time"
)

// RoutingKey is the fake's stored Events API key; Plaintext is kept only so
// the original create response can return it once.
type RoutingKey struct {
	ID                 int64
	ProjectID          int64
	Name               string
	EscalationPolicyID *int64
	Plaintext          string
	LastFour           string
	RevokedAt          *time.Time
	LastUsedAt         *time.Time
	LockVersion        int64
	CreatedAt          time.Time
}

type routingKeyStore struct{ byID map[int64]*RoutingKey }

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["routing_keys"] = &routingKeyStore{byID: map[int64]*RoutingKey{}}
		mux.HandleFunc("GET /api/v1/projects/{project_id}/routing-keys", s.listRoutingKeys)
		mux.HandleFunc("POST /api/v1/projects/{project_id}/routing-keys", s.createRoutingKey)
		mux.HandleFunc("GET /api/v1/projects/{project_id}/routing-keys/{id}", s.showRoutingKey)
		mux.HandleFunc("PATCH /api/v1/projects/{project_id}/routing-keys/{id}", s.updateRoutingKey)
		mux.HandleFunc("DELETE /api/v1/projects/{project_id}/routing-keys/{id}", s.revokeRoutingKey)
	})
}

func (s *Server) routingKeys() *routingKeyStore {
	store, _ := s.stores["routing_keys"].(*routingKeyStore)
	return store
}

// RoutingKey returns a stored key (revoked or not), or nil.
func (s *Server) RoutingKey(id int64) *RoutingKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.routingKeys().byID[id]
	if k == nil {
		return nil
	}
	cp := *k
	return &cp
}

// RevokeRoutingKeyOutOfBand revokes a key the way the console does.
func (s *Server) RevokeRoutingKeyOutOfBand(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k := s.routingKeys().byID[id]; k != nil && k.RevokedAt == nil {
		now := time.Now()
		k.RevokedAt = &now
		k.LockVersion++
	}
}

// TouchRoutingKey simulates an out-of-band edit that bumps lock_version.
func (s *Server) TouchRoutingKey(id int64, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k := s.routingKeys().byID[id]; k != nil {
		k.Name = name
		k.LockVersion++
	}
}

// AttachRoutingKeyEscalationPolicy points a key at an escalation policy the
// way the console does. There is no API for it; this exists so a test can
// prove the provider reports it and never writes it.
func (s *Server) AttachRoutingKeyEscalationPolicy(id, policyID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k := s.routingKeys().byID[id]; k != nil {
		k.EscalationPolicyID = &policyID
		k.LockVersion++
	}
}

func (s *Server) liveRoutingKey(projectID, id int64) *RoutingKey {
	k := s.routingKeys().byID[id]
	if k == nil || k.ProjectID != projectID || s.liveProject(projectID) == nil {
		return nil
	}
	return k
}

// serializeRoutingKey mirrors the API's shape; reveal adds the secret.
func serializeRoutingKey(k *RoutingKey, reveal bool) map[string]any {
	out := map[string]any{
		"id": k.ID, "project_id": k.ProjectID, "name": k.Name,
		"escalation_policy_id": nil, "masked": "fd_evt_…" + k.LastFour, "last_four": k.LastFour,
		"revoked": k.RevokedAt != nil, "revoked_at": nil, "last_used_at": nil,
		"lock_version": k.LockVersion, "created_at": iso(k.CreatedAt), "updated_at": iso(k.CreatedAt),
	}
	if k.EscalationPolicyID != nil {
		out["escalation_policy_id"] = *k.EscalationPolicyID
	}
	if k.RevokedAt != nil {
		out["revoked_at"] = iso(*k.RevokedAt)
	}
	if k.LastUsedAt != nil {
		out["last_used_at"] = iso(*k.LastUsedAt)
	}
	if reveal {
		out["routing_key"] = k.Plaintext
	}
	return out
}

func (s *Server) listRoutingKeys(w http.ResponseWriter, r *http.Request) {
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
	var rows []*RoutingKey
	for _, k := range s.routingKeys().byID {
		if k.ProjectID == pid {
			rows = append(rows, k)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	items := make([]any, 0, len(rows))
	for _, k := range rows {
		items = append(items, serializeRoutingKey(k, false))
	}
	s.mu.Unlock()
	writeCollection(w, r, items)
}

func (s *Server) showRoutingKey(w http.ResponseWriter, r *http.Request) {
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
	k := s.liveRoutingKey(pid, id)
	if k == nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, serializeRoutingKey(k, false))
}

// Create renders the secret once; the idempotency cache holds a REDACTED body,
// so a replay returns the row with `routing_key: null, secret_available: false`.
func (s *Server) createRoutingKey(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "routing_key")
	if !ok {
		return
	}
	s.withIdempotencyRedacted(w, r, "routing_key", func() (int, any, any) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.liveProject(pid) == nil {
			return http.StatusNotFound, errorBody("Not found", "not_found"), nil
		}
		for key := range attrs {
			if key != "name" && key != "escalation_policy_id" && key != "lock_version" {
				return http.StatusUnprocessableEntity,
					errorBody("unknown key: "+key+" (settable: name, escalation_policy_id)", "invalid_attribute"), nil
			}
		}
		k := &RoutingKey{ID: s.id(), ProjectID: pid, Name: "Routing key", CreatedAt: time.Now()}
		if v := strings.TrimSpace(asString(attrs["name"])); v != "" {
			k.Name = v
		}
		raw := make([]byte, 32)
		_, _ = rand.Read(raw)
		k.Plaintext = "fd_evt_" + base64.RawURLEncoding.EncodeToString(raw)
		k.LastFour = k.Plaintext[len(k.Plaintext)-4:]
		s.routingKeys().byID[k.ID] = k
		redacted := serializeRoutingKey(k, false)
		redacted["routing_key"] = nil
		redacted["secret_available"] = false
		redacted["message"] = "Replay of a previously-used Idempotency-Key. The routing key is returned only by the original create and is never stored, so it cannot be replayed."
		return http.StatusCreated, serializeRoutingKey(k, true), redacted
	})
}

// Update writes name only. escalation_policy_id is settable at the API but the
// provider never sends it; an absent key keeps its stored value.
func (s *Server) updateRoutingKey(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "routing_key")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.liveRoutingKey(pid, id)
	if k == nil {
		notFound(w)
		return
	}
	if !checkIfMatch(w, r, k.LockVersion) {
		return
	}
	for key := range attrs {
		if key != "name" && key != "escalation_policy_id" && key != "lock_version" {
			writeError(w, http.StatusUnprocessableEntity, "invalid_attribute",
				"unknown key: "+key+" (settable: name, escalation_policy_id)")
			return
		}
	}
	candidate := *k
	if v, has := attrs["name"]; has && strings.TrimSpace(asString(v)) != "" {
		candidate.Name = strings.TrimSpace(asString(v))
	}
	if v, has := attrs["escalation_policy_id"]; has {
		if v == nil {
			candidate.EscalationPolicyID = nil
		} else if n, isNum := asInt64(v); isNum {
			candidate.EscalationPolicyID = &n
		}
	}
	candidate.LockVersion++
	*k = candidate
	writeJSON(w, http.StatusOK, serializeRoutingKey(k, false))
}

// Revoke, not delete: the row stays, answered 200 with the revoked row, and a
// second revoke is idempotent. Revoking cannot be undone.
func (s *Server) revokeRoutingKey(w http.ResponseWriter, r *http.Request) {
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
	k := s.liveRoutingKey(pid, id)
	if k == nil {
		notFound(w)
		return
	}
	if !checkIfMatch(w, r, k.LockVersion) {
		return
	}
	if k.RevokedAt == nil {
		now := time.Now()
		k.RevokedAt = &now
		k.LockVersion++
	}
	writeJSON(w, http.StatusOK, serializeRoutingKey(k, false))
}
