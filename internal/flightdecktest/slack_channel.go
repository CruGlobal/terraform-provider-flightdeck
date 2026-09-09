package flightdecktest

import (
	"net/http"
	"sort"
	"strings"
)

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["slack_channel"] = &slackChannelStore{enabled: true, available: true, scopesSufficient: true}
		mux.HandleFunc("GET /api/v1/projects/{project_id}/slack-channel", s.showSlackChannel)
		mux.HandleFunc("PATCH /api/v1/projects/{project_id}/slack-channel", s.updateSlackChannel)
	})
}

// slackChannelStore holds the endpoint's knobs. available and
// scopesSufficient are workspace-wide, like the Slack integration they stand
// for.
type slackChannelStore struct {
	// enabled=false simulates a Flightdeck without the endpoint: every route 404s.
	enabled bool
	// available=false is a workspace with no connected Slack integration, and
	// scopesSufficient=false a connection that predates the channel scopes.
	// Either one means a write stores the configuration and provisions nothing.
	available        bool
	scopesSufficient bool
	// notAdmin=true withholds :administer_project from the token's user.
	notAdmin bool
}

func (s *Server) slackChannelStore() *slackChannelStore {
	st, _ := s.stores["slack_channel"].(*slackChannelStore)
	return st
}

// SetSlackChannelEndpoint enables or disables the slack-channel routes, to
// simulate a Flightdeck version that does not expose them.
func (s *Server) SetSlackChannelEndpoint(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slackChannelStore().enabled = on
}

// SetSlackIntegration sets the workspace's Slack posture: whether an
// integration is connected at all, and whether its token carries the channel
// scopes.
func (s *Server) SetSlackIntegration(available, scopesSufficient bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.slackChannelStore()
	st.available, st.scopesSufficient = available, scopesSufficient
}

// SetProjectAdmin controls whether the token's user administers projects. The
// slack-channel routes are :administer_project for reads as well as writes.
func (s *Server) SetProjectAdmin(admin bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slackChannelStore().notAdmin = !admin
}

// CompleteSlackProvision stands in for the provision job finishing: it links
// the channel the way the worker would, without an HTTP round trip.
func (s *Server) CompleteSlackProvision(projectID int64, channelID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.projects().byID[projectID]; p != nil {
		p.SlackChannelID = channelID
		p.SlackProvisionStatus = "linked"
		p.SlackProvisionNote = "Created #" + slackChannelBasename(p)
	}
}

// SlackEventCategories are the activity categories the filter accepts, in the
// API's order. The legacy "completed" category the settings form still
// tolerates is deliberately absent.
var SlackEventCategories = []string{"created", "state_changed", "field_changed", "logged", "assigned"}

// SlackEventDefaults are the values a read reports for a category the project
// has never overridden.
var SlackEventDefaults = map[string]bool{
	"created": true, "state_changed": true, "field_changed": false,
	"logged": false, "assigned": true,
}

// slackChannelWritable are the settable keys, spelled as the read emits them.
var slackChannelWritable = []string{
	"slack_channel_enabled", "slack_notifications_enabled",
	"slack_channel_name", "slack_event_filter",
}

// slackChannelReadOnly are the keys a read emits and a write must refuse BY
// NAME with a reason — a client that cannot see its write being discarded
// re-diffs forever.
var slackChannelReadOnly = map[string]string{
	"project_id":              "it identifies the resource",
	"slack_channel_available": "it reports whether the workspace has a connected Slack integration",
	"slack_scopes_sufficient": "it reports the workspace Slack token's scopes",
	"slack_channel_linked":    "it is derived from slack_channel_id",
	"slack_channel_id":        "the channel is linked by the provisioner, not by a client. Set slack_channel_name to point at a different channel",
	"slack_channel_basename":  "it is derived from slack_channel_name (or the project name)",
	"slack_provision_status":  "the provision job reports it",
	"slack_provision_note":    "the provision job reports it",
	"slack_invites_skipped":   "the provision job reports it",
}

// slackChannelMaxName is the API's channel-name length cap.
const slackChannelMaxName = 80

// normalizeSlackChannelName coerces arbitrary text into a Slack channel name:
// trim, lower-case, runs of other characters become a single "-", no leading
// or trailing "-", cut to the length cap.
func normalizeSlackChannelName(source string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(source)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > slackChannelMaxName {
		name = name[:slackChannelMaxName]
	}
	return name
}

