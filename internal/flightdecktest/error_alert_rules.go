package flightdecktest

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
)

var (
	alertTriggers      = []string{"new_group", "regression", "occurrence_threshold"}
	alertConditionKeys = []string{"min_level", "environment", "count", "window_minutes"}
	alertActionKeys    = []string{"notify_slack", "notify_email", "create_work_item", "file_intake", "notify_webhook", "open_incident"}
	errorLevels        = []string{"debug", "info", "warning", "error", "critical"}
	httpURL            = regexp.MustCompile(`(?i)^https?://\S+$`)
)

// ErrorAlertRule is the fake's stored rule. Condition and Action hold the
// normalised JSONB the model would persist.
type ErrorAlertRule struct {
	ID          int64
	ProjectID   int64
	Name        string
	Enabled     bool
	Trigger     string
	Condition   map[string]any
	Action      map[string]any
	LockVersion int64
}

type errorAlertRuleStore struct {
	byID map[int64]*ErrorAlertRule
	// escalationPolicies per project, for open_incident's escalation_policy_id.
	escalationPolicies map[int64][]int64
}

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["error_alert_rules"] = &errorAlertRuleStore{byID: map[int64]*ErrorAlertRule{}, escalationPolicies: map[int64][]int64{}}
		mux.HandleFunc("GET /api/v1/projects/{project_id}/error_alert_rules", s.listErrorAlertRules)
		mux.HandleFunc("POST /api/v1/projects/{project_id}/error_alert_rules", s.createErrorAlertRule)
		mux.HandleFunc("GET /api/v1/error_alert_rules/{id}", s.showErrorAlertRule)
		mux.HandleFunc("PATCH /api/v1/error_alert_rules/{id}", s.updateErrorAlertRule)
		mux.HandleFunc("DELETE /api/v1/error_alert_rules/{id}", s.destroyErrorAlertRule)
	})
}

func (s *Server) errorAlertRules() *errorAlertRuleStore {
	store, _ := s.stores["error_alert_rules"].(*errorAlertRuleStore)
	return store
}

// AddEscalationPolicy registers an escalation policy id on a project so an
// open_incident action may reference it.
func (s *Server) AddEscalationPolicy(projectID, policyID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.errorAlertRules()
	st.escalationPolicies[projectID] = append(st.escalationPolicies[projectID], policyID)
}

// TouchErrorAlertRule simulates an out-of-band edit that bumps lock_version.
func (s *Server) TouchErrorAlertRule(id int64, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.errorAlertRules().byID[id]; r != nil {
		r.Name = name
		r.LockVersion++
	}
}

func (s *Server) liveRule(id int64) *ErrorAlertRule {
	r := s.errorAlertRules().byID[id]
	if r == nil || s.liveProject(r.ProjectID) == nil {
		return nil
	}
	return r
}

func serializeRule(r *ErrorAlertRule) map[string]any {
	condition := map[string]any{}
	for k, v := range r.Condition {
		condition[k] = v
	}
	action := map[string]any{}
	for k, v := range r.Action {
		action[k] = v
	}
	return map[string]any{
		"id": r.ID, "project_id": r.ProjectID, "name": r.Name, "enabled": r.Enabled,
		"trigger": r.Trigger, "condition": condition, "action": action, "lock_version": r.LockVersion,
	}
}

