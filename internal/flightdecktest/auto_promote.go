package flightdecktest

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The auto-promote settings ride the self-healing resource beside `rollback`:
// the read reports them flat, resolved against their defaults, and the write
// takes them flat too. They are stored in their own map on the project
// (Project.AutoPromote), under the API's stored keys, which differ from the
// wire keys only for the mode ("mode" stored, "auto_promote" on the wire).
//
// The rules mirror the API's, which checks the whole request before it saves
// anything:
//
//   - null or a blank string is no opinion for every key, and for every part
//     of promote_window;
//   - auto_promote is "off", "deps_only" or "all", trimmed and lowercased; a
//     boolean is refused;
//   - soak_hours is a whole number from 4 to 720, heads_up_hours from 1 to 72;
//   - promote_window parts merge onto the window in effect, and the merged
//     window is checked whole: days names at least one day, none twice; times
//     are HH:MM with start before end, and only end may be 24:00; time_zone is
//     an exact IANA name;
//   - freeze_dates is at most 100 ISO dates, none twice, stored sorted; past
//     dates are kept, and [] clears the list;
//   - a refusal is a 422 invalid_attribute whose message starts with the
//     field it is about (freeze_dates[1], promote_window.days);
//   - a value equal to the one in effect is not stored, so a setting left on
//     its default stays on it.

// The stored keys and their wire names.
var (
	autoPromoteStoredKeys = []string{"mode", "soak_hours", "promote_window", "freeze_dates", "heads_up_hours"}
	autoPromoteWireKeys   = map[string]string{
		"mode": "auto_promote", "soak_hours": "soak_hours", "promote_window": "promote_window",
		"freeze_dates": "freeze_dates", "heads_up_hours": "heads_up_hours",
	}
	// AutoPromoteWritable are the wire keys, in the API's order.
	AutoPromoteWritable = []string{"auto_promote", "soak_hours", "promote_window", "freeze_dates", "heads_up_hours"}
	autoPromoteModes    = []string{"off", "deps_only", "all"}
	autoPromoteDays     = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}
	promoteWindowParts  = []string{"days", "start", "end", "time_zone"}
	autoPromoteLimits   = map[string][2]int64{"soak_hours": {4, 720}, "heads_up_hours": {1, 72}}
)

// autoPromoteMaxFreezeDates is the most freeze dates a project may list.
const autoPromoteMaxFreezeDates = 100

// AutoPromoteDefaults are the API's defaults, under the stored keys.
func AutoPromoteDefaults() map[string]any {
	return map[string]any{
		"mode":       "off",
		"soak_hours": int64(24),
		"promote_window": map[string]any{
			"days": []string{"mon", "tue", "wed", "thu", "fri"}, "start": "09:00", "end": "16:00",
			"time_zone": "America/New_York",
		},
		"freeze_dates":   []string{},
		"heads_up_hours": int64(4),
	}
}

// fakeTimeZones are the IANA names the fake knows. The API knows every name
// in the server's time zone database; a test only needs a handful, and an
// exact list keeps the fake's answer the same on every machine (a lookup on a
// case-insensitive file system would take "america/new_york").
var fakeTimeZones = []string{
	"Africa/Nairobi", "America/Chicago", "America/Denver", "America/Los_Angeles", "America/New_York",
	"America/Port-au-Prince", "Asia/Kolkata", "Asia/Tokyo", "Australia/Sydney", "Etc/GMT+5", "Etc/UTC",
	"Europe/Berlin", "Europe/London", "Pacific/Auckland", "UTC",
}

// fakeRailsZones maps a few of Rails' own zone names to their IANA names, for
// the API's "Did you mean" hint.
var fakeRailsZones = map[string]string{
	"Eastern Time (US & Canada)": "America/New_York",
	"Central Time (US & Canada)": "America/Chicago",
	"Pacific Time (US & Canada)": "America/Los_Angeles",
}

// resolveAutoPromote is the stored map with every unset key at its default:
// the API's, or one SetAutoPromoteDefault changed.
func (s *Server) resolveAutoPromote(stored map[string]any) map[string]any {
	out := AutoPromoteDefaults()
	for k, v := range s.selfHealingStore().autoPromoteDefaults {
		out[k] = v
	}
	for k, v := range stored {
		out[k] = v
	}
	return out
}

