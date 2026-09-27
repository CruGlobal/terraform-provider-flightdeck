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
	ProjectID      int64 `json:"project_id"`
	FeatureEnabled bool  `json:"feature_enabled"`
	// Rollback is the loop's mode, RollbackReport or RollbackAuto. It is nil
	// on a Flightdeck that predates the setting; RollbackMode falls back to
	// config.armed there.
	Rollback *string `json:"rollback"`
	// RollbackBlockers lists what would stop auto-rollback on this project
	// right now, empty when nothing Flightdeck can check would. It is filled in
	// either mode, and changes with every deploy. Read-only.
	RollbackBlockers []RollbackBlocker `json:"rollback_blockers"`
	GloballyDisarmed bool              `json:"globally_disarmed"`
	Config           SelfHealingConfig `json:"config"`
	LockVersion      int64             `json:"lock_version"`
	// WritableSettings is the endpoint's own list of what it accepts on a
	// write. It is reported so the provider's idea of that set can be checked
	// against the API's rather than only asserted in a comment.
	WritableSettings []string `json:"writable_settings"`
}

// RollbackBlocker is one reason auto-rollback could not act right now. Code
// is a stable slug; Message says what to do about it.
type RollbackBlocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// The two modes of the self-healing loop. In report only it notes what it
// would do and never touches production; in auto-rollback it rolls production
// back by itself when every check passes.
const (
	RollbackReport = "report"
	RollbackAuto   = "auto"
)

// RollbackModes are the values the API accepts for `rollback`.
var RollbackModes = []string{RollbackReport, RollbackAuto}

// RollbackMode returns the loop's mode. A Flightdeck that predates the
// `rollback` setting has no top-level value, so it is derived from
// config.armed, which the API documents as true exactly when the mode is auto.
func (sh *SelfHealing) RollbackMode() string {
	if sh.Rollback != nil && *sh.Rollback != "" {
		return *sh.Rollback
	}
	if sh.Config.Armed {
		return RollbackAuto
	}
	return RollbackReport
}

// Accepts reports whether the endpoint takes key on a write, by its own
// writable_settings. A read without the list says nothing either way, so it
// counts as accepting.
func (sh *SelfHealing) Accepts(key string) bool {
	if len(sh.WritableSettings) == 0 {
		return true
	}
	for _, k := range sh.WritableSettings {
		if k == key {
			return true
		}
	}
	return false
}

// SelfHealingConfig is the API's resolved self-healing config, the values
// (defaults applied). Armed is the old name for the mode, true exactly when
// `rollback` is auto; it is still reported for older clients, and a write
// that changes it directly is refused. CountBrowserErrors is nil on a
// Flightdeck that predates it.
type SelfHealingConfig struct {
	Armed                 bool    `json:"armed"`
	CountBrowserErrors    *bool   `json:"count_browser_errors"`
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

// SelfHealingThresholdKeys are the writable threshold settings, in the order
// the API documents them. A threshold sent as null drops the project's
// override and restores the default.
var SelfHealingThresholdKeys = []string{
	"bake_minutes", "baseline_multiplier", "absolute_floor", "long_window_minutes",
	"short_window_minutes", "burn_rate", "sustain_count", "consecutive_error_limit",
	"cooldown_minutes", "max_rollbacks_per_hour", "recovery_window_minutes",
}

// SelfHealingSwitchKeys are the writable settings that are not thresholds:
// for each, null or blank is "no opinion" rather than a reset. The endpoint's
// full writable set is these plus SelfHealingThresholdKeys. "armed" is
// deliberately absent: the mode is set through "rollback".
var SelfHealingSwitchKeys = []string{"feature_enabled", "rollback", "count_browser_errors"}

const selfHealingRoot = "self_healing"

func selfHealingPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/self-healing"
}

// GetSelfHealing reads a project's resolved self-healing config. Workspace
// admins only: other tokens get a 403. A deployment without the endpoint 404s.
func (c *Client) GetSelfHealing(ctx context.Context, projectID int64) (*SelfHealing, error) {
	return GetResource[*SelfHealing](ctx, c, selfHealingPath(projectID), selfHealingRoot)
}

// UpdateSelfHealing PATCHes writable settings (the thresholds and the
// switches, never `armed`) under an If-Match carrying the PROJECT's
// lock_version.
//
// The endpoint MERGES: a key that is absent keeps its stored value, so a write
// only ever changes what it names. A threshold sent as JSON null drops the
// project's override and restores the documented default — the only way to
// spell "unset", since a default is not otherwise writable. A switch sent as
// null is a no-op rather than a reset.
//
// Turning `feature_enabled` on for a project stored as auto-rollback is
// refused with CodeArmingRefused unless the same write names `rollback`.
func (c *Client) UpdateSelfHealing(ctx context.Context, projectID int64, settings Fields, projectLockVersion int64) (*SelfHealing, error) {
	return PatchResource[*SelfHealing](ctx, c, selfHealingPath(projectID), selfHealingRoot, settings, &projectLockVersion)
}
