package flightdecktest

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["agent_work"] = &agentWorkStore{enabled: true, byProject: map[int64]*AgentWorkSetting{}, extraBlockers: map[int64][]AgentWorkBlocker{}}
		mux.HandleFunc("GET /api/v1/projects/{project_id}/agent-work", s.showAgentWork)
		mux.HandleFunc("PATCH /api/v1/projects/{project_id}/agent-work", s.updateAgentWork)
	})
}

// AgentWorkSetting is the fake's stored agent work settings row. A project
// without one reads the defaults at lock_version 0, and reading never
// creates one, as in the API.
type AgentWorkSetting struct {
	Enabled             bool
	Kinds               []string
	LabelID             *int64
	LabelChosenAt       *time.Time
	AcceptMachineLabels bool
	AgentAccountID      *int64
	BaseRef             string
	MaxInProgress       int64
	DailyBudgetUSD      float64
	TaskMaxUSD          float64
	TaskMaxMinutes      int64
	QueueMinutes        int64
	Runbook             string
	LockVersion         int64
	UpdatedAt           *time.Time
}

// AgentWorkBlocker is one entry of the read's blockers.
type AgentWorkBlocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type agentWorkStore struct {
	// enabled=false simulates a Flightdeck without the endpoint: both routes 404.
	enabled   bool
	byProject map[int64]*AgentWorkSetting
	// poolConnected clears the pool_not_connected blocker.
	poolConnected bool
	// extraBlockers are appended to a project's computed blockers, for the
	// ones the fake does not model (paused, app_not_billable, budget_spent).
	extraBlockers map[int64][]AgentWorkBlocker
}

func (s *Server) agentWorkStore() *agentWorkStore {
	st, _ := s.stores["agent_work"].(*agentWorkStore)
	return st
}

// SetAgentWorkEndpoint enables or disables the agent-work routes, to simulate
// a Flightdeck version that does not expose them.
func (s *Server) SetAgentWorkEndpoint(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agentWorkStore().enabled = on
}

// SetAgentWorkPoolConnected controls the pool_not_connected blocker. The fake
// starts disconnected, as a Flightdeck that has not been connected to the
// agent pool does.
func (s *Server) SetAgentWorkPoolConnected(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agentWorkStore().poolConnected = on
}

// AddAgentWorkBlockers appends blockers to what a project reports.
func (s *Server) AddAgentWorkBlockers(projectID int64, blockers ...AgentWorkBlocker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.agentWorkStore()
	st.extraBlockers[projectID] = append(st.extraBlockers[projectID], blockers...)
}

// AgentWorkOf returns a copy of a project's stored settings row, or nil when
// none was ever saved.
func (s *Server) AgentWorkOf(projectID int64) *AgentWorkSetting {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.agentWorkStore().byProject[projectID]
	if row == nil {
		return nil
	}
	cp := *row
	cp.Kinds = append([]string(nil), row.Kinds...)
	return &cp
}

// SetAgentWorkOutOfBand changes a project's settings the way the settings page
// does (no HTTP): fn edits the row, which is created if missing, and the
// row's lock_version moves on.
func (s *Server) SetAgentWorkOutOfBand(projectID int64, fn func(*AgentWorkSetting)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.agentWorkStore()
	row := st.byProject[projectID]
	if row == nil {
		d := defaultAgentWork()
		row = &d
		st.byProject[projectID] = row
	}
	before := row.LabelID
	fn(row)
	if !sameID(before, row.LabelID) {
		stampLabelChosenAt(row)
	}
	row.LockVersion++
	now := time.Now()
	row.UpdatedAt = &now
}

func defaultAgentWork() AgentWorkSetting {
	return AgentWorkSetting{
		Kinds: []string{}, BaseRef: "main", MaxInProgress: 1, DailyBudgetUSD: 10, TaskMaxUSD: 5,
		TaskMaxMinutes: 30, QueueMinutes: 60, Runbook: "implement-work-item@1",
	}
}

