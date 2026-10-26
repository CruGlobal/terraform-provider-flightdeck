package client

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"
)

// The fake's research label follows the API's contract: it is set, stamped
// and cleared like the agent label, a blank clears it, and the rules and the
// read-only chosen time are refused with the API's codes and words. The
// provider's tests lean on this, so it is checked here directly, including
// the refusal of research_label_chosen_at, which the provider never sends.
func TestAgentWork_researchLabelContract(t *testing.T) {
	ctx := context.Background()
	fake := flightdecktest.New(t)
	c, err := New(fake.URL, fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	project := fake.AddProject("Research", "RES").ID
	other := fake.AddProject("Other", "OTH").ID
	newLabel := func(projectID int64, name string) int64 {
		t.Helper()
		l, err := c.CreateLabel(ctx, projectID, Fields{"name": name}, "label-"+name)
		if err != nil {
			t.Fatalf("creating label %s: %v", name, err)
		}
		return l.ID
	}
	agent, research, foreign := newLabel(project, "agent-ready"), newLabel(project, "research-first"), newLabel(other, "elsewhere")
	next := newLabel(project, "look-into-it")

	aw, err := c.GetAgentWork(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if aw.ResearchLabelID != nil || aw.ResearchLabelChosenAt != nil {
		t.Fatalf("an unsaved project reads no research label, got %s at %s", show(aw.ResearchLabelID), show(aw.ResearchLabelChosenAt))
	}
	version := aw.LockVersion
	write := func(fields Fields) (*AgentWork, error) {
		t.Helper()
		got, err := c.UpdateAgentWork(ctx, project, fields, version)
		if err == nil {
			version = got.LockVersion
		}
		return got, err
	}
	mustWrite := func(fields Fields) *AgentWork {
		t.Helper()
		got, err := write(fields)
		if err != nil {
			t.Fatalf("writing %v: %v", fields, err)
		}
		return got
	}
	refused := func(fields Fields, code, words string) {
		t.Helper()
		_, err := write(fields)
		apiErr, ok := AsError(err)
		if !ok || apiErr.Status != 422 || apiErr.Code != code || !strings.Contains(apiErr.Message, words) {
			t.Fatalf("writing %v: want a 422 %s saying %q, got %v", fields, code, words, err)
		}
	}

	got := mustWrite(Fields{"label_id": agent, "research_label_id": research})
	if got.ResearchLabelID == nil || *got.ResearchLabelID != research || got.ResearchLabelChosenAt == nil {
		t.Fatalf("set: research label %s chosen at %s", show(got.ResearchLabelID), show(got.ResearchLabelChosenAt))
	}

	// Switching to another research label stamps the time again. The stored
	// time is moved back first, so a stamp in the same second still shows.
	const longAgo = "2020-01-02T03:04:05Z"
	fake.SetAgentWorkOutOfBand(project, func(row *flightdecktest.AgentWorkSetting) {
		old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
		row.ResearchLabelChosenAt = &old
	})
	if aw, err = c.GetAgentWork(ctx, project); err != nil {
		t.Fatal(err)
	}
	version = aw.LockVersion
	if show(aw.ResearchLabelChosenAt) != longAgo {
		t.Fatalf("the moved-back time reads %s, want %s", show(aw.ResearchLabelChosenAt), longAgo)
	}
	got = mustWrite(Fields{"research_label_id": next})
	if got.ResearchLabelChosenAt == nil || *got.ResearchLabelChosenAt == longAgo {
		t.Fatalf("switching research labels kept the old chosen time: %s", show(got.ResearchLabelChosenAt))
	}
	mustWrite(Fields{"research_label_id": research})

	refused(Fields{"research_label_id": foreign}, CodeValidationFailed, "Research label must be a label in this project")
	refused(Fields{"research_label_id": agent}, CodeValidationFailed, "Research label must be different from the agent label")
	refused(Fields{"label_id": research}, CodeValidationFailed, "Research label must be different from the agent label")
	refused(Fields{"research_label_chosen_at": "2026-10-01T00:00:00Z"}, CodeInvalidAttribute, "research_label_chosen_at is read-only over the API")
	refused(Fields{"research_label_id": 0}, CodeInvalidAttribute, "research_label_id must be an id")

	// A blank of either kind clears it, and its chosen time with it; the
	// agent label is left alone.
	for _, blank := range []any{nil, "  "} {
		mustWrite(Fields{"research_label_id": research})
		got = mustWrite(Fields{"research_label_id": blank})
		if got.ResearchLabelID != nil || got.ResearchLabelChosenAt != nil {
			t.Fatalf("a blank %#v did not clear the research label: %s at %s", blank, show(got.ResearchLabelID), show(got.ResearchLabelChosenAt))
		}
		if got.LabelID == nil || *got.LabelID != agent {
			t.Fatalf("clearing the research label touched the agent label: %s", show(got.LabelID))
		}
	}

	// Leaving the key out leaves it alone.
	mustWrite(Fields{"research_label_id": research})
	got = mustWrite(Fields{"max_in_progress": 2})
	if got.ResearchLabelID == nil || *got.ResearchLabelID != research {
		t.Fatalf("a write without the key changed the research label: %s", show(got.ResearchLabelID))
	}
}

// show prints what a nullable field holds.
func show[T any](v *T) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprint(*v)
}

// The fake lists the label blockers the way the API does: each only when no
// ticked kind can run. With research ticked and its label chosen, no agent
// label is needed, so no_label is not listed; take the research label away
// and no_research_label is.
func TestAgentWork_researchBlockers(t *testing.T) {
	ctx := context.Background()
	fake := flightdecktest.New(t)
	c, err := New(fake.URL, fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	project := fake.AddProject("Research", "RES").ID
	l, err := c.CreateLabel(ctx, project, Fields{"name": "research-first"}, "label-research-first")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := c.CreateLabel(ctx, project, Fields{"name": "agent-ready"}, "label-agent-ready")
	if err != nil {
		t.Fatal(err)
	}
	var version int64
	codes := func(fields Fields) map[string]bool {
		t.Helper()
		aw, err := c.UpdateAgentWork(ctx, project, fields, version)
		if err != nil {
			t.Fatalf("writing %v: %v", fields, err)
		}
		version = aw.LockVersion
		out := map[string]bool{}
		for _, b := range aw.Blockers {
			out[b.Code] = true
		}
		return out
	}
	check := func(what string, got map[string]bool, listed, notListed []string) {
		t.Helper()
		for _, code := range listed {
			if !got[code] {
				t.Errorf("%s: %s is not listed: %v", what, code, got)
			}
		}
		for _, code := range notListed {
			if got[code] {
				t.Errorf("%s: %s is listed: %v", what, code, got)
			}
		}
	}

	got := codes(Fields{"kinds": []string{"research"}, "research_label_id": l.ID})
	check("research with its label, no agent label", got, nil, []string{"no_label", "no_research_label", "no_github_repo"})

	got = codes(Fields{"research_label_id": nil})
	check("research without its label", got, []string{"no_research_label"}, []string{"no_label", "no_github_repo"})

	// With implement ticked too and neither label chosen, both are missing.
	got = codes(Fields{"kinds": []string{"implement-work-item", "research"}})
	check("implement and research, no labels", got, []string{"no_label", "no_research_label"}, []string{"no_github_repo"})

	// Research runs again once its label is back, so the agent label is not a
	// blocker, though implement items will not go.
	got = codes(Fields{"research_label_id": l.ID})
	check("implement and research, research label only", got, nil, []string{"no_label", "no_research_label", "no_github_repo"})

	// The other way round: implement runs on the agent label, so the missing
	// research label stops nothing, but the missing repository does.
	got = codes(Fields{"label_id": agent.ID, "research_label_id": nil})
	check("implement and research, agent label only", got, []string{"no_github_repo"}, []string{"no_label", "no_research_label"})

	// fix-error runs on the agent label, as implement does.
	got = codes(Fields{"kinds": []string{"fix-error", "research"}})
	check("fix-error and research, agent label only", got, []string{"no_github_repo"}, []string{"no_label", "no_research_label"})

	got = codes(Fields{"label_id": nil})
	check("fix-error and research, no labels", got, []string{"no_label", "no_research_label"}, []string{"no_github_repo"})
}

// The fake's auto_merge follows the API's contract: false until set, true and
// false are values, a null or a blank is no opinion, leaving the key out
// leaves it alone, and any other value is a 422 invalid_attribute naming the
// key. A Flightdeck older than the setting reads without the key and refuses
// any write naming it, whatever its value, as an unknown key. The provider's
// tests lean on this, so it is checked here directly.
func TestAgentWork_autoMergeContract(t *testing.T) {
	ctx := context.Background()
	fake := flightdecktest.New(t)
	c, err := New(fake.URL, fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	project := fake.AddProject("Auto-merge", "AUTO").ID

	aw, err := c.GetAgentWork(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if !aw.ReportsAutoMerge() || *aw.AutoMerge {
		t.Fatalf("an unsaved project reads auto_merge false, got %s (reported: %t)", show(aw.AutoMerge), aw.ReportsAutoMerge())
	}
	version := aw.LockVersion
	write := func(fields Fields) (*AgentWork, error) {
		t.Helper()
		got, err := c.UpdateAgentWork(ctx, project, fields, version)
		if err == nil {
			version = got.LockVersion
		}
		return got, err
	}
	mustWrite := func(fields Fields) *AgentWork {
		t.Helper()
		got, err := write(fields)
		if err != nil {
			t.Fatalf("writing %v: %v", fields, err)
		}
		return got
	}
	wantAutoMerge := func(got *AgentWork, want bool, what string) {
		t.Helper()
		if got.AutoMerge == nil || *got.AutoMerge != want {
			t.Fatalf("%s: auto_merge reads %s, want %t", what, show(got.AutoMerge), want)
		}
	}

	got := mustWrite(Fields{"auto_merge": true})
	wantAutoMerge(got, true, "set true")
	if got.LockVersion != 1 {
		t.Fatalf("the first write that changes something creates the row at 1, got %d", got.LockVersion)
	}

	// A null or a blank is no opinion: nothing changes, and no version moves.
	// A blank is what Ruby's String#strip leaves empty: ASCII whitespace and
	// NUL.
	for _, blank := range []any{nil, "", "  ", "\t\n\v\f\r ", "\x00", " \x00\t"} {
		got = mustWrite(Fields{"auto_merge": blank})
		wantAutoMerge(got, true, fmt.Sprintf("a blank %#v", blank))
		if got.LockVersion != 1 {
			t.Fatalf("a blank %#v moved lock_version to %d", blank, got.LockVersion)
		}
	}

	// Leaving the key out leaves it alone.
	got = mustWrite(Fields{"max_in_progress": 2})
	wantAutoMerge(got, true, "a write without the key")

	got = mustWrite(Fields{"auto_merge": false})
	wantAutoMerge(got, false, "set false")

	// Only a real JSON boolean is a value. The refusal repeats the value the
	// way the API does (Ruby's inspect): a string quoted, an object in braces.
	for _, tc := range []struct {
		bad  any
		echo string
	}{
		{"true", `"true"`}, {"false", `"false"`}, {1, "1"}, {0, "0"}, {[]any{true}, "[true]"},
		{map[string]any{}, "{}"}, {map[string]any{"on": true}, `{"on" => true}`},
		// Ruby's strip leaves these, so they are values, not blanks.
		{"\u00a0", "\"\u00a0\""}, {"\u3000", "\"\u3000\""},
	} {
		_, err := write(Fields{"auto_merge": tc.bad})
		apiErr, ok := AsError(err)
		want := "auto_merge must be true or false, got " + tc.echo
		if !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute || apiErr.Message != want {
			t.Fatalf("writing auto_merge %#v: want a 422 invalid_attribute saying %q, got %v", tc.bad, want, err)
		}
	}
	if aw, err = c.GetAgentWork(ctx, project); err != nil {
		t.Fatal(err)
	}
	wantAutoMerge(aw, false, "after the refused writes")

	// A Flightdeck older than the setting.
	fake.SetAgentWorkAutoMergeSupported(false)
	if aw, err = c.GetAgentWork(ctx, project); err != nil {
		t.Fatal(err)
	}
	if aw.ReportsAutoMerge() {
		t.Fatalf("an older Flightdeck must read without auto_merge, got %s", show(aw.AutoMerge))
	}
	version = aw.LockVersion
	for _, value := range []any{true, false, nil} {
		_, err := write(Fields{"auto_merge": value, "max_in_progress": 3})
		apiErr, ok := AsError(err)
		if !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute ||
			!strings.HasPrefix(apiErr.Message, "unknown key: auto_merge (settable: ") {
			t.Fatalf("an older Flightdeck must refuse auto_merge %#v as an unknown key, got %v", value, err)
		}
		if strings.Contains(strings.TrimPrefix(apiErr.Message, "unknown key: auto_merge"), "auto_merge") {
			t.Fatalf("an older Flightdeck must not list auto_merge as settable: %s", apiErr.Message)
		}
	}
	got = mustWrite(Fields{"max_in_progress": 3})
	if got.MaxInProgress != 3 || got.ReportsAutoMerge() {
		t.Fatalf("an older Flightdeck takes a write without auto_merge: max_in_progress %d, auto_merge %s",
			got.MaxInProgress, show(got.AutoMerge))
	}
}

// The fake repeats a refused value the way the API does, value.inspect cut to
// 100 characters, after Rails' params parsing has dropped any null inside a
// list (only kinds is checked for one first, from the raw body), and reads a
// blank as Ruby's String#strip does. A no-break
// space is not a blank, so it does not clear a label: it is refused as not
// an id.
func TestAgentWork_refusalsEchoTheValueAsTheAPIDoes(t *testing.T) {
	ctx := context.Background()
	fake := flightdecktest.New(t)
	c, err := New(fake.URL, fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	project := fake.AddProject("Echo", "ECHO").ID
	long := strings.Repeat("x", 200)
	for _, tc := range []struct {
		fields Fields
		want   string
	}{
		{Fields{"max_in_progress": "2"}, `max_in_progress must be a whole number, got "2"`},
		{Fields{"daily_budget_usd": map[string]any{}}, "daily_budget_usd must be a number, got {}"},
		{Fields{"research_label_id": "\u00a0"}, "research_label_id must be an id (a positive whole number) or null, got \"\u00a0\""},
		// Rails' params parsing drops a null inside a list, at any depth, before
		// the applier echoes it.
		{Fields{"base_ref": []any{"main", nil}}, `base_ref must be a string, got ["main"]`},
		{Fields{"base_ref": []any{nil}}, "base_ref must be a string, got []"},
		{Fields{"auto_merge": []any{true, nil}}, "auto_merge must be true or false, got [true]"},
		{Fields{"auto_merge": map[string]any{"on": []any{nil, []any{true, nil}}}}, `auto_merge must be true or false, got {"on" => [[true]]}`},
		{Fields{"runbook": false}, "runbook must be a string, got false"},
		{Fields{"kinds": "research"}, `kinds must be a list of kind names, got "research"`},
		{Fields{"kinds": []any{"research", nil}}, `kinds must be a list of kind names, got ["research",null]`},
		{Fields{"kinds": []any{"fix-ci"}}, `kinds has an unknown kind: "fix-ci" (valid: implement-work-item, fix-error, research)`},
		{Fields{"kinds": []any{"fix-ci", "fix-ci"}}, `kinds has unknown kinds: "fix-ci" (valid: implement-work-item, fix-error, research)`},
		{Fields{"enabled": "say \"hi\" #{x}\x01"}, `enabled must be true or false, got "say \"hi\" \#{x}\u0001"`},
		{Fields{"enabled": long}, "enabled must be true or false, got \"" + strings.Repeat("x", 96) + "..."},
	} {
		_, err := c.UpdateAgentWork(ctx, project, tc.fields, 0)
		apiErr, ok := AsError(err)
		if !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute || apiErr.Message != tc.want {
			t.Errorf("writing %v:\n want a 422 invalid_attribute saying %s\n  got %v", tc.fields, tc.want, err)
		}
	}
	if row := fake.AgentWorkOf(project); row != nil {
		t.Fatalf("a refused write must save nothing, got %+v", row)
	}
}

// The fake's send_ci_failures follows the API's contract, the same as
// auto_merge's: false until set, true and false are values, a null or a blank
// is no opinion, leaving the key out leaves it alone, and any other value is a
// 422 invalid_attribute naming the key. A Flightdeck older than the setting
// reads without the key and refuses any write naming it, whatever its value,
// as an unknown key. The provider's tests lean on this, so it is checked here
// directly, including a Flightdeck that has auto_merge but not
// send_ci_failures, and one older than both.
func TestAgentWork_sendCIFailuresContract(t *testing.T) {
	ctx := context.Background()
	fake := flightdecktest.New(t)
	c, err := New(fake.URL, fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	project := fake.AddProject("CI failures", "CIF").ID

	aw, err := c.GetAgentWork(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if !aw.ReportsSendCIFailures() || *aw.SendCIFailures {
		t.Fatalf("an unsaved project reads send_ci_failures false, got %s (reported: %t)", show(aw.SendCIFailures), aw.ReportsSendCIFailures())
	}
	version := aw.LockVersion
	write := func(fields Fields) (*AgentWork, error) {
		t.Helper()
		got, err := c.UpdateAgentWork(ctx, project, fields, version)
		if err == nil {
			version = got.LockVersion
		}
		return got, err
	}
	mustWrite := func(fields Fields) *AgentWork {
		t.Helper()
		got, err := write(fields)
		if err != nil {
			t.Fatalf("writing %v: %v", fields, err)
		}
		return got
	}
	want := func(got *AgentWork, want bool, what string) {
		t.Helper()
		if got.SendCIFailures == nil || *got.SendCIFailures != want {
			t.Fatalf("%s: send_ci_failures reads %s, want %t", what, show(got.SendCIFailures), want)
		}
	}

	got := mustWrite(Fields{"send_ci_failures": true})
	want(got, true, "set true")
	if got.LockVersion != 1 {
		t.Fatalf("the first write that changes something creates the row at 1, got %d", got.LockVersion)
	}
	if got.AutoMerge == nil || *got.AutoMerge {
		t.Fatalf("setting send_ci_failures must leave auto_merge alone, got %s", show(got.AutoMerge))
	}

	// A null or a blank is no opinion: nothing changes, and no version moves.
	for _, blank := range []any{nil, "", "  ", "\t\n\v\f\r ", "\x00", " \x00\t"} {
		got = mustWrite(Fields{"send_ci_failures": blank})
		want(got, true, fmt.Sprintf("a blank %#v", blank))
		if got.LockVersion != 1 {
			t.Fatalf("a blank %#v moved lock_version to %d", blank, got.LockVersion)
		}
	}

	// Leaving the key out leaves it alone, and so does setting auto_merge.
	got = mustWrite(Fields{"max_in_progress": 2})
	want(got, true, "a write without the key")
	got = mustWrite(Fields{"auto_merge": true})
	want(got, true, "a write of auto_merge only")

	got = mustWrite(Fields{"send_ci_failures": false})
	want(got, false, "set false")
	if got.AutoMerge == nil || !*got.AutoMerge {
		t.Fatalf("setting send_ci_failures must leave auto_merge alone, got %s", show(got.AutoMerge))
	}

	// Only a real JSON boolean is a value, refused in the API's words.
	for _, tc := range []struct {
		bad  any
		echo string
	}{
		{"true", `"true"`}, {"false", `"false"`}, {1, "1"}, {0, "0"}, {[]any{}, "[]"}, {[]any{true}, "[true]"},
		{map[string]any{}, "{}"}, {map[string]any{"on": true}, `{"on" => true}`},
		{"yes", `"yes"`}, {" ", "\" \""},
	} {
		_, err := write(Fields{"send_ci_failures": tc.bad, "max_in_progress": 3})
		apiErr, ok := AsError(err)
		msg := "send_ci_failures must be true or false, got " + tc.echo
		if !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute || apiErr.Message != msg {
			t.Fatalf("writing send_ci_failures %#v: want a 422 invalid_attribute saying %q, got %v", tc.bad, msg, err)
		}
	}
	if aw, err = c.GetAgentWork(ctx, project); err != nil {
		t.Fatal(err)
	}
	want(aw, false, "after the refused writes")
	if aw.MaxInProgress != 2 {
		t.Fatalf("a refused send_ci_failures must leave the rest of its write unsaved, max_in_progress is %d", aw.MaxInProgress)
	}

	// A Flightdeck that has auto_merge but not send_ci_failures.
	fake.SetAgentWorkSendCIFailuresSupported(false)
	if aw, err = c.GetAgentWork(ctx, project); err != nil {
		t.Fatal(err)
	}
	if aw.ReportsSendCIFailures() || !aw.ReportsAutoMerge() {
		t.Fatalf("this Flightdeck reads auto_merge but not send_ci_failures, got send_ci_failures %s, auto_merge %s",
			show(aw.SendCIFailures), show(aw.AutoMerge))
	}
	version = aw.LockVersion
	for _, value := range []any{true, false, nil} {
		_, err := write(Fields{"send_ci_failures": value, "auto_merge": false})
		apiErr, ok := AsError(err)
		if !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute ||
			!strings.HasPrefix(apiErr.Message, "unknown key: send_ci_failures (settable: enabled, kinds, auto_merge, label_id, ") {
			t.Fatalf("this Flightdeck must refuse send_ci_failures %#v as an unknown key and list auto_merge as settable, got %v", value, err)
		}
		if strings.Contains(strings.TrimPrefix(apiErr.Message, "unknown key: send_ci_failures"), "send_ci_failures") {
			t.Fatalf("this Flightdeck must not list send_ci_failures as settable: %s", apiErr.Message)
		}
	}
	got = mustWrite(Fields{"auto_merge": false})
	if got.AutoMerge == nil || *got.AutoMerge || got.ReportsSendCIFailures() {
		t.Fatalf("this Flightdeck takes a write without send_ci_failures: auto_merge %s, send_ci_failures %s",
			show(got.AutoMerge), show(got.SendCIFailures))
	}

	// A Flightdeck older than both names both, sorted, as the API does.
	fake.SetAgentWorkAutoMergeSupported(false)
	_, err = write(Fields{"send_ci_failures": true, "auto_merge": true})
	apiErr, ok := AsError(err)
	if !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute ||
		!strings.HasPrefix(apiErr.Message, "unknown keys: auto_merge, send_ci_failures (settable: enabled, kinds, label_id, ") {
		t.Fatalf("a Flightdeck older than both must refuse both as unknown keys, got %v", err)
	}
	if row := fake.AgentWorkOf(project); row == nil || row.SendCIFailures || row.AutoMerge {
		t.Fatalf("the older Flightdecks' refusals must save nothing, got %+v", row)
	}
}

// The fake's wait_for_checks follows the API's contract. Unlike auto_merge
// and send_ci_failures it is true until set: an unsaved project reads true,
// and so does the row the first write creates. true and false are values, a
// null or a blank is no opinion, leaving the key out leaves it alone, and any
// other value is a 422 invalid_attribute naming the key. A Flightdeck older
// than the setting reads without the key and refuses any write naming it,
// whatever its value, as an unknown key. The provider's tests lean on this, so
// it is checked here directly, including a Flightdeck that has the other two
// newer settings but not this one, and one older than all three.
func TestAgentWork_waitForChecksContract(t *testing.T) {
	ctx := context.Background()
	fake := flightdecktest.New(t)
	c, err := New(fake.URL, fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	project := fake.AddProject("Wait for checks", "WFC").ID

	aw, err := c.GetAgentWork(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if !aw.ReportsWaitForChecks() || !*aw.WaitForChecks {
		t.Fatalf("an unsaved project reads wait_for_checks true, got %s (reported: %t)", show(aw.WaitForChecks), aw.ReportsWaitForChecks())
	}
	version := aw.LockVersion
	write := func(fields Fields) (*AgentWork, error) {
		t.Helper()
		got, err := c.UpdateAgentWork(ctx, project, fields, version)
		if err == nil {
			version = got.LockVersion
		}
		return got, err
	}
	mustWrite := func(fields Fields) *AgentWork {
		t.Helper()
		got, err := write(fields)
		if err != nil {
			t.Fatalf("writing %v: %v", fields, err)
		}
		return got
	}
	want := func(got *AgentWork, want bool, what string) {
		t.Helper()
		if got.WaitForChecks == nil || *got.WaitForChecks != want {
			t.Fatalf("%s: wait_for_checks reads %s, want %t", what, show(got.WaitForChecks), want)
		}
	}

	// Writing the default changes nothing, so no row is created.
	got := mustWrite(Fields{"wait_for_checks": true})
	want(got, true, "set to the default")
	if got.LockVersion != 0 || fake.AgentWorkOf(project) != nil {
		t.Fatalf("writing the default must create nothing, got lock_version %d and row %+v", got.LockVersion, fake.AgentWorkOf(project))
	}
	// The first write of something else creates the row with the default.
	got = mustWrite(Fields{"max_in_progress": 2})
	want(got, true, "the row the first write creates")
	if got.LockVersion != 1 {
		t.Fatalf("the first write that changes something creates the row at 1, got %d", got.LockVersion)
	}

	got = mustWrite(Fields{"wait_for_checks": false})
	want(got, false, "set false")
	if got.LockVersion != 2 {
		t.Fatalf("turning wait_for_checks off must move lock_version to 2, got %d", got.LockVersion)
	}
	if got.AutoMerge == nil || *got.AutoMerge || got.SendCIFailures == nil || *got.SendCIFailures {
		t.Fatalf("setting wait_for_checks must leave the other switches alone, got auto_merge %s, send_ci_failures %s",
			show(got.AutoMerge), show(got.SendCIFailures))
	}

	// A null or a blank is no opinion: nothing changes, and no version moves.
	for _, blank := range []any{nil, "", "  ", "\t\n\v\f\r ", "\x00", " \x00\t"} {
		got = mustWrite(Fields{"wait_for_checks": blank})
		want(got, false, fmt.Sprintf("a blank %#v", blank))
		if got.LockVersion != 2 {
			t.Fatalf("a blank %#v moved lock_version to %d", blank, got.LockVersion)
		}
	}

	// Leaving the key out leaves it alone, and so do the other switches.
	got = mustWrite(Fields{"max_in_progress": 3})
	want(got, false, "a write without the key")
	got = mustWrite(Fields{"auto_merge": true, "send_ci_failures": true})
	want(got, false, "a write of the other switches only")

	got = mustWrite(Fields{"wait_for_checks": true})
	want(got, true, "set back to true")
	if got.AutoMerge == nil || !*got.AutoMerge || got.SendCIFailures == nil || !*got.SendCIFailures {
		t.Fatalf("setting wait_for_checks must leave the other switches alone, got auto_merge %s, send_ci_failures %s",
			show(got.AutoMerge), show(got.SendCIFailures))
	}

	// Only a real JSON boolean is a value, refused in the API's words.
	for _, tc := range []struct {
		bad  any
		echo string
	}{
		{"true", `"true"`}, {"false", `"false"`}, {1, "1"}, {0, "0"}, {[]any{}, "[]"}, {[]any{false}, "[false]"},
		{map[string]any{}, "{}"}, {map[string]any{"on": false}, `{"on" => false}`},
		{"no", `"no"`}, {"\u00a0", "\"\u00a0\""},
	} {
		_, err := write(Fields{"wait_for_checks": tc.bad, "max_in_progress": 4})
		apiErr, ok := AsError(err)
		msg := "wait_for_checks must be true or false, got " + tc.echo
		if !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute || apiErr.Message != msg {
			t.Fatalf("writing wait_for_checks %#v: want a 422 invalid_attribute saying %q, got %v", tc.bad, msg, err)
		}
	}
	if aw, err = c.GetAgentWork(ctx, project); err != nil {
		t.Fatal(err)
	}
	want(aw, true, "after the refused writes")
	if aw.MaxInProgress != 3 {
		t.Fatalf("a refused wait_for_checks must leave the rest of its write unsaved, max_in_progress is %d", aw.MaxInProgress)
	}

	// A Flightdeck that has auto_merge and send_ci_failures but not
	// wait_for_checks.
	fake.SetAgentWorkWaitForChecksSupported(false)
	if aw, err = c.GetAgentWork(ctx, project); err != nil {
		t.Fatal(err)
	}
	if aw.ReportsWaitForChecks() || !aw.ReportsAutoMerge() || !aw.ReportsSendCIFailures() {
		t.Fatalf("this Flightdeck reads auto_merge and send_ci_failures but not wait_for_checks, got wait_for_checks %s, "+
			"auto_merge %s, send_ci_failures %s", show(aw.WaitForChecks), show(aw.AutoMerge), show(aw.SendCIFailures))
	}
	version = aw.LockVersion
	for _, value := range []any{true, false, nil, ""} {
		_, err := write(Fields{"wait_for_checks": value, "send_ci_failures": false})
		apiErr, ok := AsError(err)
		if !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute ||
			!strings.HasPrefix(apiErr.Message, "unknown key: wait_for_checks (settable: enabled, kinds, auto_merge, send_ci_failures, label_id, ") {
			t.Fatalf("this Flightdeck must refuse wait_for_checks %#v as an unknown key and list the other switches as settable, got %v", value, err)
		}
		if strings.Contains(strings.TrimPrefix(apiErr.Message, "unknown key: wait_for_checks"), "wait_for_checks") {
			t.Fatalf("this Flightdeck must not list wait_for_checks as settable: %s", apiErr.Message)
		}
	}
	got = mustWrite(Fields{"send_ci_failures": false})
	if got.SendCIFailures == nil || *got.SendCIFailures || got.ReportsWaitForChecks() {
		t.Fatalf("this Flightdeck takes a write without wait_for_checks: send_ci_failures %s, wait_for_checks %s",
			show(got.SendCIFailures), show(got.WaitForChecks))
	}

	// A Flightdeck older than all three names all three, sorted, as the API
	// does.
	fake.SetAgentWorkAutoMergeSupported(false)
	_, err = write(Fields{"wait_for_checks": false, "send_ci_failures": true, "auto_merge": false})
	apiErr, ok := AsError(err)
	if !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute ||
		!strings.HasPrefix(apiErr.Message, "unknown keys: auto_merge, send_ci_failures, wait_for_checks (settable: enabled, kinds, label_id, ") {
		t.Fatalf("a Flightdeck older than all three must refuse them as unknown keys, got %v", err)
	}
	if row := fake.AgentWorkOf(project); row == nil || !row.WaitForChecks || row.SendCIFailures || !row.AutoMerge {
		t.Fatalf("the older Flightdecks' refusals must save nothing, got %+v", row)
	}
}

// The fake's send_errors follows the API's contract, the same rule as
// send_ci_failures: it is false until set, true and false are values, a null
// or a blank is no opinion, leaving the key out leaves it alone, and any other
// value is a 422 invalid_attribute naming the key. The read also carries
// send_errors_since, which the fake stamps when send_errors turns on, clears
// when it turns off, and refuses on a write as read-only, as the API does. A
// Flightdeck older than the setting reads without either key and refuses any
// write naming them, whatever the value, as unknown keys. The provider's tests
// lean on this, so it is checked here directly, including a Flightdeck that
// has the other three newer settings but not this one, and one older than all
// four.
func TestAgentWork_sendErrorsContract(t *testing.T) {
	ctx := context.Background()
	fake := flightdecktest.New(t)
	c, err := New(fake.URL, fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	project := fake.AddProject("Send errors", "SER").ID
	// since reads send_errors_since as the API sends it (AgentWork does not
	// carry it), and whether the read has the key at all.
	since := func() (any, bool) {
		t.Helper()
		got, err := GetResource[map[string]any](ctx, c, agentWorkPath(project), agentWorkRoot)
		if err != nil {
			t.Fatal(err)
		}
		v, ok := got["send_errors_since"]
		return v, ok
	}
	stamped := func(what string) string {
		t.Helper()
		v, ok := since()
		str, isStr := v.(string)
		if !ok || !isStr {
			t.Fatalf("%s: send_errors_since must be a time, got %#v (present: %t)", what, v, ok)
		}
		if _, err := time.Parse(time.RFC3339, str); err != nil {
			t.Fatalf("%s: send_errors_since %q is not RFC 3339: %v", what, str, err)
		}
		return str
	}
	cleared := func(what string) {
		t.Helper()
		if v, ok := since(); !ok || v != nil {
			t.Fatalf("%s: send_errors_since must read null, got %#v (present: %t)", what, v, ok)
		}
	}

	aw, err := c.GetAgentWork(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if !aw.ReportsSendErrors() || *aw.SendErrors {
		t.Fatalf("an unsaved project reads send_errors false, got %s (reported: %t)", show(aw.SendErrors), aw.ReportsSendErrors())
	}
	cleared("an unsaved project")
	version := aw.LockVersion
	write := func(fields Fields) (*AgentWork, error) {
		t.Helper()
		got, err := c.UpdateAgentWork(ctx, project, fields, version)
		if err == nil {
			version = got.LockVersion
		}
		return got, err
	}
	mustWrite := func(fields Fields) *AgentWork {
		t.Helper()
		got, err := write(fields)
		if err != nil {
			t.Fatalf("writing %v: %v", fields, err)
		}
		return got
	}
	want := func(got *AgentWork, want bool, what string) {
		t.Helper()
		if got.SendErrors == nil || *got.SendErrors != want {
			t.Fatalf("%s: send_errors reads %s, want %t", what, show(got.SendErrors), want)
		}
	}
	othersAre := func(got *AgentWork, autoMerge, sendCIFailures, waitForChecks bool) {
		t.Helper()
		if got.AutoMerge == nil || *got.AutoMerge != autoMerge || got.SendCIFailures == nil || *got.SendCIFailures != sendCIFailures ||
			got.WaitForChecks == nil || *got.WaitForChecks != waitForChecks {
			t.Fatalf("setting send_errors must leave the other switches alone, got auto_merge %s, send_ci_failures %s, wait_for_checks %s",
				show(got.AutoMerge), show(got.SendCIFailures), show(got.WaitForChecks))
		}
	}

	got := mustWrite(Fields{"send_errors": true})
	want(got, true, "set true")
	if got.LockVersion != 1 {
		t.Fatalf("the first write that changes something creates the row at 1, got %d", got.LockVersion)
	}
	othersAre(got, false, false, true)
	first := stamped("turned on")

	// A null or a blank is no opinion: nothing changes, no version moves, and
	// the time it was turned on stays.
	for _, blank := range []any{nil, "", "  ", "\t\n\v\f\r ", "\x00", " \x00\t"} {
		got = mustWrite(Fields{"send_errors": blank})
		want(got, true, fmt.Sprintf("a blank %#v", blank))
		if got.LockVersion != 1 {
			t.Fatalf("a blank %#v moved lock_version to %d", blank, got.LockVersion)
		}
	}
	if now := stamped("after the blanks"); now != first {
		t.Fatalf("a blank moved send_errors_since from %s to %s", first, now)
	}

	// Leaving the key out leaves it alone, and so do the other switches.
	got = mustWrite(Fields{"max_in_progress": 2})
	want(got, true, "a write without the key")
	got = mustWrite(Fields{"auto_merge": true, "send_ci_failures": true, "wait_for_checks": false})
	want(got, true, "a write of the other switches only")

	// Setting it to what it already is changes nothing, so the time stays.
	// Move the stored time back first, so a fresh stamp would show.
	longAgo := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	fake.SetAgentWorkOutOfBand(project, func(row *flightdecktest.AgentWorkSetting) { row.SendErrorsSince = &longAgo })
	if aw, err = c.GetAgentWork(ctx, project); err != nil {
		t.Fatal(err)
	}
	version = aw.LockVersion
	got = mustWrite(Fields{"send_errors": true})
	if got.LockVersion != version {
		t.Fatalf("writing send_errors true when it is already true moved lock_version to %d", got.LockVersion)
	}
	if now := stamped("set to what it already is"); now != longAgo.Format(time.RFC3339) {
		t.Fatalf("a write that changed nothing moved send_errors_since to %s", now)
	}

	// send_errors_since is the server's: refused as read-only, with its reason,
	// and nothing else in the write is saved.
	for _, value := range []any{"2026-10-01T00:00:00Z", nil} {
		_, err := write(Fields{"send_errors_since": value, "send_errors": false, "max_in_progress": 3})
		apiErr, ok := AsError(err)
		if !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute ||
			!strings.HasPrefix(apiErr.Message, "send_errors_since is read-only over the API") ||
			!strings.HasSuffix(apiErr.Message, "Flightdeck sets it when send_errors changes: to now when it is turned on, and to null when it is turned off") {
			t.Fatalf("writing send_errors_since %#v: want a 422 invalid_attribute saying it is read-only, got %v", value, err)
		}
	}
	if aw, err = c.GetAgentWork(ctx, project); err != nil {
		t.Fatal(err)
	}
	want(aw, true, "after the refused send_errors_since")
	if aw.MaxInProgress != 2 {
		t.Fatalf("a refused send_errors_since must leave the rest of its write unsaved, max_in_progress is %d", aw.MaxInProgress)
	}

	got = mustWrite(Fields{"send_errors": false})
	want(got, false, "set false")
	othersAre(got, true, true, false)
	cleared("turned off")

	// Turned on again, it is stamped afresh.
	got = mustWrite(Fields{"send_errors": true})
	want(got, true, "set true again")
	if again := stamped("turned on again"); again == longAgo.Format(time.RFC3339) {
		t.Fatalf("turning send_errors on again kept the old send_errors_since %s", again)
	}
	got = mustWrite(Fields{"send_errors": false})
	want(got, false, "set false again")

	// Only a real JSON boolean is a value, refused in the API's words.
	for _, tc := range []struct {
		bad  any
		echo string
	}{
		{"true", `"true"`}, {"false", `"false"`}, {1, "1"}, {0, "0"}, {[]any{}, "[]"}, {[]any{true}, "[true]"},
		{map[string]any{}, "{}"}, {map[string]any{"on": true}, `{"on" => true}`},
		{"yes", `"yes"`}, {"\u00a0", "\"\u00a0\""},
	} {
		_, err := write(Fields{"send_errors": tc.bad, "max_in_progress": 4})
		apiErr, ok := AsError(err)
		msg := "send_errors must be true or false, got " + tc.echo
		if !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute || apiErr.Message != msg {
			t.Fatalf("writing send_errors %#v: want a 422 invalid_attribute saying %q, got %v", tc.bad, msg, err)
		}
	}
	if aw, err = c.GetAgentWork(ctx, project); err != nil {
		t.Fatal(err)
	}
	want(aw, false, "after the refused writes")
	if aw.MaxInProgress != 2 {
		t.Fatalf("a refused send_errors must leave the rest of its write unsaved, max_in_progress is %d", aw.MaxInProgress)
	}

	// A Flightdeck that has the other three newer settings but not
	// send_errors: neither key is read, and both are unknown on a write.
	fake.SetAgentWorkSendErrorsSupported(false)
	if aw, err = c.GetAgentWork(ctx, project); err != nil {
		t.Fatal(err)
	}
	if aw.ReportsSendErrors() || !aw.ReportsAutoMerge() || !aw.ReportsSendCIFailures() || !aw.ReportsWaitForChecks() {
		t.Fatalf("this Flightdeck reads the other three newer settings but not send_errors, got send_errors %s, auto_merge %s, "+
			"send_ci_failures %s, wait_for_checks %s", show(aw.SendErrors), show(aw.AutoMerge), show(aw.SendCIFailures), show(aw.WaitForChecks))
	}
	if v, ok := since(); ok {
		t.Fatalf("this Flightdeck must not read send_errors_since, got %#v", v)
	}
	version = aw.LockVersion
	for _, value := range []any{true, false, nil, ""} {
		_, err := write(Fields{"send_errors": value, "wait_for_checks": true})
		apiErr, ok := AsError(err)
		if !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute ||
			!strings.HasPrefix(apiErr.Message, "unknown key: send_errors (settable: enabled, kinds, auto_merge, send_ci_failures, wait_for_checks, label_id, ") {
			t.Fatalf("this Flightdeck must refuse send_errors %#v as an unknown key and list the other switches as settable, got %v", value, err)
		}
		if strings.Contains(strings.TrimPrefix(apiErr.Message, "unknown key: send_errors"), "send_errors") {
			t.Fatalf("this Flightdeck must not list send_errors as settable: %s", apiErr.Message)
		}
	}
	_, err = write(Fields{"send_errors_since": "2026-10-01T00:00:00Z"})
	if apiErr, ok := AsError(err); !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute ||
		!strings.HasPrefix(apiErr.Message, "unknown key: send_errors_since (settable: ") {
		t.Fatalf("this Flightdeck must refuse send_errors_since as an unknown key, not a read-only one, got %v", err)
	}
	got = mustWrite(Fields{"wait_for_checks": true})
	if got.WaitForChecks == nil || !*got.WaitForChecks || got.ReportsSendErrors() {
		t.Fatalf("this Flightdeck takes a write without send_errors: wait_for_checks %s, send_errors %s",
			show(got.WaitForChecks), show(got.SendErrors))
	}

	// A Flightdeck older than all four names all four, sorted, as the API
	// does.
	fake.SetAgentWorkAutoMergeSupported(false)
	_, err = write(Fields{"send_errors": true, "wait_for_checks": false, "send_ci_failures": false, "auto_merge": false})
	apiErr, ok := AsError(err)
	if !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute ||
		!strings.HasPrefix(apiErr.Message, "unknown keys: auto_merge, send_ci_failures, send_errors, wait_for_checks (settable: enabled, kinds, label_id, ") {
		t.Fatalf("a Flightdeck older than all four must refuse them as unknown keys, got %v", err)
	}
	if row := fake.AgentWorkOf(project); row == nil || row.SendErrors || row.SendErrorsSince != nil || !row.WaitForChecks ||
		!row.SendCIFailures || !row.AutoMerge {
		t.Fatalf("the older Flightdecks' refusals must save nothing, got %+v", row)
	}
}

// The fake's models follows the API's contract: an object from kind of work to
// model, {} until set. A write REPLACES the stored map: a kind left out, or
// sent with a null or blank model, goes back to the default, {} clears them
// all, and a null or blank for the whole key is no opinion. An unknown kind,
// a model the release doesn't offer, and a value that isn't an object are
// each a 422 invalid_attribute in the API's words, and nothing is saved. A
// model taken off the list after it was stored is accepted back for the kind
// it is stored for, and for no other. A Flightdeck older than the setting
// reads without the key and refuses any write naming it, whatever its value,
// as an unknown key. The provider's tests lean on this, so it is checked here
// directly.
func TestAgentWork_modelsContract(t *testing.T) {
	ctx := context.Background()
	fake := flightdecktest.New(t)
	c, err := New(fake.URL, fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	project := fake.AddProject("Models", "MOD").ID
	aw, err := c.GetAgentWork(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if !aw.ReportsModels() || len(aw.Models) != 0 {
		t.Fatalf("an unsaved project reads models as {}, got %v (reported: %t)", aw.Models, aw.ReportsModels())
	}
	version := aw.LockVersion
	write := func(fields Fields) (*AgentWork, error) {
		t.Helper()
		got, err := c.UpdateAgentWork(ctx, project, fields, version)
		if err == nil {
			version = got.LockVersion
		}
		return got, err
	}
	mustWrite := func(fields Fields) *AgentWork {
		t.Helper()
		got, err := write(fields)
		if err != nil {
			t.Fatalf("writing %v: %v", fields, err)
		}
		return got
	}
	want := func(got *AgentWork, want map[string]string, what string) {
		t.Helper()
		if !got.ReportsModels() || fmt.Sprint(got.Models) != fmt.Sprint(want) {
			t.Fatalf("%s: models reads %v (reported: %t), want %v", what, got.Models, got.ReportsModels(), want)
		}
	}
	refused := func(fields Fields, msg string) {
		t.Helper()
		_, err := write(fields)
		apiErr, ok := AsError(err)
		if !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute || apiErr.Message != msg {
			t.Fatalf("writing %v:\n want a 422 invalid_attribute saying %s\n  got %v", fields, msg, err)
		}
	}
	both := map[string]string{"implement-work-item": "opus-5-5", "research": "sonnet-5-5"}

	got := mustWrite(Fields{"models": map[string]string{"implement-work-item": "opus-5-5"}})
	want(got, map[string]string{"implement-work-item": "opus-5-5"}, "one kind set")
	if got.LockVersion != 1 {
		t.Fatalf("the first write that changes something creates the row at 1, got %d", got.LockVersion)
	}
	got = mustWrite(Fields{"models": both})
	want(got, both, "a second kind added")

	// The same map again changes nothing, so no version moves.
	got = mustWrite(Fields{"models": map[string]string{"research": "sonnet-5-5", "implement-work-item": "opus-5-5"}})
	if got.LockVersion != 2 {
		t.Fatalf("writing the map it already holds moved lock_version to %d", got.LockVersion)
	}

	// Leaving the key out, or a null or blank for the whole key, leaves the map
	// alone, and so does a write of the other switches.
	for _, fields := range []Fields{{"max_in_progress": 2}, {"models": nil}, {"models": ""}, {"models": "  "},
		{"auto_merge": true, "send_errors": true}} {
		got = mustWrite(fields)
		want(got, both, fmt.Sprintf("a write of %v", fields))
	}

	// The map replaces the stored one: a kind left out goes back to the
	// default.
	got = mustWrite(Fields{"models": map[string]string{"research": "opus-5-5"}})
	want(got, map[string]string{"research": "opus-5-5"}, "replaced")

	// A null or blank model for a kind reads as leaving it out.
	for _, blank := range []any{nil, "", "   "} {
		mustWrite(Fields{"models": both})
		got = mustWrite(Fields{"models": map[string]any{"implement-work-item": "opus-5-5", "research": blank}})
		want(got, map[string]string{"implement-work-item": "opus-5-5"}, fmt.Sprintf("a blank %#v for research", blank))
	}

	// {} clears them all.
	got = mustWrite(Fields{"models": map[string]string{}})
	want(got, map[string]string{}, "cleared")

	// What the API refuses, in its words, saving nothing.
	mustWrite(Fields{"models": both, "max_in_progress": 1})
	for name, tc := range map[string]struct {
		value any
		msg   string
	}{
		"an unknown kind": {map[string]any{"fix-ci": "opus-5-5"},
			`models has an unknown kind: "fix-ci" (valid: implement-work-item, fix-error, research)`},
		"two unknown kinds": {map[string]any{"fix-ci": "opus-5-5", "deploy": "opus-5-5", "research": "opus-5-5"},
			`models has unknown kinds: "deploy", "fix-ci" (valid: implement-work-item, fix-error, research)`},
		"a model not offered": {map[string]any{"research": "opus-4-1"},
			`models.research must be one of: sonnet-5-5, opus-5-5, got "opus-4-1"`},
		"a model's label":   {map[string]any{"research": "Opus 5.5"}, `models.research must be one of: sonnet-5-5, opus-5-5, got "Opus 5.5"`},
		"a number":          {map[string]any{"research": 5}, "models.research must be one of: sonnet-5-5, opus-5-5, got 5"},
		"false":             {map[string]any{"research": false}, "models.research must be one of: sonnet-5-5, opus-5-5, got false"},
		"a list of models":  {map[string]any{"research": []any{"opus-5-5"}}, `models.research must be one of: sonnet-5-5, opus-5-5, got ["opus-5-5"]`},
		"an empty object":   {map[string]any{"research": map[string]any{}}, "models.research must be one of: sonnet-5-5, opus-5-5, got {}"},
		"a list":            {[]any{"opus-5-5"}, `models must be an object of kinds of work to model names, got ["opus-5-5"]`},
		"a string":          {"opus-5-5", `models must be an object of kinds of work to model names, got "opus-5-5"`},
		"true":              {true, "models must be an object of kinds of work to model names, got true"},
		"a whole number":    {7, "models must be an object of kinds of work to model names, got 7"},
		"a padded model":    {map[string]any{"research": " opus-5-5"}, `models.research must be one of: sonnet-5-5, opus-5-5, got " opus-5-5"`},
		"another kind's ok": {map[string]any{"implement-work-item": "sonnet-5-5", "fix-error": "gpt"}, `models.fix-error must be one of: sonnet-5-5, opus-5-5, got "gpt"`},
	} {
		refused(Fields{"models": tc.value, "max_in_progress": 4}, tc.msg)
		if row := fake.AgentWorkOf(project); row == nil || fmt.Sprint(row.Models) != fmt.Sprint(both) || row.MaxInProgress != 1 {
			t.Fatalf("%s: a refused write must save nothing, got %+v", name, row)
		}
	}

	// The list may grow: a model the release adds is taken.
	fake.SetAgentWorkModelsOffered("sonnet-5-5", "opus-5-5", "sonnet-6-0")
	got = mustWrite(Fields{"models": map[string]string{"fix-error": "sonnet-6-0", "implement-work-item": "opus-5-5", "research": "sonnet-5-5"}})
	want(got, map[string]string{"fix-error": "sonnet-6-0", "implement-work-item": "opus-5-5", "research": "sonnet-5-5"}, "a new model")
	mustWrite(Fields{"models": both})

	// A model taken off the list stays stored, and the read PATCHes back
	// unchanged, beside a change to another kind too. A changed one must be on
	// the list, and the refusal names only what is.
	fake.SetAgentWorkModelsOffered("sonnet-5-5")
	if aw, err = c.GetAgentWork(ctx, project); err != nil {
		t.Fatal(err)
	}
	want(aw, both, "after opus-5-5 came off the list")
	version = aw.LockVersion
	got = mustWrite(Fields{"models": aw.Models})
	if got.LockVersion != aw.LockVersion {
		t.Fatalf("the read PATCHed back moved lock_version from %d to %d", aw.LockVersion, got.LockVersion)
	}
	got = mustWrite(Fields{"models": map[string]string{"implement-work-item": "opus-5-5", "fix-error": "sonnet-5-5"}})
	want(got, map[string]string{"implement-work-item": "opus-5-5", "fix-error": "sonnet-5-5"}, "kept beside a change to another kind")
	refused(Fields{"models": map[string]string{"research": "opus-5-5"}}, `models.research must be one of: sonnet-5-5, got "opus-5-5"`)
	// Once it is cleared from its kind, it can't come back.
	mustWrite(Fields{"models": map[string]string{"fix-error": "sonnet-5-5"}})
	refused(Fields{"models": map[string]string{"implement-work-item": "opus-5-5"}},
		`models.implement-work-item must be one of: sonnet-5-5, got "opus-5-5"`)
	fake.SetAgentWorkModelsOffered("sonnet-5-5", "opus-5-5")

	// A Flightdeck that has the other four newer settings but not models: the
	// key is not read, and is unknown on a write.
	mustWrite(Fields{"models": both})
	fake.SetAgentWorkModelsSupported(false)
	if aw, err = c.GetAgentWork(ctx, project); err != nil {
		t.Fatal(err)
	}
	if aw.ReportsModels() || !aw.ReportsAutoMerge() || !aw.ReportsSendCIFailures() || !aw.ReportsWaitForChecks() || !aw.ReportsSendErrors() {
		t.Fatalf("this Flightdeck reads the other four newer settings but not models, got models %v, auto_merge %s, send_ci_failures %s, "+
			"wait_for_checks %s, send_errors %s", aw.Models, show(aw.AutoMerge), show(aw.SendCIFailures), show(aw.WaitForChecks), show(aw.SendErrors))
	}
	version = aw.LockVersion
	for _, value := range []any{map[string]string{}, both, nil, ""} {
		_, err := write(Fields{"models": value, "send_errors": false})
		apiErr, ok := AsError(err)
		if !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute ||
			!strings.HasPrefix(apiErr.Message, "unknown key: models (settable: enabled, kinds, auto_merge, send_ci_failures, send_errors, wait_for_checks, label_id, ") {
			t.Fatalf("this Flightdeck must refuse models %#v as an unknown key and list the other switches as settable, got %v", value, err)
		}
		if strings.Contains(strings.TrimPrefix(apiErr.Message, "unknown key: models"), "models") {
			t.Fatalf("this Flightdeck must not list models as settable: %s", apiErr.Message)
		}
	}
	got = mustWrite(Fields{"send_errors": false})
	if got.ReportsModels() || got.SendErrors == nil || *got.SendErrors {
		t.Fatalf("this Flightdeck takes a write without models: send_errors %s, models %v", show(got.SendErrors), got.Models)
	}

	// A Flightdeck older than all five names all five, sorted, as the API
	// does.
	fake.SetAgentWorkAutoMergeSupported(false)
	_, err = write(Fields{"models": both, "send_errors": true, "wait_for_checks": false, "send_ci_failures": false, "auto_merge": false})
	apiErr, ok := AsError(err)
	if !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute ||
		!strings.HasPrefix(apiErr.Message, "unknown keys: auto_merge, models, send_ci_failures, send_errors, wait_for_checks (settable: enabled, kinds, label_id, ") {
		t.Fatalf("a Flightdeck older than all five must refuse them as unknown keys, got %v", err)
	}
	if row := fake.AgentWorkOf(project); row == nil || fmt.Sprint(row.Models) != fmt.Sprint(both) || row.SendErrors {
		t.Fatalf("the older Flightdecks' refusals must save nothing, got %+v", row)
	}
}

// The fake acts as one Flightdeck release at a time, and releases are
// ordered: auto_merge came first, then send_ci_failures, then
// wait_for_checks, then send_errors, then models. So a fake older than
// auto_merge knows none of them, and none knows a setting without the ones
// before it. The older-Flightdeck tests of all five settings lean on this, so
// the switches are checked here directly.
func TestAgentWork_fakeReleasesAreOrdered(t *testing.T) {
	ctx := context.Background()
	fake := flightdecktest.New(t)
	c, err := New(fake.URL, fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	project := fake.AddProject("Releases", "REL").ID
	// Each setting's value is one the release takes when it knows the key.
	values := map[string]any{"auto_merge": false, "send_ci_failures": false, "wait_for_checks": false, "send_errors": false,
		"models": map[string]any{}}
	knows := func(what string, autoMerge, sendCIFailures, waitForChecks, sendErrors, models bool) {
		t.Helper()
		aw, err := c.GetAgentWork(ctx, project)
		if err != nil {
			t.Fatal(err)
		}
		if aw.ReportsAutoMerge() != autoMerge || aw.ReportsSendCIFailures() != sendCIFailures || aw.ReportsWaitForChecks() != waitForChecks ||
			aw.ReportsSendErrors() != sendErrors || aw.ReportsModels() != models {
			t.Fatalf("%s: reads auto_merge %t, send_ci_failures %t, wait_for_checks %t, send_errors %t and models %t, want %t, %t, %t, %t and %t",
				what, aw.ReportsAutoMerge(), aw.ReportsSendCIFailures(), aw.ReportsWaitForChecks(), aw.ReportsSendErrors(), aw.ReportsModels(),
				autoMerge, sendCIFailures, waitForChecks, sendErrors, models)
		}
		// A write naming a setting the release lacks is refused as an unknown
		// key; one it knows is taken.
		for key, known := range map[string]bool{"auto_merge": autoMerge, "send_ci_failures": sendCIFailures,
			"wait_for_checks": waitForChecks, "send_errors": sendErrors, "models": models} {
			_, err := c.UpdateAgentWork(ctx, project, Fields{key: values[key]}, aw.LockVersion)
			switch {
			case known && err != nil:
				t.Fatalf("%s: a write of %s must be taken, got %v", what, key, err)
			case !known && (err == nil || !strings.Contains(err.Error(), "unknown key: "+key+" ")):
				t.Fatalf("%s: a write of %s must be refused as an unknown key, got %v", what, key, err)
			}
			// Reread: a write that changed something moved the version.
			if aw, err = c.GetAgentWork(ctx, project); err != nil {
				t.Fatal(err)
			}
		}
	}

	knows("the fake's default", true, true, true, true, true)
	fake.SetAgentWorkAutoMergeSupported(false)
	knows("auto_merge turned off", false, false, false, false, false)
	fake.SetAgentWorkAutoMergeSupported(true)
	knows("auto_merge turned back on", true, false, false, false, false)
	fake.SetAgentWorkSendCIFailuresSupported(true)
	knows("send_ci_failures turned on", true, true, false, false, false)
	fake.SetAgentWorkSendCIFailuresSupported(false)
	knows("send_ci_failures turned off", true, false, false, false, false)
	fake.SetAgentWorkAutoMergeSupported(true)
	knows("auto_merge turned on, already known", true, false, false, false, false)
	fake.SetAgentWorkRelease(flightdecktest.AgentWorkBeforeAutoMerge)
	fake.SetAgentWorkSendCIFailuresSupported(false)
	knows("send_ci_failures turned off below auto_merge", false, false, false, false, false)
	fake.SetAgentWorkSendCIFailuresSupported(true)
	knows("send_ci_failures turned on below auto_merge", true, true, false, false, false)
	fake.SetAgentWorkRelease(flightdecktest.AgentWorkWithAutoMerge)
	knows("the release with auto_merge", true, false, false, false, false)
	fake.SetAgentWorkWaitForChecksSupported(true)
	knows("wait_for_checks turned on", true, true, true, false, false)
	fake.SetAgentWorkSendCIFailuresSupported(true)
	knows("send_ci_failures turned on, already known", true, true, true, false, false)
	fake.SetAgentWorkWaitForChecksSupported(false)
	knows("wait_for_checks turned off", true, true, false, false, false)
	fake.SetAgentWorkRelease(flightdecktest.AgentWorkBeforeAutoMerge)
	fake.SetAgentWorkWaitForChecksSupported(false)
	knows("wait_for_checks turned off below auto_merge", false, false, false, false, false)
	fake.SetAgentWorkWaitForChecksSupported(true)
	knows("wait_for_checks turned on below auto_merge", true, true, true, false, false)
	fake.SetAgentWorkSendCIFailuresSupported(false)
	knows("send_ci_failures turned off from wait_for_checks", true, false, false, false, false)
	fake.SetAgentWorkSendErrorsSupported(true)
	knows("send_errors turned on", true, true, true, true, false)
	fake.SetAgentWorkWaitForChecksSupported(true)
	knows("wait_for_checks turned on, already known", true, true, true, true, false)
	fake.SetAgentWorkSendErrorsSupported(false)
	knows("send_errors turned off", true, true, true, false, false)
	fake.SetAgentWorkSendErrorsSupported(false)
	knows("send_errors turned off again", true, true, true, false, false)
	fake.SetAgentWorkRelease(flightdecktest.AgentWorkBeforeAutoMerge)
	fake.SetAgentWorkSendErrorsSupported(false)
	knows("send_errors turned off below auto_merge", false, false, false, false, false)
	fake.SetAgentWorkSendErrorsSupported(true)
	knows("send_errors turned on below auto_merge", true, true, true, true, false)
	fake.SetAgentWorkWaitForChecksSupported(false)
	knows("wait_for_checks turned off from send_errors", true, true, false, false, false)
	fake.SetAgentWorkSendErrorsSupported(true)
	fake.SetAgentWorkSendCIFailuresSupported(false)
	knows("send_ci_failures turned off from send_errors", true, false, false, false, false)
	fake.SetAgentWorkSendErrorsSupported(true)
	fake.SetAgentWorkAutoMergeSupported(false)
	knows("auto_merge turned off from send_errors", false, false, false, false, false)
	fake.SetAgentWorkModelsSupported(true)
	knows("models turned on", true, true, true, true, true)
	fake.SetAgentWorkSendErrorsSupported(true)
	knows("send_errors turned on, already known", true, true, true, true, true)
	fake.SetAgentWorkModelsSupported(false)
	knows("models turned off", true, true, true, true, false)
	fake.SetAgentWorkModelsSupported(false)
	knows("models turned off again", true, true, true, true, false)
	fake.SetAgentWorkRelease(flightdecktest.AgentWorkBeforeAutoMerge)
	fake.SetAgentWorkModelsSupported(false)
	knows("models turned off below auto_merge", false, false, false, false, false)
	fake.SetAgentWorkModelsSupported(true)
	knows("models turned on below auto_merge", true, true, true, true, true)
	fake.SetAgentWorkSendErrorsSupported(false)
	knows("send_errors turned off from the newest", true, true, true, false, false)
	fake.SetAgentWorkModelsSupported(true)
	fake.SetAgentWorkAutoMergeSupported(false)
	knows("auto_merge turned off from the newest", false, false, false, false, false)
}