func (s *Server) listErrorAlertRules(w http.ResponseWriter, r *http.Request) {
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
	var rows []*ErrorAlertRule
	for _, rule := range s.errorAlertRules().byID {
		if rule.ProjectID == pid {
			rows = append(rows, rule)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	items := make([]any, 0, len(rows))
	for _, rule := range rows {
		items = append(items, serializeRule(rule))
	}
	s.mu.Unlock()
	writeCollection(w, r, items)
}

func (s *Server) showErrorAlertRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	rule := s.liveRule(id)
	var body map[string]any
	if rule != nil {
		body = serializeRule(rule)
	}
	s.mu.Unlock()
	if body == nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// applyRuleAttrs mirrors ErrorAlertRule#normalize_jsonb + #validate_values.
// wasOpeningIncident is the pre-write open_incident flag (FD-665 gate).
func (s *Server) applyRuleAttrs(rule *ErrorAlertRule, attrs map[string]any, project *Project) (int, string, string) {
	wasOpening := truthy(rule.Action["open_incident"])
	if v, has := attrs["name"]; has {
		rule.Name = asString(v)
	}
	if v, has := attrs["enabled"]; has {
		rule.Enabled = truthy(v)
	}
	if v, has := attrs["trigger"]; has {
		t := asString(v)
		if !contains(alertTriggers, t) {
			return http.StatusUnprocessableEntity, "invalid_attribute", "'" + t + "' is not a valid trigger"
		}
		rule.Trigger = t
	}
	if v, has := attrs["condition"]; has {
		raw, isMap := v.(map[string]any)
		if !isMap {
			return http.StatusUnprocessableEntity, "invalid_attribute", "condition must be an object"
		}
		normalized := map[string]any{}
		for _, k := range alertConditionKeys {
			if val, present := raw[k]; present && val != nil {
				normalized[k] = val
			}
		}
		rule.Condition = normalized
	}
	if v, has := attrs["action"]; has {
		raw, isMap := v.(map[string]any)
		if !isMap {
			return http.StatusUnprocessableEntity, "invalid_attribute", "action must be an object"
		}
		normalized := map[string]any{}
		for _, k := range alertActionKeys {
			if val, present := raw[k]; present {
				normalized[k] = truthy(val)
			}
		}
		if url := strings.TrimSpace(asString(raw["webhook_url"])); url != "" {
			normalized["webhook_url"] = url
		}
		if pid, isNum := asInt64(raw["escalation_policy_id"]); isNum && truthy(raw["open_incident"]) {
			normalized["escalation_policy_id"] = pid
		}
		rule.Action = normalized
	}

	if strings.TrimSpace(rule.Name) == "" {
		return http.StatusUnprocessableEntity, "validation_failed", "Name can't be blank"
	}
	if level := asString(rule.Condition["min_level"]); level != "" && !contains(errorLevels, level) {
		return http.StatusUnprocessableEntity, "validation_failed", "Condition has an invalid level"
	}
	var anyAction bool
	for _, k := range alertActionKeys {
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
	if pid, has := rule.Action["escalation_policy_id"]; has {
		id, _ := asInt64(pid)
		found := false
		for _, known := range s.errorAlertRules().escalationPolicies[rule.ProjectID] {
			if known == id {
				found = true
			}
		}
		if !found {
			return http.StatusUnprocessableEntity, "validation_failed", "Action names an escalation policy that isn't in this project"
		}
	}
	if truthy(rule.Action["open_incident"]) && !wasOpening {
		enabled, stored := project.Features["incidents"]
		if !stored {
			enabled = DefaultFeatures["incidents"]
		}
		if !enabled {
			return http.StatusUnprocessableEntity, "validation_failed",
				"Action can't open incidents until Incident management is enabled for this project"
		}
	}
	return 0, "", ""
}

func (s *Server) createErrorAlertRule(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "error_alert_rule")
	if !ok {
		return
	}
	s.withIdempotency(w, r, "error_alert_rule", func() (int, any) {
		s.mu.Lock()
		defer s.mu.Unlock()
		project := s.liveProject(pid)
		if project == nil {
			return http.StatusNotFound, map[string]any{"error": "Not found", "code": "not_found"}
		}
		rule := &ErrorAlertRule{ID: s.id(), ProjectID: pid, Enabled: true, Trigger: "new_group", Condition: map[string]any{}, Action: map[string]any{}}
		if status, code, msg := s.applyRuleAttrs(rule, attrs, project); status != 0 {
			return status, map[string]any{"error": msg, "code": code}
		}
		s.errorAlertRules().byID[rule.ID] = rule
		return http.StatusCreated, serializeRule(rule)
	})
}

func (s *Server) updateErrorAlertRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	attrs, ok := decodeBody(w, r, "error_alert_rule")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rule := s.liveRule(id)
	if rule == nil {
		notFound(w)
		return
	}
	if !checkIfMatch(w, r, rule.LockVersion) {
		return
	}
	candidate := *rule
	if status, code, msg := s.applyRuleAttrs(&candidate, attrs, s.liveProject(rule.ProjectID)); status != 0 {
		writeError(w, status, code, msg)
		return
	}
	candidate.LockVersion++
	*rule = candidate
	writeJSON(w, http.StatusOK, serializeRule(rule))
}

func (s *Server) destroyErrorAlertRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.liveRule(id) == nil {
		notFound(w)
		return
	}
	delete(s.errorAlertRules().byID, id)
	w.WriteHeader(http.StatusNoContent)
}