// autoPromoteEcho repeats a refused value back the way the API does.
func autoPromoteEcho(v any) string { return agentWorkEcho(v) }

// autoPromoteRefusal is a 422 invalid_attribute message.
type autoPromoteRefusal string

// parseAutoPromote validates every auto-promote key in submitted (wire keys)
// against the project's settings in effect, and returns the changes under the
// stored keys, only for keys with an opinion. A null inside a list is
// refused first, as the API does from the body as sent. It assigns nothing.
func parseAutoPromote(current map[string]any, submitted map[string]any) (map[string]any, autoPromoteRefusal) {
	if dates, ok := submitted["freeze_dates"].([]any); ok {
		for _, d := range dates {
			if d == nil {
				raw, _ := json.Marshal(dates)
				return nil, autoPromoteRefusal("freeze_dates must be a list of ISO 8601 dates (YYYY-MM-DD), got " + truncate100(string(raw)))
			}
		}
	}
	if window, ok := submitted["promote_window"].(map[string]any); ok {
		if days, ok := window["days"].([]any); ok {
			for _, d := range days {
				if d == nil {
					raw, _ := json.Marshal(days)
					return nil, autoPromoteRefusal("promote_window.days must be a list of day names (" +
						strings.Join(autoPromoteDays, ", ") + "), got " + truncate100(string(raw)))
				}
			}
		}
	}
	changes := map[string]any{}
	for _, stored := range autoPromoteStoredKeys {
		wire := autoPromoteWireKeys[stored]
		value, has := submitted[wire]
		if !has || blankValue(value) {
			continue
		}
		var (
			parsed  any
			refusal autoPromoteRefusal
		)
		switch stored {
		case "mode":
			parsed, refusal = autoPromoteMode(value)
		case "soak_hours", "heads_up_hours":
			parsed, refusal = autoPromoteHours(stored, value)
		case "promote_window":
			currentWindow, _ := current["promote_window"].(map[string]any)
			parsed, refusal = autoPromoteWindow(value, currentWindow)
		case "freeze_dates":
			parsed, refusal = autoPromoteFreezeDates(value)
		}
		if refusal != "" {
			return nil, refusal
		}
		changes[stored] = parsed
	}
	return changes, ""
}

func truncate100(s string) string {
	r := []rune(s)
	if len(r) <= 100 {
		return s
	}
	return string(r[:97]) + "..."
}

func autoPromoteMode(v any) (string, autoPromoteRefusal) {
	if s, ok := v.(string); ok {
		token := strings.ToLower(strings.Trim(s, rubyStripped))
		if contains(autoPromoteModes, token) {
			return token, ""
		}
	}
	return "", autoPromoteRefusal(`auto_promote must be "off", "deps_only" or "all", got ` + autoPromoteEcho(v))
}

// wholeNumber is the API's WHOLE_NUMBER: digits, optionally followed by ".0".
var wholeNumber = regexp.MustCompile(`^\d+(?:\.0+)?$`)

func autoPromoteHours(key string, v any) (int64, autoPromoteRefusal) {
	limit := autoPromoteLimits[key]
	var (
		n  int64
		ok bool
	)
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			n, ok = i, true
		} else if f, err := t.Float64(); err == nil && !math.IsInf(f, 0) && f == math.Trunc(f) && math.Abs(f) < 1e15 {
			n, ok = int64(f), true
		}
	case string:
		token := strings.Trim(t, rubyStripped)
		if len(token) <= 64 && wholeNumber.MatchString(token) {
			if i, err := strconv.ParseInt(strings.SplitN(token, ".", 2)[0], 10, 64); err == nil {
				n, ok = i, true
			}
		}
	}
	if !ok || n < limit[0] || n > limit[1] {
		return 0, autoPromoteRefusal(fmt.Sprintf("%s must be a whole number of hours from %d to %d, got %s",
			key, limit[0], limit[1], autoPromoteEcho(v)))
	}
	return n, ""
}

