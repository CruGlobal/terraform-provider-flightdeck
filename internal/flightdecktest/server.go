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
	"testing"
	"time"
)

// DefaultToken is the personal access token the fake accepts unless changed.
const DefaultToken = "fd_pat_test_token_0123456789"

// User is a workspace member.
type User struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Role  string `json:"role,omitempty"`
}

// RecordedRequest is one request the fake served, kept for assertions.
type RecordedRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
	Status int
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
	// projectHooks run after every project create so the nested-resource
	// fakes can seed a project's defaults (states, labels).
	projectHooks []func(s *Server, p *Project)

	// Fault injection.
	throttleNext   int
	throttleRetry  time.Duration
	inFlightNext   int
	failNext       []int
	beforeRequest  []requestHook
	requests       []RecordedRequest
	workspaceAdmin bool
}

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
		workspaceAdmin: true,
	}
	s.members = []User{
		{ID: 1, Name: "Token Owner", Email: "owner@example.com", Role: "admin"},
		{ID: 2, Name: "Alex Example", Email: "alex@example.com", Role: "member"},
		{ID: 3, Name: "Sam Sample", Email: "sam@example.com", Role: "guest"},
	}
	mux := http.NewServeMux()
	s.routes(mux)
	s.Server = httptest.NewServer(s.middleware(mux))
	t.Cleanup(s.Close)
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

// FailNext queues HTTP statuses to answer the next requests with, in order,
// before normal handling resumes (e.g. 502, 503).
func (s *Server) FailNext(statuses ...int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = append(s.failNext, statuses...)
}

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

// ResetRequests clears the recorded request log.
func (s *Server) ResetRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

// statusRecorder captures the status code for the request log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
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
				Header: r.Header.Clone(), Body: body, Status: rec.status,
			})
			s.mu.Unlock()
		}()

		if !strings.HasPrefix(r.URL.Path, "/api/v1") {
			http.NotFound(rec, r)
			return
		}

		// Fault injection runs before auth, as Rack::Attack does.
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
		if len(s.failNext) > 0 {
			status := s.failNext[0]
			s.failNext = s.failNext[1:]
			s.mu.Unlock()
			rec.Header().Set("Content-Type", "text/html")
			rec.WriteHeader(status)
			_, _ = fmt.Fprintf(rec, "<html>%d</html>", status)
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
		s.mu.Unlock()
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
	mux.HandleFunc("GET /api/v1/workspace_members", s.listWorkspaceMembers)
	for _, hook := range resourceHooks {
		hook(s, mux)
	}
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "Not found")
	})
}

func (s *Server) listWorkspaceMembers(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	members := append([]User(nil), s.members...)
	s.mu.Unlock()
	items := make([]any, 0, len(members))
	for _, m := range members {
		items = append(items, map[string]any{"id": m.ID, "name": m.Name, "email": m.Email, "role": m.Role})
	}
	writeCollection(w, r, items)
}

// --- envelope helpers -------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": message, "code": code})
}

// writeCollection applies Api::Pagination semantics (default 50, max 100) and
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
