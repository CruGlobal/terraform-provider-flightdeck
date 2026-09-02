package provider

import (
	"context"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = &statesDataSource{}
	_ datasource.DataSourceWithConfigure = &statesDataSource{}
)

// NewStatesDataSource returns the flightdeck_states data source.
func NewStatesDataSource() datasource.DataSource { return &statesDataSource{} }

type statesDataSource struct {
	client *client.Client
}

type statesModel struct {
	ProjectID types.Int64  `tfsdk:"project_id"`
	States    []stateModel `tfsdk:"states"`
}

func (d *statesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_states"
}

func (d *statesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *statesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the workflow states of a Flightdeck project, in workflow order (group, then position). " +
			"Useful for finding the ids of the default states a project is created with, for example to import one " +
			"as a `flightdeck_state`.",
		Attributes: map[string]schema.Attribute{
			"project_id": schema.Int64Attribute{
				MarkdownDescription: "Id of the project whose states to list.",
				Required:            true,
			},
			"states": schema.ListNestedAttribute{
				MarkdownDescription: "The project's states.",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id":           schema.Int64Attribute{MarkdownDescription: "Numeric id of the state.", Computed: true},
						"project_id":   schema.Int64Attribute{MarkdownDescription: "Id of the owning project.", Computed: true},
						"name":         schema.StringAttribute{MarkdownDescription: "Display name.", Computed: true},
						"group":        schema.StringAttribute{MarkdownDescription: "Workflow group.", Computed: true},
						"color":        schema.StringAttribute{MarkdownDescription: "Hex color.", Computed: true},
						"default":      schema.BoolAttribute{MarkdownDescription: "Whether new work items land in this state.", Computed: true},
						"position":     schema.Int64Attribute{MarkdownDescription: "Sort position within the group.", Computed: true},
						"lock_version": schema.Int64Attribute{MarkdownDescription: "Optimistic-locking version.", Computed: true},
					},
				},
			},
		},
	}
}

func (d *statesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg statesModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	states, err := d.client.ListStates(ctx, cfg.ProjectID.ValueInt64())
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error listing Flightdeck states", err)
		return
	}
	out := statesModel{ProjectID: cfg.ProjectID, States: make([]stateModel, 0, len(states))}
	for i := range states {
		out.States = append(out.States, stateToModel(&states[i]))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
