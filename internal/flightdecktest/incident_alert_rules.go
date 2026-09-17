package flightdecktest

import (
	"net/http"
	"sort"
	"strings"
)

var (
	incidentTriggers      = []string{"incident_opened", "incident_repeated"}
	incidentConditionKeys = []string{"min_severity", "count", "window_minutes"}
	incidentActionKeys    = []string{"notify_slack", "notify_email", "create_work_item", "file_intake", "notify_webhook"}
	incidentSeverities    = []string{"critical", "error", "warning", "info"}
	incidentPriorities    = []string{"none", "low", "medium", "high", "urgent"}
)

// IncidentPriorityDefaults is the API's severity-to-priority table. Only rows
// that differ from it are stored as overrides.
var IncidentPriorityDefaults = map[string]string{
	"critical": "urgent", "error": "high", "warning": "medium", "info": "low",
}

// Incident condition windows: repeats are folded per five-minute note, and a
// year is the ceiling.
const (
	minIncidentWindow = 5
	maxIncidentWindow = 525600
)

// IncidentAlertRule is the fake's stored rule. Condition and Action hold the
// normalised JSONB the model would persist; Action["priority_map"] holds only
// the severities that differ from IncidentPriorityDefaults.
type IncidentAlertRule struct {
	ID          int64
	ProjectID   int64
	Name        string
	Enabled     bool
	Trigger     string
	Condition   map[string]any
	Action      map[string]any
	LockVersion int64
}

type incidentAlertRuleStore struct {
	byID map[int64]*IncidentAlertRule
}

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["incident_alert_rules"] = &incidentAlertRuleStore{byID: map[int64]*IncidentAlertRule{}}
		mux.HandleFunc("GET /api/v1/projects/{project_id}/incident-rules", s.listIncidentAlertRules)
		mux.HandleFunc("POST /api/v1/projects/{project_id}/incident-rules", s.createIncidentAlertRule)
		mux.HandleFunc("GET /api/v1/projects/{project_id}/incident-rules/{id}", s.showIncidentAlertRule)
		mux.HandleFunc("PATCH /api/v1/projects/{project_id}/incident-rules/{id}", s.updateIncidentAlertRule)
		mux.HandleFunc("DELETE /api/v1/projects/{project_id}/incident-rules/{id}", s.destroyIncidentAlertRule)
	})
}

func (s *Server) incidentAlertRules() *incidentAlertRuleStore {
	store, _ := s.stores["incident_alert_rules"].(*incidentAlertRuleStore)
	return store
}

// TouchIncidentAlertRule simulates an out-of-band edit that bumps lock_version.
func (s *Server) TouchIncidentAlertRule(id int64, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.incidentAlertRules().byID[id]; r != nil {
		r.Name = name
		r.LockVersion++
	}
}

// SetIncidentAlertRuleEnabled flips a rule's enabled flag out of band, to
// stand in for someone toggling it in the console.
func (s *Server) SetIncidentAlertRuleEnabled(id int64, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.incidentAlertRules().byID[id]; r != nil {
		r.Enabled = enabled
		r.LockVersion++
	}
}

// liveIncidentRule resolves @project.incident_alert_rules.find(id): the rule
// must belong to the (live) project in the path.
func (s *Server) liveIncidentRule(projectID, id int64) *IncidentAlertRule {
	r := s.incidentAlertRules().byID[id]
	if r == nil || r.ProjectID != projectID || s.liveProject(projectID) == nil {
		return nil
	}
	return r
}

// effectivePriorities resolves the stored overrides against the defaults.
func effectivePriorities(action map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range IncidentPriorityDefaults {
		out[k] = v
	}
	if stored, ok := action["priority_map"].(map[string]any); ok {
		for k, v := range stored {
			out[k] = asString(v)
		}
	}
	return out
}

func serializeIncidentRule(r *IncidentAlertRule) map[string]any {
	condition := map[string]any{}
	for k, v := range r.Condition {
		condition[k] = v
	}
	action := map[string]any{}
	for k, v := range r.Action {
		action[k] = v
	}
	// `actions` is the ordered list of enabled flags, computed on read.
	actions := []any{}
	for _, k := range incidentActionKeys {
		if truthy(r.Action[k]) {
			actions = append(actions, k)
		}
	}
	return map[string]any{
		"id": r.ID, "project_id": r.ProjectID, "name": r.Name, "enabled": r.Enabled,
		"trigger": r.Trigger, "condition": condition, "action": action, "actions": actions,
		"priority_map": effectivePriorities(r.Action), "lock_version": r.LockVersion,
	}
}

