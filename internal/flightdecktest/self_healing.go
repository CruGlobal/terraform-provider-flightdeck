package flightdecktest

import (
	"net/http"
	"strings"
)

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["self_healing"] = &selfHealingStore{enabled: true, blockers: map[int64][]RollbackBlocker{}}
		mux.HandleFunc("GET /api/v1/projects/{project_id}/self-healing", s.showSelfHealing)
		mux.HandleFunc("PATCH /api/v1/projects/{project_id}/self-healing", s.updateSelfHealing)
	})
}

// selfHealingStore holds the endpoint's knobs.
type selfHealingStore struct {
	// enabled=false simulates a Flightdeck without the endpoint: every route 404s.
	enabled bool
	// legacy simulates a Flightdeck from before the `rollback` setting: the
	// read has no rollback, rollback_blockers or count_browser_errors, a
	// write naming either new setting is an unknown key, and any change to
	// `armed` is refused with arming_refused, as it used to be.
	legacy bool
	// blockers overrides the release-side rollback blockers per project; a
	// project absent here reports that it has no release yet.
	blockers map[int64][]RollbackBlocker
}

// RollbackBlocker is one entry of the read's rollback_blockers.
type RollbackBlocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// defaultReleaseBlocker is what a project the fake knows no releases for
// reports: the loop has nothing live to roll back from.
var defaultReleaseBlocker = RollbackBlocker{
	Code:    "no_current_release",
	Message: "No release has been recorded for production yet. Promote twice through the pipeline so the loop has two trusted releases to work with.",
}

func (s *Server) selfHealingStore() *selfHealingStore {
	st, _ := s.stores["self_healing"].(*selfHealingStore)
	return st
}

// SetSelfHealingEndpoint enables or disables the self-healing routes, to
// simulate a Flightdeck version that does not expose them.
func (s *Server) SetSelfHealingEndpoint(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selfHealingStore().enabled = on
}

// SetSelfHealingLegacy makes the endpoint behave like a Flightdeck from
// before the `rollback` setting (see selfHealingStore.legacy).
func (s *Server) SetSelfHealingLegacy(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selfHealingStore().legacy = on
}

// SetRollbackBlockers sets the release-side blockers a project reports, in
// place of the default "no release yet". An empty list means the project is
// ready as far as releases go; feature_off is still added while the feature
// is off, as the API does.
func (s *Server) SetRollbackBlockers(projectID int64, blockers []RollbackBlocker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selfHealingStore().blockers[projectID] = append([]RollbackBlocker{}, blockers...)
}

// SelfHealingDefaults are the API's documented threshold defaults.
var SelfHealingDefaults = map[string]any{
	"armed": false, "count_browser_errors": false, "bake_minutes": int64(20), "baseline_multiplier": 5.0, "absolute_floor": 5.0,
	"long_window_minutes": int64(60), "short_window_minutes": int64(5), "burn_rate": 14.4,
	"sustain_count": int64(3), "consecutive_error_limit": int64(3), "cooldown_minutes": int64(30),
	"max_rollbacks_per_hour": int64(1), "recovery_window_minutes": int64(15),
}

