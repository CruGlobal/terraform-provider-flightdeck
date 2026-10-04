package provider

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	datasourceschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/float64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The agent_work block rides its own API resource, GET/PATCH
// /api/v1/projects/:project_id/agent-work, workspace-admin for both: whether
// Flightdeck's AI agents may take the project's labelled work items, and how
// much they may do. It is modelled as a block on flightdeck_project, like
// self_healing, because it configures that project. Three things about the
// API shape it:
//
//   - The settings are their OWN ROW, so the If-Match is that row's
//     lock_version (reported as agent_work.lock_version), not the project's,
//     and a write here never bumps the project's. A project whose settings were
//     never saved reads the defaults at version 0; the first write that
//     changes something creates the row at 1.
//   - The write MERGES and reads a blank as "no opinion" — except label_id and
//     agent_account_id, where a blank or null CLEARS the choice. So only the
//     configured settings are ever sent, and an unset label_id or
//     agent_account_id is left out of the write rather than sent as null:
//     otherwise every apply that did not mention them would clear them.
//   - `blockers` (what would stop Flightdeck sending work right now) changes
//     with the project and the day, like self-healing's rollback_blockers. It
//     is reported as warnings on refresh and apply while agent work is on,
//     never stored.
//
// Every attribute is Optional+Computed or Computed, for the reason the
// slack_channel block gives: an Optional-only member nulled by dropping the
// block would cascade into "known after apply" across the project.

// agentWorkModel is the Terraform shape of the block.
type agentWorkModel struct {
	Enabled             types.Bool    `tfsdk:"enabled"`
	Kinds               types.Set     `tfsdk:"kinds"`
	LabelID             types.Int64   `tfsdk:"label_id"`
	LabelChosenAt       types.String  `tfsdk:"label_chosen_at"`
	AcceptMachineLabels types.Bool    `tfsdk:"accept_machine_labels"`
	AgentAccountID      types.Int64   `tfsdk:"agent_account_id"`
	BaseRef             types.String  `tfsdk:"base_ref"`
	MaxInProgress       types.Int64   `tfsdk:"max_in_progress"`
	DailyBudgetUSD      types.Float64 `tfsdk:"daily_budget_usd"`
	TaskMaxUSD          types.Float64 `tfsdk:"task_max_usd"`
	TaskMaxMinutes      types.Int64   `tfsdk:"task_max_minutes"`
	QueueMinutes        types.Int64   `tfsdk:"queue_minutes"`
	Runbook             types.String  `tfsdk:"runbook"`
	LockVersion         types.Int64   `tfsdk:"lock_version"`
}

var agentWorkAttrTypes = map[string]attr.Type{
	"enabled":               types.BoolType,
	"kinds":                 types.SetType{ElemType: types.StringType},
	"label_id":              types.Int64Type,
	"label_chosen_at":       types.StringType,
	"accept_machine_labels": types.BoolType,
	"agent_account_id":      types.Int64Type,
	"base_ref":              types.StringType,
	"max_in_progress":       types.Int64Type,
	"daily_budget_usd":      types.Float64Type,
	"task_max_usd":          types.Float64Type,
	"task_max_minutes":      types.Int64Type,
	"queue_minutes":         types.Int64Type,
	"runbook":               types.StringType,
	"lock_version":          types.Int64Type,
}

// The API's ranges for the settings it holds to one.
const (
	agentWorkMaxInProgress  = 10
	agentWorkMaxDailyBudget = 1000
	agentWorkMaxTaskCost    = 100
	agentWorkMaxTaskMinutes = 240
	agentWorkMaxQueue       = 1440
)

// noWhitespace refuses a blank or space-padded value. The API reads a blank as
// "no opinion", so a configured "" would be dropped and the apply would end in
// an inconsistent result instead of an error.
var noWhitespace = regexp.MustCompile(`^\S+$`)

