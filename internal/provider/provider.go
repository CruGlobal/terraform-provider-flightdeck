package provider

import (
	"context"
	"os"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Environment variables the provider falls back to when the corresponding
// provider-block attribute is unset.
const (
	envEndpoint = "FLIGHTDECK_ENDPOINT"
	envToken    = "FLIGHTDECK_TOKEN"
)

var _ provider.Provider = &flightdeckProvider{}

type flightdeckProvider struct {
	// version is the provider version on release, "dev" for a local build, and
	// "test" under the test harness.
	version string
}

type flightdeckProviderModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	Token    types.String `tfsdk:"token"`
}

// New returns the provider constructor the plugin server and the test harness
// both use.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &flightdeckProvider{version: version}
	}
}

func (p *flightdeckProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "flightdeck"
	resp.Version = p.version
}

func (p *flightdeckProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The `flightdeck` provider manages a Flightdeck workspace's project configuration " +
			"— projects, workflow states, labels, members, ingestion tokens, " +
			"error alert rules and outbound webhooks — through its REST API, so that configuration can live in " +
			"Terraform alongside the rest of an application's infrastructure. Flightdeck is a project-management " +
			"application (workspaces, projects, work items) with built-in error tracking and incident management.\n\n" +
			"## Authentication\n\n" +
			"The provider authenticates with a Flightdeck **personal access token** (`fd_pat_…`), created under " +
			"your account's API tokens page in the Flightdeck UI. A token is bound to one workspace, so a provider " +
			"instance manages exactly that workspace; configure a second aliased provider to manage another. " +
			"The token acts as the user who created it and is subject to the same per-project role checks as the UI.\n\n" +
			"Both attributes fall back to environment variables (`FLIGHTDECK_ENDPOINT`, `FLIGHTDECK_TOKEN`); " +
			"keeping the token out of configuration files is recommended.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				MarkdownDescription: "Base URL of the Flightdeck deployment, for example `https://flightdeck.example.com`. " +
					"The `/api/v1` path is appended automatically. Falls back to the `FLIGHTDECK_ENDPOINT` environment variable.",
				Optional: true,
			},
			"token": schema.StringAttribute{
				MarkdownDescription: "Personal access token (`fd_pat_…`) used as the bearer token on every request. " +
					"Falls back to the `FLIGHTDECK_TOKEN` environment variable.",
				Optional:  true,
				Sensitive: true,
			},
		},
	}
}

func (p *flightdeckProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg flightdeckProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if cfg.Endpoint.IsUnknown() {
		resp.Diagnostics.AddAttributeError(
			path.Root("endpoint"),
			"Unknown Flightdeck endpoint",
			"The provider cannot create the Flightdeck API client because `endpoint` is unknown at configure time. "+
				"Either target-apply the source of the value first, set it statically, or use the "+envEndpoint+" environment variable.",
		)
	}
	if cfg.Token.IsUnknown() {
		resp.Diagnostics.AddAttributeError(
			path.Root("token"),
			"Unknown Flightdeck token",
			"The provider cannot create the Flightdeck API client because `token` is unknown at configure time. "+
				"Either target-apply the source of the value first, set it statically, or use the "+envToken+" environment variable.",
		)
	}
	if resp.Diagnostics.HasError() {
		return
	}

	endpoint := stringValueOrEnv(cfg.Endpoint, envEndpoint)
	token := stringValueOrEnv(cfg.Token, envToken)

	if endpoint == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("endpoint"),
			"Missing Flightdeck endpoint",
			"Set `endpoint` in the provider configuration or the "+envEndpoint+" environment variable to the base URL "+
				"of your Flightdeck deployment (for example https://flightdeck.example.com).",
		)
	}
	if token == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("token"),
			"Missing Flightdeck token",
			"Set `token` in the provider configuration or the "+envToken+" environment variable to a Flightdeck "+
				"personal access token (fd_pat_…).",
		)
	}
	if resp.Diagnostics.HasError() {
		return
	}

	c, err := client.New(endpoint, token, client.WithUserAgent(client.DefaultUserAgent+"/"+p.version))
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Invalid Flightdeck endpoint", err.Error())
		return
	}

	resp.DataSourceData = c
	resp.ResourceData = c
}

func (p *flightdeckProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{}
}

func (p *flightdeckProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{}
}

// stringValueOrEnv resolves the config-then-environment fallback: a non-empty
// configured value wins, otherwise the first non-empty environment variable.
func stringValueOrEnv(v types.String, envVars ...string) string {
	if !v.IsNull() && !v.IsUnknown() && v.ValueString() != "" {
		return v.ValueString()
	}
	for _, name := range envVars {
		if got := os.Getenv(name); got != "" {
			return got
		}
	}
	return ""
}
