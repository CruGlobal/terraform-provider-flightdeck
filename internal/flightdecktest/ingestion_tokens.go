package flightdecktest

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sort"
	"strings"
	"time"
)

var ingestionScopes = []string{"post_server_item", "post_client_item"}

// IngestionToken is the fake's stored token; Plaintext is kept only so the
// original create response can return it once.
type IngestionToken struct {
	ID          int64
	ProjectID   int64
	Name        string
	Environment string
	Scope       string
	Plaintext   string
	LastFour    string
	RevokedAt   *time.Time
	LockVersion int64
	CreatedAt   time.Time
}

type ingestionTokenStore struct{ byID map[int64]*IngestionToken }

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["ingestion_tokens"] = &ingestionTokenStore{byID: map[int64]*IngestionToken{}}
		mux.HandleFunc("GET /api/v1/projects/{project_id}/ingestion-tokens", s.listIngestionTokens)
		mux.HandleFunc("POST /api/v1/projects/{project_id}/ingestion-tokens", s.createIngestionToken)
		mux.HandleFunc("GET /api/v1/projects/{project_id}/ingestion-tokens/{id}", s.showIngestionToken)
		mux.HandleFunc("DELETE /api/v1/projects/{project_id}/ingestion-tokens/{id}", s.revokeIngestionToken)
	})
}

func (s *Server) ingestionTokens() *ingestionTokenStore {
	store, _ := s.stores["ingestion_tokens"].(*ingestionTokenStore)
	return store
}

// IngestionToken returns a stored token (revoked or not), or nil.
func (s *Server) IngestionToken(id int64) *IngestionToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.ingestionTokens().byID[id]
	if t == nil {
		return nil
	}
	cp := *t
	return &cp
}

// RevokeIngestionTokenOutOfBand revokes a token the way the UI does.
func (s *Server) RevokeIngestionTokenOutOfBand(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.ingestionTokens().byID[id]; t != nil {
		now := time.Now()
		t.RevokedAt = &now
		t.LockVersion++
	}
}

func (s *Server) liveIngestionToken(projectID, id int64) *IngestionToken {
	t := s.ingestionTokens().byID[id]
	if t == nil || t.ProjectID != projectID || s.liveProject(projectID) == nil {
		return nil
	}
	return t
}

// serializeIngestionToken mirrors Serializers.ingestion_token(token, reveal:).
func serializeIngestionToken(t *IngestionToken, reveal bool) map[string]any {
	out := map[string]any{
		"id": t.ID, "project_id": t.ProjectID, "name": t.Name, "environment": t.Environment,
		"scope": t.Scope, "masked": "fd_post_…" + t.LastFour, "last_four": t.LastFour,
		"revoked": t.RevokedAt != nil, "revoked_at": nil, "last_used_at": nil,
		"lock_version": t.LockVersion, "created_at": iso(t.CreatedAt),
	}
	if t.RevokedAt != nil {
		out["revoked_at"] = iso(*t.RevokedAt)
	}
	if reveal {
		out["token"] = t.Plaintext
	}
	return out
}

func (s *Server) listIngestionTokens(w http.ResponseWriter, r *http.Request) {
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
	var rows []*IngestionToken
	for _, t := range s.ingestionTokens().byID {
		if t.ProjectID == pid {
			rows = append(rows, t)
		}
	}
	// recent_first, as the model scope orders.
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID > rows[j].ID })
	items := make([]any, 0, len(rows))
	for _, t := range rows {
		items = append(items, serializeIngestionToken(t, false))
	}
	s.mu.Unlock()
	writeCollection(w, r, items)
}

func (s *Server) showIngestionToken(w http.ResponseWriter, r *http.Request) {
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
	t := s.liveIngestionToken(pid, id)
	if t == nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, serializeIngestionToken(t, false))
}

// Create renders the secret once; the idempotency cache holds a REDACTED body,
// so a replay returns the row with `token: null, secret_available: false`.
func (s *Server) createIngestionToken(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "ingestion_token")
	if !ok {
		return
	}
	s.withIdempotencyRedacted(w, r, "ingestion_token", func() (int, any, any) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.liveProject(pid) == nil {
			return http.StatusNotFound, map[string]any{"error": "Not found", "code": "not_found"}, nil
		}
		t := &IngestionToken{ID: s.id(), ProjectID: pid, Name: "Ingestion token", Environment: "production", Scope: "post_server_item", CreatedAt: time.Now()}
		if v := strings.TrimSpace(asString(attrs["name"])); v != "" {
			t.Name = v
		}
		if v := strings.TrimSpace(asString(attrs["environment"])); v != "" {
			t.Environment = v
		}
		if v := strings.TrimSpace(asString(attrs["scope"])); v != "" {
			if !contains(ingestionScopes, v) {
				return http.StatusUnprocessableEntity, map[string]any{"error": "unknown scope: " + v, "code": "invalid_attribute"}, nil
			}
			t.Scope = v
		}
		raw := make([]byte, 32)
		_, _ = rand.Read(raw)
		t.Plaintext = "fd_post_" + base64.RawURLEncoding.EncodeToString(raw)
		t.LastFour = t.Plaintext[len(t.Plaintext)-4:]
		s.ingestionTokens().byID[t.ID] = t
		redacted := serializeIngestionToken(t, false)
		redacted["token"] = nil
		redacted["secret_available"] = false
		redacted["message"] = "Replay of a previously-used Idempotency-Key. The token is returned only by the original create and is never stored, so it cannot be replayed."
		return http.StatusCreated, serializeIngestionToken(t, true), redacted
	})
}

// Revoke, not delete: the row stays; answered 200 with the revoked row, and a
// second revoke is idempotent. If-Match is honoured.
func (s *Server) revokeIngestionToken(w http.ResponseWriter, r *http.Request) {
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
	t := s.liveIngestionToken(pid, id)
	if t == nil {
		notFound(w)
		return
	}
	if !checkIfMatch(w, r, t.LockVersion) {
		return
	}
	if t.RevokedAt == nil {
		now := time.Now()
		t.RevokedAt = &now
		t.LockVersion++
	}
	writeJSON(w, http.StatusOK, serializeIngestionToken(t, false))
}
