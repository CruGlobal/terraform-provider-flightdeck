// Package client is a small, hand-written REST client for the Flightdeck
// /api/v1 surface. It encodes the API's cross-cutting contract in one place so
// every resource gets identical behaviour:
//
//   - bearer-token auth (`Authorization: Bearer fd_pat_…`), JSON in and out;
//   - the `{"results": [...], "meta": {...}}` collection envelope with full
//     pagination (see List);
//   - the `{"error": "<prose>", "code": "<slug>"}` error envelope, surfaced as
//     *Error so callers can branch on status and code rather than prose;
//   - `Idempotency-Key` on creates and `If-Match` on updates;
//   - client-side backoff on 429 (honouring Retry-After), on the 409
//     "idempotency key in flight" replay window, and on transient 5xx/network
//     failures.
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

// RequestOption tunes a single request.
type RequestOption func(*request)

type request struct {
	query          url.Values
	idempotencyKey string
	ifMatch        *int64
	// retryOnConnectionError is set for calls the caller knows are safe to
	// replay: GET/DELETE always, and POST/PATCH only when guarded by an
	// Idempotency-Key or If-Match precondition.
	retryOnConnectionError bool
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
		r.retryOnConnectionError = key != ""
	}
}

// WithIfMatch sends the resource's lock_version as an If-Match precondition.
// The server answers 409 (code stale_object) if the resource moved on.
func WithIfMatch(lockVersion int64) RequestOption {
	return func(r *request) {
		v := lockVersion
		r.ifMatch = &v
		r.retryOnConnectionError = true
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
	Meta    Meta              `json:"meta"`
}

// MaxPerPage is the server's cap on per_page; List always asks for it.
const MaxPerPage = 100

// List walks every page of a collection endpoint, decoding each element into
// T. It asks for the maximum page size and follows meta.total_pages, so a
// caller never sees a partial listing.
func List[T any](ctx context.Context, c *Client, path string, opts ...RequestOption) ([]T, error) {
	var all []T
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("per_page", strconv.Itoa(MaxPerPage))
		var coll Collection
		if err := c.do(ctx, http.MethodGet, path, nil, &coll, append(opts, WithQuery(q))...); err != nil {
			return nil, err
		}
		for _, raw := range coll.Results {
			var item T
			if err := json.Unmarshal(raw, &item); err != nil {
				return nil, fmt.Errorf("decoding %s page %d: %w", path, page, err)
			}
			all = append(all, item)
		}
		if page >= coll.Meta.TotalPages || len(coll.Results) == 0 {
			return all, nil
		}
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any, opts ...RequestOption) error {
	req := &request{retryOnConnectionError: method == http.MethodGet || method == http.MethodDelete}
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

	for attempt := 0; ; attempt++ {
		resp, err := c.send(ctx, method, u.String(), payload, req)
		if err != nil {
			if !req.retryOnConnectionError || !isTransient(err) || attempt >= c.maxRetries {
				return &Error{Method: method, Path: path, Message: err.Error(), Err: err}
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
					Message: fmt.Sprintf("decoding response: %s", err), Err: err}
			}
			return nil
		}

		apiErr := newError(method, path, resp, respBody)
		if !apiErr.Retryable() || attempt >= c.maxRetries {
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

// isTransient reports whether a transport-level error is worth retrying.
// Context cancellation and deadline expiry are never retried.
func isTransient(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}
