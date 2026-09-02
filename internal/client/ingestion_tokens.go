package client

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
)

// IngestionTokenScopes are the IngestionToken.scope enum values.
var IngestionTokenScopes = []string{"post_server_item", "post_client_item"}

// IngestionToken is a project's error-ingestion token. Token (the plaintext)
// is present only in the create response.
type IngestionToken struct {
	ID          int64   `json:"id"`
	ProjectID   int64   `json:"project_id"`
	Name        string  `json:"name"`
	Environment string  `json:"environment"`
	Scope       string  `json:"scope"`
	LastFour    string  `json:"last_four"`
	Token       string  `json:"token"`
	RevokedAt   *string `json:"revoked_at"`
	LastUsedAt  *string `json:"last_used_at"`
	CreatedAt   string  `json:"created_at"`
}

func ingestionTokensPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/ingestion_tokens"
}

// ListIngestionTokens returns a project's tokens (masked; revoked ones may be
// included with revoked_at set).
func (c *Client) ListIngestionTokens(ctx context.Context, projectID int64) ([]IngestionToken, error) {
	return List[IngestionToken](ctx, c, ingestionTokensPath(projectID))
}

// FindIngestionToken returns the active token with the given id or a 404
// *Error (a revoked token counts as gone).
func (c *Client) FindIngestionToken(ctx context.Context, projectID, id int64) (*IngestionToken, error) {
	tokens, err := c.ListIngestionTokens(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for i := range tokens {
		if tokens[i].ID == id && tokens[i].RevokedAt == nil {
			return &tokens[i], nil
		}
	}
	return nil, &Error{
		Method: http.MethodGet, Path: ingestionTokensPath(projectID), Status: http.StatusNotFound, Code: CodeNotFound,
		Message: fmt.Sprintf("Ingestion token %d is not active in project %d", id, projectID),
	}
}

// CreateIngestionToken mints a token. The response carries the plaintext once.
func (c *Client) CreateIngestionToken(ctx context.Context, projectID int64, fields Fields, idempotencyKey string) (*IngestionToken, error) {
	return CreateWithReplayGuard(ctx, idempotencyKey,
		func(ctx context.Context, key string) (*IngestionToken, error) {
			var t IngestionToken
			if err := c.Post(ctx, ingestionTokensPath(projectID), map[string]any{"ingestion_token": fields}, &t, WithIdempotencyKey(key)); err != nil {
				return nil, err
			}
			return &t, nil
		},
		func(ctx context.Context, created *IngestionToken) error {
			_, err := c.FindIngestionToken(ctx, projectID, created.ID)
			return err
		})
}

// RevokeIngestionToken revokes a token (the API's DELETE); 404 is success.
func (c *Client) RevokeIngestionToken(ctx context.Context, id int64) error {
	err := c.Delete(ctx, "/ingestion_tokens/"+strconv.FormatInt(id, 10), nil)
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
