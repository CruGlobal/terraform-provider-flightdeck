package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"golang.org/x/text/cases"
)

// ciWorkflowsSecret makes every integration here caller-managed, so the
// tests that run live need no GitHub App access.
const ciWorkflowsSecret = "caller-managed-secret-0123456789"

// ciWorkflowsConfig is a caller-managed integration with the given
// ci_workflows line ("" leaves it unset) and any other lines.
func ciWorkflowsConfig(env *testEnv, identifier, repo, workflows string, more ...string) string {
	body := fmt.Sprintf("  secret = %q", ciWorkflowsSecret)
	if workflows != "" {
		body += "\n  ci_workflows = " + workflows
	}
	for _, line := range more {
		body += "\n  " + line
	}
	return ciActionConfig(env, identifier, repo, body)
}

// ciWorkflowsIs checks the list in state, in order. want nil checks for null.
func ciWorkflowsIs(want []string) statecheck.StateCheck {
	return statecheck.ExpectKnownValue(ghRes, tfjsonpath.New(ciWorkflowsKey), ciWorkflowsValue(want))
}

func ciWorkflowsValue(want []string) knownvalue.Check {
	if want == nil {
		return knownvalue.Null()
	}
	checks := make([]knownvalue.Check, 0, len(want))
	for _, name := range want {
		checks = append(checks, knownvalue.StringExact(name))
	}
	return knownvalue.ListExact(checks)
}

// wrapped matches the phrases in order, as Terraform prints a diagnostic:
// each phrase literally, except that a line may break at any space.
func wrapped(phrases ...string) *regexp.Regexp {
	parts := make([]string, 0, len(phrases))
	for _, phrase := range phrases {
		words := strings.Split(phrase, " ")
		for i, w := range words {
			words[i] = regexp.QuoteMeta(w)
		}
		parts = append(parts, strings.Join(words, `\s+`))
	}
	return regexp.MustCompile("(?s)" + strings.Join(parts, ".*"))
}

// ciWorkflowsWrites returns the github_integration object of every create
// and update the fake saw, in order.
func ciWorkflowsWrites(t *testing.T, env *testEnv) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range env.fake.Requests() {
		if (r.Method == "POST" || r.Method == "PATCH") && strings.Contains(r.Path, "github-integrations") {
			var body map[string]map[string]any
			if err := json.Unmarshal(r.Body, &body); err != nil {
				t.Fatalf("decoding %s: %v", r.Body, err)
			}
			out = append(out, body["github_integration"])
		}
	}
	return out
}

// ci_workflows is set on create, imported, changed, cleared with [] and
// dropped from the configuration like ci_failure_action. This one runs live
// too: the integration is caller-managed, so no GitHub App is involved.
func TestGithubIntegration_ciWorkflowsRoundTrip(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	update := resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(ghRes, plancheck.ResourceActionUpdate)},
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:            ciWorkflowsConfig(env, identifier, repo, `["CI", "Deploy"]`),
				ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs([]string{"CI", "Deploy"})},
			},
			{
				ResourceName:            ghRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"secret"},
			},
			{
				Config:            ciWorkflowsConfig(env, identifier, repo, `["Tests"]`),
				ConfigPlanChecks:  update,
				ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs([]string{"Tests"})},
			},
			{
				// [] clears the list, and reads back as [], not null.
				Config:            ciWorkflowsConfig(env, identifier, repo, `[]`),
				ConfigPlanChecks:  update,
				ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs([]string{})},
			},
			{
				// U+0264 and U+A7CB (a capital added in Unicode 16) are two names
				// under Unicode 15, which Flightdeck's Ruby folds by. A Flightdeck
				// whose Ruby folds by Unicode 16 or later would keep only the
				// first, so the live run of this test notices that change, which
				// the plan's own folding must then follow.
				Config:            ciWorkflowsConfig(env, identifier, repo, `["\u0264", "\ua7cb"]`),
				ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs([]string{"\u0264", "\ua7cb"})},
			},
			{
				// A name may differ from another only in a no-break space, which
				// Flightdeck does not trim, so it is stored as written.
				Config:            ciWorkflowsConfig(env, identifier, repo, `["Nightly", "CI\u00a0"]`),
				ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs([]string{"Nightly", "CI\u00a0"})},
			},
			{
				// Dropping the line keeps the stored list and plans nothing.
				Config: ciWorkflowsConfig(env, identifier, repo, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs([]string{"Nightly", "CI\u00a0"})},
			},
		},
	})
	if env.live() {
		return
	}
	writes := ciWorkflowsWrites(t, env)
	want := []any{[]any{"CI", "Deploy"}, []any{"Tests"}, []any{}, []any{"\u0264", "\ua7cb"}, []any{"Nightly", "CI\u00a0"}}
	if len(writes) != len(want) {
		t.Fatalf("expected %d writes, got %d: %v", len(want), len(writes), writes)
	}
	for i, w := range want {
		got, sent := writes[i][ciWorkflowsKey]
		// [] must go out as [], which clears the list; null would be no opinion.
		if !sent || !reflect.DeepEqual(got, w) {
			t.Errorf("write %d: ci_workflows sent as %#v, want %#v", i+1, got, w)
		}
	}
}

