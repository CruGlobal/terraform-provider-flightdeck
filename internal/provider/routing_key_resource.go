package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// A routing key is the Events API credential an external monitor uses to open
// an incident in Flightdeck. It is deliberately independent of paging: the key
// belongs to the project, and whether anything pages is a separate question
// that Flightdeck answers only if an escalation policy is attached to the key.
// That attachment is reported here and never written — escalation policies
// have no API of their own, and on-call is not something this provider
// manages.
//
// The secret follows the ingestion-token pattern exactly: the API returns it
// once, on create, and never again. Rotation is replacement, because the API
// has no rotate route. Terraform's default replacement destroys first, so
// `terraform apply -replace` revokes the old key and then mints the new one;
// only a resource with `create_before_destroy` mints first.
//
// A key's `environment` and its `self_healing_trigger` flag together decide
// whether its incidents can trip the project's self-healing loop. Both are
// changed in place, and both follow the provider's rule for a setting the API
// leaves alone when it is omitted: Optional and Computed with no framework
// default, sent only when configured, so a value set in the console survives
// an apply that does not name it.

var (
	_ resource.Resource                = &routingKeyResource{}
	_ resource.ResourceWithConfigure   = &routingKeyResource{}
	_ resource.ResourceWithImportState = &routingKeyResource{}
	_ resource.ResourceWithModifyPlan  = &routingKeyResource{}
)

// NewRoutingKeyResource returns the flightdeck_routing_key resource.
func NewRoutingKeyResource() resource.Resource { return &routingKeyResource{} }

type routingKeyResource struct {
	client *client.Client
}

type routingKeyModel struct {
	ID                 types.Int64  `tfsdk:"id"`
	ProjectID          types.Int64  `tfsdk:"project_id"`
	Name               types.String `tfsdk:"name"`
	Key                types.String `tfsdk:"routing_key"`
	Masked             types.String `tfsdk:"masked"`
	LastFour           types.String `tfsdk:"last_four"`
	EscalationPolicyID types.Int64  `tfsdk:"escalation_policy_id"`
	Environment        types.String `tfsdk:"environment"`
	SelfHealingTrigger types.Bool   `tfsdk:"self_healing_trigger"`
	LockVersion        types.Int64  `tfsdk:"lock_version"`
}

// routingKeyToModel maps an API key. The plaintext comes from the create
// response and otherwise from prior state; it is never re-read.
func routingKeyToModel(k *client.RoutingKey, key types.String) routingKeyModel {
	if k.Key != "" {
		key = types.StringValue(k.Key)
	}
	policy := types.Int64Null()
	if k.EscalationPolicyID != nil {
		policy = types.Int64Value(*k.EscalationPolicyID)
	}
	return routingKeyModel{
		ID:                 types.Int64Value(k.ID),
		ProjectID:          types.Int64Value(k.ProjectID),
		Name:               types.StringValue(k.Name),
		Key:                key,
		Masked:             types.StringValue(k.Masked),
		LastFour:           types.StringValue(k.LastFour),
		EscalationPolicyID: policy,
		Environment:        types.StringPointerValue(k.Environment),
		SelfHealingTrigger: types.BoolPointerValue(k.SelfHealingTrigger),
		LockVersion:        types.Int64Value(k.LockVersion),
	}
}

// routingKeyFields is what a create or an update sends: the name, plus
// environment and self_healing_trigger where the configuration sets them, at
// the plan's value.
func routingKeyFields(config, plan routingKeyModel) client.Fields {
	fields := client.Fields{"name": plan.Name.ValueString()}
	if !config.Environment.IsNull() && known(plan.Environment) {
		fields["environment"] = plan.Environment.ValueString()
	}
	if !config.SelfHealingTrigger.IsNull() && known(plan.SelfHealingTrigger) {
		fields["self_healing_trigger"] = plan.SelfHealingTrigger.ValueBool()
	}
	return fields
}

// routingKeyNewerFields are the settings a Flightdeck older than them refuses
// as unknown keys, in the order they arrived.
var routingKeyNewerFields = []string{"environment", "self_healing_trigger"}

