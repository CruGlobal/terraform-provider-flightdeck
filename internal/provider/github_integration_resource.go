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
			"with `repo_unreachable`. `webhook_registered` is `true` when Flightdeck registered the hook itself; if a " +
			"webhook targeting Flightdeck already exists on the repository it is left in place and not claimed.\n" +
			"- **Caller-managed** (`secret` supplied): Flightdeck stores the secret and touches nothing on GitHub; you " +
			"declare the matching repository webhook yourself (for example a `github_repository_webhook` pointing at " +
			"Flightdeck's GitHub webhook endpoint with the same secret). `webhook_registered` is `false`.\n\n" +
			"A repository can be linked once across the workspace, enabled or not (`repo_already_linked`), and a " +
			"project can have one enabled integration at a time. The secret is write-only: sent on create only, " +
			"never read back, and state holds only the value you configured; it must be at least 16 characters " +
			"(a blank value counts as omitted). Changing `repo_full_name` or `secret` replaces the integration " +
			"(unlink, then link again), since a webhook has to be torn down and another registered.\n\n" +
			"Reading and writing this resource requires the token's user to be a **workspace admin** — stricter " +
			"than the other project-scoped resources, because linking spends the workspace's GitHub App credential " +
			"and aims the self-healing rollback loop. Import by numeric id: " +
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
				MarkdownDescription: "Whether deliveries from the repository are processed. The API always creates an " +
					"integration enabled; `enabled = false` on a new resource is applied by an immediate follow-up " +
					"update. When unset, the current value is kept.",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"secret": schema.StringAttribute{
				MarkdownDescription: "Webhook signing secret for the caller-managed mode; at least 16 characters. Omit it " +
					"(or pass an empty string) to let Flightdeck generate one and register the webhook itself. " +
					"Write-only: sent on create, never read back. Changing it replaces the integration.",
				Optional:      true,
				Sensitive:     true,
				Validators:    []validator.String{blankOrMinLength(16)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"webhook_registered": schema.BoolAttribute{
				MarkdownDescription: "`true` when Flightdeck registered the repository webhook itself and will remove it on " +
					"unlink. `false` when you supplied `secret`, and also in the managed mode when a webhook targeting " +
					"Flightdeck already existed on the repository (Flightdeck leaves it alone and does not claim it).",
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
	// The API ignores `enabled` on create (a new integration is always
	// enabled); a planned false is applied with a follow-up update below.
	fields := client.Fields{"repo_full_name": plan.RepoFullName.ValueString()}
	// A blank secret is "omitted" (managed mode), as the API reads it.
	if !plan.Secret.IsNull() && !plan.Secret.IsUnknown() && strings.TrimSpace(plan.Secret.ValueString()) != "" {
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
		case client.IsForbidden(err):
			resp.Diagnostics.AddError("Linking a GitHub repository requires a workspace admin",
				"Only a workspace owner or admin may manage a project's GitHub integration. "+apiMessage(err))
		default:
			addAPIError(&resp.Diagnostics, "Error linking GitHub repository", err)
		}
		return
	}
	if !plan.Enabled.IsNull() && !plan.Enabled.IsUnknown() && !plan.Enabled.ValueBool() {
		disabled, err := r.client.UpdateGithubIntegration(ctx, created.ID, client.Fields{"enabled": false}, created.LockVersion)
		if err != nil {
			// The link exists; record it so the next apply reconciles rather
			// than linking twice, and report why it is still enabled.
			state := githubIntegrationToModel(created, plan.Secret)
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			addAPIError(&resp.Diagnostics, "Error disabling the new Flightdeck GitHub integration", err)
			return
		}
		created = disabled
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
		case client.IsValidation(err):
			resp.Diagnostics.AddAttributeError(pathRoot("enabled"), "Cannot enable this integration", apiMessage(err))
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

// blankOrMinLength accepts null or an empty string (which the API treats as
// "omitted") and otherwise requires at least n characters.
type blankOrMinLength int

func (v blankOrMinLength) Description(context.Context) string {
	return fmt.Sprintf("must be empty or at least %d characters", int(v))
}

func (v blankOrMinLength) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (v blankOrMinLength) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	s := req.ConfigValue.ValueString()
	if strings.TrimSpace(s) == "" || len(s) >= int(v) {
		return
	}
	resp.Diagnostics.AddAttributeError(req.Path, "Secret too short",
		fmt.Sprintf("must be at least %d characters (omit it, or pass an empty string, to have Flightdeck generate one and register the webhook itself); got %d.", int(v), len(s)))
}
