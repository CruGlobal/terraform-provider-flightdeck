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
