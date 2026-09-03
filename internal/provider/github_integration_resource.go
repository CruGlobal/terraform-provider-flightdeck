package provider

import (
	"context"
	"fmt"
	"strconv"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = &githubIntegrationResource{}
	_ resource.ResourceWithConfigure   = &githubIntegrationResource{}
	_ resource.ResourceWithImportState = &githubIntegrationResource{}
)

// NewGithubIntegrationResource returns the flightdeck_github_integration resource.
func NewGithubIntegrationResource() resource.Resource { return &githubIntegrationResource{} }

type githubIntegrationResource struct {
	client *client.Client
}

type githubIntegrationModel struct {
	ID                types.Int64  `tfsdk:"id"`
	ProjectID         types.Int64  `tfsdk:"project_id"`
	RepoFullName      types.String `tfsdk:"repo_full_name"`
	Enabled           types.Bool   `tfsdk:"enabled"`
	Secret            types.String `tfsdk:"secret"`
	WebhookRegistered types.Bool   `tfsdk:"webhook_registered"`
	LockVersion       types.Int64  `tfsdk:"lock_version"`
}

// githubIntegrationToModel maps an API integration. The secret is never
// returned by the API; state keeps only what the configuration supplied.
func githubIntegrationToModel(g *client.GithubIntegration, configuredSecret types.String) githubIntegrationModel {
	return githubIntegrationModel{
		ID:                types.Int64Value(g.ID),
		ProjectID:         types.Int64Value(g.ProjectID),
		RepoFullName:      types.StringValue(g.RepoFullName),
		Enabled:           types.BoolValue(g.Enabled),
		Secret:            configuredSecret,
		WebhookRegistered: types.BoolValue(g.WebhookRegistered),
		LockVersion:       types.Int64Value(g.LockVersion),
	}
}

func (r *githubIntegrationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_github_integration"
}

