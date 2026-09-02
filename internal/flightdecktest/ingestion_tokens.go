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
// create response can return it once.
type IngestionToken struct {
	ID          int64
	ProjectID   int64
	Name        string
	Environment string
	Scope       string
	Plaintext   string
	LastFour    string
	RevokedAt   *time.Time
	CreatedAt   time.Time
}

type ingestionTokenStore struct {
	byID map[int64]*IngestionToken
	// hideFromList simulates a list that filters out (or mis-serialises) a
	// just-created token, so its create can never be verified.
	hideFromList bool
}

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["ingestion_tokens"] = &ingestionTokenStore{byID: map[int64]*IngestionToken{}}
		mux.HandleFunc("GET /api/v1/projects/{project_id}/ingestion_tokens", s.listIngestionTokens)
		mux.HandleFunc("POST /api/v1/projects/{project_id}/ingestion_tokens", s.createIngestionToken)
		mux.HandleFunc("DELETE /api/v1/ingestion_tokens/{id}", s.revokeIngestionToken)
	})
}

func (s *Server) ingestionTokens() *ingestionTokenStore {
	store, _ := s.stores["ingestion_tokens"].(*ingestionTokenStore)
	return store
}

// HideIngestionTokensFromList makes the list endpoint omit every token.
func (s *Server) HideIngestionTokensFromList(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ingestionTokens().hideFromList = on
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
	}
}

func serializeIngestionToken(t *IngestionToken, withPlaintext bool) map[string]any {
	out := map[string]any{
		"id": t.ID, "project_id": t.ProjectID, "name": t.Name, "environment": t.Environment,
		"scope": t.Scope, "last_four": t.LastFour, "masked": "fd_post_…" + t.LastFour,
		"revoked_at": nil, "last_used_at": nil, "created_at": iso(t.CreatedAt),
	}
	if t.RevokedAt != nil {
		out["revoked_at"] = iso(*t.RevokedAt)
	}
	if withPlaintext {
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
		if t.ProjectID == pid && !s.ingestionTokens().hideFromList {
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

func (s *Server) createIngestionToken(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "ingestion_token")
	if !ok {
		return
	}
	s.withIdempotency(w, r, "ingestion_token", func() (int, any) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.liveProject(pid) == nil {
			return http.StatusNotFound, map[string]any{"error": "Not found", "code": "not_found"}
		}
		t := &IngestionToken{ID: s.id(), ProjectID: pid, Environment: "production", Scope: "post_server_item", CreatedAt: time.Now()}
		t.Name = asString(attrs["name"])
		if v, has := attrs["environment"]; has && v != nil {
			t.Environment = asString(v)
		}
		if v, has := attrs["scope"]; has && v != nil {
			t.Scope = asString(v)
		}
		if strings.TrimSpace(t.Name) == "" {
			return http.StatusUnprocessableEntity, map[string]any{"error": "Name can't be blank", "code": "validation_failed"}
		}
		if strings.TrimSpace(t.Environment) == "" {
			return http.StatusUnprocessableEntity, map[string]any{"error": "Environment can't be blank", "code": "validation_failed"}
		}
		if !contains(ingestionScopes, t.Scope) {
			return http.StatusUnprocessableEntity, map[string]any{
				"error": "'" + t.Scope + "' is not a valid scope", "code": "invalid_attribute"}
		}
		raw := make([]byte, 32)
		_, _ = rand.Read(raw)
		t.Plaintext = "fd_post_" + base64.RawURLEncoding.EncodeToString(raw)
		t.LastFour = t.Plaintext[len(t.Plaintext)-4:]
		s.ingestionTokens().byID[t.ID] = t
		return http.StatusCreated, serializeIngestionToken(t, true)
	})
}

func (s *Server) revokeIngestionToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.ingestionTokens().byID[id]
	if t == nil || s.liveProject(t.ProjectID) == nil || t.RevokedAt != nil {
		notFound(w)
		return
	}
	now := time.Now()
	t.RevokedAt = &now
	w.WriteHeader(http.StatusNoContent)
}
