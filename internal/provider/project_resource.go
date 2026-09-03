package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
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
	_ resource.Resource                   = &projectResource{}
	_ resource.ResourceWithConfigure      = &projectResource{}
	_ resource.ResourceWithImportState    = &projectResource{}
	_ resource.ResourceWithValidateConfig = &projectResource{}
)

// NewProjectResource returns the flightdeck_project resource.
func NewProjectResource() resource.Resource { return &projectResource{} }

type projectResource struct {
	client *client.Client
}

func (r *projectResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_project"
}

func (r *projectResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *projectResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Flightdeck project — the container for an application's work items, states, " +
			"labels, members and integrations.\n\n" +
			"Creating a project seeds it the way the web form does (default workflow states, Task/Epic work item types, " +
			"starter labels) and makes the token's user its admin. Deleting a project marks it for deletion and tears it " +
			"down asynchronously; it disappears from the API immediately.\n\n" +
			"Updates carry the project's `lock_version` as an `If-Match` precondition. If the project was changed " +
			"elsewhere since the last plan, the apply fails without overwriting anything; re-run `terraform plan`.\n\n" +
			"A project can be imported by numeric id or by identifier: `terraform import flightdeck_project.app APP`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				MarkdownDescription: "Numeric id of the project.",
				Computed:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Display name.",
				Required:            true,
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"identifier": schema.StringAttribute{
				MarkdownDescription: "Short key that prefixes work item keys (`APP-123`). 1–10 uppercase letters or digits, " +
					"starting with a letter; unique within the workspace.",
				Required: true,
				Validators: []validator.String{
					stringvalidator.RegexMatches(identifierPattern, "must be 1-10 uppercase letters/digits starting with a letter"),
				},
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "Free-text description. Removing it from configuration clears it.",
				Optional:            true,
			},
			"emoji": schema.StringAttribute{
				MarkdownDescription: "Emoji shown next to the project name. Defaults to the server's default (📁).",
				Optional:            true,
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"archived": schema.BoolAttribute{
				MarkdownDescription: "Whether the project is archived. New projects are not archived. When unset, the " +
					"project's current value is kept (so importing an archived project does not unarchive it).",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"features": schema.MapAttribute{
				MarkdownDescription: "Feature toggles to manage, as a map of feature key to boolean. Only the keys listed here " +
					"are managed; keys you leave out keep whatever value the project has. Settable keys: `" +
					strings.Join(toggleableFeatures, "`, `") + "`. (`self_healing` and `slack` are reported by the " +
					"`flightdeck_project` data source but cannot be set through the API.)",
				ElementType: types.BoolType,
				Optional:    true,
				Validators: []validator.Map{
					mapvalidator.KeysAre(stringvalidator.OneOf(toggleableFeatures...)),
				},
			},
			"lead_id": schema.Int64Attribute{
				MarkdownDescription: "User id of the project lead; must be a workspace member. Defaults to the token's user " +
					"on create. When unset, the current lead is kept.",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"github_repo_full_name": schema.StringAttribute{
				MarkdownDescription: "GitHub repository this project is linked to, as `owner/repo`. **Read-only**: linking " +
					"and unlinking need the webhook-secret round-trip the project's Settings → Integrations page " +
					"performs, so the API refuses writes to this field.",
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"network": schema.StringAttribute{
				MarkdownDescription: "Project visibility: `public_project` (every workspace member can see it) or " +
					"`private_project` (explicit members only). New projects are public. When unset, the current " +
					"value is kept. Making a project private also gives the token's user and the project lead admin " +
					"memberships so nobody is locked out; members who lose implicit access are not notified.",
				Optional:      true,
				Computed:      true,
				Validators:    []validator.String{stringvalidator.OneOf(client.ProjectNetworks...)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"lock_version": schema.Int64Attribute{
				MarkdownDescription: "Optimistic-locking version the API bumps on every change (including self-healing writes). " +
					"Sent as `If-Match` on updates.",
				Computed: true,
			},
			"self_healing": selfHealingSchema(),
		},
	}
}

func (r *projectResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg projectModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	validateSelfHealingConfig(ctx, cfg.SelfHealing, &resp.Diagnostics)
}

func (r *projectResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan, config projectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	fields := projectFields(ctx, &plan, nil, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	created, err := r.client.CreateProject(ctx, fields, client.PayloadKey("project", "", fields))
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error creating Flightdeck project", err)
		return
	}

	state := projectToModel(ctx, created, &plan, featuresFromPrior, &resp.Diagnostics)
	reconcileFeatures(ctx, &state, &plan, &resp.Diagnostics)
	block, lockVersion := writeSelfHealing(ctx, r.client, created.ID, config.SelfHealing, created.LockVersion, &resp.Diagnostics)
	state.SelfHealing = block
	state.LockVersion = types.Int64Value(lockVersion)
	// The project is created even if the self-healing write failed; record it so
	// the next apply reconciles rather than creating a duplicate.
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *projectResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state projectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	p, err := r.client.GetProject(ctx, state.ID.ValueInt64())
	if err != nil {
		if client.IsNotFound(err) {
			// Deleted (or mid-teardown, or no longer visible to this token):
			// gone as far as Terraform is concerned.
			resp.State.RemoveResource(ctx)
			return
		}
		addAPIError(&resp.Diagnostics, "Error reading Flightdeck project", err)
		return
	}

	newState := projectToModel(ctx, p, &state, featuresFromPrior, &resp.Diagnostics)
	newState.SelfHealing = readSelfHealing(ctx, r.client, p.ID, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *projectResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state, config projectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	fields := projectFields(ctx, &plan, &state, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	id := state.ID.ValueInt64()
	updated, err := r.client.UpdateProject(ctx, id, fields, state.LockVersion.ValueInt64())
	if err != nil {
		if client.IsStale(err) {
			var current *int64
			if fresh, rerr := r.client.GetProject(ctx, id); rerr == nil {
				current = &fresh.LockVersion
			}
			addStaleError(&resp.Diagnostics, fmt.Sprintf("Project %s", state.Identifier.ValueString()), state.LockVersion.ValueInt64(), current, err)
			return
		}
		addAPIError(&resp.Diagnostics, "Error updating Flightdeck project", err)
		return
	}

	newState := projectToModel(ctx, updated, &plan, featuresFromPrior, &resp.Diagnostics)
	reconcileFeatures(ctx, &newState, &plan, &resp.Diagnostics)
	// The self-healing write pins the lock_version the project PATCH just
	// produced and bumps it again; the state keeps the final value.
	block, lockVersion := writeSelfHealing(ctx, r.client, id, config.SelfHealing, updated.LockVersion, &resp.Diagnostics)
	newState.SelfHealing = block
	newState.LockVersion = types.Int64Value(lockVersion)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *projectResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state projectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteProject(ctx, state.ID.ValueInt64()); err != nil {
		addAPIError(&resp.Diagnostics, "Error deleting Flightdeck project", err)
	}
}

// ImportState accepts a numeric id or a project identifier. Every settable
// feature key is imported so the imported state shows the project's full
// toggle set; trim `features` in configuration to the keys you want managed.
func (r *projectResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importID := strings.TrimSpace(req.ID)
	var (
		p   *client.Project
		err error
	)
	switch {
	case importID == "":
		resp.Diagnostics.AddError("Invalid import id", "Expected a project id (e.g. 42) or identifier (e.g. APP).")
		return
	case isDigits(importID):
		id, _ := strconv.ParseInt(importID, 10, 64)
		p, err = r.client.GetProject(ctx, id)
	case identifierPattern.MatchString(strings.ToUpper(importID)):
		p, err = r.client.FindProjectByIdentifier(ctx, strings.ToUpper(importID))
	default:
		resp.Diagnostics.AddError("Invalid import id",
			fmt.Sprintf("%q is neither a numeric project id nor a project identifier (1-10 uppercase letters/digits).", importID))
		return
	}
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error importing Flightdeck project", err)
		return
	}

	state := projectToModel(ctx, p, nil, featuresToggleable, &resp.Diagnostics)
	state.SelfHealing = readSelfHealing(ctx, r.client, p.ID, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