func resolveSelfHealing(overrides map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range SelfHealingDefaults {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// ArmSelfHealing sets the mode directly (no HTTP), the way a console change
// does: true is auto-rollback, false is report only.
func (s *Server) ArmSelfHealing(projectID int64, armed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.projects().byID[projectID]; p != nil {
		if p.SelfHealing == nil {
			p.SelfHealing = map[string]any{}
		}
		p.SelfHealing["armed"] = armed
	}
}

// selfHealingLimits are the API's per-setting ranges.
type selfHealingLimit struct {
	decimal  bool
	min, max float64 // min is inclusive for integers, exclusive (`above`) for decimals
}

var selfHealingLimits = map[string]selfHealingLimit{
	"bake_minutes":            {min: 1, max: 1440},
	"baseline_multiplier":     {decimal: true, min: 0, max: 1000},
	"absolute_floor":          {decimal: true, min: 0, max: 100000},
	"long_window_minutes":     {min: 1, max: 1440},
	"short_window_minutes":    {min: 1, max: 1440},
	"burn_rate":               {decimal: true, min: 0, max: 1000},
	"sustain_count":           {min: 1, max: 100},
	"consecutive_error_limit": {min: 1, max: 100},
	"cooldown_minutes":        {min: 1, max: 1440},
	"max_rollbacks_per_hour":  {min: 1, max: 100},
	"recovery_window_minutes": {min: 1, max: 1440},
}

// applySelfHealing mirrors the API's write rules: only submitted keys change,
// a nil clears a threshold override, unknown keys are refused, values are
// coerced per key and range-checked (non-positive values are refused because
// the engine reads them as "no limit"), and short/long windows must stay
// coherent. The whole payload is validated before anything changes.
//
// The mode is `rollback`, "report" or "auto" (trimmed, any case); null or
// blank is no opinion. `armed` is its old name: re-sending the stored value
// (or blank) is fine even next to a rollback that changes the mode, but a
// change through armed that rollback does not also make is refused. Turning
// feature_enabled on for a project stored as auto is going live and is
// refused with arming_refused unless the same write names rollback.
func (s *Server) applySelfHealing(p *Project, submitted map[string]any) (int, string, string) {
	legacy := s.selfHealingStore().legacy
	resolved := resolveSelfHealing(p.SelfHealing)
	storedAuto := truthy(resolved["armed"])
	switches := map[string]bool{"armed": true, "feature_enabled": true, "lock_version": true}
	if !legacy {
		switches["rollback"] = true
		switches["count_browser_errors"] = true
	}
	for k := range submitted {
		if _, known := selfHealingLimits[k]; !known && !switches[k] {
			return http.StatusUnprocessableEntity, "invalid_attribute", "unknown self-healing setting: " + k
		}
	}
	blank := func(v any) bool { return v == nil || strings.TrimSpace(asString(v)) == "" }
	boolSwitch := func(key string) (bool, bool, string) {
		v, has := submitted[key]
		if !has || blank(v) {
			return false, false, ""
		}
		switch t := v.(type) {
		case bool:
			return t, true, ""
		case string:
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "true", "1", "on":
				return true, true, ""
			case "false", "0", "off":
				return false, true, ""
			}
		}
		return false, false, key + " must be true or false"
	}

	// rollback: the mode, "report" or "auto".
	var wantAuto, modeNamed bool
	if v, has := submitted["rollback"]; has && !blank(v) {
		switch strings.ToLower(strings.TrimSpace(asString(v))) {
		case "auto":
			wantAuto, modeNamed = true, true
		case "report":
			wantAuto, modeNamed = false, true
		default:
			return http.StatusUnprocessableEntity, "invalid_attribute", `rollback must be "report" or "auto"`
		}
	}
	// armed: accepted when it matches the stored value, or agrees with a
	// rollback in the same request.
	if v, has := submitted["armed"]; has && !blank(v) {
		armed := truthy(v)
		switch {
		case armed == storedAuto:
		case legacy:
			return http.StatusUnprocessableEntity, "arming_refused",
				"armed cannot be changed over the API. Arming the automated-rollback loop is operator-provisioned from the console on purpose."
		case !modeNamed || armed != wantAuto:
			return http.StatusUnprocessableEntity, "invalid_attribute",
				`armed cannot be changed directly; set rollback to "report" or "auto" instead`
		}
	}
	featureOn, featureNamed, msg := boolSwitch("feature_enabled")
	if msg != "" {
		return http.StatusUnprocessableEntity, "invalid_attribute", msg
	}
	countBrowser, countNamed, msg := boolSwitch("count_browser_errors")
	if msg != "" {
		return http.StatusUnprocessableEntity, "invalid_attribute", msg
	}
	// The go-live guard: turning the feature on over a stored auto has to say
	// which mode to go live in.
	wasOn := s.selfHealingFeatureOn(p)
	if !legacy && featureNamed && featureOn && !wasOn && storedAuto && !modeNamed {
		return http.StatusUnprocessableEntity, "arming_refused",
			`This project is set to auto-rollback, so turning self-healing on would start live rollbacks. Send rollback: "auto" to go live with auto-rollback, or rollback: "report" to start in report only.`
	}
	next := map[string]any{}
	for k, v := range p.SelfHealing {
		next[k] = v
	}
	for k, v := range submitted {
		if switches[k] {
			continue
		}
		if v == nil {
			delete(next, k)
			continue
		}
		limit := selfHealingLimits[k]
		f, ok := asFloat64(v)
		if !ok {
			return http.StatusUnprocessableEntity, "invalid_attribute", k + " must be a number"
		}
		if limit.decimal {
			if f <= limit.min || f > limit.max {
				return http.StatusUnprocessableEntity, "invalid_attribute", k + " must be greater than 0 and at most " + asString(limit.max)
			}
			next[k] = f
		} else {
			if f != float64(int64(f)) {
				return http.StatusUnprocessableEntity, "invalid_attribute", k + " must be a whole number of minutes or times"
			}
			if f < limit.min || f > limit.max {
				return http.StatusUnprocessableEntity, "invalid_attribute", k + " must be between 1 and " + asString(limit.max)
			}
			next[k] = int64(f)
		}
	}
	if modeNamed {
		next["armed"] = wantAuto
	}
	if countNamed {
		next["count_browser_errors"] = countBrowser
	}
	after := resolveSelfHealing(next)
	shortW, _ := asFloat64(after["short_window_minutes"])
	longW, _ := asFloat64(after["long_window_minutes"])
	if shortW > longW {
		// The API names both values, and it compares the MERGED pair — so a
		// write naming only one window can be refused by the other's stored
		// value. Tests match on this shape, so keep it close to the API's.
		return http.StatusUnprocessableEntity, "invalid_attribute",
			"short_window_minutes (" + asString(int64(shortW)) + ") cannot exceed long_window_minutes (" + asString(int64(longW)) + ")"
	}
	p.SelfHealing = next
	// feature_enabled lives on the project's feature map, not among the
	// thresholds, and a null is a no-op rather than a reset.
	if featureNamed {
		if p.Features == nil {
			p.Features = map[string]bool{}
		}
		p.Features["self_healing"] = featureOn
	}
	return 0, "", ""
}

func (s *Server) selfHealingFeatureOn(p *Project) bool {
	if on, stored := p.Features["self_healing"]; stored {
		return on
	}
	return DefaultFeatures["self_healing"]
}

// rollbackBlockers is the fake's rollback_blockers: the feature switch, then
// the release-side blockers the test set (or "no release yet").
func (s *Server) rollbackBlockers(p *Project) []RollbackBlocker {
	out := []RollbackBlocker{}
	if !s.selfHealingFeatureOn(p) {
		out = append(out, RollbackBlocker{Code: "feature_off",
			Message: "Self-healing is off for this project. Turn feature_enabled on for the loop to act."})
	}
	release, set := s.selfHealingStore().blockers[p.ID]
	if !set {
		release = []RollbackBlocker{defaultReleaseBlocker}
	}
	return append(out, release...)
}

func (s *Server) serializeSelfHealing(p *Project) map[string]any {
	overrides := map[string]any{}
	for k, v := range p.SelfHealing {
		overrides[k] = v
	}
	config := resolveSelfHealing(p.SelfHealing)
	writable := make([]string, 0, len(selfHealingLimits)+3)
	for k := range selfHealingLimits {
		writable = append(writable, k)
	}
	writable = append(writable, "feature_enabled")
	out := map[string]any{
		"project_id": p.ID, "feature_enabled": s.selfHealingFeatureOn(p), "globally_disarmed": false,
		"config": config, "overrides": overrides,
		"lock_version": p.LockVersion, "updated_at": iso(p.UpdatedAt),
	}
	if s.selfHealingStore().legacy {
		delete(config, "count_browser_errors")
		delete(overrides, "count_browser_errors")
	} else {
		writable = append(writable, "rollback", "count_browser_errors")
		out["rollback"] = "report"
		if truthy(config["armed"]) {
			out["rollback"] = "auto"
		}
		out["rollback_blockers"] = s.rollbackBlockers(p)
	}
	out["writable_settings"] = writable
	return out
}

// The existence check runs before the admin bar so a non-admin cannot tell
// project ids apart by 403 vs 404.
func (s *Server) showSelfHealing(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.liveProject(pid)
	if p == nil || !s.selfHealingStore().enabled {
		notFound(w)
		return
	}
	if !s.requireWorkspaceAdmin(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.serializeSelfHealing(p))
}

func (s *Server) updateSelfHealing(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	s.mu.Lock()
	p := s.liveProject(pid)
	if p == nil || !s.selfHealingStore().enabled {
		s.mu.Unlock()
		notFound(w)
		return
	}
	if !s.requireWorkspaceAdmin(w) {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	attrs, ok := decodeBody(w, r, "self_healing")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The precondition pins the PROJECT's lock_version.
	if !checkIfMatch(w, r, p.LockVersion) {
		return
	}
	// A struct copy shares the maps, and applySelfHealing writes to both, so
	// deep-copy each: a rejected write must leave the project untouched.
	candidate := *p
	candidate.SelfHealing = map[string]any{}
	for k, v := range p.SelfHealing {
		candidate.SelfHealing[k] = v
	}
	candidate.Features = map[string]bool{}
	for k, v := range p.Features {
		candidate.Features[k] = v
	}
	if status, code, msg := s.applySelfHealing(&candidate, attrs); status != 0 {
		writeError(w, status, code, msg)
		return
	}
	candidate.LockVersion++
	*p = candidate
	writeJSON(w, http.StatusOK, s.serializeSelfHealing(p))
}
