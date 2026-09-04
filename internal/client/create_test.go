package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// widget is a minimal Identified resource for exercising CreateResource.
type widget struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (w *widget) ResourceID() int64 { return w.ID }

// widgetServer is a tiny API: POST /widgets creates (honouring Idempotency-Key
// replay), GET /widgets/{id} shows. Knobs shape the responses.
type widgetServer struct {
	mu       sync.Mutex
	nextID   int64
	rows     map[int64]*widget
	replays  map[string][]byte
	wrap     bool // wrap responses in {"widget": ...}
	zeroID   bool // omit id from the create response
	lagReads int  // number of GETs to 404 before answering
	posts    int
	gets     int
	deleteOn map[int64]bool
}

func newWidgetServer(t *testing.T) (*widgetServer, *Client) {
	t.Helper()
	ws := &widgetServer{nextID: 100, rows: map[int64]*widget{}, replays: map[string][]byte{}, deleteOn: map[int64]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/widgets", ws.create)
	mux.HandleFunc("GET /api/v1/widgets/{id}", ws.show)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return ws, c
}

func (ws *widgetServer) write(w http.ResponseWriter, status int, row *widget) {
	body := map[string]any{"name": row.Name}
	if !ws.zeroID {
		body["id"] = row.ID
	}
	var out any = body
	if ws.wrap {
		out = map[string]any{"widget": body}
	}
	encoded, _ := json.Marshal(out)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

func (ws *widgetServer) create(w http.ResponseWriter, r *http.Request) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.posts++
	key := r.Header.Get("Idempotency-Key")
	if stored, ok := ws.replays[key]; ok && key != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(stored)
		return
	}
	var body struct {
		Widget map[string]any `json:"widget"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	ws.nextID++
	row := &widget{ID: ws.nextID, Name: fmt.Sprint(body.Widget["name"])}
	ws.rows[row.ID] = row
	rec := httptest.NewRecorder()
	ws.write(rec, http.StatusCreated, row)
	if key != "" {
		ws.replays[key] = rec.Body.Bytes()
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(rec.Body.Bytes())
}

func (ws *widgetServer) show(w http.ResponseWriter, r *http.Request) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.gets++
	var id int64
	_, _ = fmt.Sscan(r.PathValue("id"), &id)
	row, ok := ws.rows[id]
	if ws.lagReads > 0 {
		ws.lagReads--
		ok = false
	}
	if !ok || ws.deleteOn[id] {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"error":"Not found","code":"not_found"}`)
		return
	}
	ws.write(w, http.StatusOK, row)
}

func (ws *widgetServer) get(ctx context.Context, c *Client, id int64) (*widget, error) {
	return GetResource[*widget](ctx, c, fmt.Sprintf("/widgets/%d", id), "widget")
}

func createWidget(ctx context.Context, c *Client, ws *widgetServer, key string) (*widget, error) {
	return CreateResource(ctx, c, "/widgets", "widget", Fields{"name": "w"}, key,
		VerifyByGet(func(ctx context.Context, id int64) (*widget, error) { return ws.get(ctx, c, id) }))
}

func TestCreateResource_flatAndWrappedResponses(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrapped=%v", wrapped), func(t *testing.T) {
			ws, c := newWidgetServer(t)
			ws.wrap = wrapped
			got, err := createWidget(context.Background(), c, ws, "k1")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if got.ID != 101 || got.Name != "w" {
				t.Errorf("got %+v", got)
			}
			if ws.posts != 1 {
				t.Errorf("posts = %d, want 1", ws.posts)
			}
		})
	}
}

func TestCreateResource_responseWithoutIDIsAHardError(t *testing.T) {
	ws, c := newWidgetServer(t)
	ws.zeroID = true
	_, err := createWidget(context.Background(), c, ws, "k1")
	if err == nil {
		t.Fatal("expected an error for a create response without an id")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %T %v", err, err)
	}
	if !strings.Contains(apiErr.Message, "no usable widget id") || !strings.Contains(apiErr.Message, "object with keys [name]") {
		t.Errorf("message = %q", apiErr.Message)
	}
	if apiErr.Path != "/widgets" {
		t.Errorf("path = %q", apiErr.Path)
	}
	if ws.posts != 1 {
		t.Errorf("posts = %d: a second POST after an unusable response would duplicate the resource", ws.posts)
	}
	if ws.gets != 0 {
		t.Errorf("gets = %d: verification must not run against id 0", ws.gets)
	}
}

