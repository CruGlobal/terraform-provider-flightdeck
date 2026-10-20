package provider

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"golang.org/x/text/cases"
)

// ci_workflows on flightdeck_github_integration: the names of the repository's
// GitHub Actions workflows that Flightdeck treats as CI.
//
// Flightdeck trims each name and drops one that repeats an earlier one
// ignoring case, so a configuration like [" CI "] or ["CI", "ci"] would be
// stored as something other than what was written, and Terraform would fail
// the apply with "Provider produced inconsistent result". The plan refuses
// such a list instead (ciWorkflowsValidator), by the API's own rules, so that
// whatever passes is exactly what Flightdeck stores. Terraform runs the check
// again before an apply, once names that were not known at plan time are.
//
// Like ci_failure_action, the attribute is Optional + Computed and sent only
// when configured. A Flightdeck older than the setting has no such key on a
// read, so it reads as null there and is never sent while unset. Setting it
// on such a Flightdeck fails with an error on the attribute that asks for an
// upgrade, whether that Flightdeck refuses the key as unknown or, older
// still, ignores it.

// ciWorkflowsDescription is the attribute's documentation.
const ciWorkflowsDescription = "The names of the repository's GitHub Actions workflows that Flightdeck treats as CI, " +
	"such as `[\"CI\"]`. Each is a workflow's name as GitHub shows it (the `name:` in the workflow file), and " +
	"Flightdeck matches it without regard to letter case. When a listed workflow's first attempt fails on the " +
	"default branch and `ci_failure_action` isn't `off`, Flightdeck asks GitHub to re-run the failed jobs once " +
	"before it files anything, and files a flaky-test item if the re-run passes. Flightdeck also uses the list to " +
	"decide which CI failures it may send to agents without a label, when the project's agent work settings allow " +
	"that. Don't list a deploy workflow: it fails because CI failed, so a re-run fails too.\n\n" +
	"At most 20 names, each up to 100 characters, with no space, tab or line break at either end, no control " +
	"character inside, and no two the same ignoring case. Flightdeck would trim such a name or drop the repeat, " +
	"so the plan refuses it rather than let Flightdeck store something other than what you wrote. `[]` clears the " +
	"list. When unset, the current list is kept, including one saved on the Flightdeck settings page; set it, " +
	"even to `[]`, for Terraform to own it.\n\n" +
	"Needs a Flightdeck that supports CI workflows. On an older one it reads as null and is never sent while it " +
	"is unset. Setting it there fails with an error that asks you to upgrade Flightdeck, and nothing is saved. The " +
	"oldest versions ignore the setting instead of refusing it. There, an update saves its other changes and then " +
	"fails, and the apply that creates the integration links the repository with a warning, so it isn't marked " +
	"tainted, and the next plan shows the list still to apply."

// ciWorkflowsKey is the attribute's name, in the schema and on the wire.
const ciWorkflowsKey = "ci_workflows"

// ciWorkflowsValidator refuses, at plan time, a list Flightdeck would refuse
// or store differently. It reports each problem on the entry it is about.
type ciWorkflowsValidator struct{}

func (ciWorkflowsValidator) Description(context.Context) string {
	return fmt.Sprintf("at most %d workflow names, each up to %d characters, with no space, tab or line break at "+
		"either end, no control character inside, and no two the same ignoring case",
		client.GithubCIWorkflowsMax, client.GithubCIWorkflowNameMaxLength)
}

