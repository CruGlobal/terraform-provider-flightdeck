package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
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
	Email types.String `tfsdk:"email"`
	ID    types.Int64  `tfsdk:"id"`
	Name  types.String `tfsdk:"name"`
	Kind  types.String `tfsdk:"kind"`
}

func (d *workspaceMemberDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_workspace_member"
}

func (d *workspaceMemberDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *workspaceMemberDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Resolves a member of the token's workspace by email address, so a configuration can say who " +
			"should have access rather than which numeric user id — the form `flightdeck_project_member.user_id` and " +
			"`flightdeck_project.lead_id` take.\n\n" +
			"The lookup is an **exact match**. Case and surrounding whitespace are ignored, but there is no partial or " +
			"prefix matching, so the address has to be the one Flightdeck holds. An address that resolves to nobody " +
			"fails the plan rather than yielding an empty id.\n\n" +
			"Any workspace member may look up a person. **Service accounts are visible only to workspace admins**: to " +
			"any other token a bot's address simply resolves to nothing, exactly like an unknown address. A service " +
			"account's own token is capped at the member role, so it can never resolve another service account.",
		Attributes: map[string]schema.Attribute{
			"email": schema.StringAttribute{
				MarkdownDescription: "Email address of the workspace member, matched exactly (case and surrounding whitespace ignored).",
				Required:            true,
				Validators:          []validator.String{stringvalidator.LengthAtLeast(3)},
			},
			"id": schema.Int64Attribute{
				MarkdownDescription: "Numeric user id, for `flightdeck_project_member.user_id` or `flightdeck_project.lead_id`.",
				Computed:            true,
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Display name.",
				Computed:            true,
			},
			"kind": schema.StringAttribute{
				MarkdownDescription: "`human` for a person, `service` for a service account. Worth asserting when granting an " +
					"agent principal access: a service account's email is synthetic but still looks like an address, so " +
					"this is what distinguishes the bot from a person with a similar one.",
				Computed: true,
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
	email := strings.TrimSpace(cfg.Email.ValueString())
	members, err := d.client.FindWorkspaceMembersByEmail(ctx, email)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error resolving Flightdeck workspace member", err)
		return
	}
	switch len(members) {
	case 1:
	case 0:
		// A miss and "not visible to this token" are the same answer, so the
		// message has to offer both rather than assert the address is unknown.
		resp.Diagnostics.AddAttributeError(path.Root("email"), "No such workspace member",
			fmt.Sprintf("No member of this workspace has the email address %q.\n\n"+
				"The lookup is exact — case and surrounding whitespace are ignored, but a prefix or a fragment will not "+
				"match — so check the address against the workspace's member list.\n\n"+
				"If it belongs to a service account, it resolves only for a workspace admin: with any other token the "+
				"account is absent from the directory rather than refused, which looks identical to an unknown address. "+
				"A service account's own token never resolves another service account.", email))
		return
	default:
		// The filter guarantees at most one row. Refuse rather than pick.
		resp.Diagnostics.AddAttributeError(path.Root("email"), "Ambiguous workspace member lookup",
			fmt.Sprintf("Flightdeck returned %d members for the email address %q, but an exact-match lookup should "+
				"resolve at most one. Refusing rather than choosing one of them.", len(members), email))
		return
	}
	m := members[0]
	resp.Diagnostics.Append(resp.State.Set(ctx, &workspaceMemberModel{
		// Echoed as configured: Terraform requires an argument to come back
		// unchanged, and the API's spelling may differ in case or whitespace.
		Email: cfg.Email,
		ID:    types.Int64Value(m.ID),
		Name:  types.StringValue(m.Name),
		Kind:  types.StringValue(m.Kind),
	})...)
}