// slackChannelBasename is the effective name the provisioner uses: the
// override if there is one, else the project-name default.
func slackChannelBasename(p *Project) string {
	if p.SlackChannelName != "" {
		return normalizeSlackChannelName(p.SlackChannelName)
	}
	return normalizeSlackChannelName("fd-" + normalizeSlackChannelName(p.Name))
}

// applySlackChannel mirrors the API's write rules: only submitted keys change,
// server-owned keys are refused by name, the notifications switch merges into
// the features jsonb, a blank or null name resets the override, the event
// filter MERGES rather than replacing, and a change to the effective name
// drops the stored channel id so the next provision re-links.
func (s *Server) applySlackChannel(p *Project, submitted map[string]any) (int, string, string) {
	var readOnly, unknown []string
	for k := range submitted {
		switch {
		case slackChannelReadOnly[k] != "":
			readOnly = append(readOnly, k)
		case k == "lock_version" || contains(slackChannelWritable, k):
			// A precondition, or settable.
		default:
			unknown = append(unknown, k)
		}
	}
	if len(readOnly) > 0 {
		sort.Strings(readOnly)
		reasons := make([]string, 0, len(readOnly))
		for _, k := range readOnly {
			reasons = append(reasons, k+" is read-only — "+slackChannelReadOnly[k])
		}
		return http.StatusUnprocessableEntity, "invalid_attribute", strings.Join(reasons, "; ")
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return http.StatusUnprocessableEntity, "invalid_attribute",
			"unknown key: " + strings.Join(unknown, ", ") + " (settable: " + strings.Join(slackChannelWritable, ", ") + ")"
	}

	// Captured before any assignment: the rename check compares against it.
	oldBasename := slackChannelBasename(p)

	if v, ok := submitted["slack_channel_enabled"]; ok {
		enabled, valid := strictBool(v)
		if !valid {
			return http.StatusUnprocessableEntity, "invalid_attribute", "slack_channel_enabled must be true or false, got " + asString(v)
		}
		p.SlackChannelEnabled = enabled
	}
	if v, ok := submitted["slack_notifications_enabled"]; ok {
		enabled, valid := strictBool(v)
		if !valid {
			return http.StatusUnprocessableEntity, "invalid_attribute", "slack_notifications_enabled must be true or false, got " + asString(v)
		}
		// The master switch lives in the features jsonb, so it is merged, never
		// rebuilt: a write here must not wipe the Features page's keys.
		if p.Features == nil {
			p.Features = map[string]bool{}
		}
		p.Features["slack"] = enabled
	}
	if v, ok := submitted["slack_channel_name"]; ok {
		if v == nil {
			p.SlackChannelName = ""
		} else {
			raw, isString := v.(string)
			if !isString {
				return http.StatusUnprocessableEntity, "invalid_attribute", "slack_channel_name must be a string"
			}
			if strings.TrimSpace(raw) == "" {
				p.SlackChannelName = "" // blank resets to the project-name default
			} else if name := normalizeSlackChannelName(raw); name == "" {
				return http.StatusUnprocessableEntity, "invalid_attribute",
					"slack_channel_name " + asString(v) + " normalizes to an empty Slack channel name — it needs at least " +
						"one letter or digit (send null to use the project-name default)"
			} else {
				p.SlackChannelName = name
			}
		}
	}
	if v, ok := submitted["slack_event_filter"]; ok {
		categories, isMap := v.(map[string]any)
		if !isMap {
			return http.StatusUnprocessableEntity, "invalid_attribute", "slack_event_filter must be an object of category => true/false"
		}
		var unknownCategories []string
		for k := range categories {
			if !contains(SlackEventCategories, k) {
				unknownCategories = append(unknownCategories, k)
			}
		}
		if len(unknownCategories) > 0 {
			sort.Strings(unknownCategories)
			return http.StatusUnprocessableEntity, "invalid_attribute",
				"unknown slack_event_filter category: " + strings.Join(unknownCategories, ", ") +
					" (valid: " + strings.Join(SlackEventCategories, ", ") + ")"
		}
		if p.SlackEventFilter == nil {
			p.SlackEventFilter = map[string]bool{}
		}
		for k, raw := range categories {
			on, valid := strictBool(raw)
			if !valid {
				return http.StatusUnprocessableEntity, "invalid_attribute", "slack_event_filter." + k + " must be true or false, got " + asString(raw)
			}
			p.SlackEventFilter[k] = on
		}
	}

	// The provisioner short-circuits on an existing channel id, so a rename
	// that kept it would be a silent no-op. The old channel is left on Slack.
	if slackChannelBasename(p) != oldBasename {
		p.SlackChannelID = ""
	}
	return 0, "", ""
}

