package client

import (
	"context"
	"strconv"
)

// IngestionTokenScopes are the IngestionToken.scope enum values.
var IngestionTokenScopes = []string{"post_server_item", "post_client_item"}

// IngestionToken is a project's error-ingestion token. Token (the secret) is
// present only in the response to the ORIGINAL create; a replayed create (same
// Idempotency-Key within 24 hours) returns the row with Token empty and
// SecretAvailable false, and no read ever returns it.
type IngestionToken struct {
	ID              int64   `json:"id"`
	ProjectID       int64   `json:"project_id"`
	Name            string  `json:"name"`
	Environment     string  `json:"environment"`
	Scope           string  `json:"scope"`
	Masked          string  `json:"masked"`
	LastFour        string  `json:"last_four"`
	Revoked         bool    `json:"revoked"`
	RevokedAt       *string `json:"revoked_at"`
	LastUsedAt      *string `json:"last_used_at"`
	LockVersion     int64   `json:"lock_version"`
	CreatedAt       string  `json:"created_at"`
	Token           string  `json:"token"`
	SecretAvailable *bool   `json:"secret_available"`
}

// ResourceID implements Identified.
func (t *IngestionToken) ResourceID() int64 { return t.ID }

// IsRevoked reports whether the API marked the token revoked, from either the
// boolean or a non-empty revoked_at.
func (t *IngestionToken) IsRevoked() bool {
	return t.Revoked || (t.RevokedAt != nil && *t.RevokedAt != "")
}

// secret implements secretBearing.
func (t *IngestionToken) secret() string { return t.Token }

const ingestionTokenRoot = "ingestion_token"

func ingestionTokensPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/ingestion-tokens"
}

func ingestionTokenPath(projectID, id int64) string {
	return ingestionTokensPath(projectID) + "/" + strconv.FormatInt(id, 10)
}

// ListIngestionTokens returns a project's tokens, revoked ones included.
func (c *Client) ListIngestionTokens(ctx context.Context, projectID int64) ([]IngestionToken, error) {
	return ListResources[IngestionToken](ctx, c, ingestionTokensPath(projectID), ingestionTokenRoot)
}

// GetIngestionToken fetches one token (masked). A revoked token is still
// returned, with Revoked set; callers decide what "gone" means.
func (c *Client) GetIngestionToken(ctx context.Context, projectID, id int64) (*IngestionToken, error) {
	return GetResource[*IngestionToken](ctx, c, ingestionTokenPath(projectID, id), ingestionTokenRoot)
}

// CreateIngestionToken mints a token and guarantees the returned value carries
// the secret. If the API replays an earlier create (the secret is redacted on
// a replay), the replayed row is revoked and the token is minted again under a
// fresh key; the stable key is never re-sent. See CreateSecretResource.
func (c *Client) CreateIngestionToken(ctx context.Context, projectID int64, fields Fields, idempotencyKey string) (*IngestionToken, error) {
	return CreateSecretResource(ctx, c, ingestionTokensPath(projectID), ingestionTokenRoot, fields, idempotencyKey,
		VerifyByGet(func(ctx context.Context, id int64) (*IngestionToken, error) {
			return c.GetIngestionToken(ctx, projectID, id)
		}),
		func(ctx context.Context, replayed *IngestionToken) error {
			// The replayed body carries the lock_version at creation time; the
			// row may have moved on (it was revoked when the predecessor was
			// destroyed), so revoke against the CURRENT version.
			current, err := c.GetIngestionToken(ctx, projectID, replayed.ID)
			if err != nil {
				return err
			}
			if current.IsRevoked() {
				return nil
			}
			return c.RevokeIngestionToken(ctx, projectID, current.ID, current.LockVersion)
		})
}

// RevokeIngestionToken revokes a token (the API's DELETE, which answers 200
// with the revoked row and is idempotent) under an If-Match precondition; 404
// is success.
func (c *Client) RevokeIngestionToken(ctx context.Context, projectID, id, lockVersion int64) error {
	err := c.Delete(ctx, ingestionTokenPath(projectID, id), nil, WithIfMatch(lockVersion))
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
