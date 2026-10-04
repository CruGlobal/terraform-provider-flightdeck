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
	// (a 404 for the id) may say this; see VerifyByGet.
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

func postResource[T Identified](ctx context.Context, c *Client, path, rootKey string, fields Fields, key string, opts ...RequestOption) (T, error) {
	var zero T
	var raw json.RawMessage
	if err := c.Post(ctx, path, map[string]any{rootKey: fields}, &raw, append([]RequestOption{WithIdempotencyKey(key)}, opts...)...); err != nil {
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

// secretBearing is implemented by resources whose create response carries a
// secret exactly once (ingestion tokens, routing keys, webhooks).
type secretBearing interface {
	Identified
	secret() string
}

// SecretRecord is how CreateSecretResource inspects and removes a record that
// a replayed create names. Both are per resource, because what "live" means
// is: a routing key is live until it is revoked or renamed, an ingestion token
// until it is revoked, a webhook until it is deleted. Each mirrors the API's
// own rule for refusing a replay.
type SecretRecord struct {
	// Live reports whether the record is still live. A record that is gone (a
	// 404) is not live, which is an answer rather than an error.
	Live func(ctx context.Context, id int64) (bool, error)
	// Retire revokes the record (deletes it, for a webhook) at its current
	// lock_version. A record that is already revoked or gone is success.
	Retire func(ctx context.Context, id int64) error
	// Verb is what Retire does, as the messages say it: "revoke" or "delete".
	Verb string
}

// CreateSecretResource is CreateResource for a resource whose create response
// carries a secret that the API returns ONLY to the original create. The
// stable Idempotency-Key is sent once; whatever comes back, it is never sent
// again in this call.
//
// The API never replays such a create with its secret. While the record the
// create made is still live it refuses the replay with 409
// CodeIdempotencyReplayWithheld and the record's id; once the record is
// revoked or deleted it replays it with the secret redacted. Either way the
// question is whose record that is, and the only evidence is how this call's
// own sends went:
//
//   - An earlier send of this request may have reached the server and got no
//     usable response (Error.EarlierSendUnanswered, or the same fact from the
//     trace on a successful answer). The record is then this call's own
//     create, whose response was lost: nobody holds its secret. It is retired
//     and the resource is created again under a fresh key.
//   - Every send was answered. The record was made by something else that sent
//     the same body: another resource declared with identical values, or an
//     earlier apply that failed after creating it. Both look the same from
//     here, and the first holds a credential somebody uses, so nothing is
//     retired: the create fails with an error that names the record and both
//     ways out (see replayWithheld).
//   - The replayed record is no longer live (revoked, deleted, or for a
//     routing key renamed away). Nothing can be lost: the resource is created
//     again under a fresh key, and nothing is retired. This is the path
//     Terraform's default destroy-then-create replacement takes.
//
// An older Flightdeck replays a LIVE record with its secret redacted instead
// of refusing it. That answer is checked with record.Live and handled exactly
// like the refusal, so the outcome does not depend on the server's version.
//
// One case remains that no client can settle: two resources declared with
// identical values, where the first's create succeeded and the second's first
// send was then lost on the way back. The second's retry finds a live record
// after an unanswered send and retires it as its own, although it was the
// first resource's. That needs a duplicated declaration and a lost response on
// exactly that request. The first resource's next refresh then finds its
// record gone and the next apply creates it again, so the damage lasts until
// that apply rather than hiding, but whatever used the old secret is refused
// in between. A send that timed out after it was written counts as lost in
// the same way, even when something between the client and Flightdeck (a
// proxy, say) dropped it before Flightdeck saw it.
//
// A fresh create that cannot be read back is retired too, so a live
// credential is never left unrecorded, and a second response without the
// secret is an error, never a third attempt.
func CreateSecretResource[T secretBearing](
	ctx context.Context, c *Client, path, rootKey string, fields Fields, key string,
	verify Verifier[T], record SecretRecord,
) (T, error) {
	var zero T
	var trace sendTrace
	created, err := postResource[T](ctx, c, path, rootKey, fields, key, withSendTrace(&trace))
	if err != nil {
		apiErr, ok := asError(err)
		if !ok || apiErr.Code != CodeIdempotencyReplayWithheld || apiErr.ID <= 0 {
			return zero, err
		}
		if !apiErr.EarlierSendUnanswered {
			return zero, replayWithheld(path, rootKey, apiErr.ID, record.Verb, apiErr)
		}
		// Our own create, whose response was lost: nobody holds its secret.
		if rerr := record.Retire(ctx, apiErr.ID); rerr != nil {
			return zero, retireFailed(path, rootKey, apiErr.ID, record.Verb, rerr)
		}
		return createSecretAfresh[T](ctx, c, path, rootKey, fields, verify, record, apiErr.ID)
	}

	if created.secret() != "" {
		verdict, err := verifyWithRetry(ctx, c, created, verify)
		if err != nil {
			return zero, err
		}
		if verdict == VerifiedPresent {
			return created, nil
		}
		// A fresh create that cannot be read back: do not leave a live
		// credential unrecorded, and do not mint another.
		_ = record.Retire(ctx, created.ResourceID())
		return zero, &Error{Method: http.MethodPost, Path: path, Status: http.StatusCreated,
			Message: fmt.Sprintf("the API reported %s %d created, but a follow-up read could not find it; "+
				"it was %s and nothing else was created", rootKey, created.ResourceID(), pastTense(record.Verb))}
	}

	// A replay: the secret was redacted. Whose record it names decides what
	// happens to it.
	replayedID := created.ResourceID()
	live, err := record.Live(ctx, replayedID)
	if err != nil {
		return zero, &Error{Method: http.MethodPost, Path: path, Status: http.StatusCreated, Err: err,
			Message: fmt.Sprintf("the API replayed an earlier create of %s %d without its secret, and the replayed "+
				"record could not be read to see whether it is still in use; nothing was created or %s: %s",
				rootKey, replayedID, pastTense(record.Verb), err)}
	}
	if live {
		if !trace.earlierSendUnanswered {
			return zero, replayWithheld(path, rootKey, replayedID, record.Verb, nil)
		}
		if err := record.Retire(ctx, replayedID); err != nil {
			return zero, retireFailed(path, rootKey, replayedID, record.Verb, err)
		}
	}
	return createSecretAfresh[T](ctx, c, path, rootKey, fields, verify, record, replayedID)
}

// createSecretAfresh creates the resource under a one-off key, after the
// stable key turned out to name record replacedID, and insists on a usable
// answer: a secret, and a record that reads back.
func createSecretAfresh[T secretBearing](
	ctx context.Context, c *Client, path, rootKey string, fields Fields,
	verify Verifier[T], record SecretRecord, replacedID int64,
) (T, error) {
	var zero T
	recreated, err := postResource[T](ctx, c, path, rootKey, fields, RandomIdempotencyKey())
	if err != nil {
		return zero, err
	}
	if recreated.secret() == "" {
		return zero, &Error{Method: http.MethodPost, Path: path, Status: http.StatusCreated,
			Message: fmt.Sprintf("the API returned %s %d without its secret on a fresh create; refusing to record a "+
				"credential Terraform cannot know. Check the deployed Flightdeck API version", rootKey, recreated.ResourceID())}
	}
	verdict, err := verifyWithRetry(ctx, c, recreated, verify)
	if err != nil {
		return zero, err
	}
	if verdict != VerifiedPresent {
		_ = record.Retire(ctx, recreated.ResourceID())
		return zero, &Error{Method: http.MethodPost, Path: path, Status: http.StatusCreated,
			Message: fmt.Sprintf("the API reported %s %d created (after the replayed %s %d), but it cannot be "+
				"read back; it was %s and nothing else was created", rootKey, recreated.ResourceID(), rootKey, replacedID,
				pastTense(record.Verb))}
	}
	return recreated, nil
}

// replayWithheld is the error for a create whose stable key names a live
// record this call did not make. cause is the API's own refusal, when that is
// how the replay was answered.
func replayWithheld(path, rootKey string, id int64, verb string, cause *Error) *Error {
	noun := strings.ReplaceAll(rootKey, "_", " ")
	e := &Error{Method: http.MethodPost, Path: path, Status: http.StatusConflict,
		Code: CodeIdempotencyReplayWithheld, ID: id, Idempotent: true,
		Message: fmt.Sprintf("an identical create already made %s %d, which is still live, and its secret is "+
			"returned only once, so it cannot be recorded here; nothing was created or %s. Either give this "+
			"resource values that differ from the other one's, or, if %s %d was left behind by an earlier apply "+
			"that failed, %s it and apply again", noun, id, pastTense(verb), noun, id, verb)}
	if cause != nil {
		e.Err = cause
		e.EarlierSendUnanswered = cause.EarlierSendUnanswered
	}
	return e
}

func retireFailed(path, rootKey string, id int64, verb string, err error) *Error {
	return &Error{Method: http.MethodPost, Path: path, Status: http.StatusCreated, Err: err,
		Message: fmt.Sprintf("an earlier attempt at this create got no usable response, and the %s %d it left "+
			"behind could not be %s, so nothing else was created: %s",
			strings.ReplaceAll(rootKey, "_", " "), id, pastTense(verb), err)}
}

// pastTense turns "revoke" into "revoked" and "delete" into "deleted".
func pastTense(verb string) string {
	if verb == "" {
		return "removed"
	}
	if strings.HasSuffix(verb, "e") {
		return verb + "d"
	}
	return verb + "ed"
}
