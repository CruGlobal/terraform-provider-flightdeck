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
		// The row stores the name it was created with, as the API does — the
		// rename path depends on that being real rather than hardcoded.
		var sent map[string]map[string]any
		_ = json.NewDecoder(r.Body).Decode(&sent)
		name, _ := sent["routing_key"]["name"].(string)
		if name == "" {
			name = "Routing key"
		}
		s.next++
		id := s.next
		row := map[string]any{
			"id": id, "project_id": 1, "name": name, "last_four": "aaaa",
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
	mux.HandleFunc("PATCH /api/v1/projects/{pid}/routing-keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		row, ok := s.rows[id]
		if !ok {
			writeTestJSON(w, http.StatusNotFound, map[string]any{"error": "Not found", "code": "not_found"})
			return
		}
		var body map[string]map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if name, present := body["routing_key"]["name"]; present {
			row["name"] = name
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
// Idempotency-Key, so the second is served as a replay of the first's live
// row. That row is the first declaration's working credential, so the second
// create fails and names it rather than revoking it. This fake is an older
// Flightdeck that replays a live row without its secret; a current one refuses
// the replay with a 409, and the client reads both the same way.
func TestCreateRoutingKey_identicalSiblingIsRefusedAndNothingIsRevoked(t *testing.T) {
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
	_, retiredID, err := c.CreateRoutingKey(ctx, 1, fields, stable)
	if !HasCode(err, CodeIdempotencyReplayWithheld) {
		t.Fatalf("err = %v, want %s", err, CodeIdempotencyReplayWithheld)
	}
	if apiErr, _ := AsError(err); apiErr.ID != first.ID {
		t.Fatalf("the error names %d, want the first row %d", apiErr.ID, first.ID)
	}
	if retiredID != 0 || len(s.revoked) != 0 {
		t.Fatalf("retiredID=%d revoked=%v: the first row is live and must be left alone", retiredID, s.revoked)
	}
	if s.posts != 2 {
		t.Fatalf("posts = %d, want 2 (original, replay) and no fresh key", s.posts)
	}
}

// The same payload really does produce the same key: this is the mechanism the
// collision above rests on, and `name` is the only thing in the body that can vary.
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

// A key renamed in place leaves its ORIGINAL name claimed in the API's
// idempotency cache for 24 hours, because the cached create was keyed on the
// body that carried that name. Declaring a new key with the freed-up name
// therefore replays that create — and the row it names is live, and belongs to
// somebody else. Revoking it is the same harm as the sibling collision,
// reached by a sequence the schema invites: `name` is editable in place, and
// freeing a name to reuse it is a normal thing to do.
func TestCreateRoutingKey_renamedRowIsNotRevokedByAReusedName(t *testing.T) {
	s, c := newRoutingKeyServer(t)
	ctx := context.Background()
	original := Fields{"name": "Uptime monitor"}
	stable := PayloadKey("routing_key", "1", original)

	first, _, err := c.CreateRoutingKey(ctx, 1, original, stable)
	if err != nil {
		t.Fatal(err)
	}
	// Renamed in place: same row, same live secret, different label.
	if _, err := c.UpdateRoutingKey(ctx, 1, first.ID, Fields{"name": "Uptime monitor (eu)"}, first.LockVersion); err != nil {
		t.Fatal(err)
	}

	// A different monitor now takes the name the first one gave up.
	reused, retiredID, err := c.CreateRoutingKey(ctx, 1, original, stable)
	if err != nil {
		t.Fatal(err)
	}
	if reused.ID == first.ID {
		t.Fatalf("the replay was returned instead of a fresh key: %+v", reused)
	}
	if len(s.revoked) != 0 {
		t.Fatalf("revoked %v — row %d is live and owned by another resource; a reused name must not take it", s.revoked, first.ID)
	}
	if retiredID != 0 {
		t.Fatalf("retiredID = %d, want 0: nothing was retired", retiredID)
	}
	// And the renamed row is still usable.
	still, err := c.GetRoutingKey(ctx, 1, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if still.IsRevoked() {
		t.Fatalf("row %d was revoked", first.ID)
	}
}

// A replay of a create whose row is ALREADY revoked retires nothing, so the
// caller must not be told a row went. This is the path `terraform apply
// -replace` takes — the rotation the docs recommend — and a warning that fires
// on the happy path is one people learn to skip past.
func TestCreateRoutingKey_alreadyRevokedRowIsNotReportedAsRetired(t *testing.T) {
	s, c := newRoutingKeyServer(t)
	ctx := context.Background()
	fields := Fields{"name": "Rotating"}
	stable := PayloadKey("routing_key", "1", fields)

	first, _, err := c.CreateRoutingKey(ctx, 1, fields, stable)
	if err != nil {
		t.Fatal(err)
	}
	// The destroy half of a replacement.
	if err := c.RevokeRoutingKey(ctx, 1, first.ID, first.LockVersion); err != nil {
		t.Fatal(err)
	}
	before := len(s.revoked)

	_, retiredID, err := c.CreateRoutingKey(ctx, 1, fields, stable)
	if err != nil {
		t.Fatal(err)
	}
	if retiredID != 0 {
		t.Fatalf("retiredID = %d, want 0: row %d was already revoked, so this create retired nothing", retiredID, first.ID)
	}
	if len(s.revoked) != before {
		t.Fatalf("revoked %v: nothing more should have been revoked", s.revoked)
	}
}
