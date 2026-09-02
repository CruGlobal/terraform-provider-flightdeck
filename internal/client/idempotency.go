package client

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// IdempotencyKey derives a stable Idempotency-Key from the given parts. Use
// PayloadKey for creates; this is the primitive it is built on.
func IdempotencyKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return "tf-" + hex.EncodeToString(sum[:20])
}

// PayloadKey derives the Idempotency-Key for a create from the resource kind,
// its parent scope (a project id, or "" for workspace-level resources) and the
// exact body about to be sent. The same declaration therefore always sends the
// same key, so a create that is retried — by this client after a throttle or a
// dropped connection, or by Terraform on the next apply after one that failed
// before state was written — replays the original 201 instead of creating a
// duplicate.
//
// Terraform does not tell a provider the resource address, so the body is the
// closest available stand-in for "this declaration". Two declarations that
// differ in any attribute get different keys; two byte-identical declarations
// of the same resource in one configuration would share one, which is a
// configuration error in its own right (the API rejects the duplicate anyway
// where it enforces uniqueness).
//
// The server honours a key for 24 hours. CreateResource handles the one way a
// stable key can mislead: destroying and recreating the same resource inside
// that window.
func PayloadKey(kind, scope string, payload any) string {
	// encoding/json emits map keys in sorted order, so the encoding is canonical
	// for the map-based bodies the provider sends.
	encoded, err := json.Marshal(payload)
	if err != nil {
		encoded = []byte(fmt.Sprint(payload))
	}
	return IdempotencyKey(kind, scope, string(encoded))
}

// RandomIdempotencyKey returns a one-off key. Used only as the fallback when a
// stable key turns out to replay a resource that no longer exists.
func RandomIdempotencyKey() string {
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not a condition worth handling gracefully in a
		// CLI plugin; fall back to time so the key is still unique in practice.
		return "tf-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "tf-" + hex.EncodeToString(b[:])
}

// Identified is implemented by every resource type the client creates, so the
// create path can insist on a usable id before trusting a response.
type Identified interface {
	ResourceID() int64
}

// Verdict is what a post-create verification read concludes.
type Verdict int

const (
	// VerifiedPresent: the created resource is readable; the create is genuine.
	VerifiedPresent Verdict = iota
	// VerifiedGone: the resource the 201 named definitely does not exist — the
	// 201 was the server replaying an earlier create (same Idempotency-Key)
	// for a resource that has since been deleted. Only an authoritative signal
	// (a 404 for the id, or the row present but marked revoked) may say this.
	VerifiedGone
	// VerifiedUnknown: the read neither confirmed nor refuted the resource —
	// typically a list that does not contain it, which could be a filtering or
	// field-mapping problem rather than a replay. Never grounds for a recreate.
	VerifiedUnknown
)

// Verifier reads back a just-created resource. It returns the verdict, or an
// error for any failure that is not itself the verdict (auth, throttle, …).
type Verifier[T Identified] func(ctx context.Context, created T) (Verdict, error)

// VerifyByGet is the Verifier for resources with a show route: a 404 for the
// returned id is authoritative.
func VerifyByGet[T Identified](get func(ctx context.Context, id int64) (T, error)) Verifier[T] {
	return func(ctx context.Context, created T) (Verdict, error) {
		if _, err := get(ctx, created.ResourceID()); err != nil {
			if IsNotFound(err) {
				return VerifiedGone, nil
			}
			return VerifiedUnknown, err
		}
		return VerifiedPresent, nil
	}
}

// verifyAttempts and verifyDelays absorb read-after-write lag before a 404 is
// taken as authoritative: the id is read up to three times over ~1s.
var verifyDelays = []time.Duration{0, 250 * time.Millisecond, 750 * time.Millisecond}