var (
	agentWorkKinds    = []string{"implement-work-item"}
	agentWorkRunbooks = []string{"implement-work-item@1"}
	agentWorkWritable = []string{
		"enabled", "kinds", "label_id", "accept_machine_labels", "agent_account_id", "base_ref",
		"max_in_progress", "daily_budget_usd", "task_max_usd", "task_max_minutes", "queue_minutes", "runbook",
	}
	agentWorkReadOnly = map[string]string{
		"project_id":      "it is the :project_id in the path, and a project's agent work settings belong to that project",
		"blockers":        "Flightdeck works them out from the settings and the project each time they are read",
		"label_chosen_at": "Flightdeck sets it when label_id changes: to now when a label is chosen, and to null when it is cleared",
		"updated_at":      "the database sets it",
	}
)

// agentWorkNoOpinion is what a blank means for every key but the two nullable
// references: leave the key alone.
type agentWorkNoOpinion struct{}

func agentWorkBlank(v any) bool {
	if v == nil {
		return true
	}
	str, ok := v.(string)
	return ok && strings.TrimSpace(str) == ""
}

// agentWorkWhole reads a JSON number as a whole number; 3.0 is 3, 3.5 is not.
func agentWorkWhole(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	if i, err := n.Int64(); err == nil {
		return i, true
	}
	f, err := n.Float64()
	if err != nil || math.IsInf(f, 0) || f != math.Trunc(f) {
		return 0, false
	}
	return int64(f), true
}

// agentWorkDecimals counts the decimal places of a JSON number as written.
func agentWorkDecimals(n json.Number) int {
	s := strings.ToLower(n.String())
	if strings.ContainsAny(s, "e") {
		// Re-render through the shortest float form, as the API's Float#to_s does.
		f, _ := n.Float64()
		s = strconv.FormatFloat(f, 'f', -1, 64)
	}
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return 0
	}
	return len(strings.TrimRight(s[dot+1:], "0"))
}

// agentWorkValue checks one submitted value's JSON type the way the API's
// write core does, returning the value to assign, agentWorkNoOpinion{} to
// leave the key alone, or a 422 invalid_attribute message.
func agentWorkValue(key string, v any) (any, string) {
	if agentWorkBlank(v) {
		if key == "label_id" || key == "agent_account_id" {
			return (*int64)(nil), ""
		}
		return agentWorkNoOpinion{}, ""
	}
	switch key {
	case "enabled", "accept_machine_labels":
		if b, ok := v.(bool); ok {
			return b, ""
		}
		return nil, fmt.Sprintf("%s must be true or false, got %v", key, v)
	case "max_in_progress", "task_max_minutes", "queue_minutes":
		if i, ok := agentWorkWhole(v); ok {
			return i, ""
		}
		return nil, fmt.Sprintf("%s must be a whole number, got %v", key, v)
	case "label_id", "agent_account_id":
		if i, ok := agentWorkWhole(v); ok && i >= 1 {
			return &i, ""
		}
		return nil, fmt.Sprintf("%s must be an id (a positive whole number) or null, got %v", key, v)
	case "daily_budget_usd", "task_max_usd":
		n, ok := v.(json.Number)
		if !ok {
			return nil, fmt.Sprintf("%s must be a number, got %v", key, v)
		}
		return n, ""
	case "kinds":
		list, ok := v.([]any)
		if !ok {
			return nil, fmt.Sprintf("kinds must be a list of kind names, got %v", v)
		}
		kinds := make([]string, 0, len(list))
		var unknown []string
		for _, e := range list {
			str, isStr := e.(string)
			if !isStr {
				return nil, fmt.Sprintf("kinds must be a list of kind names, got %v", v)
			}
			if !contains(agentWorkKinds, str) {
				unknown = append(unknown, fmt.Sprintf("%q", str))
			}
			kinds = append(kinds, str)
		}
		if len(unknown) > 0 {
			return nil, fmt.Sprintf("kinds has an unknown kind: %s (valid: %s)", strings.Join(unknown, ", "), strings.Join(agentWorkKinds, ", "))
		}
		return kinds, ""
	case "runbook":
		str, ok := v.(string)
		if !ok {
			return nil, fmt.Sprintf("runbook must be a string, got %v", v)
		}
		if !contains(agentWorkRunbooks, str) {
			return nil, fmt.Sprintf("runbook is not a runbook the pool knows, got %q (valid: %s)", str, strings.Join(agentWorkRunbooks, ", "))
		}
		return str, ""
	case "base_ref":
		if str, ok := v.(string); ok {
			return str, ""
		}
		return nil, fmt.Sprintf("base_ref must be a string, got %v", v)
	}
	return nil, "unknown key: " + key
}

