package client

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"
)

// The fake's auto-promote settings follow the API's contract. The provider's
// tests lean on it, so each rule is checked here directly, through the
// client: the read's defaults and keys, what a blank means, every value rule,
// the window's merge, the freeze dates' order, the whole request checked
// before anything saves, and a Flightdeck that predates auto-promote.

// autoPromoteFake is a fake with one project, and a client for it.
type autoPromoteFake struct {
	t       *testing.T
	fake    *flightdecktest.Server
	c       *Client
	project int64
}

func newAutoPromoteFake(t *testing.T) *autoPromoteFake {
	t.Helper()
	fake := flightdecktest.New(t)
	c, err := New(fake.URL, fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	return &autoPromoteFake{t: t, fake: fake, c: c, project: fake.AddProject("Auto-promote", "AUP").ID}
}

func (f *autoPromoteFake) read() *SelfHealing {
	f.t.Helper()
	sh, err := f.c.GetSelfHealing(context.Background(), f.project)
	if err != nil {
		f.t.Fatal(err)
	}
	return sh
}

// write PATCHes fields at the project's current lock_version.
func (f *autoPromoteFake) write(fields Fields) (*SelfHealing, error) {
	f.t.Helper()
	return f.c.UpdateSelfHealing(context.Background(), f.project, fields, f.read().LockVersion)
}

func (f *autoPromoteFake) mustWrite(fields Fields) *SelfHealing {
	f.t.Helper()
	sh, err := f.write(fields)
	if err != nil {
		f.t.Fatalf("writing %v: %v", fields, err)
	}
	return sh
}

// refused writes fields and wants a 422 invalid_attribute whose message
// starts with prefix, and nothing saved.
func (f *autoPromoteFake) refused(fields Fields, prefix string) string {
	f.t.Helper()
	before := f.read()
	_, err := f.write(fields)
	apiErr, ok := AsError(err)
	if !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute || !strings.HasPrefix(apiErr.Message, prefix) {
		f.t.Fatalf("writing %v: want a 422 invalid_attribute starting %q, got %v", fields, prefix, err)
	}
	was, _ := json.Marshal(before)
	is, _ := json.Marshal(f.read())
	if string(was) != string(is) {
		f.t.Fatalf("a refused write must save nothing:\nbefore %s\n after %s", was, is)
	}
	return apiErr.Message
}

func windowOf(sh *SelfHealing) string {
	if sh.PromoteWindow == nil {
		return "<nil>"
	}
	w := sh.PromoteWindow
	return fmt.Sprintf("%s %s-%s %s", strings.Join(w.Days, ","), w.Start, w.End, w.TimeZone)
}

func TestAutoPromote_readsTheDefaults(t *testing.T) {
	f := newAutoPromoteFake(t)
	sh := f.read()
	if !sh.ReportsAutoPromote() || *sh.AutoPromote != AutoPromoteOff {
		t.Fatalf("an unset project reads auto_promote off, got %s", show(sh.AutoPromote))
	}
	if *sh.SoakHours != 24 || *sh.HeadsUpHours != 4 || len(sh.FreezeDates) != 0 || sh.FreezeDates == nil {
		t.Fatalf("defaults: soak %s, heads-up %s, freeze %v", show(sh.SoakHours), show(sh.HeadsUpHours), sh.FreezeDates)
	}
	if got := windowOf(sh); got != "mon,tue,wed,thu,fri 09:00-16:00 America/New_York" {
		t.Fatalf("default window = %s", got)
	}
	for _, key := range SelfHealingAutoPromoteKeys {
		if !sh.Accepts(key) {
			t.Errorf("writable_settings leaves out %s: %v", key, sh.WritableSettings)
		}
	}
	// Every project lists not_yet_available, and rollback_not_auto while the
	// mode is report.
	codes := blockerCodes(sh)
	if !strings.HasPrefix(codes, "rollback_not_auto,") || !strings.HasSuffix(codes, ",not_yet_available") {
		t.Fatalf("auto_promote_blockers = %s", codes)
	}
	// Writing the defaults stores nothing.
	f.mustWrite(Fields{"auto_promote": "off", "soak_hours": 24, "heads_up_hours": 4, "freeze_dates": []string{},
		"promote_window": map[string]any{"days": []string{"mon", "tue", "wed", "thu", "fri"}, "start": "09:00"}})
	if stored := f.fake.AutoPromoteOf(f.project); stored != nil {
		t.Fatalf("re-stating the defaults must store nothing, got %v", stored)
	}
}

func blockerCodes(sh *SelfHealing) string {
	codes := make([]string, 0, len(sh.AutoPromoteBlockers))
	for _, b := range sh.AutoPromoteBlockers {
		codes = append(codes, b.Code)
	}
	return strings.Join(codes, ",")
}

func TestAutoPromote_mode(t *testing.T) {
	f := newAutoPromoteFake(t)
	// Trimmed and lowercased.
	if sh := f.mustWrite(Fields{"auto_promote": "  DEPS_ONLY "}); *sh.AutoPromote != AutoPromoteDepsOnly {
		t.Fatalf("auto_promote = %s", show(sh.AutoPromote))
	}
	// Null and blank are no opinion.
	for _, blank := range []any{nil, "", "   "} {
		if sh := f.mustWrite(Fields{"auto_promote": blank}); *sh.AutoPromote != AutoPromoteDepsOnly {
			t.Fatalf("a blank %#v changed the mode to %s", blank, show(sh.AutoPromote))
		}
	}
	for _, tc := range []struct {
		bad  any
		echo string
	}{{true, "true"}, {false, "false"}, {"on", `"on"`}, {1, "1"}, {[]any{"all"}, `["all"]`}} {
		msg := f.refused(Fields{"auto_promote": tc.bad}, "auto_promote must be")
		if want := `auto_promote must be "off", "deps_only" or "all", got ` + tc.echo; msg != want {
			t.Errorf("refusal = %q, want %q", msg, want)
		}
	}
	if sh := f.mustWrite(Fields{"auto_promote": "all"}); *sh.AutoPromote != AutoPromoteAll {
		t.Fatalf("auto_promote = %s", show(sh.AutoPromote))
	}
}

func TestAutoPromote_hours(t *testing.T) {
	f := newAutoPromoteFake(t)
	for _, tc := range []struct {
		key      string
		low, top int
	}{{"soak_hours", 4, 720}, {"heads_up_hours", 1, 72}} {
		for _, ok := range []any{tc.low, tc.top, fmt.Sprintf("%d", tc.low+1)} {
			f.mustWrite(Fields{tc.key: ok})
		}
		for _, bad := range []any{tc.low - 1, tc.top + 1, 5.5, true, "five", []any{}} {
			f.refused(Fields{tc.key: bad}, fmt.Sprintf("%s must be a whole number of hours from %d to %d, got ", tc.key, tc.low, tc.top))
		}
		// A blank is no opinion.
		f.mustWrite(Fields{tc.key: nil})
		f.mustWrite(Fields{tc.key: " "})
	}
	sh := f.read()
	if *sh.SoakHours != 5 || *sh.HeadsUpHours != 2 {
		t.Fatalf("soak %s, heads-up %s, want 5 and 2", show(sh.SoakHours), show(sh.HeadsUpHours))
	}
}

func TestAutoPromote_windowPartsMerge(t *testing.T) {
	f := newAutoPromoteFake(t)
	// One part replaces that part only.
	if got := windowOf(f.mustWrite(Fields{"promote_window": map[string]any{"start": "8:00"}})); got != "mon,tue,wed,thu,fri 08:00-16:00 America/New_York" {
		t.Fatalf("after start only: %s", got)
	}
	// Days are stored in week order, trimmed and lowercased; a blank part is
	// no opinion; {} names nothing.
	sh := f.mustWrite(Fields{"promote_window": map[string]any{"days": []any{"Sun", " wed", "mon"}, "end": nil, "time_zone": " "}})
	if got := windowOf(sh); got != "mon,wed,sun 08:00-16:00 America/New_York" {
		t.Fatalf("after days: %s", got)
	}
	f.mustWrite(Fields{"promote_window": map[string]any{}})
	f.mustWrite(Fields{"promote_window": nil})
	if got := windowOf(f.mustWrite(Fields{"promote_window": map[string]any{"end": "24:00", "time_zone": "Europe/London"}})); got != "mon,wed,sun 08:00-24:00 Europe/London" {
		t.Fatalf("after end and zone: %s", got)
	}
	// The merged window is checked whole: an end before the stored start.
	msg := f.refused(Fields{"promote_window": map[string]any{"end": "07:00"}}, "promote_window.start (08:00) must be before promote_window.end (07:00)")
	if !strings.Contains(msg, "use 24:00 for the end of the day") {
		t.Errorf("refusal = %q", msg)
	}
	f.refused(Fields{"promote_window": map[string]any{"start": "24:00"}}, "promote_window.start must be a time of day as HH:MM (00:00 to 23:59)")
	f.refused(Fields{"promote_window": map[string]any{"end": "24:01"}}, "promote_window.end must be a time of day as HH:MM (00:00 to 24:00)")
	f.refused(Fields{"promote_window": map[string]any{"start": "9am"}}, "promote_window.start must be a time of day")
	f.refused(Fields{"promote_window": map[string]any{"days": []any{}}}, "promote_window.days must name at least one day")
	f.refused(Fields{"promote_window": map[string]any{"days": []any{"mon", "Mon"}}}, "promote_window.days names mon more than once")
	f.refused(Fields{"promote_window": map[string]any{"days": []any{"monday"}}}, `promote_window.days has an unknown day: "monday"`)
	f.refused(Fields{"promote_window": map[string]any{"days": []any{"mon", nil}}}, "promote_window.days must be a list of day names")
	f.refused(Fields{"promote_window": map[string]any{"days": "mon"}}, "promote_window.days must be a list of day names")
	f.refused(Fields{"promote_window": map[string]any{"timezone": "UTC"}}, "promote_window has an unknown part: timezone (settable: days, start, end, time_zone)")
	f.refused(Fields{"promote_window": "always"}, "promote_window must be an object with days, start, end and time_zone")
	// The time zone is an exact IANA name.
	if msg := f.refused(Fields{"promote_window": map[string]any{"time_zone": "america/new_york"}}, "promote_window.time_zone must be an IANA time zone name"); !strings.HasSuffix(msg, "Did you mean America/New_York?") {
		t.Errorf("refusal = %q", msg)
	}
	if msg := f.refused(Fields{"promote_window": map[string]any{"time_zone": "Eastern Time (US & Canada)"}}, "promote_window.time_zone must be"); !strings.HasSuffix(msg, "Did you mean America/New_York?") {
		t.Errorf("refusal = %q", msg)
	}
}

func TestAutoPromote_freezeDates(t *testing.T) {
	f := newAutoPromoteFake(t)
	// Stored sorted and written with two digits; past dates are kept.
	sh := f.mustWrite(Fields{"freeze_dates": []any{"2030-12-25", "2001-1-5", " 2030-12-24"}})
	if got := strings.Join(sh.FreezeDates, ","); got != "2001-01-05,2030-12-24,2030-12-25" {
		t.Fatalf("freeze_dates = %s", got)
	}
	// A blank is no opinion; [] clears.
	f.mustWrite(Fields{"freeze_dates": nil})
	if got := len(f.read().FreezeDates); got != 3 {
		t.Fatalf("a null freeze_dates changed the list to %d dates", got)
	}
	if sh := f.mustWrite(Fields{"freeze_dates": []string{}}); len(sh.FreezeDates) != 0 {
		t.Fatalf("[] must clear the list, got %v", sh.FreezeDates)
	}
	f.refused(Fields{"freeze_dates": []any{"2030-12-24", "2030-02-30"}}, `freeze_dates[1] must be an ISO 8601 date (YYYY-MM-DD), got "2030-02-30", which is not a real date`)
	f.refused(Fields{"freeze_dates": []any{"Dec 24"}}, `freeze_dates[0] must be an ISO 8601 date (YYYY-MM-DD), got "Dec 24"`)
	f.refused(Fields{"freeze_dates": []any{"0000-01-01"}}, `freeze_dates[0] must be an ISO 8601 date (YYYY-MM-DD), got "0000-01-01". The year must be between 1 and 9999`)
	f.refused(Fields{"freeze_dates": []any{"2030-12-24", "2030-12-24"}}, "freeze_dates lists 2030-12-24 more than once")
	f.refused(Fields{"freeze_dates": []any{"2030-12-24", nil}}, "freeze_dates must be a list of ISO 8601 dates")
	f.refused(Fields{"freeze_dates": "2030-12-24"}, "freeze_dates must be a list of ISO 8601 dates")
	many := make([]any, 101)
	for i := range many {
		many[i] = fmt.Sprintf("2031-%02d-%02d", i/28+1, i%28+1)
	}
	f.refused(Fields{"freeze_dates": many}, "freeze_dates may list at most 100 dates, got 101")
	f.mustWrite(Fields{"freeze_dates": many[:100]})
}

// Flightdeck checks the whole request before anything saves, so a refused
// setting of either kind leaves the other kind unsaved too.
func TestAutoPromote_wholeRequestCheckedFirst(t *testing.T) {
	f := newAutoPromoteFake(t)
	f.refused(Fields{"auto_promote": "all", "bake_minutes": 0}, "bake_minutes must be between 1 and")
	f.refused(Fields{"rollback": "auto", "promote_window": map[string]any{"start": "25:00"}}, "promote_window.start must be")
	sh := f.read()
	if *sh.AutoPromote != AutoPromoteOff || sh.RollbackMode() != RollbackReport {
		t.Fatalf("nothing may be saved, got auto_promote %s and rollback %s", show(sh.AutoPromote), sh.RollbackMode())
	}
	// Together and valid, both save in one write, and the mode's blocker
	// for report only goes away.
	sh = f.mustWrite(Fields{"rollback": "auto", "auto_promote": "all"})
	if *sh.AutoPromote != AutoPromoteAll || sh.RollbackMode() != RollbackAuto {
		t.Fatalf("got auto_promote %s and rollback %s", show(sh.AutoPromote), sh.RollbackMode())
	}
	if strings.Contains(blockerCodes(sh), "rollback_not_auto") {
		t.Fatalf("rollback is auto, but the blockers still say otherwise: %s", blockerCodes(sh))
	}
}

// A Flightdeck from before auto-promote has none of its keys on a read, and
// refuses a write naming any of them as an unknown setting, listing what it
// does take. One from before `rollback` is older still.
func TestAutoPromote_olderFlightdeck(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("before rollback too: %t", legacy), func(t *testing.T) {
			f := newAutoPromoteFake(t)
			if legacy {
				f.fake.SetSelfHealingLegacy(true)
			} else {
				f.fake.SetAutoPromoteSupported(false)
			}
			sh := f.read()
			if sh.ReportsAutoPromote() || sh.PromoteWindow != nil || sh.AutoPromoteBlockers != nil || sh.Accepts("auto_promote") {
				t.Fatalf("an older Flightdeck reports no auto-promote, got %+v", sh)
			}
			msg := f.refused(Fields{"soak_hours": 24, "auto_promote": "off", "bake_minutes": 30},
				"unknown self-healing settings: auto_promote, soak_hours (settable: bake_minutes, ")
			if strings.Contains(strings.SplitN(msg, "(settable:", 2)[1], "auto_promote") {
				t.Fatalf("the settable list must not name auto-promote keys: %s", msg)
			}
			f.fake.SetSelfHealingLegacy(false)
			f.fake.SetAutoPromoteSupported(true)
			if sh := f.read(); !sh.ReportsAutoPromote() {
				t.Fatal("upgraded, the read reports auto-promote")
			}
		})
	}
}