// Unset, ci_workflows is never sent: state reads whatever Flightdeck holds,
// including a list saved on the settings page, and plans nothing for it.
func TestGithubIntegration_ciWorkflowsUnsetIsNeverSent(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:            ciWorkflowsConfig(env, identifier, repo, ""),
				Check:             captureAttr(ghRes, "id", &id),
				ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs([]string{})},
			},
			{
				PreConfig: func() { env.fake.SetGithubCIWorkflows(mustInt(id), []string{"Nightly"}) },
				Config:    ciWorkflowsConfig(env, identifier, repo, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs([]string{"Nightly"})},
			},
			{
				// Another change keeps the list as the page left it, and the plan
				// knows it rather than calling it known after apply.
				Config: ciWorkflowsConfig(env, identifier, repo, "", `ci_failure_action = "file_intake"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(ghRes, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(ghRes, tfjsonpath.New(ciWorkflowsKey), ciWorkflowsValue([]string{"Nightly"})),
					},
				},
				ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs([]string{"Nightly"})},
				Check: func(*terraform.State) error {
					if g := env.fake.GithubIntegration(mustInt(id)); !slices.Equal(g.CIWorkflows, []string{"Nightly"}) {
						return fmt.Errorf("the update changed the stored list: %#v", g.CIWorkflows)
					}
					return nil
				},
			},
		},
	})
	for i, w := range ciWorkflowsWrites(t, env) {
		if _, sent := w[ciWorkflowsKey]; sent {
			t.Errorf("write %d sent ci_workflows while it is unset: %v", i+1, w)
		}
	}
}

// Configured, a list changed on the settings page shows in the next plan and
// is set back.
func TestGithubIntegration_ciWorkflowsDrift(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	config := ciWorkflowsConfig(env, identifier, repo, `["CI"]`)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config, Check: captureAttr(ghRes, "id", &id)},
			{
				PreConfig: func() { env.fake.SetGithubCIWorkflows(mustInt(id), []string{"CI", "Deploy"}) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(ghRes, plancheck.ResourceActionUpdate)},
				},
				ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs([]string{"CI"})},
			},
			{Config: config, PlanOnly: true},
		},
	})
	if g := env.fake.GithubIntegration(mustInt(id)); g != nil {
		t.Fatalf("the integration should be gone after the test, got %+v", g)
	}
}

// The plan refuses a list Flightdeck would refuse or store differently, each
// problem on its entry, and nothing is created.
func TestGithubIntegration_ciWorkflowsPlanRefusals(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	twentyOne := make([]string, 0, 21)
	for i := 1; i <= 21; i++ {
		twentyOne = append(twentyOne, fmt.Sprintf("%q", fmt.Sprintf("Workflow %d", i)))
	}
	refused := func(workflows string, phrases ...string) resource.TestStep {
		return resource.TestStep{
			Config:      ciWorkflowsConfig(env, identifier, repo, workflows),
			PlanOnly:    true,
			ExpectError: wrapped(phrases...),
		}
	}
	invalid := "Invalid CI workflow name"
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			refused(`[" CI "]`, invalid, `ci_workflows[0] (" CI ") starts or ends with a space`, `Write "CI" instead`),
			refused(`["CI", "Deploy\n"]`, invalid, `ci_workflows[1] ("Deploy\n") starts or ends`),
			refused(`["CI", "ci"]`, "Repeated CI workflow name", `ci_workflows[1] ("ci") repeats ci_workflows[0] ("CI")`),
			refused(`["Straße", "STRASSE"]`, "Repeated CI workflow name", `ci_workflows[1] ("STRASSE") repeats`),
			refused(`["Deploy", "CI\nnightly"]`, invalid, `ci_workflows[1] ("CI\nnightly") has a control character`),
			refused(`["CI\u2028"]`, invalid, "ci_workflows[0]", "control character"),
			refused(`["CI", ""]`, invalid, "ci_workflows[1] is blank"),
			refused(`["\u00a0"]`, invalid, "ci_workflows[0] is blank"),
			refused(`["CI", null]`, invalid, "ci_workflows[1] is null"),
			refused(fmt.Sprintf("[%q]", strings.Repeat("x", 101)), invalid, "ci_workflows[0] is 101 characters long"),
			refused("["+strings.Join(twentyOne, ", ")+"]", "Too many CI workflows", "ci_workflows lists 21 workflows. Flightdeck takes at most 20."),
		},
	})
	for _, r := range env.fake.Requests() {
		if strings.Contains(r.Path, "github-integrations") {
			t.Fatalf("a refused plan must reach no GitHub integration endpoint, got %s %s", r.Method, r.Path)
		}
	}
}

// A name not known until apply is checked then, before anything is sent: a
// list the plan could not judge is refused the way the plan would have, and
// the repository is not linked.
func TestGithubIntegration_ciWorkflowsCheckedAgainAtApply(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:      ciWorkflowsConfig(env, identifier, repo, `[" ${flightdeck_project.parent.id}"]`),
				ExpectError: regexMust(`(?s)Invalid CI workflow name.*ci_workflows\[0\] \(" [0-9]+"\) starts or ends`),
			},
		},
	})
	for _, r := range env.fake.Requests() {
		if strings.Contains(r.Path, "github-integrations") && r.Method != "GET" {
			t.Fatalf("the refused list must not be sent: %s %s %s", r.Method, r.Path, r.Body)
		}
	}
}

// A Flightdeck older than ci_workflows that refuses unknown keys: an
// integration whose configuration leaves it unset works as before (it reads
// null, is never sent, and plans nothing). Setting it fails on the attribute,
// saying the server is too old, saves nothing, and applies once Flightdeck is
// upgraded.
func TestGithubIntegration_ciWorkflowsOnAnOlderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	env.fake.SetGithubCIWorkflowsSupport(flightdecktest.CIWorkflowsUnknownKey)
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	withList := ciWorkflowsConfig(env, identifier, repo, `["CI"]`, `ci_failure_action = "create_work_item"`)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:            ciWorkflowsConfig(env, identifier, repo, ""),
				Check:             captureAttr(ghRes, "id", &id),
				ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs(nil)},
			},
			{
				// Another change: the list stays unset and unsent.
				Config: ciWorkflowsConfig(env, identifier, repo, "", `ci_failure_action = "file_intake"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectKnownValue(ghRes, tfjsonpath.New(ciWorkflowsKey), knownvalue.Null())},
				},
				ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs(nil)},
			},
			{
				Config: withList,
				ExpectError: regexMust(`(?s)This Flightdeck does not support ci_workflows yet.*` +
					`refused\s+ci_workflows\s+as\s+an\s+unknown\s+setting.*older\s+than\s+the\s+release\s+that\s+added\s+CI\s+workflows.*` +
					`Nothing\s+was\s+saved.*Upgrade\s+Flightdeck\s+first.*unknown key: ci_workflows`),
			},
			{
				// Nothing was saved, ci_failure_action included.
				PreConfig: func() {
					if g := env.fake.GithubIntegration(mustInt(id)); g.CIFailureAction != "file_intake" {
						t.Errorf("a refused write must save nothing, but ci_failure_action is %q", g.CIFailureAction)
					}
				},
				Config:             withList,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Flightdeck is upgraded; the same configuration applies.
				PreConfig: func() { env.fake.SetGithubCIWorkflowsSupport(flightdecktest.CIWorkflowsSupported) },
				Config:    withList,
				ConfigStateChecks: []statecheck.StateCheck{
					ciWorkflowsIs([]string{"CI"}),
					statecheck.ExpectKnownValue(ghRes, tfjsonpath.New("ci_failure_action"), knownvalue.StringExact("create_work_item")),
				},
			},
			{Config: withList, PlanOnly: true},
		},
	})
	writes := ciWorkflowsWrites(t, env)
	for i, w := range writes[:2] {
		if _, sent := w[ciWorkflowsKey]; sent {
			t.Errorf("write %d sent an unset ci_workflows to an older Flightdeck: %v", i+1, w)
		}
	}
}