// applyAgentWork mirrors the API's write: read-only keys refused by name,
// unknown keys refused, every value type-checked before anything is assigned,
// then the model's value rules against the merged row. It returns the merged
// candidate and whether any setting changed, or a status and message.
func (s *Server) applyAgentWork(p *Project, current AgentWorkSetting, submitted map[string]any) (AgentWorkSetting, bool, int, string, string) {
	delete(submitted, "lock_version")
	var readOnly []string
	for key := range submitted {
		if why, ro := agentWorkReadOnly[key]; ro {
			readOnly = append(readOnly, key+" is read-only over the API — "+why)
		}
	}
	if len(readOnly) > 0 {
		sort.Strings(readOnly)
		return current, false, http.StatusUnprocessableEntity, "invalid_attribute", strings.Join(readOnly, "; ")
	}
	for key := range submitted {
		if !contains(agentWorkWritable, key) {
			return current, false, http.StatusUnprocessableEntity, "invalid_attribute",
				"unknown key: " + key + " (settable: " + strings.Join(agentWorkWritable, ", ") + ")"
		}
	}
	values := map[string]any{}
	for key, raw := range submitted {
		v, msg := agentWorkValue(key, raw)
		if msg != "" {
			return current, false, http.StatusUnprocessableEntity, "invalid_attribute", msg
		}
		if _, skip := v.(agentWorkNoOpinion); !skip {
			values[key] = v
		}
	}

	next := current
	next.Kinds = append([]string(nil), current.Kinds...)
	var moneyRefused []string
	// agentWorkValue has already checked each value's type for its key.
	for key, v := range values {
		switch t := v.(type) {
		case bool:
			if key == "enabled" {
				next.Enabled = t
			} else {
				next.AcceptMachineLabels = t
			}
		case []string:
			next.Kinds = t
		case *int64:
			if key == "label_id" {
				next.LabelID = t
			} else {
				next.AgentAccountID = t
			}
		case string:
			if key == "base_ref" {
				next.BaseRef = t
			} else {
				next.Runbook = t
			}
		case int64:
			switch key {
			case "max_in_progress":
				next.MaxInProgress = t
			case "task_max_minutes":
				next.TaskMaxMinutes = t
			default:
				next.QueueMinutes = t
			}
		case json.Number:
			f, _ := t.Float64()
			if agentWorkDecimals(t) > 2 {
				moneyRefused = append(moneyRefused, key)
			}
			if key == "daily_budget_usd" {
				next.DailyBudgetUSD = f
			} else {
				next.TaskMaxUSD = f
			}
		}
	}

	// The model's value rules, against the merged row. The messages use the
	// settings page's words, as the API's do.
	var problems []string
	if next.MaxInProgress < 1 || next.MaxInProgress > 10 {
		problems = append(problems, "Items at once must be between 1 and 10")
	}
	if next.DailyBudgetUSD <= 0 || next.DailyBudgetUSD > 1000 {
		problems = append(problems, "Daily budget must be greater than 0 and at most 1000")
	}
	if next.TaskMaxUSD <= 0 || next.TaskMaxUSD > 100 {
		problems = append(problems, "Most a task may cost must be greater than 0 and at most 100")
	}
	if next.TaskMaxMinutes < 1 || next.TaskMaxMinutes > 240 {
		problems = append(problems, "Most minutes a task may run must be between 1 and 240")
	}
	if next.QueueMinutes < 1 || next.QueueMinutes > 1440 {
		problems = append(problems, "Minutes a task may wait to start must be between 1 and 1440")
	}
	for _, key := range moneyRefused {
		if key == "daily_budget_usd" {
			problems = append(problems, "Daily budget can have at most 2 decimal places")
		} else {
			problems = append(problems, "Most a task may cost can have at most 2 decimal places")
		}
	}
	if next.TaskMaxUSD > next.DailyBudgetUSD {
		problems = append(problems, "Most a task may cost can't be more than the daily budget")
	}
	if !agentWorkGitRef(next.BaseRef) {
		problems = append(problems, "Base branch must be a branch name Git allows")
	}
	seen := map[string]bool{}
	for _, k := range next.Kinds {
		if seen[k] {
			problems = append(problems, "Kinds of work can't list a kind twice")
			break
		}
		seen[k] = true
	}
	if !sameID(current.LabelID, next.LabelID) && next.LabelID != nil {
		if l := s.labels().byID[*next.LabelID]; l == nil || l.ProjectID != p.ID {
			problems = append(problems, "Agent label must be a label in this project")
		}
	}
	if !sameID(current.AgentAccountID, next.AgentAccountID) && next.AgentAccountID != nil {
		if m := s.memberByID(*next.AgentAccountID); m == nil || m.Kind != KindService {
			problems = append(problems, "Agents work as must be a service account")
		}
	}
	if len(problems) > 0 {
		return current, false, http.StatusUnprocessableEntity, "validation_failed", strings.Join(problems, ", ")
	}
	return next, !agentWorkEqual(current, next), 0, "", ""
}

