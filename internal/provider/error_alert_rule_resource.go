package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
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

var (
	_ resource.Resource                   = &errorAlertRuleResource{}
	_ resource.ResourceWithConfigure      = &errorAlertRuleResource{}
	_ resource.ResourceWithImportState    = &errorAlertRuleResource{}
	_ resource.ResourceWithValidateConfig = &errorAlertRuleResource{}
)

// NewErrorAlertRuleResource returns the flightdeck_error_alert_rule resource.
func NewErrorAlertRuleResource() resource.Resource { return &errorAlertRuleResource{} }

type errorAlertRuleResource struct {
	client *client.Client
}

type errorAlertRuleModel struct {
	ID          types.Int64  `tfsdk:"id"`
	ProjectID   types.Int64  `tfsdk:"project_id"`
	Name        types.String `tfsdk:"name"`
	Enabled     types.Bool   `tfsdk:"enabled"`
	Trigger     types.String `tfsdk:"trigger"`
	Condition   types.Object `tfsdk:"condition"`
	Action      types.Object `tfsdk:"action"`
	LockVersion types.Int64  `tfsdk:"lock_version"`
}

type alertConditionModel struct {
	MinLevel      types.String `tfsdk:"min_level"`
	Environment   types.String `tfsdk:"environment"`
	Count         types.Int64  `tfsdk:"count"`
	WindowMinutes types.Int64  `tfsdk:"window_minutes"`
}

type alertActionModel struct {
	NotifySlack        types.Bool   `tfsdk:"notify_slack"`
	NotifyEmail        types.Bool   `tfsdk:"notify_email"`
	CreateWorkItem     types.Bool   `tfsdk:"create_work_item"`
	FileIntake         types.Bool   `tfsdk:"file_intake"`
	NotifyWebhook      types.Bool   `tfsdk:"notify_webhook"`
	OpenIncident       types.Bool   `tfsdk:"open_incident"`
	WebhookURL         types.String `tfsdk:"webhook_url"`
	EscalationPolicyID types.Int64  `tfsdk:"escalation_policy_id"`
}

var alertConditionAttrTypes = map[string]attr.Type{
	"min_level":      types.StringType,
	"environment":    types.StringType,
	"count":          types.Int64Type,
	"window_minutes": types.Int64Type,
}

var alertActionAttrTypes = map[string]attr.Type{
	"notify_slack":         types.BoolType,
	"notify_email":         types.BoolType,
	"create_work_item":     types.BoolType,
	"file_intake":          types.BoolType,
	"notify_webhook":       types.BoolType,
	"open_incident":        types.BoolType,
	"webhook_url":          types.StringType,
	"escalation_policy_id": types.Int64Type,
}

func (r *errorAlertRuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_error_alert_rule"
}

