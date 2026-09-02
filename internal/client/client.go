// Package client is a small, hand-written REST client for the Flightdeck
// /api/v1 surface. It encodes the API's cross-cutting contract in one place so
// every resource gets identical behaviour:
//
//   - bearer-token auth (`Authorization: Bearer fd_pat_…`), JSON in and out;
//   - the `{"results": [...], "meta": {...}}` collection envelope with full
//     pagination (see List and ListResources);
//   - the `{"error": "<prose>", "code": "<slug>"}` error envelope, surfaced as
//     *Error so callers can branch on status and code rather than prose;
//   - `Idempotency-Key` on creates and `If-Match` on updates;
//   - client-side backoff on 429 (honouring Retry-After), on the 409
//     "idempotency key in flight" replay window, and on transient 5xx/network
//     failures — but only for requests that are safe to replay.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// APIPath is the versioned base path every endpoint hangs off.
const APIPath = "/api/v1"

// DefaultUserAgent identifies the provider in Flightdeck's request logs.
const DefaultUserAgent = "terraform-provider-flightdeck"

// Defaults for the retry policy. The server throttles at 120 requests/min per
// token, so a burst of parallel Terraform operations can legitimately trip it;
// the policy is generous enough to ride out a full window without failing the
// apply, and bounded so a misbehaving server cannot hang a plan forever.
const (
	DefaultMaxRetries     = 6
	DefaultInitialBackoff = 500 * time.Millisecond
	DefaultMaxBackoff     = 30 * time.Second
	DefaultTimeout        = 60 * time.Second
)

// maxPages bounds a listing so a server that never reports a last page cannot
// spin the provider forever.
const maxPages = 10_000

// maxErrorBody bounds how much of a non-JSON error body is kept in Error.Message.
const maxErrorBody = 512

// Client talks to one Flightdeck deployment as one personal access token. A
// token is bound to a single workspace, so the client is implicitly
// single-workspace too.
type Client struct {
	baseURL        *url.URL
	token          string
	httpClient     *http.Client
	userAgent      string
	maxRetries     int
	initialBackoff time.Duration
	maxBackoff     time.Duration

	// sleep is swapped out by tests so retry timing is deterministic.
	sleep func(context.Context, time.Duration) error
}

// Option customises a Client at construction.
type Option func(*Client)

// WithHTTPClient replaces the underlying *http.Client (timeouts, TLS, proxies).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.httpClient = h }
}

// WithUserAgent overrides the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.userAgent = ua }
}

// WithMaxRetries bounds how many times a retryable failure is retried.
func WithMaxRetries(n int) Option {
	return func(c *Client) { c.maxRetries = n }
}

// WithBackoff sets the exponential backoff bounds used when the server does
// not supply a Retry-After.
func WithBackoff(initial, maxBackoff time.Duration) Option {
	return func(c *Client) {
		c.initialBackoff = initial
		c.maxBackoff = maxBackoff
	}
}

