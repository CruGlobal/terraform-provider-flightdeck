package flightdecktest

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// RoutingKey is the fake's stored Events API key; Plaintext is kept only so
// the original create response can return it once.
type RoutingKey struct {
	ID                 int64
	ProjectID          int64
	Name               string
	EscalationPolicyID *int64
	// Environment is nil for a key with none. SelfHealingTrigger is whether
	// the key's incidents may trip self-healing.
	Environment        *string
	SelfHealingTrigger bool
	Plaintext          string
	LastFour           string
	RevokedAt          *time.Time
	LastUsedAt         *time.Time
	LockVersion        int64
	CreatedAt          time.Time
}

type routingKeyStore struct {
	byID map[int64]*RoutingKey
	// olderFields simulates a Flightdeck from before a key's environment and
	// self_healing_trigger: the read has neither, and a write naming either
	// is an unknown key.
	olderFields bool
}

// SetRoutingKeyFieldsSupported switches a key's environment and
// self_healing_trigger on or off, to simulate a Flightdeck from before them.
func (s *Server) SetRoutingKeyFieldsSupported(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routingKeys().olderFields = !on
}

// SetRoutingKeyEnvironment sets a key's environment the way the console does
// (nil clears it).
func (s *Server) SetRoutingKeyEnvironment(id int64, environment *string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k := s.routingKeys().byID[id]; k != nil {
		k.Environment = environment
		k.LockVersion++
	}
}

// SetRoutingKeyTrigger switches a key's self_healing_trigger the way the
// console does.
func (s *Server) SetRoutingKeyTrigger(id int64, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k := s.routingKeys().byID[id]; k != nil {
		k.SelfHealingTrigger = on
		k.LockVersion++
	}
}

// routingKeySettable is what a write may name, as the API lists it.
func (s *Server) routingKeySettable() []string {
	if s.routingKeys().olderFields {
		return []string{"name", "escalation_policy_id"}
	}
	return []string{"name", "escalation_policy_id", "environment", "self_healing_trigger"}
}

// routingKeyUnknownKey is the API's refusal of a key a write may not name, or
// "" when every key is settable.
func (s *Server) routingKeyUnknownKey(attrs map[string]any) string {
	settable := s.routingKeySettable()
	var keys []string
	for key := range attrs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if key != "lock_version" && !contains(settable, key) {
			return "unknown key: " + key + " (settable: " + strings.Join(settable, ", ") + ")"
		}
	}
	return ""
}

// routingKeyChanges are the environment and trip flag a write names, read
// the API's way. A nil trigger is no opinion.
type routingKeyChanges struct {
	environmentNamed bool
	environment      *string
	trigger          *bool
}

// parseRoutingKeyChanges reads environment and self_healing_trigger as the
// API does. environment: null, blank or only blank space is none; anything
// else must be a string of at most 255 characters once stripped.
// self_healing_trigger: null or blank is no opinion; true, false and the
// on/off words set it. It returns the API's refusal, or "".
func parseRoutingKeyChanges(attrs map[string]any) (routingKeyChanges, string) {
	var out routingKeyChanges
	if v, has := attrs["environment"]; has {
		out.environmentNamed = true
		switch t := v.(type) {
		case nil:
		case string:
			if trimmed := strings.Trim(t, rubyBlank); trimmed != "" {
				if utf8.RuneCountInString(trimmed) > 255 {
					return out, "environment must be 255 characters or fewer"
				}
				out.environment = &trimmed
			}
		default:
			return out, fmt.Sprintf("environment must be a string, got %T", v)
		}
	}
	if v, has := attrs["self_healing_trigger"]; has && v != nil {
		on, opinion := false, true
		switch t := v.(type) {
		case bool:
			on = t
		case string:
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "":
				opinion = false
			case "true", "on", "1":
				on = true
			case "false", "off", "0":
			default:
				return out, fmt.Sprintf("self_healing_trigger must be true or false, got %q", t)
			}
		default:
			return out, fmt.Sprintf("self_healing_trigger must be true or false, got %v", v)
		}
		if opinion {
			out.trigger = &on
		}
	}
	return out, ""
}

