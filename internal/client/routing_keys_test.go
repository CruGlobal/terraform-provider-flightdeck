package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// routingKeyServer is the smallest fake that exhibits the behaviour under
// test: a create returns its secret once, the SAME Idempotency-Key replays the
// row with the secret redacted, and a revoke is recorded.
type routingKeyServer struct {
	mu      sync.Mutex
	next    int64
	rows    map[int64]map[string]any
	replays map[string]int64
	revoked []int64
	posts   int
}

func newRoutingKeyServer(t *testing.T) (*routingKeyServer, *Client) {
	t.Helper()
	s := &routingKeyServer{rows: map[int64]map[string]any{}, replays: map[string]int64{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/projects/{pid}/routing-keys", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.posts++
		key := r.Header.Get("Idempotency-Key")
		if id, replayed := s.replays[key]; replayed && key != "" {
			// A replay never carries the secret: that is the whole problem.
			body := map[string]any{}
			for k, v := range s.rows[id] {
				body[k] = v
			}
			body["routing_key"] = nil
			body["secret_available"] = false
			writeTestJSON(w, http.StatusCreated, body)
			return
		}
		s.next++
		id := s.next
		row := map[string]any{
			"id": id, "project_id": 1, "name": "k", "last_four": "aaaa",
			"masked": "fd_evt_…aaaa", "revoked": false, "lock_version": 0,
		}
		s.rows[id] = row
		if key != "" {
			s.replays[key] = id
		}
		body := map[string]any{}
		for k, v := range row {
			body[k] = v
		}
		body["routing_key"] = fmt.Sprintf("fd_evt_secret_%d", id)
		writeTestJSON(w, http.StatusCreated, body)
	})
	mux.HandleFunc("GET /api/v1/projects/{pid}/routing-keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		row, ok := s.rows[id]
		if !ok {
			writeTestJSON(w, http.StatusNotFound, map[string]any{"error": "Not found", "code": "not_found"})
			return
		}
		writeTestJSON(w, http.StatusOK, row)
	})
	mux.HandleFunc("DELETE /api/v1/projects/{pid}/routing-keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if row, ok := s.rows[id]; ok {
			row["revoked"] = true
			s.revoked = append(s.revoked, id)
		}
		writeTestJSON(w, http.StatusOK, s.rows[id])
	})
	return s, newTestClient(t, mux)
}

func writeTestJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// A fresh create reports nothing retired, so the provider stays quiet.
func TestCreateRoutingKey_freshCreateRetiresNothing(t *testing.T) {
	s, c := newRoutingKeyServer(t)
	key, retiredID, err := c.CreateRoutingKey(context.Background(), 1, Fields{"name": "a"}, "key-a")
	if err != nil {
		t.Fatal(err)
	}
	if retiredID != 0 {
		t.Fatalf("retiredID = %d, want 0", retiredID)
	}
	if key.Key == "" {
		t.Fatal("the created key carries no secret")
	}
	if len(s.revoked) != 0 {
		t.Fatalf("revoked %v, want none", s.revoked)
	}
}

// Two declarations sending an identical body send an identical
// Idempotency-Key, so the second is served as a replay. Recovering from that
// retires the FIRST declaration's live row — which is why the caller has to be
// told, and is the case `name` being Required makes accidental rather than
// impossible.
func TestCreateRoutingKey_replayReportsTheRetiredRow(t *testing.T) {
	s, c := newRoutingKeyServer(t)
	ctx := context.Background()
	fields := Fields{"name": "Same"}
	stable := PayloadKey("routing_key", "1", fields)

	first, retired, err := c.CreateRoutingKey(ctx, 1, fields, stable)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 0 {
		t.Fatalf("first create retired %d, want 0", retired)
	}

	// A second resource, same project, same name: byte-identical body.
	second, retiredID, err := c.CreateRoutingKey(ctx, 1, fields, stable)
	if err != nil {
		t.Fatal(err)
	}
	if retiredID != first.ID {
		t.Fatalf("retiredID = %d, want the first row %d — the caller cannot warn about what it is not told", retiredID, first.ID)
	}
	if second.ID == first.ID || second.Key == "" {
		t.Fatalf("the replay was returned instead of a fresh key: %+v", second)
	}
	if len(s.revoked) != 1 || s.revoked[0] != first.ID {
		t.Fatalf("revoked = %v, want exactly the first row %d", s.revoked, first.ID)
	}
	if s.posts != 3 {
		t.Fatalf("posts = %d, want 3 (original, replay, fresh key)", s.posts)
	}
}

// The same payload really does produce the same key: this is the mechanism the
// collision rests on, and `name` is the only thing in the body that can vary.
func TestPayloadKey_sameBodySameKeyDifferentNameDiffers(t *testing.T) {
	same := PayloadKey("routing_key", "1", Fields{"name": "Same"})
	again := PayloadKey("routing_key", "1", Fields{"name": "Same"})
	other := PayloadKey("routing_key", "1", Fields{"name": "Other"})
	if same != again {
		t.Fatalf("identical bodies produced different keys: %s vs %s", same, again)
	}
	if same == other {
		t.Fatal("different names produced the same key")
	}
	if !strings.HasPrefix(same, "tf-") {
		t.Fatalf("unexpected key shape %q", same)
	}
}
