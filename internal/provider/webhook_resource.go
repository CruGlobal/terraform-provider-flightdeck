package provider

import (
	"context"
	"fmt"
	"regexp"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

// httpURLPattern mirrors the model's http(s) URL format validation.
var httpURLPattern = regexp.MustCompile(`(?i)^https?://\S+$`)

// objectAsOptions lets nested objects with null attributes decode into models.
var objectAsOptions = basetypes.ObjectAsOptions{UnhandledNullAsEmpty: true, UnhandledUnknownAsEmpty: true}

var (
	_ resource.Resource                = &webhookResource{}
	_ resource.ResourceWithConfigure   = &webhookResource{}
	_ resource.ResourceWithImportState = &webhookResource{}
)

// NewWebhookResource returns the flightdeck_webhook resource.
func NewWebhookResource() resource.Resource { return &webhookResource{} }

type webhookResource struct {
	client *client.Client
}

type webhookModel struct {
	ID          types.Int64  `tfsdk:"id"`
	ProjectID   types.Int64  `tfsdk:"project_id"`
	URL         types.String `tfsdk:"url"`
	Events      types.Set    `tfsdk:"events"`
	Secret      types.String `tfsdk:"secret"`
	Active      types.Bool   `tfsdk:"active"`
	LockVersion types.Int64  `tfsdk:"lock_version"`
}

// webhookToModel maps an API webhook. The secret comes from the create
// response when present, otherwise from prior state.
func webhookToModel(w *client.Webhook, priorSecret types.String, diags *diag.Diagnostics) webhookModel {
	events := make([]attr.Value, 0, len(w.Events))
	for _, e := range w.Events {
		events = append(events, types.StringValue(e))
	}
	set, d := types.SetValue(types.StringType, events)
	diags.Append(d...)
	secret := priorSecret
	if w.Secret != "" {
		secret = types.StringValue(w.Secret)
	}
	m := webhookModel{
		ID:          types.Int64Value(w.ID),
		ProjectID:   types.Int64Null(),
		URL:         types.StringValue(w.URL),
		Events:      set,
		Secret:      secret,
		Active:      types.BoolValue(w.Active),
		LockVersion: types.Int64Value(w.LockVersion),
	}
	if w.ProjectID != nil {
		m.ProjectID = types.Int64Value(*w.ProjectID)
	}
	return m
}

func webhookFields(ctx context.Context, plan *webhookModel, includeSecret bool, diags *diag.Diagnostics) client.Fields {
	var events []string
	diags.Append(plan.Events.ElementsAs(ctx, &events, false)...)
	fields := client.Fields{
		"url":    plan.URL.ValueString(),
		"events": events,
	}
	// Always sent so removing project_id from configuration widens the
	// webhook back to the whole workspace.
	if !plan.ProjectID.IsUnknown() {
		fields["project_id"] = plan.ProjectID.ValueInt64Pointer()
	}
	if !plan.Active.IsNull() && !plan.Active.IsUnknown() {
		fields["active"] = plan.Active.ValueBool()
	}
	if includeSecret && !plan.Secret.IsNull() && !plan.Secret.IsUnknown() {
		fields["secret"] = plan.Secret.ValueString()
	}
	return fields
}

func (r *webhookResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_webhook"
}

func (r *webhookResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *webhookResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an outbound webhook in the token's Flightdeck workspace. Subscribed events are " +
			"POSTed to `url` with an HMAC-SHA256 signature computed from `secret`.\n\n" +
			"A webhook receives events from every project unless `project_id` scopes it to one. Managing webhooks " +
			"requires the token's user to be a **workspace admin**.\n\n" +
			"The signing secret is returned by the API once, on create, and kept in state as a sensitive attribute; it " +
			"is never re-read, so an imported webhook has no `secret` value. Supplying your own `secret` is optional. " +
			"Changing it replaces the webhook.\n\n" +
			"Import by numeric id: `terraform import flightdeck_webhook.ci 9`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				MarkdownDescription: "Numeric id of the webhook.",
				Computed:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"project_id": schema.Int64Attribute{
				MarkdownDescription: "Restrict the webhook to events from this project. Omit for the whole workspace.",
				Optional:            true,
			},
			"url": schema.StringAttribute{
				MarkdownDescription: "http(s) endpoint to deliver to. Internal and private addresses are rejected by the API.",
				Required:            true,
				Validators: []validator.String{
					stringvalidator.RegexMatches(httpURLPattern, "must be an http(s) URL"),
				},
			},
			"events": schema.SetAttribute{
				MarkdownDescription: "Events to subscribe to; at least one. Known events: `" + joinBackticked(client.WebhookEvents) + "`.",
				ElementType:         types.StringType,
				Required:            true,
				Validators: []validator.Set{
					setvalidator.SizeAtLeast(1),
					setvalidator.ValueStringsAre(stringvalidator.OneOf(client.WebhookEvents...)),
				},
			},
			"secret": schema.StringAttribute{
				MarkdownDescription: "HMAC signing secret. Generated by the API when omitted. Changing it replaces the webhook.",
				Optional:            true,
				Computed:            true,
				Sensitive:           true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
					stringplanmodifier.RequiresReplace(),
				},
			},
			"active": schema.BoolAttribute{
				MarkdownDescription: "Whether deliveries are sent. Defaults to `true`.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
			},
			"lock_version": schema.Int64Attribute{
				MarkdownDescription: "Optimistic-locking version the API bumps on every change. Sent as `If-Match` on updates.",
				Computed:            true,
			},
		},
	}
}