// Creating an integration with ci_workflows on such a Flightdeck fails the
// same way, and links nothing; after the upgrade the same configuration
// creates it.
func TestGithubIntegration_ciWorkflowsOnAnOlderFlightdeckAtCreate(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	env.fake.SetGithubCIWorkflowsSupport(flightdecktest.CIWorkflowsUnknownKey)
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	config := ciWorkflowsConfig(env, identifier, repo, `["CI"]`)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				ExpectError: regexMust(`(?s)This Flightdeck does not support ci_workflows yet.*Nothing\s+was\s+created:\s+the\s+repository\s+is\s+not\s+linked.*` +
					`Upgrade\s+Flightdeck\s+first.*unknown key: ci_workflows`),
			},
			{
				PreConfig: func() {
					for _, r := range env.fake.Requests() {
						if r.Method == "POST" && strings.HasSuffix(r.Path, "/github-integrations") && r.Status == 201 {
							t.Errorf("the refused create linked the repository: %s", r.Response)
						}
					}
					env.fake.SetGithubCIWorkflowsSupport(flightdecktest.CIWorkflowsSupported)
				},
				Config:            config,
				ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs([]string{"CI"})},
			},
		},
	})
}

// A Flightdeck older still, from before the GitHub integration resource
// refused unknown keys, takes a write and drops ci_workflows. On the apply
// that creates the integration that is a warning, so it isn't tainted; the
// next plan shows the list again, and the update that follows fails saying
// the server is too old, until it is upgraded.
func TestGithubIntegration_ciWorkflowsIgnoredByAnOlderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	env.fake.SetGithubCIWorkflowsSupport(flightdecktest.CIWorkflowsIgnored)
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	config := ciWorkflowsConfig(env, identifier, repo, `["CI"]`)
	var id string
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config:             config,
				ExpectNonEmptyPlan: true,
				Check:              captureAttr(ghRes, "id", &id),
			},
			{
				Config:      config,
				ExpectError: regexMust(`(?s)This Flightdeck does not support ci_workflows yet.*ignored\s+ci_workflows.*Upgrade\s+Flightdeck\s+first`),
			},
			{
				// An update, not a replace: the create did not taint it.
				PreConfig: func() { env.fake.SetGithubCIWorkflowsSupport(flightdecktest.CIWorkflowsSupported) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(ghRes, plancheck.ResourceActionUpdate)},
				},
				Check:             resource.TestCheckResourceAttrPtr(ghRes, "id", &id),
				ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs([]string{"CI"})},
			},
			{Config: config, PlanOnly: true},
		},
	})
	at := tftypes.NewAttributePath().WithAttributeName(ciWorkflowsKey)
	if !diagnosticAt(recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning),
		ciWorkflowsUnsupportedSummary, at, "The repository was linked", "ignored ci_workflows") {
		t.Fatalf("no create-time warning on ci_workflows; apply warnings: %s",
			describeWithPaths(recorded.bySeverity(tfprotov6.DiagnosticSeverityWarning)))
	}
	if !diagnosticAt(recorded.bySeverity(tfprotov6.DiagnosticSeverityError),
		ciWorkflowsUnsupportedSummary, at, "The rest of this change was saved") {
		t.Fatalf("no update-time error on ci_workflows; apply errors: %s",
			describeWithPaths(recorded.bySeverity(tfprotov6.DiagnosticSeverityError)))
	}
}

