package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// PagerDuty is a project's link to a PagerDuty service: the Events API v2
// integration key Flightdeck forwards a signal to when something needs a
// human, plus what it forwards and a couple of display-only labels.
//
// It is a SINGLETON per project and has no id of its own — the project scopes
// it. POST creates it, PATCH changes it, DELETE removes it outright; a second
// POST is refused with CodePagerDutyAlreadyConfigured rather than replacing
// the stored credential.
//
// The routing key itself is never returned. Flightdeck stores it encrypted
// (it has to replay it to PagerDuty, so it cannot be a one-way digest) but
// does not hand it back on any route. Reads report RoutingKeyLastFour and
// RoutingKeyMasked instead, which is four characters of signal and the only
// thing a caller can compare a stored credential against.
type PagerDuty struct {
	ProjectID          int64   `json:"project_id"`
	Enabled            bool    `json:"enabled"`
	RoutingKeyLastFour string  `json:"routing_key_last_four"`
	RoutingKeyMasked   string  `json:"routing_key_masked"`
	ServiceID          *string `json:"service_id"`
	ServiceURL         *string `json:"service_url"`
	MinSeverity        string  `json:"min_severity"`
	LockVersion        int64   `json:"lock_version"`
	CreatedAt          string  `json:"created_at"`
	UpdatedAt          string  `json:"updated_at"`
}

// PagerDutyMinSeverities are the severities the link will forward at or above.
// They are the incident severities, so the allowlist lives in one place.
var PagerDutyMinSeverities = IncidentSeverities

const pagerDutyRoot = "pagerduty"

func pagerDutyPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/pagerduty"
}

// GetPagerDuty reads a project's PagerDuty link. A project without one answers
// 404, which is how absence is spelled.
func (c *Client) GetPagerDuty(ctx context.Context, projectID int64) (*PagerDuty, error) {
	return GetResource[*PagerDuty](ctx, c, pagerDutyPath(projectID), pagerDutyRoot)
}

// CreatePagerDuty POSTs the link. `routing_key` is required; the API refuses a
// null or blank one rather than storing a credential-less integration.
//
// This does not go through CreateResource: the resource has no id, so there is
// nothing to verify a create against. It does not need one — the singleton is
// its own guard, because a second POST is a 422 CodePagerDutyAlreadyConfigured
// instead of a duplicate row.
func (c *Client) CreatePagerDuty(ctx context.Context, projectID int64, fields Fields, idempotencyKey string) (*PagerDuty, error) {
	path := pagerDutyPath(projectID)
	var raw json.RawMessage
	if err := c.Post(ctx, path, map[string]any{pagerDutyRoot: fields}, &raw, WithIdempotencyKey(idempotencyKey)); err != nil {
		return nil, err
	}
	out, err := DecodeResource[*PagerDuty](raw, pagerDutyRoot)
	if err != nil {
		return nil, &Error{Method: http.MethodPost, Path: path, Status: http.StatusCreated, Err: err,
			Message: fmt.Sprintf("create response could not be decoded as a %s: %s", pagerDutyRoot, err)}
	}
	return out, nil
}

// UpdatePagerDuty PATCHes the link with an If-Match precondition carrying the
// link's OWN lock_version (not the project's).
//
// Every key MERGES: an absent key keeps its stored value. `routing_key` is the
// exception that matters — omitting it keeps the stored credential, sending a
// new value rotates it, and sending null is refused, because that would leave
// the integration with nothing to page through. `min_severity` sent as null
// resets to the default; `enabled` sent as null is a no-op.
func (c *Client) UpdatePagerDuty(ctx context.Context, projectID int64, fields Fields, lockVersion int64) (*PagerDuty, error) {
	return PatchResource[*PagerDuty](ctx, c, pagerDutyPath(projectID), pagerDutyRoot, fields, &lockVersion)
}

// DeletePagerDuty removes the link under an If-Match precondition; 404 is
// success. This is a hard delete: the stored credential is dropped and nothing
// is forwarded afterwards. It does not invalidate the key on PagerDuty's side
// — regenerate the integration key on the PagerDuty service to do that.
func (c *Client) DeletePagerDuty(ctx context.Context, projectID, lockVersion int64) error {
	err := c.Delete(ctx, pagerDutyPath(projectID), nil, WithIfMatch(lockVersion))
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