func autoPromoteWindow(v any, current map[string]any) (map[string]any, autoPromoteRefusal) {
	parts, ok := v.(map[string]any)
	if !ok {
		return nil, autoPromoteRefusal("promote_window must be an object with days, start, end and time_zone, got " + autoPromoteEcho(v))
	}
	var unknown []string
	for k := range parts {
		if !contains(promoteWindowParts, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		what := "an unknown part"
		if len(unknown) > 1 {
			what = "unknown parts"
		}
		if len(unknown) > 5 {
			unknown = unknown[:5]
		}
		return nil, autoPromoteRefusal("promote_window has " + what + ": " + strings.Join(unknown, ", ") +
			" (settable: " + strings.Join(promoteWindowParts, ", ") + ")")
	}
	merged := map[string]any{}
	for k, val := range current {
		merged[k] = val
	}
	for _, part := range promoteWindowParts {
		value := parts[part]
		if blankValue(value) {
			continue
		}
		var (
			parsed  any
			refusal autoPromoteRefusal
		)
		switch part {
		case "days":
			parsed, refusal = promoteWindowDays(value)
		case "start":
			parsed, refusal = promoteWindowTime("promote_window.start", value, false)
		case "end":
			parsed, refusal = promoteWindowTime("promote_window.end", value, true)
		case "time_zone":
			parsed, refusal = promoteWindowTimeZone(value)
		}
		if refusal != "" {
			return nil, refusal
		}
		merged[part] = parsed
	}
	start, end := asString(merged["start"]), asString(merged["end"])
	if windowMinutes(start) >= windowMinutes(end) {
		return nil, autoPromoteRefusal(fmt.Sprintf("promote_window.start (%s) must be before promote_window.end (%s). "+
			"A window can't run past midnight; use 24:00 for the end of the day", start, end))
	}
	return merged, ""
}

func promoteWindowDays(v any) ([]string, autoPromoteRefusal) {
	list, ok := v.([]any)
	refuse := autoPromoteRefusal("promote_window.days must be a list of day names (" + strings.Join(autoPromoteDays, ", ") +
		"), got " + autoPromoteEcho(v))
	if !ok {
		return nil, refuse
	}
	tokens := make([]string, 0, len(list))
	for _, e := range list {
		s, isString := e.(string)
		if !isString {
			return nil, refuse
		}
		tokens = append(tokens, strings.ToLower(strings.Trim(s, rubyStripped)))
	}
	var unknown []string
	for _, day := range tokens {
		if !contains(autoPromoteDays, day) && !contains(unknown, day) {
			unknown = append(unknown, day)
		}
	}
	if len(unknown) > 0 {
		what := "an unknown day"
		if len(unknown) > 1 {
			what = "unknown days"
		}
		if len(unknown) > 5 {
			unknown = unknown[:5]
		}
		echoes := make([]string, 0, len(unknown))
		for _, day := range unknown {
			echoes = append(echoes, autoPromoteEcho(day))
		}
		return nil, autoPromoteRefusal("promote_window.days has " + what + ": " + strings.Join(echoes, ", ") +
			" (days: " + strings.Join(autoPromoteDays, ", ") + ")")
	}
	if len(tokens) == 0 {
		return nil, "promote_window.days must name at least one day"
	}
	seen := map[string]int{}
	var repeated []string
	for _, day := range tokens {
		seen[day]++
		if seen[day] == 2 {
			repeated = append(repeated, day)
		}
	}
	if len(repeated) > 0 {
		return nil, autoPromoteRefusal("promote_window.days names " + strings.Join(repeated, ", ") + " more than once")
	}
	out := []string{}
	for _, day := range autoPromoteDays {
		if contains(tokens, day) {
			out = append(out, day)
		}
	}
	return out, ""
}

// windowTime is the API's TIME: "H:MM" or "HH:MM".
var windowTime = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)

func promoteWindowTime(key string, v any, endOfDay bool) (string, autoPromoteRefusal) {
	upper := "23:59"
	if endOfDay {
		upper = "24:00"
	}
	refuse := autoPromoteRefusal(fmt.Sprintf("%s must be a time of day as HH:MM (00:00 to %s), got %s", key, upper, autoPromoteEcho(v)))
	s, ok := v.(string)
	if !ok {
		return "", refuse
	}
	m := windowTime.FindStringSubmatch(strings.Trim(s, rubyStripped))
	if m == nil {
		return "", refuse
	}
	hours, _ := strconv.Atoi(m[1])
	mins, _ := strconv.Atoi(m[2])
	endOfTheDay := endOfDay && hours == 24 && mins == 0
	if mins >= 60 || (hours >= 24 && !endOfTheDay) {
		return "", refuse
	}
	return fmt.Sprintf("%02d:%02d", hours, mins), ""
}