func (v ciWorkflowsValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (ciWorkflowsValidator) ValidateList(_ context.Context, req validator.ListRequest, resp *validator.ListResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	checkCIWorkflows(req.ConfigValue.Elements(), req.Path, &resp.Diagnostics)
}

// rubyStripped is what Ruby's String#strip, which Flightdeck trims names
// with, removes from either end: ASCII white space and NUL. It leaves a
// no-break space, so Flightdeck keeps one at either end of a name as it is.
const rubyStripped = "\x00\t\n\v\f\r "

// checkCIWorkflows reports every way the list breaks Flightdeck's rules, or
// would be stored differently, as errors at the entry (or, for the count, the
// list) under at. Entries not yet known are skipped.
func checkCIWorkflows(elems []attr.Value, at path.Path, diags *diag.Diagnostics) {
	if len(elems) > client.GithubCIWorkflowsMax {
		diags.AddAttributeError(at, "Too many CI workflows",
			fmt.Sprintf("ci_workflows lists %d workflows. Flightdeck takes at most %d.", len(elems), client.GithubCIWorkflowsMax))
	}
	type first struct {
		index int
		name  string
	}
	seen := map[string]first{}
	for i, e := range elems {
		if e.IsUnknown() {
			continue
		}
		entry := at.AtListIndex(i)
		if e.IsNull() {
			diags.AddAttributeError(entry, "Invalid CI workflow name",
				fmt.Sprintf("ci_workflows[%d] is null. Each entry must be the name of a workflow.", i))
			continue
		}
		s, ok := e.(types.String)
		if !ok {
			continue
		}
		name := s.ValueString()
		if problem := ciWorkflowNameProblem(name); problem != "" {
			diags.AddAttributeError(entry, "Invalid CI workflow name", fmt.Sprintf("ci_workflows[%d] %s", i, problem))
		}
		if isRailsBlank(name) {
			continue
		}
		key := ciWorkflowFold(strings.Trim(name, rubyStripped))
		if earlier, repeated := seen[key]; repeated {
			diags.AddAttributeError(entry, "Repeated CI workflow name",
				fmt.Sprintf("ci_workflows[%d] (%q) repeats ci_workflows[%d] (%q). Flightdeck compares workflow names "+
					"without regard to letter case and keeps only the first, so the next plan would show a change. "+
					"Remove one of them.", i, name, earlier.index, earlier.name))
			continue
		}
		seen[key] = first{index: i, name: name}
	}
}

// ciWorkflowNameProblem says what is wrong with one name, finishing a
// sentence that starts "ci_workflows[i]", or "" when Flightdeck would store it
// exactly as written.
func ciWorkflowNameProblem(name string) string {
	if isRailsBlank(name) {
		return "is blank. Each entry must be the name of a workflow."
	}
	trimmed := strings.Trim(name, rubyStripped)
	// Flightdeck refuses a control character inside the trimmed name. A NUL
	// at either end it neither refuses nor trims away (it stores a marker in
	// its place), so a NUL anywhere is refused here.
	if strings.IndexFunc(trimmed, isCIWorkflowControl) >= 0 || strings.ContainsRune(name, 0) {
		return fmt.Sprintf("(%q) has a control character, such as a line break or a tab, inside it. Flightdeck "+
			"refuses these: a workflow name is one line of plain text.", name)
	}
	if n := utf8.RuneCountInString(trimmed); n > client.GithubCIWorkflowNameMaxLength {
		return fmt.Sprintf("is %d characters long. Flightdeck takes names of up to %d characters.", n, client.GithubCIWorkflowNameMaxLength)
	}
	if trimmed != name {
		return fmt.Sprintf("(%q) starts or ends with a space, tab or line break. Flightdeck trims names, so it would "+
			"store %q and the next plan would show a change. Write %q instead.", name, trimmed, trimmed)
	}
	return ""
}

// isRailsBlank is Flightdeck's blank (Rails' String#blank?): empty, or
// nothing but Unicode white space, a no-break space included. Unlike isBlank,
// a NUL is not blank.
func isRailsBlank(s string) bool { return strings.TrimFunc(s, unicode.IsSpace) == "" }

// isCIWorkflowControl is Flightdeck's control-character rule for a workflow
// name: any C0 or C1 control character, DEL, and the Unicode line and
// paragraph separators.
func isCIWorkflowControl(r rune) bool {
	return r <= 0x1f || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029
}

// ciWorkflowFold is the form Flightdeck compares two names in: Ruby's
// String#downcase(:fold), full Unicode case folding, so "Straße" and
// "STRASSE" are one name. x/text's folding agrees with Ruby's for every code
// point but the Cherokee capitals, which Unicode folds to themselves and
// x/text folds to the small letters; mapping the small letters back to the
// capitals, as Unicode folds them, puts that right.
func ciWorkflowFold(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 0xab70 && r <= 0xabbf:
			return r - 0xab70 + 0x13a0
		case r >= 0x13f8 && r <= 0x13fd:
			return r - 0x13f8 + 0x13f0
		}
		return r
	}, cases.Fold().String(s))
}