// Flightdeck is upgraded to ci_workflows, or rolled back below it, between
// the plan and the apply, as a saved plan applied later can see. An unset
// ci_workflows is never sent, so the apply keeps the value the plan promised
// rather than fail with "Provider produced inconsistent result", and the next
// refresh adopts Flightdeck's without planning anything.
func TestGithubIntegration_ciWorkflowsUnsetSurvivesAServerChangeDuringApply(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after flightdecktest.CIWorkflowsSupport
		// planned and adopted are the list before the change and after it.
		planned, adopted []string
	}{
		{"upgraded", flightdecktest.CIWorkflowsUnknownKey, flightdecktest.CIWorkflowsSupported, nil, []string{}},
		{"rolled back", flightdecktest.CIWorkflowsSupported, flightdecktest.CIWorkflowsUnknownKey, []string{}, nil},
		{"rolled back below the strict root", flightdecktest.CIWorkflowsSupported, flightdecktest.CIWorkflowsIgnored, []string{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, "github_integration")
			env.requireFake(t)
			env.fake.SetGithubCIWorkflowsSupport(tc.before)
			identifier := randIdentifier()
			repo := "example-org/" + strings.ToLower(identifier)
			changed := ciWorkflowsConfig(env, identifier, repo, "", `ci_failure_action = "file_intake"`)
			var id string
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{
						Config:            ciWorkflowsConfig(env, identifier, repo, ""),
						Check:             captureAttr(ghRes, "id", &id),
						ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs(tc.planned)},
					},
					{
						PreConfig: func() {
							env.fake.OnNextRequest("PATCH", "/api/v1/github-integrations/"+id, func() {
								env.fake.SetGithubCIWorkflowsSupport(tc.after)
							})
						},
						Config:            changed,
						ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs(tc.planned)},
					},
					{
						Config: changed,
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
						},
						ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs(tc.adopted)},
					},
				},
			})
		})
	}
}

// A configured list gets no such allowance: a Flightdeck rolled back below
// the setting between the plan and the apply fails the apply, saying so,
// whether it refuses the key or ignores it.
func TestGithubIntegration_ciWorkflowsConfiguredMeetsARollbackDuringApply(t *testing.T) {
	for _, tc := range []struct {
		name    string
		after   flightdecktest.CIWorkflowsSupport
		pattern string
	}{
		{"refused", flightdecktest.CIWorkflowsUnknownKey, `(?s)This Flightdeck does not support ci_workflows yet.*unknown\s+setting`},
		{"ignored", flightdecktest.CIWorkflowsIgnored, `(?s)This Flightdeck does not support ci_workflows yet.*ignored\s+ci_workflows`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, "github_integration")
			env.requireFake(t)
			identifier := randIdentifier()
			repo := "example-org/" + strings.ToLower(identifier)
			var id string
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{Config: ciWorkflowsConfig(env, identifier, repo, `["CI"]`), Check: captureAttr(ghRes, "id", &id)},
					{
						PreConfig: func() {
							env.fake.OnNextRequest("PATCH", "/api/v1/github-integrations/"+id, func() {
								env.fake.SetGithubCIWorkflowsSupport(tc.after)
							})
						},
						Config:      ciWorkflowsConfig(env, identifier, repo, `["CI", "Deploy"]`),
						ExpectError: regexMust(tc.pattern),
					},
				},
			})
		})
	}
}

// checkCIWorkflows names each problem on its entry, in the words the plan
// shows, and passes what Flightdeck stores as written.
func TestCheckCIWorkflows(t *testing.T) {
	list := func(names ...any) []attr.Value {
		out := make([]attr.Value, 0, len(names))
		for _, n := range names {
			switch v := n.(type) {
			case string:
				out = append(out, types.StringValue(v))
			case attr.Value:
				out = append(out, v)
			}
		}
		return out
	}
	type problem struct {
		at      path.Path
		summary string
		detail  string
	}
	root := path.Root(ciWorkflowsKey)
	invalid := func(i int, detail string) problem {
		return problem{root.AtListIndex(i), "Invalid CI workflow name", detail}
	}
	repeated := func(i int, detail string) problem {
		return problem{root.AtListIndex(i), "Repeated CI workflow name", detail}
	}
	twenty := make([]any, 0, 21)
	for i := 1; i <= 20; i++ {
		twenty = append(twenty, fmt.Sprintf("Workflow %d", i))
	}
	cases := []struct {
		name  string
		elems []attr.Value
		want  []problem
	}{
		{"empty", list(), nil},
		{"plain names", list("CI", "Deploy", "Nightly build"), nil},
		{"twenty", list(twenty...), nil},
		{"a hundred characters", list(strings.Repeat("x", 100)), nil},
		// Flightdeck trims only ASCII white space and NUL, so a no-break space
		// at either end is kept and stored as written.
		{"a no-break space at the end", list("CI\u00a0", "\u00a0Deploy"), nil},
		{"names not known yet", list(types.StringUnknown(), types.StringUnknown()), nil},
		{"twenty-one", list(append(slices.Clone(twenty), "Workflow 21")...), []problem{{root, "Too many CI workflows", "ci_workflows lists 21 workflows. Flightdeck takes at most 20."}}},
		{"a hundred and one characters", list(strings.Repeat("é", 101)), []problem{invalid(0, "is 101 characters long")}},
		{"blank", list("CI", ""), []problem{invalid(1, "ci_workflows[1] is blank")}},
		{"spaces", list("   "), []problem{invalid(0, "is blank")}},
		{"no-break spaces", list("\u00a0\u3000"), []problem{invalid(0, "is blank")}},
		{"null", list("CI", types.StringNull()), []problem{invalid(1, "ci_workflows[1] is null")}},
		{"leading space", list(" CI"), []problem{invalid(0, `(" CI") starts or ends with a space, tab or line break`)}},
		{"trailing tab", list("CI\t"), []problem{invalid(0, `would store "CI"`)}},
		{"line break inside", list("CI\nnightly"), []problem{invalid(0, `("CI\nnightly") has a control character`)}},
		{"bell inside", list("C\aI"), []problem{invalid(0, "has a control character")}},
		{"next line inside", list("CI\u0085nightly"), []problem{invalid(0, "has a control character")}},
		{"next line at the end", list("CI\u0085"), []problem{invalid(0, "has a control character")}},
		{"line separator", list("CI\u2028"), []problem{invalid(0, "has a control character")}},
		{"NUL at the end", list("CI\x00"), []problem{invalid(0, "has a control character")}},
		{"case", list("CI", "Deploy", "ci"), []problem{repeated(2, `ci_workflows[2] ("ci") repeats ci_workflows[0] ("CI")`)}},
		{"full case folding", list("Straße", "STRASSE"), []problem{repeated(1, "repeats ci_workflows[0]")}},
		{"Cherokee", list("\u13a0", "\uab70"), []problem{repeated(1, "repeats ci_workflows[0]")}},
		{"Kelvin sign", list("K", "k"), []problem{repeated(1, "repeats ci_workflows[0]")}},
		{"untrimmed repeat", list("CI", " ci"), []problem{invalid(1, "starts or ends"), repeated(1, "repeats ci_workflows[0]")}},
		{"the same twice", list("CI", "CI", "CI"), []problem{repeated(1, "repeats ci_workflows[0]"), repeated(2, "repeats ci_workflows[0]")}},
		{"blanks are not repeats", list("", ""), []problem{invalid(0, "is blank"), invalid(1, "is blank")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			checkCIWorkflows(tc.elems, root, &diags)
			if len(diags) != len(tc.want) {
				t.Fatalf("got %d diagnostics, want %d: %v", len(diags), len(tc.want), diags)
			}
			for i, want := range tc.want {
				d, ok := diags[i].(diag.DiagnosticWithPath)
				if !ok || !d.Path().Equal(want.at) || d.Summary() != want.summary || !strings.Contains(d.Detail(), want.detail) ||
					d.Severity() != diag.SeverityError {
					t.Errorf("diagnostic %d: got %v, want %s at %s containing %q", i, diags[i], want.summary, want.at, want.detail)
				}
			}
		})
	}
}