// applyIncidentRuleAttrs mirrors the API's normalisation and validation. Top
// level keys are only touched when submitted; condition and action REPLACE
// what is stored whenever they are sent.
func (s *Server) applyIncidentRuleAttrs(rule *IncidentAlertRule, attrs map[string]any) (int, string, string) {
	for k := range attrs {
		switch k {
		case "name", "trigger", "enabled", "condition", "action", "lock_version":
		default:
			return http.StatusUnprocessableEntity, "invalid_attribute",
				"unknown key: " + k + " (settable: name, trigger, enabled, condition, action)"
		}
	}
	if v, has := attrs["name"]; has && v != nil {
		rule.Name = asString(v)
	}
	// A null enabled is "no opinion", as is omitting it.
	if v, has := attrs["enabled"]; has && v != nil {
		rule.Enabled = truthy(v)
	}
	if v, has := attrs["trigger"]; has {
		t := asString(v)
		if !contains(incidentTriggers, t) {
			return http.StatusUnprocessableEntity, "invalid_attribute",
				"unknown trigger: " + t + " (valid: " + strings.Join(incidentTriggers, ", ") + ")"
		}
		rule.Trigger = t
	}
	if v, has := attrs["condition"]; has && v != nil {
		raw, isMap := v.(map[string]any)
		if !isMap {
			return http.StatusUnprocessableEntity, "invalid_attribute", "condition must be an object"
		}
		for k := range raw {
			if !contains(incidentConditionKeys, k) {
				return http.StatusUnprocessableEntity, "invalid_attribute",
					"unknown condition key: " + k + " (valid: " + strings.Join(incidentConditionKeys, ", ") + ")"
			}
		}
		normalized := map[string]any{}
		for _, k := range incidentConditionKeys {
			if val, present := raw[k]; present && val != nil {
				if k == "count" || k == "window_minutes" {
					n, isNum := asInt64(val)
					if !isNum {
						return http.StatusUnprocessableEntity, "invalid_attribute", k + " must be a whole number"
					}
					normalized[k] = n
					continue
				}
				normalized[k] = val
			}
		}
		rule.Condition = normalized
	}
	if v, has := attrs["action"]; has && v != nil {
		raw, isMap := v.(map[string]any)
		if !isMap {
			return http.StatusUnprocessableEntity, "invalid_attribute", "action must be an object"
		}
		for k := range raw {
			if !contains(incidentActionKeys, k) && k != "webhook_url" && k != "priority_map" {
				return http.StatusUnprocessableEntity, "invalid_attribute",
					"unknown action key: " + k + " (valid: " + strings.Join(incidentActionKeys, ", ") + ", webhook_url, priority_map)"
			}
		}
		normalized := map[string]any{}
		for _, k := range incidentActionKeys {
			if val, present := raw[k]; present && val != nil {
				normalized[k] = truthy(val)
			}
		}
		if url := strings.TrimSpace(asString(raw["webhook_url"])); url != "" {
			normalized["webhook_url"] = url
		}
		if pm, present := raw["priority_map"]; present && pm != nil {
			table, isMap := pm.(map[string]any)
			if !isMap {
				return http.StatusUnprocessableEntity, "invalid_attribute", "priority_map must be an object"
			}
			overrides := map[string]any{}
			for severity, priority := range table {
				if !contains(incidentSeverities, severity) {
					return http.StatusUnprocessableEntity, "invalid_attribute",
						"unknown priority_map severity: " + severity + " (valid: " + strings.Join(incidentSeverities, ", ") + ")"
				}
				p := asString(priority)
				if !contains(incidentPriorities, p) {
					return http.StatusUnprocessableEntity, "invalid_attribute",
						"unknown priority for " + severity + ": " + p + " (valid: " + strings.Join(incidentPriorities, ", ") + ")"
				}
				// Only genuine overrides are stored; a row set to its own
				// default is dropped.
				if IncidentPriorityDefaults[severity] != p {
					overrides[severity] = p
				}
			}
			if len(overrides) > 0 {
				normalized["priority_map"] = overrides
			}
		}
		rule.Action = normalized
	}

	if strings.TrimSpace(rule.Name) == "" {
		return http.StatusUnprocessableEntity, "validation_failed", "Name can't be blank"
	}
	if sev := asString(rule.Condition["min_severity"]); sev != "" && !contains(incidentSeverities, sev) {
		return http.StatusUnprocessableEntity, "invalid_attribute",
			"unknown min_severity: " + sev + " (valid: " + strings.Join(incidentSeverities, ", ") + ")"
	}
	if w, has := rule.Condition["window_minutes"]; has {
		n, _ := asInt64(w)
		if n < minIncidentWindow {
			return http.StatusUnprocessableEntity, "validation_failed",
				"Condition window must be at least 5 minutes (repeat events are folded per 5-minute note)"
		}
		if n > maxIncidentWindow {
			return http.StatusUnprocessableEntity, "validation_failed", "Condition window must be at most 525600 minutes (one year)"
		}
	}
	if rule.Trigger == "incident_repeated" {
		n, has := asInt64(rule.Condition["count"])
		if !has || n < 1 {
			return http.StatusUnprocessableEntity, "validation_failed", "Condition needs a count of at least 1 for the repeated trigger"
		}
	}
	var anyAction bool
	for _, k := range incidentActionKeys {
		if truthy(rule.Action[k]) {
			anyAction = true
		}
	}
	if !anyAction {
		return http.StatusUnprocessableEntity, "validation_failed", "Action must enable at least one action"
	}
	if truthy(rule.Action["notify_webhook"]) && !httpURL.MatchString(asString(rule.Action["webhook_url"])) {
		return http.StatusUnprocessableEntity, "validation_failed", "Action needs a valid http(s) webhook URL"
	}
	return 0, "", ""
}

