package client

import (
	"strings"
	"testing"
)

// The fake's hotfix_incident_fixes follows the API's contract, checked here
// through the client the way the auto-promote settings are: it defaults to
// false, is read at the top level, takes a JSON boolean or one of the
// feature-flag words, reads false as a value and null or blank as no opinion,
// and refuses anything else by name without saving anything.
func TestSelfHealing_hotfixIncidentFixes(t *testing.T) {
	f := newAutoPromoteFake(t)
	sh := f.read()
	if sh.HotfixIncidentFixes == nil || *sh.HotfixIncidentFixes {
		t.Fatalf("an unset project reads hotfix_incident_fixes false, got %s", show(sh.HotfixIncidentFixes))
	}
	if !sh.Accepts("hotfix_incident_fixes") {
		t.Fatalf("writable_settings must list hotfix_incident_fixes, got %v", sh.WritableSettings)
	}
	if sh := f.mustWrite(Fields{"hotfix_incident_fixes": true}); !*sh.HotfixIncidentFixes {
		t.Fatal("hotfix_incident_fixes true was not saved")
	}
	for _, blank := range []any{nil, "", "   "} {
		if sh := f.mustWrite(Fields{"hotfix_incident_fixes": blank}); !*sh.HotfixIncidentFixes {
			t.Fatalf("a blank %#v changed hotfix_incident_fixes", blank)
		}
	}
	// false is a value, not a blank.
	if sh := f.mustWrite(Fields{"hotfix_incident_fixes": false}); *sh.HotfixIncidentFixes {
		t.Fatal("hotfix_incident_fixes false was not saved")
	}
	for _, tc := range []struct {
		word any
		want bool
	}{{"TRUE", true}, {" off ", false}, {"1", true}, {"0", false}, {"t", true}, {"F", false}, {"On", true}, {"false", false}} {
		if sh := f.mustWrite(Fields{"hotfix_incident_fixes": tc.word}); *sh.HotfixIncidentFixes != tc.want {
			t.Fatalf("%#v read as %t, want %t", tc.word, *sh.HotfixIncidentFixes, tc.want)
		}
	}
	for _, tc := range []struct {
		bad  any
		echo string
	}{{"yes", `"yes"`}, {1, "1"}, {0, "0"}, {[]any{}, "[]"}, {map[string]any{}, "{}"}} {
		msg := f.refused(Fields{"hotfix_incident_fixes": tc.bad, "bake_minutes": 25}, "hotfix_incident_fixes must be")
		if want := "hotfix_incident_fixes must be true or false, got " + tc.echo; msg != want {
			t.Errorf("refusal = %q, want %q", msg, want)
		}
	}
}

// A Flightdeck that predates hotfix_incident_fixes has no such key on a read,
// leaves it out of writable_settings, and refuses a write naming it as an
// unknown setting. One from before monitor_rollback predates it too.
func TestSelfHealing_hotfixIncidentFixesOnAnOlderFlightdeck(t *testing.T) {
	f := newAutoPromoteFake(t)
	f.fake.SetHotfixIncidentFixesSupported(false)
	sh := f.read()
	if sh.HotfixIncidentFixes != nil || sh.Accepts("hotfix_incident_fixes") || sh.MonitorRollback == nil {
		t.Fatalf("want monitor_rollback without hotfix_incident_fixes, got %s and writable %v",
			show(sh.HotfixIncidentFixes), sh.WritableSettings)
	}
	msg := f.refused(Fields{"hotfix_incident_fixes": true, "bake_minutes": 25},
		"unknown self-healing setting: hotfix_incident_fixes (settable: bake_minutes, ")
	if strings.Contains(strings.SplitN(msg, "(settable:", 2)[1], "hotfix_incident_fixes") {
		t.Fatalf("the settable list must not name hotfix_incident_fixes: %s", msg)
	}
	if sh := f.mustWrite(Fields{"bake_minutes": 25}); sh.Config.BakeMinutes != 25 {
		t.Fatalf("the other settings still save, got bake_minutes %d", sh.Config.BakeMinutes)
	}
	f.fake.SetHotfixIncidentFixesSupported(true)
	if sh := f.read(); sh.HotfixIncidentFixes == nil || *sh.HotfixIncidentFixes || !sh.Accepts("hotfix_incident_fixes") {
		t.Fatalf("upgraded, the read reports hotfix_incident_fixes false, got %s", show(sh.HotfixIncidentFixes))
	}
	f.fake.SetMonitorRollbackSupported(false)
	if sh := f.read(); sh.HotfixIncidentFixes != nil || sh.Accepts("hotfix_incident_fixes") {
		t.Fatalf("a Flightdeck from before monitor_rollback has no hotfix_incident_fixes, got %s", show(sh.HotfixIncidentFixes))
	}
}
