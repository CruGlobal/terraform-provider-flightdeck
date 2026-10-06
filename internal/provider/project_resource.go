package provider

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
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
	_ resource.ResourceWithModifyPlan     = &projectResource{}
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
					strings.Join(toggleableFeatures, "`, `") + "`. `modules` is the old name of `epics`: it still works, " +
					"with a deprecation warning, and is sent as `epics`; rename it. `self_healing` and `slack` are reported by the " +
					"`flightdeck_project` data source and refused here, because each is settable on its own endpoint: " +
					"self-healing through the `self_healing` block, and `slack` — the Slack notifications master switch — " +
					"as `slack_channel.notifications_enabled`.",
				ElementType: types.BoolType,
				Optional:    true,
				Validators:  []validator.Map{featureKeys{}},
			},
			"lead_id": schema.Int64Attribute{
				MarkdownDescription: "User id of the project lead; must be a workspace member, and a " +
					"`flightdeck_workspace_member` data source resolves one from an email address. Defaults to the " +
					"token's user on create. When unset, the current lead is kept.",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"github_repo_full_name": schema.StringAttribute{
				MarkdownDescription: "GitHub repository this project is linked to, as `owner/repo`. **Read-only**: the API " +
					"refuses writes to this field. Manage the link with a `flightdeck_github_integration` resource, which " +
					"sets it on link and clears it on unlink.",
				Optional:      true,
				Computed:      true,
				Validators:    []validator.String{readOnlyAttribute("manage the repository link with a flightdeck_github_integration resource instead")},
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
			"app": schema.StringAttribute{
				MarkdownDescription: "The deployed app this project belongs to, as the deploy pipeline's release events " +
					"name it: the app's GitHub repository name, 1 to 100 letters, digits, `.`, `_` or `-`. One project is " +
					"one app, and within a workspace an app belongs to at most one project (compared ignoring case), so an " +
					"app another project already has fails the apply (`app_taken`). Setting or changing it needs a " +
					"**workspace owner or admin** token, even where the token could otherwise update the project. When " +
					"unset, the project's current app is kept: the first release event that names an app binds the " +
					"project to it, so leaving this unset leaves the binding to the pipeline. The API can set or change " +
					"`app` but never clear it (a workspace admin can, in Flightdeck's project settings), so removing it from " +
					"configuration does not unbind the project. That first " +
					"binding bumps the project's `lock_version`, so an apply racing it fails once with a lock conflict; " +
					"run `terraform plan` again and re-apply.",
				Optional:      true,
				Computed:      true,
				Validators:    []validator.String{stringvalidator.RegexMatches(appNamePattern, "must be 1-100 letters, digits, '.', '_' or '-'")},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"lock_version": schema.Int64Attribute{
				MarkdownDescription: "Optimistic-locking version the API bumps on every change (including self-healing and " +
					"Slack channel writes, and the deploy pipeline binding the project's `app`). " +
					"Sent as `If-Match` on updates. Agent work settings have their own, `agent_work.lock_version`.",
				Computed: true,
			},
			"self_healing":  selfHealingSchema(),
			"slack_channel": slackChannelSchema(),
			"agent_work":    agentWorkSchema(),
		},
	}
}

// ModifyPlan keeps the deprecated self_healing.armed in step with the planned
// mode and agent_work's computed attributes in step with its settings, and
// warns about what ValidateConfig cannot see: an apply that turns
// auto-rollback or agent work on, which needs the prior value, and the
// self-healing burn-rate windows and agent work limits as the API will MERGE
// them, which needs the prior values the plan carries for settings the
// configuration does not mention.
func (r *projectResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	// Nothing to warn about while destroying (no plan).
	if req.Plan.Raw.IsNull() {
		return
	}
	var config, plan projectModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	creating := req.State.Raw.IsNull()
	prior := types.ObjectNull(selfHealingAttrTypes)
	priorAgentWork := types.ObjectNull(agentWorkAttrTypes)
	if !creating {
		var state projectModel
		resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
		if resp.Diagnostics.HasError() {
			return
		}
		prior = state.SelfHealing
		priorAgentWork = state.AgentWork
	}
	if planned := planArmed(ctx, plan.SelfHealing, &resp.Diagnostics); !planned.Equal(plan.SelfHealing) {
		plan.SelfHealing = planned
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("self_healing"), planned)...)
	}
	warnAutoRollback(ctx, plan.Identifier, config.SelfHealing, prior, plan.SelfHealing, &resp.Diagnostics)
	if planned := planAgentWorkComputed(ctx, priorAgentWork, plan.AgentWork, &resp.Diagnostics); !planned.Equal(plan.AgentWork) {
		plan.AgentWork = planned
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("agent_work"), planned)...)
	}
	warnAgentWorkEnabled(ctx, plan.Identifier, config.AgentWork, priorAgentWork, plan.AgentWork, &resp.Diagnostics)
	// Nothing merged to check while creating (no prior).
	if !creating {
		warnSelfHealingWindows(ctx, config.SelfHealing, plan.SelfHealing, &resp.Diagnostics)
		warnAgentWorkBudget(ctx, config.AgentWork, plan.AgentWork, &resp.Diagnostics)
	}
}

