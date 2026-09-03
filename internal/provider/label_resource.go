package provider

import (
	"context"
	"fmt"
	"strconv"

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

var (
	_ resource.Resource                = &labelResource{}
	_ resource.ResourceWithConfigure   = &labelResource{}
	_ resource.ResourceWithImportState = &labelResource{}
)

// NewLabelResource returns the flightdeck_label resource.
func NewLabelResource() resource.Resource { return &labelResource{} }

type labelResource struct {
	client *client.Client
}

type labelModel struct {
	ID          types.Int64   `tfsdk:"id"`
	ProjectID   types.Int64   `tfsdk:"project_id"`
	Name        types.String  `tfsdk:"name"`
	Color       hexColorValue `tfsdk:"color"`
	LockVersion types.Int64   `tfsdk:"lock_version"`
}

func labelToModel(l *client.Label) labelModel {
	return labelModel{
		ID:          types.Int64Value(l.ID),
		ProjectID:   types.Int64Value(l.ProjectID),
		Name:        types.StringValue(l.Name),
		Color:       hexColorValue{StringValue: types.StringValue(l.Color)},
		LockVersion: types.Int64Value(l.LockVersion),
	}
}

func labelFields(plan *labelModel) client.Fields {
	fields := client.Fields{"name": plan.Name.ValueString()}
	if !plan.Color.IsNull() && !plan.Color.IsUnknown() {
		fields["color"] = plan.Color.ValueString()
	}
	return fields
}

func (r *labelResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_label"
}

func (r *labelResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *labelResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a label within a Flightdeck project.\n\n" +
			"A new project comes with starter labels (Bug, Feature, Enhancement, Documentation, Tech debt); import one " +
			"to manage it.\n\n" +
			"Import by numeric id: `terraform import flightdeck_label.bug 23`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				MarkdownDescription: "Numeric id of the label.",
				Computed:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"project_id": schema.Int64Attribute{
				MarkdownDescription: "Id of the project the label belongs to. Changing it replaces the label.",
				Required:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Display name; unique within the project.",
				Required:            true,
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"color": schema.StringAttribute{
				MarkdownDescription: "Hex color (`#rgb` or `#rrggbb`, compared case-insensitively). Defaults to the server's default.",
				CustomType:          hexColorType{},
				Optional:            true,
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				Validators: []validator.String{
					stringvalidator.RegexMatches(hexColorPattern, "must be a hex color such as #3b82f6"),
				},
			},
			"lock_version": schema.Int64Attribute{
				MarkdownDescription: "Optimistic-locking version the API bumps on every change. Sent as `If-Match` on updates and deletes.",
				Computed:            true,
			},
		},
	}
}

func (r *labelResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan labelModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID := plan.ProjectID.ValueInt64()
	fields := labelFields(&plan)
	created, err := r.client.CreateLabel(ctx, projectID, fields, client.PayloadKey("label", strconv.FormatInt(projectID, 10), fields))
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error creating Flightdeck label", err)
		return
	}
	state := labelToModel(created)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *labelResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state labelModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	l, err := r.client.GetLabel(ctx, state.ID.ValueInt64())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		addAPIError(&resp.Diagnostics, "Error reading Flightdeck label", err)
		return
	}
	newState := labelToModel(l)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *labelResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state labelModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueInt64()
	updated, err := r.client.UpdateLabel(ctx, id, labelFields(&plan), state.LockVersion.ValueInt64())
	if err != nil {
		if client.IsStale(err) {
			var current *int64
			if fresh, rerr := r.client.GetLabel(ctx, id); rerr == nil {
				current = &fresh.LockVersion
			}
			addStaleError(&resp.Diagnostics, fmt.Sprintf("Label %q", state.Name.ValueString()), state.LockVersion.ValueInt64(), current, err)
			return
		}
		addAPIError(&resp.Diagnostics, "Error updating Flightdeck label", err)
		return
	}
	newState := labelToModel(updated)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *labelResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state labelModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueInt64()
	err := deleteWithIfMatch(ctx, state.LockVersion.ValueInt64(),
		func(ctx context.Context, lv int64) error { return r.client.DeleteLabel(ctx, id, lv) },
		func(ctx context.Context) (int64, error) {
			fresh, err := r.client.GetLabel(ctx, id)
			if err != nil {
				return 0, err
			}
			return fresh.LockVersion, nil
		})
	if err != nil {
		if client.IsStale(err) {
			addStaleError(&resp.Diagnostics, fmt.Sprintf("Label %q", state.Name.ValueString()), state.LockVersion.ValueInt64(), nil, err)
			return
		}
		addAPIError(&resp.Diagnostics, "Error deleting Flightdeck label", err)
	}
}

func (r *labelResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, ok := parseImportID(req.ID, "label", &resp.Diagnostics)
	if !ok {
		return
	}
	l, err := r.client.GetLabel(ctx, id)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error importing Flightdeck label", err)
		return
	}
	state := labelToModel(l)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