func TestCreateResource_replayOfDeletedResourceRecreatesExactlyOnce(t *testing.T) {
	ws, c := newWidgetServer(t)
	ctx := context.Background()
	first, err := createWidget(ctx, c, ws, "stable")
	if err != nil {
		t.Fatal(err)
	}
	// Delete it out of band; the server still holds the cached 201 for "stable".
	ws.mu.Lock()
	ws.deleteOn[first.ID] = true
	ws.mu.Unlock()

	second, err := createWidget(ctx, c, ws, "stable")
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("recreate returned the replayed, deleted id %d", first.ID)
	}
	// 1 original + 1 replayed + 1 fresh-key create.
	if ws.posts != 3 {
		t.Errorf("posts = %d, want 3", ws.posts)
	}
	// The replayed id was read len(verifyDelays) times before being declared gone.
	if ws.gets < len(verifyDelays)+1 {
		t.Errorf("gets = %d, want at least %d (lag retries) + 1 (verify the fresh create)", ws.gets, len(verifyDelays))
	}
}

func TestCreateResource_readAfterWriteLagIsAbsorbed(t *testing.T) {
	ws, c := newWidgetServer(t)
	ws.lagReads = len(verifyDelays) - 1 // 404 on every read but the last permitted attempt
	got, err := createWidget(context.Background(), c, ws, "k1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.ID != 101 || ws.posts != 1 {
		t.Errorf("got %+v posts=%d: lag must not be mistaken for a replay", got, ws.posts)
	}
}

func TestCreateResource_secondGoneIsAnErrorNotAThirdPost(t *testing.T) {
	ws, c := newWidgetServer(t)
	ctx := context.Background()
	first, err := createWidget(ctx, c, ws, "stable")
	if err != nil {
		t.Fatal(err)
	}
	// Every widget reads as gone from now on: the replay AND the fresh create.
	ws.mu.Lock()
	ws.deleteOn[first.ID] = true
	ws.deleteOn[first.ID+1] = true
	ws.mu.Unlock()
	_, err = createWidget(ctx, c, ws, "stable")
	if err == nil || !strings.Contains(err.Error(), "cannot be read back") {
		t.Fatalf("err = %v", err)
	}
	if ws.posts != 3 {
		t.Errorf("posts = %d, want exactly 3 (no third create attempt)", ws.posts)
	}
}

func TestCreateResource_inconclusiveVerifyIsAnErrorNotARecreate(t *testing.T) {
	ws, c := newWidgetServer(t)
	unknown := func(ctx context.Context, created *widget) (Verdict, error) { return VerifiedUnknown, nil }
	_, err := CreateResource(context.Background(), c, "/widgets", "widget", Fields{"name": "w"}, "k1", unknown)
	if err == nil || !strings.Contains(err.Error(), "refusing to create another") {
		t.Fatalf("err = %v", err)
	}
	if ws.posts != 1 {
		t.Errorf("posts = %d, want 1", ws.posts)
	}
}

func TestCreateResource_verifyErrorsSurface(t *testing.T) {
	ws, c := newWidgetServer(t)
	boom := &Error{Status: 403, Code: CodeForbidden, Message: "nope"}
	failing := func(ctx context.Context, created *widget) (Verdict, error) { return VerifiedUnknown, boom }
	_, err := CreateResource(context.Background(), c, "/widgets", "widget", Fields{"name": "w"}, "k1", failing)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if ws.posts != 1 {
		t.Errorf("posts = %d", ws.posts)
	}
}

func TestDecodeResource(t *testing.T) {
	flat := json.RawMessage(`{"id": 5, "name": "a"}`)
	wrapped := json.RawMessage(`{"widget": {"id": 6, "name": "b"}}`)
	// A flat resource that happens to carry an attribute named like the root
	// key must not be unwrapped.
	tricky := json.RawMessage(`{"id": 7, "widget": {"nested": true}, "name": "c"}`)

	if w, err := DecodeResource[*widget](flat, "widget"); err != nil || w.ID != 5 {
		t.Errorf("flat: %+v %v", w, err)
	}
	if w, err := DecodeResource[*widget](wrapped, "widget"); err != nil || w.ID != 6 {
		t.Errorf("wrapped: %+v %v", w, err)
	}
	if w, err := DecodeResource[*widget](tricky, "widget"); err != nil || w.ID != 7 {
		t.Errorf("tricky: %+v %v", w, err)
	}
	if _, err := DecodeResource[*widget](json.RawMessage(`[1,2]`), "widget"); err == nil {
		t.Error("array decoded as a widget")
	}
}

