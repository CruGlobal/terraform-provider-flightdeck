// Package flightdecktest is an in-memory fake of the Flightdeck /api/v1 surface
// the provider manages. It encodes the API contract — bearer auth, the
// {results, meta} collection envelope, the {error, code} error envelope,
// Idempotency-Key replay, If-Match / lock_version preconditions, 202 on project
// delete, 429 throttling — so the provider can be exercised end to end through
// Terraform without a live deployment. Anything the fake accepts that the real
// API would reject is a bug in the fake, not a feature.
package flightdecktest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// DefaultToken is the personal access token the fake accepts unless changed.
const DefaultToken = "fd_pat_test_token_0123456789"

// User is a workspace member. Kind is "human" or "service" (empty reads as
// human); a service account is visible in the directory only to a workspace
// admin.
type User struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Role  string `json:"role,omitempty"`
	Kind  string `json:"kind,omitempty"`
}

// RecordedRequest is one request the fake served, kept for assertions. Body is
// the request body; Response is the body the fake answered with.
type RecordedRequest struct {
	Method   string
	Path     string
	Query    string
	Header   http.Header
	Body     []byte
	Status   int
	Response []byte
}

// Server is the fake. All exported fields are safe to read only after the
// server is stopped or while no test traffic is in flight; use the accessor
// methods otherwise.
type Server struct {
	*httptest.Server

	mu     sync.Mutex
	token  string
	nextID int64

	// Workspace-level fixtures.
	members []User

	// Resource stores live in their own files alongside their handlers and
	// register themselves through registerResource.
	stores     map[string]any
	idempotent map[string]idempotentResponse
	// reservedKeys are the idempotency keys whose create is running. Like
	// the API, a second request with one is 409 idempotency_key_in_flight
	// rather than a second create.
	reservedKeys map[string]bool
	// projectHooks run after every project create so the nested-resource
	// fakes can seed a project's defaults (states, labels).
	projectHooks []func(s *Server, p *Project)

	// Fault injection.
	throttleNext   int
	throttleRetry  time.Duration
	inFlightNext   int
	beforeRequest  []requestHook
	requests       []RecordedRequest
	workspaceAdmin bool
	// omitCodes drops the `code` field from every error body, like an older
	// deployment that only sent the prose message. Atomic because the envelope
	// helpers consult it while handlers hold mu.
	omitCodes atomic.Bool
	// legacySecretReplays replays a secret-bearing create without its secret
	// even while the record it made is live, as Flightdeck did before it
	// started refusing such replays.
	legacySecretReplays bool
	// dropResponses are requests to serve and then answer with nothing: the
	// connection is closed instead, as if the response were lost on the way.
	dropResponses []requestHook
	// refusals are requests to answer with a canned error instead of serving
	// them, so nothing they ask for happens.
	refusals []refusal
	// slowResponses are requests to serve at once and then answer late.
	slowResponses []slowResponse
}

// slowResponse holds back the answer to the next n requests matching it.
type slowResponse struct {
	method, pathSuffix string
	delay              time.Duration
	n                  int
}

// refusal is a one-shot canned error for the next request matching it.
type refusal struct {
	method, pathSuffix string
	status             int
	code, message      string
}

// The API's answers to a request that lost a race to a DELETE: a write that
// names something deleted at the same moment, and a DELETE that another
// request added something to while it was being deleted. Both are 409
// stale_object, and nothing was written.
const (
	LostRaceWriteMessage  = "Something this request refers to was deleted while it was being saved, so nothing was saved. Re-read it and try again."
	LostRaceDeleteMessage = "This changed while it was being deleted, so it was not deleted. Send the DELETE again."
)

// requestHook runs once, just before the first request matching method + path
// is handled — the seam for "someone else wrote in between plan and apply".
type requestHook struct {
	method, path string
	fn           func()
}

// resourceHook wires one resource family (store + routes) into a Server. Each
// resource file registers one via registerResource in an init function, so
// adding a resource never touches this file.
type resourceHook func(s *Server, mux *http.ServeMux)

var resourceHooks []resourceHook

// New starts a fake bound to a random local port and stops it when the test
// ends. It seeds one workspace member (id 1) who owns the token.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		token:          DefaultToken,
		nextID:         1000,
		stores:         map[string]any{},
		idempotent:     map[string]idempotentResponse{},
		reservedKeys:   map[string]bool{},
		workspaceAdmin: true,
	}
	s.members = []User{
		{ID: 1, Name: "Token Owner", Email: "owner@example.com", Role: "admin", Kind: KindHuman},
		{ID: 2, Name: "Alex Example", Email: "alex@example.com", Role: "member", Kind: KindHuman},
		{ID: 3, Name: "Sam Sample", Email: "sam@example.com", Role: "guest", Kind: KindHuman},
		{ID: 4, Name: "Deploy Bot", Email: "deploy-bot@example.com", Role: "member", Kind: KindService},
	}
	mux := http.NewServeMux()
	s.routes(mux)
	s.Server = httptest.NewServer(s.middleware(mux))
	currentServer = s
	t.Cleanup(func() {
		s.Close()
		if currentServer == s {
			currentServer = nil
		}
	})
	return s
}