func (r *errorAlertRuleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *errorAlertRuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	emptyCondition := types.ObjectValueMust(alertConditionAttrTypes, map[string]attr.Value{
		"min_level": types.StringNull(), "environment": types.StringNull(),
		"count": types.Int64Null(), "window_minutes": types.Int64Null(),
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
		MarkdownDescription: "Manages an error alert rule in a Flightdeck project: when a *trigger* fires for an error group " +
			"that satisfies the *conditions*, run the enabled *actions*.\n\n" +
			"At least one action must be enabled. `notify_webhook` requires `webhook_url`; `open_incident` requires " +
			"the project's `incidents` feature to be enabled, and `escalation_policy_id` is only honoured alongside it. " +
			"Condition and action keys are validated against the API's allowlists.\n\n" +
			"Import with `<project_id>/<rule_id>`: `terraform import flightdeck_error_alert_rule.new_errors 42/12`.",
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
					"value is kept (so importing a disabled rule does not plan to enable it).",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"trigger": schema.StringAttribute{
				MarkdownDescription: "What fires the rule: `new_group` (a never-seen error), `regression` (a resolved error " +
					"recurs) or `occurrence_threshold` (`condition.count` occurrences within `condition.window_minutes`).",
				Required:   true,
				Validators: []validator.String{stringvalidator.OneOf(client.ErrorAlertTriggers...)},
			},
			"condition": schema.SingleNestedAttribute{
				MarkdownDescription: "Conditions, all of which must hold (omit for none).",
				Optional:            true,
				Computed:            true,
				Default:             objectdefault.StaticValue(emptyCondition),
				Attributes: map[string]schema.Attribute{
					"min_level": schema.StringAttribute{
						MarkdownDescription: "Minimum error level: one of `" + strings.Join(client.ErrorLevels, "`, `") + "`.",
						Optional:            true,
						Validators:          []validator.String{stringvalidator.OneOf(client.ErrorLevels...)},
					},
					"environment": schema.StringAttribute{
						MarkdownDescription: "Only errors reported from this environment (for example `production`).",
						Optional:            true,
					},
					"count": schema.Int64Attribute{
						MarkdownDescription: "Occurrence count for the `occurrence_threshold` trigger.",
						Optional:            true,
					},
					"window_minutes": schema.Int64Attribute{
						MarkdownDescription: "Window in minutes for the `occurrence_threshold` trigger.",
						Optional:            true,
					},
				},
			},
			"action": schema.SingleNestedAttribute{
				MarkdownDescription: "Actions to run; enable at least one.",
				Required:            true,
				Attributes: map[string]schema.Attribute{
					"notify_slack":     boolFlag("Post to the project's Slack channel."),
					"notify_email":     boolFlag("Email the project's members."),
					"create_work_item": boolFlag("Create a work item for the error group."),
					"file_intake":      boolFlag("File an intake request for triage."),
					"notify_webhook":   boolFlag("POST the alert to `webhook_url`."),
					"open_incident":    boolFlag("Open an incident (requires the project's `incidents` feature)."),
					"webhook_url": schema.StringAttribute{
						MarkdownDescription: "http(s) URL for `notify_webhook`. Internal and private addresses are rejected by the API.",
						Optional:            true,
						Validators: []validator.String{
							stringvalidator.RegexMatches(httpURLPattern, "must be an http(s) URL"),
						},
					},
					"escalation_policy_id": schema.Int64Attribute{
						MarkdownDescription: "Escalation policy (in this project) that `open_incident` routes to. Omit to open an unrouted incident.",
						Optional:            true,
					},
				},
			},
			"lock_version": schema.Int64Attribute{
				MarkdownDescription: "Optimistic-locking version the API bumps on every change. Sent as `If-Match` on updates.",
				Computed:            true,
			},
		},
	}
}

// ValidateConfig enforces the cross-attribute rules the API applies at save
// time, so a misconfiguration fails at plan rather than apply.
func (r *errorAlertRuleResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg errorAlertRuleModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() || cfg.Action.IsNull() || cfg.Action.IsUnknown() {
		return
	}
	var action alertActionModel
	resp.Diagnostics.Append(cfg.Action.As(ctx, &action, objectAsOptions)...)
	if resp.Diagnostics.HasError() {
		return
	}
	flags := []types.Bool{action.NotifySlack, action.NotifyEmail, action.CreateWorkItem, action.FileIntake, action.NotifyWebhook, action.OpenIncident}
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
			"At least one of notify_slack, notify_email, create_work_item, file_intake, notify_webhook or open_incident must be true.")
	}
	if action.NotifyWebhook.ValueBool() && action.WebhookURL.IsNull() {
		resp.Diagnostics.AddAttributeError(path.Root("action").AtName("webhook_url"), "webhook_url required",
			"`notify_webhook` is enabled, so `webhook_url` must be set.")
	}
	if !action.WebhookURL.IsNull() && !action.NotifyWebhook.IsUnknown() && !action.NotifyWebhook.ValueBool() {
		resp.Diagnostics.AddAttributeError(path.Root("action").AtName("webhook_url"), "webhook_url requires notify_webhook",
			"`webhook_url` is set but `notify_webhook` is not true, so it would never be used; set both or neither.")
	}
	if !action.EscalationPolicyID.IsNull() && !action.OpenIncident.IsUnknown() && !action.OpenIncident.ValueBool() {
		resp.Diagnostics.AddAttributeError(path.Root("action").AtName("escalation_policy_id"), "escalation_policy_id requires open_incident",
			"The API only stores `escalation_policy_id` when `open_incident` is true; set both or neither.")
	}
}