// The plan and Flightdeck agree: the plan passes a list exactly when
// Flightdeck takes it and stores it as written, and refuses it when
// Flightdeck would refuse it or store something else. Checked pairwise over
// names chosen to sit on every edge of the rules, against the fake's
// implementation of the API. The fake's rules are written separately from the
// plan's, but it folds names with the same x/text tables, so this test cannot
// see the plan fold differently from Flightdeck's Ruby: TestCIWorkflowFold
// and TestCIWorkflowFold_unicodeVersionIsFlightdecks guard that.
func TestCheckCIWorkflows_agreesWithFlightdeck(t *testing.T) {
	ctx := context.Background()
	fake := flightdecktest.New(t)
	c, err := client.New(fake.URL, fake.Token())
	if err != nil {
		t.Fatal(err)
	}
	project := fake.AddProject("Agreement", "AGREE").ID
	g, err := c.CreateGithubIntegration(ctx, project,
		client.Fields{"repo_full_name": "example-org/agree", "secret": ciWorkflowsSecret}, "agree")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{
		"CI", "ci", " CI", "CI ", "\tCI\n", "Deploy", "Straße", "STRASSE", "strasse", "\u13a0", "\uab70", "K", "k",
		"\u01c5", "\u01c6", "\u01c4", "\ufb03", "ffi", "\u0130", "i\u0307", "é", "e\u0301", "", " ", "\u00a0", "\u3000", "\u00a0CI",
		"CI\u00a0", "C\tI", "CI\x00", "\x00CI", "C\x00I", "\x00", "CI\u0085", "\u0085", "C\u2028I", "C\u2029I",
		"C\u007fI", "C\u009fI", "C\u00a0I", strings.Repeat("x", 100), strings.Repeat("x", 101), " " + strings.Repeat("x", 100),
		strings.Repeat("ß", 100), "\u200bCI",
	}
	var lists [][]string
	for _, a := range names {
		lists = append(lists, []string{a})
		for _, b := range names {
			lists = append(lists, []string{a, b})
		}
	}
	for n := 19; n <= 22; n++ {
		list := make([]string, 0, n)
		for i := 1; i <= n; i++ {
			list = append(list, fmt.Sprintf("Workflow %d", i))
		}
		lists = append(lists, list)
		lists = append(lists, append(slices.Clone(list[:n-1]), "workflow 1"))
	}
	version := g.LockVersion
	checked := 0
	for _, list := range lists {
		elems := make([]attr.Value, 0, len(list))
		for _, name := range list {
			elems = append(elems, types.StringValue(name))
		}
		var diags diag.Diagnostics
		checkCIWorkflows(elems, path.Root(ciWorkflowsKey), &diags)
		planOK := !diags.HasError()

		got, err := c.UpdateGithubIntegration(ctx, g.ID, client.Fields{ciWorkflowsKey: list}, version)
		var storedAsWritten bool
		switch {
		case err == nil:
			version = got.LockVersion
			storedAsWritten = slices.Equal(got.CIWorkflows, list)
		case client.HasCode(err, client.CodeInvalidAttribute):
		default:
			t.Fatalf("writing %q: %v", list, err)
		}
		if planOK != storedAsWritten {
			var stored []string
			if got != nil {
				stored = got.CIWorkflows
			}
			t.Errorf("%q: the plan passes it: %t, Flightdeck stores it as written: %t (stored %q, refused: %v, plan: %v)",
				list, planOK, storedAsWritten, stored, err, diags)
		}
		checked++
	}
	if checked < 2000 {
		t.Fatalf("only %d lists checked", checked)
	}
}