// agentWorkGitRef is a simplified git check-ref-format --branch.
func agentWorkGitRef(ref string) bool {
	if ref == "" || len(ref) > 255 || ref == "@" || ref == "HEAD" {
		return false
	}
	if strings.ContainsAny(ref, " \t\n~^:?*[\\") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.Contains(ref, "//") {
		return false
	}
	return !strings.HasPrefix(ref, "-") && !strings.HasPrefix(ref, "/") && !strings.HasSuffix(ref, "/") && !strings.HasSuffix(ref, ".")
}

func (s *Server) memberByID(id int64) *User {
	for i := range s.members {
		if s.members[i].ID == id {
			return &s.members[i]
		}
	}
	return nil
}

func sameID(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func agentWorkEqual(a, b AgentWorkSetting) bool {
	if len(a.Kinds) != len(b.Kinds) {
		return false
	}
	for i := range a.Kinds {
		if a.Kinds[i] != b.Kinds[i] {
			return false
		}
	}
	return a.Enabled == b.Enabled && sameID(a.LabelID, b.LabelID) && a.AcceptMachineLabels == b.AcceptMachineLabels &&
		sameID(a.AgentAccountID, b.AgentAccountID) && a.BaseRef == b.BaseRef && a.MaxInProgress == b.MaxInProgress &&
		a.DailyBudgetUSD == b.DailyBudgetUSD && a.TaskMaxUSD == b.TaskMaxUSD && a.TaskMaxMinutes == b.TaskMaxMinutes &&
		a.QueueMinutes == b.QueueMinutes && a.Runbook == b.Runbook
}

func stampLabelChosenAt(row *AgentWorkSetting) {
	if row.LabelID == nil {
		row.LabelChosenAt = nil
		return
	}
	now := time.Now()
	row.LabelChosenAt = &now
}

// agentWorkBlockers is the fake's blockers, in the API's order, for the ones
// it models; anything added with AddAgentWorkBlockers follows them.
func (s *Server) agentWorkBlockers(p *Project, row AgentWorkSetting) []AgentWorkBlocker {
	out := []AgentWorkBlocker{}
	add := func(code, message string) { out = append(out, AgentWorkBlocker{Code: code, Message: message}) }
	if !row.Enabled {
		add("disabled", "Agent work is off for this project.")
	}
	if p.Archived {
		add("archived", "The project is archived.")
	}
	if !s.agentWorkStore().poolConnected {
		add("pool_not_connected", "Flightdeck isn't connected to the agent pool yet.")
	}
	if len(row.Kinds) == 0 {
		add("no_kinds", "No kinds of work are ticked.")
	}
	if !s.githubRepoLinked(p) {
		add("no_github_repo", "The project has no linked GitHub repository.")
	}
	if row.LabelID == nil {
		add("no_label", "No agent label is chosen.")
	} else if l := s.labels().byID[*row.LabelID]; l == nil || l.ProjectID != p.ID {
		add("no_label", "No agent label is chosen.")
	}
	if row.AgentAccountID == nil {
		add("no_agent_account", "No service account is chosen for agents to work as.")
	} else if m := s.memberByID(*row.AgentAccountID); m == nil || m.Kind != KindService {
		add("no_agent_account", "That account isn't a service account in this workspace.")
	}
	return append(out, s.agentWorkStore().extraBlockers[p.ID]...)
}

func (s *Server) githubRepoLinked(p *Project) bool {
	if p.GithubRepoFullName != nil && *p.GithubRepoFullName != "" {
		return true
	}
	if store, ok := s.stores["github_integrations"].(*githubIntegrationStore); ok {
		for _, gi := range store.byID {
			if gi.ProjectID == p.ID && gi.Enabled {
				return true
			}
		}
	}
	return false
}

func (s *Server) serializeAgentWork(p *Project, row AgentWorkSetting) map[string]any {
	var labelChosenAt, updatedAt any
	if row.LabelID != nil && row.LabelChosenAt != nil {
		labelChosenAt = iso(*row.LabelChosenAt)
	}
	if row.UpdatedAt != nil {
		updatedAt = iso(*row.UpdatedAt)
	}
	kinds := append([]string{}, row.Kinds...)
	return map[string]any{
		"project_id":            p.ID,
		"enabled":               row.Enabled,
		"kinds":                 kinds,
		"label_id":              row.LabelID,
		"label_chosen_at":       labelChosenAt,
		"accept_machine_labels": row.AcceptMachineLabels,
		"agent_account_id":      row.AgentAccountID,
		"base_ref":              row.BaseRef,
		"max_in_progress":       row.MaxInProgress,
		"daily_budget_usd":      row.DailyBudgetUSD,
		"task_max_usd":          row.TaskMaxUSD,
		"task_max_minutes":      row.TaskMaxMinutes,
		"queue_minutes":         row.QueueMinutes,
		"runbook":               row.Runbook,
		"blockers":              s.agentWorkBlockers(p, row),
		"lock_version":          row.LockVersion,
		"updated_at":            updatedAt,
	}
}

// agentWorkRow returns the stored row or the unsaved defaults.
func (s *Server) agentWorkRow(projectID int64) (AgentWorkSetting, bool) {
	if row := s.agentWorkStore().byProject[projectID]; row != nil {
		return *row, true
	}
	return defaultAgentWork(), false
}

// The existence check runs before the admin bar, as in the API, so a
// non-admin cannot tell project ids apart by 403 vs 404.
func (s *Server) showAgentWork(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.liveProject(pid)
	if p == nil || !s.agentWorkStore().enabled {
		notFound(w)
		return
	}
	if !s.requireWorkspaceAdmin(w) {
		return
	}
	row, _ := s.agentWorkRow(pid)
	writeJSON(w, http.StatusOK, s.serializeAgentWork(p, row))
}

func (s *Server) updateAgentWork(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	s.mu.Lock()
	p := s.liveProject(pid)
	if p == nil || !s.agentWorkStore().enabled {
		s.mu.Unlock()
		notFound(w)
		return
	}
	if !s.requireWorkspaceAdmin(w) {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	attrs, ok := decodeBody(w, r, "agent_work")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, persisted := s.agentWorkRow(pid)
	// The precondition pins the SETTINGS row's lock_version; a project with no
	// row is at 0, and naming any other version there is stale.
	if !checkIfMatch(w, r, current.LockVersion) {
		return
	}
	next, changed, status, code, msg := s.applyAgentWork(p, current, attrs)
	if status != 0 {
		writeError(w, status, code, msg)
		return
	}
	if !changed {
		// Nothing changed: no row is created, no version moves.
		writeJSON(w, http.StatusOK, s.serializeAgentWork(p, current))
		return
	}
	if !sameID(current.LabelID, next.LabelID) {
		stampLabelChosenAt(&next)
	}
	if persisted {
		next.LockVersion = current.LockVersion + 1
	} else {
		next.LockVersion = 1
	}
	now := time.Now()
	next.UpdatedAt = &now
	stored := next
	s.agentWorkStore().byProject[pid] = &stored
	writeJSON(w, http.StatusOK, s.serializeAgentWork(p, next))
}