func (s *Server) listIncidentAlertRules(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	s.mu.Lock()
	if s.liveProject(pid) == nil {
		s.mu.Unlock()
		notFound(w)
		return
	}
	var rows []*IncidentAlertRule
	for _, rule := range s.incidentAlertRules().byID {
		if rule.ProjectID == pid {
			rows = append(rows, rule)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	items := make([]any, 0, len(rows))
	for _, rule := range rows {
		items = append(items, serializeIncidentRule(rule))
	}
	s.mu.Unlock()
	writeCollection(w, r, items)
}

func (s *Server) showIncidentAlertRule(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	rule := s.liveIncidentRule(pid, id)
	var body map[string]any
	if rule != nil {
		body = serializeIncidentRule(rule)
	}
	s.mu.Unlock()
	if body == nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) createIncidentAlertRule(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "incident_alert_rule")
	if !ok {
		return
	}
	s.withIdempotency(w, r, "incident_alert_rule", func() (int, any) {
		s.mu.Lock()
		defer s.mu.Unlock()
		project := s.liveProject(pid)
		if project == nil {
			return http.StatusNotFound, errorBody("Not found", "not_found")
		}
		// The incidents feature gates CREATES only; an existing rule keeps
		// working after the feature is switched off.
		enabled, stored := project.Features["incidents"]
		if !stored {
			enabled = DefaultFeatures["incidents"]
		}
		if !enabled {
			return http.StatusUnprocessableEntity, errorBody(
				"Incident management must be enabled for this project before an incident alert rule can be created (Project Settings → Features)",
				"validation_failed")
		}
		rule := &IncidentAlertRule{
			ID: s.id(), ProjectID: pid, Enabled: true, Trigger: "incident_opened",
			Condition: map[string]any{}, Action: map[string]any{},
		}
		if status, code, msg := s.applyIncidentRuleAttrs(rule, attrs); status != 0 {
			return status, errorBody(msg, code)
		}
		s.incidentAlertRules().byID[rule.ID] = rule
		return http.StatusCreated, serializeIncidentRule(rule)
	})
}

func (s *Server) updateIncidentAlertRule(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "incident_alert_rule")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rule := s.liveIncidentRule(pid, id)
	if rule == nil {
		notFound(w)
		return
	}
	if !checkIfMatch(w, r, rule.LockVersion) {
		return
	}
	candidate := *rule
	if status, code, msg := s.applyIncidentRuleAttrs(&candidate, attrs); status != 0 {
		writeError(w, status, code, msg)
		return
	}
	candidate.LockVersion++
	*rule = candidate
	writeJSON(w, http.StatusOK, serializeIncidentRule(rule))
}

func (s *Server) destroyIncidentAlertRule(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rule := s.liveIncidentRule(pid, id)
	if rule == nil {
		notFound(w)
		return
	}
	if !checkIfMatch(w, r, rule.LockVersion) {
		return
	}
	delete(s.incidentAlertRules().byID, id)
	w.WriteHeader(http.StatusNoContent)
}
