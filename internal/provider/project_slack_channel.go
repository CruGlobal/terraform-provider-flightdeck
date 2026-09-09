package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// The slack_channel block rides its own API resource, GET/PATCH
// /api/v1/projects/:project_id/slack-channel, project-admin for both. It is
// modelled as a block on flightdeck_project because it configures that
// project, but its transport is separate, and the If-Match is the PROJECT's
// lock_version (the columns live on the project row, so a write here bumps
// it) — the same arrangement as self_healing.
//
// Three of the API's rules shape what is modelled here:
//
//   - PROVISIONING IS ASYNCHRONOUS. A write saves the configuration and
//     enqueues a job; the channel is created, linked and invited into
//     afterwards. Everything the job reports (channel_id, provision_status,
//     provision_note, linked, invites_skipped) is therefore Computed and never
//     Optional — the resource converges on configuration, not on the channel
//     existing, and a refresh picks the outcome up whenever it lands.
//   - THE EVENT FILTER MERGES server-side, unlike every other object on this
//     API: only the categories a write names change. So event_filter declares
//     OVERRIDES, not the full set — dropping a category from the map stops
//     managing it at its current value rather than restoring its default.
//   - THE CHANNEL NAME IS NORMALIZED on write ("My Team!" comes back as
//     my-team), so `name` carries a semantic-equality type: a configured
//     spelling survives the round trip instead of diffing forever.
//
// Every attribute here is Optional+Computed or Computed, never Optional
// alone: dropping the block from a configuration nulls an Optional-only
// member, and that difference cascades into "known after apply" on every
// computed attribute of the project. Unset therefore means "keep what the
// project has" throughout, and an override is cleared by writing the value
// that clears it (`name = ""`) rather than by deleting the line.

// slackChannelModel is the Terraform shape of the block.
type slackChannelModel struct {
	Enabled              types.Bool            `tfsdk:"enabled"`
	NotificationsEnabled types.Bool            `tfsdk:"notifications_enabled"`
	Name                 slackChannelNameValue `tfsdk:"name"`
	EventFilter          types.Map             `tfsdk:"event_filter"`
	Available            types.Bool            `tfsdk:"available"`
	ScopesSufficient     types.Bool            `tfsdk:"scopes_sufficient"`
	Linked               types.Bool            `tfsdk:"linked"`
	ChannelID            types.String          `tfsdk:"channel_id"`
	Basename             types.String          `tfsdk:"basename"`
	ProvisionStatus      types.String          `tfsdk:"provision_status"`
	ProvisionNote        types.String          `tfsdk:"provision_note"`
	InvitesSkipped       types.Int64           `tfsdk:"invites_skipped"`
}

var slackChannelAttrTypes = map[string]attr.Type{
	"enabled":               types.BoolType,
	"notifications_enabled": types.BoolType,
	"name":                  slackChannelNameType{},
	"event_filter":          types.MapType{ElemType: types.BoolType},
	"available":             types.BoolType,
	"scopes_sufficient":     types.BoolType,
	"linked":                types.BoolType,
	"channel_id":            types.StringType,
	"basename":              types.StringType,
	"provision_status":      types.StringType,
	"provision_note":        types.StringType,
	"invites_skipped":       types.Int64Type,
}

// slackEventFilterScope says which categories to record in state.
type slackEventFilterScope int

const (
	// slackEventsManaged keeps exactly the categories the configuration lists,
	// so a block that manages a subset never diffs against the rest. No
	// configured categories means a null map.
	slackEventsManaged slackEventFilterScope = iota
	// slackEventsAll keeps every category the API resolves (the data source).
	slackEventsAll
)

