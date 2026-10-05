package provider

import (
	"context"
	"fmt"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = &teamspaceMemberResource{}
	_ resource.ResourceWithConfigure   = &teamspaceMemberResource{}
	_ resource.ResourceWithImportState = &teamspaceMemberResource{}
)

// NewTeamspaceMemberResource returns the flightdeck_teamspace_member resource.
func NewTeamspaceMemberResource() resource.Resource { return &teamspaceMemberResource{} }

type teamspaceMemberResource struct {
	client *client.Client
}

// teamspaceMemberModel: a member has no id of its own, so `id` is the import
// id, "<teamspace_id>/<user_id>".
type teamspaceMemberModel struct {
	ID          types.String `tfsdk:"id"`
	TeamspaceID types.Int64  `tfsdk:"teamspace_id"`
	UserID      types.Int64  `tfsdk:"user_id"`
}

func teamspaceMemberToModel(m *client.TeamspaceMember) teamspaceMemberModel {
	return teamspaceMemberModel{
		ID:          types.StringValue(fmt.Sprintf("%d/%d", m.TeamspaceID, m.UserID)),
		TeamspaceID: types.Int64Value(m.TeamspaceID),
		UserID:      types.Int64Value(m.UserID),
	}
}

func (r *teamspaceMemberResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_teamspace_member"
}

func (r *teamspaceMemberResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *teamspaceMemberResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Puts a member of the workspace on a Flightdeck teamspace. Being on a team grants no access " +
			"to anything; project access is `flightdeck_project_member`.\n\n" +
			"Any member of the workspace except a guest can add and remove a team's members. Adding a user who is " +
			"already on the team adopts that membership rather than failing, so destroying this resource takes them " +
			"off the team even if they were added some other way. Changing either id replaces the membership.\n\n" +
			"Import with `<teamspace_id>/<user_id>`: `terraform import flightdeck_teamspace_member.alex 12/7`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "`<teamspace_id>/<user_id>`, the import id. A membership has no id of its own.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"teamspace_id": schema.Int64Attribute{
				MarkdownDescription: "Id of the teamspace. Changing it replaces the membership.",
				Required:            true,
				Validators:          []validator.Int64{int64validator.AtLeast(1)},
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"user_id": schema.Int64Attribute{
				MarkdownDescription: "User id of a member of the workspace, which a `flightdeck_workspace_member` data source " +
					"resolves from an email address. Changing it replaces the membership.",
				Required:      true,
				Validators:    []validator.Int64{int64validator.AtLeast(1)},
				PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *teamspaceMemberResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan teamspaceMemberModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	teamspaceID, userID := plan.TeamspaceID.ValueInt64(), plan.UserID.ValueInt64()
	m, err := r.client.AddTeamspaceMember(ctx, teamspaceID, userID)
	if err != nil {
		switch {
		case client.HasCode(err, client.CodeInvalidAttribute):
			resp.Diagnostics.AddAttributeError(pathRoot("user_id"), "Cannot add this user to the teamspace",
				"Only members of the workspace can be on a teamspace. Nothing was added.\n\nThe API said: "+apiMessage(err))
		case client.IsNotFound(err):
			resp.Diagnostics.AddAttributeError(pathRoot("teamspace_id"), fmt.Sprintf("Teamspace %d not found", teamspaceID),
				"There is no teamspace with this id in the token's workspace; it may have been deleted. Nothing was "+
					"added.\n\nThe API said: "+apiMessage(err))
		case client.IsForbidden(err):
			resp.Diagnostics.AddError("Workspace guests cannot change a teamspace's members",
				"Any member of the workspace except a guest can add and remove a team's members.\n\nThe API said: "+apiMessage(err))
		default:
			addAPIError(&resp.Diagnostics, "Error adding Flightdeck teamspace member", err)
		}
		return
	}
	state := teamspaceMemberToModel(m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *teamspaceMemberResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state teamspaceMemberModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	m, err := r.client.GetTeamspaceMember(ctx, state.TeamspaceID.ValueInt64(), state.UserID.ValueInt64())
	if err != nil {
		// Off the team, or the team is gone.
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		addAPIError(&resp.Diagnostics, "Error reading Flightdeck teamspace member", err)
		return
	}
	newState := teamspaceMemberToModel(m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Update is never called with a change: both ids require replacement.
func (r *teamspaceMemberResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var state teamspaceMemberModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *teamspaceMemberResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state teamspaceMemberModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.RemoveTeamspaceMember(ctx, state.TeamspaceID.ValueInt64(), state.UserID.ValueInt64()); err != nil {
		addAPIError(&resp.Diagnostics, "Error removing Flightdeck teamspace member", err)
	}
}

func (r *teamspaceMemberResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	teamspaceID, userID, ok := parseImportPair(req.ID, "teamspace", "user", &resp.Diagnostics)
	if !ok {
		return
	}
	m, err := r.client.GetTeamspaceMember(ctx, teamspaceID, userID)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error importing Flightdeck teamspace member", err)
		return
	}
	state := teamspaceMemberToModel(m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
