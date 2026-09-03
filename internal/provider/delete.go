package provider

import (
	"context"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
)

// deleteWithIfMatch runs a DELETE that carries the state's lock_version. A
// delete is not an overwrite — the user asked for the resource to be gone
// whatever it now contains — so a stale-version 409 is answered by re-reading
// the current version once and deleting with that; only a second 409 is
// returned. Every other error is returned as-is.
func deleteWithIfMatch(ctx context.Context, lockVersion int64,
	del func(ctx context.Context, lockVersion int64) error,
	currentVersion func(ctx context.Context) (int64, error),
) error {
	err := del(ctx, lockVersion)
	if err == nil || !client.IsStale(err) {
		return err
	}
	fresh, rerr := currentVersion(ctx)
	if rerr != nil {
		if client.IsNotFound(rerr) {
			return nil // already gone
		}
		return err
	}
	return del(ctx, fresh)
}
