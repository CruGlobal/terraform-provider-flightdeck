package flightdecktest

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
)

// CIWorkflowsSupport is how the fake treats a GitHub integration's
// ci_workflows, to simulate Flightdeck versions on either side of it.
type CIWorkflowsSupport int

const (
	// CIWorkflowsSupported is a Flightdeck that has ci_workflows: reads report
	// it, and writes set it by the API's rules.
	CIWorkflowsSupported CIWorkflowsSupport = iota
	// CIWorkflowsUnknownKey is a Flightdeck older than ci_workflows that
	// already refuses unknown root keys: reads leave it out, and a write
	// naming it is refused as an unknown key, whatever its value.
	CIWorkflowsUnknownKey
	// CIWorkflowsIgnored is a Flightdeck older still, from before the GitHub
	// integration resource refused unknown root keys: reads leave it out, and
	// a write naming it succeeds without it.
	CIWorkflowsIgnored
)

// githubIntegrationWritableBeforeCIWorkflows is the settable list an older
// Flightdeck names when it refuses ci_workflows as an unknown key.
const githubIntegrationWritableBeforeCIWorkflows = "repo_full_name, secret, enabled, ci_failure_action"

// The API's bounds and words for ci_workflows (GithubIntegration's constants).
const (
	ciWorkflowsMax           = 20
	ciWorkflowNameMaxLength  = 100
	ciWorkflowControlMessage = "must not include control characters such as line breaks or tabs"
	// nulMarker is what Flightdeck stores in place of a NUL, which Postgres
	// cannot hold (U+2400 SYMBOL FOR NULL).
	nulMarker = "\u2400"
)

// SetGithubCIWorkflowsSupport simulates a Flightdeck version with or without
// ci_workflows. Stored lists are kept, so turning support back on reads them
// again, as an upgraded database would.
func (s *Server) SetGithubCIWorkflowsSupport(mode CIWorkflowsSupport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.githubIntegrations().ciWorkflows = mode
}

// SetGithubCIWorkflows simulates the settings page saving the list, out of
// band, which bumps lock_version. The list is normalised as the model does it
// (trimmed, repeats ignoring case dropped) but not checked.
func (s *Server) SetGithubCIWorkflows(id int64, names []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.githubIntegrations().byID[id]; g != nil {
		g.CIWorkflows = normalizeCIWorkflows(names)
		g.LockVersion++
	}
}

// refusesCIWorkflowsAsUnknown is an older Flightdeck's strict-root refusal of
// ci_workflows, or "" when this one takes the key (or ignores it). The strict
// root runs before any value is read, so a null or a blank is refused too.
func (st *githubIntegrationStore) refusesCIWorkflowsAsUnknown(attrs map[string]any) string {
	if _, named := attrs["ci_workflows"]; !named || st.ciWorkflows != CIWorkflowsUnknownKey {
		return ""
	}
	return "unknown key: ci_workflows (settable: " + githubIntegrationWritableBeforeCIWorkflows + ")"
}

// readsCIWorkflows reports whether this Flightdeck reads and writes the key.
func (st *githubIntegrationStore) readsCIWorkflows() bool {
	return st.ciWorkflows == CIWorkflowsSupported
}

// ciWorkflowsValue is the API's ci_workflows! for one submitted value: the
// list to store and set=true, set=false for no opinion (a null, or a string
// that Ruby's strip leaves empty), or a refusal message. In the API's order:
// the whole value must be a list; a null entry is refused first (read from the
// raw body, since Rails' params parsing would drop it); then each entry, in
// order, must be a string, not blank, without a control character once
// trimmed, and at most 100 characters once trimmed. The list is then trimmed,
// a name repeating an earlier one ignoring case is dropped, and what is left
// may hold at most 20 names.
func ciWorkflowsValue(v any) (list []string, set bool, refusal string) {
	if blankValue(v) {
		return nil, false, ""
	}
	entries, ok := v.([]any)
	if !ok {
		return nil, false, "ci_workflows must be a list of workflow names, got " + agentWorkEcho(railsDeepMunge(v))
	}
	for i, e := range entries {
		if e == nil {
			return nil, false, fmt.Sprintf("ci_workflows[%d] must be a workflow name (a string), got null", i)
		}
	}
	names := make([]string, 0, len(entries))
	for i, e := range entries {
		name, isString := e.(string)
		if !isString {
			return nil, false, fmt.Sprintf("ci_workflows[%d] must be a workflow name (a string), got %s", i, agentWorkEcho(railsDeepMunge(e)))
		}
		if railsBlank(name) {
			return nil, false, fmt.Sprintf("ci_workflows[%d] must not be blank", i)
		}
		trimmed := strings.Trim(name, rubyStripped)
		if strings.IndexFunc(trimmed, ciWorkflowControl) >= 0 {
			return nil, false, fmt.Sprintf("ci_workflows[%d] %s", i, ciWorkflowControlMessage)
		}
		if n := utf8.RuneCountInString(trimmed); n > ciWorkflowNameMaxLength {
			return nil, false, fmt.Sprintf("ci_workflows[%d] must be %d characters or fewer (got %d)", i, ciWorkflowNameMaxLength, n)
		}
		names = append(names, name)
	}
	list = normalizeCIWorkflows(names)
	if len(list) > ciWorkflowsMax {
		return nil, false, fmt.Sprintf("ci_workflows may list at most %d workflows (got %d different names)", ciWorkflowsMax, len(list))
	}
	return list, true, ""
}

// normalizeCIWorkflows is GithubIntegration.normalize_ci_workflows: each name
// has its NULs marked (the column cannot hold one) and is then trimmed with
// Ruby's strip, and a name whose folded form repeats an earlier one is
// dropped, keeping the first spelling. The marking comes before the trim, so a
// NUL at either end survives as the marker: Flightdeck stores "CI\x00" as
// "CI\u2400" even though its check, made on the trimmed name, passes it.
func normalizeCIWorkflows(names []string) []string {
	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		name = strings.Trim(strings.ReplaceAll(name, "\x00", nulMarker), rubyStripped)
		key := rubyCaseFold(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	return out
}

// railsBlank is Rails' String#blank?: empty, or nothing but Unicode white
// space (Go's unicode.IsSpace is the same set as Ruby's [[:space:]]). So a
// name of no-break spaces is blank, where Ruby's strip would keep it.
func railsBlank(s string) bool {
	return strings.TrimFunc(s, unicode.IsSpace) == ""
}

// ciWorkflowControl is GithubIntegration::CI_WORKFLOW_CONTROL_CHARACTERS: any
// C0 or C1 control character, DEL, and the Unicode line and paragraph
// separators.
func ciWorkflowControl(r rune) bool {
	return r <= 0x1f || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029
}

// rubyCaseFold is Ruby's String#downcase(:fold), full Unicode case folding,
// so "Straße" and "STRASSE" fold alike. x/text's folding agrees with Ruby's
// for every code point but the Cherokee capitals, which Unicode folds to
// themselves and x/text folds to the small letters; folding the small letters
// back to the capitals (as Unicode folds them) puts that right.
func rubyCaseFold(s string) string {
	return strings.Map(cherokeeCapital, cases.Fold().String(s))
}

// cherokeeCapital maps a Cherokee small letter to its capital, the form
// Unicode case folding gives both, and leaves every other rune alone.
func cherokeeCapital(r rune) rune {
	switch {
	case r >= 0xab70 && r <= 0xabbf:
		return r - 0xab70 + 0x13a0
	case r >= 0x13f8 && r <= 0x13fd:
		return r - 0x13f8 + 0x13f0
	}
	return r
}