// agentWorkSchema is the resource attribute. Nothing carries a framework
// default: a default would manufacture a value for an unset attribute and
// write it, which on a merging endpoint overrides whatever the project has.
func agentWorkSchema() schema.Attribute {
	whole := func(desc string, upper int64) schema.Attribute {
		return schema.Int64Attribute{
			MarkdownDescription: desc + fmt.Sprintf(" A whole number from 1 to %d. When unset, the project's current value is kept.", upper),
			Optional:            true,
			Computed:            true,
			Validators:          []validator.Int64{int64validator.Between(1, upper)},
			PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
		}
	}
	money := func(desc string, upper float64) schema.Attribute {
		return schema.Float64Attribute{
			MarkdownDescription: desc + fmt.Sprintf(" More than 0 and at most %g, with **at most two decimal places**: "+
				"Flightdeck refuses more rather than rounding, so the plan does too. When unset, the project's current value is kept.", upper),
			Optional:      true,
			Computed:      true,
			Validators:    []validator.Float64{moneyValidator{max: upper}},
			PlanModifiers: []planmodifier.Float64{float64planmodifier.UseStateForUnknown()},
		}
	}
	reference := func(desc string) schema.Attribute {
		return schema.Int64Attribute{
			MarkdownDescription: desc,
			Optional:            true,
			Computed:            true,
			Validators:          []validator.Int64{int64validator.AtLeast(1)},
			PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
		}
	}
	return schema.SingleNestedAttribute{
		MarkdownDescription: "Agent work settings, managed through the project's `agent-work` API resource: whether " +
			"AutoPilot, the AI agents Flightdeck sends work to, may take this project's work items, which label marks an " +
			"item for them, the service account they work as, and how much they may spend and run. An agent works on a " +
			"copy of the project's GitHub repository and opens a pull request for a person to review. Reading and " +
			"writing these settings requires the token's user to be a **workspace owner or admin**; for other tokens, " +
			"and on a Flightdeck version without the endpoint, the block is null.\n\n" +
			"Agent work is off until `enabled` turns it on. A plan that turns it on carries a warning saying so, and " +
			"while it is on, each thing that stops Flightdeck sending work right now (the API's `blockers`: no " +
			"label chosen, no linked GitHub repository, the day's budget spent, and so on) is shown as a warning on " +
			"refresh and after apply rather than stored, because it changes as the project does.\n\n" +
			"The endpoint merges, so this block only ever sends what you configure, and a setting you never name keeps " +
			"whatever the project has, including a value changed on the settings page. `label_id` and " +
			"`agent_account_id` are never sent unless configured, because the API reads an empty value for either as " +
			"\"clear it\"; removing one from configuration therefore leaves the stored choice in place (clear it on " +
			"the project's **Agent work** settings page).\n\n" +
			"These settings have their own `lock_version`, separate from the project's: a write here never conflicts " +
			"with a project update and does not bump the project's `lock_version`.",
		Optional: true,
		Computed: true,
		PlanModifiers: []planmodifier.Object{
			objectplanmodifier.UseStateForUnknown(),
		},
		Attributes: map[string]schema.Attribute{
			"enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether AutoPilot agents may take this project's work items. Off by default. Turning it " +
					"on lets agents take each work item a person marks with the agent label and open pull requests " +
					"for it, as soon as nothing in `blockers` stands in the way; a plan that turns it on warns. " +
					"Flightdeck posts the change to the project's Slack updates. When unset, the project's current " +
					"value is kept; set it explicitly, even to `false`, for Terraform to own it.",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"kinds": schema.SetAttribute{
				MarkdownDescription: "The kinds of work agents may do. Known kinds: `" + joinBackticked(client.AgentWorkKinds) +
					"`. None by default, and an empty set clears the list. When unset, the project's current list is kept.",
				ElementType:   types.StringType,
				Optional:      true,
				Computed:      true,
				Validators:    []validator.Set{setvalidator.ValueStringsAre(stringvalidator.OneOf(client.AgentWorkKinds...))},
				PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()},
			},
			"label_id": reference("Id of the label in this project that marks a work item for an agent. Only a label a " +
				"person adds after it is chosen here counts (see `label_chosen_at`), unless `accept_machine_labels` " +
				"says otherwise. When unset, the stored label is kept; it is never sent empty, because the API would " +
				"read that as clearing it.\n\n" +
				"A `flightdeck_label` in this project depends on the project, so referring to it here forms a " +
				"dependency cycle. For a project that already exists, give the label its `project_id` from a " +
				"`flightdeck_project` **data source** looked up by `identifier`, which breaks the cycle; otherwise create " +
				"the label first and set the id afterwards."),
			"label_chosen_at": schema.StringAttribute{
				MarkdownDescription: "When `label_id` was last set to a label (RFC 3339), or null while none is chosen. Only " +
					"a label a person adds after this time counts, so renaming an old label to the agent label brings " +
					"none of its items along. Read-only: Flightdeck sets it whenever `label_id` changes.",
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"accept_machine_labels": schema.BoolAttribute{
				MarkdownDescription: "Whether the agent label counts even when a machine added it (an automation rule, an " +
					"import, a service account). Off by default, so only a label a person adds sends an item to an agent. " +
					"When unset, the project's current value is kept.",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"agent_account_id": reference("User id of the workspace **service account** agents work as: a claimed item " +
				"is assigned to it while an agent works on it, so it also needs a project role that can edit work " +
				"items (a `flightdeck_project_member`). A `flightdeck_workspace_member` data source resolves one from " +
				"its email address (service accounts are visible to workspace admins, and its `kind` is `service`); " +
				"the id is also on the workspace's **Service accounts** settings page. When unset, the stored account " +
				"is kept; it is never sent empty, because the API would read that as clearing it."),
			"base_ref": schema.StringAttribute{
				MarkdownDescription: "The branch agents start from. Default `main`. Must be a branch name Git allows. " +
					"When unset, the project's current value is kept.",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 255),
					stringvalidator.RegexMatches(noWhitespace, "must be a branch name, with no spaces"),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"max_in_progress":  whole("How many items agents may work on at once (default 1).", agentWorkMaxInProgress),
			"daily_budget_usd": money("The most agent work may cost this project in one UTC day, in US dollars (default 10).", agentWorkMaxDailyBudget),
			"task_max_usd": money("The most one task may cost, in US dollars (default 5). It may not exceed "+
				"`daily_budget_usd`, which Flightdeck checks against the stored budget when only one of the two is set.", agentWorkMaxTaskCost),
			"task_max_minutes": whole("The most minutes one task may run (default 30).", agentWorkMaxTaskMinutes),
			"queue_minutes":    whole("How many minutes a task may wait to start before it is dropped (default 60).", agentWorkMaxQueue),
			"runbook": schema.StringAttribute{
				MarkdownDescription: "The steps the agent follows. Known runbooks: `" + joinBackticked(client.AgentWorkRunbooks) +
					"` (the default). When unset, the project's current value is kept.",
				Optional:      true,
				Computed:      true,
				Validators:    []validator.String{stringvalidator.OneOf(client.AgentWorkRunbooks...)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"lock_version": schema.Int64Attribute{
				MarkdownDescription: "Optimistic-locking version of these settings, separate from the project's. 0 while " +
					"the project's settings were never saved. Sent as `If-Match` on every write here.",
				Computed:      true,
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
		},
	}
}

// agentWorkDataSourceSchema is the data source's read-only copy of the block.
func agentWorkDataSourceSchema() datasourceschema.Attribute {
	return datasourceschema.SingleNestedAttribute{
		MarkdownDescription: "Agent work settings (whether AutoPilot agents may take the project's work items, and " +
			"how much), read from the project's `agent-work` API resource. Null unless the token's user is a " +
			"workspace owner or admin and the Flightdeck version exposes the endpoint.",
		Computed: true,
		Attributes: map[string]datasourceschema.Attribute{
			"enabled":               datasourceschema.BoolAttribute{MarkdownDescription: "Whether agents may take the project's work items.", Computed: true},
			"kinds":                 datasourceschema.SetAttribute{MarkdownDescription: "The kinds of work agents may do.", ElementType: types.StringType, Computed: true},
			"label_id":              datasourceschema.Int64Attribute{MarkdownDescription: "Id of the label that marks an item for an agent, if one is chosen.", Computed: true},
			"label_chosen_at":       datasourceschema.StringAttribute{MarkdownDescription: "When `label_id` was last set to a label (RFC 3339).", Computed: true},
			"accept_machine_labels": datasourceschema.BoolAttribute{MarkdownDescription: "Whether the agent label counts when a machine added it.", Computed: true},
			"agent_account_id":      datasourceschema.Int64Attribute{MarkdownDescription: "User id of the service account agents work as, if one is chosen.", Computed: true},
			"base_ref":              datasourceschema.StringAttribute{MarkdownDescription: "The branch agents start from.", Computed: true},
			"max_in_progress":       datasourceschema.Int64Attribute{MarkdownDescription: "How many items agents may work on at once.", Computed: true},
			"daily_budget_usd":      datasourceschema.Float64Attribute{MarkdownDescription: "The most agent work may cost the project in one UTC day, in US dollars.", Computed: true},
			"task_max_usd":          datasourceschema.Float64Attribute{MarkdownDescription: "The most one task may cost, in US dollars.", Computed: true},
			"task_max_minutes":      datasourceschema.Int64Attribute{MarkdownDescription: "The most minutes one task may run.", Computed: true},
			"queue_minutes":         datasourceschema.Int64Attribute{MarkdownDescription: "How many minutes a task may wait to start.", Computed: true},
			"runbook":               datasourceschema.StringAttribute{MarkdownDescription: "The steps the agent follows.", Computed: true},
			"lock_version":          datasourceschema.Int64Attribute{MarkdownDescription: "Optimistic-locking version of these settings, separate from the project's.", Computed: true},
		},
	}
}

// moneyValidator holds a US-dollar amount to the API's rule: more than 0, at
// most max, and at most two decimal places. The API refuses a third decimal
// place rather than rounding it, so the plan does too. The check reads the
// shortest decimal form of the float64 the provider will send, which is what
// the API parses: 10.005 has three places however it is spelled in HCL.
type moneyValidator struct{ max float64 }

func (v moneyValidator) Description(context.Context) string {
	return fmt.Sprintf("must be more than 0 and at most %g, with at most two decimal places", v.max)
}

func (v moneyValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (v moneyValidator) ValidateFloat64(_ context.Context, req validator.Float64Request, resp *validator.Float64Response) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	amount := req.ConfigValue.ValueFloat64()
	switch {
	case math.IsNaN(amount) || amount <= 0:
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid amount",
			fmt.Sprintf("must be more than 0, got %s.", formatMoney(amount)))
	case amount > v.max:
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid amount",
			fmt.Sprintf("must be at most %g, got %s.", v.max, formatMoney(amount)))
	case decimalPlaces(amount) > 2:
		resp.Diagnostics.AddAttributeError(req.Path, "Too many decimal places",
			fmt.Sprintf("%s has more than two decimal places. Flightdeck refuses an amount with more rather than rounding it, "+
				"so give it in whole cents.", formatMoney(amount)))
	}
}

// formatMoney is the shortest decimal form of a float64: the form the
// provider sends and the API reads.
func formatMoney(amount float64) string { return strconv.FormatFloat(amount, 'f', -1, 64) }

// decimalPlaces counts the digits after the point in formatMoney's form.
func decimalPlaces(amount float64) int {
	s := formatMoney(amount)
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return 0
	}
	return len(s) - dot - 1
}

// validateAgentWorkConfig checks the cross-field rule the API enforces (a task
// may not cost more than the day) when both sides are configured. The API
// checks it against the MERGED row, so a write naming one side can still be
// refused by the other's stored value; warnAgentWorkBudget looks for that.
func validateAgentWorkConfig(ctx context.Context, block types.Object, diags *diag.Diagnostics) {
	if block.IsNull() || block.IsUnknown() {
		return
	}
	var m agentWorkModel
	diags.Append(block.As(ctx, &m, objectAsOptions)...)
	if !known(m.TaskMaxUSD) || !known(m.DailyBudgetUSD) {
		return
	}
	if m.TaskMaxUSD.ValueFloat64() > m.DailyBudgetUSD.ValueFloat64() {
		diags.AddAttributeError(path.Root("agent_work").AtName("task_max_usd"), "A task may not cost more than the day",
			fmt.Sprintf("task_max_usd (%s) cannot be more than daily_budget_usd (%s).",
				formatMoney(m.TaskMaxUSD.ValueFloat64()), formatMoney(m.DailyBudgetUSD.ValueFloat64())))
	}
}

// warnAgentWorkBudget checks task_max_usd <= daily_budget_usd against the
// MERGED pair the plan carries when only one side is configured: an unset side
// is Optional+Computed with UseStateForUnknown, so the plan holds the stored
// value the API will merge into. It warns rather than errors because that
// stored value is only as current as the last refresh, the same reasoning as
// the self-healing windows.
func warnAgentWorkBudget(ctx context.Context, configBlock, planBlock types.Object, diags *diag.Diagnostics) {
	if configBlock.IsNull() || configBlock.IsUnknown() || planBlock.IsNull() || planBlock.IsUnknown() {
		return
	}
	var cfg, planned agentWorkModel
	diags.Append(configBlock.As(ctx, &cfg, objectAsOptions)...)
	diags.Append(planBlock.As(ctx, &planned, objectAsOptions)...)
	if diags.HasError() {
		return
	}
	taskSet, daySet := known(cfg.TaskMaxUSD), known(cfg.DailyBudgetUSD)
	if taskSet == daySet || !known(planned.TaskMaxUSD) || !known(planned.DailyBudgetUSD) {
		return
	}
	task, day := planned.TaskMaxUSD.ValueFloat64(), planned.DailyBudgetUSD.ValueFloat64()
	if task <= day {
		return
	}
	at, set, setValue, held, heldValue := "task_max_usd", "task_max_usd", task, "daily_budget_usd", day
	if daySet {
		at, set, setValue, held, heldValue = "daily_budget_usd", "daily_budget_usd", day, "task_max_usd", task
	}
	diags.AddAttributeWarning(path.Root("agent_work").AtName(at), "Agent work limits will not be coherent",
		fmt.Sprintf("This configuration sets %s to %s, and the project's stored %s is %s. A task may not cost more than "+
			"the day, and Flightdeck checks that against the stored value too, so it will refuse this with a 422 during "+
			"apply.\n\nSet both in configuration. This is a warning rather than an error because the stored value comes "+
			"from the last refresh.", set, formatMoney(setValue), held, formatMoney(heldValue)))
}

// planAgentWorkComputed keeps the two computed attributes honest in a plan.
// Both carry UseStateForUnknown, so a plan would otherwise promise their prior
// values: lock_version moves on whenever a write changes a setting, and
// label_chosen_at whenever label_id changes. Each becomes unknown exactly
// then, so an apply that changes nothing here plans nothing here.
func planAgentWorkComputed(ctx context.Context, priorBlock, planBlock types.Object, diags *diag.Diagnostics) types.Object {
	if planBlock.IsNull() || planBlock.IsUnknown() {
		return planBlock
	}
	var planned, prior agentWorkModel
	diags.Append(planBlock.As(ctx, &planned, objectAsOptions)...)
	priorKnown := !priorBlock.IsNull() && !priorBlock.IsUnknown()
	if priorKnown {
		diags.Append(priorBlock.As(ctx, &prior, objectAsOptions)...)
	}
	if diags.HasError() {
		return planBlock
	}
	if !priorKnown || !sameAgentWorkSettings(planned, prior) {
		planned.LockVersion = types.Int64Unknown()
	}
	if !priorKnown || planned.LabelID.IsUnknown() || !planned.LabelID.Equal(prior.LabelID) {
		planned.LabelChosenAt = types.StringUnknown()
	}
	obj, d := types.ObjectValueFrom(ctx, agentWorkAttrTypes, planned)
	diags.Append(d...)
	return obj
}

// sameAgentWorkSettings reports whether every setting is known and equal in
// both. Money is compared as the float64 sent, since a configured amount
// carries more precision than the API's answer.
func sameAgentWorkSettings(a, b agentWorkModel) bool {
	sameFloat := func(x, y types.Float64) bool {
		if x.IsUnknown() || y.IsUnknown() || x.IsNull() != y.IsNull() {
			return false
		}
		return x.IsNull() || x.ValueFloat64() == y.ValueFloat64()
	}
	same := func(x, y attr.Value) bool { return !x.IsUnknown() && !y.IsUnknown() && x.Equal(y) }
	return same(a.Enabled, b.Enabled) && same(a.Kinds, b.Kinds) && same(a.LabelID, b.LabelID) &&
		same(a.AcceptMachineLabels, b.AcceptMachineLabels) && same(a.AgentAccountID, b.AgentAccountID) &&
		same(a.BaseRef, b.BaseRef) && same(a.MaxInProgress, b.MaxInProgress) &&
		sameFloat(a.DailyBudgetUSD, b.DailyBudgetUSD) && sameFloat(a.TaskMaxUSD, b.TaskMaxUSD) &&
		same(a.TaskMaxMinutes, b.TaskMaxMinutes) && same(a.QueueMinutes, b.QueueMinutes) && same(a.Runbook, b.Runbook)
}

// known reports a known, non-null value.
func known(v attr.Value) bool { return !v.IsNull() && !v.IsUnknown() }

// agentWorkActs is what the plan-time warning says agents will do.
const agentWorkActs = "AutoPilot agents may then take each of this project's work items that a person marks with the " +
	"agent label, work on a copy of its GitHub repository as the agent account, and open pull requests for people " +
	"to review, within the daily budget and the per-task limits."

// warnAgentWorkEnabled is the plan-time warning for an apply that turns agent
// work on: one that moves `enabled` to true, or may (a value not known until
// apply). priorBlock is null on create. The summary names the project,
// because Terraform folds warnings that share a summary into one.
func warnAgentWorkEnabled(ctx context.Context, identifier types.String, configBlock, priorBlock, planBlock types.Object, diags *diag.Diagnostics) {
	if planBlock.IsNull() || planBlock.IsUnknown() {
		return
	}
	var cfg, planned, prior agentWorkModel
	diags.Append(planBlock.As(ctx, &planned, objectAsOptions)...)
	if !priorBlock.IsNull() && !priorBlock.IsUnknown() {
		diags.Append(priorBlock.As(ctx, &prior, objectAsOptions)...)
	}
	if !configBlock.IsNull() && !configBlock.IsUnknown() {
		diags.Append(configBlock.As(ctx, &cfg, objectAsOptions)...)
	}
	if diags.HasError() {
		return
	}
	isOn := func(v types.Bool) bool { return known(v) && v.ValueBool() }
	if isOn(prior.Enabled) {
		return
	}
	project := "this project"
	if known(identifier) {
		project = "project " + identifier.ValueString()
	}
	at := path.Root("agent_work").AtName("enabled")
	switch {
	case cfg.Enabled.IsUnknown() && !configBlock.IsNull() && !configBlock.IsUnknown():
		diags.AddAttributeWarning(at, "This apply may turn on agent work for "+project,
			fmt.Sprintf("agent_work.enabled for %s is not known until apply. If it is true, this apply lets AutoPilot "+
				"agents take this project's labelled work items and open pull requests. %s", project, agentWorkActs))
	case isOn(planned.Enabled):
		diags.AddAttributeWarning(at, "This apply turns on agent work for "+project,
			fmt.Sprintf("This apply lets AutoPilot agents take %s's labelled work items and open pull requests. %s "+
				"Flightdeck starts sending work as soon as nothing blocks it; anything that still does is shown as a "+
				"warning after the apply.", project, agentWorkActs))
	}
}

// warnAgentWorkBlockers reports, one warning each, what would stop Flightdeck
// sending a project's work right now, while agent work is on. They are
// warnings rather than state because they change as the project does. Each
// summary names the project and the blocker's code, because Terraform folds
// warnings that share a summary into one.
func warnAgentWorkBlockers(aw *client.AgentWork, identifier string, diags *diag.Diagnostics) {
	if aw == nil || !aw.Enabled {
		return
	}
	seen := map[string]int{}
	for _, b := range aw.Blockers {
		seen[b.Code]++
		summary := fmt.Sprintf("Agent work cannot start on project %s right now: %s", identifier, b.Code)
		if n := seen[b.Code]; n > 1 {
			summary += fmt.Sprintf(" (%d)", n)
		}
		diags.AddAttributeWarning(path.Root("agent_work").AtName("enabled"), summary,
			b.Message+"\n\nAgent work is on for this project, and this is one of the things stopping Flightdeck "+
				"sending it work. Flightdeck checks again every time the settings are read, so this is reported on "+
				"each refresh and apply rather than stored in state.")
	}
}

// agentWorkToObject maps the API's settings into the block.
func agentWorkToObject(ctx context.Context, aw *client.AgentWork, diags *diag.Diagnostics) types.Object {
	if aw == nil {
		return types.ObjectNull(agentWorkAttrTypes)
	}
	kinds := aw.Kinds
	if kinds == nil {
		kinds = []string{}
	}
	set, d := types.SetValueFrom(ctx, types.StringType, kinds)
	diags.Append(d...)
	obj, d := types.ObjectValue(agentWorkAttrTypes, map[string]attr.Value{
		"enabled":               types.BoolValue(aw.Enabled),
		"kinds":                 set,
		"label_id":              types.Int64PointerValue(aw.LabelID),
		"label_chosen_at":       types.StringPointerValue(aw.LabelChosenAt),
		"accept_machine_labels": types.BoolValue(aw.AcceptMachineLabels),
		"agent_account_id":      types.Int64PointerValue(aw.AgentAccountID),
		"base_ref":              types.StringValue(aw.BaseRef),
		"max_in_progress":       types.Int64Value(aw.MaxInProgress),
		"daily_budget_usd":      types.Float64Value(aw.DailyBudgetUSD),
		"task_max_usd":          types.Float64Value(aw.TaskMaxUSD),
		"task_max_minutes":      types.Int64Value(aw.TaskMaxMinutes),
		"queue_minutes":         types.Int64Value(aw.QueueMinutes),
		"runbook":               types.StringValue(aw.Runbook),
		"lock_version":          types.Int64Value(aw.LockVersion),
	})
	diags.Append(d...)
	return obj
}

// agentWorkFields returns the settings to send from the CONFIGURED block, or
// nil when the configuration has no block. Only known, non-null settings are
// sent. That is what makes the merge safe, and for label_id and
// agent_account_id it is what keeps an apply from clearing them: the API
// reads a null for either as "clear the choice", so an unset one is left out,
// never sent as null.
func agentWorkFields(ctx context.Context, block types.Object, diags *diag.Diagnostics) client.Fields {
	if block.IsNull() || block.IsUnknown() {
		return nil
	}
	var m agentWorkModel
	diags.Append(block.As(ctx, &m, objectAsOptions)...)
	fields := client.Fields{}
	putBool := func(key string, v types.Bool) {
		if known(v) {
			fields[key] = v.ValueBool()
		}
	}
	putInt := func(key string, v types.Int64) {
		if known(v) {
			fields[key] = v.ValueInt64()
		}
	}
	putFloat := func(key string, v types.Float64) {
		if known(v) {
			fields[key] = v.ValueFloat64()
		}
	}
	putString := func(key string, v types.String) {
		if known(v) {
			fields[key] = v.ValueString()
		}
	}
	putBool("enabled", m.Enabled)
	if known(m.Kinds) {
		kinds := []string{}
		diags.Append(m.Kinds.ElementsAs(ctx, &kinds, false)...)
		sort.Strings(kinds)
		fields["kinds"] = kinds
	}
	putInt("label_id", m.LabelID)
	putBool("accept_machine_labels", m.AcceptMachineLabels)
	putInt("agent_account_id", m.AgentAccountID)
	putString("base_ref", m.BaseRef)
	putInt("max_in_progress", m.MaxInProgress)
	putFloat("daily_budget_usd", m.DailyBudgetUSD)
	putFloat("task_max_usd", m.TaskMaxUSD)
	putInt("task_max_minutes", m.TaskMaxMinutes)
	putInt("queue_minutes", m.QueueMinutes)
	putString("runbook", m.Runbook)
	return fields
}

// readAgentWork fetches the block for a project, and the API's answer for
// callers that report on it. A 403 (token is not a workspace admin) or a 404
// (project gone between calls, or a Flightdeck without the endpoint) leaves
// the block null and the answer nil; anything else is an error.
func readAgentWork(ctx context.Context, c *client.Client, projectID int64, diags *diag.Diagnostics) (types.Object, *client.AgentWork) {
	aw, err := c.GetAgentWork(ctx, projectID)
	if err != nil {
		if client.IsForbidden(err) || client.IsNotFound(err) {
			return types.ObjectNull(agentWorkAttrTypes), nil
		}
		addAPIError(diags, "Error reading Flightdeck agent work settings", err)
		return types.ObjectNull(agentWorkAttrTypes), nil
	}
	return agentWorkToObject(ctx, aw, diags), aw
}

// agentWorkApply says which apply a write belongs to, which decides how a
// refusal is reported.
type agentWorkApply int

const (
	// agentWorkOnUpdate: a refusal is an error, and the block in state stays
	// as it was read before.
	agentWorkOnUpdate agentWorkApply = iota
	// agentWorkOnCreate: a refusal the API explains, on a project it can still
	// read, is a warning. Any error during a create makes Terraform taint the
	// new project and replace it on the next apply, which is far worse than
	// settings that still need fixing.
	agentWorkOnCreate
)

// writeAgentWork PATCHes the configured settings (if any) under the settings
// row's own lock_version, and returns the block. With nothing configured it
// just reads. priorBlock is the block in state before this apply (null on
// create), whose lock_version is the If-Match; without one, the current
// version is read first. planned is the planned block, which a create-time
// refusal hands back so the apply stays consistent with its plan. Either way,
// blockers are reported as warnings while agent work is on; identifier names
// the project in them.
func writeAgentWork(ctx context.Context, c *client.Client, projectID int64, identifier string, configBlock, priorBlock, planned types.Object, apply agentWorkApply, diags *diag.Diagnostics) types.Object {
	settings := agentWorkFields(ctx, configBlock, diags)
	if diags.HasError() {
		return types.ObjectNull(agentWorkAttrTypes)
	}
	if len(settings) == 0 {
		block, aw := readAgentWork(ctx, c, projectID, diags)
		warnAgentWorkBlockers(aw, identifier, diags)
		return block
	}

	lockVersion, ok := agentWorkLockVersion(ctx, priorBlock, diags)
	if !ok {
		current, err := c.GetAgentWork(ctx, projectID)
		if err != nil {
			addAgentWorkWriteError(ctx, c, projectID, 0, err, diags)
			return types.ObjectNull(agentWorkAttrTypes)
		}
		lockVersion = current.LockVersion
	}

	aw, err := c.UpdateAgentWork(ctx, projectID, settings, lockVersion)
	if err == nil {
		warnAgentWorkBlockers(aw, identifier, diags)
		return agentWorkToObject(ctx, aw, diags)
	}
	if apply == agentWorkOnCreate && !client.IsNotFound(err) && !client.IsForbidden(err) {
		var fresh diag.Diagnostics
		if block, current := readAgentWork(ctx, c, projectID, &fresh); current != nil && !fresh.HasError() {
			diags.AddAttributeWarning(path.Root("agent_work"), "Agent work settings were not saved",
				"The project was created, but Flightdeck refused this block's settings and saved none of them. The next "+
					"plan shows them again, and the next apply tries again, so fix the configuration before then.\n\n"+
					"The API said: "+apiMessage(err))
			return agentWorkKeepPlan(planned, block, diags)
		}
	}
	addAgentWorkWriteError(ctx, c, projectID, lockVersion, err, diags)
	return types.ObjectNull(agentWorkAttrTypes)
}

// agentWorkLockVersion is the settings' lock_version from the block in state.
func agentWorkLockVersion(ctx context.Context, priorBlock types.Object, diags *diag.Diagnostics) (int64, bool) {
	if priorBlock.IsNull() || priorBlock.IsUnknown() {
		return 0, false
	}
	var prior agentWorkModel
	diags.Append(priorBlock.As(ctx, &prior, objectAsOptions)...)
	if !known(prior.LockVersion) {
		return 0, false
	}
	return prior.LockVersion.ValueInt64(), true
}

// agentWorkKeepPlan returns the planned block with its unknowns (whatever the
// configuration did not set, and the computed attributes) filled from a fresh
// read. A create that returns no error must hand back what it planned; the
// refresh before the next plan reads the real values, so that plan shows the
// refused settings again.
func agentWorkKeepPlan(planned, fresh types.Object, diags *diag.Diagnostics) types.Object {
	if planned.IsNull() || planned.IsUnknown() {
		return fresh
	}
	plannedAttrs, freshAttrs := planned.Attributes(), fresh.Attributes()
	out := make(map[string]attr.Value, len(plannedAttrs))
	for name, v := range plannedAttrs {
		if v.IsUnknown() {
			out[name] = freshAttrs[name]
			continue
		}
		out[name] = v
	}
	obj, d := types.ObjectValue(agentWorkAttrTypes, out)
	diags.Append(d...)
	return obj
}

// addAgentWorkWriteError explains a refused read or write of the settings.
func addAgentWorkWriteError(ctx context.Context, c *client.Client, projectID, lockVersion int64, err error, diags *diag.Diagnostics) {
	if addIfProjectGone(ctx, c, projectID, "", err, diags) {
		return
	}
	at := path.Root("agent_work")
	switch {
	case client.IsNotFound(err):
		diags.AddAttributeError(at, "Agent work settings are not available on this Flightdeck",
			"The project exists but its agent-work endpoint answered 404, so this Flightdeck version does not expose agent "+
				"work settings over the API yet. Remove the block or upgrade Flightdeck. "+apiMessage(err))
	case client.IsForbidden(err):
		diags.AddAttributeError(at, "Agent work settings require a workspace owner or admin",
			"Only a workspace owner or admin may read or write a project's agent work settings; a project admin is not "+
				"enough. "+apiMessage(err))
	case client.IsStale(err):
		var current *int64
		if fresh, rerr := c.GetAgentWork(ctx, projectID); rerr == nil {
			current = &fresh.LockVersion
		}
		addStaleError(diags, "Project agent work settings", lockVersion, current, err)
	case client.IsValidation(err):
		diags.AddAttributeError(at, "Flightdeck refused the agent work settings",
			"Nothing was saved. "+apiMessage(err))
	default:
		addAPIError(diags, "Error updating Flightdeck agent work settings", err)
	}
}
