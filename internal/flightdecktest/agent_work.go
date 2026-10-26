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
		s.stores["agent_work"] = &agentWorkStore{
			enabled: true, release: AgentWorkWithModels, models: append([]string(nil), agentWorkModels...),
			byProject: map[int64]*AgentWorkSetting{}, extraBlockers: map[int64][]AgentWorkBlocker{},
		}
		mux.HandleFunc("GET /api/v1/projects/{project_id}/agent-work", s.showAgentWork)
		mux.HandleFunc("PATCH /api/v1/projects/{project_id}/agent-work", s.updateAgentWork)
	})
}

// AgentWorkSetting is the fake's stored agent work settings row. A project
// without one reads the defaults at lock_version 0, and reading never
// creates one, as in the API.
type AgentWorkSetting struct {
	Enabled bool
	Kinds   []string
	// Models maps a kind of work to the AI model its tasks ask for. A kind not
	// in it uses AutoPilot's default. Empty by default, never nil on a row the
	// fake made, and never a blocker.
	Models map[string]string
	// AutoMerge asks AutoPilot to turn on GitHub auto-merge for each pull
	// request an agent opens for the project. Off by default.
	AutoMerge bool
	// SendCIFailures sends each item Flightdeck files for a failed CI run to
	// an agent without a person adding the agent label. Off by default, and
	// never a blocker.
	SendCIFailures bool
	// SendErrors sends each item an error alert rule files to an agent, as
	// fix-error, without a person adding the agent label. Off by default, and
	// never a blocker. SendErrorsSince is when it last went from off to on,
	// stamped whenever it changes (nil while it is off), and read-only over
	// the API.
	SendErrors      bool
	SendErrorsSince *time.Time
	// WaitForChecks has an agent wait for its pull request's CI checks, and fix
	// a failure itself, before it finishes. ON by default, unlike the other
	// switches, and never a blocker.
	WaitForChecks       bool
	LabelID             *int64
	LabelChosenAt       *time.Time
	AcceptMachineLabels bool
	// ResearchLabelID sends an item for research first; it must be a label in
	// the project and a different one from LabelID. ResearchLabelChosenAt is
	// stamped whenever it changes, as LabelChosenAt is for LabelID.
	ResearchLabelID       *int64
	ResearchLabelChosenAt *time.Time
	AgentAccountID        *int64
	BaseRef               string
	MaxInProgress         int64
	DailyBudgetUSD        float64
	TaskMaxUSD            float64
	TaskMaxMinutes        int64
	QueueMinutes          int64
	Runbook               string
	LockVersion           int64
	UpdatedAt             *time.Time
}

