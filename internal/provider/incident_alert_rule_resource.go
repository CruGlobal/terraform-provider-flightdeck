package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// An incident alert rule is the sibling of an error alert rule, and close
// enough to it to be confusing. The differences are real and all live in the
// vocabulary: the triggers are incident lifecycle events rather than error
// ones, the condition gates on incident SEVERITY (`min_severity`) rather than
// error level, and the action has neither `open_incident` (the incident is
// already open — that is what fired the rule) nor `escalation_policy_id`.
// What it adds is `priority_map`, the severity-to-priority table applied to
// anything the rule files.
//
// Two merge behaviours sit in one resource, in opposite directions:
//
//   - TOP-LEVEL keys keep their stored value when absent, so `enabled` has to
//     be re-sent on every write to be managed at all. It is Optional+Computed
//     with no framework default for that reason: a default would invent a
//     value for an unset attribute and write it.
//   - `condition` and `action` REPLACE whatever is stored whenever they are
//     sent. A partial action object drops every flag it does not name, so both
//     are always sent whole and the configuration is authoritative.
//
// `priority_map` needs one more turn of the screw. The API stores only the
// rows that differ from the default, so a row set to its own default is
// dropped from `action.priority_map` — reading that back would diff forever.
// The rule's top-level `priority_map` reports the EFFECTIVE table instead, and
// state keeps the subset of it the configuration names, exactly as the
// project's `features` map does.

var (
	_ resource.Resource                   = &incidentAlertRuleResource{}
	_ resource.ResourceWithConfigure      = &incidentAlertRuleResource{}
	_ resource.ResourceWithImportState    = &incidentAlertRuleResource{}
	_ resource.ResourceWithValidateConfig = &incidentAlertRuleResource{}
)

// NewIncidentAlertRuleResource returns the flightdeck_incident_alert_rule resource.
func NewIncidentAlertRuleResource() resource.Resource { return &incidentAlertRuleResource{} }

type incidentAlertRuleResource struct {
	client *client.Client
}

type incidentAlertRuleModel struct {
	ID          types.Int64  `tfsdk:"id"`
	ProjectID   types.Int64  `tfsdk:"project_id"`
	Name        types.String `tfsdk:"name"`
	Enabled     types.Bool   `tfsdk:"enabled"`
	Trigger     types.String `tfsdk:"trigger"`
	Condition   types.Object `tfsdk:"condition"`
	Action      types.Object `tfsdk:"action"`
	LockVersion types.Int64  `tfsdk:"lock_version"`
}

type incidentConditionModel struct {
	MinSeverity   types.String `tfsdk:"min_severity"`
	Count         types.Int64  `tfsdk:"count"`
	WindowMinutes types.Int64  `tfsdk:"window_minutes"`
}

type incidentActionModel struct {
	NotifySlack    types.Bool   `tfsdk:"notify_slack"`
	NotifyEmail    types.Bool   `tfsdk:"notify_email"`
	CreateWorkItem types.Bool   `tfsdk:"create_work_item"`
	FileIntake     types.Bool   `tfsdk:"file_intake"`
	NotifyWebhook  types.Bool   `tfsdk:"notify_webhook"`
	WebhookURL     types.String `tfsdk:"webhook_url"`
	PriorityMap    types.Map    `tfsdk:"priority_map"`
}

var incidentConditionAttrTypes = map[string]attr.Type{
	"min_severity":   types.StringType,
	"count":          types.Int64Type,
	"window_minutes": types.Int64Type,
}

var incidentActionAttrTypes = map[string]attr.Type{
	"notify_slack":     types.BoolType,
	"notify_email":     types.BoolType,
	"create_work_item": types.BoolType,
	"file_intake":      types.BoolType,
	"notify_webhook":   types.BoolType,
	"webhook_url":      types.StringType,
	"priority_map":     types.MapType{ElemType: types.StringType},
}

// Incident condition windows are bounded by the API: repeats are folded per
// five-minute note, and a year is the ceiling.
const (
	minIncidentWindowMinutes = 5
	maxIncidentWindowMinutes = 525600
)