func alertRuleFields(ctx context.Context, plan *errorAlertRuleModel, diags *diag.Diagnostics) client.Fields {
	fields := client.Fields{
		"name":    plan.Name.ValueString(),
		"trigger": plan.Trigger.ValueString(),
	}
	if !plan.Enabled.IsNull() && !plan.Enabled.IsUnknown() {
		fields["enabled"] = plan.Enabled.ValueBool()
	}

	condition := map[string]any{}
	if !plan.Condition.IsNull() && !plan.Condition.IsUnknown() {
		var c alertConditionModel
		diags.Append(plan.Condition.As(ctx, &c, objectAsOptions)...)
		if !c.MinLevel.IsNull() {
			condition["min_level"] = c.MinLevel.ValueString()
		}
		if !c.Environment.IsNull() {
			condition["environment"] = c.Environment.ValueString()
		}
		if !c.Count.IsNull() {
			condition["count"] = c.Count.ValueInt64()
		}
		if !c.WindowMinutes.IsNull() {
			condition["window_minutes"] = c.WindowMinutes.ValueInt64()
		}
	}
	// Always sent, so removing a condition from configuration clears it.
	fields["condition"] = condition

	action := map[string]any{}
	if !plan.Action.IsNull() && !plan.Action.IsUnknown() {
		var a alertActionModel
		diags.Append(plan.Action.As(ctx, &a, objectAsOptions)...)
		action["notify_slack"] = a.NotifySlack.ValueBool()
		action["notify_email"] = a.NotifyEmail.ValueBool()
		action["create_work_item"] = a.CreateWorkItem.ValueBool()
		action["file_intake"] = a.FileIntake.ValueBool()
		action["notify_webhook"] = a.NotifyWebhook.ValueBool()
		action["open_incident"] = a.OpenIncident.ValueBool()
		if !a.WebhookURL.IsNull() {
			action["webhook_url"] = a.WebhookURL.ValueString()
		}
		if !a.EscalationPolicyID.IsNull() {
			action["escalation_policy_id"] = a.EscalationPolicyID.ValueInt64()
		}
	}
	fields["action"] = action
	return fields
}

func alertRuleToModel(rule *client.ErrorAlertRule, diags *diag.Diagnostics) errorAlertRuleModel {
	condition, d := types.ObjectValue(alertConditionAttrTypes, map[string]attr.Value{
		"min_level":      rawString("condition.min_level", rule.Condition["min_level"], diags),
		"environment":    rawString("condition.environment", rule.Condition["environment"], diags),
		"count":          rawInt64("condition.count", rule.Condition["count"], diags),
		"window_minutes": rawInt64("condition.window_minutes", rule.Condition["window_minutes"], diags),
	})
	diags.Append(d...)
	action, d := types.ObjectValue(alertActionAttrTypes, map[string]attr.Value{
		"notify_slack":         rawBool(rule.Action["notify_slack"]),
		"notify_email":         rawBool(rule.Action["notify_email"]),
		"create_work_item":     rawBool(rule.Action["create_work_item"]),
		"file_intake":          rawBool(rule.Action["file_intake"]),
		"notify_webhook":       rawBool(rule.Action["notify_webhook"]),
		"open_incident":        rawBool(rule.Action["open_incident"]),
		"webhook_url":          rawString("action.webhook_url", rule.Action["webhook_url"], diags),
		"escalation_policy_id": rawInt64("action.escalation_policy_id", rule.Action["escalation_policy_id"], diags),
	})
	diags.Append(d...)
	return errorAlertRuleModel{
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

// The API stores condition/action values as whatever JSON the writer sent, so
// the readers below accept the natural type and its string spelling — and
// nothing else: a value of the wrong shape is an error naming the field, not a
// silent truncation or a JSON fragment smuggled into a string.

func rawString(field string, raw json.RawMessage, diags *diag.Diagnostics) types.String {
	if len(raw) == 0 || string(raw) == "null" {
		return types.StringNull()
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		diags.AddError("Unexpected value from Flightdeck",
			fmt.Sprintf("%s should be a string but the API returned %s.", field, strings.TrimSpace(string(raw))))
		return types.StringNull()
	}
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}

func rawInt64(field string, raw json.RawMessage, diags *diag.Diagnostics) types.Int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return types.Int64Null()
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		if i, err := n.Int64(); err == nil {
			return types.Int64Value(i)
		}
		diags.AddError("Unexpected value from Flightdeck",
			fmt.Sprintf("%s should be an integer but the API returned %s.", field, n.String()))
		return types.Int64Null()
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if i, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			return types.Int64Value(i)
		}
	}
	diags.AddError("Unexpected value from Flightdeck",
		fmt.Sprintf("%s should be an integer but the API returned %s.", field, strings.TrimSpace(string(raw))))
	return types.Int64Null()
}

func rawBool(raw json.RawMessage) types.Bool {
	if len(raw) == 0 || string(raw) == "null" {
		return types.BoolValue(false)
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return types.BoolValue(b)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return types.BoolValue(s == "true" || s == "1" || s == "on")
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return types.BoolValue(n.String() != "0")
	}
	return types.BoolValue(false)
}

