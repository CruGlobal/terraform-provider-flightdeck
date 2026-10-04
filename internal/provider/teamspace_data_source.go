package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

var (
	_ datasource.DataSource                     = &teamspaceDataSource{}
	_ datasource.DataSourceWithConfigure        = &teamspaceDataSource{}
	_ datasource.DataSourceWithConfigValidators = &teamspaceDataSource{}
)

// NewTeamspaceDataSource returns the flightdeck_teamspace data source.
func NewTeamspaceDataSource() datasource.DataSource { return &teamspaceDataSource{} }

type teamspaceDataSource struct {
	client *client.Client
}

func (d *teamspaceDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_teamspace"
}

func (d *teamspaceDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *teamspaceDataSource) ConfigValidators(_ context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(path.MatchRoot("id"), path.MatchRoot("name")),
	}
}

func (d *teamspaceDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up a Flightdeck teamspace by numeric `id` or by `name`. Exactly one of the two must " +
			"be set.\n\n" +
			"**Names are not unique**, so a lookup by name fails unless exactly one teamspace in the workspace has that " +
			"name; it never picks one of several. The match is on the whole name and ignores letter case, but nothing " +
			"else: no partial matches, and blank space counts. A lookup by `id` cannot become ambiguous later, so " +
			"prefer it where you can. A name can be at most 255 characters long for the lookup, not counting blank " +
			"space at either end; a team with a longer name can only be found by its id.\n\n" +
			"Any member of the workspace, guests included, can read teamspaces.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				MarkdownDescription: "Numeric id of the teamspace. Set this or `name`.",
				Optional:            true,
				Computed:            true,
				Validators:          []validator.Int64{int64validator.AtLeast(1)},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Name of the teamspace, matched whole and ignoring letter case. The lookup fails " +
					"unless exactly one teamspace has it. Set this or `id`.",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					notBlank("give the name of the team to look up"),
					trimmedLengthAtMost(client.TeamspaceNameFilterMaxLength),
				},
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "Free-text description, or null when there is none.",
				Computed:            true,
			},
			"lead_id": schema.Int64Attribute{
				MarkdownDescription: "User id of the team's lead, or null when it has none.",
				Computed:            true,
			},
			"lock_version": schema.Int64Attribute{
				MarkdownDescription: "Optimistic-locking version the API bumps on every change.",
				Computed:            true,
			},
		},
	}
}

func (d *teamspaceDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg teamspaceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !cfg.ID.IsNull() {
		t, err := d.client.GetTeamspace(ctx, cfg.ID.ValueInt64())
		if err != nil {
			if client.IsNotFound(err) {
				resp.Diagnostics.AddAttributeError(path.Root("id"), "No such teamspace",
					fmt.Sprintf("There is no teamspace with id %d in the token's workspace.", cfg.ID.ValueInt64()))
				return
			}
			addAPIError(&resp.Diagnostics, "Error reading Flightdeck teamspace", err)
			return
		}
		state := teamspaceToModel(t)
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		return
	}

	name := cfg.Name.ValueString()
	matches, err := d.client.FindTeamspacesByName(ctx, name)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error looking up Flightdeck teamspace", err)
		return
	}
	switch len(matches) {
	case 1:
	case 0:
		resp.Diagnostics.AddAttributeError(path.Root("name"), "No such teamspace",
			fmt.Sprintf("No teamspace in this workspace is named %q.\n\n"+
				"The match is on the whole name and ignores letter case, but nothing else: a part of the name will not "+
				"match, and neither will extra or missing blank space.", name))
		return
	default:
		ids := make([]string, len(matches))
		for i, t := range matches {
			ids[i] = strconv.FormatInt(t.ID, 10)
		}
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Ambiguous teamspace name",
			fmt.Sprintf("%d teamspaces in this workspace are named %q (ids %s). Teamspace names are not unique, so "+
				"this lookup refuses to pick one. Look the team up by `id` instead, or rename the others.",
				len(matches), name, strings.Join(ids, ", ")))
		return
	}
	state := teamspaceToModel(&matches[0])
	// Echoed as configured: Terraform requires an argument to come back
	// unchanged, and the stored name may differ in letter case.
	state.Name = cfg.Name
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
