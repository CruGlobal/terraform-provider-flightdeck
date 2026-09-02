package provider

import (
	"context"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource                     = &projectDataSource{}
	_ datasource.DataSourceWithConfigure        = &projectDataSource{}
	_ datasource.DataSourceWithConfigValidators = &projectDataSource{}
)

// NewProjectDataSource returns the flightdeck_project data source.
func NewProjectDataSource() datasource.DataSource { return &projectDataSource{} }

type projectDataSource struct {
	client *client.Client
}

func (d *projectDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_project"
}

func (d *projectDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *projectDataSource) ConfigValidators(_ context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(path.MatchRoot("id"), path.MatchRoot("identifier")),
	}
}

func (d *projectDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up a Flightdeck project by numeric `id` or by `identifier` (the short key that " +
			"prefixes its work items). Exactly one of the two must be set.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				MarkdownDescription: "Numeric id of the project. Set this or `identifier`.",
				Optional:            true,
				Computed:            true,
			},
			"identifier": schema.StringAttribute{
				MarkdownDescription: "Project identifier, e.g. `APP` (case-insensitive). Set this or `id`.",
				Optional:            true,
				Computed:            true,
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Display name.",
				Computed:            true,
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "Free-text description.",
				Computed:            true,
			},
			"emoji": schema.StringAttribute{
				MarkdownDescription: "Emoji shown next to the project name.",
				Computed:            true,
			},
			"archived": schema.BoolAttribute{
				MarkdownDescription: "Whether the project is archived.",
				Computed:            true,
			},
			"features": schema.MapAttribute{
				MarkdownDescription: "Effective value of every feature toggle the API reports, including read-only ones " +
					"such as `self_healing` and `slack`.",
				ElementType: types.BoolType,
				Computed:    true,
			},
			"github_repo_full_name": schema.StringAttribute{
				MarkdownDescription: "GitHub repository the project maps to, as `owner/repo`, if any.",
				Computed:            true,
			},
			"lock_version": schema.Int64Attribute{
				MarkdownDescription: "Optimistic-locking version the API bumps on every change.",
				Computed:            true,
			},
			"self_healing": schema.SingleNestedAttribute{
				MarkdownDescription: "Resolved self-healing control-loop configuration (armed flag and thresholds). " +
					"Reported only when the token's user is a workspace admin; null otherwise.",
				Computed: true,
				Attributes: map[string]schema.Attribute{
					"armed":                   schema.BoolAttribute{MarkdownDescription: "Whether live rollback is armed.", Computed: true},
					"bake_minutes":            schema.Int64Attribute{MarkdownDescription: "Eligibility window after a deploy, in minutes.", Computed: true},
					"baseline_multiplier":     schema.Float64Attribute{MarkdownDescription: "Required multiple of the baseline error rate.", Computed: true},
					"absolute_floor":          schema.Float64Attribute{MarkdownDescription: "Required errors per minute floor.", Computed: true},
					"long_window_minutes":     schema.Int64Attribute{MarkdownDescription: "Long burn-rate window in minutes.", Computed: true},
					"short_window_minutes":    schema.Int64Attribute{MarkdownDescription: "Short burn-rate window in minutes.", Computed: true},
					"burn_rate":               schema.Float64Attribute{MarkdownDescription: "Burn rate that counts as severe.", Computed: true},
					"sustain_count":           schema.Int64Attribute{MarkdownDescription: "Consecutive trips required before acting.", Computed: true},
					"consecutive_error_limit": schema.Int64Attribute{MarkdownDescription: "Metrics-query failures tolerated before inconclusive.", Computed: true},
					"cooldown_minutes":        schema.Int64Attribute{MarkdownDescription: "Cooldown between actions, in minutes.", Computed: true},
					"max_rollbacks_per_hour":  schema.Int64Attribute{MarkdownDescription: "Per-app blast-radius cap.", Computed: true},
					"recovery_window_minutes": schema.Int64Attribute{MarkdownDescription: "Post-rollback grace period, in minutes.", Computed: true},
				},
			},
		},
	}
}

func (d *projectDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg projectModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var (
		p   *client.Project
		err error
	)
	if !cfg.ID.IsNull() {
		p, err = d.client.GetProject(ctx, cfg.ID.ValueInt64())
	} else {
		p, err = d.client.FindProjectByIdentifier(ctx, cfg.Identifier.ValueString())
	}
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error reading Flightdeck project", err)
		return
	}

	state := projectToModel(ctx, p, nil, featuresAll, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