func (r *incidentAlertRuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_incident_alert_rule"
}

func (r *incidentAlertRuleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *incidentAlertRuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	emptyCondition := types.ObjectValueMust(incidentConditionAttrTypes, map[string]attr.Value{
		"min_severity": types.StringNull(), "count": types.Int64Null(), "window_minutes": types.Int64Null(),
	})
	boolFlag := func(desc string) schema.Attribute {
		return schema.BoolAttribute{
			MarkdownDescription: desc,
			Optional:            true,
			Computed:            true,
			Default:             booldefault.StaticBool(false),
		}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an incident alert rule in a Flightdeck project: when an incident *trigger* fires for " +
			"an incident that satisfies the *conditions*, run the enabled *actions*. It is the incident-lifecycle " +
			"sibling of `flightdeck_error_alert_rule` and does not share its vocabulary — the triggers are " +
			"`incident_opened` and `incident_repeated`, the condition gates on `min_severity` rather than an error " +
			"level, and the action has no `open_incident` or `escalation_policy_id` (the incident already exists).\n\n" +
			"At least one action must be enabled. `notify_webhook` requires `webhook_url`. Creating a rule requires " +
			"the project's `incidents` feature to be enabled; the gate is on create only, so an existing rule keeps " +
			"working if the feature is later switched off.\n\n" +
			"`condition` and `action` are sent whole and **replace** what the API has stored, so the configuration is " +
			"authoritative for both. Every other attribute keeps its current value when you leave it out — see " +
			"`enabled`.\n\n" +
			"Import with `<project_id>/<rule_id>`: `terraform import flightdeck_incident_alert_rule.paged 42/12`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				MarkdownDescription: "Numeric id of the rule.",
				Computed:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"project_id": schema.Int64Attribute{
				MarkdownDescription: "Id of the project. Changing it replaces the rule.",
				Required:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Display name.",
				Required:            true,
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether the rule is evaluated. A new rule is enabled. When unset, the rule's current " +
					"value is kept — including one changed in the console — so set it explicitly for Terraform to own " +
					"it; removing the line leaves the current value in place.",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"trigger": schema.StringAttribute{
				MarkdownDescription: "What fires the rule: `incident_opened` (an incident is opened) or " +
					"`incident_repeated` (`condition.count` further events fold into an open incident within " +
					"`condition.window_minutes`).",
				Required:   true,
				Validators: []validator.String{stringvalidator.OneOf(client.IncidentAlertTriggers...)},
			},
			"condition": schema.SingleNestedAttribute{
				MarkdownDescription: "Conditions, all of which must hold (omit for none). Sent whole: the API replaces " +
					"the stored condition with what is here, so removing a key clears it.",
				Optional: true,
				Computed: true,
				Default:  objectdefault.StaticValue(emptyCondition),
				Attributes: map[string]schema.Attribute{
					"min_severity": schema.StringAttribute{
						MarkdownDescription: "Only incidents at or above this severity: one of `" +
							strings.Join(client.IncidentSeverities, "`, `") + "`.",
						Optional:   true,
						Validators: []validator.String{stringvalidator.OneOf(client.IncidentSeverities...)},
					},
					"count": schema.Int64Attribute{
						MarkdownDescription: "How many repeat events must fold in for the `incident_repeated` trigger. " +
							"Required for that trigger; accepted but inert for `incident_opened`.",
						Optional:   true,
						Validators: []validator.Int64{int64validator.AtLeast(1)},
					},
					"window_minutes": schema.Int64Attribute{
						MarkdownDescription: fmt.Sprintf("Window in minutes for the `incident_repeated` trigger. Between "+
							"%d (repeat events are folded per five-minute note, so a shorter window cannot mean anything) "+
							"and %d (one year).", minIncidentWindowMinutes, maxIncidentWindowMinutes),
						Optional:   true,
						Validators: []validator.Int64{int64validator.Between(minIncidentWindowMinutes, maxIncidentWindowMinutes)},
					},
				},
			},
			"action": schema.SingleNestedAttribute{
				MarkdownDescription: "Actions to run; enable at least one. Sent whole: the API replaces the stored " +
					"action with what is here, so a flag you remove is turned off rather than left alone.",
				Required: true,
				Attributes: map[string]schema.Attribute{
					"notify_slack":     boolFlag("Post to the project's Slack channel."),
					"notify_email":     boolFlag("Email the project's members."),
					"create_work_item": boolFlag("Create a work item for the incident."),
					"file_intake":      boolFlag("File an intake request for triage."),
					"notify_webhook":   boolFlag("POST the alert to `webhook_url`."),
					"webhook_url": schema.StringAttribute{
						MarkdownDescription: "http(s) URL for `notify_webhook`. Internal and private addresses are rejected by the API.",
						Optional:            true,
						Validators: []validator.String{
							stringvalidator.RegexMatches(httpURLPattern, "must be an http(s) URL"),
						},
					},
					"priority_map": schema.MapAttribute{
						MarkdownDescription: "Priority to give whatever this rule files, per incident severity. Keys are " +
							"`" + strings.Join(client.IncidentSeverities, "`, `") + "`; values are `" +
							strings.Join(client.IncidentPriorities, "`, `") + "`. The default table is " +
							"`critical` → `urgent`, `error` → `high`, `warning` → `medium`, `info` → `low`.\n\n" +
							"Declare only the severities you want to change: state records the severities you name and " +
							"leaves the rest at their default, so a partial map is not a perpetual diff. Naming a " +
							"severity and giving it its own default value is fine — the API stores only genuine " +
							"overrides, but the effective value is what is read back.",
						Optional:    true,
						ElementType: types.StringType,
						Validators: []validator.Map{
							mapvalidator.KeysAre(stringvalidator.OneOf(client.IncidentSeverities...)),
							mapvalidator.ValueStringsAre(stringvalidator.OneOf(client.IncidentPriorities...)),
						},
					},
				},
			},
			"lock_version": schema.Int64Attribute{
				MarkdownDescription: "Optimistic-locking version the API bumps on every change. Sent as `If-Match` on updates and deletes.",
				Computed:            true,
			},
		},
	}
}