// sentNewerRoutingKeyFields returns the newer settings a write named.
func sentNewerRoutingKeyFields(sent client.Fields) []string {
	var named []string
	for _, key := range routingKeyNewerFields {
		if _, ok := sent[key]; ok {
			named = append(named, key)
		}
	}
	return named
}

// routingKeyTooOldNote closes the error for a refused write that named a
// newer setting when nothing could be read to tell whether this Flightdeck
// knows it: the API refuses an unknown key in prose, and its own words may
// still say so.
func routingKeyTooOldNote(named []string) string {
	return fmt.Sprintf("If the API calls %s an unknown key, this Flightdeck is older than the release that added it: "+
		"upgrade Flightdeck first, or leave it out of the configuration.", strings.Join(named, " or "))
}

// addRoutingKeyTooOldErrors explains an update refused for naming a setting
// this Flightdeck is too old to know. Rather than match the refusal's
// wording, a fresh read is asked: a Flightdeck that knows a setting always
// reports it. It reports whether it added a diagnostic; false leaves the
// error to the caller.
func addRoutingKeyTooOldErrors(ctx context.Context, c *client.Client, projectID, id int64, sent client.Fields, err error, diags *diag.Diagnostics) bool {
	named := sentNewerRoutingKeyFields(sent)
	if !client.IsValidation(err) || len(named) == 0 {
		return false
	}
	k, rerr := c.GetRoutingKey(ctx, projectID, id)
	if rerr != nil {
		addAPIErrorNote(diags, "Error updating Flightdeck routing key", err, routingKeyTooOldNote(named))
		return true
	}
	reported := map[string]bool{
		"environment":          k.ReportsEnvironment(),
		"self_healing_trigger": k.ReportsSelfHealingTrigger(),
	}
	added := false
	for _, key := range named {
		if reported[key] {
			continue
		}
		diags.AddAttributeError(path.Root(key), "This Flightdeck is too old for "+key,
			fmt.Sprintf("The Flightdeck at this endpoint does not accept %s on a routing key, so it cannot be set from "+
				"Terraform there. Nothing in this update was saved. Remove %s from the configuration, or upgrade "+
				"Flightdeck. %s", key, key, apiMessage(err)))
		added = true
	}
	return added
}

func (r *routingKeyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_routing_key"
}