// The API runs the go-live guard before it reads the auto-promote settings.
// So a write that turns the feature on over a paused auto-rollback project,
// without saying which mode, is refused as going live, even when an
// auto-promote value in it is wrong too.
func TestAutoPromote_goLiveGuardComesFirst(t *testing.T) {
	f := newAutoPromoteFake(t)
	f.mustWrite(Fields{"feature_enabled": true, "rollback": "auto"})
	f.mustWrite(Fields{"feature_enabled": false})
	_, err := f.write(Fields{"feature_enabled": true, "soak_hours": 3})
	if apiErr, ok := AsError(err); !ok || apiErr.Status != 422 || apiErr.Code != CodeArmingRefused {
		t.Fatalf("want the go-live refusal (arming_refused) first, got %v", err)
	}
	// Saying which mode lets the guard pass, and then the bad value is refused.
	f.refused(Fields{"feature_enabled": true, "rollback": "report", "soak_hours": 3}, "soak_hours must be a whole number of hours")
}

// More of the API's value rules, each against what AutoPromote::Settings
// does with it.
func TestAutoPromote_moreValueRules(t *testing.T) {
	f := newAutoPromoteFake(t)
	// Digits only: "0x10" is not sixteen.
	f.refused(Fields{"soak_hours": "0x10"}, `soak_hours must be a whole number of hours from 4 to 720, got "0x10"`)
	// A whole float is a whole number.
	if sh := f.mustWrite(Fields{"soak_hours": 30.0}); *sh.SoakHours != 30 {
		t.Fatalf("soak_hours 30.0 = %s", show(sh.SoakHours))
	}
	// Two spellings of one date are the same date twice.
	f.refused(Fields{"freeze_dates": []string{"2030-01-05", "2030-1-5"}}, "freeze_dates lists 2030-01-05 more than once")
	// A date column takes a date, not a timestamp.
	f.refused(Fields{"freeze_dates": []string{"2030-01-01T00:00:00Z"}}, `freeze_dates[0] must be an ISO 8601 date (YYYY-MM-DD), got "2030-01-01T00:00:00Z"`)
	// A blank list key is no opinion; a blank window part too.
	f.mustWrite(Fields{"freeze_dates": []string{"2031-01-01"}})
	f.mustWrite(Fields{"freeze_dates": "", "promote_window": map[string]any{"start": "", "end": nil}})
	sh := f.read()
	if len(sh.FreezeDates) != 1 || windowOf(sh) != "mon,tue,wed,thu,fri 09:00-16:00 America/New_York" {
		t.Fatalf("blanks changed something: %v, %s", sh.FreezeDates, windowOf(sh))
	}
}

// A default the fake changes (as a Flightdeck release can) reaches a project
// that never set the key, and moves no lock_version; one that set it keeps
// its own value.
func TestAutoPromote_changedDefault(t *testing.T) {
	f := newAutoPromoteFake(t)
	before := f.read()
	f.fake.SetAutoPromoteDefault("soak_hours", int64(48))
	after := f.read()
	if *after.SoakHours != 48 || after.LockVersion != before.LockVersion {
		t.Fatalf("soak_hours %s at lock_version %d, want 48 at %d", show(after.SoakHours), after.LockVersion, before.LockVersion)
	}
	f.mustWrite(Fields{"heads_up_hours": 6})
	f.fake.SetAutoPromoteDefault("heads_up_hours", int64(8))
	if sh := f.read(); *sh.HeadsUpHours != 6 {
		t.Fatalf("a set value must not follow the default, got %s", show(sh.HeadsUpHours))
	}
}