func (r *errorAlertRuleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan errorAlertRuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID := plan.ProjectID.ValueInt64()
	fields := alertRuleFields(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	created, err := r.client.CreateErrorAlertRule(ctx, projectID, fields, client.PayloadKey("error_alert_rule", strconv.FormatInt(projectID, 10), fields))
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error creating Flightdeck error alert rule", err)
		return
	}
	state := alertRuleToModel(created, &resp.Diagnostics)
	reconcileAlertAction(ctx, &state, &plan, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *errorAlertRuleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state errorAlertRuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	rule, err := r.client.GetErrorAlertRule(ctx, state.ProjectID.ValueInt64(), state.ID.ValueInt64())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		addAPIError(&resp.Diagnostics, "Error reading Flightdeck error alert rule", err)
		return
	}
	newState := alertRuleToModel(rule, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *errorAlertRuleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state errorAlertRuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	fields := alertRuleFields(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID, id := state.ProjectID.ValueInt64(), state.ID.ValueInt64()
	updated, err := r.client.UpdateErrorAlertRule(ctx, projectID, id, fields, state.LockVersion.ValueInt64())
	if err != nil {
		if client.IsStale(err) {
			var current *int64
			if fresh, rerr := r.client.GetErrorAlertRule(ctx, projectID, id); rerr == nil {
				current = &fresh.LockVersion
			}
			addStaleError(&resp.Diagnostics, fmt.Sprintf("Error alert rule %q", state.Name.ValueString()), state.LockVersion.ValueInt64(), current, err)
			return
		}
		addAPIError(&resp.Diagnostics, "Error updating Flightdeck error alert rule", err)
		return
	}
	newState := alertRuleToModel(updated, &resp.Diagnostics)
	reconcileAlertAction(ctx, &newState, &plan, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// reconcileAlertAction keeps a planned webhook_url / escalation_policy_id that
// the server dropped (it only stores escalation_policy_id alongside
// open_incident, and normalises the action object) so the apply is consistent;
// a warning names the field and the next refresh shows the drop as a diff.
func reconcileAlertAction(ctx context.Context, state, plan *errorAlertRuleModel, diags *diag.Diagnostics) {
	if plan.Action.IsNull() || plan.Action.IsUnknown() || state.Action.IsNull() {
		return
	}
	var wanted, got alertActionModel
	diags.Append(plan.Action.As(ctx, &wanted, objectAsOptions)...)
	diags.Append(state.Action.As(ctx, &got, objectAsOptions)...)
	var dropped []string
	if !wanted.WebhookURL.IsNull() && !wanted.WebhookURL.IsUnknown() && got.WebhookURL.IsNull() {
		got.WebhookURL = wanted.WebhookURL
		dropped = append(dropped, "action.webhook_url")
	}
	if !wanted.EscalationPolicyID.IsNull() && !wanted.EscalationPolicyID.IsUnknown() && got.EscalationPolicyID.IsNull() {
		got.EscalationPolicyID = wanted.EscalationPolicyID
		dropped = append(dropped, "action.escalation_policy_id")
	}
	if len(dropped) == 0 {
		return
	}
	obj, d := types.ObjectValueFrom(ctx, alertActionAttrTypes, got)
	diags.Append(d...)
	state.Action = obj
	diags.AddWarning("Flightdeck did not store every action value",
		"The API dropped "+strings.Join(dropped, " and ")+" (it keeps escalation_policy_id only while open_incident "+
			"is true). State records the configured value; the next `terraform plan` will show the difference.")
}

func (r *errorAlertRuleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state errorAlertRuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID, id := state.ProjectID.ValueInt64(), state.ID.ValueInt64()
	err := deleteWithIfMatch(ctx, state.LockVersion.ValueInt64(),
		func(ctx context.Context, lv int64) error {
			return r.client.DeleteErrorAlertRule(ctx, projectID, id, lv)
		},
		func(ctx context.Context) (int64, error) {
			fresh, err := r.client.GetErrorAlertRule(ctx, projectID, id)
			if err != nil {
				return 0, err
			}
			return fresh.LockVersion, nil
		})
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error deleting Flightdeck error alert rule", err)
	}
}

// ImportState accepts `<project_id>/<rule_id>`: rules are addressed within
// their project.
func (r *errorAlertRuleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(strings.TrimSpace(req.ID), "/")
	if len(parts) != 2 {
		resp.Diagnostics.AddError("Invalid import id", fmt.Sprintf("Expected <project_id>/<rule_id> (for example 42/12), got %q.", req.ID))
		return
	}
	projectID, ok := parseImportID(parts[0], "project", &resp.Diagnostics)
	if !ok {
		return
	}
	id, ok := parseImportID(parts[1], "error alert rule", &resp.Diagnostics)
	if !ok {
		return
	}
	rule, err := r.client.GetErrorAlertRule(ctx, projectID, id)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error importing Flightdeck error alert rule", err)
		return
	}
	state := alertRuleToModel(rule, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
