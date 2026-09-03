package flightdecktest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func registerResource(h resourceHook) { resourceHooks = append(resourceHooks, h) }

func (s *Server) id() int64 {
	s.nextID++
	return s.nextID
}

// --- error envelopes ---------------------------------------------------------

func notFound(w http.ResponseWriter) { writeError(w, http.StatusNotFound, "not_found", "Not found") }

func staleObject(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "stale_object",
		"This resource was modified by someone else. Re-read it (GET) to get the current lock_version, then retry your write with the new If-Match value.")
}

// --- request helpers ---------------------------------------------------------

// decodeBody reads the JSON body and returns the object under root (Rails
// wraps params under the resource name). A missing root is a 400 ParameterMissing.
func decodeBody(w http.ResponseWriter, r *http.Request, root string) (map[string]any, bool) {
	var envelope map[string]any
	body, _ := io.ReadAll(r.Body)
	if len(bytes.TrimSpace(body)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&envelope); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "malformed JSON body")
			return nil, false
		}
	}
	inner, ok := envelope[root].(map[string]any)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "param is missing or the value is empty: "+root)
		return nil, false
	}
	return inner, true
}

// pathID parses a numeric path segment; a non-numeric id is a 404 like Rails' find.
func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		notFound(w)
		return 0, false
	}
	return id, true
}

// checkIfMatch enforces the If-Match precondition: absent means last-writer-wins,
// a non-integer is ignored, and a mismatch is a 409 stale_object.
func checkIfMatch(w http.ResponseWriter, r *http.Request, current int64) bool {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		return true
	}
	raw = strings.Trim(raw, `"`)
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return true
	}
	if v != current {
		staleObject(w)
		return false
	}
	return true
}

type idempotentResponse struct {
	status      int
	body        []byte
	fingerprint string
}

// fingerprintOf mirrors IdempotentRequests#idempotency_fingerprint: a hash of
// the submitted attributes, stringified and sorted.
func fingerprintOf(attrs map[string]any) string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(asString(attrs[k]))
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// withIdempotency wraps a create. The same Idempotency-Key replays the stored
// 2xx verbatim; only successful responses are remembered. Keys are scoped per
// endpoint, as IdempotentRequests does.
func (s *Server) withIdempotency(w http.ResponseWriter, r *http.Request, scope string, create func() (int, any)) {
	s.idempotently(w, r, scope, "", func() (int, any, any) {
		status, body := create()
		return status, body, nil
	})
}

// withIdempotencyFingerprint is withIdempotency for a create that opts into
// body fingerprinting (FD-794): the same key with different attributes is a
// 409 idempotency_key_reused instead of a replay.
func (s *Server) withIdempotencyFingerprint(w http.ResponseWriter, r *http.Request, scope, fingerprint string, create func() (int, any)) {
	s.idempotently(w, r, scope, fingerprint, func() (int, any, any) {
		status, body := create()
		return status, body, nil
	})
}

// withIdempotencyRedacted is withIdempotency for creates whose response carries
// a secret: create returns the body to render AND the body to cache for
// replays (nil caches the rendered body). Mirrors cache_idempotent_response_as.
func (s *Server) withIdempotencyRedacted(w http.ResponseWriter, r *http.Request, scope string, create func() (int, any, any)) {
	s.idempotently(w, r, scope, "", create)
}

// idempotently is the shared implementation: an optional attribute fingerprint
// (empty opts out) and an optional redacted body to cache (nil caches the
// rendered body).
func (s *Server) idempotently(w http.ResponseWriter, r *http.Request, scope, fingerprint string, create func() (int, any, any)) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 255 {
		status, body, _ := create()
		writeJSON(w, status, body)
		return
	}
	cacheKey := scope + "/" + key
	s.mu.Lock()
	if stored, ok := s.idempotent[cacheKey]; ok {
		s.mu.Unlock()
		if fingerprint != "" && stored.fingerprint != "" && stored.fingerprint != fingerprint {
			writeError(w, http.StatusConflict, "idempotency_key_reused",
				"This Idempotency-Key was already used for a create with different attributes. Send the original attributes to replay it, or a new key to create a separate resource.")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Idempotent-Replayed", "true")
		w.WriteHeader(stored.status)
		_, _ = w.Write(stored.body)
		return
	}
	if s.inFlightNext > 0 {
		s.inFlightNext--
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "idempotency_key_in_flight",
			"A request with this Idempotency-Key is still in progress. Retry in a moment to receive its result.")
		return
	}
	s.mu.Unlock()

	status, body, cached := create()
	encoded, _ := json.Marshal(body)
	if status >= 200 && status < 300 {
		toCache := encoded
		if cached != nil {
			toCache, _ = json.Marshal(cached)
		}
		s.mu.Lock()
		s.idempotent[cacheKey] = idempotentResponse{status: status, body: toCache, fingerprint: fingerprint}
		s.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

// --- value coercion (Rails-style permissive params) ---------------------------

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1" || t == "on"
	case json.Number:
		return t.String() != "0"
	case float64:
		return t != 0
	}
	return false
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func iso(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func asInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case int:
		return int64(t), true
	case json.Number:
		i, err := t.Int64()
		return i, err == nil
	case string:
		i, err := strconv.ParseInt(t, 10, 64)
		return i, err == nil
	}
	return 0, false
}

func asFloat64(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	}
	return 0, false
}
