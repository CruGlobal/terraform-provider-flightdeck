package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
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
// has no rotate route — `terraform apply -replace` mints a new key and revokes
// the old one, in that order.

var (
	_ resource.Resource                = &routingKeyResource{}
	_ resource.ResourceWithConfigure   = &routingKeyResource{}
	_ resource.ResourceWithImportState = &routingKeyResource{}
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
		LockVersion:        types.Int64Value(k.LockVersion),
	}
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
			"with its own `name`, which must be distinct within the project. " +
			"Receiving events does not imply paging: a key pages only if an escalation policy is attached to it, " +
			"which is a console operation and read-only here (see `escalation_policy_id`).\n\n" +
			"The key value is returned by the API **once, on create**, and stored in Terraform state as a sensitive " +
			"attribute so it can be handed to the monitor. It is never re-read; an imported key has no `routing_key` " +
			"value. The API has no rotate route, so **rotation is replacement**: " +
			"`terraform apply -replace=flightdeck_routing_key.monitor` mints a new key and revokes the old one. " +
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
					"Editable in place.\n\n" +
					"**Give every routing key in a project a distinct name.** A create is made idempotent with a key " +
					"derived from the request body, and `name` is the only thing in that body, so two keys in one " +
					"project declared with the same name are one create as far as the API is concerned: the second " +
					"replays the first, and because a replay never returns the secret the provider retires that row " +
					"and mints a fresh key. The second declaration ends up holding a working key and the first is left " +
					"pointing at a revoked one. The provider warns when this happens, but distinct names avoid it.",
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
					"separate from paging.\n\n" +
					"**Read-only here.** Flightdeck's escalation policies and on-call schedules have no API and are " +
					"deliberately not managed by this provider: on-call belongs in PagerDuty (see " +
					"`flightdeck_pagerduty_integration`), and Terraforming a second on-call system is what that split " +
					"exists to avoid. Attach a policy from the Flightdeck console if you want one; this attribute " +
					"reports what is attached so the state of the world is visible, and a value here is never written.",
				Computed:      true,
				Validators:    []validator.Int64{readOnlyAttribute("escalation policies are attached from the Flightdeck console and have no API")},
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"lock_version": schema.Int64Attribute{
				MarkdownDescription: "Optimistic-locking version the API bumps on every change. Sent as `If-Match` on updates and when revoking.",
				Computed:            true,
			},
		},
	}
}

func (r *routingKeyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan routingKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID := plan.ProjectID.ValueInt64()
	fields := client.Fields{"name": plan.Name.ValueString()}
	created, retiredID, err := r.client.CreateRoutingKey(ctx, projectID, fields, client.PayloadKey("routing_key", strconv.FormatInt(projectID, 10), fields))
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error creating Flightdeck routing key", err)
		return
	}
	if retiredID != 0 {
		// Either a previous attempt at this same declaration is recovering
		// itself, which is fine, or another resource in this project declared
		// the same name and has just had its live key revoked, which is not.
		// Nothing below this layer can tell those apart, so say so plainly.
		resp.Diagnostics.AddWarning("An existing routing key was retired to create this one",
			fmt.Sprintf("Creating %q replayed an earlier create of an identical declaration, so routing key %d was revoked "+
				"and a new key minted. A replayed create never returns its secret, so the old key could not be kept.\n\n"+
				"If an earlier attempt at this same resource failed part-way, this is that attempt being cleaned up and "+
				"there is nothing to do. If another flightdeck_routing_key in project %d is declared with the name %q, "+
				"that resource is now holding a revoked key: give each routing key a distinct name and re-apply to mint "+
				"it a working one.", plan.Name.ValueString(), retiredID, projectID, plan.Name.ValueString()))
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
			resp.State.RemoveResource(ctx)
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

// Update writes the only editable attribute, the name. The key itself cannot
// be rotated in place — replacing the resource is the rotation.
func (r *routingKeyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state routingKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID, id := state.ProjectID.ValueInt64(), state.ID.ValueInt64()
	fields := client.Fields{"name": plan.Name.ValueString()}
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
		addAPIError(&resp.Diagnostics, "Error updating Flightdeck routing key", err)
		return
	}
	newState := routingKeyToModel(updated, state.Key)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

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
			"that mints a new key and revokes this one.")
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