func TestGetAndPatchResource_acceptWrappedResponses(t *testing.T) {
	ws, c := newWidgetServer(t)
	ws.wrap = true
	ctx := context.Background()
	created, err := createWidget(ctx, c, ws, "k1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ws.get(ctx, c, created.ID)
	if err != nil || got.ID != created.ID {
		t.Fatalf("get: %+v %v", got, err)
	}
}

func TestListResources_terminators(t *testing.T) {
	type item struct {
		ID int `json:"id"`
	}
	serve := func(t *testing.T, pages [][]int, meta func(page int) map[string]any, wrapItems bool) *Client {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var page int
			_, _ = fmt.Sscan(r.URL.Query().Get("page"), &page)
			results := []any{}
			if page-1 < len(pages) {
				for _, id := range pages[page-1] {
					if wrapItems {
						results = append(results, map[string]any{"item": map[string]any{"id": id}})
					} else {
						results = append(results, map[string]any{"id": id})
					}
				}
			}
			body := map[string]any{"results": results}
			if meta != nil {
				if m := meta(page); m != nil {
					body["meta"] = m
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
		}))
		t.Cleanup(srv.Close)
		c, err := New(srv.URL, "tok")
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	ids := func(items []item) []int {
		out := []int{}
		for _, it := range items {
			out = append(out, it.ID)
		}
		return out
	}

	t.Run("meta absent: stops at the first short page", func(t *testing.T) {
		c := serve(t, [][]int{{1, 2, 3}, {4, 5, 6}, {7}}, nil, false)
		items, err := List[item](context.Background(), c, "/things")
		if err != nil || len(items) != 7 || ids(items)[6] != 7 {
			t.Fatalf("items=%v err=%v", ids(items), err)
		}
	})
	t.Run("meta absent: stops at an empty page after full pages", func(t *testing.T) {
		c := serve(t, [][]int{{1, 2}, {3, 4}}, nil, false)
		items, err := List[item](context.Background(), c, "/things")
		if err != nil || len(items) != 4 {
			t.Fatalf("items=%v err=%v", ids(items), err)
		}
	})
	t.Run("total_pages absent but per_page present", func(t *testing.T) {
		c := serve(t, [][]int{{1, 2}, {3}}, func(int) map[string]any { return map[string]any{"per_page": 2} }, false)
		items, err := List[item](context.Background(), c, "/things")
		if err != nil || len(items) != 3 {
			t.Fatalf("items=%v err=%v", ids(items), err)
		}
	})
	t.Run("total_pages present is authoritative", func(t *testing.T) {
		c := serve(t, [][]int{{1}, {2}, {3}}, func(int) map[string]any { return map[string]any{"total_pages": 2, "per_page": 1} }, false)
		items, err := List[item](context.Background(), c, "/things")
		if err != nil || len(items) != 2 {
			t.Fatalf("items=%v err=%v", ids(items), err)
		}
	})
	t.Run("wrapped items", func(t *testing.T) {
		c := serve(t, [][]int{{1, 2}}, func(int) map[string]any { return map[string]any{"total_pages": 1} }, true)
		items, err := ListResources[item](context.Background(), c, "/things", "item")
		if err != nil || len(items) != 2 || items[1].ID != 2 {
			t.Fatalf("items=%v err=%v", ids(items), err)
		}
	})
}

func TestUncodedConflicts(t *testing.T) {
	// A deployment without error codes: every 409 is prose only.
	var posts, patches int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/widgets":
			posts++
			if posts == 1 {
				w.WriteHeader(http.StatusConflict)
				_, _ = fmt.Fprint(w, `{"error":"A request with this Idempotency-Key is still in progress. Retry in a moment to receive its result."}`)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id": 1}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/unique":
			posts++
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprint(w, `{"error":"Name has already been taken"}`)
		case r.Method == http.MethodPatch:
			patches++
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprint(w, `{"error":"This resource was modified by someone else. Re-read it (GET) to get the current lock_version, then retry your write with the new If-Match value."}`)
		}
	}))
	ctx := context.Background()

	// Keyed create + uncoded in-flight prose: retried once, then succeeds.
	var out struct{ ID int64 }
	if err := c.Post(ctx, "/widgets", map[string]any{}, &out, WithIdempotencyKey("k")); err != nil {
		t.Fatalf("keyed create: %v", err)
	}
	if posts != 2 || out.ID != 1 {
		t.Errorf("posts=%d out=%+v", posts, out)
	}

	// Keyed create + uncoded uniqueness 409: retried once at most, then surfaced
	// as a plain conflict — neither retryable forever nor stale.
	posts = 0
	err := c.Post(ctx, "/unique", map[string]any{}, nil, WithIdempotencyKey("k"))
	if err == nil {
		t.Fatal("expected a 409")
	}
	if posts != 2 {
		t.Errorf("posts=%d, want exactly one retry", posts)
	}
	if IsStale(err) {
		t.Error("a uniqueness conflict must not be reported as a stale write")
	}
	apiErr, _ := AsError(err)
	if apiErr.Message != "Name has already been taken" {
		t.Errorf("message lost: %q", apiErr.Message)
	}

	// Update with If-Match + uncoded lock prose: stale, not retried.
	err = c.Patch(ctx, "/widgets/1", map[string]any{}, nil, WithIfMatch(0))
	if !IsStale(err) {
		t.Fatalf("expected stale, got %v", err)
	}
	if patches != 1 {
		t.Errorf("stale 409 retried %d times", patches-1)
	}
}

