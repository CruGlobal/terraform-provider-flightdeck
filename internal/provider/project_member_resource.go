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
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = &projectMemberResource{}
	_ resource.ResourceWithConfigure   = &projectMemberResource{}
	_ resource.ResourceWithImportState = &projectMemberResource{}
)

// NewProjectMemberResource returns the flightdeck_project_member resource.
func NewProjectMemberResource() resource.Resource { return &projectMemberResource{} }

type projectMemberResource struct {
	client *client.Client
}

type projectMemberModel struct {
	ID          types.Int64  `tfsdk:"id"`
	ProjectID   types.Int64  `tfsdk:"project_id"`
	UserID      types.Int64  `tfsdk:"user_id"`
	Role        types.String `tfsdk:"role"`
	LockVersion types.Int64  `tfsdk:"lock_version"`
}

func projectMemberToModel(m *client.ProjectMember) projectMemberModel {
	out := projectMemberModel{
		ID:          types.Int64Value(m.ID),
		ProjectID:   types.Int64Value(m.ProjectID),
		UserID:      types.Int64Value(m.UserID),
		Role:        types.StringValue(m.Role),
		LockVersion: types.Int64Null(),
	}
	if m.LockVersion != nil {
		out.LockVersion = types.Int64Value(*m.LockVersion)
	}
	return out
}

func (r *projectMemberResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_project_member"
}

func (r *projectMemberResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *projectMemberResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a user's role on a Flightdeck project. Use the `flightdeck_workspace_member` data source " +
			"to resolve a `user_id` from an email address.\n\n" +
			"The user must already be a member of the workspace. The role is one of the built-in roles (`" +
			strings.Join(client.ProjectMemberRoles, "`, `") + "`) or a custom role key defined by the workspace's " +
			"permission scheme; the API rejects anything else.\n\n" +
			"Note that the user who created a project is written in as its admin automatically; managing that " +
			"membership here requires importing it first.\n\n" +
			"Import with `<project_id>/<user_id>`: `terraform import flightdeck_project_member.deploy_bot 42/7`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				MarkdownDescription: "Numeric id of the membership row.",
				Computed:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"project_id": schema.Int64Attribute{
				MarkdownDescription: "Id of the project. Changing it replaces the membership.",
				Required:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"user_id": schema.Int64Attribute{
				MarkdownDescription: "Id of the workspace member. Changing it replaces the membership.",
				Required:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"role": schema.StringAttribute{
				MarkdownDescription: "Project role: a built-in (`" + strings.Join(client.ProjectMemberRoles, "`, `") +
					"`) or a custom role key from the workspace's permission scheme.",
				Required:   true,
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"lock_version": schema.Int64Attribute{
				MarkdownDescription: "Optimistic-locking version, when the API versions membership rows. Sent as `If-Match` on updates when known.",
				Computed:            true,
			},
		},
	}
}

func (r *projectMemberResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan projectMemberModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID := plan.ProjectID.ValueInt64()
	fields := client.Fields{"user_id": plan.UserID.ValueInt64(), "role": plan.Role.ValueString()}
	created, err := r.client.AddProjectMember(ctx, projectID, fields, client.PayloadKey("project_member", strconv.FormatInt(projectID, 10), fields))
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error adding Flightdeck project member", err)
		return
	}
	state := projectMemberToModel(created)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *projectMemberResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state projectMemberModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	m, err := r.client.FindProjectMember(ctx, state.ProjectID.ValueInt64(), state.UserID.ValueInt64())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		addAPIError(&resp.Diagnostics, "Error reading Flightdeck project member", err)
		return
	}
	newState := projectMemberToModel(m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *projectMemberResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state projectMemberModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID, userID := state.ProjectID.ValueInt64(), state.UserID.ValueInt64()
	var lockVersion *int64
	if !state.LockVersion.IsNull() && !state.LockVersion.IsUnknown() {
		v := state.LockVersion.ValueInt64()
		lockVersion = &v
	}
	updated, err := r.client.UpdateProjectMember(ctx, projectID, userID, client.Fields{"role": plan.Role.ValueString()}, lockVersion)
	if err != nil {
		if client.IsStale(err) {
			var current *int64
			if fresh, rerr := r.client.FindProjectMember(ctx, projectID, userID); rerr == nil {
				current = fresh.LockVersion
			}
			var stateVersion int64
			if lockVersion != nil {
				stateVersion = *lockVersion
			}
			addStaleError(&resp.Diagnostics, fmt.Sprintf("Membership of user %d on project %d", userID, projectID), stateVersion, current)
			return
		}
		addAPIError(&resp.Diagnostics, "Error updating Flightdeck project member", err)
		return
	}
	newState := projectMemberToModel(updated)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *projectMemberResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state projectMemberModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.RemoveProjectMember(ctx, state.ProjectID.ValueInt64(), state.UserID.ValueInt64()); err != nil {
		addAPIError(&resp.Diagnostics, "Error removing Flightdeck project member", err)
	}
}

func (r *projectMemberResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(strings.TrimSpace(req.ID), "/")
	if len(parts) != 2 {
		resp.Diagnostics.AddError("Invalid import id", fmt.Sprintf("Expected <project_id>/<user_id> (for example 42/7), got %q.", req.ID))
		return
	}
	projectID, ok := parseImportID(parts[0], "project", &resp.Diagnostics)
	if !ok {
		return
	}
	userID, ok := parseImportID(parts[1], "user", &resp.Diagnostics)
	if !ok {
		return
	}
	m, err := r.client.FindProjectMember(ctx, projectID, userID)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error importing Flightdeck project member", err)
		return
	}
	state := projectMemberToModel(m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
