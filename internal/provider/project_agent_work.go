package provider

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"slices"
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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
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
//   - The write MERGES and reads a blank as "no opinion", except label_id,
//     research_label_id and agent_account_id, where a blank or null CLEARS the
//     choice. So only the configured settings are ever sent, and an unset
//     label_id, research_label_id or agent_account_id is left out of the write
//     rather than sent as null: otherwise every apply that did not mention
//     them would clear them.
//   - `blockers` (what would stop Flightdeck sending work right now) changes
//     with the project and the day, like self-healing's rollback_blockers. It
//     is reported as warnings on refresh and apply while agent work is on,
//     never stored.
//   - `models`, `auto_merge`, `send_ci_failures`, `send_errors` and
//     `wait_for_checks` are newer than the rest (agentWorkNewerSettings). A
//     Flightdeck older than one has no such key on a read and refuses any
//     write naming it, so it reads as null there and, like every setting, is
//     sent only when configured. A write that names it to such a Flightdeck
//     is refused, and the provider says to upgrade (agentWorkSettingRefused).
//     The read's `send_errors_since` is read-only and not in the block, so it
//     is never sent.
//   - `models` is the one setting that is a map. Like `kinds`, a write
//     REPLACES it: a kind left out goes back to AutoPilot's default, and `{}`
//     clears them all. The plan checks its keys, the kinds of work, but never
//     its model names: Flightdeck names the models it offers, the list may
//     grow, and it accepts a model it no longer offers for the kind that
//     already has it.
//
// Every attribute is Optional+Computed or Computed, for the reason the
// slack_channel block gives: an Optional-only member nulled by dropping the
// block would cascade into "known after apply" across the project.

// agentWorkModel is the Terraform shape of the block.
type agentWorkModel struct {
	Enabled             types.Bool   `tfsdk:"enabled"`
	Kinds               types.Set    `tfsdk:"kinds"`
	Models              types.Map    `tfsdk:"models"`
	AutoMerge           types.Bool   `tfsdk:"auto_merge"`
	SendCIFailures      types.Bool   `tfsdk:"send_ci_failures"`
	SendErrors          types.Bool   `tfsdk:"send_errors"`
	WaitForChecks       types.Bool   `tfsdk:"wait_for_checks"`
	LabelID             types.Int64  `tfsdk:"label_id"`
	LabelChosenAt       types.String `tfsdk:"label_chosen_at"`
	AcceptMachineLabels types.Bool   `tfsdk:"accept_machine_labels"`
	// ResearchLabelID and ResearchLabelChosenAt work like LabelID and
	// LabelChosenAt, for the label that sends an item for research first.
	ResearchLabelID       types.Int64   `tfsdk:"research_label_id"`
	ResearchLabelChosenAt types.String  `tfsdk:"research_label_chosen_at"`
	AgentAccountID        types.Int64   `tfsdk:"agent_account_id"`
	BaseRef               types.String  `tfsdk:"base_ref"`
	MaxInProgress         types.Int64   `tfsdk:"max_in_progress"`
	DailyBudgetUSD        types.Float64 `tfsdk:"daily_budget_usd"`
	TaskMaxUSD            types.Float64 `tfsdk:"task_max_usd"`
	TaskMaxMinutes        types.Int64   `tfsdk:"task_max_minutes"`
	QueueMinutes          types.Int64   `tfsdk:"queue_minutes"`
	Runbook               types.String  `tfsdk:"runbook"`
	LockVersion           types.Int64   `tfsdk:"lock_version"`
}

