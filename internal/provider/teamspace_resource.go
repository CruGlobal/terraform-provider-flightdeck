package provider

import (
	"context"
	"fmt"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = &teamspaceResource{}
	_ resource.ResourceWithConfigure   = &teamspaceResource{}
	_ resource.ResourceWithImportState = &teamspaceResource{}
)

// NewTeamspaceResource returns the flightdeck_teamspace resource.
func NewTeamspaceResource() resource.Resource { return &teamspaceResource{} }

type teamspaceResource struct {
	client *client.Client
}

// teamspaceModel is the resource's state and the data source's result.
type teamspaceModel struct {
	ID          types.Int64  `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	LeadID      types.Int64  `tfsdk:"lead_id"`
	LockVersion types.Int64  `tfsdk:"lock_version"`
}

func teamspaceToModel(t *client.Teamspace) teamspaceModel {
	// The API stores a blank description as null, but the web form can save
	// an empty box as "". Both mean no description, and a blank one cannot be
	// configured, so both read as null.
	description := types.StringNull()
	if t.Description != nil && !isBlank(*t.Description) {
		description = types.StringValue(*t.Description)
	}
	return teamspaceModel{
		ID:          types.Int64Value(t.ID),
		Name:        types.StringValue(t.Name),
		Description: description,
		LeadID:      types.Int64PointerValue(t.LeadID),
		LockVersion: types.Int64Value(t.LockVersion),
	}
}

// teamspaceFields builds the write body. A create sends only what is
// configured. An update sends all three, with null for an unset description
// or lead, because the configuration is the whole truth for both and the API
// reads null as "clear".
func teamspaceFields(plan *teamspaceModel, create bool) client.Fields {
	fields := client.Fields{"name": plan.Name.ValueString()}
	switch {
	case !plan.Description.IsNull():
		fields["description"] = plan.Description.ValueString()
	case !create:
		fields["description"] = nil
	}
	switch {
	case !plan.LeadID.IsNull():
		fields["lead_id"] = plan.LeadID.ValueInt64()
	case !create:
		fields["lead_id"] = nil
	}
	return fields
}

func (r *teamspaceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_teamspace"
}

func (r *teamspaceResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *teamspaceResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Flightdeck teamspace: a team in the workspace, with a name, a description and an " +
			"optional lead. A teamspace is how Flightdeck says which team owns a project, and a project can belong to " +
			"more than one team. The team's members and the projects it owns are separate resources, " +
			"`flightdeck_teamspace_member` and `flightdeck_teamspace_project`.\n\n" +
			"**Names are not unique.** Two teamspaces in a workspace can share a name, so refer to a team by its `id`. " +
			"The `flightdeck_teamspace` data source looks a team up by name, and fails unless exactly one team has it.\n\n" +
			"Any member of the workspace except a guest can create and change a teamspace. **Deleting** one unlinks " +
			"every project it owns, so it also needs the right to unlink each of them: read and administer on the " +
			"project, which a workspace owner or admin has on every project. That includes links made outside " +
			"Terraform. Without it the delete fails and nothing is removed.\n\n" +
			"A create carries an idempotency key derived from its arguments, so a create retried after a lost " +
			"response does not make a second team. Flightdeck answers a repeat of a create from the last 24 hours " +
			"with the team it made, and the provider takes that team only while it still has those arguments: one " +
			"that has since been renamed or changed is left alone and a new team is made. So two " +
			"`flightdeck_teamspace` resources with the same `name`, `description` and `lead_id`, created with the " +
			"same token within 24 hours of each other, are one create: both manage the same team, and destroying " +
			"either deletes it. Give such teams different descriptions.\n\n" +
			"Import by numeric id: `terraform import flightdeck_teamspace.platform 12`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				MarkdownDescription: "Numeric id of the teamspace, the one reference to a team that cannot become ambiguous.",
				Computed:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Display name. Not unique in the workspace, and must not be blank.",
				Required:            true,
				Validators:          []validator.String{notBlank("give the team a name")},
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "Free-text description. Leave it out for none; removing it clears the description. " +
					"It must not be blank, because Flightdeck stores a blank description as none.",
				Optional:   true,
				Validators: []validator.String{notBlank("leave `description` out for no description")},
			},
			"lead_id": schema.Int64Attribute{
				MarkdownDescription: "User id of the team's lead, who must be a member of the workspace. A " +
					"`flightdeck_workspace_member` data source resolves one from an email address. Leave it out for no " +
					"lead; removing it clears the lead.",
				Optional:   true,
				Validators: []validator.Int64{int64validator.AtLeast(1)},
			},
			"lock_version": schema.Int64Attribute{
				MarkdownDescription: "Optimistic-locking version the API bumps on every change. Sent as `If-Match` on updates and deletes.",
				Computed:            true,
			},
		},
	}
}

func (r *teamspaceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan teamspaceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	fields := teamspaceFields(&plan, true)
	created, err := r.client.CreateTeamspace(ctx, fields, client.PayloadKey("teamspace", "", fields))
	if err != nil {
		addTeamspaceWriteError(&resp.Diagnostics, "Error creating Flightdeck teamspace", err)
		return
	}
	state := teamspaceToModel(created)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *teamspaceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state teamspaceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	t, err := r.client.GetTeamspace(ctx, state.ID.ValueInt64())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		addAPIError(&resp.Diagnostics, "Error reading Flightdeck teamspace", err)
		return
	}
	newState := teamspaceToModel(t)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *teamspaceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state teamspaceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueInt64()
	updated, err := r.client.UpdateTeamspace(ctx, id, teamspaceFields(&plan, false), state.LockVersion.ValueInt64())
	if err != nil {
		if client.IsStale(err) {
			var current *int64
			if fresh, rerr := r.client.GetTeamspace(ctx, id); rerr == nil {
				current = &fresh.LockVersion
			}
			addStaleError(&resp.Diagnostics, fmt.Sprintf("Teamspace %q", state.Name.ValueString()), state.LockVersion.ValueInt64(), current, err)
			return
		}
		addTeamspaceWriteError(&resp.Diagnostics, "Error updating Flightdeck teamspace", err)
		return
	}
	newState := teamspaceToModel(updated)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *teamspaceResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state teamspaceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueInt64()
	err := deleteWithIfMatch(ctx, state.LockVersion.ValueInt64(),
		func(ctx context.Context, lv int64) error { return r.client.DeleteTeamspace(ctx, id, lv) },
		func(ctx context.Context) (int64, error) {
			fresh, err := r.client.GetTeamspace(ctx, id)
			if err != nil {
				return 0, err
			}
			return fresh.LockVersion, nil
		})
	switch {
	case err == nil:
	case client.IsForbidden(err):
		resp.Diagnostics.AddError(fmt.Sprintf("Teamspace %q cannot be deleted with this token", state.Name.ValueString()),
			"Flightdeck refused to delete the teamspace, and nothing was removed. A workspace guest cannot delete a "+
				"teamspace. Anyone else needs the right to unlink every project the team owns, because deleting the "+
				"team unlinks them all: read and administer on each project, which a workspace owner or admin has "+
				"everywhere. Links made outside Terraform count too. Delete it with a token that has that right, or "+
				"have someone who administers those projects unlink them first.\n\nThe API said: "+apiMessage(err))
	case client.IsStale(err):
		addStaleError(&resp.Diagnostics, fmt.Sprintf("Teamspace %q", state.Name.ValueString()), state.LockVersion.ValueInt64(), nil, err)
	default:
		addAPIError(&resp.Diagnostics, "Error deleting Flightdeck teamspace", err)
	}
}

func (r *teamspaceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, ok := parseImportID(req.ID, "teamspace", &resp.Diagnostics)
	if !ok {
		return
	}
	t, err := r.client.GetTeamspace(ctx, id)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error importing Flightdeck teamspace", err)
		return
	}
	state := teamspaceToModel(t)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// addTeamspaceWriteError reports a failed create or update. A 403 on these is
// the workspace guest floor: any other member may write a teamspace.
func addTeamspaceWriteError(diags *diag.Diagnostics, summary string, err error) {
	if client.IsForbidden(err) {
		diags.AddError("Workspace guests cannot create or change teamspaces",
			"Any member of the workspace except a guest can create and change a teamspace. Use a token whose user "+
				"is a workspace member, or ask a workspace admin to make this user one.\n\nThe API said: "+apiMessage(err))
		return
	}
	addAPIError(diags, summary, err)
}