// Token returns the bearer token the fake accepts.
func (s *Server) Token() string { return s.token }

// Members returns the seeded workspace members.
func (s *Server) Members() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]User(nil), s.members...)
}

// ThrottleNext makes the next n requests answer 429 with the given Retry-After.
func (s *Server) ThrottleNext(n int, retryAfter time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.throttleNext = n
	s.throttleRetry = retryAfter
}

// InFlightNext makes the next n creates carrying an Idempotency-Key answer 409
// idempotency_key_in_flight, as the real API does while the original request
// holding that key is still running.
func (s *Server) InFlightNext(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlightNext = n
}

// OnNextRequest runs fn once, immediately before the next request with the
// given method and exact path is handled (after fault injection and auth).
func (s *Server) OnNextRequest(method, path string, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beforeRequest = append(s.beforeRequest, requestHook{method: method, path: path, fn: fn})
}

// LegacySecretReplays makes a replayed create of a routing key, ingestion
// token or webhook come back as a 201 without its secret even while the
// record it made is live, the way Flightdeck answered before it refused such
// replays with 409 idempotency_replay_withheld.
func (s *Server) LegacySecretReplays(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.legacySecretReplays = on
}

// DropNextResponse makes the next request with the given method whose path
// ends in pathSuffix take effect as usual, and then lose its response: the
// fake closes the connection instead of answering, the way a timeout or a
// dropped connection loses an answer the server did send.
func (s *Server) DropNextResponse(method, pathSuffix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropResponses = append(s.dropResponses, requestHook{method: method, path: pathSuffix})
}

// RefuseNext makes the next request with the given method whose path ends in
// pathSuffix answer the given error without being served, so nothing it asks
// for happens. It runs after authentication and any OnNextRequest hook.
func (s *Server) RefuseNext(method, pathSuffix string, status int, code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refusals = append(s.refusals, refusal{method: method, pathSuffix: pathSuffix, status: status, code: code, message: message})
}

// DelayResponses makes the next n requests with the given method whose path
// ends in pathSuffix take effect at once, as usual, and then hold their answer
// back for delay before sending it: a server that did the work and answered
// slowly. A client that gives up first sees a timeout for a request the
// server did receive. The answer is dropped if the client has gone.
func (s *Server) DelayResponses(method, pathSuffix string, delay time.Duration, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slowResponses = append(s.slowResponses, slowResponse{method: method, pathSuffix: pathSuffix, delay: delay, n: n})
}

// OmitErrorCodes makes every error body prose-only (no `code`), like a
// deployment that predates machine-readable codes.
func (s *Server) OmitErrorCodes(on bool) { s.omitCodes.Store(on) }

// SetWorkspaceAdmin controls whether the token's user is treated as a
// workspace admin (required for webhooks and the self-healing block).
func (s *Server) SetWorkspaceAdmin(admin bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workspaceAdmin = admin
}

// Requests returns every request served so far.
func (s *Server) Requests() []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RecordedRequest(nil), s.requests...)
}

// RequestsMatching returns the requests with the given method whose path has
// the given prefix.
func (s *Server) RequestsMatching(method, pathPrefix string) []RecordedRequest {
	var out []RecordedRequest
	for _, r := range s.Requests() {
		if r.Method == method && strings.HasPrefix(r.Path, pathPrefix) {
			out = append(out, r)
		}
	}
	return out
}

// discardWriter is a ResponseWriter that keeps nothing, for a response the
// fake is about to lose on purpose.
type discardWriter struct{ header http.Header }

func (d discardWriter) Header() http.Header         { return d.header }
func (d discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d discardWriter) WriteHeader(int)             {}