// The plan has to fold names exactly as Flightdeck's String#downcase(:fold)
// does, or it could pass a list Flightdeck stores differently, and the apply
// would fail with "Provider produced inconsistent result". Flightdeck's Ruby
// (3.4) folds by Unicode 15.0.0. x/text picks its case tables by the Go
// version that builds the provider: Unicode 15.0.0 before go1.27, 17.0.0 from
// go1.27, where 55 code points fold differently. This guards the Go side
// only: Flightdeck's version is a constant here, so a newer Ruby in
// Flightdeck cannot fail it. TestGithubIntegration_ciWorkflowsRoundTrip, run
// live, watches that side. When this fails, compare ciWorkflowFold with
// Ruby's downcase(:fold) for every code point again, fix any difference, and
// update the version here.
func TestCIWorkflowFold_unicodeVersionIsFlightdecks(t *testing.T) {
	const flightdeckRubyUnicodeVersion = "15.0.0"
	if cases.UnicodeVersion != flightdeckRubyUnicodeVersion {
		t.Fatalf("x/text's case tables are Unicode %s, but Flightdeck folds workflow names by Unicode %s: "+
			"check ciWorkflowFold against Ruby's downcase(:fold) for every code point before changing this",
			cases.UnicodeVersion, flightdeckRubyUnicodeVersion)
	}
}

// The fold Flightdeck compares names in is Ruby's downcase(:fold).
func TestCIWorkflowFold(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"CI", "ci"},
		{"Straße", "strasse"},
		{"STRASSE", "strasse"},
		{"\ufb01x ΣΑΣ ς", "fix σασ σ"},
		{"\u0130stanbul", "i\u0307stanbul"},
		{"\u212a", "k"},
		{"\u13a0\uab70\u13f8\u13f0", "\u13a0\u13a0\u13f0\u13f0"},
	} {
		if got := ciWorkflowFold(tc.in); got != tc.want {
			t.Errorf("ciWorkflowFold(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// keepUnsentCIWorkflowsAsPlanned changes the result only when the update did
// not send ci_workflows and the plan knew its value.
func TestKeepUnsentCIWorkflowsAsPlanned(t *testing.T) {
	ci := ciWorkflowsList([]string{"CI"})
	empty := ciWorkflowsList([]string{})
	null := types.ListNull(types.StringType)
	unknown := types.ListUnknown(types.StringType)
	for _, tc := range []struct {
		name          string
		sent          bool
		planned, read types.List
		want          types.List
	}{
		{"not sent, planned null, read []", false, null, empty, null},
		{"not sent, planned [], read null", false, empty, null, empty},
		{"not sent, planned [CI], read []", false, ci, empty, ci},
		{"not sent, not known at plan", false, unknown, empty, empty},
		{"sent, read differs", true, ci, null, null},
		{"sent empty, read null", true, empty, null, null},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := githubIntegrationModel{CIWorkflows: tc.read}
			keepUnsentCIWorkflowsAsPlanned(tc.sent, tc.planned, &result)
			if !result.CIWorkflows.Equal(tc.want) {
				t.Errorf("got %v, want %v", result.CIWorkflows, tc.want)
			}
		})
	}
}

// Only an unknown-key refusal naming ci_workflows says the server is too old,
// and a refusal is about a key only when its message names it outside the
// list of settable keys.
func TestCIWorkflowsRefusals(t *testing.T) {
	refusal := func(code, message string) error {
		return &client.Error{Method: "PATCH", Path: "/github-integrations/4", Status: 422, Code: code, Message: message}
	}
	older := refusal(client.CodeInvalidAttribute, "unknown key: ci_workflows (settable: repo_full_name, secret, enabled, ci_failure_action)")
	several := refusal(client.CodeInvalidAttribute, "unknown keys: ci_workflows, colour (settable: repo_full_name, secret, enabled, ci_failure_action)")
	other := refusal(client.CodeInvalidAttribute, "unknown key: colour (settable: repo_full_name, secret, enabled, ci_failure_action, ci_workflows)")
	entry := refusal(client.CodeInvalidAttribute, "ci_workflows[0] must not be blank")
	action := refusal(client.CodeInvalidAttribute, "ci_failure_action must be one of: off, create_work_item, file_intake (got always)")
	stale := &client.Error{Status: 409, Code: client.CodeStaleObject, Message: "ci_workflows changed"}
	for _, tc := range []struct {
		name               string
		err                error
		unknown, aboutList bool
		aboutAction        bool
	}{
		{"an older Flightdeck", older, true, true, false},
		{"several unknown keys", several, true, true, false},
		{"another unknown key", other, false, false, false},
		{"an entry refused", entry, false, true, false},
		{"the action refused", action, false, false, true},
		{"not a 422", stale, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := refusesCIWorkflowsAsUnknown(tc.err); got != tc.unknown {
				t.Errorf("refusesCIWorkflowsAsUnknown = %t, want %t", got, tc.unknown)
			}
			if got := refusalIsAbout(tc.err, ciWorkflowsKey); got != tc.aboutList {
				t.Errorf("refusalIsAbout(ci_workflows) = %t, want %t", got, tc.aboutList)
			}
			if got := refusalIsAbout(tc.err, "ci_failure_action"); got != tc.aboutAction {
				t.Errorf("refusalIsAbout(ci_failure_action) = %t, want %t", got, tc.aboutAction)
			}
		})
	}
}

