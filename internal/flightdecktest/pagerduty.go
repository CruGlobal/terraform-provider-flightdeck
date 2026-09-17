package flightdecktest

import (
	"net/http"
	"strings"
	"time"
)

var pagerDutySeverities = []string{"critical", "error", "warning", "info"}

// PagerDuty is the fake's stored per-project link. Plaintext stands in for the
// encrypted credential: it is kept so a rotation can be observed through
// LastFour, and is never serialized.
type PagerDuty struct {
	ProjectID   int64
	Plaintext   string
	LastFour    string
	Enabled     bool
	MinSeverity string
	ServiceID   *string
	ServiceURL  *string
	LockVersion int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type pagerDutyStore struct{ byProject map[int64]*PagerDuty }

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["pagerduty"] = &pagerDutyStore{byProject: map[int64]*PagerDuty{}}
		mux.HandleFunc("GET /api/v1/projects/{project_id}/pagerduty", s.showPagerDuty)
		mux.HandleFunc("POST /api/v1/projects/{project_id}/pagerduty", s.createPagerDuty)
		mux.HandleFunc("PATCH /api/v1/projects/{project_id}/pagerduty", s.updatePagerDuty)
		mux.HandleFunc("DELETE /api/v1/projects/{project_id}/pagerduty", s.destroyPagerDuty)
	})
}

func (s *Server) pagerDuty() *pagerDutyStore {
	store, _ := s.stores["pagerduty"].(*pagerDutyStore)
	return store
}

// PagerDutyLink returns a project's stored link, or nil.
func (s *Server) PagerDutyLink(projectID int64) *PagerDuty {
	s.mu.Lock()
	defer s.mu.Unlock()
	pd := s.pagerDuty().byProject[projectID]
	if pd == nil {
		return nil
	}
	cp := *pd
	return &cp
}

// RotatePagerDutyKeyOutOfBand replaces the stored credential the way someone
// editing the Flightdeck settings page would, so drift detection can be tested.
func (s *Server) RotatePagerDutyKeyOutOfBand(projectID int64, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pd := s.pagerDuty().byProject[projectID]; pd != nil {
		pd.Plaintext = key
		pd.LastFour = lastFourOf(key)
		pd.LockVersion++
		pd.UpdatedAt = time.Now()
	}
}

// lastFourOf is the API's masking rule: the trailing four characters.
func lastFourOf(key string) string {
	runes := []rune(key)
	if len(runes) <= 4 {
		return key
	}
	return string(runes[len(runes)-4:])
}

// serializePagerDuty mirrors the API's shape. The credential itself is never
// in it — only the last four characters and the masked form.
func serializePagerDuty(pd *PagerDuty) map[string]any {
	out := map[string]any{
		"project_id": pd.ProjectID, "enabled": pd.Enabled,
		"routing_key_last_four": pd.LastFour, "routing_key_masked": "…" + pd.LastFour,
		"service_id": nil, "service_url": nil, "min_severity": pd.MinSeverity,
		"lock_version": pd.LockVersion, "created_at": iso(pd.CreatedAt), "updated_at": iso(pd.UpdatedAt),
	}
	if pd.ServiceID != nil {
		out["service_id"] = *pd.ServiceID
	}
	if pd.ServiceURL != nil {
		out["service_url"] = *pd.ServiceURL
	}
	return out
}

// applyPagerDutyAttrs mirrors the API's write rules. Every key merges: absent
// keeps the stored value. routing_key is the exception that matters — null is
// refused outright, because that would leave an integration with no credential.
func applyPagerDutyAttrs(pd *PagerDuty, attrs map[string]any, creating bool) (int, string, string) {
	for k := range attrs {
		switch k {
		case "routing_key", "service_id", "service_url", "min_severity", "enabled", "lock_version":
		default:
			return http.StatusUnprocessableEntity, "invalid_attribute",
				"unknown key: " + k + " (settable: routing_key, service_id, service_url, min_severity, enabled)"
		}
	}
	key, hasKey := attrs["routing_key"]
	switch {
	case hasKey && key == nil:
		return http.StatusUnprocessableEntity, "invalid_attribute",
			"routing_key cannot be set to null — that would leave a PagerDuty integration with no credential. " +
				"Omit the key to keep the stored one, send a new value to rotate it, or DELETE this resource to " +
				"remove the integration entirely."
	case hasKey:
		v, isString := key.(string)
		if !isString {
			return http.StatusUnprocessableEntity, "invalid_attribute", "routing_key must be a string"
		}
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return http.StatusUnprocessableEntity, "validation_failed", "Routing key can't be blank"
		}
		if strings.ContainsAny(trimmed, " \t\n") {
			return http.StatusUnprocessableEntity, "validation_failed", "Routing key must not contain whitespace"
		}
		// Deliberately no format check: the API accepts any non-blank,
		// whitespace-free string.
		pd.Plaintext = trimmed
		pd.LastFour = lastFourOf(trimmed)
	case creating:
		return http.StatusUnprocessableEntity, "invalid_attribute",
			"routing_key is required — it is the PagerDuty Events API v2 integration key for the service this project pages through"
	}
	// A null enabled is "no opinion", as is omitting it.
	if v, has := attrs["enabled"]; has && v != nil {
		switch v.(type) {
		case bool, string:
			pd.Enabled = truthy(v)
		default:
			return http.StatusUnprocessableEntity, "invalid_attribute", "enabled must be true or false"
		}
	}
	if v, has := attrs["min_severity"]; has {
		if v == nil {
			pd.MinSeverity = "error"
		} else {
			sev := asString(v)
			if !contains(pagerDutySeverities, sev) {
				return http.StatusUnprocessableEntity, "invalid_attribute",
					"min_severity must be one of: " + strings.Join(pagerDutySeverities, ", ") +
						" (got \"" + sev + "\"; send null to reset it to error)"
			}
			pd.MinSeverity = sev
		}
	}
	if v, has := attrs["service_id"]; has {
		if v == nil || strings.TrimSpace(asString(v)) == "" {
			pd.ServiceID = nil
		} else {
			id := strings.TrimSpace(asString(v))
			pd.ServiceID = &id
		}
	}
	if v, has := attrs["service_url"]; has {
		if v == nil || strings.TrimSpace(asString(v)) == "" {
			pd.ServiceURL = nil
		} else {
			u := strings.TrimSpace(asString(v))
			if !httpURL.MatchString(u) {
				return http.StatusUnprocessableEntity, "validation_failed", "Service url must be an http:// or https:// URL"
			}
			pd.ServiceURL = &u
		}
	}
	return 0, "", ""
}

