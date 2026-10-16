package flightdecktest

import (
	"net/http"
	"sort"
	"strings"
)

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		s.stores["slack_channel"] = &slackChannelStore{
			enabled: true, available: true, scopesSufficient: true, modeSupported: true,
			unusable: map[string]bool{}, archived: map[string]bool{}, needsInvite: map[string]bool{},
			existing: map[string]bool{},
		}
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
	// modeSupported=false simulates a Flightdeck older than the Slack channel
	// mode: reads leave slack_channel_mode out, and a write naming it is
	// refused as an unknown key, whatever its value.
	modeSupported bool
	// existing holds the channels, by derived name, that already exist in
	// Slack, that Flightdeck can use (public ones), and that Flightdeck did not
	// make. Create mode refuses one; link mode needs one.
	existing map[string]bool
	// unusable holds the channels, by derived name, that exist in Slack but
	// that Flightdeck cannot see: private with the bot not a member (archived
	// or not, since Slack lists neither to a non-member). With no mode chosen
	// a write is refused with the bot message; create mode calls the name
	// taken, and link mode calls the channel missing.
	unusable map[string]bool
	// archived holds the channels, by derived name, that Flightdeck can see
	// but that are archived. Every mode refuses one with the archived message.
	archived map[string]bool
	// needsInvite holds the channels, by derived name, that Flightdeck can
	// link but is not in and cannot join (a public channel and a connection
	// without channels:join, say): a write links them with provision status
	// needs_invite instead of linked.
	needsInvite map[string]bool
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

// SlackBotHandle is how the fake's messages name the Flightdeck bot a user
// has to invite into a channel.
const SlackBotHandle = "@example-bot"

// SetSlackChannelUnusable marks a channel as one that exists in Slack but that
// Flightdeck cannot see or post to: private, with the bot not a member. A
// write that would leave a project's channel enabled on it and not yet
// linked is then refused with 422 slack_channel_unusable, and nothing is
// saved: with no mode chosen the message names the channel and the bot to
// invite, create mode calls the name taken, and link mode calls the channel
// missing. The name is matched after the API's normalization, so "Example
// Channel" and "example-channel" are one channel. Passing false makes the
// channel usable again, the way inviting the bot would.
func (s *Server) SetSlackChannelUnusable(name string, unusable bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.slackChannelStore()
	if unusable {
		st.unusable[normalizeSlackChannelName(name)] = true
	} else {
		delete(st.unusable, normalizeSlackChannelName(name))
	}
}

// SetSlackChannelArchived marks a channel as one Flightdeck can see but that
// is archived. A write that would leave a project's channel enabled on it and
// not yet linked is refused with 422 slack_channel_unusable and the archived
// message, whatever the mode, and nothing is saved. Passing false is someone
// unarchiving it.
func (s *Server) SetSlackChannelArchived(name string, archived bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.slackChannelStore()
	if archived {
		st.archived[normalizeSlackChannelName(name)] = true
	} else {
		delete(st.archived, normalizeSlackChannelName(name))
	}
}

// SetSlackChannelNeedsInvite marks a channel as one Flightdeck can link but is
// not in and cannot join. A write that leaves a project's channel enabled on
// it links it with provision status needs_invite and a note saying what to
// do, and any later write retries the check. Passing false is someone
// inviting the Flightdeck app: the next write finds it a member and links it.
func (s *Server) SetSlackChannelNeedsInvite(name string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.slackChannelStore()
	if on {
		st.needsInvite[normalizeSlackChannelName(name)] = true
	} else {
		delete(st.needsInvite, normalizeSlackChannelName(name))
	}
}

// SetSlackChannelModeSupported turns the Slack channel mode on or off, to
// simulate a Flightdeck version that predates it.
func (s *Server) SetSlackChannelModeSupported(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slackChannelStore().modeSupported = on
}