// ci_workflows under `lifecycle { ignore_changes = [ci_workflows] }`: a list
// changed on the settings page is kept through an update made for another
// attribute. Terraform applies ignore_changes to the plan only, so the list
// sent is the plan's (the one in state), never the configuration's, and the
// next plan is empty.
func TestGithubIntegration_ciWorkflowsIgnoreChangesKeepsAWebEdit(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	lifecycle := "lifecycle {\n    ignore_changes = [ci_workflows]\n  }"
	changed := ciWorkflowsConfig(env, identifier, repo, `["CI"]`, lifecycle, `ci_failure_action = "file_intake"`)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: ciWorkflowsConfig(env, identifier, repo, `["CI"]`, lifecycle), Check: captureAttr(ghRes, "id", &id)},
			{
				PreConfig: func() { env.fake.SetGithubCIWorkflows(mustInt(id), []string{"CI", "Deploy"}) },
				Config:    changed,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(ghRes, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(ghRes, tfjsonpath.New(ciWorkflowsKey), ciWorkflowsValue([]string{"CI", "Deploy"})),
					},
				},
				ConfigStateChecks: []statecheck.StateCheck{
					ciWorkflowsIs([]string{"CI", "Deploy"}),
					statecheck.ExpectKnownValue(ghRes, tfjsonpath.New("ci_failure_action"), knownvalue.StringExact("file_intake")),
				},
				Check: func(*terraform.State) error {
					if g := env.fake.GithubIntegration(mustInt(id)); !slices.Equal(g.CIWorkflows, []string{"CI", "Deploy"}) {
						return fmt.Errorf("the update overwrote the list saved on the settings page: %#v", g.CIWorkflows)
					}
					return nil
				},
			},
			{Config: changed, PlanOnly: true},
		},
	})
	writes := ciWorkflowsWrites(t, env)
	if last := writes[len(writes)-1]; !reflect.DeepEqual(last[ciWorkflowsKey], []any{"CI", "Deploy"}) {
		t.Errorf("the update must send the list in state, got %v", last)
	}
}

// ci_failure_action has always been sent from the plan, so it keeps a value
// changed on the settings page under ignore_changes the same way.
func TestGithubIntegration_ciFailureActionIgnoreChangesKeepsAWebEdit(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	lifecycle := "lifecycle {\n    ignore_changes = [ci_failure_action]\n  }"
	changed := ciWorkflowsConfig(env, identifier, repo, `["CI"]`, lifecycle, `ci_failure_action = "file_intake"`)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: ciWorkflowsConfig(env, identifier, repo, "", lifecycle, `ci_failure_action = "file_intake"`),
				Check:  captureAttr(ghRes, "id", &id),
			},
			{
				PreConfig: func() { env.fake.SetGithubCIFailureAction(mustInt(id), "create_work_item") },
				Config:    changed,
				ConfigStateChecks: []statecheck.StateCheck{
					ciWorkflowsIs([]string{"CI"}),
					statecheck.ExpectKnownValue(ghRes, tfjsonpath.New("ci_failure_action"), knownvalue.StringExact("create_work_item")),
				},
			},
			{Config: changed, PlanOnly: true},
		},
	})
}

// Replacing the integration (a new repository) plans the new one's list as
// known after apply, even while the old one has a list from the settings
// page, and the new integration starts empty.
func TestGithubIntegration_ciWorkflowsReplaceStartsEmpty(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	renamed := ciWorkflowsConfig(env, identifier, repo+"-renamed", "")
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: ciWorkflowsConfig(env, identifier, repo, ""), Check: captureAttr(ghRes, "id", &id)},
			{
				PreConfig: func() { env.fake.SetGithubCIWorkflows(mustInt(id), []string{"Nightly"}) },
				Config:    renamed,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(ghRes, plancheck.ResourceActionDestroyBeforeCreate),
						plancheck.ExpectUnknownValue(ghRes, tfjsonpath.New(ciWorkflowsKey)),
					},
				},
				ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs([]string{})},
			},
			{Config: renamed, PlanOnly: true},
		},
	})
}

// On a Flightdeck that ignores ci_workflows, an update refused for another
// reason (a second enabled integration on the project, validation_failed) is
// reported as that reason, not as the server being too old for
// ci_workflows: only an invalid_attribute refusal can be an unknown key.
func TestGithubIntegration_ciWorkflowsOtherRefusalOnAnIgnoringFlightdeck(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	env.fake.SetGithubCIWorkflowsSupport(flightdecktest.CIWorkflowsIgnored)
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	var projectID string
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: ciWorkflowsConfig(env, identifier, repo, "", "enabled = false"),
				Check:  captureAttr("flightdeck_project.parent", "id", &projectID),
			},
			{
				// Another integration on the project is enabled first, so
				// enabling this one breaks the one-enabled rule.
				PreConfig: func() {
					c, err := client.New(env.fake.URL, env.fake.Token())
					if err != nil {
						t.Fatal(err)
					}
					if _, err := c.CreateGithubIntegration(context.Background(), mustInt(projectID),
						client.Fields{"repo_full_name": repo + "-other", "secret": ciWorkflowsSecret}, "other"); err != nil {
						t.Fatal(err)
					}
				},
				Config:      ciWorkflowsConfig(env, identifier, repo, `["CI"]`, "enabled = true"),
				ExpectError: wrapped("Cannot enable this integration", "already has an enabled GitHub integration"),
			},
		},
	})
	for _, d := range recorded.bySeverity(tfprotov6.DiagnosticSeverityError) {
		if d.Summary == ciWorkflowsUnsupportedSummary {
			t.Fatalf("a validation_failed refusal was blamed on ci_workflows: %s", d.Detail)
		}
	}
}