// New builds a client for the Flightdeck deployment at endpoint (scheme + host,
// optionally a path prefix; the /api/v1 segment is appended here) using the
// given personal access token.
func New(endpoint, token string, opts ...Option) (*Client, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, errors.New("endpoint must not be empty")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("token must not be empty")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint %q: %w", endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid endpoint %q: scheme must be http or https", endpoint)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid endpoint %q: missing host", endpoint)
	}
	u.RawQuery = ""
	u.Fragment = ""
	// Tolerate an endpoint that already carries /api/v1 (or a trailing slash) so
	// copy-pasting the URL out of the app's API docs works either way.
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), APIPath) + APIPath

	c := &Client{
		baseURL:        u,
		token:          token,
		httpClient:     &http.Client{Timeout: DefaultTimeout},
		userAgent:      DefaultUserAgent,
		maxRetries:     DefaultMaxRetries,
		initialBackoff: DefaultInitialBackoff,
		maxBackoff:     DefaultMaxBackoff,
		sleep:          sleepCtx,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// BaseURL returns the resolved API base (endpoint + /api/v1).
func (c *Client) BaseURL() string { return c.baseURL.String() }

// Fields is a partial write body: only the keys present are sent, so an
// omitted key leaves the server-side attribute untouched (the API is
// PATCH-style on both create and update). A key set to nil sends JSON null,
// which the API treats as "clear".
type Fields map[string]any

// RequestOption tunes a single request.
type RequestOption func(*request)

type request struct {
	query          url.Values
	idempotencyKey string
	ifMatch        *int64
	// replayable marks a request the server can safely see twice: GET and
	// DELETE always, POST only under an Idempotency-Key, PATCH only under an
	// If-Match precondition. Only replayable requests are retried after a
	// dropped connection or a gateway 5xx, where the first attempt may have
	// been processed.
	replayable bool
}

// WithQuery adds query-string parameters.
func WithQuery(q url.Values) RequestOption {
	return func(r *request) {
		if r.query == nil {
			r.query = url.Values{}
		}
		for k, vs := range q {
			for _, v := range vs {
				r.query.Add(k, v)
			}
		}
	}
}

// WithIdempotencyKey sends an Idempotency-Key header. The server replays the
// original 2xx for the same key within a 24-hour window instead of creating a
// duplicate, so a retried create is safe.
func WithIdempotencyKey(key string) RequestOption {
	return func(r *request) {
		r.idempotencyKey = key
		r.replayable = key != ""
	}
}

// WithIfMatch sends the resource's lock_version as an If-Match precondition.
// The server answers 409 (code stale_object) if the resource moved on.
func WithIfMatch(lockVersion int64) RequestOption {
	return func(r *request) {
		v := lockVersion
		r.ifMatch = &v
		r.replayable = true
	}
}

// Get performs a GET and decodes the JSON body into out (which may be nil).
func (c *Client) Get(ctx context.Context, path string, out any, opts ...RequestOption) error {
	return c.do(ctx, http.MethodGet, path, nil, out, opts...)
}

// Post performs a POST with a JSON body and decodes the response into out.
func (c *Client) Post(ctx context.Context, path string, body, out any, opts ...RequestOption) error {
	return c.do(ctx, http.MethodPost, path, body, out, opts...)
}

// Patch performs a PATCH with a JSON body and decodes the response into out.
func (c *Client) Patch(ctx context.Context, path string, body, out any, opts ...RequestOption) error {
	return c.do(ctx, http.MethodPatch, path, body, out, opts...)
}

// Delete performs a DELETE. A 2xx of any flavour (200, 202 Accepted for an
// asynchronous teardown, 204) is success; out receives the body when there is
// one and out is non-nil.
func (c *Client) Delete(ctx context.Context, path string, out any, opts ...RequestOption) error {
	return c.do(ctx, http.MethodDelete, path, nil, out, opts...)
}

// Meta is the pagination block on every collection response.
type Meta struct {
	Count      int `json:"count"`
	Page       int `json:"page"`
	PerPage    int `json:"per_page"`
	TotalPages int `json:"total_pages"`
}

// Collection is the raw {results, meta} envelope.
type Collection struct {
	Results []json.RawMessage `json:"results"`
	Meta    *Meta             `json:"meta"`
}

// MaxPerPage is the server's cap on per_page; List always asks for it.
const MaxPerPage = 100

// List walks every page of a collection endpoint, decoding each element into
// T. It asks for the maximum page size and stops at the first of: the page
// meta.total_pages reports as last, an empty page, or (when the meta block or
// its total_pages is missing) a page shorter than the page size the server is
// actually using. A caller therefore never sees a partial listing, and a
// server that never reports a last page cannot spin it forever.
func List[T any](ctx context.Context, c *Client, path string, opts ...RequestOption) ([]T, error) {
	return ListResources[T](ctx, c, path, "", opts...)
}

// ListResources is List for a collection whose elements may each be wrapped
// in rootKey (`{"project": {...}}`) rather than flat; see DecodeResource.
func ListResources[T any](ctx context.Context, c *Client, path, rootKey string, opts ...RequestOption) ([]T, error) {
	var all []T
	pageSize := 0
	for page := 1; page <= maxPages; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("per_page", strconv.Itoa(MaxPerPage))
		var coll Collection
		if err := c.do(ctx, http.MethodGet, path, nil, &coll, append(opts, WithQuery(q))...); err != nil {
			return nil, err
		}
		for _, raw := range coll.Results {
			item, err := DecodeResource[T](raw, rootKey)
			if err != nil {
				return nil, &Error{Method: http.MethodGet, Path: path, Message: fmt.Sprintf("decoding page %d: %s", page, err), Err: err}
			}
			all = append(all, item)
		}
		if len(coll.Results) == 0 {
			return all, nil
		}
		if coll.Meta != nil && coll.Meta.TotalPages > 0 {
			if page >= coll.Meta.TotalPages {
				return all, nil
			}
			continue
		}
		// No usable meta: the server's real page size is whatever the first
		// full page held (it may cap per_page below what we asked for).
		if pageSize == 0 {
			pageSize = len(coll.Results)
			if coll.Meta != nil && coll.Meta.PerPage > 0 {
				pageSize = coll.Meta.PerPage
			}
		}
		if len(coll.Results) < pageSize {
			return all, nil
		}
	}
	return nil, &Error{Method: http.MethodGet, Path: path, Message: fmt.Sprintf("listing did not terminate after %d pages", maxPages)}
}

// DecodeResource decodes a resource that may arrive flat (`{"id": 1, ...}`)
// or wrapped in its root key (`{"project": {"id": 1, ...}}`), the way the
// request body is wrapped. The wrapped form is only taken when the top level
// has the root key holding an object and no `id` of its own, so a flat
// resource that happens to have a same-named attribute is never unwrapped.
func DecodeResource[T any](raw json.RawMessage, rootKey string) (T, error) {
	var out T
	payload := raw
	if rootKey != "" {
		var top map[string]json.RawMessage
		if json.Unmarshal(raw, &top) == nil {
			inner, wrapped := top[rootKey]
			_, hasID := top["id"]
			if wrapped && !hasID && len(inner) > 0 && inner[0] == '{' {
				payload = inner
			}
		}
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return out, err
	}
	return out, nil
}

