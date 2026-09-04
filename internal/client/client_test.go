package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.Handler, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "fd_pat_test", opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func TestNew_NormalisesEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://flightdeck.example.com":         "https://flightdeck.example.com/api/v1",
		"https://flightdeck.example.com/":        "https://flightdeck.example.com/api/v1",
		"https://flightdeck.example.com/api/v1":  "https://flightdeck.example.com/api/v1",
		"https://flightdeck.example.com/api/v1/": "https://flightdeck.example.com/api/v1",
		"http://localhost:3000?x=1#frag":         "http://localhost:3000/api/v1",
	}
	for in, want := range cases {
		c, err := New(in, "tok")
		if err != nil {
			t.Fatalf("New(%q): %v", in, err)
		}
		if got := c.BaseURL(); got != want {
			t.Errorf("New(%q).BaseURL() = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "ftp://x", "flightdeck.example.com", "https://"} {
		if _, err := New(bad, "tok"); err == nil {
			t.Errorf("New(%q) succeeded, want error", bad)
		}
	}
	if _, err := New("https://flightdeck.example.com", ""); err == nil {
		t.Error("New with empty token succeeded, want error")
	}
}

func TestNew_TrimsTokenWhitespace(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "fd_pat_pasted\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), "/me", nil); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer fd_pat_pasted" {
		t.Errorf("Authorization = %q; a pasted trailing newline must not reach the API", got)
	}
}

func TestDo_SendsAuthAndHeaders(t *testing.T) {
	var got *http.Request
	var body []byte
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		body = make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id": 7, "lock_version": 3}`)
	}))

	var out struct {
		ID          int64 `json:"id"`
		LockVersion int64 `json:"lock_version"`
	}
	err := c.Patch(context.Background(), "/projects/7", map[string]any{"project": map[string]any{"name": "x"}}, &out,
		WithIfMatch(2), WithIdempotencyKey("abc"))
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if out.ID != 7 || out.LockVersion != 3 {
		t.Errorf("decoded %+v", out)
	}
	if got.URL.Path != "/api/v1/projects/7" {
		t.Errorf("path = %q", got.URL.Path)
	}
	if h := got.Header.Get("Authorization"); h != "Bearer fd_pat_test" {
		t.Errorf("Authorization = %q", h)
	}
	if h := got.Header.Get("If-Match"); h != `"2"` {
		t.Errorf("If-Match = %q, want quoted lock_version", h)
	}
	if h := got.Header.Get("Idempotency-Key"); h != "abc" {
		t.Errorf("Idempotency-Key = %q", h)
	}
	if h := got.Header.Get("Content-Type"); h != "application/json" {
		t.Errorf("Content-Type = %q", h)
	}
	if h := got.Header.Get("User-Agent"); h != DefaultUserAgent {
		t.Errorf("User-Agent = %q", h)
	}
	if string(body) != `{"project":{"name":"x"}}` {
		t.Errorf("body = %s", body)
	}
}

func TestDo_ErrorEnvelope(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/404":
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"error":"Not found","code":"not_found"}`)
		case "/api/v1/projects/409":
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprint(w, `{"error":"This resource was modified by someone else.","code":"stale_object"}`)
		case "/api/v1/projects/422":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = fmt.Fprint(w, `{"error":"Identifier has already been taken","code":"validation_failed"}`)
		case "/api/v1/projects/legacy":
			// Pre-`code` envelope: prose only.
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"error":"Your project role does not permit this action"}`)
		case "/api/v1/projects/html":
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprint(w, `<html>bad gateway</html>`)
		}
	}), WithMaxRetries(0))

	ctx := context.Background()
	err := c.Get(ctx, "/projects/404", nil)
	if !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != CodeNotFound || apiErr.Status != 404 {
		t.Errorf("error = %+v", apiErr)
	}
	if want := "GET /projects/404: HTTP 404 (not_found): Not found"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}

	err = c.Patch(ctx, "/projects/409", map[string]any{}, nil)
	if !IsStale(err) {
		t.Errorf("expected stale, got %v", err)
	}
	if IsNotFound(err) {
		t.Error("stale reported as not found")
	}

	err = c.Post(ctx, "/projects/422", map[string]any{}, nil)
	if !IsValidation(err) {
		t.Errorf("expected validation, got %v", err)
	}
	errors.As(err, &apiErr)
	if apiErr.Retryable() {
		t.Error("422 must never be retryable")
	}

	err = c.Get(ctx, "/projects/legacy", nil)
	if !IsForbidden(err) {
		t.Errorf("expected forbidden, got %v", err)
	}
	errors.As(err, &apiErr)
	if apiErr.Code != "" || apiErr.Message != "Your project role does not permit this action" {
		t.Errorf("legacy envelope parsed as %+v", apiErr)
	}

	err = c.Get(ctx, "/projects/html", nil)
	errors.As(err, &apiErr)
	if apiErr.Status != 502 || apiErr.Message != "<html>bad gateway</html>" {
		t.Errorf("html error parsed as %+v", apiErr)
	}
}