func (r *githubIntegrationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *githubIntegrationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Links a Flightdeck project to a GitHub repository so the repository's webhook deliveries " +
			"drive work-item transitions. Linking also records the repository on the project " +
			"(`flightdeck_project.github_repo_full_name`, which is read-only there); unlinking clears it.\n\n" +
			"Two modes follow from `secret`:\n\n" +
			"- **Flightdeck-managed** (`secret` omitted): Flightdeck generates the signing secret and registers the " +
			"repository webhook through its GitHub App. The App must be installed on the repository, or the create fails " +
			"with `repo_unreachable`. `webhook_registered` is `true`.\n" +
			"- **Caller-managed** (`secret` supplied): Flightdeck stores the secret and touches nothing on GitHub; you " +
			"declare the matching repository webhook yourself (for example a `github_repository_webhook` pointing at " +
			"Flightdeck's GitHub webhook endpoint with the same secret). `webhook_registered` is `false`.\n\n" +
			"A repository can be linked to one enabled integration per workspace (`repo_already_linked`). The secret " +
			"is write-only: it is sent on create only, never read back, and state holds only the value you configured. " +
			"Changing `repo_full_name` or `secret` replaces the integration (unlink, then link again).\n\n" +
			"Requires project-admin rights on the project; check your Flightdeck version's API documentation for the " +
			"exact role, which may be workspace admin. Import by numeric id: " +
			"`terraform import flightdeck_github_integration.app 17`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				MarkdownDescription: "Numeric id of the integration.",
				Computed:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"project_id": schema.Int64Attribute{
				MarkdownDescription: "Id of the project. Changing it replaces the integration.",
				Required:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"repo_full_name": schema.StringAttribute{
				MarkdownDescription: "Repository as `owner/repo`. Changing it replaces the integration.",
				Required:            true,
				Validators: []validator.String{
					stringvalidator.RegexMatches(client.RepoFullNamePattern, `must be "owner/repo"`),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether deliveries from the repository are processed. A new integration is enabled; " +
					"when unset, the current value is kept.",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"secret": schema.StringAttribute{
				MarkdownDescription: "Webhook signing secret for the caller-managed mode. Omit to let Flightdeck generate " +
					"one and register the webhook itself. Write-only: sent on create, never read back. Changing it " +
					"replaces the integration.",
				Optional:      true,
				Sensitive:     true,
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"webhook_registered": schema.BoolAttribute{
				MarkdownDescription: "Whether Flightdeck registered the repository webhook through its GitHub App " +
					"(`true` in the Flightdeck-managed mode, `false` when you supplied `secret`).",
				Computed:      true,
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"lock_version": schema.Int64Attribute{
				MarkdownDescription: "Optimistic-locking version the API bumps on every change. Sent as `If-Match` on updates and deletes.",
				Computed:            true,
			},
		},
	}
}

func (r *githubIntegrationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan githubIntegrationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID := plan.ProjectID.ValueInt64()
	fields := client.Fields{"repo_full_name": plan.RepoFullName.ValueString()}
	if !plan.Enabled.IsNull() && !plan.Enabled.IsUnknown() {
		fields["enabled"] = plan.Enabled.ValueBool()
	}
	if !plan.Secret.IsNull() && !plan.Secret.IsUnknown() {
		fields["secret"] = plan.Secret.ValueString()
	}
	created, err := r.client.CreateGithubIntegration(ctx, projectID, fields, client.PayloadKey("github_integration", strconv.FormatInt(projectID, 10), fields))
	if err != nil {
		switch {
		case client.HasCode(err, client.CodeRepoUnreachable):
			resp.Diagnostics.AddAttributeError(pathRoot("repo_full_name"), "Flightdeck's GitHub App cannot reach the repository",
				apiMessage(err)+"\n\nInstall the App on the repository, or supply `secret` and register the webhook yourself.")
		case client.HasCode(err, client.CodeRepoAlreadyLinked):
			resp.Diagnostics.AddAttributeError(pathRoot("repo_full_name"), "Repository already linked", apiMessage(err))
		default:
			addAPIError(&resp.Diagnostics, "Error linking GitHub repository", err)
		}
		return
	}
	state := githubIntegrationToModel(created, plan.Secret)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *githubIntegrationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state githubIntegrationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	g, err := r.client.GetGithubIntegration(ctx, state.ID.ValueInt64())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		addAPIError(&resp.Diagnostics, "Error reading Flightdeck GitHub integration", err)
		return
	}
	newState := githubIntegrationToModel(g, state.Secret)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *githubIntegrationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state githubIntegrationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueInt64()
	fields := client.Fields{}
	if !plan.Enabled.IsNull() && !plan.Enabled.IsUnknown() {
		fields["enabled"] = plan.Enabled.ValueBool()
	}
	updated, err := r.client.UpdateGithubIntegration(ctx, id, fields, state.LockVersion.ValueInt64())
	if err != nil {
		switch {
		case client.IsStale(err):
			var current *int64
			if fresh, rerr := r.client.GetGithubIntegration(ctx, id); rerr == nil {
				current = &fresh.LockVersion
			}
			addStaleError(&resp.Diagnostics, fmt.Sprintf("GitHub integration for %s", state.RepoFullName.ValueString()), state.LockVersion.ValueInt64(), current, err)
		case client.HasCode(err, client.CodeRepoAlreadyLinked):
			resp.Diagnostics.AddAttributeError(pathRoot("enabled"), "Repository already linked", apiMessage(err))
		default:
			addAPIError(&resp.Diagnostics, "Error updating Flightdeck GitHub integration", err)
		}
		return
	}
	newState := githubIntegrationToModel(updated, state.Secret)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Delete unlinks the repository. The API clears the project's
// github_repo_full_name when it matches; nothing else to do here.
func (r *githubIntegrationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state githubIntegrationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueInt64()
	err := deleteWithIfMatch(ctx, state.LockVersion.ValueInt64(),
		func(ctx context.Context, lv int64) error { return r.client.DeleteGithubIntegration(ctx, id, lv) },
		func(ctx context.Context) (int64, error) {
			fresh, err := r.client.GetGithubIntegration(ctx, id)
			if err != nil {
				return 0, err
			}
			return fresh.LockVersion, nil
		})
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error unlinking GitHub repository", err)
	}
}

// ImportState imports by integration id; the secret is never recoverable.
func (r *githubIntegrationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, ok := parseImportID(req.ID, "GitHub integration", &resp.Diagnostics)
	if !ok {
		return
	}
	g, err := r.client.GetGithubIntegration(ctx, id)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error importing Flightdeck GitHub integration", err)
		return
	}
	state := githubIntegrationToModel(g, types.StringNull())
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