func TestErrorClassification_uncoded409(t *testing.T) {
	cases := []struct {
		name          string
		e             Error
		wantRetryable bool
		wantStale     bool
	}{
		{"coded in-flight", Error{Status: 409, Code: CodeIdempotencyKeyInFlight}, true, false},
		{"coded stale", Error{Status: 409, Code: CodeStaleObject, Preconditioned: true}, false, true},
		{"uncoded keyed create, generic prose", Error{Status: 409, Idempotent: true, Message: "still in progress"}, true, false},
		{"uncoded keyed create, lock prose", Error{Status: 409, Idempotent: true, Message: "stale lock_version"}, false, true},
		{"uncoded update with If-Match", Error{Status: 409, Preconditioned: true, Message: "conflict"}, false, true},
		{"uncoded delete", Error{Status: 409, Message: "conflict"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.e
			if got := e.Retryable(); got != tc.wantRetryable {
				t.Errorf("Retryable() = %v, want %v", got, tc.wantRetryable)
			}
			if got := IsStale(&e); got != tc.wantStale {
				t.Errorf("IsStale() = %v, want %v", got, tc.wantStale)
			}
		})
	}
}

func TestErrorMessageTruncation(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("x", 10_000)))
	}), WithMaxRetries(0))
	err := c.Get(context.Background(), "/me", nil)
	apiErr, _ := AsError(err)
	if len(apiErr.Message) > maxErrorBody+32 || !strings.HasSuffix(apiErr.Message, "(truncated)") {
		t.Errorf("message length %d: %q…", len(apiErr.Message), apiErr.Message[:40])
	}
}

func TestGatewayErrorsRetryOnlyReplayableRequests(t *testing.T) {
	var calls int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}), WithMaxRetries(2))
	ctx := context.Background()

	// Keyless POST: the first attempt may have been processed; never replayed.
	_ = c.Post(ctx, "/widgets", map[string]any{}, nil)
	if calls != 1 {
		t.Errorf("keyless POST retried: %d calls", calls)
	}
	calls = 0
	_ = c.Post(ctx, "/widgets", map[string]any{}, nil, WithIdempotencyKey("k"))
	if calls != 3 {
		t.Errorf("keyed POST calls = %d, want 1 + 2 retries", calls)
	}
	calls = 0
	_ = c.Patch(ctx, "/widgets/1", map[string]any{}, nil)
	if calls != 1 {
		t.Errorf("PATCH without If-Match retried: %d calls", calls)
	}
}