// slackChannelSchema is the resource attribute.
func slackChannelSchema() schema.Attribute {
	return schema.SingleNestedAttribute{
		MarkdownDescription: "Per-project Slack channel configuration, managed through the project's `slack-channel` API " +
			"resource. Reading and writing it requires the token's user to be an **admin of this project** (a workspace " +
			"owner or admin qualifies); for other tokens, and on a Flightdeck version without the endpoint, the block is " +
			"null.\n\n" +
			"**Provisioning is asynchronous.** Enabling the channel saves the configuration and enqueues a job that " +
			"creates the channel, links it and invites the project's members; `channel_id`, `linked`, `provision_status` " +
			"and `provision_note` are filled in afterwards and a later refresh picks them up. An apply that ends with " +
			"`provision_status` at `queued` has succeeded — it does not wait for Slack, and the pending values never " +
			"produce a diff.\n\n" +
			"Nothing provisions at all when `available` is false (the workspace has no connected Slack integration) or " +
			"`scopes_sufficient` is false (the connection predates the channel scopes). Both are fixed by re-authorizing " +
			"Slack under Settings → Integrations, which the API cannot do; the provider warns rather than failing, since " +
			"the configuration is still stored.\n\n" +
			"A write here bumps the project's `lock_version`. Attributes map onto the API's keys by dropping the " +
			"`slack_` prefix (`enabled` is `slack_channel_enabled`, `notifications_enabled` is " +
			"`slack_notifications_enabled`, `name` is `slack_channel_name`, `event_filter` is `slack_event_filter`).",
		Optional: true,
		Computed: true,
		PlanModifiers: []planmodifier.Object{
			objectplanmodifier.UseStateForUnknown(),
		},
		Attributes: map[string]schema.Attribute{
			"enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether this project gets its own Slack channel. When unset, the project's current " +
					"value is kept. Turning it on enqueues provisioning; turning it off stops future posts but never " +
					"deletes the channel on Slack.",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"notifications_enabled": schema.BoolAttribute{
				MarkdownDescription: "Master switch for this project's Slack posts (the `slack` key of the project's " +
					"`features`). When false, every project-scoped Slack post is silenced, even with a workspace default " +
					"channel; channel provisioning is independent of it. When unset, the project's current value is kept. " +
					"This is the only place the API accepts the `slack` feature — writing it in `features` is refused.",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Channel name to use instead of the project-name default. Flightdeck normalizes it to " +
					"Slack's rules (lower case, runs of other characters become `-`, at most 80 characters), so `My Team!` " +
					"is stored as `my-team`; the configured spelling is kept in state and does not diff. When unset the " +
					"project's current value is kept, so **set it to `\"\"` to clear an override** and fall back to the " +
					"project-name default — removing the attribute leaves whatever is there. Empty means no override, " +
					"which is what `basename` then reports. Changing the effective name drops the stored `channel_id` so " +
					"the next provision links the new channel; the old one is left on Slack.",
				Optional:      true,
				Computed:      true,
				CustomType:    slackChannelNameType{},
				Validators:    []validator.String{slackChannelNameValidator{}},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"event_filter": schema.MapAttribute{
				MarkdownDescription: "Activity categories to post, as a map of category to boolean. Categories: `" +
					strings.Join(client.SlackEventCategories, "`, `") + "` (defaults: `created`, `state_changed` and " +
					"`assigned` on, `field_changed` and `logged` off). The API MERGES this map rather than replacing it, so " +
					"it declares overrides: only the categories listed here are managed, and removing one leaves it at its " +
					"current value rather than restoring its default — set it explicitly to change it back.",
				ElementType: types.BoolType,
				Optional:    true,
				Computed:    true,
				Validators: []validator.Map{
					mapvalidator.KeysAre(stringvalidator.OneOf(client.SlackEventCategories...)),
				},
				PlanModifiers: []planmodifier.Map{mapplanmodifier.UseStateForUnknown()},
			},
			"available": schema.BoolAttribute{
				MarkdownDescription: "Whether the workspace has a connected Slack integration. False means nothing will provision.",
				Computed:            true,
			},
			"scopes_sufficient": schema.BoolAttribute{
				MarkdownDescription: "Whether the workspace's Slack connection has the channel scopes. False means provisioning " +
					"will fail until an admin re-authorizes Slack.",
				Computed: true,
			},
			"linked": schema.BoolAttribute{
				MarkdownDescription: "Whether a Slack channel is linked to this project (`channel_id` is set).",
				Computed:            true,
			},
			"channel_id": schema.StringAttribute{
				MarkdownDescription: "Slack's id for the linked channel, set by the provision job. Null until it lands.",
				Computed:            true,
			},
			"basename": schema.StringAttribute{
				MarkdownDescription: "The effective channel name the provisioner uses: `name` normalized, or the project-name default.",
				Computed:            true,
			},
			"provision_status": schema.StringAttribute{
				MarkdownDescription: "Outcome of the asynchronous provision: `queued`, `provisioning`, `linked` or `failed`. " +
					"Null when nothing has been enqueued.",
				Computed: true,
			},
			"provision_note": schema.StringAttribute{
				MarkdownDescription: "Short note from the provision job, such as the channel it created or why it failed.",
				Computed:            true,
			},
			"invites_skipped": schema.Int64Attribute{
				MarkdownDescription: "Project members the latest invite cycle could not invite (no Slack match, or the " +
					"connection cannot read e-mail addresses). Non-zero means linked, but not everyone is in.",
				Computed: true,
			},
		},
	}
}

// slackChannelDataSourceSchema is the read-only view for the data source.
func slackChannelDataSourceSchema() schema.Attribute {
	return schema.SingleNestedAttribute{
		MarkdownDescription: "Per-project Slack channel configuration, read from the project's `slack-channel` API " +
			"resource. Null unless the token's user administers the project and the Flightdeck version exposes the " +
			"endpoint. `event_filter` reports every category resolved against its default.",
		Computed: true,
		Attributes: map[string]schema.Attribute{
			"enabled":               schema.BoolAttribute{MarkdownDescription: "Whether the project gets its own Slack channel.", Computed: true},
			"notifications_enabled": schema.BoolAttribute{MarkdownDescription: "Master switch for the project's Slack posts.", Computed: true},
			"name":                  schema.StringAttribute{MarkdownDescription: "Channel-name override; empty when there is none.", Computed: true},
			"event_filter":          schema.MapAttribute{MarkdownDescription: "Every activity category, resolved against its default.", ElementType: types.BoolType, Computed: true},
			"available":             schema.BoolAttribute{MarkdownDescription: "Whether the workspace has a connected Slack integration.", Computed: true},
			"scopes_sufficient":     schema.BoolAttribute{MarkdownDescription: "Whether that connection has the channel scopes.", Computed: true},
			"linked":                schema.BoolAttribute{MarkdownDescription: "Whether a Slack channel is linked.", Computed: true},
			"channel_id":            schema.StringAttribute{MarkdownDescription: "Slack's id for the linked channel.", Computed: true},
			"basename":              schema.StringAttribute{MarkdownDescription: "The effective channel name the provisioner uses.", Computed: true},
			"provision_status":      schema.StringAttribute{MarkdownDescription: "Outcome of the asynchronous provision.", Computed: true},
			"provision_note":        schema.StringAttribute{MarkdownDescription: "Short note from the provision job.", Computed: true},
			"invites_skipped":       schema.Int64Attribute{MarkdownDescription: "Members the latest invite cycle could not invite.", Computed: true},
		},
	}
}

// slackChannelWriteNeeded reports whether the CONFIGURED block asks for
// anything the project does not already have. An unchanged apply must not
// PATCH: every write re-enqueues provisioning, which resets provision_status
// to `queued` on a channel that is merely waiting to be linked.
func slackChannelWriteNeeded(ctx context.Context, config, state types.Object, diags *diag.Diagnostics) bool {
	if config.IsNull() || config.IsUnknown() {
		return false // nothing configured: this block is not managed here
	}
	var cfg slackChannelModel
	diags.Append(config.As(ctx, &cfg, objectAsOptions)...)
	if diags.HasError() {
		return false
	}
	if state.IsNull() || state.IsUnknown() {
		return true // create, or a block that was never readable before
	}
	var st slackChannelModel
	diags.Append(state.As(ctx, &st, objectAsOptions)...)
	if diags.HasError() {
		return false
	}

	if !cfg.Enabled.IsNull() && !cfg.Enabled.IsUnknown() && !cfg.Enabled.Equal(st.Enabled) {
		return true
	}
	if !cfg.NotificationsEnabled.IsNull() && !cfg.NotificationsEnabled.IsUnknown() && !cfg.NotificationsEnabled.Equal(st.NotificationsEnabled) {
		return true
	}
	// Compared by effective name, so a spelling the server normalizes is not a
	// change; a configured "" against a stored override is.
	if !cfg.Name.IsNull() && !cfg.Name.IsUnknown() &&
		normalizeSlackChannelName(cfg.Name.ValueString()) != normalizeSlackChannelName(st.Name.ValueString()) {
		return true
	}
	if cfg.EventFilter.IsNull() || cfg.EventFilter.IsUnknown() {
		return false
	}
	var wanted, current map[string]bool
	diags.Append(cfg.EventFilter.ElementsAs(ctx, &wanted, false)...)
	if !st.EventFilter.IsNull() && !st.EventFilter.IsUnknown() {
		diags.Append(st.EventFilter.ElementsAs(ctx, &current, false)...)
	}
	for category, want := range wanted {
		if have, ok := current[category]; !ok || have != want {
			return true
		}
	}
	return false
}

// slackChannelToObject maps the API's configuration into the block. scope says
// which event categories to record; prior supplies them when the scope is
// slackEventsManaged.
func slackChannelToObject(ctx context.Context, sc *client.SlackChannel, prior types.Map, scope slackEventFilterScope, diags *diag.Diagnostics) types.Object {
	if sc == nil {
		return types.ObjectNull(slackChannelAttrTypes)
	}
	obj, d := types.ObjectValue(slackChannelAttrTypes, map[string]attr.Value{
		"enabled":               types.BoolValue(sc.ChannelEnabled),
		"notifications_enabled": types.BoolValue(sc.NotificationsEnabled),
		"name":                  slackChannelNameValue{StringValue: types.StringValue(slackChannelNameOf(sc))},
		"event_filter":          slackEventFilterValue(ctx, sc.EventFilter, prior, scope, diags),
		"available":             types.BoolValue(sc.ChannelAvailable),
		"scopes_sufficient":     types.BoolValue(sc.ScopesSufficient),
		"linked":                types.BoolValue(sc.ChannelLinked),
		"channel_id":            stringPointerValue(sc.ChannelID),
		"basename":              types.StringValue(sc.ChannelBasename),
		"provision_status":      stringPointerValue(sc.ProvisionStatus),
		"provision_note":        stringPointerValue(sc.ProvisionNote),
		"invites_skipped":       types.Int64Value(sc.InvitesSkipped),
	})
	diags.Append(d...)
	return obj
}

// slackChannelNameOf spells "no override" as the empty string rather than
// null, because that is the one spelling a configuration can round-trip: the
// API reports no override as null but accepts a blank string to clear one,
// and the framework never consults semantic equality across a null, so a
// configured "" would otherwise read back as null and never settle.
func slackChannelNameOf(sc *client.SlackChannel) string {
	if sc.ChannelName == nil {
		return ""
	}
	return *sc.ChannelName
}

// slackEventFilterValue records the categories the configuration manages (the
// keys of prior), or every category the API reports when the scope is
// slackEventsAll.
func slackEventFilterValue(ctx context.Context, reported map[string]bool, prior types.Map, scope slackEventFilterScope, diags *diag.Diagnostics) types.Map {
	selected := map[string]bool{}
	switch scope {
	case slackEventsAll:
		selected = reported
	case slackEventsManaged:
		if prior.IsNull() || prior.IsUnknown() {
			return types.MapNull(types.BoolType)
		}
		for category := range prior.Elements() {
			if value, ok := reported[category]; ok {
				selected[category] = value
			}
		}
	}
	value, d := types.MapValueFrom(ctx, types.BoolType, selected)
	diags.Append(d...)
	return value
}

// slackEventFilterOf pulls the event_filter map out of a block, for use as the
// prior when mapping a fresh read.
func slackEventFilterOf(ctx context.Context, block types.Object, diags *diag.Diagnostics) types.Map {
	if block.IsNull() || block.IsUnknown() {
		return types.MapNull(types.BoolType)
	}
	var m slackChannelModel
	diags.Append(block.As(ctx, &m, objectAsOptions)...)
	if diags.HasError() {
		return types.MapNull(types.BoolType)
	}
	return m.EventFilter
}

// slackChannelFields returns the settings to send from the CONFIGURED block,
// or nil when the configuration has no block. Only configured keys are sent:
// the API changes nothing it is not given, which is how "unset keeps the
// project's value" is honoured. A configured "" is sent as it is, and resets
// the name override.
func slackChannelFields(ctx context.Context, block types.Object, diags *diag.Diagnostics) client.Fields {
	if block.IsNull() || block.IsUnknown() {
		return nil
	}
	var m slackChannelModel
	diags.Append(block.As(ctx, &m, objectAsOptions)...)
	if diags.HasError() {
		return nil
	}
	fields := client.Fields{}
	if !m.Enabled.IsNull() && !m.Enabled.IsUnknown() {
		fields["slack_channel_enabled"] = m.Enabled.ValueBool()
	}
	if !m.NotificationsEnabled.IsNull() && !m.NotificationsEnabled.IsUnknown() {
		fields["slack_notifications_enabled"] = m.NotificationsEnabled.ValueBool()
	}
	if !m.Name.IsNull() && !m.Name.IsUnknown() {
		fields["slack_channel_name"] = m.Name.ValueString()
	}
	if !m.EventFilter.IsNull() && !m.EventFilter.IsUnknown() {
		var categories map[string]bool
		diags.Append(m.EventFilter.ElementsAs(ctx, &categories, false)...)
		fields["slack_event_filter"] = categories
	}
	return fields
}

// readSlackChannel fetches the block for a project. A 403 (the token does not
// administer the project) or a 404 (project gone between calls, or a
// Flightdeck without the endpoint) leaves the block null; anything else is an
// error.
func readSlackChannel(ctx context.Context, c *client.Client, projectID int64, prior types.Map, scope slackEventFilterScope, diags *diag.Diagnostics) types.Object {
	sc, err := c.GetSlackChannel(ctx, projectID)
	if err != nil {
		if client.IsForbidden(err) || client.IsNotFound(err) {
			return types.ObjectNull(slackChannelAttrTypes)
		}
		addAPIError(diags, "Error reading Flightdeck Slack channel configuration", err)
		return types.ObjectNull(slackChannelAttrTypes)
	}
	return slackChannelToObject(ctx, sc, prior, scope, diags)
}

// writeSlackChannel brings the project's Slack channel configuration in line
// with the configured block and returns the block plus the project's
// lock_version.
//
// It writes only when the configuration asks for something the project does
// not already have: every write re-enqueues provisioning, which puts
// provision_status back to `queued` on a channel that is merely waiting to be
// linked, so an apply that changes nothing here must not make one.
func writeSlackChannel(ctx context.Context, c *client.Client, projectID int64, config, planned, state types.Object, lockVersion int64, diags *diag.Diagnostics) (types.Object, int64) {
	if slackChannelWriteNeeded(ctx, config, state, diags) {
		settings := slackChannelFields(ctx, config, diags)
		if diags.HasError() {
			return types.ObjectNull(slackChannelAttrTypes), lockVersion
		}
		sc, err := c.UpdateSlackChannel(ctx, projectID, settings, lockVersion)
		if err != nil {
			switch {
			case client.IsNotFound(err):
				diags.AddAttributeError(path.Root("slack_channel"), "Slack channel configuration is not available on this Flightdeck",
					"The project exists but its slack-channel endpoint answered 404, so this Flightdeck version does not expose "+
						"the project's Slack configuration over the API yet. Remove the block or upgrade Flightdeck. "+apiMessage(err))
			case client.IsForbidden(err):
				diags.AddAttributeError(path.Root("slack_channel"), "Slack channel configuration requires a project admin",
					"Only an admin of this project (or a workspace owner or admin) may read or write its Slack channel configuration. "+apiMessage(err))
			case client.IsStale(err):
				addStaleError(diags, "Project Slack channel configuration", lockVersion, nil, err)
			default:
				addAPIError(diags, "Error updating Flightdeck Slack channel configuration", err)
			}
			return types.ObjectNull(slackChannelAttrTypes), lockVersion
		}
		slackChannelProvisioningWarnings(sc, diags)
		return slackChannelToObject(ctx, sc, slackEventFilterOf(ctx, config, diags), slackEventsManaged, diags), sc.LockVersion
	}

	if !slackChannelHasUnknowns(planned) {
		return planned, lockVersion
	}
	// Nothing to write, but Terraform plans every server-owned value as unknown
	// once anything on the project changes, and a create has nothing to go on
	// at all, so those have to be filled: read, and keep whatever the plan
	// already promised.
	fresh := readSlackChannel(ctx, c, projectID, slackEventFilterOf(ctx, config, diags), slackEventsManaged, diags)
	return slackChannelResolveUnknowns(planned, fresh, state, diags), lockVersion
}

// slackChannelHasUnknowns reports whether the plan left anything for the apply
// to fill in.
func slackChannelHasUnknowns(planned types.Object) bool {
	if planned.IsUnknown() {
		return true
	}
	if planned.IsNull() {
		return false
	}
	for _, value := range planned.Attributes() {
		if value.IsUnknown() {
			return true
		}
	}
	return false
}

// slackChannelResolveUnknowns fills the values the plan left unknown from a
// fresh read, and keeps everything the plan already knows exactly as promised
// — Terraform requires the two to match. A read that came back null (the token
// lost the project-admin role, say) falls back to the values in state, which
// is where those known plan values came from.
func slackChannelResolveUnknowns(planned, fresh, state types.Object, diags *diag.Diagnostics) types.Object {
	if planned.IsNull() {
		return planned
	}
	if planned.IsUnknown() {
		return fresh
	}
	sources := []map[string]attr.Value{}
	for _, source := range []types.Object{fresh, state} {
		if !source.IsNull() && !source.IsUnknown() {
			sources = append(sources, source.Attributes())
		}
	}
	attrs := map[string]attr.Value{}
	for name, value := range planned.Attributes() {
		attrs[name] = value
		if !value.IsUnknown() {
			continue
		}
		for _, source := range sources {
			if from, ok := source[name]; ok {
				attrs[name] = from
				break
			}
		}
	}
	obj, d := types.ObjectValue(slackChannelAttrTypes, attrs)
	diags.Append(d...)
	if diags.HasError() {
		return planned
	}
	return obj
}

// slackChannelProvisioningWarnings flags the two states that look like success
// and provision nothing. The configuration is stored either way, so this is a
// warning: the fix is an interactive OAuth flow the API cannot perform.
func slackChannelProvisioningWarnings(sc *client.SlackChannel, diags *diag.Diagnostics) {
	if !sc.ChannelEnabled {
		return
	}
	switch {
	case !sc.ChannelAvailable:
		diags.AddAttributeWarning(path.Root("slack_channel").AtName("enabled"),
			"Slack channel enabled, but the workspace has no Slack integration",
			"Flightdeck saved the configuration and enqueued nothing, because no Slack integration is connected to this "+
				"workspace. Connect Slack under Settings → Integrations — an interactive authorization the API cannot "+
				"perform — and provisioning starts the next time this block changes, or from the project's settings page.")
	case !sc.ScopesSufficient:
		diags.AddAttributeWarning(path.Root("slack_channel").AtName("enabled"),
			"Slack channel enabled, but the workspace's Slack connection lacks the channel scopes",
			"The configuration is stored, but provisioning will fail: this workspace's Slack connection predates the "+
				"scopes needed to create channels and invite members. Re-authorize Slack under Settings → Integrations — "+
				"an interactive authorization the API cannot perform — then re-apply.")
	}
}

// normalizeSlackChannelName mirrors the API's rule: trim, lower-case, collapse
// every run of other characters into a single `-`, drop leading and trailing
// `-`, and cut to 80 characters. Blank input normalizes to "", which is how
// both sides spell "no override, use the project-name default".
func normalizeSlackChannelName(source string) string {
	var b strings.Builder
	b.Grow(len(source))
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(source)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	normalized := strings.Trim(b.String(), "-")
	if len(normalized) > slackChannelMaxName {
		normalized = normalized[:slackChannelMaxName]
	}
	return normalized
}

// slackChannelMaxName is the API's channel-name length cap.
const slackChannelMaxName = 80

// slackChannelNameValidator refuses a name that normalizes to nothing, which
// the API answers with a 422. Blank is fine: it is the reset.
type slackChannelNameValidator struct{}

func (slackChannelNameValidator) Description(context.Context) string {
	return "must contain at least one letter or digit"
}

func (v slackChannelNameValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (slackChannelNameValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	name := req.ConfigValue.ValueString()
	if strings.TrimSpace(name) == "" || normalizeSlackChannelName(name) != "" {
		return
	}
	resp.Diagnostics.AddAttributeError(req.Path, "Invalid Slack channel name",
		fmt.Sprintf("%q normalizes to an empty Slack channel name; it needs at least one letter or digit. "+
			"Omit the attribute to use the project-name default.", name))
}

// slackChannelNameType is a string type whose values compare by the channel
// they name rather than by spelling: `My Team!`, `my-team` and ` My  Team ! `
// all name #my-team. The server stores the normalized form, so a configured
// spelling reads back as equal to itself and no perpetual diff appears — the
// same arrangement hexColorType uses for colours.
type slackChannelNameType struct {
	basetypes.StringType
}

var _ basetypes.StringTypable = slackChannelNameType{}

func (t slackChannelNameType) Equal(o attr.Type) bool {
	_, ok := o.(slackChannelNameType)
	return ok
}

func (t slackChannelNameType) String() string { return "slackChannelNameType" }

func (t slackChannelNameType) ValueFromString(_ context.Context, in basetypes.StringValue) (basetypes.StringValuable, diag.Diagnostics) {
	return slackChannelNameValue{StringValue: in}, nil
}

func (t slackChannelNameType) ValueFromTerraform(ctx context.Context, in tftypes.Value) (attr.Value, error) {
	v, err := t.StringType.ValueFromTerraform(ctx, in)
	if err != nil {
		return nil, err
	}
	sv, ok := v.(basetypes.StringValue)
	if !ok {
		return nil, fmt.Errorf("unexpected value type %T", v)
	}
	return slackChannelNameValue{StringValue: sv}, nil
}

func (t slackChannelNameType) ValueType(context.Context) attr.Value { return slackChannelNameValue{} }

// slackChannelNameValue is the value side of slackChannelNameType.
type slackChannelNameValue struct {
	basetypes.StringValue
}

var _ basetypes.StringValuableWithSemanticEquals = slackChannelNameValue{}

func (v slackChannelNameValue) Type(context.Context) attr.Type { return slackChannelNameType{} }

func (v slackChannelNameValue) Equal(o attr.Value) bool {
	other, ok := o.(slackChannelNameValue)
	if !ok {
		return false
	}
	return v.StringValue.Equal(other.StringValue)
}

// StringSemanticEquals treats two names as equal when they normalize to the
// same channel. A null and a blank name are equal too: both mean "no
// override", which the API reports back as null.
func (v slackChannelNameValue) StringSemanticEquals(_ context.Context, other basetypes.StringValuable) (bool, diag.Diagnostics) {
	o, ok := other.(slackChannelNameValue)
	if !ok {
		return false, nil
	}
	if v.IsUnknown() || o.IsUnknown() {
		return v.StringValue.Equal(o.StringValue), nil
	}
	return normalizeSlackChannelName(v.ValueString()) == normalizeSlackChannelName(o.ValueString()), nil
}
