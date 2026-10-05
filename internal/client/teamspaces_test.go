package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"
)

func teamspaceListServer(t *testing.T, names ...string) (*Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		rows := make([]map[string]any, len(names))
		for i, n := range names {
			rows[i] = map[string]any{"id": i + 1, "name": n, "lock_version": 0}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": rows, "meta": map[string]any{"page": 1, "per_page": 100, "total_pages": 1}})
	}))
	return c, &calls
}

// The API lowercases both sides in SQL, which matches a dotted capital I
// that Unicode case folding does not. Every team the API matched comes back.
func TestFindTeamspacesByName_matchesTheWayTheAPIDoes(t *testing.T) {
	c, _ := teamspaceListServer(t, "Infra", "İnfra", "INFRA")
	got, err := c.FindTeamspacesByName(context.Background(), "infra")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d teams, want all 3 the API matched: %+v", len(got), got)
	}
}

// A row that does not carry the name is an error, never something quietly
// left out, so a lookup cannot narrow several teams to one by itself.
func TestFindTeamspacesByName_refusesARowThatDoesNotMatch(t *testing.T) {
	c, _ := teamspaceListServer(t, "Platform", "Platform Team")
	_, err := c.FindTeamspacesByName(context.Background(), "platform")
	if err == nil || !strings.Contains(err.Error(), `2 ("Platform Team")`) {
		t.Fatalf("err = %v, want a refusal naming the stray team", err)
	}
}

// A blank name would be no filter at all, so it is refused without asking.
func TestFindTeamspacesByName_blankIsRefused(t *testing.T) {
	c, calls := teamspaceListServer(t, "Platform")
	if _, err := c.FindTeamspacesByName(context.Background(), " \t"); err == nil {
		t.Fatal("a blank name was looked up")
	}
	if calls.Load() != 0 {
		t.Errorf("a blank lookup reached the server %d time(s)", calls.Load())
	}
}

func TestTeamspaceHas(t *testing.T) {
	desc, lead := "Pipelines", int64(7)
	team := &Teamspace{Name: "Platform", Description: &desc, LeadID: &lead}
	cases := []struct {
		fields Fields
		want   bool
	}{
		{Fields{"name": "Platform", "description": "Pipelines", "lead_id": int64(7)}, true},
		{Fields{"name": "Platform core", "description": "Pipelines", "lead_id": int64(7)}, false},
		{Fields{"name": "Platform", "lead_id": int64(7)}, false},
		{Fields{"name": "Platform", "description": "Pipelines"}, false},
		{Fields{"name": "Platform", "description": "Other", "lead_id": int64(7)}, false},
		{Fields{"name": "Platform", "description": "Pipelines", "lead_id": int64(8)}, false},
	}
	for i, tc := range cases {
		if got := teamspaceHas(team, tc.fields); got != tc.want {
			t.Errorf("case %d: teamspaceHas = %v, want %v", i, got, tc.want)
		}
	}
	if !teamspaceHas(&Teamspace{Name: "Bare"}, Fields{"name": "Bare"}) {
		t.Error("a team with no description or lead should match a create that sends neither")
	}
}

// Like the API, the fake checks a teamspace create's body against its key:
// the same key and body replay the first create, and the same key with a
// different body is a 409 idempotency_key_reused that creates nothing.
func TestFakeTeamspaceCreateIsFingerprinted(t *testing.T) {
	fake := flightdecktest.New(t)
	c, err := New(fake.URL, fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	post := func(fields Fields) (*Teamspace, error) {
		var out Teamspace
		err := c.Post(ctx, teamspacesPath, map[string]any{teamspaceRoot: fields}, &out, WithIdempotencyKey("one-key"))
		return &out, err
	}
	first, err := post(Fields{"name": "Platform"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := post(Fields{"name": "Platform"})
	if err != nil || again.ID != first.ID {
		t.Fatalf("the same key and body: got %+v, %v; want a replay of %d", again, err, first.ID)
	}
	for _, other := range []Fields{{"name": "Platform core"}, {"name": "Platform", "description": "Pipelines"}} {
		if _, err := post(other); !HasCode(err, CodeIdempotencyKeyReused) {
			t.Errorf("the same key with %v: err = %v, want 409 idempotency_key_reused", other, err)
		}
	}
	teams, err := c.ListTeamspaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(teams) != 1 {
		t.Errorf("teams = %+v, want only the first", teams)
	}
}