// ValidateConfig enforces the cross-attribute rules the API applies at save
// time, so a misconfiguration fails at plan rather than apply.
func (r *incidentAlertRuleResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg incidentAlertRuleModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !cfg.Action.IsNull() && !cfg.Action.IsUnknown() {
		var action incidentActionModel
		resp.Diagnostics.Append(cfg.Action.As(ctx, &action, objectAsOptions)...)
		if resp.Diagnostics.HasError() {
			return
		}
		flags := []types.Bool{action.NotifySlack, action.NotifyEmail, action.CreateWorkItem, action.FileIntake, action.NotifyWebhook}
		anyUnknown, anyTrue := false, false
		for _, f := range flags {
			if f.IsUnknown() {
				anyUnknown = true
			} else if f.ValueBool() {
				anyTrue = true
			}
		}
		if !anyUnknown && !anyTrue {
			resp.Diagnostics.AddAttributeError(path.Root("action"), "No action enabled",
				"At least one of notify_slack, notify_email, create_work_item, file_intake or notify_webhook must be true.")
		}
		if action.NotifyWebhook.ValueBool() && action.WebhookURL.IsNull() {
			resp.Diagnostics.AddAttributeError(path.Root("action").AtName("webhook_url"), "webhook_url required",
				"`notify_webhook` is enabled, so `webhook_url` must be set.")
		}
		if !action.WebhookURL.IsNull() && !action.NotifyWebhook.IsUnknown() && !action.NotifyWebhook.ValueBool() {
			resp.Diagnostics.AddAttributeError(path.Root("action").AtName("webhook_url"), "webhook_url requires notify_webhook",
				"`webhook_url` is set but `notify_webhook` is not true, so it would never be used; set both or neither.")
		}
	}
	// The repeated trigger counts events, so it needs something to count.
	if cfg.Trigger.ValueString() == "incident_repeated" && !cfg.Condition.IsUnknown() {
		var cond incidentConditionModel
		if !cfg.Condition.IsNull() {
			resp.Diagnostics.Append(cfg.Condition.As(ctx, &cond, objectAsOptions)...)
		}
		if cond.Count.IsNull() && !cond.Count.IsUnknown() {
			resp.Diagnostics.AddAttributeError(path.Root("condition").AtName("count"), "count required for incident_repeated",
				"The `incident_repeated` trigger fires after a number of repeat events, so `condition.count` must be set (at least 1).")
		}
	}
}

