package client

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"
)

// A read decodes ci_workflows so that [] (a Flightdeck that has the setting,
// with nothing listed) and no key at all (one older than the setting) stay
// apart: ReportsCIWorkflows is what the provider asks.
func TestGithubIntegration_ciWorkflowsDecoding(t *testing.T) {
	for _, tc := range []struct {
		body     string
		reported bool
		want     []string
	}{
		{`{"id": 1, "ci_workflows": ["CI", "Deploy"]}`, true, []string{"CI", "Deploy"}},
		{`{"id": 1, "ci_workflows": []}`, true, []string{}},
		{`{"id": 1}`, false, nil},
		{`{"id": 1, "ci_workflows": null}`, false, nil},
	} {
		var g GithubIntegration
		if err := json.Unmarshal([]byte(tc.body), &g); err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		if g.ReportsCIWorkflows() != tc.reported || !slices.Equal(g.CIWorkflows, tc.want) {
			t.Errorf("%s: reported %t, list %#v; want %t, %#v", tc.body, g.ReportsCIWorkflows(), g.CIWorkflows, tc.reported, tc.want)
		}
	}
}

// newCIWorkflowsFake returns a client for a fake with one caller-managed
// integration, and that integration.
func newCIWorkflowsFake(t *testing.T) (*flightdecktest.Server, *Client, *GithubIntegration) {
	t.Helper()
	fake := flightdecktest.New(t)
	c, err := New(fake.URL, fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	project := fake.AddProject("CI workflows", "CIW").ID
	g, err := c.CreateGithubIntegration(context.Background(), project,
		Fields{"repo_full_name": "example-org/ciw", "secret": strings.Repeat("s", 20)}, "ciw-create")
	if err != nil {
		t.Fatal(err)
	}
	return fake, c, g
}

// The fake's ci_workflows follows the API's contract: [] until set, set on
// create and update, trimmed with a repeat ignoring case dropped (the first
// spelling kept), [] clears it, a null or a blank string is no opinion, and
// leaving the key out leaves it alone. The provider's tests lean on this, so
// it is checked here directly.
func TestGithubIntegration_ciWorkflowsContract(t *testing.T) {
	ctx := context.Background()
	fake, c, g := newCIWorkflowsFake(t)
	if !g.ReportsCIWorkflows() || len(g.CIWorkflows) != 0 {
		t.Fatalf("a new integration reads ci_workflows [], got %#v (reported: %t)", g.CIWorkflows, g.ReportsCIWorkflows())
	}
	version := g.LockVersion
	write := func(fields Fields) (*GithubIntegration, error) {
		t.Helper()
		got, err := c.UpdateGithubIntegration(ctx, g.ID, fields, version)
		if err == nil {
			version = got.LockVersion
		}
		return got, err
	}
	want := func(fields Fields, list ...string) {
		t.Helper()
		got, err := write(fields)
		if err != nil {
			t.Fatalf("writing %#v: %v", fields, err)
		}
		if list == nil {
			list = []string{}
		}
		if !slices.Equal(got.CIWorkflows, list) {
			t.Fatalf("writing %#v: ci_workflows reads %#v, want %#v", fields, got.CIWorkflows, list)
		}
		read, err := c.GetGithubIntegration(ctx, g.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(read.CIWorkflows, list) {
			t.Fatalf("after writing %#v, a read has %#v, want %#v", fields, read.CIWorkflows, list)
		}
	}

	want(Fields{"ci_workflows": []string{"CI", " ci ", "Deploy\t", "\nCI\n"}}, "CI", "Deploy")
	want(Fields{"ci_workflows": []string{"Tests"}}, "Tests")
	for _, blank := range []any{nil, "", "   ", "\t\n"} {
		want(Fields{"ci_workflows": blank}, "Tests")
	}
	want(Fields{"enabled": true}, "Tests")
	want(Fields{"ci_workflows": []string{}})

	// Compared with full Unicode case folding, as Ruby's downcase(:fold)
	// does: "Straße" and "STRASSE" are one name, and so are the Cherokee
	// capital and small letters. A no-break space is not trimmed.
	want(Fields{"ci_workflows": []string{"Straße", "STRASSE", "\u13a0", "\uab70", "\u00a0CI"}}, "Straße", "\u13a0", "\u00a0CI")
	// A NUL at either end passes the check, made on the trimmed name, but is
	// stored as a marker, because the model marks NULs before it trims.
	want(Fields{"ci_workflows": []string{"CI\x00"}}, "CI\u2400")
	// The length is counted after the trim, and the count after the repeats
	// are dropped.
	padded := "  " + strings.Repeat("y", 100) + "  "
	want(Fields{"ci_workflows": []string{padded}}, strings.Repeat("y", 100))
	twenty := make([]string, 0, 21)
	for i := 1; i <= 20; i++ {
		twenty = append(twenty, fmt.Sprintf("Workflow %d", i))
	}
	want(Fields{"ci_workflows": append(slices.Clone(twenty), "WORKFLOW 1")}, twenty...)

	// The settings page saves it out of band.
	fake.SetGithubCIWorkflows(g.ID, []string{"Nightly ", "nightly"})
	read, err := c.GetGithubIntegration(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(read.CIWorkflows, []string{"Nightly"}) || read.LockVersion != version+1 {
		t.Fatalf("an out-of-band save reads %#v at %d, want [Nightly] at %d", read.CIWorkflows, read.LockVersion, version+1)
	}
}

// Every refusal the API makes, in its words: a 422 invalid_attribute naming
// the entry, and nothing changes. A refused list on create links nothing.
func TestGithubIntegration_ciWorkflowsRefusals(t *testing.T) {
	ctx := context.Background()
	fake, c, g := newCIWorkflowsFake(t)
	stored, err := c.UpdateGithubIntegration(ctx, g.ID, Fields{"ci_workflows": []string{"CI"}}, g.LockVersion)
	if err != nil {
		t.Fatal(err)
	}
	twentyOne := make([]any, 0, 21)
	for i := 1; i <= 21; i++ {
		twentyOne = append(twentyOne, fmt.Sprintf("Workflow %d", i))
	}
	control := "ci_workflows[1] must not include control characters such as line breaks or tabs"
	cases := []struct {
		value any
		want  string
	}{
		{5, "ci_workflows must be a list of workflow names, got 5"},
		{true, "ci_workflows must be a list of workflow names, got true"},
		{"CI", `ci_workflows must be a list of workflow names, got "CI"`},
		{map[string]any{"name": "CI"}, `ci_workflows must be a list of workflow names, got {"name" => "CI"}`},
		{[]any{5}, "ci_workflows[0] must be a workflow name (a string), got 5"},
		{[]any{"CI", []any{"x"}}, `ci_workflows[1] must be a workflow name (a string), got ["x"]`},
		{[]any{map[string]any{"a": 1}}, `ci_workflows[0] must be a workflow name (a string), got {"a" => 1}`},
		{[]any{nil}, "ci_workflows[0] must be a workflow name (a string), got null"},
		{[]any{"CI", nil}, "ci_workflows[1] must be a workflow name (a string), got null"},
		// A null entry is refused before any entry is checked.
		{[]any{"  ", nil}, "ci_workflows[1] must be a workflow name (a string), got null"},
		{[]any{strings.Repeat("x", 101)}, "ci_workflows[0] must be 100 characters or fewer (got 101)"},
		{[]any{"CI", "  "}, "ci_workflows[1] must not be blank"},
		{[]any{"\u00a0"}, "ci_workflows[0] must not be blank"},
		{[]any{"Deploy", "CI\nnightly"}, control},
		{[]any{"Deploy", "CI\r\nnightly"}, control},
		{[]any{"Deploy", "CI\tnightly"}, control},
		{[]any{"Deploy", "CI\u0085nightly"}, control},
		{[]any{"Deploy", "CI\u2028nightly"}, control},
		{[]any{"Deploy", "C\u0007I"}, control},
		{[]any{"Deploy", "C\x00I"}, control},
		{twentyOne, "ci_workflows may list at most 20 workflows (got 21 different names)"},
	}
	for _, tc := range cases {
		_, err := c.UpdateGithubIntegration(ctx, g.ID, Fields{"ci_workflows": tc.value}, stored.LockVersion)
		apiErr, ok := AsError(err)
		if !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute || apiErr.Message != tc.want {
			t.Errorf("writing %#v:\n want a 422 invalid_attribute saying %s\n  got %v", tc.value, tc.want, err)
		}
	}
	if now := fake.GithubIntegration(g.ID); !slices.Equal(now.CIWorkflows, []string{"CI"}) || now.LockVersion != stored.LockVersion {
		t.Fatalf("a refused write must change nothing, got %#v at %d", now.CIWorkflows, now.LockVersion)
	}

	project := fake.AddProject("Refused", "REF").ID
	_, err = c.CreateGithubIntegration(ctx, project,
		Fields{"repo_full_name": "example-org/refused", "secret": strings.Repeat("s", 20), "ci_workflows": []any{"a\nb"}}, "ciw-refused")
	if apiErr, ok := AsError(err); !ok || apiErr.Status != 422 || apiErr.Message != "ci_workflows[0] must not include control characters such as line breaks or tabs" {
		t.Fatalf("a create with a refused list: want a 422 naming the entry, got %v", err)
	}
	if list, err := c.ListGithubIntegrations(ctx, project); err != nil || len(list) != 0 {
		t.Fatalf("a refused create must link nothing, got %d integrations (%v)", len(list), err)
	}
}

// A Flightdeck older than ci_workflows reads without the key. One that
// refuses unknown root keys refuses any write naming it, whatever its value
// (the check comes before any value is read), and lists what it takes; one
// older still, from before that check, takes the write and drops the list.
// Either way a write without the key works.
func TestGithubIntegration_ciWorkflowsOnAnOlderFlightdeck(t *testing.T) {
	ctx := context.Background()
	fake, c, g := newCIWorkflowsFake(t)
	stored, err := c.UpdateGithubIntegration(ctx, g.ID, Fields{"ci_workflows": []string{"CI"}}, g.LockVersion)
	if err != nil {
		t.Fatal(err)
	}

	fake.SetGithubCIWorkflowsSupport(flightdecktest.CIWorkflowsUnknownKey)
	read, err := c.GetGithubIntegration(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.ReportsCIWorkflows() {
		t.Fatalf("an older Flightdeck must read without ci_workflows, got %#v", read.CIWorkflows)
	}
	const refusal = "unknown key: ci_workflows (settable: repo_full_name, secret, enabled, ci_failure_action)"
	for _, value := range []any{[]string{"CI"}, []string{}, nil, "", []any{nil}} {
		_, err := c.UpdateGithubIntegration(ctx, g.ID, Fields{"ci_workflows": value, "enabled": true}, read.LockVersion)
		if apiErr, ok := AsError(err); !ok || apiErr.Status != 422 || apiErr.Code != CodeInvalidAttribute || apiErr.Message != refusal {
			t.Fatalf("an older Flightdeck must refuse ci_workflows %#v as an unknown key, got %v", value, err)
		}
	}
	project := fake.AddProject("Older", "OLD").ID
	_, err = c.CreateGithubIntegration(ctx, project,
		Fields{"repo_full_name": "example-org/older", "secret": strings.Repeat("s", 20), "ci_workflows": []string{"CI"}}, "ciw-older")
	if apiErr, ok := AsError(err); !ok || apiErr.Message != refusal {
		t.Fatalf("an older Flightdeck must refuse ci_workflows on create too, got %v", err)
	}
	if list, _ := c.ListGithubIntegrations(ctx, project); len(list) != 0 {
		t.Fatal("a refused create must link nothing")
	}
	got, err := c.UpdateGithubIntegration(ctx, g.ID, Fields{"enabled": true}, read.LockVersion)
	if err != nil || got.ReportsCIWorkflows() {
		t.Fatalf("an older Flightdeck takes a write without ci_workflows: %v, %#v", err, got)
	}

	fake.SetGithubCIWorkflowsSupport(flightdecktest.CIWorkflowsIgnored)
	got, err = c.UpdateGithubIntegration(ctx, g.ID, Fields{"ci_workflows": []string{"Tests"}, "enabled": true}, got.LockVersion)
	if err != nil || got.ReportsCIWorkflows() {
		t.Fatalf("a Flightdeck from before the strict root takes the write without the key: %v, %#v", err, got)
	}

	// Upgraded, it reads what it had stored: the ignored write changed nothing.
	fake.SetGithubCIWorkflowsSupport(flightdecktest.CIWorkflowsSupported)
	if read, err = c.GetGithubIntegration(ctx, g.ID); err != nil || !slices.Equal(read.CIWorkflows, stored.CIWorkflows) {
		t.Fatalf("after the upgrade ci_workflows reads %#v, want %#v (%v)", read.CIWorkflows, stored.CIWorkflows, err)
	}
}