// shapeOf describes a JSON body for a diagnostic without reproducing its
// contents: the top-level keys of an object, or the JSON kind otherwise.
func shapeOf(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "empty body"
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(trimmed, &top) == nil {
		keys := make([]string, 0, len(top))
		for k := range top {
			keys = append(keys, k)
		}
		sortStrings(keys)
		return "object with keys [" + strings.Join(keys, ", ") + "]"
	}
	switch trimmed[0] {
	case '[':
		return "JSON array"
	case '"':
		return "JSON string"
	default:
		return "JSON " + string(trimmed[:min(len(trimmed), 20)])
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any, opts ...RequestOption) error {
	req := &request{replayable: method == http.MethodGet || method == http.MethodDelete}
	for _, opt := range opts {
		opt(req)
	}

	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding %s %s body: %w", method, path, err)
		}
	}

	u := *c.baseURL
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + strings.TrimPrefix(path, "/")
	if len(req.query) > 0 {
		u.RawQuery = req.query.Encode()
	}

	uncodedConflicts := 0
	for attempt := 0; ; attempt++ {
		resp, err := c.send(ctx, method, u.String(), payload, req)
		if err != nil {
			if !req.replayable || !isTransient(err) || attempt >= c.maxRetries {
				return &Error{Method: method, Path: path, Message: err.Error(), Err: err,
					Idempotent: req.idempotencyKey != "", Preconditioned: req.ifMatch != nil}
			}
			if werr := c.sleep(ctx, c.backoff(attempt, 0)); werr != nil {
				return werr
			}
			continue
		}

		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return &Error{Method: method, Path: path, Status: resp.StatusCode, Message: readErr.Error(), Err: readErr}
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out == nil || len(bytes.TrimSpace(respBody)) == 0 {
				return nil
			}
			if err := json.Unmarshal(respBody, out); err != nil {
				return &Error{Method: method, Path: path, Status: resp.StatusCode,
					Message: fmt.Sprintf("decoding response (%s): %s", shapeOf(respBody), err), Err: err}
			}
			return nil
		}

		apiErr := newError(method, path, resp, respBody)
		apiErr.Idempotent = req.idempotencyKey != ""
		apiErr.Preconditioned = req.ifMatch != nil

		retry := false
		switch {
		case apiErr.Status == http.StatusTooManyRequests:
			// The request was not processed; always safe to repeat.
			retry = true
		case apiErr.Status == http.StatusConflict && apiErr.Code == CodeIdempotencyKeyInFlight:
			retry = true
		case apiErr.Status == http.StatusConflict && apiErr.Code == "" && req.idempotencyKey != "" && !looksStale(apiErr.Message):
			// An uncoded 409 on a keyed create is most likely the in-flight
			// replay window; give it exactly one more try.
			uncodedConflicts++
			retry = uncodedConflicts <= 1
		case isGatewayStatus(apiErr.Status):
			retry = req.replayable
		}
		if !retry || attempt >= c.maxRetries {
			return apiErr
		}
		if werr := c.sleep(ctx, c.backoff(attempt, apiErr.RetryAfter)); werr != nil {
			return werr
		}
	}
}

func (c *Client) send(ctx context.Context, method, rawURL string, payload []byte, req *request) (*http.Response, error) {
	var bodyReader io.Reader
	if payload != nil {
		bodyReader = bytes.NewReader(payload)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", c.userAgent)
	if payload != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if req.idempotencyKey != "" {
		httpReq.Header.Set("Idempotency-Key", req.idempotencyKey)
	}
	if req.ifMatch != nil {
		httpReq.Header.Set("If-Match", fmt.Sprintf("%q", strconv.FormatInt(*req.ifMatch, 10)))
	}
	return c.httpClient.Do(httpReq)
}

// backoff returns how long to wait before the next attempt. A server-supplied
// Retry-After wins outright; otherwise exponential backoff with full jitter,
// capped at maxBackoff.
func (c *Client) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > c.maxBackoff {
			return c.maxBackoff
		}
		return retryAfter
	}
	d := c.initialBackoff << uint(attempt) //nolint:gosec // attempt is bounded by maxRetries
	if d > c.maxBackoff || d <= 0 {
		d = c.maxBackoff
	}
	// Full jitter: anywhere in [d/2, d].
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1)) //nolint:gosec // jitter, not security
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func isGatewayStatus(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

// isTransient reports whether a transport-level error is worth retrying: a
// timeout, a refused or reset connection, or a torn-down response. Context
// cancellation, TLS failures, DNS failures that are not timeouts, and malformed
// requests are permanent and are never retried.
func isTransient(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsTimeout || dnsErr.IsTemporary
	}
	return false
}