// An unknown-key refusal ends with every key the endpoint takes,
// ci_failure_action among them. That list says nothing about what was
// refused, so such a refusal of another key is not reported on
// ci_failure_action, or on any attribute: it is reported as it came.
func TestGithubIntegration_settableListIsNotACIFailureActionRefusal(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	const refusal = "unknown key: colour (settable: repo_full_name, secret, enabled, ci_failure_action, ci_workflows)"
	var id string
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: ciWorkflowsConfig(env, identifier, repo, ""), Check: captureAttr(ghRes, "id", &id)},
			{
				PreConfig: func() {
					env.fake.RefuseNext("PATCH", "/github-integrations/"+id, 422, client.CodeInvalidAttribute, refusal)
				},
				Config:      ciWorkflowsConfig(env, identifier, repo, "", `ci_failure_action = "file_intake"`),
				ExpectError: wrapped(refusal),
			},
		},
	})
	// It is reported as it came, on no attribute: not on ci_failure_action,
	// and not on enabled through the catch-all for other refusals.
	errs := recorded.bySeverity(tfprotov6.DiagnosticSeverityError)
	if len(errs) != 1 || errs[0].Summary != "Error updating Flightdeck GitHub integration" || errs[0].Attribute != nil ||
		!strings.Contains(errs[0].Detail, refusal) {
		t.Fatalf("want one error, \"Error updating Flightdeck GitHub integration\" on no attribute, carrying the refusal; got %s",
			describeWithPaths(errs))
	}
}

// A configured list under ignore_changes, with state from a Flightdeck that
// refuses ci_workflows (so null, and the plan null too): nothing is sent, the
// update for another attribute works, and the next plan is empty.
func TestGithubIntegration_ciWorkflowsIgnoreChangesWithNullStateSendsNothing(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	env.fake.SetGithubCIWorkflowsSupport(flightdecktest.CIWorkflowsUnknownKey)
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	lifecycle := "lifecycle {\n    ignore_changes = [ci_workflows]\n  }"
	changed := ciWorkflowsConfig(env, identifier, repo, `["CI"]`, lifecycle, `ci_failure_action = "file_intake"`)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: ciWorkflowsConfig(env, identifier, repo, ""), ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs(nil)}},
			{
				Config: changed,
				ConfigStateChecks: []statecheck.StateCheck{
					ciWorkflowsIs(nil),
					statecheck.ExpectKnownValue(ghRes, tfjsonpath.New("ci_failure_action"), knownvalue.StringExact("file_intake")),
				},
			},
			{Config: changed, PlanOnly: true},
		},
	})
	for i, w := range ciWorkflowsWrites(t, env) {
		if _, sent := w[ciWorkflowsKey]; sent {
			t.Errorf("write %d sent ci_workflows, which the plan holds as null: %v", i+1, w)
		}
	}
}

// The same, with Flightdeck upgraded to ci_workflows between the plan and the
// apply: the update sent no list, so the apply keeps the plan's null rather
// than fail with "Provider produced inconsistent result", and the next
// refresh adopts the [] Flightdeck now reports without planning anything.
func TestGithubIntegration_ciWorkflowsIgnoreChangesWithNullStateSurvivesAnUpgrade(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	env.fake.SetGithubCIWorkflowsSupport(flightdecktest.CIWorkflowsUnknownKey)
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	lifecycle := "lifecycle {\n    ignore_changes = [ci_workflows]\n  }"
	changed := ciWorkflowsConfig(env, identifier, repo, `["CI"]`, lifecycle, `ci_failure_action = "file_intake"`)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: ciWorkflowsConfig(env, identifier, repo, ""), Check: captureAttr(ghRes, "id", &id)},
			{
				PreConfig: func() {
					env.fake.OnNextRequest("PATCH", "/api/v1/github-integrations/"+id, func() {
						env.fake.SetGithubCIWorkflowsSupport(flightdecktest.CIWorkflowsSupported)
					})
				},
				Config:            changed,
				ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs(nil)},
			},
			{
				Config: changed,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				ConfigStateChecks: []statecheck.StateCheck{ciWorkflowsIs([]string{})},
			},
		},
	})
}

// On a Flightdeck that ignores ci_workflows, an update that sets it and
// changes something else saves the other change and then fails on the list,
// as the attribute's documentation says. The next plan still shows the list.
func TestGithubIntegration_ciWorkflowsIgnoringFlightdeckSavesTheRest(t *testing.T) {
	env := newTestEnv(t, "github_integration")
	env.requireFake(t)
	env.fake.SetGithubCIWorkflowsSupport(flightdecktest.CIWorkflowsIgnored)
	identifier := randIdentifier()
	repo := "example-org/" + strings.ToLower(identifier)
	changed := ciWorkflowsConfig(env, identifier, repo, `["CI"]`, `ci_failure_action = "file_intake"`)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: ciWorkflowsConfig(env, identifier, repo, ""), Check: captureAttr(ghRes, "id", &id)},
			{
				Config:      changed,
				ExpectError: wrapped(ciWorkflowsUnsupportedSummary, "ignored ci_workflows", "The rest of this change was saved."),
			},
			{
				PreConfig: func() {
					if action := env.fake.GithubIntegration(mustInt(id)).CIFailureAction; action != "file_intake" {
						t.Errorf("the other change was not saved: ci_failure_action is %q", action)
					}
				},
				Config:             changed,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}