// SetSlackChannelExists marks a channel as one that already exists in Slack,
// that Flightdeck can use, and that Flightdeck did not make. In create mode a
// write that would set the channel up on it is refused (the name is taken);
// in link mode it is the channel to link. With no mode chosen it is simply
// linked. The name is matched after the API's normalization, like the other
// channel knobs. Passing false is someone deleting the channel.
func (s *Server) SetSlackChannelExists(name string, exists bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.slackChannelStore()
	if exists {
		st.existing[normalizeSlackChannelName(name)] = true
	} else {
		delete(st.existing, normalizeSlackChannelName(name))
	}
}

// SetSlackChannelMode stands in for someone choosing the mode outside
// Terraform, on the settings page. Like any save there it bumps the project's
// lock_version. An empty mode is a project from before the mode existed.
func (s *Server) SetSlackChannelMode(projectID int64, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.projects().byID[projectID]; p != nil {
		p.SlackChannelMode = mode
		p.LockVersion++
	}
}

// FailSlackProvision stands in for the provision job failing: it records the
// failure and its note the way the worker would, and leaves the channel
// unlinked.
func (s *Server) FailSlackProvision(projectID int64, note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.projects().byID[projectID]; p != nil {
		p.SlackChannelID = ""
		p.SlackProvisionStatus = "failed"
		p.SlackProvisionNote = note
	}
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

// slackChannelWritable are the settable keys, spelled as the read emits them,
// on a Flightdeck older than the Slack channel mode. writableKeys adds the
// mode to them on one that has it.
var slackChannelWritable = []string{
	"slack_channel_enabled", "slack_notifications_enabled",
	"slack_channel_name", "slack_event_filter",
}

// SlackChannelModes are the values slack_channel_mode accepts, spelled
// exactly: the API neither trims nor folds case.
var SlackChannelModes = []string{"create", "link"}

func (st *slackChannelStore) writableKeys() []string {
	if !st.modeSupported {
		return slackChannelWritable
	}
	return append(append([]string{}, slackChannelWritable...), "slack_channel_mode")
}

// taken reports whether some channel already has the name: one Flightdeck
// can use, one it cannot see, one it can link but is not in, or an archived
// one.
func (st *slackChannelStore) taken(name string) bool {
	return st.existing[name] || st.unusable[name] || st.needsInvite[name] || st.archived[name]
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
// or trailing "-", cut to the length cap. The API applies this lazily, to
// DERIVE the basename — a written name is stored as typed — so the trim runs
// before the cut and a derived name may end in "-".
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
// filter MERGES rather than replacing, a blank or null mode changes nothing,
// and a change to the effective name drops the stored channel id so the next
// provision re-links.
func (s *Server) applySlackChannel(p *Project, submitted map[string]any) (int, string, string) {
	// An explicit null means "no opinion" on every key but the name, where it
	// is a real instruction (reset to the project-name default). An unset
	// optional serializes as null, so refusing it would leave a configuration
	// no way to be correct.
	for _, key := range []string{"slack_channel_enabled", "slack_notifications_enabled", "slack_event_filter"} {
		if value, sent := submitted[key]; sent && value == nil {
			delete(submitted, key)
		}
	}
	st := s.slackChannelStore()
	// The mode has no value to reset to (null is "nobody has chosen"), so a
	// null or blank leaves it alone. An older Flightdeck refuses the key as
	// unknown whatever its value, since its unknown-key check comes first.
	if value, sent := submitted["slack_channel_mode"]; sent && st.modeSupported {
		if text, isString := value.(string); value == nil || (isString && strings.TrimSpace(text) == "") {
			delete(submitted, "slack_channel_mode")
		}
	}
	writable := st.writableKeys()
	var readOnly, unknown []string
	for k := range submitted {
		switch {
		case slackChannelReadOnly[k] != "":
			readOnly = append(readOnly, k)
		case k == "lock_version" || contains(writable, k):
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
			"unknown key: " + strings.Join(unknown, ", ") + " (settable: " + strings.Join(writable, ", ") + ")"
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
			trimmed := strings.TrimSpace(raw)
			switch {
			case trimmed == "":
				p.SlackChannelName = "" // blank resets to the project-name default
			case normalizeSlackChannelName(trimmed) == "":
				return http.StatusUnprocessableEntity, "invalid_attribute",
					"slack_channel_name " + asString(v) + " normalizes to an empty Slack channel name — it needs at least " +
						"one letter or digit (send null to use the project-name default)"
			default:
				// Stored as typed: the derivation happens lazily, at provision
				// time, and is visible on the read as slack_channel_basename.
				p.SlackChannelName = trimmed
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
	if v, ok := submitted["slack_channel_mode"]; ok {
		mode, isString := v.(string)
		if !isString || !contains(SlackChannelModes, mode) {
			return http.StatusUnprocessableEntity, "invalid_attribute",
				`slack_channel_mode must be "create" or "link" (or null for no change), got ` + asString(v)
		}
		// A mode change never moves or drops the link: the mode only decides
		// what happens next.
		p.SlackChannelMode = mode
	}

	// The provisioner short-circuits on an existing channel id, so a rename
	// that kept it would be a silent no-op. The old channel is left on Slack.
	if slackChannelBasename(p) != oldBasename {
		p.SlackChannelID = ""
	}
	return 0, "", ""
}

// slackChannelChanged reports whether the write touched anything the Slack
// channel resource owns, standing in for the saved_changes check the API makes.
func slackChannelChanged(before, after *Project) bool {
	// The features entry is compared by what is STORED, presence included: the
	// column changing from "no opinion" to an explicit value is a change even
	// when the value matches the default it was resolving to.
	beforeSlack, beforeStored := before.Features["slack"]
	afterSlack, afterStored := after.Features["slack"]
	if before.SlackChannelEnabled != after.SlackChannelEnabled ||
		before.SlackChannelMode != after.SlackChannelMode ||
		before.SlackChannelName != after.SlackChannelName ||
		before.SlackChannelID != after.SlackChannelID ||
		beforeStored != afterStored || beforeSlack != afterSlack ||
		len(before.SlackEventFilter) != len(after.SlackEventFilter) {
		return true
	}
	for category, on := range after.SlackEventFilter {
		if before.SlackEventFilter[category] != on {
			return true
		}
	}
	return false
}

// checkSlackChannel mirrors the API's synchronous check. A write that leaves
// the channel enabled and not yet linked, and that points it at a channel
// (turns it on, changes its effective name, or changes its mode), looks the
// channel up in Slack first, in the API's order:
//
//   - a live channel Flightdeck can see is linked, except in create mode,
//     which calls the name taken;
//   - an archived one Flightdeck can see is refused as archived, in every
//     mode;
//   - one Flightdeck cannot see (private, the bot not a member) is taken in
//     create mode, missing in link mode, and refused with the bot message
//     when no mode is chosen;
//   - no channel at all is missing in link mode, and otherwise passes, since
//     the job can create it.
//
// Every refusal is a 422 slack_channel_unusable, and nothing is saved.
//
// A linked channel is checked again only by a write that switches it to
// create, and is refused when someone other than Flightdeck made it. The API
// checks the linked channel by its id; here its name stands in for that.
//
// Without a connection that can see channels there is nothing to check with,
// so the write goes through as it did before the check existed; the API does
// the same when Slack is rate limiting or down.
func (s *Server) checkSlackChannel(before, p *Project) (int, string, string) {
	st := s.slackChannelStore()
	if !p.SlackChannelEnabled || !st.available || !st.scopesSufficient {
		return 0, "", ""
	}
	name := slackChannelBasename(p)
	remoded := p.SlackChannelMode != before.SlackChannelMode
	if p.SlackChannelID != "" {
		if remoded && p.SlackChannelMode == "create" && st.taken(name) {
			return slackChannelExists(name)
		}
		return 0, "", ""
	}
	if before.SlackChannelEnabled && slackChannelBasename(before) == name && !remoded {
		return 0, "", "" // a mute or a filter change: not aimed at the channel
	}
	live := st.existing[name] || st.needsInvite[name]
	unusable := func(message string) (int, string, string) {
		return http.StatusUnprocessableEntity, "slack_channel_unusable", message
	}
	switch {
	case live && p.SlackChannelMode == "create":
		return slackChannelExists(name)
	case live:
		return 0, "", "" // linked
	case st.archived[name]:
		return unusable("#" + name + " is archived. Unarchive it in Slack or choose a different channel name, then try again.")
	case p.SlackChannelMode == "create" && st.unusable[name]:
		return slackChannelExists(name)
	case p.SlackChannelMode == "link":
		return unusable("No Slack channel named #" + name + ". Check the name, or create a new channel instead. " +
			"If it's private, invite the Flightdeck app to it first.")
	case st.unusable[name]:
		return unusable("#" + name + " exists, but Flightdeck can't post to it: it's private and " + SlackBotHandle +
			" isn't a member, or it's archived. Invite " + SlackBotHandle + " to #" + name + " (or unarchive it), then try again.")
	}
	return 0, "", ""
}

// slackChannelExists is create mode's refusal of a name that is taken.
func slackChannelExists(name string) (int, string, string) {
	return http.StatusUnprocessableEntity, "slack_channel_unusable",
		"A Slack channel named #" + name + " already exists. To use it, link it instead of creating a new one."
}

// checkSlackMembership mirrors the half of the inline check that asks whether
// Flightdeck is in the channel. A write that links a channel Flightdeck is not
// in and cannot join records needs_invite with the API's note, and any write
// to a needs_invite channel checks again, even one that changes nothing:
// retrying is the point. The status columns are the provisioner's, so this
// never moves lock_version.
func (s *Server) checkSlackMembership(p *Project, changed bool) {
	st := s.slackChannelStore()
	if !p.SlackChannelEnabled || !st.available || !st.scopesSufficient {
		return
	}
	retry := p.SlackProvisionStatus == "needs_invite"
	linking := changed && p.SlackChannelID == ""
	if !retry && !linking {
		return
	}
	name := slackChannelBasename(p)
	if st.needsInvite[name] {
		p.SlackChannelID = "C" + strings.ToUpper(strings.ReplaceAll(name, "-", ""))
		p.SlackProvisionStatus = "needs_invite"
		p.SlackProvisionNote = "Linked #" + name + ", but Flightdeck isn't in it and can't join without the channels:join " +
			"permission. Invite the Flightdeck app to #" + name + ", or reconnect Slack in workspace settings to grant it, " +
			"then save the Slack settings again."
		return
	}
	if retry {
		p.SlackProvisionStatus = "linked"
		p.SlackProvisionNote = "Linked existing #" + name
	}
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
	out := map[string]any{
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
	// Always present on a Flightdeck that has the mode, null while nobody has
	// chosen; absent on one that predates it.
	if st.modeSupported {
		out["slack_channel_mode"] = nilIfEmpty(p.SlackChannelMode)
	}
	return out
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
	// Checked on the candidate, so a refused write leaves the project exactly
	// as it was: no saved keys, no lock_version bump, no enqueue.
	if status, code, msg := s.checkSlackChannel(p, &candidate); status != 0 {
		writeError(w, status, code, msg)
		return
	}
	// A write that changes nothing saves nothing, so it neither bumps the
	// lock_version nor re-enqueues provisioning — which would otherwise stamp
	// a channel still waiting to be linked back to "queued" and lose a
	// terminal failure note. The empty body this endpoint blesses is exactly
	// that case.
	changed := slackChannelChanged(p, &candidate)
	if changed {
		candidate.LockVersion++
		*p = candidate
		s.enqueueSlackProvision(p)
	}
	s.checkSlackMembership(p, changed)
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