func TestIsTransient(t *testing.T) {
	if isTransient(context.Canceled) || isTransient(context.DeadlineExceeded) {
		t.Error("context errors must not be transient")
	}
	if !isTransient(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}) {
		t.Error("connection refused should be transient")
	}
	if !isTransient(&url.Error{Op: "Get", Err: &net.OpError{Op: "read", Err: syscall.ECONNRESET}}) {
		t.Error("connection reset should be transient")
	}
	if isTransient(&url.Error{Op: "Get", Err: errors.New("x509: certificate signed by unknown authority")}) {
		t.Error("a TLS failure must not be retried")
	}
	if isTransient(&url.Error{Op: "Get", Err: errors.New("unsupported protocol scheme")}) {
		t.Error("a malformed request must not be retried")
	}
	if !isTransient(&net.DNSError{IsTimeout: true}) || isTransient(&net.DNSError{IsNotFound: true}) {
		t.Error("DNS: only timeouts/temporary failures are transient")
	}
}

// ---- CreateSecretResource -----------------------------------------------------

// keyed is a minimal secret-bearing resource: the create returns the secret
// once, a replay returns the row without it.
type keyed struct {
	ID     int64  `json:"id"`
	Secret string `json:"secret"`
}

func (k *keyed) ResourceID() int64 { return k.ID }
func (k *keyed) secret() string    { return k.Secret }

// keyedServer serves POST /keys with idempotent, secret-redacting replays and
// GET /keys/{id}; discards are recorded rather than performed.
type keyedServer struct {
	mu        sync.Mutex
	nextID    int64
	rows      map[int64]bool
	replays   map[string]int64
	redactAll bool // even a fresh create comes back without its secret
	posts     int
	discarded []int64
}

