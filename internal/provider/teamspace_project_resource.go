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
	_ resource.Resource                = &teamspaceProjectResource{}
	_ resource.ResourceWithConfigure   = &teamspaceProjectResource{}
	_ resource.ResourceWithImportState = &teamspaceProjectResource{}
)

// NewTeamspaceProjectResource returns the flightdeck_teamspace_project resource.
func NewTeamspaceProjectResource() resource.Resource { return &teamspaceProjectResource{} }

type teamspaceProjectResource struct {
	client *client.Client
}

// teamspaceProjectModel: a link has no id of its own, so `id` is the import
// id, "<teamspace_id>/<project_id>".
type teamspaceProjectModel struct {
	ID          types.String `tfsdk:"id"`
	TeamspaceID types.Int64  `tfsdk:"teamspace_id"`
	ProjectID   types.Int64  `tfsdk:"project_id"`
}

func teamspaceProjectToModel(l *client.TeamspaceProject) teamspaceProjectModel {
	return teamspaceProjectModel{
		ID:          types.StringValue(fmt.Sprintf("%d/%d", l.TeamspaceID, l.ProjectID)),
		TeamspaceID: types.Int64Value(l.TeamspaceID),
		ProjectID:   types.Int64Value(l.ProjectID),
	}
}

func (r *teamspaceProjectResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_teamspace_project"
}

func (r *teamspaceProjectResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *teamspaceProjectResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Says that a Flightdeck teamspace owns a project. A project can belong to more than one " +
			"team, and the link shows the project's name on the team's page to everyone in the workspace.\n\n" +
			"Linking or unlinking a project needs a workspace member who is not a guest, with both **read** and " +
			"**administer** on that project. With the built-in roles that is the project's admins, and a workspace " +
			"owner or admin on every project. A project the token cannot see is refused the same way as an id that " +
			"names no project, so the error cannot tell you which. Linking a project that is already linked adopts " +
			"that link rather than failing, so destroying this resource unlinks it even if it was linked some other " +
			"way. Changing either id replaces the link.\n\n" +
			"Import with `<teamspace_id>/<project_id>`: `terraform import flightdeck_teamspace_project.app 12/42`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "`<teamspace_id>/<project_id>`, the import id. A link has no id of its own.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"teamspace_id": schema.Int64Attribute{
				MarkdownDescription: "Id of the teamspace. Changing it replaces the link.",
				Required:            true,
				Validators:          []validator.Int64{int64validator.AtLeast(1)},
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"project_id": schema.Int64Attribute{
				MarkdownDescription: "Id of the project the team owns. Changing it replaces the link.",
				Required:            true,
				Validators:          []validator.Int64{int64validator.AtLeast(1)},
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *teamspaceProjectResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan teamspaceProjectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	teamspaceID, projectID := plan.TeamspaceID.ValueInt64(), plan.ProjectID.ValueInt64()
	l, err := r.client.LinkTeamspaceProject(ctx, teamspaceID, projectID)
	if err != nil {
		switch {
		case client.HasCode(err, client.CodeInvalidAttribute):
			resp.Diagnostics.AddAttributeError(pathRoot("project_id"), fmt.Sprintf("Cannot link project %d", projectID),
				"Flightdeck refused the project. It gives this same answer for an id that names no project in the "+
					"workspace, for a project being deleted, and for a project this token cannot see, so it does not say "+
					"which. Nothing was linked.\n\nThe API said: "+apiMessage(err))
		case client.IsNotFound(err):
			resp.Diagnostics.AddAttributeError(pathRoot("teamspace_id"), fmt.Sprintf("Teamspace %d not found", teamspaceID),
				"There is no teamspace with this id in the token's workspace; it may have been deleted. Nothing was "+
					"linked.\n\nThe API said: "+apiMessage(err))
		case client.IsForbidden(err):
			resp.Diagnostics.AddAttributeError(pathRoot("project_id"), fmt.Sprintf("Not allowed to link project %d", projectID),
				linkRuleText+" Nothing was linked.\n\nThe API said: "+apiMessage(err))
		default:
			addAPIError(&resp.Diagnostics, "Error linking a project to a Flightdeck teamspace", err)
		}
		return
	}
	state := teamspaceProjectToModel(l)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// linkRuleText is who may link or unlink a project, for the 403s.
const linkRuleText = "Linking a project to a teamspace, or unlinking it, needs a workspace member who is not a guest, " +
	"with both read and administer on the project. With the built-in roles that is the project's admins, and a " +
	"workspace owner or admin on every project."

func (r *teamspaceProjectResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state teamspaceProjectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	teamspaceID, projectID := state.TeamspaceID.ValueInt64(), state.ProjectID.ValueInt64()
	l, err := r.client.GetTeamspaceProject(ctx, teamspaceID, projectID)
	if err != nil {
		switch {
		case client.IsNotFound(err):
			// Unlinked, the team is gone, or the project is gone or cannot be seen.
			removeGone(ctx, r.client, resp, projectID, fmt.Sprintf("teamspace %d's link to project %d", teamspaceID, projectID), state.ID.ValueString())
		case client.IsForbidden(err):
			// Not gone and not readable: say so rather than guess either way.
			resp.Diagnostics.AddError(fmt.Sprintf("Cannot read teamspace %d's link to project %d", teamspaceID, projectID),
				"This token can see the project but may not read it, so Flightdeck will not show the link. It needs a "+
					"role with read access on the project.\n\nThe API said: "+apiMessage(err))
		default:
			addAPIError(&resp.Diagnostics, "Error reading a Flightdeck teamspace's project link", err)
		}
		return
	}
	newState := teamspaceProjectToModel(l)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Update is never called with a change: both ids require replacement.
func (r *teamspaceProjectResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var state teamspaceProjectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *teamspaceProjectResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state teamspaceProjectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID := state.ProjectID.ValueInt64()
	if err := r.client.UnlinkTeamspaceProject(ctx, state.TeamspaceID.ValueInt64(), projectID); err != nil {
		if client.IsForbidden(err) {
			resp.Diagnostics.AddError(fmt.Sprintf("Not allowed to unlink project %d", projectID),
				linkRuleText+" Nothing was unlinked.\n\nThe API said: "+apiMessage(err))
			return
		}
		addAPIError(&resp.Diagnostics, "Error unlinking a project from a Flightdeck teamspace", err)
	}
}

func (r *teamspaceProjectResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	teamspaceID, projectID, ok := parseImportPair(req.ID, "teamspace", "project", &resp.Diagnostics)
	if !ok {
		return
	}
	l, err := r.client.GetTeamspaceProject(ctx, teamspaceID, projectID)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error importing a Flightdeck teamspace's project link", err)
		return
	}
	state := teamspaceProjectToModel(l)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
