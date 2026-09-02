package client

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
// The server honours a key for 24 hours. CreateWithReplayGuard handles the one
// way a stable key can mislead: destroying and recreating the same resource
// inside that window.
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

// CreateWithReplayGuard runs create with the stable key, then verifies the
// resource it returned is actually readable. A 404 on that verification means
// the 2xx was the server replaying a create that happened earlier under the
// same key — for a resource that has since been deleted (destroy + recreate of
// the same declaration inside the 24-hour idempotency window). In that one
// case the create is re-run with a fresh random key. Any other verification
// error is returned as-is.
func CreateWithReplayGuard[T any](
	ctx context.Context,
	key string,
	create func(ctx context.Context, key string) (T, error),
	verify func(ctx context.Context, created T) error,
) (T, error) {
	created, err := create(ctx, key)
	if err != nil {
		return created, err
	}
	verr := verify(ctx, created)
	if verr == nil {
		return created, nil
	}
	if !IsNotFound(verr) {
		return created, verr
	}
	return create(ctx, RandomIdempotencyKey())
}