func (r *routingKeyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *routingKeyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an Events API routing key for a Flightdeck project — the credential an external " +
			"monitor presents to open, fold, acknowledge or resolve a Flightdeck incident.\n\n" +
			"The key is posted to Flightdeck's Events API in a PagerDuty-Events-v2-shaped body:\n\n" +
			"```json\n{\n  \"routing_key\": \"fd_evt_…\",\n  \"event_action\": \"trigger\",\n  \"dedup_key\": \"disk-full-web-1\",\n" +
			"  \"payload\": { \"summary\": \"Disk nearly full\", \"severity\": \"warning\", \"source\": \"web-1\" }\n}\n```\n\n" +
			"A project may hold as many keys as it likes, so issue one per monitor and revoke it on its own — each " +
			"with its own `name`. Names must be distinct within a project. The API does not check that as such, but " +
			"for 24 hours after a key's create, a second key declared with the same name and settings is the same " +
			"create as far as the API is concerned, and fails (see `name`). " +
			"Receiving events does not imply paging: a key pages only if an escalation policy is attached to it, " +
			"which is a console operation and read-only here (see `escalation_policy_id`).\n\n" +
			"A key can also let its incidents trip the project's self-healing loop, as a second signal beside the " +
			"error rate: set `self_healing_trigger = true` and `environment = \"production\"`, and set the project's " +
			"`self_healing.monitor_rollback` (see `flightdeck_project`). Put only server-side monitors on such a key, " +
			"never browser RUM monitors, whose client token is public.\n\n" +
			"The key value is returned by the API **once, on create**, and stored in Terraform state as a sensitive " +
			"attribute so it can be handed to the monitor. It is never re-read; an imported key has no `routing_key` " +
			"value. The API has no rotate route, so **rotation is replacement**: " +
			"`terraform apply -replace=flightdeck_routing_key.monitor` revokes the old key and then mints a new one, " +
			"because Terraform destroys before it creates by default, so the monitor is without a working key until " +
			"it is given the new one. To mint the new key first, set `create_before_destroy` in the resource's " +
			"`lifecycle` block. That has one limit: within 24 hours of the old key's create, the replacement sends " +
			"the same create again while the old key is still live, and Flightdeck refuses it, so either wait out " +
			"the 24 hours or let that one replacement destroy first. " +
			"Deleting the resource revokes the key — irreversibly, and the row stays readable as history, so a " +
			"revoked key is not a free identifier.\n\n" +
			"Import with `<project_id>/<key_id>`: `terraform import flightdeck_routing_key.monitor 42/7`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				MarkdownDescription: "Numeric id of the routing key.",
				Computed:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"project_id": schema.Int64Attribute{
				MarkdownDescription: "Id of the project events on this key open incidents in. Changing it replaces the key.",
				Required:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Label shown in the project's routing-key list — the monitor's name, usually. " +
					"Editable in place. **Give every routing key in a project a distinct name:** a create is made " +
					"idempotent with a key derived from the request body, which holds `name` and, when they are set, " +
					"`environment` and `self_healing_trigger`, so two keys declared in one project with the same name " +
					"and settings are one create as far as the API is " +
					"concerned for the 24 hours it remembers a create. While the first key is live, Flightdeck refuses the second create rather than replay it " +
					"without its secret, and the apply fails naming the key that already exists: give the resource the " +
					"error is reported on another name. The same error follows an apply that failed after Flightdeck made the key; then " +
					"revoke the key it names and apply again. Reusing a name a key was *renamed away from* is safe: " +
					"the renamed key is not the one the create would replay.",
				Required:   true,
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"routing_key": schema.StringAttribute{
				MarkdownDescription: "The key value (`fd_evt_…`). Available only when this resource created the key; " +
					"null for an imported one.",
				Computed:      true,
				Sensitive:     true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"masked": schema.StringAttribute{
				MarkdownDescription: "The key as the UI shows it: an ellipsis and the last four characters.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"last_four": schema.StringAttribute{
				MarkdownDescription: "Last four characters of the key, for matching a key in the UI against one in state.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"escalation_policy_id": schema.Int64Attribute{
				MarkdownDescription: "Escalation policy that events on this key page, or null for a key that records " +
					"incidents without paging anyone — which is the default, and the point of a routing key being " +
					"separate from paging. **Read-only here:** Flightdeck's escalation policies and on-call schedules " +
					"have no API and are deliberately not managed by this provider, because on-call belongs in " +
					"PagerDuty (see `flightdeck_pagerduty_integration`) and Terraforming a second on-call system is " +
					"what that split exists to avoid. Attach a policy from the Flightdeck console if you want one; " +
					"this attribute reports what is attached so the state of the world is visible, and a value here " +
					"is never written.",
				Computed:      true,
				Validators:    []validator.Int64{readOnlyAttribute("escalation policies are attached from the Flightdeck console and have no API")},
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"environment": schema.StringAttribute{
				MarkdownDescription: "What the key's monitors watch, such as `production` or `staging`, using the same " +
					"names as your release events. Every incident the key opens keeps the environment the key had at that " +
					"moment, so a change applies only to incidents opened afterwards. It is free text and compared " +
					"exactly: a key can trip self-healing only with `production`, and `Production` or `prod` never " +
					"match. Changed in place, with no new key. At most 255 characters, with no space, tab or line break " +
					"at either end. When unset, the key keeps the environment it has (none, for a new key), so one set " +
					"in the console survives an apply; set it for Terraform to own it. Terraform does not clear an " +
					"environment once one is set: clear it in the console. Null on a Flightdeck that predates the " +
					"setting, where a configured value fails the apply.",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					notBlank("leave `environment` out for a key with no environment"),
					noSurroundingBlank{},
					charLengthAtMost(client.RoutingKeyEnvironmentMaxLength),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"self_healing_trigger": schema.BoolAttribute{
				MarkdownDescription: "Whether an incident this key opens may trip the project's self-healing loop, as a " +
					"second signal beside the error rate. It counts only while `environment` is exactly `production` and " +
					"the project's `self_healing.monitor_rollback` isn't `\"off\"`, so a plan that turns it on for a key " +
					"with any other environment, or none, carries a warning. Only put server-side monitors on such a key " +
					"(APM, infrastructure, synthetics, server logs), never browser RUM monitors, whose client token is " +
					"public. Flightdeck records each change of this flag, and of a flagged key's environment, and posts " +
					"it to the project's Slack channel. The server's default is `false`. When unset, the key keeps its " +
					"current value; set it explicitly, even to `false`, for Terraform to own it. Null on a Flightdeck " +
					"that predates the setting, where a configured value fails the apply.",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"lock_version": schema.Int64Attribute{
				MarkdownDescription: "Optimistic-locking version the API bumps on every change. Sent as `If-Match` on updates and when revoking.",
				Computed:            true,
			},
		},
	}
}

func (r *routingKeyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan, config routingKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID := plan.ProjectID.ValueInt64()
	fields := routingKeyFields(config, plan)
	created, retiredID, err := r.client.CreateRoutingKey(ctx, projectID, fields, client.PayloadKey("routing_key", strconv.FormatInt(projectID, 10), fields))
	if err != nil {
		// The client revokes only a key this create made itself and could not
		// record (its answer was lost, or it could not be read back), so the
		// revoked key's value was never handed to anyone. Naming it explains a
		// revoked row nobody asked for.
		if retiredID != 0 {
			resp.Diagnostics.AddWarning("A routing key this apply had just created was revoked",
				fmt.Sprintf("Routing key %d in project %d was created by this apply but could not be recorded, so it was "+
					"revoked. Its value was never handed to anything, so nothing was using it. Re-apply to create the "+
					"routing key.", retiredID, projectID))
		}
		if addReplayWithheldError(&resp.Diagnostics, pathRoot("name"), "A routing key with this name already exists",
			func(id int64) replayWithheldText {
				return replayWithheldText{
					record: fmt.Sprintf("Routing key %d in project %d", id, projectID),
					distinct: fmt.Sprintf("If another flightdeck_routing_key in this project is named %q, it holds that key: give this "+
						"resource a different name.", plan.Name.ValueString()),
					retire: fmt.Sprintf("revoke routing key %d in Flightdeck", id),
				}
			}, err) {
			return
		}
		// There is no key yet to read, so a refusal of a newer setting gets a
		// note rather than a verdict.
		note := ""
		if named := sentNewerRoutingKeyFields(fields); client.IsValidation(err) && len(named) > 0 {
			note = routingKeyTooOldNote(named)
		}
		addAPIErrorNote(&resp.Diagnostics, "Error creating Flightdeck routing key", err, note)
		return
	}
	// The client guarantees the key carries its secret or fails.
	state := routingKeyToModel(created, types.StringNull())
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *routingKeyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state routingKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	k, err := r.client.GetRoutingKey(ctx, state.ProjectID.ValueInt64(), state.ID.ValueInt64())
	if err != nil {
		if client.IsNotFound(err) {
			removeGone(ctx, r.client, resp, state.ProjectID.ValueInt64(), fmt.Sprintf("routing key %d", state.ID.ValueInt64()),
				fmt.Sprintf("%d/%d", state.ProjectID.ValueInt64(), state.ID.ValueInt64()))
			return
		}
		addAPIError(&resp.Diagnostics, "Error reading Flightdeck routing key", err)
		return
	}
	if k.IsRevoked() {
		// Revoked (from the console, or by a replaced resource): the row stays
		// as history, but the credential is gone; recreate on the next apply.
		resp.State.RemoveResource(ctx)
		return
	}
	newState := routingKeyToModel(k, state.Key)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Update writes the name, and environment and self_healing_trigger where the
// configuration sets them. The key itself cannot be rotated in place —
// replacing the resource is the rotation.
func (r *routingKeyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state, config routingKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID, id := state.ProjectID.ValueInt64(), state.ID.ValueInt64()
	fields := routingKeyFields(config, plan)
	updated, err := r.client.UpdateRoutingKey(ctx, projectID, id, fields, state.LockVersion.ValueInt64())
	if err != nil {
		if client.IsStale(err) {
			var current *int64
			if fresh, rerr := r.client.GetRoutingKey(ctx, projectID, id); rerr == nil {
				current = &fresh.LockVersion
			}
			addStaleError(&resp.Diagnostics, fmt.Sprintf("Routing key %q", state.Name.ValueString()), state.LockVersion.ValueInt64(), current, err)
			return
		}
		if addRoutingKeyTooOldErrors(ctx, r.client, projectID, id, fields, err, &resp.Diagnostics) {
			return
		}
		addAPIError(&resp.Diagnostics, "Error updating Flightdeck routing key", err)
		return
	}
	newState := routingKeyToModel(updated, state.Key)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// ModifyPlan warns when the configuration turns self_healing_trigger on for a
// key whose environment will not be exactly "production": Flightdeck takes
// it, but the key's incidents never trip anything. On create, an environment
// the configuration leaves out means none.
func (r *routingKeyResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var plan, config routingKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !known(config.SelfHealingTrigger) || !config.SelfHealingTrigger.ValueBool() {
		return
	}
	environment := plan.Environment
	if config.Environment.IsNull() && req.State.Raw.IsNull() {
		environment = types.StringNull()
	}
	if environment.IsUnknown() || (known(environment) && environment.ValueString() == tripEnvironment) {
		return
	}
	has := "has no environment"
	if known(environment) {
		has = fmt.Sprintf("has the environment %q", environment.ValueString())
	}
	name := "this routing key"
	if known(plan.Name) {
		name = fmt.Sprintf("routing key %q", plan.Name.ValueString())
	}
	resp.Diagnostics.AddAttributeWarning(path.Root("self_healing_trigger"), capitalize(name)+" can't trip self-healing",
		fmt.Sprintf("self_healing_trigger is on, but %s %s. Flightdeck counts a key's incidents toward self-healing only "+
			"while its environment is exactly %q, so this key's incidents never trip anything, and the project's "+
			"self-healing settings report it as a blocker. Set environment = %q if this key's monitors watch production.",
			name, has, tripEnvironment, tripEnvironment))
}

// tripEnvironment is the only environment a key that trips self-healing may
// have: the one the loop watches.
const tripEnvironment = "production"

func (r *routingKeyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state routingKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID, id := state.ProjectID.ValueInt64(), state.ID.ValueInt64()
	err := deleteWithIfMatch(ctx, state.LockVersion.ValueInt64(),
		func(ctx context.Context, lv int64) error {
			return r.client.RevokeRoutingKey(ctx, projectID, id, lv)
		},
		func(ctx context.Context) (int64, error) {
			fresh, err := r.client.GetRoutingKey(ctx, projectID, id)
			if err != nil {
				return 0, err
			}
			return fresh.LockVersion, nil
		})
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error revoking Flightdeck routing key", err)
	}
}

// ImportState accepts `<project_id>/<key_id>`: keys are addressed within their
// project. The secret is never recoverable.
func (r *routingKeyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(strings.TrimSpace(req.ID), "/")
	if len(parts) != 2 {
		resp.Diagnostics.AddError("Invalid import id", fmt.Sprintf("Expected <project_id>/<key_id> (for example 42/7), got %q.", req.ID))
		return
	}
	projectID, ok := parseImportID(parts[0], "project", &resp.Diagnostics)
	if !ok {
		return
	}
	id, ok := parseImportID(parts[1], "routing key", &resp.Diagnostics)
	if !ok {
		return
	}
	k, err := r.client.GetRoutingKey(ctx, projectID, id)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error importing Flightdeck routing key", err)
		return
	}
	if k.IsRevoked() {
		resp.Diagnostics.AddError("Routing key is revoked",
			fmt.Sprintf("Routing key %d was revoked on %s and cannot be imported; create a new one.", k.ID, valueOr(k.RevokedAt, "an unknown date")))
		return
	}
	state := routingKeyToModel(k, types.StringNull())
	resp.Diagnostics.AddWarning("Imported routing key has no key value",
		"The API returns a routing key's value only when it is created, so `routing_key` is null for an imported key. "+
			"Replace the resource (`terraform apply -replace`) if Terraform needs to hand the value to a monitor; "+
			"that revokes this key and mints a new one (or the other way round, with `create_before_destroy`).")
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