// CreateResource POSTs {rootKey: fields} to path under the stable idempotency
// key and returns the created resource. It is the single create path for
// every resource, and it refuses to trust a response it cannot verify:
//
//  1. The response must decode (flat or wrapped in rootKey) to a resource with
//     a positive id; otherwise the create is a hard error naming the endpoint
//     and the body shape, and nothing else is attempted — a second POST with
//     an unusable first response is how duplicates happen.
//  2. The id is read back. A definitive "gone" (VerifiedGone) means the 201
//     was the replay of a since-deleted resource, so the create is re-run once
//     with a fresh key and verified again; a second "gone" is an error, never
//     a third POST. Lag is absorbed by re-reading a few times first.
//  3. An inconclusive read (VerifiedUnknown) is an error, not a recreate: the
//     resource may well exist, and creating another would be the worse
//     outcome (for an ingestion token, an unrecorded live credential).
func CreateResource[T Identified](ctx context.Context, c *Client, path, rootKey string, fields Fields, key string, verify Verifier[T]) (T, error) {
	var zero T
	created, err := postResource[T](ctx, c, path, rootKey, fields, key)
	if err != nil {
		return zero, err
	}

	verdict, err := verifyWithRetry(ctx, c, created, verify)
	if err != nil {
		return zero, err
	}
	switch verdict {
	case VerifiedPresent:
		return created, nil
	case VerifiedUnknown:
		return zero, &Error{Method: http.MethodPost, Path: path, Status: http.StatusCreated,
			Message: fmt.Sprintf("the API reported %s %d created, but a follow-up read could not find it; "+
				"refusing to create another. Check the resource in the Flightdeck UI and import it if it exists",
				rootKey, created.ResourceID())}
	}

	// VerifiedGone: the stable key replayed a deleted resource.
	recreated, err := postResource[T](ctx, c, path, rootKey, fields, RandomIdempotencyKey())
	if err != nil {
		return zero, err
	}
	verdict, err = verifyWithRetry(ctx, c, recreated, verify)
	if err != nil {
		return zero, err
	}
	if verdict != VerifiedPresent {
		return zero, &Error{Method: http.MethodPost, Path: path, Status: http.StatusCreated,
			Message: fmt.Sprintf("the API reported %s %d created (after a replayed create for the deleted %s %d), "+
				"but it cannot be read back; refusing to create another",
				rootKey, recreated.ResourceID(), rootKey, created.ResourceID())}
	}
	return recreated, nil
}

func postResource[T Identified](ctx context.Context, c *Client, path, rootKey string, fields Fields, key string) (T, error) {
	var zero T
	var raw json.RawMessage
	if err := c.Post(ctx, path, map[string]any{rootKey: fields}, &raw, WithIdempotencyKey(key)); err != nil {
		return zero, err
	}
	created, err := DecodeResource[T](raw, rootKey)
	if err != nil {
		return zero, &Error{Method: http.MethodPost, Path: path, Status: http.StatusCreated, Err: err,
			Message: fmt.Sprintf("create response could not be decoded as a %s (%s): %s", rootKey, shapeOf(raw), err)}
	}
	if created.ResourceID() <= 0 {
		return zero, &Error{Method: http.MethodPost, Path: path, Status: http.StatusCreated,
			Message: fmt.Sprintf("create response has no usable %s id (%s); refusing to continue so the "+
				"resource is not created twice. Check the deployed Flightdeck API version", rootKey, shapeOf(raw))}
	}
	return created, nil
}

func verifyWithRetry[T Identified](ctx context.Context, c *Client, created T, verify Verifier[T]) (Verdict, error) {
	var verdict Verdict
	for i, delay := range verifyDelays {
		if delay > 0 {
			if err := c.sleep(ctx, delay); err != nil {
				return VerifiedUnknown, err
			}
		}
		v, err := verify(ctx, created)
		if err != nil {
			return VerifiedUnknown, err
		}
		verdict = v
		if v == VerifiedPresent || i == len(verifyDelays)-1 {
			break
		}
	}
	return verdict, nil
}

// GetResource GETs path and decodes the resource, flat or wrapped in rootKey.
func GetResource[T any](ctx context.Context, c *Client, path, rootKey string) (T, error) {
	var zero T
	var raw json.RawMessage
	if err := c.Get(ctx, path, &raw); err != nil {
		return zero, err
	}
	out, err := DecodeResource[T](raw, rootKey)
	if err != nil {
		return zero, &Error{Method: http.MethodGet, Path: path, Status: http.StatusOK, Err: err,
			Message: fmt.Sprintf("response could not be decoded as a %s (%s): %s", rootKey, shapeOf(raw), err)}
	}
	return out, nil
}

// PatchResource PATCHes {rootKey: fields} with an If-Match precondition and
// decodes the updated resource, flat or wrapped.
func PatchResource[T any](ctx context.Context, c *Client, path, rootKey string, fields Fields, lockVersion *int64) (T, error) {
	var zero T
	var raw json.RawMessage
	var opts []RequestOption
	if lockVersion != nil {
		opts = append(opts, WithIfMatch(*lockVersion))
	}
	if err := c.Patch(ctx, path, map[string]any{rootKey: fields}, &raw, opts...); err != nil {
		return zero, err
	}
	out, err := DecodeResource[T](raw, rootKey)
	if err != nil {
		return zero, &Error{Method: http.MethodPatch, Path: path, Status: http.StatusOK, Err: err,
			Message: fmt.Sprintf("response could not be decoded as a %s (%s): %s", rootKey, shapeOf(raw), err)}
	}
	return out, nil
}