// incidentRuleFields builds the write body. condition and action are always
// sent whole, because the API replaces them; every other key is sent only when
// configured, because the API keeps what it has.
func incidentRuleFields(ctx context.Context, plan *incidentAlertRuleModel, diags *diag.Diagnostics) client.Fields {
	fields := client.Fields{
		"name":    plan.Name.ValueString(),
		"trigger": plan.Trigger.ValueString(),
	}
	if !plan.Enabled.IsNull() && !plan.Enabled.IsUnknown() {
		fields["enabled"] = plan.Enabled.ValueBool()
	}

	condition := map[string]any{}
	if !plan.Condition.IsNull() && !plan.Condition.IsUnknown() {
		var c incidentConditionModel
		diags.Append(plan.Condition.As(ctx, &c, objectAsOptions)...)
		if !c.MinSeverity.IsNull() {
			condition["min_severity"] = c.MinSeverity.ValueString()
		}
		if !c.Count.IsNull() {
			condition["count"] = c.Count.ValueInt64()
		}
		if !c.WindowMinutes.IsNull() {
			condition["window_minutes"] = c.WindowMinutes.ValueInt64()
		}
	}
	fields["condition"] = condition

	action := map[string]any{}
	if !plan.Action.IsNull() && !plan.Action.IsUnknown() {
		var a incidentActionModel
		diags.Append(plan.Action.As(ctx, &a, objectAsOptions)...)
		action["notify_slack"] = a.NotifySlack.ValueBool()
		action["notify_email"] = a.NotifyEmail.ValueBool()
		action["create_work_item"] = a.CreateWorkItem.ValueBool()
		action["file_intake"] = a.FileIntake.ValueBool()
		action["notify_webhook"] = a.NotifyWebhook.ValueBool()
		if !a.WebhookURL.IsNull() {
			action["webhook_url"] = a.WebhookURL.ValueString()
		}
		if !a.PriorityMap.IsNull() && !a.PriorityMap.IsUnknown() {
			var priorities map[string]string
			diags.Append(a.PriorityMap.ElementsAs(ctx, &priorities, false)...)
			action["priority_map"] = priorities
		}
	}
	fields["action"] = action
	return fields
}

// incidentPriorityKeys says which severities to record in state.
type incidentPriorityKeys int

const (
	// incidentPrioritiesManaged keeps exactly the severities the configuration
	// names, so a rule that overrides one severity never diffs against the
	// other three. No configured severities means a null map.
	incidentPrioritiesManaged incidentPriorityKeys = iota
	// incidentPrioritiesAll keeps every severity the API resolves (import,
	// where there is no configuration to defer to).
	incidentPrioritiesAll
)

