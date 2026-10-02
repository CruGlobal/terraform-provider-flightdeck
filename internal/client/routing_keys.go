package client

import (
	"context"
	"strconv"
)

// RoutingKey is an Events API credential for a project: an external monitor
// posts an event carrying the key and Flightdeck opens, folds, acknowledges or
// resolves an incident from it. A project may hold as many keys as it likes,
// so one can be issued per monitor and revoked on its own.
//
// Key (the secret) is present only in the response to the ORIGINAL create; a
// replayed create (same Idempotency-Key within 24 hours) returns the row with
// Key empty and SecretAvailable false, and no read ever returns it. Reads
// report Masked and LastFour instead.
//
// EscalationPolicyID is the optional pointer to a Flightdeck escalation policy
// that events on this key page. It is null by default, which is the point of
// the key: a project can receive events without Flightdeck owning the paging.
// Escalation policies have no API of their own, so this is reported but never
// written here.
type RoutingKey struct {
	ID                 int64   `json:"id"`
	ProjectID          int64   `json:"project_id"`
	Name               string  `json:"name"`
	EscalationPolicyID *int64  `json:"escalation_policy_id"`
	Masked             string  `json:"masked"`
	LastFour           string  `json:"last_four"`
	Revoked            bool    `json:"revoked"`
	RevokedAt          *string `json:"revoked_at"`
	LastUsedAt         *string `json:"last_used_at"`
	LockVersion        int64   `json:"lock_version"`
	CreatedAt          string  `json:"created_at"`
	Key                string  `json:"routing_key"`
	SecretAvailable    *bool   `json:"secret_available"`
}

// ResourceID implements Identified.
func (k *RoutingKey) ResourceID() int64 { return k.ID }

// IsRevoked reports whether the API marked the key revoked, from either the
// boolean or a non-empty revoked_at.
func (k *RoutingKey) IsRevoked() bool {
	return k.Revoked || (k.RevokedAt != nil && *k.RevokedAt != "")
}

// secret implements secretBearing.
func (k *RoutingKey) secret() string { return k.Key }

const routingKeyRoot = "routing_key"

func routingKeysPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/routing-keys"
}

func routingKeyPath(projectID, id int64) string {
	return routingKeysPath(projectID) + "/" + strconv.FormatInt(id, 10)
}

// ListRoutingKeys returns a project's keys, revoked ones included: revoking
// does not remove the row.
func (c *Client) ListRoutingKeys(ctx context.Context, projectID int64) ([]RoutingKey, error) {
	return ListResources[RoutingKey](ctx, c, routingKeysPath(projectID), routingKeyRoot)
}

// GetRoutingKey fetches one key (masked). A revoked key is still returned,
// with Revoked set; callers decide what "gone" means.
func (c *Client) GetRoutingKey(ctx context.Context, projectID, id int64) (*RoutingKey, error) {
	return GetResource[*RoutingKey](ctx, c, routingKeyPath(projectID, id), routingKeyRoot)
}

// CreateRoutingKey mints a key and guarantees the returned value carries the
// secret, or fails. See CreateSecretResource for what happens when the stable
// Idempotency-Key names a key that already exists: it is retired and the key
// minted again under a fresh key only when it is this call's own lost create;
// a live key anything else made is never touched, and the create fails with
// CodeIdempotencyReplayWithheld naming it.
//
// A key is live, for that purpose, when it is not revoked and still carries
// the name this create sent: the API's own rule for refusing a replay. `name`
// is editable in place, but the cached create is keyed on the body that
// carried the ORIGINAL name, and the API honours that key for 24 hours. So a
// row renamed since can be named by a create that has nothing to do with it,
// when somebody freed a name and somebody else reused it. Such a row is
// demonstrably not this create's, so it is left alone and the new declaration
// gets its own credential.
//
// retiredID reports the row this create revoked on the way, or 0. That is
// only ever this call's own create (whose response was lost, so nobody holds
// its secret) or a fresh key that could not be read back, but a revoked row
// the operator did not ask for is still worth naming when the create then
// fails.
func (c *Client) CreateRoutingKey(ctx context.Context, projectID int64, fields Fields, idempotencyKey string) (key *RoutingKey, retiredID int64, err error) {
	sentName, named := fields["name"].(string)
	created, err := CreateSecretResource(ctx, c, routingKeysPath(projectID), routingKeyRoot, fields, idempotencyKey,
		VerifyByGet(func(ctx context.Context, id int64) (*RoutingKey, error) {
			return c.GetRoutingKey(ctx, projectID, id)
		}),
		SecretRecord{
			Verb: "revoke",
			Live: func(ctx context.Context, id int64) (bool, error) {
				current, err := c.GetRoutingKey(ctx, projectID, id)
				if err != nil {
					if IsNotFound(err) {
						return false, nil
					}
					return false, err
				}
				// Renamed since it was created, so this replay is not ours.
				if named && current.Name != sentName {
					return false, nil
				}
				return !current.IsRevoked(), nil
			},
			Retire: func(ctx context.Context, id int64) error {
				// Revoke at the CURRENT lock_version: the row may have moved on
				// since the response that named it.
				current, err := c.GetRoutingKey(ctx, projectID, id)
				if err != nil {
					if IsNotFound(err) {
						return nil
					}
					return err
				}
				if current.IsRevoked() {
					return nil
				}
				// Set before the revoke, not after: if the revoke or the
				// re-mint that follows it fails, which row went is exactly what
				// the caller most needs to be told.
				retiredID = current.ID
				return c.RevokeRoutingKey(ctx, projectID, current.ID, current.LockVersion)
			},
		})
	return created, retiredID, err
}

// UpdateRoutingKey PATCHes a key with an If-Match precondition. Only `name` is
// written here; an absent key keeps its stored value. The secret itself cannot
// be changed — the API has no rotate route, so rotating means creating another
// key and revoking this one.
func (c *Client) UpdateRoutingKey(ctx context.Context, projectID, id int64, fields Fields, lockVersion int64) (*RoutingKey, error) {
	return PatchResource[*RoutingKey](ctx, c, routingKeyPath(projectID, id), routingKeyRoot, fields, &lockVersion)
}

// RevokeRoutingKey revokes a key (the API's DELETE, which answers 200 with the
// revoked row and is idempotent) under an If-Match precondition; 404 is
// success. Revoking is irreversible and leaves the row readable forever, so
// the audit trail of which key was used when survives.
func (c *Client) RevokeRoutingKey(ctx context.Context, projectID, id, lockVersion int64) error {
	err := c.Delete(ctx, routingKeyPath(projectID, id), nil, WithIfMatch(lockVersion))
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
