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
	// 409: the same Idempotency-Key was already used for a create with different
	// attributes; retrying does not help.
	CodeIdempotencyKeyReused = "idempotency_key_reused"
	CodeRateLimited          = "rate_limited"
	// State delete guards (422), each needing different handling.
	CodeStateInUse     = "state_in_use"
	CodeStateIsDefault = "state_is_default"
	CodeLastState      = "last_state"
	// Self-healing: a write that would change `armed` (422).
	CodeArmingRefused = "arming_refused"
)

// HasCode reports whether err is an API error carrying the given code.
func HasCode(err error, code string) bool {
	e, ok := asError(err)
	return ok && e.Code == code
}

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

	// Idempotent and Preconditioned record whether the failed request carried
	// an Idempotency-Key or an If-Match header. They let a 409 without a code
	// be classified: a create's 409 is the in-flight replay window, an
	// update's 409 is a lost optimistic-locking race.
	Idempotent     bool
	Preconditioned bool
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
// available once the original finishes), and gateway-class 5xx. A 409 without
// a code counts as in-flight only when the request was a keyed create and the
// message does not read like a lock conflict.
func (e *Error) Retryable() bool {
	switch {
	case e.Status == http.StatusTooManyRequests:
		return true
	case e.Status == http.StatusConflict:
		if e.Code != "" {
			return e.Code == CodeIdempotencyKeyInFlight
		}
		return e.Idempotent && !looksStale(e.Message)
	case isGatewayStatus(e.Status):
		return true
	}
	return false
}

// IsNotFound reports a 404. The API also answers 404 for ids that belong to
// another tenant and for a project mid-teardown, so a 404 always means "gone
// from this token's point of view" and never "try again".
func IsNotFound(err error) bool { return hasStatus(err, http.StatusNotFound) }

// IsStale reports a 409 caused by a lost optimistic-locking race (If-Match /
// lock_version). With a code, that is exactly stale_object. Without one, an
// update that carried If-Match is presumed stale, as is any 409 whose message
// mentions the lock; a 409 on a keyed create is the in-flight window instead,
// and any other uncoded 409 (for example a uniqueness conflict) is neither.
func IsStale(err error) bool {
	e, ok := asError(err)
	if !ok || e.Status != http.StatusConflict {
		return false
	}
	if e.Code != "" {
		return e.Code == CodeStaleObject
	}
	if looksStale(e.Message) {
		return true
	}
	return e.Preconditioned && !e.Idempotent
}

// looksStale is the prose fallback for a 409 without a code: the API's
// conflict message has always named lock_version / If-Match.
func looksStale(message string) bool {
	m := strings.ToLower(message)
	return strings.Contains(m, "lock_version") || strings.Contains(m, "if-match") ||
		strings.Contains(m, "modified by someone else")
}

// IsUnauthorized reports a 401.
func IsUnauthorized(err error) bool { return hasStatus(err, http.StatusUnauthorized) }

// IsForbidden reports a 403.
func IsForbidden(err error) bool { return hasStatus(err, http.StatusForbidden) }

// IsValidation reports a 422 of either flavour (validation_failed or
// invalid_attribute): the request is wrong, not the timing.
func IsValidation(err error) bool { return hasStatus(err, http.StatusUnprocessableEntity) }

// IsPreconditionRequired reports a 428: the server insists on an If-Match the
// client did not have to send.
func IsPreconditionRequired(err error) bool { return hasStatus(err, http.StatusPreconditionRequired) }

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

// AsError returns the *Error in err's chain, if any.
func AsError(err error) (*Error, bool) { return asError(err) }

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
		if len(msg) > maxErrorBody {
			msg = msg[:maxErrorBody] + "… (truncated)"
		}
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
