package client

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ServiceAccount is a service account in the token's workspace: a login for
// an application or a bot, not a person. ID is the account's USER id, the
// same id a project membership takes as user_id. EmailAddress is stored
// stripped and lowercased, and never changes. Role is the workspace role,
// always "member". JoinedAt is when the account joined THIS workspace, in the
// server's own zone offset.
//
// A service account can be in several workspaces at once, and its name is
// shared by all of them, so Flightdeck refuses to rename one that is also in
// another workspace.
type ServiceAccount struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	EmailAddress string `json:"email_address"`
	Role         string `json:"role"`
	JoinedAt     string `json:"joined_at"`
}

// ResourceID implements Identified.
func (a *ServiceAccount) ResourceID() int64 { return a.ID }

// ServiceAccountAPIToken is a REST API token (fd_pat_…) of a service account.
// Token (the secret) is present only in the response to the ORIGINAL create;
// a replayed create returns it null with SecretAvailable false, and no read
// ever returns it. ExpiresAt is nil for a token that never expires, and a
// token past it is still listed until it is deleted. Times carry the
// server's own zone offset.
type ServiceAccountAPIToken struct {
	ID               int64   `json:"id"`
	ServiceAccountID int64   `json:"service_account_id"`
	Name             string  `json:"name"`
	Masked           string  `json:"masked"`
	LastFour         string  `json:"last_four"`
	ExpiresAt        *string `json:"expires_at"`
	LastUsedAt       *string `json:"last_used_at"`
	CreatedAt        string  `json:"created_at"`
	Token            string  `json:"token"`
	SecretAvailable  *bool   `json:"secret_available"`
}

// ResourceID implements Identified.
func (t *ServiceAccountAPIToken) ResourceID() int64 { return t.ID }

// secret implements secretBearing.
func (t *ServiceAccountAPIToken) secret() string { return t.Token }

// IsExpired reports whether the token's expiry is at or before now, the way
// Flightdeck decides a token no longer authenticates. A token with no expiry,
// or one whose expiry does not parse, is not expired.
func (t *ServiceAccountAPIToken) IsExpired(now time.Time) bool {
	if t.ExpiresAt == nil || strings.TrimSpace(*t.ExpiresAt) == "" {
		return false
	}
	at, err := time.Parse(time.RFC3339, *t.ExpiresAt)
	if err != nil {
		return false
	}
	return !now.Before(at)
}

// Error codes the service-account routes answer with, beyond the shared ones.
const (
	// 409: two creates for the same address raced past the "already a
	// service account here" check, and the database's unique index refused
	// the loser. The account exists now.
	CodeUniquenessConflict = "uniqueness_conflict"
)

const (
	serviceAccountRoot         = "service_account"
	serviceAccountAPITokenRoot = "api_token"
	serviceAccountsPath        = "/service-accounts"
)

func serviceAccountPath(id int64) string {
	return serviceAccountsPath + "/" + strconv.FormatInt(id, 10)
}

func serviceAccountAPITokensPath(serviceAccountID int64) string {
	return serviceAccountPath(serviceAccountID) + "/api-tokens"
}

func serviceAccountAPITokenPath(serviceAccountID, id int64) string {
	return serviceAccountAPITokensPath(serviceAccountID) + "/" + strconv.FormatInt(id, 10)
}

// ListServiceAccounts returns the workspace's service accounts, ordered by id.
func (c *Client) ListServiceAccounts(ctx context.Context) ([]ServiceAccount, error) {
	return ListResources[ServiceAccount](ctx, c, serviceAccountsPath, serviceAccountRoot)
}

// FindServiceAccountByEmail returns the workspace's service account with this
// address, or a 404 *Error. The address is compared the way Flightdeck stores
// it: stripped and lowercased.
func (c *Client) FindServiceAccountByEmail(ctx context.Context, email string) (*ServiceAccount, error) {
	accounts, err := c.ListServiceAccounts(ctx)
	if err != nil {
		return nil, err
	}
	want := strings.ToLower(strings.TrimSpace(email))
	for i := range accounts {
		if strings.ToLower(strings.TrimSpace(accounts[i].EmailAddress)) == want {
			return &accounts[i], nil
		}
	}
	return nil, &Error{Method: http.MethodGet, Path: serviceAccountsPath, Status: http.StatusNotFound, Code: CodeNotFound,
		Message: fmt.Sprintf("No service account in this workspace has the address %q", email)}
}

// GetServiceAccount fetches one service account by its user id. A person's
// id, or a service account that is not in this workspace, is a 404.
func (c *Client) GetServiceAccount(ctx context.Context, id int64) (*ServiceAccount, error) {
	return GetResource[*ServiceAccount](ctx, c, serviceAccountPath(id), serviceAccountRoot)
}