// incidentRuleToModel maps an API rule into state. wanted supplies the
// priority_map severities to keep when scope is incidentPrioritiesManaged.
func incidentRuleToModel(ctx context.Context, rule *client.IncidentAlertRule, wanted types.Map, scope incidentPriorityKeys, diags *diag.Diagnostics) incidentAlertRuleModel {
	condition, d := types.ObjectValue(incidentConditionAttrTypes, map[string]attr.Value{
		"min_severity":   rawString("condition.min_severity", rule.Condition["min_severity"], diags),
		"count":          rawInt64("condition.count", rule.Condition["count"], diags),
		"window_minutes": rawInt64("condition.window_minutes", rule.Condition["window_minutes"], diags),
	})
	diags.Append(d...)

	action, d := types.ObjectValue(incidentActionAttrTypes, map[string]attr.Value{
		"notify_slack":     rawBool(rule.Action["notify_slack"]),
		"notify_email":     rawBool(rule.Action["notify_email"]),
		"create_work_item": rawBool(rule.Action["create_work_item"]),
		"file_intake":      rawBool(rule.Action["file_intake"]),
		"notify_webhook":   rawBool(rule.Action["notify_webhook"]),
		"webhook_url":      rawString("action.webhook_url", rule.Action["webhook_url"], diags),
		"priority_map":     incidentPriorityMap(ctx, rule.PriorityMap, wanted, scope, diags),
	})
	diags.Append(d...)

	return incidentAlertRuleModel{
		ID:          types.Int64Value(rule.ID),
		ProjectID:   types.Int64Value(rule.ProjectID),
		Name:        types.StringValue(rule.Name),
		Enabled:     types.BoolValue(rule.Enabled),
		Trigger:     types.StringValue(rule.Trigger),
		Condition:   condition,
		Action:      action,
		LockVersion: types.Int64Value(rule.LockVersion),
	}
}

// incidentPriorityMap narrows the rule's effective severity-to-priority table
// to the severities being managed. Reading the effective table rather than the
// stored overrides is what lets a configuration name a severity and give it
// its own default value without diffing forever: the API drops that row as
// "not an override", but the effective table still reports it.
func incidentPriorityMap(ctx context.Context, effective map[string]string, wanted types.Map, scope incidentPriorityKeys, diags *diag.Diagnostics) types.Map {
	keep := map[string]bool{}
	switch scope {
	case incidentPrioritiesManaged:
		if wanted.IsNull() || wanted.IsUnknown() {
			return types.MapNull(types.StringType)
		}
		for k := range wanted.Elements() {
			keep[k] = true
		}
	case incidentPrioritiesAll:
		for _, k := range client.IncidentSeverities {
			keep[k] = true
		}
	}
	selected := map[string]string{}
	for k, v := range effective {
		if keep[k] {
			selected[k] = v
		}
	}
	out, d := types.MapValueFrom(ctx, types.StringType, selected)
	diags.Append(d...)
	return out
}

// managedPriorityMap pulls the priority_map out of a plan or a prior state:
// the severities this resource is managing, and so the ones to record.
func managedPriorityMap(ctx context.Context, m *incidentAlertRuleModel, diags *diag.Diagnostics) types.Map {
	if m.Action.IsNull() || m.Action.IsUnknown() {
		return types.MapNull(types.StringType)
	}
	var a incidentActionModel
	diags.Append(m.Action.As(ctx, &a, objectAsOptions)...)
	return a.PriorityMap
}