// enqueueSlackProvision mirrors the model's enqueue: it no-ops when the
// channel is off or the workspace has no live integration, and otherwise
// reports a queued job rather than a finished channel.
func (s *Server) enqueueSlackProvision(p *Project) {
	if !p.SlackChannelEnabled || !s.slackChannelStore().available {
		return
	}
	if p.SlackChannelID == "" {
		p.SlackProvisionStatus = "queued"
		p.SlackProvisionNote = ""
	}
}

func (s *Server) serializeSlackChannel(p *Project) map[string]any {
	st := s.slackChannelStore()
	filter := map[string]any{}
	for _, category := range SlackEventCategories {
		on, stored := p.SlackEventFilter[category]
		if !stored {
			on = SlackEventDefaults[category]
		}
		filter[category] = on
	}
	notifications, stored := p.Features["slack"]
	if !stored {
		notifications = DefaultFeatures["slack"]
	}
	return map[string]any{
		"project_id":                  p.ID,
		"slack_channel_available":     st.available,
		"slack_scopes_sufficient":     st.available && st.scopesSufficient,
		"slack_channel_enabled":       p.SlackChannelEnabled,
		"slack_notifications_enabled": notifications,
		"slack_channel_linked":        p.SlackChannelID != "",
		"slack_channel_id":            nilIfEmpty(p.SlackChannelID),
		"slack_channel_name":          nilIfEmpty(p.SlackChannelName),
		"slack_channel_basename":      slackChannelBasename(p),
		"slack_provision_status":      nilIfEmpty(p.SlackProvisionStatus),
		"slack_provision_note":        nilIfEmpty(p.SlackProvisionNote),
		"slack_invites_skipped":       p.SlackInvitesSkipped,
		"slack_event_filter":          filter,
		"lock_version":                p.LockVersion,
	}
}

// nilIfEmpty maps the fake's zero string onto the API's null.
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// strictBool mirrors the API's fail-CLOSED boolean: "no" is refused rather
// than read as true.
func strictBool(v any) (bool, bool) {
	switch value := v.(type) {
	case bool:
		return value, true
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true", "1", "t", "on":
			return true, true
		case "false", "0", "f", "off":
			return false, true
		}
	}
	return false, false
}

// The existence check runs before the admin bar so a non-admin cannot tell
// project ids apart by 403 vs 404.
func (s *Server) showSlackChannel(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.liveProject(pid)
	if p == nil || !s.slackChannelStore().enabled {
		notFound(w)
		return
	}
	if !s.requireProjectAdmin(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.serializeSlackChannel(p))
}

func (s *Server) updateSlackChannel(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(w, r, "project_id")
	if !ok {
		return
	}
	s.mu.Lock()
	p := s.liveProject(pid)
	if p == nil || !s.slackChannelStore().enabled {
		s.mu.Unlock()
		notFound(w)
		return
	}
	if !s.requireProjectAdmin(w) {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	attrs, ok := decodeBody(w, r, "slack_channel")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The precondition pins the PROJECT's lock_version: these columns live on
	// that row, so it is the same token the project resource uses.
	if !checkIfMatch(w, r, p.LockVersion) {
		return
	}
	candidate := *p
	candidate.Features = map[string]bool{}
	for k, v := range p.Features {
		candidate.Features[k] = v
	}
	candidate.SlackEventFilter = map[string]bool{}
	for k, v := range p.SlackEventFilter {
		candidate.SlackEventFilter[k] = v
	}
	if status, code, msg := s.applySlackChannel(&candidate, attrs); status != 0 {
		writeError(w, status, code, msg)
		return
	}
	candidate.LockVersion++
	*p = candidate
	// The write saves the configuration and enqueues the job; the channel is
	// created and linked afterwards.
	s.enqueueSlackProvision(p)
	writeJSON(w, http.StatusOK, s.serializeSlackChannel(p))
}

// requireProjectAdmin mirrors the :administer_project bar the slack-channel
// routes use for reads as well as writes.
func (s *Server) requireProjectAdmin(w http.ResponseWriter) bool {
	if s.slackChannelStore().notAdmin {
		writeError(w, http.StatusForbidden, "forbidden", "You do not have permission to administer this project")
		return false
	}
	return true
}