func promoteWindowTimeZone(v any) (string, autoPromoteRefusal) {
	var token string
	if s, ok := v.(string); ok {
		token = strings.Trim(s, rubyStripped)
		if contains(fakeTimeZones, token) {
			return token, ""
		}
	}
	hint := ""
	if suggestion, ok := fakeRailsZones[token]; ok {
		hint = ". Did you mean " + suggestion + "?"
	} else {
		for _, zone := range fakeTimeZones {
			if token != "" && strings.EqualFold(zone, token) {
				hint = ". Did you mean " + zone + "?"
				break
			}
		}
	}
	return "", autoPromoteRefusal("promote_window.time_zone must be an IANA time zone name, such as America/New_York, got " +
		autoPromoteEcho(v) + hint)
}

func windowMinutes(hhmm string) int {
	parts := strings.SplitN(hhmm, ":", 2)
	if len(parts) != 2 {
		return 0
	}
	h, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	return h*60 + m
}

// isoDate is the API's DATE_PATTERN, before the year and calendar checks.
var isoDate = regexp.MustCompile(`^(\d{4})-(\d{1,2})-(\d{1,2})$`)

func autoPromoteFreezeDates(v any) ([]string, autoPromoteRefusal) {
	list, ok := v.([]any)
	if !ok {
		return nil, autoPromoteRefusal("freeze_dates must be a list of ISO 8601 dates (YYYY-MM-DD), got " + autoPromoteEcho(v))
	}
	if len(list) > autoPromoteMaxFreezeDates {
		return nil, autoPromoteRefusal(fmt.Sprintf("freeze_dates may list at most %d dates, got %d", autoPromoteMaxFreezeDates, len(list)))
	}
	dates := make([]string, 0, len(list))
	for i, e := range list {
		name := fmt.Sprintf("freeze_dates[%d]", i)
		date, refusal := freezeDate(name, e)
		if refusal != "" {
			return nil, refusal
		}
		dates = append(dates, date)
	}
	seen := map[string]int{}
	var repeated []string
	for _, d := range dates {
		seen[d]++
		if seen[d] == 2 {
			repeated = append(repeated, d)
		}
	}
	if len(repeated) > 0 {
		if len(repeated) > 5 {
			repeated = repeated[:5]
		}
		return nil, autoPromoteRefusal("freeze_dates lists " + strings.Join(repeated, ", ") + " more than once")
	}
	sort.Strings(dates)
	return dates, ""
}

// freezeDate is the API's date check: a string YYYY-MM-DD (one-digit month
// and day allowed, written back with two), a year from 1 to 9999, and a real
// calendar date.
func freezeDate(name string, v any) (string, autoPromoteRefusal) {
	refuse := func(hint string) autoPromoteRefusal {
		return autoPromoteRefusal(name + " must be an ISO 8601 date (YYYY-MM-DD), got " + autoPromoteEcho(v) + hint)
	}
	s, ok := v.(string)
	if !ok {
		return "", refuse("")
	}
	m := isoDate.FindStringSubmatch(strings.Trim(s, rubyStripped))
	if m == nil {
		return "", refuse("")
	}
	year, _ := strconv.Atoi(m[1])
	month, _ := strconv.Atoi(m[2])
	day, _ := strconv.Atoi(m[3])
	if year < 1 || year > 9999 {
		return "", refuse(". The year must be between 1 and 9999")
	}
	t := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if month < 1 || month > 12 || t.Day() != day || int(t.Month()) != month {
		return "", refuse(", which is not a real date")
	}
	return fmt.Sprintf("%04d-%02d-%02d", year, month, day), ""
}

// assignAutoPromote stores the changes that differ from the settings in
// effect, and reports whether any did. A value equal to the one in effect is
// left unstored, so a key on its default stays on it.
func (s *Server) assignAutoPromote(p *Project, changes map[string]any) bool {
	current := s.resolveAutoPromote(p.AutoPromote)
	changed := false
	for k, v := range changes {
		if autoPromoteEqual(current[k], v) {
			continue
		}
		if p.AutoPromote == nil {
			p.AutoPromote = map[string]any{}
		}
		p.AutoPromote[k] = v
		changed = true
	}
	return changed
}

