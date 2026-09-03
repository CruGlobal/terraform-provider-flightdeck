package client

import (
	"context"
	"strconv"
)

// SelfHealing is the per-project self-healing control-loop resource at
// GET/PATCH /api/v1/projects/:project_id/self-healing. It lives on the project
// row, so LockVersion is the PROJECT's lock_version: the same token
// PATCH /api/v1/projects/:id uses, and a write here bumps it.
type SelfHealing struct {
	ProjectID        int64             `json:"project_id"`
	FeatureEnabled   bool              `json:"feature_enabled"`
	GloballyDisarmed bool              `json:"globally_disarmed"`
	Config           SelfHealingConfig `json:"config"`
	LockVersion      int64             `json:"lock_version"`
}

// SelfHealingConfig is the API's resolved self-healing config, the values
// (defaults applied). Armed is read-only over the API: a write that would
// change it is refused with code arming_refused.
type SelfHealingConfig struct {
	Armed                 bool    `json:"armed"`
	BakeMinutes           int64   `json:"bake_minutes"`
	BaselineMultiplier    float64 `json:"baseline_multiplier"`
	AbsoluteFloor         float64 `json:"absolute_floor"`
	LongWindowMinutes     int64   `json:"long_window_minutes"`
	ShortWindowMinutes    int64   `json:"short_window_minutes"`
	BurnRate              float64 `json:"burn_rate"`
	SustainCount          int64   `json:"sustain_count"`
	ConsecutiveErrorLimit int64   `json:"consecutive_error_limit"`
	CooldownMinutes       int64   `json:"cooldown_minutes"`
	MaxRollbacksPerHour   int64   `json:"max_rollbacks_per_hour"`
	RecoveryWindowMinutes int64   `json:"recovery_window_minutes"`
}

// SelfHealingThresholdKeys are the writable self_healing settings, in the order
// the API documents them. "armed" is deliberately absent.
var SelfHealingThresholdKeys = []string{
	"bake_minutes", "baseline_multiplier", "absolute_floor", "long_window_minutes",
	"short_window_minutes", "burn_rate", "sustain_count", "consecutive_error_limit",
	"cooldown_minutes", "max_rollbacks_per_hour", "recovery_window_minutes",
}

const selfHealingRoot = "self_healing"

func selfHealingPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/self-healing"
}

// GetSelfHealing reads a project's resolved self-healing config. Workspace
// admins only: other tokens get a 403. A deployment without the endpoint 404s.
func (c *Client) GetSelfHealing(ctx context.Context, projectID int64) (*SelfHealing, error) {
	return GetResource[*SelfHealing](ctx, c, selfHealingPath(projectID), selfHealingRoot)
}

// UpdateSelfHealing PATCHes threshold settings (the keys of Config, never
// `armed`) under an If-Match carrying the PROJECT's lock_version.
func (c *Client) UpdateSelfHealing(ctx context.Context, projectID int64, settings Fields, projectLockVersion int64) (*SelfHealing, error) {
	return PatchResource[*SelfHealing](ctx, c, selfHealingPath(projectID), selfHealingRoot, settings, &projectLockVersion)
}