// pagerDutyChanged compares the settable fields by VALUE. A struct comparison
// would compare the *string pointers instead, and report a change whenever the
// same label was re-sent.
func pagerDutyChanged(before, after *PagerDuty) bool {
	return before.Plaintext != after.Plaintext ||
		before.Enabled != after.Enabled ||
		before.MinSeverity != after.MinSeverity ||
		!sameOptionalString(before.ServiceID, after.ServiceID) ||
		!sameOptionalString(before.ServiceURL, after.ServiceURL)
}

func sameOptionalString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (s *Server) showPagerDuty(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.liveProject(pid) == nil {
		notFound(w)
		return
	}
	if !s.requireWorkspaceAdmin(w) {
		return
	}
	pd := s.pagerDuty().byProject[pid]
	if pd == nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, serializePagerDuty(pd))
}

func (s *Server) createPagerDuty(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "pagerduty")
	if !ok {
		return
	}
	s.withIdempotency(w, r, "pagerduty", func() (int, any) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.liveProject(pid) == nil {
			return http.StatusNotFound, errorBody("Not found", "not_found")
		}
		// Same bar as show/update/destroy: linking spends a paging credential.
		// Checked after the existence test so a non-admin cannot tell project
		// ids apart by 403 vs 404.
		if !s.workspaceAdmin {
			return http.StatusForbidden, errorBody("This action requires workspace owner or admin rights.", "forbidden")
		}
		if s.pagerDuty().byProject[pid] != nil {
			return http.StatusUnprocessableEntity, errorBody(
				"this project already has a PagerDuty integration — PATCH it to rotate the routing key or change its "+
					"settings, or DELETE it first to replace it outright. There is one credential per project.",
				"pagerduty_already_configured")
		}
		now := time.Now()
		pd := &PagerDuty{ProjectID: pid, Enabled: true, MinSeverity: "error", CreatedAt: now, UpdatedAt: now}
		if status, code, msg := applyPagerDutyAttrs(pd, attrs, true); status != 0 {
			return status, errorBody(msg, code)
		}
		s.pagerDuty().byProject[pid] = pd
		return http.StatusCreated, serializePagerDuty(pd)
	})
}

func (s *Server) updatePagerDuty(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "pagerduty")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.liveProject(pid) == nil {
		notFound(w)
		return
	}
	if !s.requireWorkspaceAdmin(w) {
		return
	}
	pd := s.pagerDuty().byProject[pid]
	if pd == nil {
		notFound(w)
		return
	}
	if !checkIfMatch(w, r, pd.LockVersion) {
		return
	}
	candidate := *pd
	if status, code, msg := applyPagerDutyAttrs(&candidate, attrs, false); status != 0 {
		writeError(w, status, code, msg)
		return
	}
	// A write that changes nothing is a true no-op: lock_version does not move.
	if pagerDutyChanged(pd, &candidate) {
		candidate.LockVersion++
		candidate.UpdatedAt = time.Now()
		*pd = candidate
	}
	writeJSON(w, http.StatusOK, serializePagerDuty(pd))
}

// Destroy is a hard delete: the stored credential is dropped and GET 404s again.
func (s *Server) destroyPagerDuty(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.liveProject(pid) == nil {
		notFound(w)
		return
	}
	if !s.requireWorkspaceAdmin(w) {
		return
	}
	pd := s.pagerDuty().byProject[pid]
	if pd == nil {
		notFound(w)
		return
	}
	if !checkIfMatch(w, r, pd.LockVersion) {
		return
	}
	delete(s.pagerDuty().byProject, pid)
	w.WriteHeader(http.StatusNoContent)
}
