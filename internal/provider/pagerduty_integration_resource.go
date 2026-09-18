package provider

import (
	"context"
	"fmt"
	"regexp"
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

// The PagerDuty link holds a credential Terraform can never read back, which
// is the whole design problem. Flightdeck stores the routing key encrypted —
// it has to replay it to PagerDuty, so it cannot be a one-way digest — but it
// does not return it on any route. Reads report `routing_key_last_four` and a
// masked form, and that is all there is to compare against.
//
// Four characters is not a fingerprint. Two different keys share a last-four
// often enough that drift detection built on it alone would be quietly wrong,
// so it is used only to TRIGGER a write, never to suppress one: a false
// positive costs a harmless re-assert (the API treats a PATCH with the stored
// value as a true no-op and does not even bump lock_version), while a false
// negative is covered by `routing_key_version`.
//
// Hence the shape:
//
//   - `routing_key` is a WRITE-ONLY argument (Terraform >= 1.11). Terraform
//     never puts it in the plan or the state file. The alternative — a
//     sensitive attribute — would persist a third-party paging credential in
//     every state file and every state backup, which is a worse trade than the
//     ergonomics it buys.
//   - Because a write-only value is null in the plan, changing it cannot by
//     itself produce a diff. `routing_key_version` is the companion trigger an
//     operator bumps to force a rotation.
//   - ModifyPlan additionally forces an update when the configured key's last
//     four characters differ from the stored ones, which is what makes a
//     console-side rotation self-correcting without anyone bumping anything.
//   - Update re-sends the key on every write, so any apply that touches this
//     resource for any reason also restores the configured credential.

var (
	_ resource.Resource                = &pagerDutyIntegrationResource{}
	_ resource.ResourceWithConfigure   = &pagerDutyIntegrationResource{}
	_ resource.ResourceWithImportState = &pagerDutyIntegrationResource{}
	_ resource.ResourceWithModifyPlan  = &pagerDutyIntegrationResource{}
)

// NewPagerDutyIntegrationResource returns the flightdeck_pagerduty_integration resource.
func NewPagerDutyIntegrationResource() resource.Resource { return &pagerDutyIntegrationResource{} }

type pagerDutyIntegrationResource struct {
	client *client.Client
}

type pagerDutyIntegrationModel struct {
	ProjectID          types.Int64  `tfsdk:"project_id"`
	RoutingKey         types.String `tfsdk:"routing_key"`
	RoutingKeyVersion  types.String `tfsdk:"routing_key_version"`
	RoutingKeyLastFour types.String `tfsdk:"routing_key_last_four"`
	RoutingKeyMasked   types.String `tfsdk:"routing_key_masked"`
	Enabled            types.Bool   `tfsdk:"enabled"`
	MinSeverity        types.String `tfsdk:"min_severity"`
	ServiceID          types.String `tfsdk:"service_id"`
	ServiceURL         types.String `tfsdk:"service_url"`
	LockVersion        types.Int64  `tfsdk:"lock_version"`
}

// pagerDutyToModel maps an API link. routing_key is write-only and never
// appears in state, so it is always null here; version is carried from the
// plan by the caller.
func pagerDutyToModel(pd *client.PagerDuty, version types.String) pagerDutyIntegrationModel {
	return pagerDutyIntegrationModel{
		ProjectID:          types.Int64Value(pd.ProjectID),
		RoutingKey:         types.StringNull(),
		RoutingKeyVersion:  version,
		RoutingKeyLastFour: types.StringValue(pd.RoutingKeyLastFour),
		RoutingKeyMasked:   types.StringValue(pd.RoutingKeyMasked),
		Enabled:            types.BoolValue(pd.Enabled),
		MinSeverity:        types.StringValue(pd.MinSeverity),
		ServiceID:          stringPointerValue(pd.ServiceID),
		ServiceURL:         stringPointerValue(pd.ServiceURL),
		LockVersion:        types.Int64Value(pd.LockVersion),
	}
}

func (r *pagerDutyIntegrationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_pagerduty_integration"
}

func (r *pagerDutyIntegrationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *pagerDutyIntegrationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Links a Flightdeck project to a PagerDuty service, so a signal that needs a human is " +
			"forwarded to PagerDuty's Events API v2 and paged through PagerDuty's own on-call. There is **one link " +
			"per project**; the project is its identity, and it has no id of its own.\n\n" +
			"## The credential\n\n" +
			"`routing_key` is a [write-only argument](https://developer.hashicorp.com/terraform/language/resources/ephemeral#write-only-arguments): " +
			"Terraform sends it on apply and never writes it to the plan or the state file. That requires " +
			"**Terraform >= 1.11** — below the provider's supported floor, so it costs nothing — and it means the " +
			"value must come from somewhere Terraform does not persist either, such as an ephemeral resource or a " +
			"variable you keep out of state.\n\n" +
			"Flightdeck stores the key encrypted (it has to replay it to PagerDuty) but never returns it. Reads " +
			"report only `routing_key_last_four` and `routing_key_masked`. Two consequences follow, and both are " +
			"worth reading before you rely on this:\n\n" +
			"- **Rotating the key.** Change the value in your configuration. Terraform cannot see that a write-only " +
			"value changed, so on its own that plans nothing; the provider compares the configured key's last four " +
			"characters against the stored ones and plans an update when they differ. If the new key happens to end " +
			"in the same four characters, bump `routing_key_version` to force the write.\n" +
			"- **Somebody changes the key in the PagerDuty settings page instead.** The next refresh sees a different " +
			"`routing_key_last_four`, the provider plans an update, and the apply puts the configured key back. " +
			"Terraform wins, which is the point of declaring it here. If the console key shares its last four " +
			"characters with the configured one the drift is invisible, so bump `routing_key_version` if you suspect " +
			"one. Every update re-sends the key regardless, so any apply that touches this resource restores it.\n\n" +
			"Deleting this resource removes Flightdeck's copy of the credential and stops the forwarding. It does " +
			"**not** invalidate the key on PagerDuty's side — regenerate the integration key on the PagerDuty " +
			"service to do that.\n\n" +
			"Import by project id: `terraform import flightdeck_pagerduty_integration.app 42`. The key cannot be " +
			"imported, so add it to configuration and apply.",
		Attributes: map[string]schema.Attribute{
			"project_id": schema.Int64Attribute{
				MarkdownDescription: "Id of the project whose signals are forwarded. It identifies the link; changing it replaces the resource.",
				Required:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"routing_key": schema.StringAttribute{
				MarkdownDescription: "PagerDuty Events API v2 integration key for the service this project pages through " +
					"(PagerDuty: *Service → Integrations → Events API v2*).\n\n" +
					"**Write-only**: sent on apply, never stored in state or shown in a plan. Flightdeck accepts any " +
					"non-blank string without whitespace — it deliberately does not check the key's shape, so a typo " +
					"is accepted here and only fails when PagerDuty rejects the event. It cannot be set to null; " +
					"delete the resource to remove the integration.",
				Required:   true,
				WriteOnly:  true,
				Sensitive:  true,
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"routing_key_version": schema.StringAttribute{
				MarkdownDescription: "An arbitrary value you change to force the routing key to be re-sent. Needed only " +
					"when a rotation is invisible to the last-four comparison — a new key ending in the same four " +
					"characters as the old one. Any value works; a date or an incrementing number is usual.",
				Optional: true,
			},
			"routing_key_last_four": schema.StringAttribute{
				MarkdownDescription: "Last four characters of the stored key. The only thing the API reports about the " +
					"credential, and what the provider compares to notice that it changed somewhere else.",
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"routing_key_masked": schema.StringAttribute{
				MarkdownDescription: "The stored key as the UI shows it: an ellipsis and the last four characters.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether signals are forwarded. A new link is enabled. `false` keeps the stored " +
					"credential and forwards nothing. When unset, the current value is kept — including one changed " +
					"in the console — so set it explicitly for Terraform to own it.",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"min_severity": schema.StringAttribute{
				MarkdownDescription: "Forward only signals at or above this severity: one of `" +
					strings.Join(client.PagerDutyMinSeverities, "`, `") + "`. Defaults to `error` on a new link. " +
					"When unset, the current value is kept, so set it explicitly for Terraform to own it.",
				Optional:      true,
				Computed:      true,
				Validators:    []validator.String{stringvalidator.OneOf(client.PagerDutyMinSeverities...)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"service_id": schema.StringAttribute{
				MarkdownDescription: "PagerDuty service id, recorded for display only — Flightdeck does not call " +
					"PagerDuty's REST API with it. Removing it from configuration clears it; it cannot be set to an " +
					"empty or space-padded value, because the API stores those normalised and the stored value has " +
					"to match what you wrote.",
				Optional:   true,
				Validators: []validator.String{stringvalidator.RegexMatches(unpaddedPattern, "must not be empty or have leading or trailing whitespace")},
			},
			"service_url": schema.StringAttribute{
				MarkdownDescription: "Link to the PagerDuty service, recorded for display only. Must be an http(s) URL. " +
					"Removing it from configuration clears it.",
				Optional: true,
				Validators: []validator.String{
					stringvalidator.RegexMatches(httpURLPattern, "must be an http(s) URL"),
				},
			},
			"lock_version": schema.Int64Attribute{
				MarkdownDescription: "Optimistic-locking version the API bumps on every change. This is the link's own " +
					"version, not the project's. Sent as `If-Match` on updates and deletes.",
				Computed: true,
			},
		},
	}
}

// unpaddedPattern matches a non-empty string with no leading or trailing
// whitespace — the form the API stores a display label in. An attribute that
// is Optional without being Computed has to come back exactly as configured,
// so a value the API would normalise has to be refused at plan instead.
var unpaddedPattern = regexp.MustCompile(`^\S(.*\S)?$`)

// lastFour returns the trailing four characters of a key, the only part of it
// the API ever reports back.
func lastFour(s string) string {
	runes := []rune(s)
	if len(runes) <= 4 {
		return s
	}
	return string(runes[len(runes)-4:])
}

// ModifyPlan forces an update when the configured key's last four characters
// differ from the stored ones. Without it, a key rotated in the PagerDuty
// settings page would sit there indefinitely: a write-only argument is null in
// the plan, so nothing else in this resource would ever show a difference.
//
// This can only ever cause an extra write, never suppress one — re-sending the
// stored value is a no-op at the API, and a rotation the comparison misses is
// what routing_key_version is for.
func (r *pagerDutyIntegrationResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	// Nothing to compare while creating (no state) or destroying (no plan).
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var config, state, plan pagerDutyIntegrationModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if config.RoutingKey.IsNull() || config.RoutingKey.IsUnknown() || state.RoutingKeyLastFour.IsNull() {
		return
	}
	if lastFour(strings.TrimSpace(config.RoutingKey.ValueString())) == state.RoutingKeyLastFour.ValueString() {
		return
	}
	// Marking what the write will change as unknown is what turns this into a
	// planned update rather than a silent no-op. lock_version has to go with
	// them: the write bumps it, and a computed attribute left known at its
	// prior value would make the apply an inconsistent result.
	plan.RoutingKeyLastFour = types.StringUnknown()
	plan.RoutingKeyMasked = types.StringUnknown()
	plan.LockVersion = types.Int64Unknown()
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

// pagerDutyFields builds the write body. service_id and service_url are always
// sent, as a value or as null, so removing them from configuration clears them
// on a merging endpoint; enabled and min_severity are sent only when
// configured, so an unset one keeps whatever the link has.
func pagerDutyFields(plan *pagerDutyIntegrationModel, routingKey types.String) client.Fields {
	fields := client.Fields{}
	if !routingKey.IsNull() && !routingKey.IsUnknown() {
		fields["routing_key"] = strings.TrimSpace(routingKey.ValueString())
	}
	if !plan.Enabled.IsNull() && !plan.Enabled.IsUnknown() {
		fields["enabled"] = plan.Enabled.ValueBool()
	}
	if !plan.MinSeverity.IsNull() && !plan.MinSeverity.IsUnknown() {
		fields["min_severity"] = plan.MinSeverity.ValueString()
	}
	if !plan.ServiceID.IsUnknown() {
		fields["service_id"] = plan.ServiceID.ValueStringPointer()
	}
	if !plan.ServiceURL.IsUnknown() {
		fields["service_url"] = plan.ServiceURL.ValueStringPointer()
	}
	return fields
}

func (r *pagerDutyIntegrationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan, config pagerDutyIntegrationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	// A write-only argument is nulled in the plan; the configuration is the
	// only place its value can be read.
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID := plan.ProjectID.ValueInt64()
	fields := pagerDutyFields(&plan, config.RoutingKey)
	created, err := r.client.CreatePagerDuty(ctx, projectID, fields, client.PayloadKey("pagerduty", strconv.FormatInt(projectID, 10), fields))
	if err != nil {
		// Two spellings, depending on which guard catches it: the controller
		// answers pagerduty_already_configured, while two creates racing in
		// one apply fall through to the model's uniqueness validation.
		if client.HasCode(err, client.CodePagerDutyAlreadyConfigured) ||
			(client.IsValidation(err) && strings.Contains(apiMessage(err), "already has a PagerDuty integration")) {
			resp.Diagnostics.AddError("The project already has a PagerDuty link",
				apiMessage(err)+fmt.Sprintf("\n\nImport it instead: terraform import flightdeck_pagerduty_integration.<name> %d", projectID))
			return
		}
		addAPIError(&resp.Diagnostics, "Error linking PagerDuty", err)
		return
	}
	state := pagerDutyToModel(created, plan.RoutingKeyVersion)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *pagerDutyIntegrationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state pagerDutyIntegrationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	pd, err := r.client.GetPagerDuty(ctx, state.ProjectID.ValueInt64())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		addAPIError(&resp.Diagnostics, "Error reading Flightdeck PagerDuty link", err)
		return
	}
	newState := pagerDutyToModel(pd, state.RoutingKeyVersion)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Update re-sends the routing key on every write. The API treats a PATCH
// carrying the stored value as a true no-op — it does not even bump
// lock_version — so this costs nothing and means any apply that touches this
// resource also restores a credential changed elsewhere.
func (r *pagerDutyIntegrationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state, config pagerDutyIntegrationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID := state.ProjectID.ValueInt64()
	fields := pagerDutyFields(&plan, config.RoutingKey)
	updated, err := r.client.UpdatePagerDuty(ctx, projectID, fields, state.LockVersion.ValueInt64())
	if err != nil {
		if client.IsStale(err) {
			var current *int64
			if fresh, rerr := r.client.GetPagerDuty(ctx, projectID); rerr == nil {
				current = &fresh.LockVersion
			}
			addStaleError(&resp.Diagnostics, fmt.Sprintf("PagerDuty link for project %d", projectID), state.LockVersion.ValueInt64(), current, err)
			return
		}
		addAPIError(&resp.Diagnostics, "Error updating Flightdeck PagerDuty link", err)
		return
	}
	newState := pagerDutyToModel(updated, plan.RoutingKeyVersion)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *pagerDutyIntegrationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state pagerDutyIntegrationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID := state.ProjectID.ValueInt64()
	err := deleteWithIfMatch(ctx, state.LockVersion.ValueInt64(),
		func(ctx context.Context, lv int64) error { return r.client.DeletePagerDuty(ctx, projectID, lv) },
		func(ctx context.Context) (int64, error) {
			fresh, err := r.client.GetPagerDuty(ctx, projectID)
			if err != nil {
				return 0, err
			}
			return fresh.LockVersion, nil
		})
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error unlinking PagerDuty", err)
	}
}

// ImportState imports by project id: the link is a singleton on the project.
// The credential is not recoverable, so configuration has to supply it.
func (r *pagerDutyIntegrationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	projectID, ok := parseImportID(req.ID, "project", &resp.Diagnostics)
	if !ok {
		return
	}
	pd, err := r.client.GetPagerDuty(ctx, projectID)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error importing Flightdeck PagerDuty link", err)
		return
	}
	state := pagerDutyToModel(pd, types.StringNull())
	resp.Diagnostics.AddWarning("Imported PagerDuty link has no routing key",
		"The API never returns the stored routing key, and `routing_key` is write-only, so it is not in state either. "+
			"Put the key in configuration before the next apply: the provider compares its last four characters "+
			"against the stored ones, so a matching key plans nothing and a different one is written.")
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