func TestDo_RetriesOn429HonouringRetryAfter(t *testing.T) {
	var calls atomic.Int32
	var slept []time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n <= 2 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprint(w, `{"error":"Rate limit exceeded. Retry in 3 seconds.","code":"rate_limited"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}

	var out map[string]bool
	if err := c.Get(context.Background(), "/me", &out); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
	if len(slept) != 2 || slept[0] != 3*time.Second || slept[1] != 3*time.Second {
		t.Errorf("slept = %v, want two 3s waits from Retry-After", slept)
	}
}

func TestDo_GivesUpAfterMaxRetries(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"error":"slow down","code":"rate_limited"}`)
	}), WithMaxRetries(2))

	err := c.Get(context.Background(), "/me", nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != 429 || apiErr.Code != CodeRateLimited {
		t.Fatalf("expected final 429, got %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 1 + 2 retries", calls.Load())
	}
}

func TestDo_RetriesInFlightIdempotencyConflictButNotStale(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects":
			if n == 1 {
				w.WriteHeader(http.StatusConflict)
				_, _ = fmt.Fprint(w, `{"error":"A request with this Idempotency-Key is still in progress.","code":"idempotency_key_in_flight"}`)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":1}`)
		default:
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprint(w, `{"error":"modified","code":"stale_object"}`)
		}
	}))

	var out struct{ ID int64 }
	if err := c.Post(context.Background(), "/projects", map[string]any{}, &out, WithIdempotencyKey("k")); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if out.ID != 1 || calls.Load() != 2 {
		t.Errorf("out=%+v calls=%d", out, calls.Load())
	}

	calls.Store(0)
	err := c.Patch(context.Background(), "/projects/1", map[string]any{}, nil, WithIfMatch(0))
	if !IsStale(err) {
		t.Fatalf("expected stale, got %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("stale 409 was retried %d times; it must not be", calls.Load()-1)
	}
}

func TestDo_RetriesGatewayErrors(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{}`)
	}))
	if err := c.Delete(context.Background(), "/projects/1", nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d", calls.Load())
	}
}

func TestDo_ContextCancellationStopsRetrying(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.Get(ctx, "/me", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestList_PaginatesFully(t *testing.T) {
	type item struct {
		ID int `json:"id"`
	}
	const total = 250
	var pagesSeen []string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		pagesSeen = append(pagesSeen, q.Get("page")+"/"+q.Get("per_page"))
		page, _ := strconv.Atoi(q.Get("page"))
		perPage, _ := strconv.Atoi(q.Get("per_page"))
		if perPage > 100 {
			perPage = 100
		}
		start := (page - 1) * perPage
		end := start + perPage
		if end > total {
			end = total
		}
		results := []item{}
		for i := start; i < end; i++ {
			results = append(results, item{ID: i + 1})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": results,
			"meta":    map[string]int{"count": total, "page": page, "per_page": perPage, "total_pages": (total + perPage - 1) / perPage},
		})
	}))

	items, err := List[item](context.Background(), c, "/projects/1/states")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != total || items[0].ID != 1 || items[total-1].ID != total {
		t.Errorf("got %d items, first %+v last %+v", len(items), items[0], items[len(items)-1])
	}
	if len(pagesSeen) != 3 || pagesSeen[0] != "1/100" || pagesSeen[2] != "3/100" {
		t.Errorf("pages requested = %v", pagesSeen)
	}
}

func TestList_EmptyCollection(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"results":[],"meta":{"count":0,"page":1,"per_page":100,"total_pages":0}}`)
	}))
	items, err := List[map[string]any](context.Background(), c, "/webhooks")
	if err != nil || len(items) != 0 {
		t.Fatalf("items=%v err=%v", items, err)
	}
}

func TestIdempotencyKey_IsStableAndDistinct(t *testing.T) {
	a := IdempotencyKey("project", "WEB")
	b := IdempotencyKey("project", "WEB")
	c := IdempotencyKey("project", "API")
	d := IdempotencyKey("state", "1", "Done")
	if a != b {
		t.Errorf("same inputs gave different keys: %s vs %s", a, b)
	}
	if a == c || a == d {
		t.Errorf("different inputs collided: %s %s %s", a, c, d)
	}
	if len(a) != 43 || a[:3] != "tf-" {
		t.Errorf("unexpected key shape %q", a)
	}
	r1, r2 := RandomIdempotencyKey(), RandomIdempotencyKey()
	if r1 == r2 {
		t.Error("random keys collided")
	}

	p1 := PayloadKey("project", "", Fields{"name": "A", "identifier": "APP"})
	p2 := PayloadKey("project", "", Fields{"identifier": "APP", "name": "A"})
	p3 := PayloadKey("project", "", Fields{"name": "B", "identifier": "APP"})
	p4 := PayloadKey("state", "12", Fields{"name": "A", "identifier": "APP"})
	if p1 != p2 {
		t.Errorf("key order must not matter: %s vs %s", p1, p2)
	}
	if p1 == p3 || p1 == p4 {
		t.Errorf("different payload/scope collided: %s %s %s", p1, p3, p4)
	}
}