// statusRecorder captures the status code and body for the request log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			s.mu.Lock()
			s.requests = append(s.requests, RecordedRequest{
				Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
				Header: r.Header.Clone(), Body: body, Status: rec.status, Response: rec.body.Bytes(),
			})
			s.mu.Unlock()
		}()

		if !strings.HasPrefix(r.URL.Path, "/api/v1") {
			http.NotFound(rec, r)
			return
		}

		// A GET or HEAD with a body is refused before anything else looks at
		// the request, the token included. An empty body is fine. Every
		// provider test runs through this, so a read that sends a body fails.
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && len(body) > 0 {
			writeError(rec, http.StatusBadRequest, "body_not_allowed",
				"A GET or HEAD request can't have a body. Send filters in the query string.")
			return
		}

		// Fault injection runs before auth, as the real throttle does.
		s.mu.Lock()
		if s.throttleNext > 0 {
			s.throttleNext--
			retry := s.throttleRetry
			s.mu.Unlock()
			if retry > 0 {
				rec.Header().Set("Retry-After", strconv.Itoa(int(retry/time.Second)))
			}
			writeError(rec, http.StatusTooManyRequests, "rate_limited",
				fmt.Sprintf("Rate limit exceeded. Retry in %d seconds.", int(retry/time.Second)))
			return
		}
		token := s.token
		s.mu.Unlock()

		if r.Header.Get("Authorization") != "Bearer "+token {
			writeError(rec, http.StatusUnauthorized, "unauthorized", "Invalid or missing API token")
			return
		}
		if r.Header.Get("Accept") != "" && !strings.Contains(r.Header.Get("Accept"), "json") && !strings.Contains(r.Header.Get("Accept"), "*/*") {
			writeError(rec, http.StatusNotAcceptable, "not_acceptable", "JSON only")
			return
		}
		s.mu.Lock()
		for i, h := range s.beforeRequest {
			if h.method == r.Method && h.path == r.URL.Path {
				s.beforeRequest = append(s.beforeRequest[:i], s.beforeRequest[i+1:]...)
				s.mu.Unlock()
				h.fn()
				s.mu.Lock()
				break
			}
		}
		var refused *refusal
		for i, rf := range s.refusals {
			if rf.method == r.Method && strings.HasSuffix(r.URL.Path, rf.pathSuffix) {
				s.refusals = append(s.refusals[:i], s.refusals[i+1:]...)
				refused = &rf
				break
			}
		}
		if refused != nil {
			s.mu.Unlock()
			writeError(rec, refused.status, refused.code, refused.message)
			return
		}
		drop := false
		for i, h := range s.dropResponses {
			if h.method == r.Method && strings.HasSuffix(r.URL.Path, h.path) {
				s.dropResponses = append(s.dropResponses[:i], s.dropResponses[i+1:]...)
				drop = true
				break
			}
		}
		var delay time.Duration
		for i := range s.slowResponses {
			if sr := &s.slowResponses[i]; !drop && sr.n > 0 && sr.method == r.Method && strings.HasSuffix(r.URL.Path, sr.pathSuffix) {
				sr.n--
				delay = sr.delay
				break
			}
		}
		s.mu.Unlock()
		if delay > 0 {
			// Serve now, so the work is done whatever happens to the answer,
			// then hold the answer back.
			held := &statusRecorder{ResponseWriter: discardWriter{header: http.Header{}}, status: http.StatusOK}
			next.ServeHTTP(held, r)
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				// The client gave up; record what the server did and stop.
				rec.status = held.status
				rec.body.Write(held.body.Bytes())
				return
			}
			for k, v := range held.Header() {
				rec.Header()[k] = v
			}
			rec.WriteHeader(held.status)
			_, _ = rec.Write(held.body.Bytes())
			return
		}
		if drop {
			// Serve into a recorder the client never sees, then hang up.
			lost := &statusRecorder{ResponseWriter: discardWriter{header: http.Header{}}, status: http.StatusOK}
			next.ServeHTTP(lost, r)
			rec.status = lost.status
			rec.body.Write(lost.body.Bytes())
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		next.ServeHTTP(rec, r)
	})
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		me := s.members[0]
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"id": me.ID, "name": me.Name, "email": me.Email, "workspace_id": 1})
	})
	for _, hook := range resourceHooks {
		hook(s, mux)
	}
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "Not found")
	})
}

// --- envelope helpers -------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	if code == "" || (currentServer != nil && currentServer.codesOmitted()) {
		writeJSON(w, status, map[string]any{"error": message})
		return
	}
	writeJSON(w, status, map[string]any{"error": message, "code": code})
}

// currentServer is the fake serving the request in flight. Tests start one
// fake at a time per process (the harness runs resource tests serially), so a
// package-level handle is enough for the envelope helper to consult knobs.
var currentServer *Server

func (s *Server) codesOmitted() bool { return s.omitCodes.Load() }

// writeCollection applies the API's pagination (default 50, max 100) and
// wraps the page in {results, meta}.
func writeCollection(w http.ResponseWriter, r *http.Request, items []any) {
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage <= 0 {
		perPage = 50
	}
	if perPage > 100 {
		perPage = 100
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	total := len(items)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	totalPages := (total + perPage - 1) / perPage
	writeJSON(w, http.StatusOK, map[string]any{
		"results": items[start:end],
		"meta":    map[string]any{"count": total, "page": page, "per_page": perPage, "total_pages": totalPages},
	})
}
