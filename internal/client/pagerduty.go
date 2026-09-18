package client

import (
	"context"
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

// ResourceID implements Identified. The link has no id of its own because the
// project IS its identity — which is also what lets a create be verified.
func (p *PagerDuty) ResourceID() int64 { return p.ProjectID }

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

// CreatePagerDuty creates the link through the verified create path.
// `routing_key` is required; the API refuses a null or blank one rather than
// storing a credential-less integration.
//
// Verification matters here even though the resource has no id of its own. The
// Idempotency-Key is derived from the payload, so re-creating an identical
// declaration inside the 24-hour window replays the original 201 — and if the
// link was deleted in between, that replay describes a link the server no
// longer has. The CodePagerDutyAlreadyConfigured guard cannot catch it, since
// a replayed request never reaches the controller. A GET on the singleton is
// the authoritative answer, and a 404 there is exactly the signal
// CreateResource re-POSTs on, under a fresh key.
func (c *Client) CreatePagerDuty(ctx context.Context, projectID int64, fields Fields, idempotencyKey string) (*PagerDuty, error) {
	return CreateResource(ctx, c, pagerDutyPath(projectID), pagerDutyRoot, fields, idempotencyKey,
		VerifyByGet(func(ctx context.Context, id int64) (*PagerDuty, error) {
			return c.GetPagerDuty(ctx, id)
		}))
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