func newKeyedServer(t *testing.T) (*keyedServer, *Client) {
	t.Helper()
	ks := &keyedServer{nextID: 500, rows: map[int64]bool{}, replays: map[string]int64{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/keys", func(w http.ResponseWriter, r *http.Request) {
		ks.mu.Lock()
		defer ks.mu.Unlock()
		ks.posts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		key := r.Header.Get("Idempotency-Key")
		if id, ok := ks.replays[key]; ok && key != "" {
			_, _ = fmt.Fprintf(w, `{"id": %d, "secret": null, "secret_available": false}`, id)
			return
		}
		ks.nextID++
		ks.rows[ks.nextID] = true
		if key != "" {
			ks.replays[key] = ks.nextID
		}
		if ks.redactAll {
			_, _ = fmt.Fprintf(w, `{"id": %d, "secret": null}`, ks.nextID)
			return
		}
		_, _ = fmt.Fprintf(w, `{"id": %d, "secret": "s3cret-%d"}`, ks.nextID, ks.nextID)
	})
	mux.HandleFunc("GET /api/v1/keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		ks.mu.Lock()
		defer ks.mu.Unlock()
		var id int64
		_, _ = fmt.Sscan(r.PathValue("id"), &id)
		w.Header().Set("Content-Type", "application/json")
		if !ks.rows[id] {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"error":"Not found","code":"not_found"}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"id": %d}`, id)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return ks, c
}

func (ks *keyedServer) discard(_ context.Context, replayed *keyed) error {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.discarded = append(ks.discarded, replayed.ID)
	delete(ks.rows, replayed.ID)
	return nil
}

func (ks *keyedServer) verify(c *Client) Verifier[*keyed] {
	return VerifyByGet(func(ctx context.Context, id int64) (*keyed, error) {
		return GetResource[*keyed](ctx, c, fmt.Sprintf("/keys/%d", id), "key")
	})
}

func TestCreateSecretResource_freshCreateWithSecretIsReturned(t *testing.T) {
	ks, c := newKeyedServer(t)
	ctx := context.Background()
	got, err := CreateSecretResource(ctx, c, "/keys", "key", Fields{}, "k1", ks.verify(c), ks.discard)
	if err != nil || got.Secret != "s3cret-501" || ks.posts != 1 || len(ks.discarded) != 0 {
		t.Fatalf("got=%+v err=%v posts=%d discarded=%v", got, err, ks.posts, ks.discarded)
	}
}

func TestCreateSecretResource_replayIsRetiredAndRecreated(t *testing.T) {
	ks, c := newKeyedServer(t)
	ctx := context.Background()
	first, err := CreateSecretResource(ctx, c, "/keys", "key", Fields{}, "stable", ks.verify(c), ks.discard)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CreateSecretResource(ctx, c, "/keys", "key", Fields{}, "stable", ks.verify(c), ks.discard)
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if second.ID == first.ID || second.Secret == "" {
		t.Fatalf("recreate returned the replay: %+v", second)
	}
	// original + replayed + fresh-key; the replayed row was retired.
	if ks.posts != 3 || len(ks.discarded) != 1 || ks.discarded[0] != first.ID {
		t.Fatalf("posts=%d discarded=%v", ks.posts, ks.discarded)
	}
}

// Branch 1: a fresh create carries its secret but cannot be read back — the
// row is retired (never leave a live credential unrecorded) and no second
// create is attempted.
func TestCreateSecretResource_unverifiableFreshCreateIsRetired(t *testing.T) {
	ks, c := newKeyedServer(t)
	ctx := context.Background()
	unknown := func(context.Context, *keyed) (Verdict, error) { return VerifiedUnknown, nil }
	_, err := CreateSecretResource(ctx, c, "/keys", "key", Fields{}, "k1", unknown, ks.discard)
	if err == nil || !strings.Contains(err.Error(), "revoked and nothing else was created") {
		t.Fatalf("err = %v", err)
	}
	if ks.posts != 1 || len(ks.discarded) != 1 {
		t.Fatalf("posts=%d discarded=%v: exactly one create, retired", ks.posts, ks.discarded)
	}
}

// Branch 2: the replay cannot be retired (discard fails with something other
// than 404) — stop, do not create another.
func TestCreateSecretResource_replayRetireFailureStops(t *testing.T) {
	ks, c := newKeyedServer(t)
	ctx := context.Background()
	if _, err := CreateSecretResource(ctx, c, "/keys", "key", Fields{}, "stable", ks.verify(c), ks.discard); err != nil {
		t.Fatal(err)
	}
	boom := &Error{Status: 403, Code: CodeForbidden, Message: "no"}
	failing := func(context.Context, *keyed) error { return boom }
	_, err := CreateSecretResource(ctx, c, "/keys", "key", Fields{}, "stable", ks.verify(c), failing)
	if err == nil || !strings.Contains(err.Error(), "could not be retired") || !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if ks.posts != 2 {
		t.Fatalf("posts=%d: no fresh-key create after a failed retire", ks.posts)
	}
}

// Branch 3: the fresh-key create also comes back without a secret — an API that
// never returns one; refuse to record it.
func TestCreateSecretResource_secretlessRecreateIsAnError(t *testing.T) {
	ks, c := newKeyedServer(t)
	ctx := context.Background()
	ks.redactAll = true
	_, err := CreateSecretResource(ctx, c, "/keys", "key", Fields{}, "k1", ks.verify(c), ks.discard)
	if err == nil || !strings.Contains(err.Error(), "without its secret on a fresh create") {
		t.Fatalf("err = %v", err)
	}
	if ks.posts != 2 || len(ks.discarded) != 1 {
		t.Fatalf("posts=%d discarded=%v: the first (secretless) row is retired, the second is reported", ks.posts, ks.discarded)
	}
}

// Branch 4: the fresh-key create has a secret but cannot be read back — retire
// it too and report, never a third create.
func TestCreateSecretResource_unverifiableRecreateIsRetired(t *testing.T) {
	ks, c := newKeyedServer(t)
	ctx := context.Background()
	if _, err := CreateSecretResource(ctx, c, "/keys", "key", Fields{}, "stable", ks.verify(c), ks.discard); err != nil {
		t.Fatal(err)
	}
	var verifies int
	flaky := func(ctx context.Context, k *keyed) (Verdict, error) {
		verifies++
		return VerifiedUnknown, nil
	}
	_, err := CreateSecretResource(ctx, c, "/keys", "key", Fields{}, "stable", flaky, ks.discard)
	if err == nil || !strings.Contains(err.Error(), "cannot be read back; it was revoked") {
		t.Fatalf("err = %v", err)
	}
	// original, replayed, fresh-key: three POSTs, both replay and fresh rows retired.
	if ks.posts != 3 || len(ks.discarded) != 2 {
		t.Fatalf("posts=%d discarded=%v", ks.posts, ks.discarded)
	}
}
