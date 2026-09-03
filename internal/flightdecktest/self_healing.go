package flightdecktest

import (
	"net/http"
)

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["self_healing"] = &selfHealingStore{enabled: true}
		mux.HandleFunc("GET /api/v1/projects/{project_id}/self-healing", s.showSelfHealing)
		mux.HandleFunc("PATCH /api/v1/projects/{project_id}/self-healing", s.updateSelfHealing)
	})
}

// selfHealingStore holds the endpoint's knobs.
type selfHealingStore struct {
	// enabled=false simulates a Flightdeck without the endpoint: every route 404s.
	enabled bool
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

// SelfHealingDefaults mirrors SelfHealing::Config::DEFAULTS.
var SelfHealingDefaults = map[string]any{
	"armed": false, "bake_minutes": int64(20), "baseline_multiplier": 5.0, "absolute_floor": 5.0,
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

// ArmSelfHealing flips the console-only armed flag directly (no HTTP).
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

// selfHealingLimits mirrors Api::SelfHealingAttributes::LIMITS.
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

// applySelfHealing mirrors Api::SelfHealingAttributes.apply!: only submitted
// keys change, a nil clears an override, `armed` may be re-sent unchanged (or
// sent null) but a change is refused with arming_refused, unknown keys are
// refused, values are coerced per key and range-checked (non-positive values
// are refused because the engine reads them as "no limit"), and short/long
// windows must stay coherent.
func (s *Server) applySelfHealing(p *Project, submitted map[string]any) (int, string, string) {
	resolved := resolveSelfHealing(p.SelfHealing)
	for k := range submitted {
		if _, known := selfHealingLimits[k]; !known && k != "armed" && k != "lock_version" {
			return http.StatusUnprocessableEntity, "invalid_attribute", "unknown self-healing setting: " + k
		}
	}
	if v, has := submitted["armed"]; has && v != nil {
		if truthy(v) != truthy(resolved["armed"]) {
			return http.StatusUnprocessableEntity, "arming_refused",
				"armed cannot be changed over the API. Arming the automated-rollback loop is operator-provisioned from the console on purpose."
		}
	}
	next := map[string]any{}
	for k, v := range p.SelfHealing {
		next[k] = v
	}
	for k, v := range submitted {
		if k == "armed" || k == "lock_version" {
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
	after := resolveSelfHealing(next)
	shortW, _ := asFloat64(after["short_window_minutes"])
	longW, _ := asFloat64(after["long_window_minutes"])
	if shortW > longW {
		return http.StatusUnprocessableEntity, "invalid_attribute", "short_window_minutes cannot exceed long_window_minutes"
	}
	p.SelfHealing = next
	return 0, "", ""
}

func (s *Server) serializeSelfHealing(p *Project) map[string]any {
	enabled, stored := p.Features["self_healing"]
	if !stored {
		enabled = DefaultFeatures["self_healing"]
	}
	overrides := map[string]any{}
	for k, v := range p.SelfHealing {
		overrides[k] = v
	}
	writable := make([]string, 0, len(selfHealingLimits))
	for k := range selfHealingLimits {
		writable = append(writable, k)
	}
	return map[string]any{
		"project_id": p.ID, "feature_enabled": enabled, "globally_disarmed": false,
		"config": resolveSelfHealing(p.SelfHealing), "overrides": overrides,
		"writable_settings": writable, "lock_version": p.LockVersion, "updated_at": iso(p.UpdatedAt),
	}
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
	candidate := *p
	candidate.SelfHealing = map[string]any{}
	for k, v := range p.SelfHealing {
		candidate.SelfHealing[k] = v
	}
	if status, code, msg := s.applySelfHealing(&candidate, attrs); status != 0 {
		writeError(w, status, code, msg)
		return
	}
	candidate.LockVersion++
	*p = candidate
	writeJSON(w, http.StatusOK, s.serializeSelfHealing(p))
}