// CreateServiceAccount makes a service account in the workspace, or adds an
// existing one with that address, through the verified create path.
//
// When the address belongs to a service account that is still in another
// workspace, the create adds that account here and keeps the name it already
// has, which is shared there: the name sent is not applied. One that is in no
// workspace at all (removed earlier; the user is kept) takes the name sent.
//
// It returns the account twice: current, as the read back after the create
// found it, and answered, as the create's own response gave it. They differ
// on a replay. Flightdeck replays the original 201 as it was, so answered has
// the name the account had then, and current the name it has now. Only
// answered tells whether the create ignored the name it was sent.
//
// A replay of a create whose account was removed from the workspace since is
// a 404 on the read back, so the create runs again under a fresh key, which
// adds the same account (and the same id) back. answered is then that second
// create's response.
func (c *Client) CreateServiceAccount(ctx context.Context, fields Fields, idempotencyKey string) (current, answered *ServiceAccount, err error) {
	verify := func(ctx context.Context, created *ServiceAccount) (Verdict, error) {
		a, err := c.GetServiceAccount(ctx, created.ID)
		if err != nil {
			if IsNotFound(err) {
				return VerifiedGone, nil
			}
			return VerifiedUnknown, err
		}
		current = a
		return VerifiedPresent, nil
	}
	answered, err = CreateResource(ctx, c, serviceAccountsPath, serviceAccountRoot, fields, idempotencyKey, verify)
	if err != nil {
		return nil, nil, err
	}
	return current, answered, nil
}

// UpdateServiceAccount PATCHes a service account. There is no lock_version
// on a service account, so the write carries no If-Match: the last writer
// wins. Only the name can change.
func (c *Client) UpdateServiceAccount(ctx context.Context, id int64, fields Fields) (*ServiceAccount, error) {
	return PatchResource[*ServiceAccount](ctx, c, serviceAccountPath(id), serviceAccountRoot, fields, nil)
}

// DeleteServiceAccount removes a service account from this workspace, with
// its API tokens and project memberships here. The account itself is never
// deleted, since it can be in other workspaces. A 404 is success.
func (c *Client) DeleteServiceAccount(ctx context.Context, id int64) error {
	return c.deleteGone(ctx, serviceAccountPath(id))
}

// ListServiceAccountAPITokens returns a service account's tokens in this
// workspace, newest first, expired ones included.
func (c *Client) ListServiceAccountAPITokens(ctx context.Context, serviceAccountID int64) ([]ServiceAccountAPIToken, error) {
	return ListResources[ServiceAccountAPIToken](ctx, c, serviceAccountAPITokensPath(serviceAccountID), serviceAccountAPITokenRoot)
}

// GetServiceAccountAPIToken fetches one token, without its secret. A 404
// means the token is gone, or its service account is no longer in this
// workspace.
func (c *Client) GetServiceAccountAPIToken(ctx context.Context, serviceAccountID, id int64) (*ServiceAccountAPIToken, error) {
	return GetResource[*ServiceAccountAPIToken](ctx, c, serviceAccountAPITokenPath(serviceAccountID, id), serviceAccountAPITokenRoot)
}

// CreateServiceAccountAPIToken mints a token and guarantees the returned
// value carries the secret, or fails. A token is live until it is deleted or
// expires: its name and expiry never change. See CreateSecretResource for
// what happens when the stable Idempotency-Key names a token that already
// exists.
func (c *Client) CreateServiceAccountAPIToken(ctx context.Context, serviceAccountID int64, fields Fields, idempotencyKey string) (*ServiceAccountAPIToken, error) {
	get := func(ctx context.Context, id int64) (*ServiceAccountAPIToken, error) {
		return c.GetServiceAccountAPIToken(ctx, serviceAccountID, id)
	}
	return CreateSecretResource(ctx, c, serviceAccountAPITokensPath(serviceAccountID), serviceAccountAPITokenRoot, fields, idempotencyKey,
		VerifyByGet(get),
		SecretRecord{
			Verb: "delete",
			Live: func(ctx context.Context, id int64) (bool, error) {
				current, err := get(ctx, id)
				if err != nil {
					if IsNotFound(err) {
						return false, nil
					}
					return false, err
				}
				return !current.IsExpired(time.Now()), nil
			},
			Retire: func(ctx context.Context, id int64) error {
				return c.DeleteServiceAccountAPIToken(ctx, serviceAccountID, id)
			},
		})
}

// DeleteServiceAccountAPIToken deletes a token, which stops it working at
// once. There is no lock_version on a token, so the DELETE carries no
// If-Match. A 404 (the token, or its service account, is gone) is success.
func (c *Client) DeleteServiceAccountAPIToken(ctx context.Context, serviceAccountID, id int64) error {
	return c.deleteGone(ctx, serviceAccountAPITokenPath(serviceAccountID, id))
}