// rubyBlank is what Ruby's String#strip removes: ASCII white space and NUL.
const rubyBlank = "\x00\t\n\v\f\r "

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
func (s *Server) serializeRoutingKey(k *RoutingKey, reveal bool) map[string]any {
	out := map[string]any{
		"id": k.ID, "project_id": k.ProjectID, "name": k.Name,
		"escalation_policy_id": nil, "masked": "fd_evt_…" + k.LastFour, "last_four": k.LastFour,
		"revoked": k.RevokedAt != nil, "revoked_at": nil, "last_used_at": nil,
		"lock_version": k.LockVersion, "created_at": iso(k.CreatedAt), "updated_at": iso(k.CreatedAt),
	}
	if k.EscalationPolicyID != nil {
		out["escalation_policy_id"] = *k.EscalationPolicyID
	}
	if !s.routingKeys().olderFields {
		out["environment"] = nil
		if k.Environment != nil {
			out["environment"] = *k.Environment
		}
		out["self_healing_trigger"] = k.SelfHealingTrigger
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
		items = append(items, s.serializeRoutingKey(k, false))
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
	writeJSON(w, http.StatusOK, s.serializeRoutingKey(k, false))
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
	s.withIdempotencyRedacted(w, r, "routing_key", s.routingKeyWithheld(pid), func() (int, any, any) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.liveProject(pid) == nil {
			return http.StatusNotFound, errorBody("Not found", "not_found"), nil
		}
		if msg := s.routingKeyUnknownKey(attrs); msg != "" {
			return http.StatusUnprocessableEntity, errorBody(msg, "invalid_attribute"), nil
		}
		changes, msg := parseRoutingKeyChanges(attrs)
		if msg != "" {
			return http.StatusUnprocessableEntity, errorBody(msg, "invalid_attribute"), nil
		}
		k := &RoutingKey{ID: s.id(), ProjectID: pid, Name: "Routing key", CreatedAt: time.Now(), Environment: changes.environment}
		if changes.trigger != nil {
			k.SelfHealingTrigger = *changes.trigger
		}
		if v := strings.TrimSpace(asString(attrs["name"])); v != "" {
			k.Name = v
		}
		raw := make([]byte, 32)
		_, _ = rand.Read(raw)
		k.Plaintext = "fd_evt_" + base64.RawURLEncoding.EncodeToString(raw)
		k.LastFour = k.Plaintext[len(k.Plaintext)-4:]
		s.routingKeys().byID[k.ID] = k
		redacted := s.serializeRoutingKey(k, false)
		redacted["routing_key"] = nil
		redacted["secret_available"] = false
		redacted["message"] = "Replay of a previously-used Idempotency-Key. The routing key is returned only by the original create and is never stored, so it cannot be replayed."
		return http.StatusCreated, s.serializeRoutingKey(k, true), redacted
	})
}

// routingKeyWithheld is the API's replay rule for routing keys: refused while
// the key the create made is live AND still carries the name the cached
// response recorded. A key renamed since replays as before.
func (s *Server) routingKeyWithheld(projectID int64) *withheldSecret {
	return &withheldSecret{
		live: func(replayed map[string]any) bool {
			id, _ := asInt64(replayed["id"])
			k := s.liveRoutingKey(projectID, id)
			return k != nil && k.RevokedAt == nil && k.Name == asString(replayed["name"])
		},
		message: func(id int64) string {
			return fmt.Sprintf("This create was already done, and the key it made is still live, so its secret "+
				"can't be sent again. If this is a retry of your own create, revoke key %d and "+
				"create again; if another key in this project needs the same name, give it a "+
				"different name.", id)
		},
	}
}

// Update writes what it names; an absent key keeps its stored value.
// escalation_policy_id is settable at the API but the provider never sends
// it. A null or blank environment clears it, and a revoked key's trip flag
// can't change.
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
	if msg := s.routingKeyUnknownKey(attrs); msg != "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_attribute", msg)
		return
	}
	changes, msg := parseRoutingKeyChanges(attrs)
	if msg != "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_attribute", msg)
		return
	}
	if changes.trigger != nil && k.RevokedAt != nil && *changes.trigger != k.SelfHealingTrigger {
		writeError(w, http.StatusUnprocessableEntity, "invalid_attribute",
			"self_healing_trigger can't be changed on a revoked routing key")
		return
	}
	candidate := *k
	if changes.environmentNamed {
		candidate.Environment = changes.environment
	}
	if changes.trigger != nil {
		candidate.SelfHealingTrigger = *changes.trigger
	}
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
	writeJSON(w, http.StatusOK, s.serializeRoutingKey(k, false))
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
	writeJSON(w, http.StatusOK, s.serializeRoutingKey(k, false))
}
