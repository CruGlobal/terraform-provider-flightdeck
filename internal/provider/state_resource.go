package provider

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// hexColorPattern mirrors State::HEX_COLOR / Label::HEX_COLOR.
var hexColorPattern = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

var (
	_ resource.Resource                = &stateResource{}
	_ resource.ResourceWithConfigure   = &stateResource{}
	_ resource.ResourceWithImportState = &stateResource{}
)

// NewStateResource returns the flightdeck_state resource.
func NewStateResource() resource.Resource { return &stateResource{} }

type stateResource struct {
	client *client.Client
}

type stateModel struct {
	ID          types.Int64  `tfsdk:"id"`
	ProjectID   types.Int64  `tfsdk:"project_id"`
	Name        types.String `tfsdk:"name"`
	Group       types.String `tfsdk:"group"`
	Color       types.String `tfsdk:"color"`
	Default     types.Bool   `tfsdk:"default"`
	Position    types.Int64  `tfsdk:"position"`
	LockVersion types.Int64  `tfsdk:"lock_version"`
}

func stateToModel(s *client.State) stateModel {
	return stateModel{
		ID:          types.Int64Value(s.ID),
		ProjectID:   types.Int64Value(s.ProjectID),
		Name:        types.StringValue(s.Name),
		Group:       types.StringValue(s.Group),
		Color:       types.StringValue(s.Color),
		Default:     types.BoolValue(s.Default),
		Position:    types.Int64Value(s.Position),
		LockVersion: types.Int64Value(s.LockVersion),
	}
}

func stateFields(plan *stateModel) client.Fields {
	fields := client.Fields{
		"name":  plan.Name.ValueString(),
		"group": plan.Group.ValueString(),
	}
	if !plan.Color.IsNull() && !plan.Color.IsUnknown() {
		fields["color"] = plan.Color.ValueString()
	}
	if !plan.Default.IsNull() && !plan.Default.IsUnknown() {
		fields["default"] = plan.Default.ValueBool()
	}
	if !plan.Position.IsNull() && !plan.Position.IsUnknown() {
		fields["position"] = plan.Position.ValueInt64()
	}
	return fields
}

func (r *stateResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_state"
}

func (r *stateResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *stateResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a workflow state within a Flightdeck project.\n\n" +
			"A new project comes with five default states (Backlog, To Do, In Progress, Done, Cancelled); use the " +
			"`flightdeck_states` data source to see them, or import one to manage it. New states are appended to the " +
			"end of their group unless `position` is set. Exactly one state per project is the default: setting " +
			"`default = true` here clears it on whichever state had it.\n\n" +
			"A state that still has work items, or that is the project default, cannot be deleted; the API rejects " +
			"the delete and the error is reported.\n\n" +
			"Import by numeric id: `terraform import flightdeck_state.done 17`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				MarkdownDescription: "Numeric id of the state.",
				Computed:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"project_id": schema.Int64Attribute{
				MarkdownDescription: "Id of the project the state belongs to. Changing it replaces the state.",
				Required:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Display name; unique within the project.",
				Required:            true,
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"group": schema.StringAttribute{
				MarkdownDescription: "Workflow group: one of `" + strings.Join(client.StateGroups, "`, `") + "`.",
				Required:            true,
				Validators:          []validator.String{stringvalidator.OneOf(client.StateGroups...)},
			},
			"color": schema.StringAttribute{
				MarkdownDescription: "Hex color (`#rgb` or `#rrggbb`). Defaults to the server's default.",
				Optional:            true,
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				Validators: []validator.String{
					stringvalidator.RegexMatches(hexColorPattern, "must be a hex color such as #3b82f6"),
				},
			},
			"default": schema.BoolAttribute{
				MarkdownDescription: "Whether new work items land in this state. Defaults to `false`. Only one state per " +
					"project can be the default, so declare it on exactly one.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
			},
			"position": schema.Int64Attribute{
				MarkdownDescription: "Sort position within the group. Assigned by the server (end of the group) when unset.",
				Optional:            true,
				Computed:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"lock_version": schema.Int64Attribute{
				MarkdownDescription: "Optimistic-locking version the API bumps on every change. Sent as `If-Match` on updates.",
				Computed:            true,
			},
		},
	}
}

func (r *stateResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan stateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID := plan.ProjectID.ValueInt64()
	fields := stateFields(&plan)
	created, err := r.client.CreateState(ctx, projectID, fields, client.PayloadKey("state", strconv.FormatInt(projectID, 10), fields))
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error creating Flightdeck state", err)
		return
	}
	state := stateToModel(created)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *stateResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state stateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s, err := r.client.GetState(ctx, state.ID.ValueInt64())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		addAPIError(&resp.Diagnostics, "Error reading Flightdeck state", err)
		return
	}
	newState := stateToModel(s)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *stateResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state stateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueInt64()
	updated, err := r.client.UpdateState(ctx, id, stateFields(&plan), state.LockVersion.ValueInt64())
	if err != nil {
		if client.IsStale(err) {
			var current *int64
			if fresh, rerr := r.client.GetState(ctx, id); rerr == nil {
				current = &fresh.LockVersion
			}
			addStaleError(&resp.Diagnostics, fmt.Sprintf("State %q", state.Name.ValueString()), state.LockVersion.ValueInt64(), current)
			return
		}
		addAPIError(&resp.Diagnostics, "Error updating Flightdeck state", err)
		return
	}
	newState := stateToModel(updated)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *stateResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state stateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteState(ctx, state.ID.ValueInt64()); err != nil {
		addAPIError(&resp.Diagnostics, "Error deleting Flightdeck state", err)
	}
}

func (r *stateResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, ok := parseImportID(req.ID, "state", &resp.Diagnostics)
	if !ok {
		return
	}
	s, err := r.client.GetState(ctx, id)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error importing Flightdeck state", err)
		return
	}
	state := stateToModel(s)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