// ciWorkflowsList is the attribute's value for what Flightdeck reported: the
// list in its stored order, or null from a Flightdeck without the setting.
func ciWorkflowsList(names []string) types.List {
	if names == nil {
		return types.ListNull(types.StringType)
	}
	elems := make([]attr.Value, 0, len(names))
	for _, name := range names {
		elems = append(elems, types.StringValue(name))
	}
	return types.ListValueMust(types.StringType, elems)
}

// ciWorkflowsToSend returns the list to send, and whether to send it.
//
// The configuration decides WHETHER: only a list the configuration sets is
// sent, because the plan of an unset ci_workflows is the value in state, and
// sending that would put the key in front of an older Flightdeck that refuses
// it. The plan decides WHAT: it equals the configuration, except under
// `lifecycle { ignore_changes = [ci_workflows] }`, where it is the value in
// state. Terraform applies ignore_changes to the plan only, never to the
// configuration it hands the apply, so sending the configuration there would
// overwrite a list changed on the settings page and fail the apply with
// "Provider produced inconsistent result".
//
// An empty list is sent as [] (never null, which Flightdeck reads as no
// opinion), since [] is how the list is cleared. Names not known at plan time
// need no check here: Terraform validates the configuration again once every
// value is known, before it applies.
func ciWorkflowsToSend(configured, planned types.List) (names []string, send bool) {
	if configured.IsNull() || configured.IsUnknown() || planned.IsNull() || planned.IsUnknown() {
		return nil, false
	}
	elems := planned.Elements()
	names = make([]string, 0, len(elems))
	for _, e := range elems {
		if s, isString := e.(types.String); isString {
			names = append(names, s.ValueString())
		}
	}
	return names, true
}

// keepUnsentCIWorkflowsAsPlanned puts the planned ci_workflows in the state
// an update leaves, when the update did not send it and the plan knew its
// value. That is a list the configuration leaves unset, and also a configured
// one under ignore_changes whose plan, taken from state, is null (state from
// a Flightdeck without the setting): ciWorkflowsToSend sends neither. Such an
// update changes nothing about the list, but what Flightdeck reports can still
// differ from the plan: one upgraded to the setting between the plan and the
// apply reports [] where the plan, made from a read without the key, holds
// null, and one rolled back below it reports nothing where the plan holds a
// list. A saved plan applied later makes that window real. Terraform refuses a
// result that differs from a known planned value, so the planned one is kept,
// and the next refresh adopts Flightdeck's. A list that was sent is left as
// Flightdeck reports it.
func keepUnsentCIWorkflowsAsPlanned(sent bool, planned types.List, result *githubIntegrationModel) {
	if !sent && !planned.IsUnknown() {
		result.CIWorkflows = planned
	}
}

// refusesCIWorkflowsAsUnknown reports whether a refusal names ci_workflows as
// an unknown key, as a Flightdeck older than the setting words it.
func refusesCIWorkflowsAsUnknown(err error) bool {
	for _, key := range refusalUnknownKeys(err) {
		if key == ciWorkflowsKey {
			return true
		}
	}
	return false
}