func (r *projectResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg projectModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	validateSelfHealingConfig(ctx, cfg.SelfHealing, &resp.Diagnostics)
	validateAgentWorkConfig(ctx, cfg.AgentWork, &resp.Diagnostics)
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
		// Creating a project needs only workspace membership, so a 403 on a
		// create that names an app is the app's own bar.
		_, settingApp := fields["app"]
		addProjectWriteError(&resp.Diagnostics, "Error creating Flightdeck project", settingApp, err)
		return
	}

	state := projectToModel(ctx, created, &plan, featuresFromPrior, &resp.Diagnostics)
	reconcileFeatures(ctx, &state, &plan, &resp.Diagnostics)
	block, lockVersion := writeSelfHealing(ctx, r.client, created.ID, created.Identifier, config.SelfHealing, created.LockVersion, &resp.Diagnostics)
	state.SelfHealing = block
	slack, lockVersion := writeSlackChannel(ctx, r.client, created.ID, config.SlackChannel, plan.SlackChannel,
		types.ObjectNull(slackChannelAttrTypes), lockVersion, slackChannelOnCreate, &resp.Diagnostics)
	state.SlackChannel = slack
	state.LockVersion = types.Int64Value(lockVersion)
	// Agent work has its own lock_version, so it leaves the project's alone.
	state.AgentWork = writeAgentWork(ctx, r.client, created.ID, created.Identifier, config.AgentWork,
		types.ObjectNull(agentWorkAttrTypes), plan.AgentWork, agentWorkOnCreate, &resp.Diagnostics)
	// The project is created even if a block's write failed; record it so the
	// next apply reconciles rather than creating a duplicate.
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
			// gone as far as Terraform is concerned, with a warning, since a
			// 404 can't say which.
			removeGoneProject(ctx, resp, state.ID.ValueInt64(), state.Identifier.ValueString())
			return
		}
		addAPIError(&resp.Diagnostics, "Error reading Flightdeck project", err)
		return
	}

	newState := projectToModel(ctx, p, &state, featuresFromPrior, &resp.Diagnostics)
	block, sh := readSelfHealing(ctx, r.client, p.ID, &resp.Diagnostics)
	newState.SelfHealing = block
	warnRollbackBlockers(sh, p.Identifier, &resp.Diagnostics)
	newState.SlackChannel = readSlackChannel(ctx, r.client, p.ID,
		slackEventFilterOf(ctx, state.SlackChannel, &resp.Diagnostics), slackEventsManaged, &resp.Diagnostics)
	agentWork, aw := readAgentWork(ctx, r.client, p.ID, &resp.Diagnostics)
	newState.AgentWork = agentWork
	warnAgentWorkBlockers(aw, p.Identifier, &resp.Diagnostics)
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
		// A delete wins over an update sent at the same moment: the update
		// lands first and goes with the project, or gets a 404, or (already
		// part way through) a 409 whose re-read then 404s.
		if client.IsStale(err) {
			var current *int64
			fresh, rerr := r.client.GetProject(ctx, id)
			switch {
			case rerr == nil:
				current = &fresh.LockVersion
			case client.IsNotFound(rerr):
				addProjectGoneError(&resp.Diagnostics, state.Identifier.ValueString(), err)
				return
			}
			addStaleError(&resp.Diagnostics, fmt.Sprintf("Project %s", state.Identifier.ValueString()), state.LockVersion.ValueInt64(), current, err)
			return
		}
		// A 404 can also be an id the write names (a lead_id outside the
		// workspace), so the project is gone only if it no longer reads back.
		if client.IsNotFound(err) {
			if _, rerr := r.client.GetProject(ctx, id); client.IsNotFound(rerr) {
				addProjectGoneError(&resp.Diagnostics, state.Identifier.ValueString(), err)
				return
			}
		}
		// The app is only sent when it changes, so a 403 on a write naming it
		// is the app's own bar.
		_, changingApp := fields["app"]
		addProjectWriteError(&resp.Diagnostics, "Error updating Flightdeck project", changingApp, err)
		return
	}

	newState := projectToModel(ctx, updated, &plan, featuresFromPrior, &resp.Diagnostics)
	reconcileFeatures(ctx, &newState, &plan, &resp.Diagnostics)
	// Each block's write pins the lock_version the previous call produced and
	// bumps it again; the state keeps the final value.
	block, lockVersion := writeSelfHealing(ctx, r.client, id, updated.Identifier, config.SelfHealing, updated.LockVersion, &resp.Diagnostics)
	newState.SelfHealing = block
	slack, lockVersion := writeSlackChannel(ctx, r.client, id, config.SlackChannel, plan.SlackChannel,
		state.SlackChannel, lockVersion, slackChannelOnUpdate, &resp.Diagnostics)
	newState.SlackChannel = slack
	newState.LockVersion = types.Int64Value(lockVersion)
	// Agent work pins its own lock_version, from the block in state.
	newState.AgentWork = writeAgentWork(ctx, r.client, id, updated.Identifier, config.AgentWork,
		state.AgentWork, plan.AgentWork, agentWorkOnUpdate, &resp.Diagnostics)
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
	// Import is followed by a refresh, which reports any rollback blockers.
	state.SelfHealing, _ = readSelfHealing(ctx, r.client, p.ID, &resp.Diagnostics)
	// No prior configuration to defer to, so no event categories are managed;
	// list the ones you want in `slack_channel.event_filter` afterwards.
	state.SlackChannel = readSlackChannel(ctx, r.client, p.ID, types.MapNull(types.BoolType), slackEventsManaged, &resp.Diagnostics)
	// Like the rollback blockers, agent work blockers come with the refresh.
	state.AgentWork, _ = readAgentWork(ctx, r.client, p.ID, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// addProjectGoneError reports a write to a project that is no longer there
// for this token: deleted (perhaps at the same moment), or no longer visible
// to it. The write failed, and the next refresh drops it from state.
func addProjectGoneError(diags *diag.Diagnostics, identifier string, err error) {
	diags.AddError(fmt.Sprintf("Project %s is gone", identifier),
		fmt.Sprintf("Project %s was deleted while this apply was writing to it, or this token can no longer see it. "+
			"Run `terraform plan` again: the refresh removes it from state, and the plan offers to create it again if "+
			"it is still in the configuration.\n\nThe API said: %s", identifier, apiMessage(err)))
}

// addIfProjectGone reports a 404 or 409 from one of a project's own settings
// endpoints as the project being gone, when that is what it was: the project
// itself no longer reads back. A delete wins over a write sent at the same
// moment, so the project write can land and the next one find nothing. It
// reports whether it added the error.
func addIfProjectGone(ctx context.Context, c *client.Client, projectID int64, identifier string, err error, diags *diag.Diagnostics) bool {
	if !client.IsNotFound(err) && !client.IsStale(err) {
		return false
	}
	if _, rerr := c.GetProject(ctx, projectID); !client.IsNotFound(rerr) {
		return false
	}
	if identifier == "" {
		identifier = strconv.FormatInt(projectID, 10)
	}
	addProjectGoneError(diags, identifier, err)
	return true
}

// addProjectWriteError reports a failed project create or update. The app has
// two refusals of its own, and each says what to change rather than only
// quoting the API. changingApp is whether the write sets or changes the app,
// which is what decides whether a 403 is the app's workspace-admin bar or the
// token lacking the project role every other update needs.
func addProjectWriteError(diags *diag.Diagnostics, summary string, changingApp bool, err error) {
	apiErr, _ := client.AsError(err)
	switch {
	case client.HasCode(err, client.CodeAppTaken):
		diags.AddAttributeError(path.Root("app"), "App already belongs to another project",
			"An app belongs to at most one project in a workspace. Point this project at a different app, or have a "+
				"workspace owner or admin change the other project's app first. Nothing was written.\n\n"+
				"The API said: "+apiErr.Error())
	// Only the write itself: a 403 from the read that verifies a create is the
	// project role, and says nothing about the app.
	case changingApp && client.IsForbidden(err) && apiErr.Method != http.MethodGet:
		diags.AddAttributeError(path.Root("app"), "Setting a project's app requires a workspace owner or admin",
			"This write sets or changes `app`, which needs a workspace owner or admin token even where the token "+
				"could otherwise update the project. Use such a token, or remove `app` from the configuration and let "+
				"the first release event that names an app bind it.\n\nThe API said: "+apiErr.Error())
	default:
		addAPIError(diags, summary, err)
	}
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