// AgentWorkBlocker is one entry of the read's blockers.
type AgentWorkBlocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type agentWorkStore struct {
	// enabled=false simulates a Flightdeck without the endpoint: both routes 404.
	enabled bool
	// release is the Flightdeck release the endpoint acts as. A setting newer
	// than it is unknown: reads leave the key out, and a write naming it is
	// refused as an unknown key, whatever its value.
	release AgentWorkRelease
	// models are the AI models the release offers, in the API's order: the
	// names a write may set a kind to, and the ones a refusal lists.
	models    []string
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

// AgentWorkRelease is a Flightdeck release the agent-work endpoint can act
// as, oldest first. Flightdeck added auto_merge, then send_ci_failures, then
// wait_for_checks, then send_errors, then models, and a release knows every
// setting added up to it, so a Flightdeck older than auto_merge is older than
// the other four as well. No release knows a setting without the ones added
// before it, and the fake cannot be made to.
type AgentWorkRelease int

const (
	// AgentWorkBeforeAutoMerge knows none of auto_merge, send_ci_failures,
	// wait_for_checks, send_errors and models.
	AgentWorkBeforeAutoMerge AgentWorkRelease = iota
	// AgentWorkWithAutoMerge knows auto_merge only.
	AgentWorkWithAutoMerge
	// AgentWorkWithSendCIFailures knows auto_merge and send_ci_failures, but
	// not wait_for_checks or send_errors.
	AgentWorkWithSendCIFailures
	// AgentWorkWithWaitForChecks knows auto_merge, send_ci_failures and
	// wait_for_checks, but not send_errors.
	AgentWorkWithWaitForChecks
	// AgentWorkWithSendErrors knows auto_merge, send_ci_failures,
	// wait_for_checks and send_errors, but not models.
	AgentWorkWithSendErrors
	// AgentWorkWithModels knows all five. The fake starts here.
	AgentWorkWithModels
)

// SetAgentWorkRelease makes the agent-work endpoint act as the given
// release.
func (s *Server) SetAgentWorkRelease(release AgentWorkRelease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agentWorkStore().release = release
}

// SetAgentWorkAutoMergeSupported moves the fake across the release that
// added auto_merge. false makes it older than that release, so it knows none
// of the newer settings, as a real Flightdeck older than auto_merge does. true
// makes it at least that release: one older is upgraded to it (auto_merge,
// still no send_ci_failures, wait_for_checks, send_errors or models), and a newer one
// is left as it is.
func (s *Server) SetAgentWorkAutoMergeSupported(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.agentWorkStore()
	switch {
	case !on:
		st.release = AgentWorkBeforeAutoMerge
	case st.release < AgentWorkWithAutoMerge:
		st.release = AgentWorkWithAutoMerge
	}
}

// SetAgentWorkSendCIFailuresSupported moves the fake across the release that
// added send_ci_failures. false makes it older than that release: a newer one
// becomes the release with auto_merge but not send_ci_failures (nor
// wait_for_checks, send_errors or models), and one older than auto_merge stays so.
// true makes it at least that release: one older is upgraded to it
// (auto_merge and send_ci_failures, still no wait_for_checks, send_errors or
// models),
// and a newer one is left as it is.
func (s *Server) SetAgentWorkSendCIFailuresSupported(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.agentWorkStore()
	switch {
	case on && st.release < AgentWorkWithSendCIFailures:
		st.release = AgentWorkWithSendCIFailures
	case !on && st.release > AgentWorkWithAutoMerge:
		st.release = AgentWorkWithAutoMerge
	}
}

// SetAgentWorkWaitForChecksSupported moves the fake across the release that
// added wait_for_checks. false makes it older than that release: a newer one
// becomes the release with send_ci_failures but not wait_for_checks (nor
// send_errors or models), and an older one stays as it is. true makes it at least that
// release: one older is upgraded to it (auto_merge, send_ci_failures and
// wait_for_checks, still no send_errors or models), and a newer one is left as
// it is.
func (s *Server) SetAgentWorkWaitForChecksSupported(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.agentWorkStore()
	switch {
	case on && st.release < AgentWorkWithWaitForChecks:
		st.release = AgentWorkWithWaitForChecks
	case !on && st.release > AgentWorkWithSendCIFailures:
		st.release = AgentWorkWithSendCIFailures
	}
}

// SetAgentWorkSendErrorsSupported moves the fake across the release that
// added send_errors. false makes it older than that release: a newer one
// becomes the release with wait_for_checks but not send_errors (nor models),
// and an older one stays as it is. true makes it at least that release: one
// older is upgraded to it (the other three too, still no models), and the
// newest is left as it is.
func (s *Server) SetAgentWorkSendErrorsSupported(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.agentWorkStore()
	switch {
	case on && st.release < AgentWorkWithSendErrors:
		st.release = AgentWorkWithSendErrors
	case !on && st.release > AgentWorkWithWaitForChecks:
		st.release = AgentWorkWithWaitForChecks
	}
}

// SetAgentWorkModelsSupported moves the fake across the release that added
// models. false makes it older than that release: the newest becomes the
// release with send_errors but not models, and an older one stays as it is.
// true makes it the newest, which knows the other four too.
func (s *Server) SetAgentWorkModelsSupported(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.agentWorkStore()
	switch {
	case on:
		st.release = AgentWorkWithModels
	case st.release > AgentWorkWithSendErrors:
		st.release = AgentWorkWithSendErrors
	}
}

// SetAgentWorkModelsOffered sets the AI models the fake offers, in order, as
// a Flightdeck that adds a model to its list, or takes one off it, does. A
// model taken off the list stays stored for the kinds it was set for, and a
// write may send it back for those kinds unchanged.
func (s *Server) SetAgentWorkModelsOffered(names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agentWorkStore().models = append([]string(nil), names...)
}

// knowsAutoMerge reports whether the release the fake acts as has auto_merge.
func (st *agentWorkStore) knowsAutoMerge() bool {
	return st.release >= AgentWorkWithAutoMerge
}

// knowsSendCIFailures reports whether the release the fake acts as has
// send_ci_failures.
func (st *agentWorkStore) knowsSendCIFailures() bool {
	return st.release >= AgentWorkWithSendCIFailures
}

// knowsWaitForChecks reports whether the release the fake acts as has
// wait_for_checks.
func (st *agentWorkStore) knowsWaitForChecks() bool {
	return st.release >= AgentWorkWithWaitForChecks
}

// knowsSendErrors reports whether the release the fake acts as has
// send_errors (and the read-only send_errors_since that comes with it).
func (st *agentWorkStore) knowsSendErrors() bool {
	return st.release >= AgentWorkWithSendErrors
}

// knowsModels reports whether the release the fake acts as has models.
func (st *agentWorkStore) knowsModels() bool {
	return st.release >= AgentWorkWithModels
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
	cp.Models = copyModels(row.Models)
	return &cp
}

// copyModels is a copy of a models map, never nil.
func copyModels(models map[string]string) map[string]string {
	out := make(map[string]string, len(models))
	for kind, model := range models {
		out[kind] = model
	}
	return out
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
	before, beforeResearch, beforeSendErrors := row.LabelID, row.ResearchLabelID, row.SendErrors
	fn(row)
	if !sameID(before, row.LabelID) {
		stampLabelChosenAt(row)
	}
	if !sameID(beforeResearch, row.ResearchLabelID) {
		stampResearchLabelChosenAt(row)
	}
	if beforeSendErrors != row.SendErrors {
		stampSendErrorsSince(row)
	}
	row.LockVersion++
	now := time.Now()
	row.UpdatedAt = &now
}

func defaultAgentWork() AgentWorkSetting {
	return AgentWorkSetting{
		Kinds: []string{}, Models: map[string]string{}, WaitForChecks: true, BaseRef: "main", MaxInProgress: 1, DailyBudgetUSD: 10, TaskMaxUSD: 5,
		TaskMaxMinutes: 30, QueueMinutes: 60, Runbook: "implement-work-item@1",
	}
}

// The kinds the API accepts. Implement and fix-error reach an item through
// the agent label (label_id), research through the research label
// (research_label_id).
const (
	agentWorkImplement = "implement-work-item"
	agentWorkFixError  = "fix-error"
	agentWorkResearch  = "research"
)

var (
	agentWorkKinds    = []string{agentWorkImplement, agentWorkFixError, agentWorkResearch}
	agentWorkRunbooks = []string{"implement-work-item@1"}
	// agentWorkModels are the AI models the API offers, in its order, until a
	// test changes the list (SetAgentWorkModelsOffered).
	agentWorkModels = []string{"sonnet-5-5", "opus-5-5"}
	// agentWorkWritable are the settable keys in the API's order (the settings
	// page's). writableKeys leaves models, auto_merge, send_ci_failures,
	// send_errors and wait_for_checks out on a Flightdeck older than each.
	agentWorkWritable = []string{
		"enabled", "kinds", "models", "auto_merge", "send_ci_failures", "send_errors", "wait_for_checks", "label_id",
		"accept_machine_labels", "research_label_id", "agent_account_id", "base_ref", "max_in_progress",
		"daily_budget_usd", "task_max_usd", "task_max_minutes", "queue_minutes", "runbook",
	}
	agentWorkReadOnly = map[string]string{
		"project_id":      "it is the :project_id in the path, and a project's agent work settings belong to that project",
		"blockers":        "Flightdeck works them out from the settings and the project each time they are read",
		"label_chosen_at": "Flightdeck sets it when label_id changes: to now when a label is chosen, and to null when it is cleared",
		"research_label_chosen_at": "Flightdeck sets it when research_label_id changes: to now when a label is chosen, " +
			"and to null when it is cleared",
		// Only on a Flightdeck that has send_errors (readOnlyKey); an older one
		// refuses it as an unknown key.
		"send_errors_since": "Flightdeck sets it when send_errors changes: to now when it is turned on, and to null " +
			"when it is turned off",
		"updated_at": "the database sets it",
	}
	// agentWorkNullable are the references a blank CLEARS rather than leaves
	// alone.
	agentWorkNullable = []string{"label_id", "research_label_id", "agent_account_id"}
)

// writableKeys is agentWorkWritable as this Flightdeck has it.
func (st *agentWorkStore) writableKeys() []string {
	out := make([]string, 0, len(agentWorkWritable))
	for _, key := range agentWorkWritable {
		if (key == "auto_merge" && !st.knowsAutoMerge()) || (key == "send_ci_failures" && !st.knowsSendCIFailures()) ||
			(key == "wait_for_checks" && !st.knowsWaitForChecks()) || (key == "send_errors" && !st.knowsSendErrors()) ||
			(key == "models" && !st.knowsModels()) {
			continue
		}
		out = append(out, key)
	}
	return out
}

// readOnlyKey reports why key is read-only on this Flightdeck, if it is.
// send_errors_since came with send_errors, so an older Flightdeck does not
// know it at all and refuses it as an unknown key instead.
func (st *agentWorkStore) readOnlyKey(key string) (string, bool) {
	if key == "send_errors_since" && !st.knowsSendErrors() {
		return "", false
	}
	why, ok := agentWorkReadOnly[key]
	return why, ok
}

// agentWorkNoOpinion is what a blank means for every key but the nullable
// references: leave the key alone.
type agentWorkNoOpinion struct{}

// agentWorkBlank is the API's Api::BlankValue.no_opinion?: a null, or a
// string that Ruby's String#strip leaves empty. Ruby strips only ASCII
// whitespace and NUL, so a no-break space (U+00A0) or an ideographic space
// (U+3000) is a value, where Go's strings.TrimSpace would strip it.
func agentWorkBlank(v any) bool {
	if v == nil {
		return true
	}
	str, ok := v.(string)
	return ok && strings.Trim(str, rubyStripped) == ""
}

// rubyStripped is what Ruby's String#strip removes from either end.
const rubyStripped = "\x00\t\n\v\f\r "

// agentWorkEcho repeats a refused value back the way the API does
// (value.inspect.truncate(100)): Ruby's inspect of the parsed JSON, so a
// string is quoted ("true"), an empty object is {}, and null is nil, cut to
// 100 characters with "...".
func agentWorkEcho(v any) string {
	out := []rune(rubyInspect(v))
	if len(out) <= 100 {
		return string(out)
	}
	return string(out[:97]) + "..."
}

// railsDeepMunge is what Rails' params parsing does to a JSON value before an
// applier sees it: every null inside a list is dropped, at any depth, and a
// null anywhere else is kept.
func railsDeepMunge(v any) any {
	switch t := v.(type) {
	case []any:
		out := make([]any, 0, len(t))
		for _, e := range t {
			if e != nil {
				out = append(out, railsDeepMunge(e))
			}
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = railsDeepMunge(e)
		}
		return out
	}
	return v
}

// rubyInspect is Ruby's inspect of a value json.Decoder produced (with
// UseNumber). It differs from the API's in two places: object keys come out
// sorted, where Ruby keeps the order they were sent in, and a number is
// written as sent, where Ruby prints 1.50 as 1.5 and 1e2 as 100.0. Neither
// matters: the provider only ever sends these settings as their own JSON
// types, so it never meets a refusal that echoes an object or such a number.
func rubyInspect(v any) string {
	switch t := v.(type) {
	case nil:
		return "nil"
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		return t.String()
	case string:
		return rubyInspectString(t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, rubyInspect(e))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, rubyInspectString(k)+" => "+rubyInspect(t[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// rubyInspectString is Ruby's String#inspect for the characters JSON can
// carry: the named escapes, an escaped # before {, $ or @, other control
// characters as \uXXXX, and everything else as it is.
func rubyInspectString(str string) string {
	named := map[rune]string{'"': `\"`, '\\': `\\`, '\n': `\n`, '\t': `\t`, '\r': `\r`, '\f': `\f`, '\v': `\v`,
		'\b': `\b`, '\a': `\a`, 0x1b: `\e`}
	runes := []rune(str)
	var b strings.Builder
	b.WriteByte('"')
	for i, r := range runes {
		switch {
		case named[r] != "":
			b.WriteString(named[r])
		case r == '#' && i+1 < len(runes) && strings.ContainsRune("{$@", runes[i+1]):
			b.WriteString(`\#`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
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
		if contains(agentWorkNullable, key) {
			return (*int64)(nil), ""
		}
		return agentWorkNoOpinion{}, ""
	}
	switch key {
	case "enabled", "accept_machine_labels", "auto_merge", "send_ci_failures", "send_errors", "wait_for_checks":
		if b, ok := v.(bool); ok {
			return b, ""
		}
		return nil, fmt.Sprintf("%s must be true or false, got %s", key, agentWorkEcho(v))
	case "max_in_progress", "task_max_minutes", "queue_minutes":
		if i, ok := agentWorkWhole(v); ok {
			return i, ""
		}
		return nil, fmt.Sprintf("%s must be a whole number, got %s", key, agentWorkEcho(v))
	case "label_id", "research_label_id", "agent_account_id":
		if i, ok := agentWorkWhole(v); ok && i >= 1 {
			return &i, ""
		}
		return nil, fmt.Sprintf("%s must be an id (a positive whole number) or null, got %s", key, agentWorkEcho(v))
	case "daily_budget_usd", "task_max_usd":
		n, ok := v.(json.Number)
		if !ok {
			return nil, fmt.Sprintf("%s must be a number, got %s", key, agentWorkEcho(v))
		}
		return n, ""
	case "kinds":
		list, ok := v.([]any)
		if !ok {
			return nil, fmt.Sprintf("kinds must be a list of kind names, got %s", agentWorkEcho(v))
		}
		kinds := make([]string, 0, len(list))
		// The API counts every unknown entry for "kind" or "kinds", and names
		// at most five, each once.
		var unknown []string
		unknownCount := 0
		for _, e := range list {
			str, isStr := e.(string)
			if !isStr {
				return nil, fmt.Sprintf("kinds must be a list of kind names, got %s", agentWorkEcho(v))
			}
			if !contains(agentWorkKinds, str) {
				unknownCount++
				if !contains(unknown, str) {
					unknown = append(unknown, str)
				}
			}
			kinds = append(kinds, str)
		}
		if len(unknown) > 0 {
			what := "an unknown kind"
			if unknownCount > 1 {
				what = "unknown kinds"
			}
			if len(unknown) > 5 {
				unknown = unknown[:5]
			}
			echoes := make([]string, 0, len(unknown))
			for _, kind := range unknown {
				echoes = append(echoes, agentWorkEcho(kind))
			}
			return nil, fmt.Sprintf("kinds has %s: %s (valid: %s)", what, strings.Join(echoes, ", "), strings.Join(agentWorkKinds, ", "))
		}
		return kinds, ""
	case "runbook":
		str, ok := v.(string)
		if !ok {
			return nil, fmt.Sprintf("runbook must be a string, got %s", agentWorkEcho(v))
		}
		if !contains(agentWorkRunbooks, str) {
			return nil, fmt.Sprintf("runbook is not a runbook the pool knows, got %q (valid: %s)", str, strings.Join(agentWorkRunbooks, ", "))
		}
		return str, ""
	case "base_ref":
		if str, ok := v.(string); ok {
			return str, ""
		}
		return nil, fmt.Sprintf("base_ref must be a string, got %s", agentWorkEcho(v))
	}
	return nil, "unknown key: " + key
}

// modelsValue is the API's check of models (Api::AgentWorkAttributes#models!),
// returning the map to store, agentWorkNoOpinion{} to leave it alone, or a 422
// invalid_attribute message. A blank for the whole key is no opinion; any
// other value that isn't an object is refused. The object REPLACES the stored
// map: a kind left out, or whose model is blank, goes back to the default. An
// unknown kind is refused, and so is a model the release doesn't offer, unless
// it is the one already stored for that kind, which passes so that a read
// PATCHes back unchanged after its model came off the list.
//
// The API names unknown kinds, and walks the models, in the order they were
// sent; the fake's decoded object has no order, so it names unknown kinds
// sorted and walks the models in the kinds' order. Only a write with more than
// one thing wrong can tell, and the provider never reads which one is named.
func (st *agentWorkStore) modelsValue(v any, stored map[string]string) (any, string) {
	if agentWorkBlank(v) {
		return agentWorkNoOpinion{}, ""
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, "models must be an object of kinds of work to model names, got " + agentWorkEcho(v)
	}
	var unknown []string
	for kind := range obj {
		if !contains(agentWorkKinds, kind) {
			unknown = append(unknown, kind)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		what := "an unknown kind"
		if len(unknown) > 1 {
			what = "unknown kinds"
		}
		if len(unknown) > 5 {
			unknown = unknown[:5]
		}
		echoes := make([]string, 0, len(unknown))
		for _, kind := range unknown {
			echoes = append(echoes, agentWorkEcho(kind))
		}
		return nil, fmt.Sprintf("models has %s: %s (valid: %s)", what, strings.Join(echoes, ", "), strings.Join(agentWorkKinds, ", "))
	}
	out := map[string]string{}
	for _, kind := range agentWorkKinds {
		model, sent := obj[kind]
		if !sent || agentWorkBlank(model) {
			continue
		}
		name, isName := model.(string)
		held, wasStored := stored[kind]
		if !isName || (!contains(st.models, name) && (!wasStored || held != name)) {
			return nil, fmt.Sprintf("models.%s must be one of: %s, got %s", kind, strings.Join(st.models, ", "), agentWorkEcho(model))
		}
		out[kind] = name
	}
	return out, ""
}

// applyAgentWork mirrors the API's write: read-only keys refused by name,
// unknown keys refused, every value type-checked before anything is assigned,
// then the model's value rules against the merged row. It returns the merged
// candidate and whether any setting changed, or a status and message.
func (s *Server) applyAgentWork(p *Project, current AgentWorkSetting, submitted map[string]any) (AgentWorkSetting, bool, int, string, string) {
	delete(submitted, "lock_version")
	var readOnly []string
	for key := range submitted {
		if why, ro := s.agentWorkStore().readOnlyKey(key); ro {
			readOnly = append(readOnly, key+" is read-only over the API — "+why)
		}
	}
	if len(readOnly) > 0 {
		sort.Strings(readOnly)
		return current, false, http.StatusUnprocessableEntity, "invalid_attribute", strings.Join(readOnly, "; ")
	}
	// An older Flightdeck's unknown-key check comes before any value check, so
	// it refuses auto_merge, send_ci_failures, send_errors or wait_for_checks
	// whatever its value, a blank included.
	writable := s.agentWorkStore().writableKeys()
	var unknown []string
	for key := range submitted {
		if !contains(writable, key) {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		plural := ""
		if len(unknown) > 1 {
			plural = "s"
		}
		return current, false, http.StatusUnprocessableEntity, "invalid_attribute",
			"unknown key" + plural + ": " + strings.Join(unknown, ", ") + " (settable: " + strings.Join(writable, ", ") + ")"
	}
	// Rails' params parsing drops every null inside a list before the API's
	// applier sees the values, so ["main", null] is read, and echoed, as
	// ["main"]. The API reads a null inside kinds from the raw body first and
	// refuses it, echoing the list as JSON.
	if list, ok := submitted["kinds"].([]any); ok {
		for _, e := range list {
			if e == nil {
				raw, _ := json.Marshal(list)
				echo := []rune(string(raw))
				if len(echo) > 100 {
					echo = append(echo[:97], []rune("...")...)
				}
				return current, false, http.StatusUnprocessableEntity, "invalid_attribute",
					"kinds must be a list of kind names, got " + string(echo)
			}
		}
	}
	values := map[string]any{}
	for key, raw := range submitted {
		var v any
		var msg string
		if key == "models" {
			v, msg = s.agentWorkStore().modelsValue(railsDeepMunge(raw), current.Models)
		} else {
			v, msg = agentWorkValue(key, railsDeepMunge(raw))
		}
		if msg != "" {
			return current, false, http.StatusUnprocessableEntity, "invalid_attribute", msg
		}
		if _, skip := v.(agentWorkNoOpinion); !skip {
			values[key] = v
		}
	}

	next := current
	next.Kinds = append([]string(nil), current.Kinds...)
	next.Models = copyModels(current.Models)
	var moneyRefused []string
	// agentWorkValue has already checked each value's type for its key.
	for key, v := range values {
		switch t := v.(type) {
		case bool:
			switch key {
			case "enabled":
				next.Enabled = t
			case "auto_merge":
				next.AutoMerge = t
			case "send_ci_failures":
				next.SendCIFailures = t
			case "send_errors":
				next.SendErrors = t
			case "wait_for_checks":
				next.WaitForChecks = t
			default:
				next.AcceptMachineLabels = t
			}
		case []string:
			next.Kinds = t
		case map[string]string:
			next.Models = t
		case *int64:
			switch key {
			case "label_id":
				next.LabelID = t
			case "research_label_id":
				next.ResearchLabelID = t
			default:
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
	labelChanged, researchChanged := !sameID(current.LabelID, next.LabelID), !sameID(current.ResearchLabelID, next.ResearchLabelID)
	if labelChanged && next.LabelID != nil && !s.projectLabel(p, *next.LabelID) {
		problems = append(problems, "Agent label must be a label in this project")
	}
	if researchChanged && next.ResearchLabelID != nil && !s.projectLabel(p, *next.ResearchLabelID) {
		problems = append(problems, "Research label must be a label in this project")
	}
	// One label cannot mean both "build it" and "research it first". Checked
	// when either changes, and always named as the research label's problem.
	if (labelChanged || researchChanged) && next.ResearchLabelID != nil && sameID(next.LabelID, next.ResearchLabelID) {
		problems = append(problems, "Research label must be different from the agent label")
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

// forgetAgentWorkLabel is the database's ON DELETE SET NULL on both label
// references: a deleted label is no longer any project's agent label or
// research label. Like the foreign key, it moves no lock_version and leaves
// the chosen time alone; the read reports that as null while the id is null.
// The caller holds s.mu.
func (s *Server) forgetAgentWorkLabel(id int64) {
	st := s.agentWorkStore()
	if st == nil {
		return
	}
	for _, row := range st.byProject {
		if row.LabelID != nil && *row.LabelID == id {
			row.LabelID = nil
		}
		if row.ResearchLabelID != nil && *row.ResearchLabelID == id {
			row.ResearchLabelID = nil
		}
	}
}

// projectLabel reports whether id is one of p's labels.
func (s *Server) projectLabel(p *Project, id int64) bool {
	l := s.labels().byID[id]
	return l != nil && l.ProjectID == p.ID
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
	if len(a.Models) != len(b.Models) {
		return false
	}
	for kind, model := range a.Models {
		if held, ok := b.Models[kind]; !ok || held != model {
			return false
		}
	}
	return a.Enabled == b.Enabled && a.AutoMerge == b.AutoMerge && a.SendCIFailures == b.SendCIFailures && a.SendErrors == b.SendErrors &&
		a.WaitForChecks == b.WaitForChecks &&
		sameID(a.LabelID, b.LabelID) && a.AcceptMachineLabels == b.AcceptMachineLabels &&
		sameID(a.ResearchLabelID, b.ResearchLabelID) && sameID(a.AgentAccountID, b.AgentAccountID) &&
		a.BaseRef == b.BaseRef && a.MaxInProgress == b.MaxInProgress && a.DailyBudgetUSD == b.DailyBudgetUSD && a.TaskMaxUSD == b.TaskMaxUSD && a.TaskMaxMinutes == b.TaskMaxMinutes &&
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

func stampResearchLabelChosenAt(row *AgentWorkSetting) {
	if row.ResearchLabelID == nil {
		row.ResearchLabelChosenAt = nil
		return
	}
	now := time.Now()
	row.ResearchLabelChosenAt = &now
}

// stampSendErrorsSince is the API's stamp when send_errors changes: now when
// it is turned on, nil when it is turned off.
func stampSendErrorsSince(row *AgentWorkSetting) {
	if !row.SendErrors {
		row.SendErrorsSince = nil
		return
	}
	now := time.Now()
	row.SendErrorsSince = &now
}

// agentWorkBlockers is the fake's blockers, in the API's order, for the ones
// it models; anything added with AddAgentWorkBlockers follows them.
//
// The repository and label blockers follow the ticked kinds, because a
// blocker stops every kind. Implement and fix-error need the agent label and
// a linked repository; research needs only the research label. So
// no_github_repo is listed when implement or fix-error could run (or nothing
// is ticked), no_label when the agent label is needed but unusable and
// research can't run either, and no_research_label when research is ticked
// but its label is unusable and no other kind can run.
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
	usable := func(id *int64) bool { return id != nil && s.projectLabel(p, *id) }
	agentLabelKinds := contains(row.Kinds, agentWorkImplement) || contains(row.Kinds, agentWorkFixError)
	researchKind := contains(row.Kinds, agentWorkResearch)
	agentRuns := agentLabelKinds && usable(row.LabelID)
	researchRuns := researchKind && usable(row.ResearchLabelID)
	if len(row.Kinds) == 0 {
		add("no_kinds", "No kinds of work are ticked.")
	}
	if (len(row.Kinds) == 0 || agentRuns) && !s.githubRepoLinked(p) {
		add("no_github_repo", "The project has no linked GitHub repository.")
	}
	if (len(row.Kinds) == 0 || agentLabelKinds) && !usable(row.LabelID) && !researchRuns {
		add("no_label", "No agent label is chosen.")
	}
	if researchKind && !researchRuns && !agentRuns {
		add("no_research_label", "No research label is chosen.")
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
	var labelChosenAt, researchLabelChosenAt, updatedAt any
	if row.LabelID != nil && row.LabelChosenAt != nil {
		labelChosenAt = iso(*row.LabelChosenAt)
	}
	if row.ResearchLabelID != nil && row.ResearchLabelChosenAt != nil {
		researchLabelChosenAt = iso(*row.ResearchLabelChosenAt)
	}
	if row.UpdatedAt != nil {
		updatedAt = iso(*row.UpdatedAt)
	}
	kinds := append([]string{}, row.Kinds...)
	out := map[string]any{
		"project_id":               p.ID,
		"enabled":                  row.Enabled,
		"kinds":                    kinds,
		"label_id":                 row.LabelID,
		"label_chosen_at":          labelChosenAt,
		"accept_machine_labels":    row.AcceptMachineLabels,
		"research_label_id":        row.ResearchLabelID,
		"research_label_chosen_at": researchLabelChosenAt,
		"agent_account_id":         row.AgentAccountID,
		"base_ref":                 row.BaseRef,
		"max_in_progress":          row.MaxInProgress,
		"daily_budget_usd":         row.DailyBudgetUSD,
		"task_max_usd":             row.TaskMaxUSD,
		"task_max_minutes":         row.TaskMaxMinutes,
		"queue_minutes":            row.QueueMinutes,
		"runbook":                  row.Runbook,
		"blockers":                 s.agentWorkBlockers(p, row),
		"lock_version":             row.LockVersion,
		"updated_at":               updatedAt,
	}
	// Each always present on a Flightdeck that has the setting; absent on one
	// that predates it.
	if s.agentWorkStore().knowsAutoMerge() {
		out["auto_merge"] = row.AutoMerge
	}
	if s.agentWorkStore().knowsSendCIFailures() {
		out["send_ci_failures"] = row.SendCIFailures
	}
	if s.agentWorkStore().knowsWaitForChecks() {
		out["wait_for_checks"] = row.WaitForChecks
	}
	if s.agentWorkStore().knowsModels() {
		// An object, {} when no kind has a model.
		out["models"] = copyModels(row.Models)
	}
	if s.agentWorkStore().knowsSendErrors() {
		out["send_errors"] = row.SendErrors
		// Null while send_errors is off, as the API reads it.
		var since any
		if row.SendErrors && row.SendErrorsSince != nil {
			since = iso(*row.SendErrorsSince)
		}
		out["send_errors_since"] = since
	}
	return out
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
	if !sameID(current.ResearchLabelID, next.ResearchLabelID) {
		stampResearchLabelChosenAt(&next)
	}
	if current.SendErrors != next.SendErrors {
		stampSendErrorsSince(&next)
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