func autoPromoteEqual(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// autoPromoteWire is the read's auto-promote keys, resolved, under the wire
// keys.
func (s *Server) autoPromoteWire(stored map[string]any) map[string]any {
	resolved := s.resolveAutoPromote(stored)
	out := map[string]any{}
	for _, k := range autoPromoteStoredKeys {
		out[autoPromoteWireKeys[k]] = resolved[k]
	}
	return out
}

// AutoPromoteBlocker is one entry of the read's auto_promote_blockers.
type AutoPromoteBlocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// autoPromoteBlockers is the fake's auto_promote_blockers, in the API's
// order: rollback_not_auto, every rollback blocker as it is, then
// no_github_repository and not_yet_available. It does not model the
// server-wide switch or the recent-errors check.
func (s *Server) autoPromoteBlockers(p *Project) []AutoPromoteBlocker {
	out := []AutoPromoteBlocker{}
	if !truthy(s.resolveSelfHealing(p.SelfHealing)["armed"]) {
		out = append(out, AutoPromoteBlocker{Code: "rollback_not_auto",
			Message: "Auto-promote needs auto-rollback as its safety net, and this project's rollback is set to report only. Turn on auto-rollback first."})
	}
	for _, b := range s.rollbackBlockers(p) {
		out = append(out, AutoPromoteBlocker(b))
	}
	if !s.githubRepoLinked(p) {
		out = append(out, AutoPromoteBlocker{Code: "no_github_repository",
			Message: "No GitHub repository is linked to this project, so Flightdeck can't see which changes are waiting to be promoted."})
	}
	if !s.selfHealingStore().promoting {
		out = append(out, AutoPromoteBlocker{Code: "not_yet_available",
			Message: "Flightdeck doesn't promote release candidates on its own yet. This setting is saved and takes effect when it does."})
	}
	return out
}

// SetAutoPromoteSupported makes the self-healing endpoint act as a Flightdeck
// from before auto-promote (false): the read has none of its keys, and a
// write naming one is refused as an unknown setting. A Flightdeck from before
// `rollback` (SetSelfHealingLegacy) is older than auto-promote too.
func (s *Server) SetAutoPromoteSupported(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selfHealingStore().autoPromoteUnsupported = !on
}

// SetAutoPromoteDefault changes an auto-promote setting's default (a stored
// key: "mode", "soak_hours", "promote_window", "freeze_dates" or
// "heads_up_hours"), as a Flightdeck release that changes it does: projects
// that never set it read the new value, and no lock_version moves.
func (s *Server) SetAutoPromoteDefault(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.selfHealingStore()
	if st.autoPromoteDefaults == nil {
		st.autoPromoteDefaults = map[string]any{}
	}
	st.autoPromoteDefaults[key] = value
}

// SetAutoPromotePromoting controls the not_yet_available blocker: false (the
// fake's start) lists it for every project, as a Flightdeck that cannot
// promote yet does.
func (s *Server) SetAutoPromotePromoting(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selfHealingStore().promoting = on
}

// AutoPromoteOf returns a copy of a project's stored auto-promote settings
// (stored keys, only the ones set), or nil when none are stored.
func (s *Server) AutoPromoteOf(projectID int64) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.projects().byID[projectID]
	if p == nil || p.AutoPromote == nil {
		return nil
	}
	return copyAutoPromote(p.AutoPromote)
}

// SetAutoPromoteOutOfBand changes a project's stored auto-promote settings
// the way the settings page does (no HTTP): fn edits the stored map, under
// the stored keys, and the project's lock_version moves on.
func (s *Server) SetAutoPromoteOutOfBand(projectID int64, fn func(map[string]any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.projects().byID[projectID]
	if p == nil {
		return
	}
	if p.AutoPromote == nil {
		p.AutoPromote = map[string]any{}
	}
	fn(p.AutoPromote)
	p.LockVersion++
}

func copyAutoPromote(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case []string:
			out[k] = append([]string{}, t...)
		case map[string]any:
			inner := make(map[string]any, len(t))
			for ik, iv := range t {
				if days, ok := iv.([]string); ok {
					iv = append([]string{}, days...)
				}
				inner[ik] = iv
			}
			out[k] = inner
		default:
			out[k] = v
		}
	}
	return out
}