var agentWorkAttrTypes = map[string]attr.Type{
	"enabled":                  types.BoolType,
	"kinds":                    types.SetType{ElemType: types.StringType},
	"models":                   types.MapType{ElemType: types.StringType},
	"auto_merge":               types.BoolType,
	"send_ci_failures":         types.BoolType,
	"send_errors":              types.BoolType,
	"wait_for_checks":          types.BoolType,
	"label_id":                 types.Int64Type,
	"label_chosen_at":          types.StringType,
	"accept_machine_labels":    types.BoolType,
	"research_label_id":        types.Int64Type,
	"research_label_chosen_at": types.StringType,
	"agent_account_id":         types.Int64Type,
	"base_ref":                 types.StringType,
	"max_in_progress":          types.Int64Type,
	"daily_budget_usd":         types.Float64Type,
	"task_max_usd":             types.Float64Type,
	"task_max_minutes":         types.Int64Type,
	"queue_minutes":            types.Int64Type,
	"runbook":                  types.StringType,
	"lock_version":             types.Int64Type,
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
			"AutoPilot, the AI agents Flightdeck sends work to, may take this project's work items, which labels mark an " +
			"item for them, which AI model each kind of work runs on, whether items filed for failed CI runs or logged errors go to " +
			"them without a label, the service account they " +
			"work as, whether they wait for CI on their pull requests, and how much they may spend and run. An agent works on a " +
			"copy of the project's GitHub repository and opens a pull request, which a person reviews and merges unless " +
			"`auto_merge` is on. Reading and " +
			"writing these settings requires the token's user to be a **workspace owner or admin**; for other tokens, " +
			"and on a Flightdeck version without the endpoint, the block is null.\n\n" +
			"Agent work is off until `enabled` turns it on. A plan that turns it on carries a warning saying so, and " +
			"while it is on, each thing that stops Flightdeck sending work right now (the API's `blockers`: no " +
			"agent label or research label chosen, no linked GitHub repository, the day's budget spent, and so on) is " +
			"shown as a warning on refresh and after apply rather than stored, because it changes as the project does.\n\n" +
			"The endpoint merges, so this block only ever sends what you configure, and a setting you never name keeps " +
			"whatever the project has, including a value changed on the settings page. `label_id` and " +
			"`agent_account_id` are never sent unless configured, because the API reads an empty value for either as " +
			"\"clear it\"; removing one from configuration therefore leaves the stored choice in place (clear it on " +
			"the project's **Agent work** settings page). `research_label_id` works the same way.\n\n" +
			"These settings have their own `lock_version`, separate from the project's: a write here never conflicts " +
			"with a project update and does not bump the project's `lock_version`.",
		Optional: true,
		Computed: true,
		PlanModifiers: []planmodifier.Object{
			objectplanmodifier.UseStateForUnknown(),
		},
		Attributes: map[string]schema.Attribute{
			"enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether AutoPilot agents may take this project's work items. Off by default. Once it " +
					"is on and nothing in `blockers` stands in the way, agents take each work item a person marks with " +
					"the agent label (and, while `send_ci_failures` is on, each item Flightdeck files for a failed CI " +
					"run, and while `send_errors` is on, each item an error alert rule files) and open pull requests " +
					"for it, and, while `research` is one of the kinds, research " +
					"each item marked with the research label and post a report on it. A plan that turns it on warns. " +
					"Flightdeck posts the change to the project's Slack updates. When unset, the project's current " +
					"value is kept; set it explicitly, even to `false`, for Terraform to own it.",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"kinds": schema.SetAttribute{
				MarkdownDescription: "The kinds of work agents may do. Known kinds: `" + joinBackticked(client.AgentWorkKinds) +
					"`. None by default, and an empty set clears the list. Add `fix-error` or `research` only once " +
					"AutoPilot allows that kind for the project: if AutoPilot refuses a kind, it holds all of the " +
					"project's agent work, other kinds included. When unset, the project's current list is kept.",
				ElementType:   types.StringType,
				Optional:      true,
				Computed:      true,
				Validators:    []validator.Set{setvalidator.ValueStringsAre(stringvalidator.OneOf(client.AgentWorkKinds...))},
				PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()},
			},
			"models": schema.MapAttribute{
				MarkdownDescription: modelsDescription,
				ElementType:         types.StringType,
				Optional:            true,
				Computed:            true,
				Validators:          []validator.Map{agentWorkModelsValidator{}},
				PlanModifiers:       []planmodifier.Map{mapplanmodifier.UseStateForUnknown()},
			},
			"auto_merge": schema.BoolAttribute{
				MarkdownDescription: autoMergeDescription,
				Optional:            true,
				Computed:            true,
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"send_ci_failures": schema.BoolAttribute{
				MarkdownDescription: sendCIFailuresDescription,
				Optional:            true,
				Computed:            true,
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"send_errors": schema.BoolAttribute{
				MarkdownDescription: sendErrorsDescription,
				Optional:            true,
				Computed:            true,
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"wait_for_checks": schema.BoolAttribute{
				MarkdownDescription: waitForChecksDescription,
				Optional:            true,
				Computed:            true,
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
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
				MarkdownDescription: "Whether the agent label and the research label count even when a machine added them " +
					"(an automation rule, an import, a service account). Off by default, so only a label a person adds " +
					"sends an item to an agent. When unset, the project's current value is kept.",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"research_label_id": reference("Id of the label in this project that sends a work item to an agent for " +
				"**research** first, rather than to be built: the agent looks into it and posts a report on the item as a " +
				"comment. It must be a different label from `label_id`. The plan fails when the configuration sets both " +
				"to the same id, and Flightdeck refuses it when one of them matches the other's stored value. Like the " +
				"agent label, only a label a person adds after it is chosen here counts (see `research_label_chosen_at`), " +
				"unless `accept_machine_labels` says otherwise. Flightdeck sends an item as research only while " +
				"`research` is in the project's kinds of work. When unset, the stored label is kept; it is never sent " +
				"empty, because the API would read that as clearing it.\n\n" +
				"Needs the Flightdeck release that added the agent work research label. A label in this project forms the " +
				"same dependency cycle as `label_id`, and the same data source breaks it."),
			"research_label_chosen_at": schema.StringAttribute{
				MarkdownDescription: "When `research_label_id` was last set to a label (RFC 3339), or null while none is " +
					"chosen. Only a label a person adds after this time counts. Read-only: Flightdeck sets it whenever " +
					"`research_label_id` changes.",
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
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
				MarkdownDescription: "The steps the agent follows for `implement-work-item`. Known runbooks: `" +
					joinBackticked(client.AgentWorkRunbooks) + "` (the default). `fix-error` and `research` always use " +
					"their own runbook. When unset, the project's current value is kept.",
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
			"enabled":                  datasourceschema.BoolAttribute{MarkdownDescription: "Whether agents may take the project's work items.", Computed: true},
			"kinds":                    datasourceschema.SetAttribute{MarkdownDescription: "The kinds of work agents may do.", ElementType: types.StringType, Computed: true},
			"models":                   datasourceschema.MapAttribute{MarkdownDescription: modelsDataSourceDescription, ElementType: types.StringType, Computed: true},
			"auto_merge":               datasourceschema.BoolAttribute{MarkdownDescription: autoMergeDataSourceDescription, Computed: true},
			"send_ci_failures":         datasourceschema.BoolAttribute{MarkdownDescription: sendCIFailuresDataSourceDescription, Computed: true},
			"send_errors":              datasourceschema.BoolAttribute{MarkdownDescription: sendErrorsDataSourceDescription, Computed: true},
			"wait_for_checks":          datasourceschema.BoolAttribute{MarkdownDescription: waitForChecksDataSourceDescription, Computed: true},
			"label_id":                 datasourceschema.Int64Attribute{MarkdownDescription: "Id of the label that marks an item for an agent, if one is chosen.", Computed: true},
			"label_chosen_at":          datasourceschema.StringAttribute{MarkdownDescription: "When `label_id` was last set to a label (RFC 3339).", Computed: true},
			"accept_machine_labels":    datasourceschema.BoolAttribute{MarkdownDescription: "Whether the agent label and the research label count when a machine added them.", Computed: true},
			"research_label_id":        datasourceschema.Int64Attribute{MarkdownDescription: "Id of the label that sends an item for research first, if one is chosen.", Computed: true},
			"research_label_chosen_at": datasourceschema.StringAttribute{MarkdownDescription: "When `research_label_id` was last set to a label (RFC 3339).", Computed: true},
			"agent_account_id":         datasourceschema.Int64Attribute{MarkdownDescription: "User id of the service account agents work as, if one is chosen.", Computed: true},
			"base_ref":                 datasourceschema.StringAttribute{MarkdownDescription: "The branch agents start from.", Computed: true},
			"max_in_progress":          datasourceschema.Int64Attribute{MarkdownDescription: "How many items agents may work on at once.", Computed: true},
			"daily_budget_usd":         datasourceschema.Float64Attribute{MarkdownDescription: "The most agent work may cost the project in one UTC day, in US dollars.", Computed: true},
			"task_max_usd":             datasourceschema.Float64Attribute{MarkdownDescription: "The most one task may cost, in US dollars.", Computed: true},
			"task_max_minutes":         datasourceschema.Int64Attribute{MarkdownDescription: "The most minutes one task may run.", Computed: true},
			"queue_minutes":            datasourceschema.Int64Attribute{MarkdownDescription: "How many minutes a task may wait to start.", Computed: true},
			"runbook":                  datasourceschema.StringAttribute{MarkdownDescription: "The steps the agent follows for `implement-work-item`.", Computed: true},
			"lock_version":             datasourceschema.Int64Attribute{MarkdownDescription: "Optimistic-locking version of these settings, separate from the project's.", Computed: true},
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

// validateAgentWorkConfig checks the cross-field rules the API enforces when
// both sides are configured: a task may not cost more than the day, and the
// research label must be a different label from the agent label. The API
// checks both against the MERGED row, so a write naming one side can still be
// refused by the other's stored value; warnAgentWorkBudget looks for that on
// the budget, and the API's refusal of a label clash lands on the attribute
// (addAgentWorkWriteError).
func validateAgentWorkConfig(ctx context.Context, block types.Object, diags *diag.Diagnostics) {
	if block.IsNull() || block.IsUnknown() {
		return
	}
	var m agentWorkModel
	diags.Append(block.As(ctx, &m, objectAsOptions)...)
	if known(m.LabelID) && known(m.ResearchLabelID) && m.LabelID.ValueInt64() == m.ResearchLabelID.ValueInt64() {
		diags.AddAttributeError(path.Root("agent_work").AtName("research_label_id"), "The research label must be a different label",
			fmt.Sprintf("research_label_id and label_id are both %d. One label cannot mean both \"build it\" and "+
				"\"research it first\", so Flightdeck refuses the same label for both. Pick a different label for one of them.",
				m.ResearchLabelID.ValueInt64()))
	}
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

// planAgentWorkComputed keeps the three computed attributes honest in a plan.
// All carry UseStateForUnknown, so a plan would otherwise promise their prior
// values: lock_version moves on whenever a write changes a setting,
// label_chosen_at whenever label_id changes, and research_label_chosen_at
// whenever research_label_id does. Each becomes unknown exactly then, so an
// apply that changes nothing here plans nothing here.
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
	if !priorKnown || planned.ResearchLabelID.IsUnknown() || !planned.ResearchLabelID.Equal(prior.ResearchLabelID) {
		planned.ResearchLabelChosenAt = types.StringUnknown()
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
	return same(a.Enabled, b.Enabled) && same(a.Kinds, b.Kinds) && same(a.Models, b.Models) && same(a.AutoMerge, b.AutoMerge) &&
		same(a.SendCIFailures, b.SendCIFailures) && same(a.SendErrors, b.SendErrors) && same(a.WaitForChecks, b.WaitForChecks) &&
		same(a.LabelID, b.LabelID) &&
		same(a.AcceptMachineLabels, b.AcceptMachineLabels) && same(a.ResearchLabelID, b.ResearchLabelID) &&
		same(a.AgentAccountID, b.AgentAccountID) &&
		same(a.BaseRef, b.BaseRef) && same(a.MaxInProgress, b.MaxInProgress) &&
		sameFloat(a.DailyBudgetUSD, b.DailyBudgetUSD) && sameFloat(a.TaskMaxUSD, b.TaskMaxUSD) &&
		same(a.TaskMaxMinutes, b.TaskMaxMinutes) && same(a.QueueMinutes, b.QueueMinutes) && same(a.Runbook, b.Runbook)
}

// known reports a known, non-null value.
func known(v attr.Value) bool { return !v.IsNull() && !v.IsUnknown() }

// agentWorkActs is what the plan-time warning says agents will do.
const agentWorkActs = "AutoPilot agents may then take each of this project's work items that a person marks with the " +
	"agent label (and, while send_ci_failures is on, each item Flightdeck files for a failed CI run, and while " +
	"send_errors is on, each item an error alert rule files), work on a copy of its GitHub repository as the agent " +
	"account, and open pull requests, which people " +
	"review and merge unless auto_merge is on. If research is one of the kinds, an item a person marks with the research label is researched " +
	"instead, and the agent posts its report on the item as a comment. All of this stays within the daily budget " +
	"and the per-task limits."

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
	// A Flightdeck older than models has no such key, and reads as null.
	models := types.MapNull(types.StringType)
	if aw.ReportsModels() {
		models, d = types.MapValueFrom(ctx, types.StringType, aw.Models)
		diags.Append(d...)
	}
	obj, d := types.ObjectValue(agentWorkAttrTypes, map[string]attr.Value{
		"enabled":                  types.BoolValue(aw.Enabled),
		"kinds":                    set,
		"models":                   models,
		"auto_merge":               types.BoolPointerValue(aw.AutoMerge),
		"send_ci_failures":         types.BoolPointerValue(aw.SendCIFailures),
		"send_errors":              types.BoolPointerValue(aw.SendErrors),
		"wait_for_checks":          types.BoolPointerValue(aw.WaitForChecks),
		"label_id":                 types.Int64PointerValue(aw.LabelID),
		"label_chosen_at":          types.StringPointerValue(aw.LabelChosenAt),
		"accept_machine_labels":    types.BoolValue(aw.AcceptMachineLabels),
		"research_label_id":        types.Int64PointerValue(aw.ResearchLabelID),
		"research_label_chosen_at": types.StringPointerValue(aw.ResearchLabelChosenAt),
		"agent_account_id":         types.Int64PointerValue(aw.AgentAccountID),
		"base_ref":                 types.StringValue(aw.BaseRef),
		"max_in_progress":          types.Int64Value(aw.MaxInProgress),
		"daily_budget_usd":         types.Float64Value(aw.DailyBudgetUSD),
		"task_max_usd":             types.Float64Value(aw.TaskMaxUSD),
		"task_max_minutes":         types.Int64Value(aw.TaskMaxMinutes),
		"queue_minutes":            types.Int64Value(aw.QueueMinutes),
		"runbook":                  types.StringValue(aw.Runbook),
		"lock_version":             types.Int64Value(aw.LockVersion),
	})
	diags.Append(d...)
	return obj
}

// agentWorkFields returns the settings to send from block, or nil when it is
// null or unknown. On an apply, block is plannedWhereConfigured's: the plan's
// value of each setting the configuration sets. Only known, non-null settings
// are sent. That is what makes the merge safe, and for label_id,
// research_label_id and agent_account_id it is what keeps an apply from
// clearing them: the API reads a null for any of them as "clear the choice",
// so an unset one is left out, never sent as null.
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
		fields["kinds"] = kindsInAPIOrder(kinds)
	}
	if known(m.Models) {
		// The whole map, which replaces the stored one. The plan refuses a null
		// or blank model (agentWorkModelsValidator), which Flightdeck would read
		// as leaving that kind out.
		models := map[string]string{}
		diags.Append(m.Models.ElementsAs(ctx, &models, false)...)
		fields["models"] = models
	}
	putBool("auto_merge", m.AutoMerge)
	putBool("send_ci_failures", m.SendCIFailures)
	putBool("send_errors", m.SendErrors)
	putBool("wait_for_checks", m.WaitForChecks)
	putInt("label_id", m.LabelID)
	putBool("accept_machine_labels", m.AcceptMachineLabels)
	putInt("research_label_id", m.ResearchLabelID)
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

// kindsInAPIOrder returns kinds in the API's own order (client.AgentWorkKinds),
// which is the order Flightdeck's settings page saves them in. Flightdeck
// stores the list as sent and counts a reordered list as a change, so a list
// sent in any other order would record a change, and move lock_version, every
// time the page and Terraform took turns. A kind the API doesn't list (the
// plan refuses one) goes last, in name order.
func kindsInAPIOrder(kinds []string) []string {
	rank := func(kind string) int {
		for i, k := range client.AgentWorkKinds {
			if k == kind {
				return i
			}
		}
		return len(client.AgentWorkKinds)
	}
	out := append([]string{}, kinds...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(out[i]), rank(out[j])
		if ri != rj {
			return ri < rj
		}
		return out[i] < out[j]
	})
	return out
}

// kindsUnchangedInState reports whether the write sends kinds and the
// configured set is the one in state. Only then may kinds be left out of the
// write, and only once the server confirms it holds that set too
// (serverHoldsKinds): state is not always what Flightdeck holds. A create
// whose settings Flightdeck refused keeps the planned kinds in state, and a
// plan made with -refresh=false never looks.
func kindsUnchangedInState(ctx context.Context, settings client.Fields, priorBlock types.Object, diags *diag.Diagnostics) bool {
	sent, ok := settings["kinds"].([]string)
	if !ok || priorBlock.IsNull() || priorBlock.IsUnknown() {
		return false
	}
	var prior agentWorkModel
	diags.Append(priorBlock.As(ctx, &prior, objectAsOptions)...)
	if !known(prior.Kinds) {
		return false
	}
	held := []string{}
	diags.Append(prior.Kinds.ElementsAs(ctx, &held, false)...)
	return !diags.HasError() && sameKinds(held, sent)
}

// serverHoldsKinds reports whether the server's settings, read at the
// version the write will pin, hold the kinds the write would send. Then
// resending them changes nothing but, in another order, would still count as
// a change there: it would move lock_version, which the plan promised would
// stay, and record a change nobody made. A different version is left to the
// write, whose If-Match makes it the usual stale-settings error.
func serverHoldsKinds(current *client.AgentWork, lockVersion int64, settings client.Fields) bool {
	sent, ok := settings["kinds"].([]string)
	return ok && current != nil && current.LockVersion == lockVersion && sameKinds(current.Kinds, sent)
}

// sameKinds reports whether two lists hold the same kinds, in any order.
func sameKinds(a, b []string) bool {
	return strings.Join(kindsInAPIOrder(a), ",") == strings.Join(kindsInAPIOrder(b), ",")
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

// agentWorkModelsValidator checks models at plan time: each key must be a kind
// of work, and each model must be named. It never checks a model's name:
// Flightdeck names the models it offers, the list may grow, and it accepts a
// model it no longer offers for a kind that already has it, so only Flightdeck
// can say. A null or blank model is refused because Flightdeck reads it as
// leaving the kind out, so the apply would store something other than the
// plan said.
type agentWorkModelsValidator struct{}

func (agentWorkModelsValidator) Description(context.Context) string {
	return "each key must be a kind of work (" + strings.Join(client.AgentWorkKinds, ", ") + "), and each model must be named"
}

func (v agentWorkModelsValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (agentWorkModelsValidator) ValidateMap(_ context.Context, req validator.MapRequest, resp *validator.MapResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	elements := req.ConfigValue.Elements()
	kinds := make([]string, 0, len(elements))
	for kind := range elements {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		at := req.Path.AtMapKey(kind)
		if !slices.Contains(client.AgentWorkKinds, kind) {
			resp.Diagnostics.AddAttributeError(at, "Unknown kind of work in agent_work.models",
				fmt.Sprintf("%q is not a kind of work. The keys of models are the kinds of work (%s), and each one maps to "+
					"the AI model that kind of work runs on.", kind, strings.Join(client.AgentWorkKinds, ", ")))
			continue
		}
		model, ok := elements[kind].(types.String)
		if !ok || model.IsUnknown() {
			continue
		}
		if model.IsNull() || isBlank(model.ValueString()) {
			resp.Diagnostics.AddAttributeError(at, "No model named for "+kind,
				fmt.Sprintf("agent_work.models gives %s no model. Flightdeck reads a missing model as \"use AutoPilot's "+
					"default\" and leaves %s out of what it stores, so the apply would not match the plan. To use the "+
					"default for %s, leave it out of models.", kind, kind, kind))
		}
	}
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
// row's own lock_version, and returns the block. Which settings are sent
// follows the configuration, and what is sent follows the plan
// (plannedWhereConfigured), so `ignore_changes` keeps a value changed on the
// settings page. kinds is left out when it is the set in state and the server
// holds that set too (kindsUnchangedInState, serverHoldsKinds). With nothing
// to send it just reads. priorBlock is the block in state before this apply
// (null on create), whose lock_version is the If-Match; without one, the
// current version is read first. planned is the planned block, which a
// create-time refusal hands back so the apply stays consistent with its plan,
// and whose newer settings (models, auto_merge, send_ci_failures, send_errors,
// wait_for_checks) the block keeps while they go unsent (keepUnsetAsPlanned).
// Either way, blockers are reported as warnings while agent work is on;
// identifier names the project in them.
func writeAgentWork(ctx context.Context, c *client.Client, projectID int64, identifier string, configBlock, priorBlock, planned types.Object, apply agentWorkApply, diags *diag.Diagnostics) types.Object {
	toSend := plannedWhereConfigured(ctx, configBlock, planned, diags)
	if diags.HasError() {
		return types.ObjectNull(agentWorkAttrTypes)
	}
	block := writeAgentWorkSettings(ctx, c, projectID, identifier, toSend, priorBlock, planned, apply, diags)
	for _, setting := range agentWorkNewerSettings {
		block = keepUnsetAsPlanned(setting.key, toSend, planned, block, diags)
	}
	return block
}

// keepUnsetAsPlanned puts the planned value of key, one of the newer settings
// (agentWorkNewerSettings), in the block an apply leaves, when the write did
// not send it and the plan knew its value. sent is what the write was built
// from (plannedWhereConfigured), where an unsent setting is null: one the
// configuration leaves unset, or one it sets under ignore_changes while the
// plan, taken from state, holds null. An unsent setting changes nothing in the
// apply, but what the server reports can still differ from the plan: a
// Flightdeck upgraded to the setting between the plan and the apply reports
// false where the plan, made from a read without the key, holds null, and one
// rolled back below it reports null where the plan holds a value. A saved plan
// applied later makes that window real. Terraform refuses a result that
// differs from a known planned value, so the planned one is kept, and the next
// refresh adopts the server's, as it does for any unset setting. A value that
// was sent is left as the server reports it, so a server that did not honor it
// still fails the apply.
func keepUnsetAsPlanned(key string, sent, planned, result types.Object, diags *diag.Diagnostics) types.Object {
	if result.IsNull() || result.IsUnknown() || planned.IsNull() || planned.IsUnknown() || sent.IsUnknown() {
		return result
	}
	if !sent.IsNull() {
		if value, ok := sent.Attributes()[key]; !ok || !value.IsNull() {
			return result
		}
	}
	want, ok := planned.Attributes()[key]
	if !ok || want.IsUnknown() {
		return result
	}
	attrs := result.Attributes()
	if got := attrs[key]; got != nil && got.Equal(want) {
		return result
	}
	out := make(map[string]attr.Value, len(attrs))
	for name, v := range attrs {
		out[name] = v
	}
	out[key] = want
	obj, d := types.ObjectValue(agentWorkAttrTypes, out)
	diags.Append(d...)
	return obj
}

// writeAgentWorkSettings is writeAgentWork without the newer settings'
// correction. toSend is the block the write is built from
// (plannedWhereConfigured).
func writeAgentWorkSettings(ctx context.Context, c *client.Client, projectID int64, identifier string, toSend, priorBlock, planned types.Object, apply agentWorkApply, diags *diag.Diagnostics) types.Object {
	settings := agentWorkFields(ctx, toSend, diags)
	if diags.HasError() {
		return types.ObjectNull(agentWorkAttrTypes)
	}
	if len(settings) == 0 {
		block, aw := readAgentWork(ctx, c, projectID, diags)
		warnAgentWorkBlockers(aw, identifier, diags)
		return block
	}

	// One read serves both needs: the version to pin when state has none, and
	// whether the server already holds the kinds state says it does. The
	// If-Match stays the state's whenever state has one.
	lockVersion, ok := agentWorkLockVersion(ctx, priorBlock, diags)
	unchanged := kindsUnchangedInState(ctx, settings, priorBlock, diags)
	if diags.HasError() {
		return types.ObjectNull(agentWorkAttrTypes)
	}
	if !ok || unchanged {
		current, err := c.GetAgentWork(ctx, projectID)
		if err != nil {
			addAgentWorkWriteError(ctx, c, projectID, lockVersion, settings, err, diags)
			return types.ObjectNull(agentWorkAttrTypes)
		}
		if !ok {
			lockVersion = current.LockVersion
		}
		if unchanged && serverHoldsKinds(current, lockVersion, settings) {
			delete(settings, "kinds")
			if len(settings) == 0 {
				warnAgentWorkBlockers(current, identifier, diags)
				return agentWorkToObject(ctx, current, diags)
			}
		}
	}

	aw, err := c.UpdateAgentWork(ctx, projectID, settings, lockVersion)
	if err == nil {
		warnAgentWorkBlockers(aw, identifier, diags)
		return agentWorkToObject(ctx, aw, diags)
	}
	if apply == agentWorkOnCreate && !client.IsNotFound(err) && !client.IsForbidden(err) {
		var fresh diag.Diagnostics
		if block, current := readAgentWork(ctx, c, projectID, &fresh); current != nil && !fresh.HasError() {
			if refused := agentWorkRefusedSettings(current, settings, err); len(refused) > 0 {
				for _, setting := range refused {
					addAgentWorkSettingUnsupported(setting, err, agentWorkOnCreate, diags)
				}
				return agentWorkKeepPlan(planned, block, diags)
			}
			at, _ := agentWorkRefusedAt(settings, err)
			if agentWorkModelsRefused(settings, err) {
				at = path.Root("agent_work").AtName("models")
			}
			diags.AddAttributeWarning(at, "Agent work settings were not saved",
				"The project was created, but Flightdeck refused this block's settings and saved none of them. The next "+
					"plan shows them again, and the next apply tries again, so fix the configuration before then.\n\n"+
					"The API said: "+apiMessage(err))
			return agentWorkKeepPlan(planned, block, diags)
		}
	}
	addAgentWorkWriteError(ctx, c, projectID, lockVersion, settings, err, diags)
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

// agentWorkRefusedAt picks where a refused write is reported: against the
// attribute when the refusal is about the research label, otherwise against
// the block. Flightdeck names the research label in its refusals ("Research
// label must be a label in this project", "Research label must be different
// from the agent label", or research_label_id by its key), and it names a
// clash between the two labels as the research label's problem whichever of
// them the write changed. So such a refusal points at research_label_id when
// the write sent it, and at label_id when it sent only that (a new agent
// label that is the stored research label). A refusal that also names the
// agent label ("Agent label must be a label in this project and Research
// label must be ...") is about both, so it stays on the block. An unknown-key
// refusal ends with the keys the endpoint does take, research_label_id among
// them, and that list says nothing about what was refused, so it is not read
// (withoutSettableList). ok reports whether it picked an attribute.
func agentWorkRefusedAt(sent client.Fields, err error) (at path.Path, ok bool) {
	block := path.Root("agent_work")
	if !client.IsValidation(err) {
		return block, false
	}
	msg := withoutSettableList(apiMessage(err))
	if !strings.Contains(msg, "Research label") && !strings.Contains(msg, "research_label_id") {
		return block, false
	}
	if namesAgentLabel.MatchString(msg) {
		return block, false
	}
	for _, key := range []string{"research_label_id", "label_id"} {
		if _, named := sent[key]; named {
			return block.AtName(key), true
		}
	}
	return block, false
}

// namesAgentLabel finds the agent label in a validation refusal, which names
// each setting by the settings page's name for it. The research label's own
// "different from the agent label" says it in lower case, so it does not
// match. The key label_id is not looked for: an older Flightdeck's refusal of
// research_label_id as an unknown key lists label_id among the settable keys.
var namesAgentLabel = regexp.MustCompile(`Agent label`)

// agentWorkModelsRefused reports whether a refused write failed on the models
// it sent: an unknown kind, a model Flightdeck doesn't offer, or a value that
// isn't an object. Flightdeck starts each such refusal with the key, as
// "models has an unknown kind: ...", "models.research must be one of: ...",
// or "models must be an object ...". An older Flightdeck's refusal of the key
// itself starts "unknown key", and is the newer settings' to explain.
func agentWorkModelsRefused(sent client.Fields, err error) bool {
	if !client.HasCode(err, client.CodeInvalidAttribute) {
		return false
	}
	if _, named := sent["models"]; !named {
		return false
	}
	msg := strings.TrimSpace(apiMessage(err))
	return strings.HasPrefix(msg, "models ") || strings.HasPrefix(msg, "models.")
}

// agentWorkModelsRule is what a refused models map must be.
const agentWorkModelsRule = "agent_work.models maps kinds of work to AI models that Flightdeck offers, and its " +
	"message below says what it takes. The provider doesn't check model names itself, because that list can " +
	"change. A model Flightdeck no longer offers is still accepted for a kind it is already set for, but not for " +
	"any other kind, nor once that kind has been changed to another model."

// agentWorkResearchLabelRule is what a refused research label must be.
const agentWorkResearchLabelRule = "research_label_id must be a label in this project, and a different label from " +
	"label_id. Flightdeck names a clash between the two as the research label's problem, whichever of them this " +
	"apply changed."

// agentWorkResearchLabelUnknownNote closes a research label refusal that the
// API gave as an invalid attribute, which is how a Flightdeck older than the
// research label refuses the key.
const agentWorkResearchLabelUnknownNote = "\n\nIf the API calls research_label_id an unknown key, this Flightdeck is " +
	"older than the release that added the agent work research label: upgrade Flightdeck first, or leave " +
	"research_label_id out of the configuration."

// addAgentWorkWriteError explains a refused read or write of the settings.
// sent is what the write sent, which says which attribute a refusal of the
// research label is about.
func addAgentWorkWriteError(ctx context.Context, c *client.Client, projectID, lockVersion int64, sent client.Fields, err error, diags *diag.Diagnostics) {
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
		// A read after the refusal decides whether this Flightdeck is too old
		// for a newer setting the write sent, not the API's wording.
		if refused := agentWorkUnsupportedSettings(ctx, c, projectID, sent, err); len(refused) > 0 {
			for _, setting := range refused {
				addAgentWorkSettingUnsupported(setting, err, agentWorkOnUpdate, diags)
			}
			return
		}
		// When that read could not tell, the refusal itself may still name a
		// newer setting as an unknown key; only then is its upgrade note added.
		note := agentWorkUnknownNotes(err)
		if agentWorkModelsRefused(sent, err) {
			diags.AddAttributeError(at.AtName("models"), "Flightdeck refused agent_work.models",
				"Nothing was saved. "+agentWorkModelsRule+"\n\nThe API said: "+apiMessage(err))
			return
		}
		if label, ok := agentWorkRefusedAt(sent, err); ok {
			detail := "Nothing was saved. " + agentWorkResearchLabelRule + "\n\nThe API said: " + apiMessage(err)
			if client.HasCode(err, client.CodeInvalidAttribute) {
				detail += agentWorkResearchLabelUnknownNote
			}
			diags.AddAttributeError(label, "Flightdeck refused the research label", detail+note)
			return
		}
		diags.AddAttributeError(at, "Flightdeck refused the agent work settings", "Nothing was saved. "+apiMessage(err)+note)
	default:
		addAPIError(diags, "Error updating Flightdeck agent work settings", err)
	}
}

// modelsDescription is the resource attribute's documentation.
const modelsDescription = "Which AI model each kind of work runs on, as a map from kind of work to model name, for " +
	"example `{ \"implement-work-item\" = \"opus-5-5\" }`. The keys are kinds of work: `" +
	"implement-work-item`, `fix-error` and `research`, and the plan fails on any other key. Each kind needs a model " +
	"name; a blank one fails the plan. A kind left out of the map uses AutoPilot's default, and " +
	"`{}` puts every kind back on the default. Setting it replaces the whole map Flightdeck holds, so a kind removed " +
	"from it goes back to the default.\n\n" +
	"Flightdeck names the models it offers (`sonnet-5-5` and `opus-5-5` today, and the list may grow), so the " +
	"provider doesn't check model names: Flightdeck refuses a model it doesn't offer, and the apply fails with its " +
	"message, which lists the ones it does (on the apply that creates the project, that is a warning and none of the " +
	"block's settings are saved). A model Flightdeck no longer offers is still accepted for the kind it " +
	"is already set for, so a configuration that names it keeps applying until that kind is changed to another " +
	"model; after that, Flightdeck won't take the old one back. A work item's own agent model, set in " +
	"Flightdeck, wins over this. Opus costs more than Sonnet, so the daily budget and each task's limit run out " +
	"sooner. If AutoPilot doesn't run a model named here, it refuses the tasks that ask for it, and Flightdeck holds " +
	"the project's agent work for a while (the `model_refused` blocker). When unset, the project's current models " +
	"are kept; set it explicitly, even to `{}`, for Terraform to own it.\n\n" +
	"Needs a Flightdeck that supports `models`. On an older one it reads as null and is never sent while it is " +
	"unset. Setting it there fails an update with an error that asks you to upgrade Flightdeck. On the apply that " +
	"creates the project it is a warning instead, so the new project isn't marked tainted, and the next plan shows " +
	"what is still to apply."

// modelsDataSourceDescription is the data source attribute's documentation.
const modelsDataSourceDescription = "Which AI model each kind of work runs on, as a map from kind of work to model " +
	"name. A kind not in the map uses AutoPilot's default. Null on a Flightdeck older than the setting."

// autoMergeDescription is the resource attribute's documentation.
const autoMergeDescription = "Whether to ask AutoPilot to turn on GitHub auto-merge for each pull request an agent " +
	"opens for this project. Off by default. AutoPilot's app for the repository, and the repository's own settings " +
	"and ruleset, must allow auto-merge too; if any of them doesn't, the pull request waits for a person. GitHub " +
	"merges only once the required checks and reviews pass. Flightdeck never merges anything itself. When unset, " +
	"the project's current value is kept; set it explicitly, even to `false`, for Terraform to own it.\n\n" +
	"Needs a Flightdeck that supports `auto_merge`. On an older one it reads as null and is never sent while it is " +
	"unset. Setting it there fails an update with an error that asks you to upgrade Flightdeck. On the apply that " +
	"creates the project it is a warning instead, so the new project isn't marked tainted, and the next plan shows " +
	"what is still to apply."

// autoMergeDataSourceDescription is the data source attribute's documentation.
const autoMergeDataSourceDescription = "Whether AutoPilot is asked to turn on GitHub auto-merge for the pull " +
	"requests agents open for the project. Null on a Flightdeck older than the setting."

// sendCIFailuresDescription is the resource attribute's documentation.
const sendCIFailuresDescription = "Whether to send each work item Flightdeck files for a failed CI run to an agent " +
	"without waiting for a person to add the agent label. Off by default, so such an item waits for a person to " +
	"label it. It applies only to items filed for workflows on the linked repository's CI workflows list (" +
	"`ci_workflows` on `flightdeck_github_integration`), and only while the integration files CI failures (" +
	"`ci_failure_action` isn't `off`), an agent label is chosen (`label_id`) and `implement-work-item` is one " +
	"of the `kinds`; an item that waits in intake goes once someone accepts it. Flightdeck sends each such item " +
	"once: after an agent has worked on it, it goes again only when a person adds the agent label. At most one " +
	"item per workflow (per job, for a flaky test) goes this way in 24 hours, and never one a person is assigned " +
	"to. The usual limits still hold, such as `max_in_progress` and the budget. Turning it on or off is posted " +
	"to the project's Slack updates. When unset, the project's current value is kept; set it explicitly, even " +
	"to `false`, for Terraform to own it.\n\n" +
	"Needs a Flightdeck that supports `send_ci_failures`. On an older one it reads as null and is never sent while " +
	"it is unset. Setting it there fails an update with an error that asks you to upgrade Flightdeck. On the apply " +
	"that creates the project it is a warning instead, so the new project isn't marked tainted, and the next plan " +
	"shows what is still to apply."

// sendCIFailuresDataSourceDescription is the data source attribute's
// documentation.
const sendCIFailuresDataSourceDescription = "Whether items Flightdeck files for failed CI runs go to an agent " +
	"without a person adding the agent label. Null on a Flightdeck older than the setting."

// sendErrorsDescription is the resource attribute's documentation.
const sendErrorsDescription = "Whether to send each work item one of Flightdeck's error alert rules files to an " +
	"agent, as `fix-error`, without waiting for a person to add the agent label. Off by default, so such an item " +
	"waits for a person to label it. It applies only while `fix-error` is one of the `kinds` and an agent label " +
	"is chosen (`label_id`), and only to items filed since it was last turned on, within the last 24 hours: " +
	"turning it on never sends the items already in the backlog. The error must be at `error` or `critical` " +
	"level and still active (not muted, snoozed or resolved), and the alert rule that filed the item must still " +
	"be on and still file work items. Flightdeck waits 15 minutes after an item is filed (or accepted, for an " +
	"item that waits in intake), so a person who sees the alert can take it first, and never sends one a person " +
	"is assigned to or has started. It sends each such item once, and only one item per error: after that, an " +
	"item goes again only when a person adds the agent label. The usual limits still hold, such as " +
	"`max_in_progress` and the budget. Turning it on or off is posted to the project's Slack updates. When " +
	"unset, the project's current value is kept; set it explicitly, even to `false`, for Terraform to own it.\n\n" +
	"Needs a Flightdeck that supports `send_errors`. On an older one it reads as null and is never sent while " +
	"it is unset. Setting it there fails an update with an error that asks you to upgrade Flightdeck. On the apply " +
	"that creates the project it is a warning instead, so the new project isn't marked tainted, and the next plan " +
	"shows what is still to apply."

// sendErrorsDataSourceDescription is the data source attribute's
// documentation.
const sendErrorsDataSourceDescription = "Whether items Flightdeck's error alert rules file go to an agent, as " +
	"`fix-error`, without a person adding the agent label. Null on a Flightdeck older than the setting."

// waitForChecksDescription is the resource attribute's documentation.
const waitForChecksDescription = "Whether an agent waits for its pull request's CI checks, and fixes a failure " +
	"itself, before it finishes. **On by default**, unlike the other switches here. Set it to `false` for a " +
	"repository whose CI takes longer than an agent should wait: AutoPilot is then asked not to wait for CI on " +
	"each pull request task, so the agent finishes once it has pushed, and Flightdeck deals with a failed run " +
	"itself: it re-runs the failed jobs once and, if they fail again, sends the item back to an agent to fix. Flightdeck " +
	"does that only for " +
	"workflows on the linked repository's CI workflows list (`ci_workflows` on `flightdeck_github_integration`), " +
	"so with this off, a failure in a workflow that isn't on the list is left for a person. It never stops agent " +
	"work from starting. When unset, the project's current value is kept; set it explicitly, even to `true`, for " +
	"Terraform to own it.\n\n" +
	"Needs a Flightdeck that supports `wait_for_checks`. On an older one it reads as null and is never sent while " +
	"it is unset. Setting it there fails an update with an error that asks you to upgrade Flightdeck. On the apply " +
	"that creates the project it is a warning instead, so the new project isn't marked tainted, and the next plan " +
	"shows what is still to apply."

// waitForChecksDataSourceDescription is the data source attribute's
// documentation.
const waitForChecksDataSourceDescription = "Whether an agent waits for its pull request's CI checks before it " +
	"finishes. Null on a Flightdeck older than the setting."

// agentWorkNewerSetting is a setting newer than the agent-work endpoint
// itself. A Flightdeck older than it has no such key on a read and refuses
// any write naming it, whatever its value, as an unknown key. So it reads as
// null there, is never sent while unset, and a write that names it to such a
// Flightdeck is reported on the attribute with a note to upgrade.
type agentWorkNewerSetting struct {
	// key is the setting's name in the API and in the block.
	key string
	// added is what the Flightdeck release that added the setting added, as in
	// "older than the release that added <added>".
	added string
	// unknownNote closes the error for a refused write whose refusal names key
	// as an unknown key, when a read could not tell whether this Flightdeck
	// knows it.
	unknownNote string
	// reported says whether a read came from a Flightdeck that knows the
	// setting: one that does always has the key.
	reported func(*client.AgentWork) bool
}

var (
	agentWorkModels = agentWorkNewerSetting{
		key: "models", added: "the choice of AI model for each kind of work",
		unknownNote: agentWorkModelsUnknownNote, reported: (*client.AgentWork).ReportsModels,
	}
	agentWorkAutoMerge = agentWorkNewerSetting{
		key: "auto_merge", added: "auto-merge for agent pull requests",
		unknownNote: agentWorkAutoMergeUnknownNote, reported: (*client.AgentWork).ReportsAutoMerge,
	}
	agentWorkSendCIFailures = agentWorkNewerSetting{
		key: "send_ci_failures", added: "sending CI failures to agents",
		unknownNote: agentWorkSendCIFailuresUnknownNote, reported: (*client.AgentWork).ReportsSendCIFailures,
	}
	agentWorkSendErrors = agentWorkNewerSetting{
		key: "send_errors", added: "sending errors to agents",
		unknownNote: agentWorkSendErrorsUnknownNote, reported: (*client.AgentWork).ReportsSendErrors,
	}
	agentWorkWaitForChecks = agentWorkNewerSetting{
		key: "wait_for_checks", added: "the choice of whether agents wait for CI",
		unknownNote: agentWorkWaitForChecksUnknownNote, reported: (*client.AgentWork).ReportsWaitForChecks,
	}
	// agentWorkNewerSettings are the newer settings, in the API's order. That
	// is not the order Flightdeck added them in: models came last, but the API
	// lists it next to kinds, and send_errors came before it, but the API
	// lists that next to send_ci_failures.
	agentWorkNewerSettings = []agentWorkNewerSetting{agentWorkModels, agentWorkAutoMerge, agentWorkSendCIFailures,
		agentWorkSendErrors, agentWorkWaitForChecks}
)

// agentWorkModelsUnknownNote is models' unknownNote.
const agentWorkModelsUnknownNote = "If the API calls models an unknown key, this Flightdeck is older than the " +
	"release that added the choice of AI model for each kind of work: upgrade Flightdeck first, or leave `models` " +
	"out of the agent_work block."

// agentWorkAutoMergeUnknownNote is auto_merge's unknownNote.
const agentWorkAutoMergeUnknownNote = "If the API calls auto_merge an unknown key, this Flightdeck is older than " +
	"the release that added auto-merge for agent pull requests: upgrade Flightdeck first, or leave `auto_merge` " +
	"out of the agent_work block."

// agentWorkSendCIFailuresUnknownNote is send_ci_failures' unknownNote.
const agentWorkSendCIFailuresUnknownNote = "If the API calls send_ci_failures an unknown key, this Flightdeck is " +
	"older than the release that added sending CI failures to agents: upgrade Flightdeck first, or leave " +
	"`send_ci_failures` out of the agent_work block."

// agentWorkSendErrorsUnknownNote is send_errors' unknownNote.
const agentWorkSendErrorsUnknownNote = "If the API calls send_errors an unknown key, this Flightdeck is older " +
	"than the release that added sending errors to agents: upgrade Flightdeck first, or leave `send_errors` out " +
	"of the agent_work block."

// agentWorkWaitForChecksUnknownNote is wait_for_checks' unknownNote.
const agentWorkWaitForChecksUnknownNote = "If the API calls wait_for_checks an unknown key, this Flightdeck is " +
	"older than the release that added the choice of whether agents wait for CI: upgrade Flightdeck first, or " +
	"leave `wait_for_checks` out of the agent_work block."

// agentWorkSettingRefused reports whether a refused write failed because it
// sent setting to a Flightdeck too old to know it, judged by current, a read
// made after the refusal. Such a Flightdeck refuses the key as unknown, in
// prose, so rather than match the wording the read is asked whether it
// reports the setting, the way terraform_managed and slack_channel.mode are
// checked.
func agentWorkSettingRefused(setting agentWorkNewerSetting, current *client.AgentWork, sent client.Fields, err error) bool {
	if current == nil || !client.IsValidation(err) {
		return false
	}
	if _, named := sent[setting.key]; !named {
		return false
	}
	return !setting.reported(current)
}

// agentWorkRefusedSettings is every newer setting agentWorkSettingRefused
// finds a refused write failed on, in the API's order. A write that sent
// both to a Flightdeck older than both failed on both.
func agentWorkRefusedSettings(current *client.AgentWork, sent client.Fields, err error) []agentWorkNewerSetting {
	var refused []agentWorkNewerSetting
	for _, setting := range agentWorkNewerSettings {
		if agentWorkSettingRefused(setting, current, sent, err) {
			refused = append(refused, setting)
		}
	}
	return refused
}

// agentWorkUnsupportedSettings is agentWorkRefusedSettings with a read of its
// own, for a refused update. It reads only when the write sent a newer
// setting. A read that fails cannot tell, and finds none: the caller falls
// back on the refusal's own words (agentWorkUnknownNotes).
func agentWorkUnsupportedSettings(ctx context.Context, c *client.Client, projectID int64, sent client.Fields, err error) []agentWorkNewerSetting {
	if !client.IsValidation(err) {
		return nil
	}
	sentNewer := false
	for _, setting := range agentWorkNewerSettings {
		if _, named := sent[setting.key]; named {
			sentNewer = true
		}
	}
	if !sentNewer {
		return nil
	}
	current, rerr := c.GetAgentWork(ctx, projectID)
	if rerr != nil {
		return nil
	}
	return agentWorkRefusedSettings(current, sent, err)
}

// settableList is where an unknown-key refusal starts listing the keys the
// endpoint takes: "unknown key: auto_merge (settable: enabled, kinds, ...)".
const settableList = " (settable: "

// withoutSettableList is a refusal's message without the list of settable
// keys an unknown-key refusal ends with.
func withoutSettableList(msg string) string {
	if i := strings.Index(msg, settableList); i >= 0 {
		return msg[:i]
	}
	return msg
}

// refusalUnknownKeys returns the keys a 422 invalid_attribute names as
// unknown, as Flightdeck words it: "unknown key: a (settable: ...)" or
// "unknown keys: a, b and 3 more (settable: ...)". Any other refusal names
// none.
func refusalUnknownKeys(err error) []string {
	if !client.HasCode(err, client.CodeInvalidAttribute) {
		return nil
	}
	msg := withoutSettableList(strings.TrimSpace(apiMessage(err)))
	var listed string
	switch {
	case strings.HasPrefix(msg, "unknown keys: "):
		listed = strings.TrimPrefix(msg, "unknown keys: ")
	case strings.HasPrefix(msg, "unknown key: "):
		listed = strings.TrimPrefix(msg, "unknown key: ")
	default:
		return nil
	}
	if i := strings.LastIndex(listed, " and "); i >= 0 && strings.HasSuffix(listed, " more") {
		listed = listed[:i]
	}
	return strings.Split(listed, ", ")
}

// refusesAsUnknown reports whether a refusal names key as an unknown key.
func refusesAsUnknown(err error, key string) bool {
	for _, unknown := range refusalUnknownKeys(err) {
		if unknown == key {
			return true
		}
	}
	return false
}

// agentWorkUnknownNotes is the upgrade note of each newer setting a refusal
// names as an unknown key, each after a blank line, or "" when it names none.
func agentWorkUnknownNotes(err error) string {
	notes := ""
	for _, setting := range agentWorkNewerSettings {
		if refusesAsUnknown(err, setting.key) {
			notes += "\n\n" + setting.unknownNote
		}
	}
	return notes
}

// addAgentWorkSettingUnsupported reports a write that sent a newer setting to
// a Flightdeck older than it, on the attribute: an error on an update, a
// warning on the apply that creates the project, for the reason
// agentWorkOnCreate gives. Flightdeck refused the whole write, so nothing in
// the block was saved.
func addAgentWorkSettingUnsupported(setting agentWorkNewerSetting, err error, apply agentWorkApply, diags *diag.Diagnostics) {
	at := path.Root("agent_work").AtName(setting.key)
	summary := "This Flightdeck does not support agent_work." + setting.key + " yet"
	if apply == agentWorkOnCreate {
		diags.AddAttributeWarning(at, summary,
			"The project was created, but this Flightdeck refused "+setting.key+" as an unknown setting: it is older "+
				"than the release that added "+setting.added+". None of this block's settings were saved. "+
				"The next plan shows them again, and the next apply fails on this until Flightdeck is upgraded or "+
				"`"+setting.key+"` is left out of the agent_work block.\n\nThe API said: "+apiMessage(err))
		return
	}
	diags.AddAttributeError(at, summary,
		"This Flightdeck refused "+setting.key+" as an unknown setting: it is older than the release that added "+
			setting.added+". Nothing was saved, including this block's other changes, so the "+
			"next plan still shows them.\n\nUpgrade Flightdeck first, then apply again. Until then, leave "+
			"`"+setting.key+"` out of the agent_work block: on this Flightdeck it reads as null, and the provider "+
			"never sends it while it is unset.\n\nThe API said: "+apiMessage(err))
}