// refusalIsAbout reports whether a 422's message names key, leaving out the
// list of settable keys an unknown-key refusal ends with (which names every
// key the endpoint takes, whatever was refused).
func refusalIsAbout(err error, key string) bool {
	return client.IsValidation(err) && strings.Contains(withoutSettableList(apiMessage(err)), key)
}

// ciWorkflowsUnsupportedSummary heads every report of a Flightdeck too old
// for ci_workflows.
const ciWorkflowsUnsupportedSummary = "This Flightdeck does not support ci_workflows yet"

// ciWorkflowsUpgrade is what to do about it.
const ciWorkflowsUpgrade = "Upgrade Flightdeck first, then apply again. Until then, leave `ci_workflows` out of " +
	"this resource: on this Flightdeck it reads as null, and the provider never sends it while it is unset."

// ciWorkflowsTooOld says why this Flightdeck doesn't take the key.
const ciWorkflowsTooOld = "it is older than the release that added CI workflows to GitHub integrations."

// addCIWorkflowsRefusedAsUnknown reports a write that Flightdeck refused for
// naming ci_workflows, which it does not know. It refused the whole write, so
// nothing was saved; on a create, nothing was linked.
func addCIWorkflowsRefusedAsUnknown(err error, onCreate bool, diags *diag.Diagnostics) {
	nothing := "Nothing was saved, including this resource's other changes, so the next plan still shows them."
	if onCreate {
		nothing = "Nothing was created: the repository is not linked."
	}
	diags.AddAttributeError(path.Root(ciWorkflowsKey), ciWorkflowsUnsupportedSummary,
		"This Flightdeck refused ci_workflows as an unknown setting: "+ciWorkflowsTooOld+" "+nothing+"\n\n"+
			ciWorkflowsUpgrade+"\n\nThe API said: "+apiMessage(err))
}

// addCIWorkflowsIgnored reports a write that Flightdeck took without
// ci_workflows: one older still, from before the GitHub integration resource
// refused keys it doesn't know, saves the rest and drops the list. On a
// create that is a warning, so the new integration isn't marked tainted, and
// the next plan shows the list again; on an update it is an error.
func addCIWorkflowsIgnored(onCreate bool, diags *diag.Diagnostics) {
	at := path.Root(ciWorkflowsKey)
	if onCreate {
		diags.AddAttributeWarning(at, ciWorkflowsUnsupportedSummary,
			"The repository was linked, but this Flightdeck ignored ci_workflows: "+ciWorkflowsTooOld+" The next "+
				"plan shows the list again, and the next apply fails on this until Flightdeck is upgraded or "+
				"`ci_workflows` is left out of this resource.")
		return
	}
	diags.AddAttributeError(at, ciWorkflowsUnsupportedSummary,
		"This Flightdeck ignored ci_workflows: "+ciWorkflowsTooOld+" The rest of this change was saved.\n\n"+
			ciWorkflowsUpgrade)
}

// ciWorkflowsRefusalUnsupported reports whether a refused update that sent
// ci_workflows failed because this Flightdeck is too old for it. Only an
// invalid_attribute refusal can be that: it is the code an unknown key gets.
// Any other 422, such as validation_failed for a second enabled integration
// on the project, is about something else, even from a Flightdeck that is
// too old (one that ignores the key refuses for its own reasons). Then a read
// made after the refusal decides, not the API's wording, as for
// agent_work.auto_merge: a Flightdeck that knows ci_workflows always has the
// key. When that read fails, the refusal's own words decide.
func ciWorkflowsRefusalUnsupported(ctx context.Context, c *client.Client, id int64, err error) bool {
	if !client.IsValidation(err) || !client.HasCode(err, client.CodeInvalidAttribute) {
		return false
	}
	if fresh, rerr := c.GetGithubIntegration(ctx, id); rerr == nil {
		return !fresh.ReportsCIWorkflows()
	}
	return refusesCIWorkflowsAsUnknown(err)
}