func (r *incidentAlertRuleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan incidentAlertRuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID := plan.ProjectID.ValueInt64()
	fields := incidentRuleFields(ctx, &plan, &resp.Diagnostics)
	wanted := managedPriorityMap(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	created, err := r.client.CreateIncidentAlertRule(ctx, projectID, fields, client.PayloadKey("incident_alert_rule", strconv.FormatInt(projectID, 10), fields))
	if err != nil {
		if client.IsValidation(err) && strings.Contains(apiMessage(err), "Incident management") {
			resp.Diagnostics.AddError("The project's incidents feature is off",
				apiMessage(err)+"\n\nSet `incidents = true` in the project's `features` map, or enable it in Project Settings.")
			return
		}
		addAPIError(&resp.Diagnostics, "Error creating Flightdeck incident alert rule", err)
		return
	}
	state := incidentRuleToModel(ctx, created, wanted, incidentPrioritiesManaged, &resp.Diagnostics)
	reconcileIncidentAction(ctx, &state, &plan, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *incidentAlertRuleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state incidentAlertRuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	rule, err := r.client.GetIncidentAlertRule(ctx, state.ProjectID.ValueInt64(), state.ID.ValueInt64())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		addAPIError(&resp.Diagnostics, "Error reading Flightdeck incident alert rule", err)
		return
	}
	wanted := managedPriorityMap(ctx, &state, &resp.Diagnostics)
	newState := incidentRuleToModel(ctx, rule, wanted, incidentPrioritiesManaged, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *incidentAlertRuleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state incidentAlertRuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	fields := incidentRuleFields(ctx, &plan, &resp.Diagnostics)
	wanted := managedPriorityMap(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID, id := state.ProjectID.ValueInt64(), state.ID.ValueInt64()
	updated, err := r.client.UpdateIncidentAlertRule(ctx, projectID, id, fields, state.LockVersion.ValueInt64())
	if err != nil {
		if client.IsStale(err) {
			var current *int64
			if fresh, rerr := r.client.GetIncidentAlertRule(ctx, projectID, id); rerr == nil {
				current = &fresh.LockVersion
			}
			addStaleError(&resp.Diagnostics, fmt.Sprintf("Incident alert rule %q", state.Name.ValueString()), state.LockVersion.ValueInt64(), current, err)
			return
		}
		addAPIError(&resp.Diagnostics, "Error updating Flightdeck incident alert rule", err)
		return
	}
	newState := incidentRuleToModel(ctx, updated, wanted, incidentPrioritiesManaged, &resp.Diagnostics)
	reconcileIncidentAction(ctx, &newState, &plan, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// reconcileIncidentAction keeps a planned webhook_url the server dropped, so
// the apply is consistent; a warning names the field and the next refresh
// shows the drop as a diff. The priority_map needs no such treatment: state
// records the effective value of each severity the configuration names, which
// is what the server reports back whether or not it stored an override.
func reconcileIncidentAction(ctx context.Context, state, plan *incidentAlertRuleModel, diags *diag.Diagnostics) {
	if plan.Action.IsNull() || plan.Action.IsUnknown() || state.Action.IsNull() {
		return
	}
	var wanted, got incidentActionModel
	diags.Append(plan.Action.As(ctx, &wanted, objectAsOptions)...)
	diags.Append(state.Action.As(ctx, &got, objectAsOptions)...)
	if wanted.WebhookURL.IsNull() || wanted.WebhookURL.IsUnknown() || !got.WebhookURL.IsNull() {
		return
	}
	got.WebhookURL = wanted.WebhookURL
	obj, d := types.ObjectValueFrom(ctx, incidentActionAttrTypes, got)
	diags.Append(d...)
	state.Action = obj
	diags.AddWarning("Flightdeck did not store every action value",
		"The API dropped action.webhook_url. State records the configured value; the next `terraform plan` will show the difference.")
}

func (r *incidentAlertRuleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state incidentAlertRuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID, id := state.ProjectID.ValueInt64(), state.ID.ValueInt64()
	err := deleteWithIfMatch(ctx, state.LockVersion.ValueInt64(),
		func(ctx context.Context, lv int64) error {
			return r.client.DeleteIncidentAlertRule(ctx, projectID, id, lv)
		},
		func(ctx context.Context) (int64, error) {
			fresh, err := r.client.GetIncidentAlertRule(ctx, projectID, id)
			if err != nil {
				return 0, err
			}
			return fresh.LockVersion, nil
		})
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error deleting Flightdeck incident alert rule", err)
	}
}

// ImportState accepts `<project_id>/<rule_id>`: rules are addressed within
// their project. With no configuration to defer to, every severity of the
// priority table is recorded.
func (r *incidentAlertRuleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(strings.TrimSpace(req.ID), "/")
	if len(parts) != 2 {
		resp.Diagnostics.AddError("Invalid import id", fmt.Sprintf("Expected <project_id>/<rule_id> (for example 42/12), got %q.", req.ID))
		return
	}
	projectID, ok := parseImportID(parts[0], "project", &resp.Diagnostics)
	if !ok {
		return
	}
	id, ok := parseImportID(parts[1], "incident alert rule", &resp.Diagnostics)
	if !ok {
		return
	}
	rule, err := r.client.GetIncidentAlertRule(ctx, projectID, id)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error importing Flightdeck incident alert rule", err)
		return
	}
	state := incidentRuleToModel(ctx, rule, types.MapNull(types.StringType), incidentPrioritiesAll, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
