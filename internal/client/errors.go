package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Error codes published by the API alongside the prose message. The prose is
// free to change; these slugs are the contract a provider branches on.
const (
	CodeUnauthorized           = "unauthorized"
	CodeForbidden              = "forbidden"
	CodeNotFound               = "not_found"
	CodeBadRequest             = "bad_request"
	CodeValidationFailed       = "validation_failed"
	CodeInvalidAttribute       = "invalid_attribute"
	CodeStaleObject            = "stale_object"
	CodeIdempotencyKeyInFlight = "idempotency_key_in_flight"
	CodeRateLimited            = "rate_limited"
)

// Error is any non-2xx answer (or a transport failure) from the API. Status is
// 0 for transport failures. Code is empty when the server did not send one.
type Error struct {
	Method     string
	Path       string
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: ", e.Method, e.Path)
	if e.Status > 0 {
		fmt.Fprintf(&b, "HTTP %d", e.Status)
		if e.Code != "" {
			fmt.Fprintf(&b, " (%s)", e.Code)
		}
		if e.Message != "" {
			fmt.Fprintf(&b, ": %s", e.Message)
		}
		return b.String()
	}
	b.WriteString(e.Message)
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether waiting and trying again could succeed: a 429, the
// 409 that means "your own create is still in flight" (its replay becomes
// available once the original finishes), and gateway-class 5xx.
func (e *Error) Retryable() bool {
	switch {
	case e.Status == http.StatusTooManyRequests:
		return true
	case e.Status == http.StatusConflict && e.Code == CodeIdempotencyKeyInFlight:
		return true
	case e.Status == http.StatusBadGateway, e.Status == http.StatusServiceUnavailable, e.Status == http.StatusGatewayTimeout:
		return true
	}
	return false
}

// IsNotFound reports a 404. The API also answers 404 for ids that belong to
// another tenant and for a project mid-teardown, so a 404 always means "gone
// from this token's point of view" and never "try again".
func IsNotFound(err error) bool { return hasStatus(err, http.StatusNotFound) }

// IsStale reports a 409 caused by a lost optimistic-locking race (If-Match /
// lock_version). Distinct from the retryable in-flight idempotency 409.
func IsStale(err error) bool {
	e, ok := asError(err)
	return ok && e.Status == http.StatusConflict && e.Code != CodeIdempotencyKeyInFlight
}

// IsUnauthorized reports a 401.
func IsUnauthorized(err error) bool { return hasStatus(err, http.StatusUnauthorized) }

// IsForbidden reports a 403.
func IsForbidden(err error) bool { return hasStatus(err, http.StatusForbidden) }

// IsValidation reports a 422 of either flavour (validation_failed or
// invalid_attribute): the request is wrong, not the timing.
func IsValidation(err error) bool { return hasStatus(err, http.StatusUnprocessableEntity) }

func hasStatus(err error, status int) bool {
	e, ok := asError(err)
	return ok && e.Status == status
}

func asError(err error) (*Error, bool) {
	for err != nil {
		if e, ok := err.(*Error); ok {
			return e, true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return nil, false
		}
		err = u.Unwrap()
	}
	return nil, false
}

// errorEnvelope is the API's uniform non-2xx body.
type errorEnvelope struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func newError(method, path string, resp *http.Response, body []byte) *Error {
	e := &Error{Method: method, Path: path, Status: resp.StatusCode}
	var env errorEnvelope
	if json.Unmarshal(body, &env) == nil && env.Error != "" {
		e.Message = env.Error
		e.Code = env.Code
	} else if msg := strings.TrimSpace(string(body)); msg != "" {
		e.Message = msg
	} else {
		e.Message = http.StatusText(resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
			e.RetryAfter = time.Duration(secs) * time.Second
		} else if when, err := http.ParseTime(ra); err == nil {
			if d := time.Until(when); d > 0 {
				e.RetryAfter = d
			}
		}
	}
	return e
}
