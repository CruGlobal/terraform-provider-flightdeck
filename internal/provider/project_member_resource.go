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

// projectMemberModel: id is the MEMBERSHIP row id (what the API's by-id routes
// take); user_id is the member.
type projectMemberModel struct {
	ID          types.Int64  `tfsdk:"id"`
	ProjectID   types.Int64  `tfsdk:"project_id"`
	UserID      types.Int64  `tfsdk:"user_id"`
	Role        types.String `tfsdk:"role"`
	BuiltinRole types.String `tfsdk:"builtin_role"`
	LockVersion types.Int64  `tfsdk:"lock_version"`
}

func projectMemberToModel(m *client.ProjectMember) projectMemberModel {
	out := projectMemberModel{
		ID:          types.Int64Value(m.ID),
		ProjectID:   types.Int64Value(m.ProjectID),
		UserID:      types.Int64Value(m.UserID),
		Role:        types.StringValue(m.Role),
		BuiltinRole: types.StringNull(),
		LockVersion: types.Int64Value(m.LockVersion),
	}
	if m.BuiltinRole != "" {
		out.BuiltinRole = types.StringValue(m.BuiltinRole)
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
		MarkdownDescription: "Manages a user's membership of a Flightdeck project — one membership row, addressed by its own " +
			"id. The user must already be a member of the workspace; the API has no route to look a user up by email, " +
			"so `user_id` is the numeric user id (visible in the workspace's member list).\n\n" +
			"The role is one of the built-in roles (`" + strings.Join(client.ProjectMemberRoles, "`, `") + "`) or a " +
			"custom role key defined by the workspace's permission scheme; the API rejects anything else. Changing " +
			"`user_id` replaces the membership (the API refuses to move a row to another user).\n\n" +
			"Reads and writes require `administer_project` on the project. The user who created a project is written " +
			"in as its admin automatically; managing that membership here requires importing it first.\n\n" +
			"Import with `<project_id>/<membership_id>`, or `<project_id>/user:<user_id>` to look the membership up " +
			"by user: `terraform import flightdeck_project_member.deploy_bot 42/user:7`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				MarkdownDescription: "Numeric id of the membership row (not the user).",
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
			"builtin_role": schema.StringAttribute{
				MarkdownDescription: "The built-in role the membership rests on, which equals `role` unless a custom role key is assigned.",
				Computed:            true,
			},
			"lock_version": schema.Int64Attribute{
				MarkdownDescription: "Optimistic-locking version the API bumps on every change. Sent as `If-Match` on updates and deletes.",
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
	m, err := r.client.GetProjectMember(ctx, state.ProjectID.ValueInt64(), state.ID.ValueInt64())
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
	projectID, id := state.ProjectID.ValueInt64(), state.ID.ValueInt64()
	updated, err := r.client.UpdateProjectMember(ctx, projectID, id, client.Fields{"role": plan.Role.ValueString()}, state.LockVersion.ValueInt64())
	if err != nil {
		if client.IsStale(err) {
			var current *int64
			if fresh, rerr := r.client.GetProjectMember(ctx, projectID, id); rerr == nil {
				current = &fresh.LockVersion
			}
			addStaleError(&resp.Diagnostics, fmt.Sprintf("Membership %d (user %d on project %d)", id, state.UserID.ValueInt64(), projectID),
				state.LockVersion.ValueInt64(), current, err)
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
	projectID, id := state.ProjectID.ValueInt64(), state.ID.ValueInt64()
	err := deleteWithIfMatch(ctx, state.LockVersion.ValueInt64(),
		func(ctx context.Context, lv int64) error { return r.client.RemoveProjectMember(ctx, projectID, id, lv) },
		func(ctx context.Context) (int64, error) {
			fresh, err := r.client.GetProjectMember(ctx, projectID, id)
			if err != nil {
				return 0, err
			}
			return fresh.LockVersion, nil
		})
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error removing Flightdeck project member", err)
	}
}

// ImportState accepts `<project_id>/<membership_id>` or
// `<project_id>/user:<user_id>` (the latter looks the membership up in the
// project's member list).
func (r *projectMemberResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(strings.TrimSpace(req.ID), "/")
	if len(parts) != 2 {
		resp.Diagnostics.AddError("Invalid import id",
			fmt.Sprintf("Expected <project_id>/<membership_id> or <project_id>/user:<user_id> (for example 42/7 or 42/user:7), got %q.", req.ID))
		return
	}
	projectID, ok := parseImportID(parts[0], "project", &resp.Diagnostics)
	if !ok {
		return
	}
	var (
		m   *client.ProjectMember
		err error
	)
	if userPart, byUser := strings.CutPrefix(parts[1], "user:"); byUser {
		userID, ok := parseImportID(userPart, "user", &resp.Diagnostics)
		if !ok {
			return
		}
		m, err = r.client.FindProjectMemberByUser(ctx, projectID, userID)
	} else {
		membershipID, ok := parseImportID(parts[1], "membership", &resp.Diagnostics)
		if !ok {
			return
		}
		m, err = r.client.GetProjectMember(ctx, projectID, membershipID)
	}
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error importing Flightdeck project member", err)
		return
	}
	state := projectMemberToModel(m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
