package provider

import (
	"context"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = &workspaceMemberDataSource{}
	_ datasource.DataSourceWithConfigure = &workspaceMemberDataSource{}
)

// NewWorkspaceMemberDataSource returns the flightdeck_workspace_member data source.
func NewWorkspaceMemberDataSource() datasource.DataSource { return &workspaceMemberDataSource{} }

type workspaceMemberDataSource struct {
	client *client.Client
}

type workspaceMemberModel struct {
	ID    types.Int64  `tfsdk:"id"`
	Email types.String `tfsdk:"email"`
	Name  types.String `tfsdk:"name"`
	Role  types.String `tfsdk:"role"`
}

func (d *workspaceMemberDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_workspace_member"
}

func (d *workspaceMemberDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *workspaceMemberDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Resolves a member of the token's workspace by email address, so configuration can say who " +
			"should have access to a project rather than which user id. Email matching is case-insensitive.",
		Attributes: map[string]schema.Attribute{
			"email": schema.StringAttribute{
				MarkdownDescription: "Email address of the workspace member.",
				Required:            true,
				Validators:          []validator.String{stringvalidator.LengthAtLeast(3)},
			},
			"id": schema.Int64Attribute{
				MarkdownDescription: "Numeric user id, for `flightdeck_project_member.user_id`.",
				Computed:            true,
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Display name.",
				Computed:            true,
			},
			"role": schema.StringAttribute{
				MarkdownDescription: "Workspace role (`guest`, `member`, `admin`, `owner`), when the API reports it.",
				Computed:            true,
			},
		},
	}
}

func (d *workspaceMemberDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg workspaceMemberModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	m, err := d.client.FindWorkspaceMemberByEmail(ctx, cfg.Email.ValueString())
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error resolving Flightdeck workspace member", err)
		return
	}
	state := workspaceMemberModel{
		ID:    types.Int64Value(m.ID),
		Email: types.StringValue(m.Email),
		Name:  types.StringValue(m.Name),
		Role:  types.StringNull(),
	}
	if m.Role != "" {
		state.Role = types.StringValue(m.Role)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