func (r *webhookResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan webhookModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	fields := webhookFields(ctx, &plan, true, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	created, err := r.client.CreateWebhook(ctx, fields, client.PayloadKey("webhook", "", fields))
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error creating Flightdeck webhook", err)
		return
	}
	state := webhookToModel(created, plan.Secret, &resp.Diagnostics)
	if state.Secret.IsUnknown() {
		state.Secret = types.StringNull()
		resp.Diagnostics.AddWarning("Webhook secret not returned",
			"The API did not include the signing secret in the create response, so `secret` is null in state. "+
				"Check the Flightdeck API version; the value is only ever available at creation.")
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *webhookResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state webhookModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	w, err := r.client.GetWebhook(ctx, state.ID.ValueInt64())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		addAPIError(&resp.Diagnostics, "Error reading Flightdeck webhook", err)
		return
	}
	newState := webhookToModel(w, state.Secret, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *webhookResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state webhookModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	fields := webhookFields(ctx, &plan, false, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueInt64()
	updated, err := r.client.UpdateWebhook(ctx, id, fields, state.LockVersion.ValueInt64())
	if err != nil {
		if client.IsStale(err) {
			var current *int64
			if fresh, rerr := r.client.GetWebhook(ctx, id); rerr == nil {
				current = &fresh.LockVersion
			}
			addStaleError(&resp.Diagnostics, fmt.Sprintf("Webhook %d", id), state.LockVersion.ValueInt64(), current)
			return
		}
		addAPIError(&resp.Diagnostics, "Error updating Flightdeck webhook", err)
		return
	}
	newState := webhookToModel(updated, state.Secret, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *webhookResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state webhookModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteWebhook(ctx, state.ID.ValueInt64()); err != nil {
		addAPIError(&resp.Diagnostics, "Error deleting Flightdeck webhook", err)
	}
}

func (r *webhookResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, ok := parseImportID(req.ID, "webhook", &resp.Diagnostics)
	if !ok {
		return
	}
	w, err := r.client.GetWebhook(ctx, id)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error importing Flightdeck webhook", err)
		return
	}
	state := webhookToModel(w, types.StringNull(), &resp.Diagnostics)
	resp.Diagnostics.AddWarning("Imported webhook has no secret value",
		"The API returns a webhook's signing secret only when it is created, so `secret` is null for an imported webhook. "+
			"Set `secret` in configuration (which replaces the webhook) if Terraform needs to know it.")
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func joinBackticked(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += "`, `"
		}
		out += s
	}
	return out
}
