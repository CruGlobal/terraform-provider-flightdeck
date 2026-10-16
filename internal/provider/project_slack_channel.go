package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	datasourceschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
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
//     existing, and a refresh picks the outcome up whenever it lands. The one
//     synchronous part is a check the API makes before saving a write that
//     leaves the channel enabled and not yet linked: a channel that exists
//     but cannot be posted to is refused with a 422 (slack_channel_unusable)
//     and nothing is saved. On an update that is an error, because there is
//     nothing to converge on. On the apply that creates the project it is a
//     warning, so the new project is not tainted and replaced over a Slack
//     problem; the refresh before the next plan shows the change again. A job
//     that fails later is only a warning, because by then the configuration
//     is saved and an error would desync state.
//   - THE EVENT FILTER MERGES server-side, unlike every other object on this
//     API: only the categories a write names change. So event_filter declares
//     OVERRIDES, not the full set — dropping a category from the map stops
//     managing it at its current value rather than restoring its default.
//   - THE CHANNEL NAME IS STORED AS TYPED, trimmed and no more: "Release Eng"
//     comes back as "Release Eng", and the Slack-legal form derived from it is
//     reported separately as `basename`. `name` carries a semantic-equality
//     type only to absorb that trim, so surrounding whitespace in a
//     configuration is not a perpetual diff.
//
// The channel mode (`mode`, the API's slack_channel_mode) says how Flightdeck
// gets the channel: create it and invite the members, or link one that exists
// and invite nobody. Null means nobody has chosen, and the API has no way back
// to null once a mode is set: a null or blank on a write leaves the mode
// alone. So the mode is sent only when configured, and the two refusals it
// adds (create naming a taken channel, link naming a missing one) arrive as
// the same 422 slack_channel_unusable as the bot check. A Flightdeck older
// than the mode has no such key on a read, so the block reads null for it
// there and the provider never sends it unless the configuration sets it.
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
	Mode                 types.String          `tfsdk:"mode"`
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
	"mode":                  types.StringType,
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

// slackChannelModeDescription is the resource's `mode` documentation.
const slackChannelModeDescription = "How Flightdeck gets the channel: `create` or `link`. With `create`, Flightdeck " +
	"creates the channel and invites the project's members, when it sets the channel up and again whenever someone " +
	"joins the project; if another channel already has the name, the write is refused. With `link`, Flightdeck uses " +
	"a channel that already exists, joining it if it is public, and **never invites anyone**: not when it links the " +
	"channel, not on a later save, and not when someone joins the project. If no channel has the name, the write is " +
	"refused. A private channel counts as missing until the Flightdeck app is a member, so **for a private channel, " +
	"invite the Flightdeck app to it in Slack first**.\n\n" +
	"Either refusal is the HTTP 422 `slack_channel_unusable` described above: nothing is saved, and the apply fails " +
	"with Flightdeck's message on this attribute (a warning instead on the apply that creates the project). Applying " +
	"the same configuration again gets the same answer, so change `mode` or `name` first. On a channel that is " +
	"enabled but not linked yet, changing the mode runs the check again under the new mode.\n\n" +
	"Switching a linked channel to `create` makes Flightdeck check whether it made that channel. While the channel " +
	"is enabled and Slack answers, that check happens during the write, and the write is refused if Flightdeck did " +
	"not make it. When the channel is turned off, or Slack cannot be reached, Flightdeck saves the switch, keeps " +
	"the link and invites nobody into it. Its background job checks the channel later (for a channel that is off, " +
	"once it is turned on again), and if Flightdeck did not make the channel, `provision_status` then reads " +
	"`failed` and `provision_note` says why. Switching never undoes anything: members already invited stay in the " +
	"channel, and a linked channel stays linked.\n\n" +
	"A project where nobody has chosen reads null, and Flightdeck keeps the behavior from before the mode existed: " +
	"it links a channel with that name if there is one, creates it if not, and invites the members either way. When " +
	"unset, the project's current mode is kept and nothing is sent, so removing the attribute plans no change. " +
	"Flightdeck has no way to go back to null once a mode is set.\n\n" +
	"Needs the Flightdeck release that added the Slack channel mode. On an older Flightdeck it reads as null; leave " +
	"it unset there, because Flightdeck refuses it and the provider asks you to upgrade Flightdeck (an error, or a " +
	"warning on the apply that creates the project)."

// slackChannelSchema is the resource attribute.
func slackChannelSchema() schema.Attribute {
	return schema.SingleNestedAttribute{
		MarkdownDescription: "Per-project Slack channel configuration, managed through the project's `slack-channel` API " +
			"resource. Reading and writing it requires the token's user to be an **admin of this project** (a workspace " +
			"owner or admin qualifies); for other tokens, and on a Flightdeck version without the endpoint, the block is " +
			"null.\n\n" +
			"**Provisioning is asynchronous, after one synchronous check.** When a write leaves the channel enabled " +
			"and not yet linked, Flightdeck first looks the channel up in Slack. If it exists but Flightdeck cannot " +
			"post to it (a private channel the Flightdeck bot is not a member of, or an archived channel), the API " +
			"answers HTTP 422 (`slack_channel_unusable`) and saves nothing. On an update the apply fails with " +
			"Flightdeck's message, which names the channel and the bot to invite, and the next plan still shows the " +
			"change: invite the bot, or choose a different `name`, and apply again. On the apply that creates the " +
			"project, the provider gives the same message as a warning instead, so the new project is kept as " +
			"created; the next plan shows the Slack change again, and the next apply retries the channel. A channel " +
			"that does not exist yet passes the check, since Flightdeck can create it, unless `mode` is `link`. The " +
			"same 422 refuses a channel that `mode` rules out, as described there. If Slack is rate limiting or " +
			"unavailable, the check is skipped and the write goes ahead.\n\n" +
			"Once the check passes, Flightdeck saves the configuration and enqueues a job that creates the channel " +
			"if needed, links it and invites the project's members (in `link` mode it invites nobody). The write " +
			"may come back already `linked`, but " +
			"usually `channel_id`, `linked`, `provision_status` and `provision_note` are filled in afterwards and a " +
			"later refresh picks them up. An apply that ends with `provision_status` at `queued` has succeeded: it " +
			"does not wait for Slack, and the pending values never produce a diff. If the write comes back with " +
			"`provision_status` at `failed`, or at `needs_invite` (linked, but Flightdeck is not in the channel and " +
			"cannot join it, so it cannot invite anyone), the provider warns and passes `provision_note` along " +
			"rather than failing, since the configuration is saved.\n\n" +
			"Nothing provisions at all when `available` is false (the workspace has no connected Slack integration) or " +
			"`scopes_sufficient` is false (the connection predates the channel scopes). Both are fixed by re-authorizing " +
			"Slack under Settings → Integrations, which the API cannot do; the provider warns rather than failing, since " +
			"the configuration is still stored.\n\n" +
			"A write here bumps the project's `lock_version`. Attributes map onto the API's keys by dropping the " +
			"`slack_` prefix (`enabled` is `slack_channel_enabled`, `mode` is `slack_channel_mode`, " +
			"`notifications_enabled` is `slack_notifications_enabled`, `name` is `slack_channel_name`, " +
			"`event_filter` is `slack_event_filter`).",
		Optional: true,
		Computed: true,
		PlanModifiers: []planmodifier.Object{
			objectplanmodifier.UseStateForUnknown(),
		},
		Attributes: map[string]schema.Attribute{
			"enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether this project gets its own Slack channel. When unset, the project's current " +
					"value is kept. Turning it on enqueues provisioning once Flightdeck has checked that it can post to " +
					"the channel (see above); turning it off stops future posts but never deletes the channel on Slack.",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"mode": schema.StringAttribute{
				MarkdownDescription: slackChannelModeDescription,
				Optional:            true,
				Computed:            true,
				Validators:          []validator.String{stringvalidator.OneOf(client.SlackChannelModes...)},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
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
				MarkdownDescription: "Channel name to use instead of the project-name default, stored as you write it " +
					"(Flightdeck only trims surrounding whitespace, which is why that whitespace is not a diff). The " +
					"Slack-legal name derived from it — lower case, runs of other characters collapsed to `-`, at most 80 " +
					"characters — is reported as `basename`, so `Release Eng` here means channel `release-eng`. A name with " +
					"no letter or digit is refused, since it would derive to nothing. When unset the project's current " +
					"value is kept, so **set it to `\"\"` to clear an override** and fall back to the project-name default " +
					"— removing the attribute leaves whatever is there. Changing the derived name drops the stored " +
					"`channel_id` so the next provision links the new channel; the old one is left on Slack.",
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
				MarkdownDescription: "Outcome of the asynchronous provision: `queued`, `provisioning`, `linked`, " +
					"`needs_invite` or `failed`. `needs_invite` means the channel is linked, but Flightdeck is not in it " +
					"and cannot join it by itself (a private channel it was removed from, an archived one, or a " +
					"connection without permission to join), so it cannot invite members until someone adds the " +
					"Flightdeck app to the channel; `provision_note` says what to do. Null when nothing has been enqueued.",
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
func slackChannelDataSourceSchema() datasourceschema.Attribute {
	return datasourceschema.SingleNestedAttribute{
		MarkdownDescription: "Per-project Slack channel configuration, read from the project's `slack-channel` API " +
			"resource. Null unless the token's user administers the project and the Flightdeck version exposes the " +
			"endpoint. `event_filter` reports every category resolved against its default.",
		Computed: true,
		Attributes: map[string]datasourceschema.Attribute{
			"enabled": datasourceschema.BoolAttribute{MarkdownDescription: "Whether the project gets its own Slack channel.", Computed: true},
			"mode": datasourceschema.StringAttribute{
				MarkdownDescription: "How Flightdeck gets the channel: `create` (it makes the channel and invites the project's " +
					"members) or `link` (it uses a channel that already exists and never invites anyone). Null when nobody " +
					"has chosen, which links a channel with that name if there is one, creates it if not, and invites the " +
					"members either way. Also null on a Flightdeck older than the Slack channel mode.",
				Computed: true,
			},
			"notifications_enabled": datasourceschema.BoolAttribute{MarkdownDescription: "Master switch for the project's Slack posts.", Computed: true},
			"name":                  datasourceschema.StringAttribute{MarkdownDescription: "Channel-name override; empty when there is none.", Computed: true},
			"event_filter":          datasourceschema.MapAttribute{MarkdownDescription: "Every activity category, resolved against its default.", ElementType: types.BoolType, Computed: true},
			"available":             datasourceschema.BoolAttribute{MarkdownDescription: "Whether the workspace has a connected Slack integration.", Computed: true},
			"scopes_sufficient":     datasourceschema.BoolAttribute{MarkdownDescription: "Whether that connection has the channel scopes.", Computed: true},
			"linked":                datasourceschema.BoolAttribute{MarkdownDescription: "Whether a Slack channel is linked.", Computed: true},
			"channel_id":            datasourceschema.StringAttribute{MarkdownDescription: "Slack's id for the linked channel.", Computed: true},
			"basename":              datasourceschema.StringAttribute{MarkdownDescription: "The effective channel name the provisioner uses.", Computed: true},
			"provision_status":      datasourceschema.StringAttribute{MarkdownDescription: "Outcome of the asynchronous provision.", Computed: true},
			"provision_note":        datasourceschema.StringAttribute{MarkdownDescription: "Short note from the provision job.", Computed: true},
			"invites_skipped":       datasourceschema.Int64Attribute{MarkdownDescription: "Members the latest invite cycle could not invite.", Computed: true},
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
	// A configured mode against a project that has none (null: nobody chose,
	// or a Flightdeck older than the mode) is a change too.
	if known(cfg.Mode) && !cfg.Mode.Equal(st.Mode) {
		return true
	}
	if !cfg.NotificationsEnabled.IsNull() && !cfg.NotificationsEnabled.IsUnknown() && !cfg.NotificationsEnabled.Equal(st.NotificationsEnabled) {
		return true
	}
	// Compared on the trim the server applies, so padding is not a change; a
	// configured "" against a stored override is.
	if !cfg.Name.IsNull() && !cfg.Name.IsUnknown() &&
		strings.TrimSpace(cfg.Name.ValueString()) != strings.TrimSpace(st.Name.ValueString()) {
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
		"mode":                  stringPointerValue(sc.Mode),
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
// the name override. An unset mode is never sent, not even as null: a
// Flightdeck older than the mode refuses the key whatever its value.
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
	if known(m.Mode) {
		fields["slack_channel_mode"] = m.Mode.ValueString()
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

// slackChannelApply says which kind of project apply a Slack channel write
// belongs to. The two answer a slack_channel_unusable refusal differently, and
// an older Flightdeck's refusal of the mode as an unknown key the same way.
type slackChannelApply int

const (
	// slackChannelOnUpdate: the refusal is an error. A failed update leaves
	// the project in state as it was, so nothing is lost by failing.
	slackChannelOnUpdate slackChannelApply = iota
	// slackChannelOnCreate: the refusal is a warning. Any error during a
	// create makes Terraform taint the new project, and the next apply would
	// replace it, which is far worse than a channel that still needs its bot
	// invited.
	slackChannelOnCreate
)

// writeSlackChannel brings the project's Slack channel configuration in line
// with the configured block and returns the block plus the project's
// lock_version.
//
// It writes only when the configuration asks for something the project does
// not already have: every write re-enqueues provisioning, which puts
// provision_status back to `queued` on a channel that is merely waiting to be
// linked, so an apply that changes nothing here must not make one.
func writeSlackChannel(ctx context.Context, c *client.Client, projectID int64, config, planned, state types.Object, lockVersion int64, apply slackChannelApply, diags *diag.Diagnostics) (types.Object, int64) {
	if slackChannelWriteNeeded(ctx, config, state, diags) {
		settings := slackChannelFields(ctx, config, diags)
		if diags.HasError() {
			return types.ObjectNull(slackChannelAttrTypes), lockVersion
		}
		sc, err := c.UpdateSlackChannel(ctx, projectID, settings, lockVersion)
		// A Flightdeck older than the mode refuses the key as unknown, and the
		// whole write with it.
		modeUnsupported, modeNote := slackChannelModeUnsupported(ctx, c, projectID, settings, err)
		if err != nil && apply == slackChannelOnCreate &&
			(client.HasCode(err, client.CodeSlackChannelUnusable) || modeUnsupported) {
			if modeUnsupported {
				addSlackChannelModeUnsupported(err, apply, diags)
			} else {
				addSlackChannelUnusable(ctx, config, err, apply, diags)
			}
			// A create that returns no error must hand back what it planned, so
			// the configured values are kept even though Flightdeck saved none
			// of them, and only the unknowns are filled from a fresh read. The
			// refresh before the next plan reads the real values back, so that
			// plan shows the Slack change again, as an update.
			fresh := readSlackChannel(ctx, c, projectID, slackEventFilterOf(ctx, config, diags), slackEventsManaged, diags)
			return slackChannelKeepPlan(ctx, planned, fresh, diags), lockVersion
		}
		if err != nil {
			if addIfProjectGone(ctx, c, projectID, "", err, diags) {
				return types.ObjectNull(slackChannelAttrTypes), lockVersion
			}
			switch {
			case client.IsNotFound(err):
				diags.AddAttributeError(path.Root("slack_channel"), "Slack channel configuration is not available on this Flightdeck",
					"The project exists but its slack-channel endpoint answered 404, so this Flightdeck version does not expose "+
						"the project's Slack configuration over the API yet. Remove the block or upgrade Flightdeck. "+apiMessage(err))
			case client.IsForbidden(err):
				diags.AddAttributeError(path.Root("slack_channel"), "Slack channel configuration requires a project admin",
					"Only an admin of this project (or a workspace owner or admin) may read or write its Slack channel configuration. "+apiMessage(err))
			case client.HasCode(err, client.CodeSlackChannelUnusable):
				// Always an error here: the create's warning is handled above,
				// because a warning needs the planned block, not the null below.
				addSlackChannelUnusable(ctx, config, err, slackChannelOnUpdate, diags)
			case modeUnsupported:
				addSlackChannelModeUnsupported(err, slackChannelOnUpdate, diags)
			case client.IsStale(err):
				addStaleError(diags, "Project Slack channel configuration", lockVersion, nil, err)
			default:
				addAPIErrorNote(diags, "Error updating Flightdeck Slack channel configuration", err, modeNote)
			}
			// Null, like every failed sub-block write: the refresh before the
			// next plan reads the block back, and after a slack_channel_unusable
			// refusal that read is the project as it was, since nothing was saved
			// and lock_version did not move. So the change shows in the next plan
			// again, and applies once the channel can be used.
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

// slackChannelKeepPlan is the block a create records after a refusal: every
// value the plan already knows, as promised, and the unknowns from a fresh
// read. Anything the read could not supply becomes null, since an apply that
// succeeds may not leave a value unknown.
func slackChannelKeepPlan(ctx context.Context, planned, fresh types.Object, diags *diag.Diagnostics) types.Object {
	resolved := slackChannelResolveUnknowns(planned, fresh, types.ObjectNull(slackChannelAttrTypes), diags)
	if resolved.IsNull() || resolved.IsUnknown() {
		return types.ObjectNull(slackChannelAttrTypes)
	}
	attrs := map[string]attr.Value{}
	for name, value := range resolved.Attributes() {
		attrs[name] = value
		if !value.IsUnknown() {
			continue
		}
		typ := slackChannelAttrTypes[name]
		null, err := typ.ValueFromTerraform(ctx, tftypes.NewValue(typ.TerraformType(ctx), nil))
		if err != nil {
			diags.AddError("Error building the Slack channel block", err.Error())
			return resolved
		}
		attrs[name] = null
	}
	obj, d := types.ObjectValue(slackChannelAttrTypes, attrs)
	diags.Append(d...)
	return obj
}

// addSlackChannelUnusable reports the API's refusal of a channel: one that
// exists but that Flightdeck cannot post to, or one the channel mode rules out
// (create naming a taken channel, link naming a missing one). It is an error
// on an update and a warning on the apply that creates the project (see
// slackChannelApply). The API's own message leads the detail, because it names
// the channel, and the bot to invite or the mode to change, and none of that is
// known here.
//
// The diagnostic points at `mode` for a mode refusal when the configuration
// sets the mode. Otherwise it points at `name` when the configuration sets
// one, since that is the channel the check looked up, and at the block when
// the channel comes from the project name.
func addSlackChannelUnusable(ctx context.Context, config types.Object, err error, apply slackChannelApply, diags *diag.Diagnostics) {
	at := path.Root("slack_channel")
	var cfg slackChannelModel
	readable := !config.IsNull() && !config.IsUnknown() && !config.As(ctx, &cfg, objectAsOptions).HasError()
	byMode := isSlackChannelModeRefusal(apiMessage(err))
	switch {
	case readable && byMode && known(cfg.Mode):
		at = at.AtName("mode")
	case readable && known(cfg.Name) && strings.TrimSpace(cfg.Name.ValueString()) != "":
		at = at.AtName("name")
	}
	const summary = "Slack channel cannot be used"
	switch {
	case apply == slackChannelOnCreate && byMode:
		diags.AddAttributeWarning(at, summary,
			apiMessage(err)+"\n\n"+
				"The project was created, but Flightdeck checked the channel against the channel mode before saving "+
				"and saved none of this block's changes. The next plan shows them again, and the next apply is refused "+
				"the same way, so change `mode` or choose a different `name` before then.")
	case apply == slackChannelOnCreate:
		diags.AddAttributeWarning(at, summary,
			apiMessage(err)+"\n\n"+
				"The project was created, but Flightdeck checked the channel before saving and saved none of this "+
				"block's changes. The next plan shows them again, and the next apply tries the channel again, so fix "+
				"the channel in Slack, or choose a different `name`, before then.")
	case byMode:
		diags.AddAttributeError(at, summary,
			apiMessage(err)+"\n\n"+
				"Flightdeck checked the channel against the channel mode before saving and saved none of this block's "+
				"changes, so the next plan still shows them. Applying them again is refused the same way: change "+
				"`mode`, or choose a different `name`, then apply again.")
	default:
		diags.AddAttributeError(at, summary,
			apiMessage(err)+"\n\n"+
				"Flightdeck checked the channel before saving and saved none of this block's changes, so the next plan "+
				"still shows them. Fix the channel in Slack, or choose a different `name`, then apply again.")
	}
}

// slackChannelModeRefusal matches the two refusals the channel mode adds, as
// Flightdeck words them: "A Slack channel named #x already exists. ..." in
// create mode and "No Slack channel named #x. ..." in link mode. They share
// the slack_channel_unusable code with the bot check, so only the prose tells
// them apart; Flightdeck's documentation quotes both word for word. The match
// decides only where the diagnostic points and which advice follows
// Flightdeck's message, which always leads, so a reworded refusal still reads
// correctly.
var slackChannelModeRefusal = regexp.MustCompile(`^(A Slack channel named #\S+ already exists\.|No Slack channel named #\S+\.)`)

func isSlackChannelModeRefusal(message string) bool {
	return slackChannelModeRefusal.MatchString(strings.TrimSpace(message))
}

// slackChannelModeUnknownNote closes the generic error for a refused write
// that sent the mode when a read could not tell whether this Flightdeck knows
// it.
const slackChannelModeUnknownNote = "If the API calls slack_channel_mode an unknown key, this Flightdeck is older " +
	"than the release that added the Slack channel mode: upgrade Flightdeck first, or leave `mode` out of the " +
	"slack_channel block."

// slackChannelModeUnsupported says whether a refused write failed because it
// sent the mode to a Flightdeck too old to know it. Such a Flightdeck refuses
// the key as unknown, in prose, so rather than match the wording a read is
// asked whether it reports the mode, the way terraform_managed is checked: a
// Flightdeck that knows the mode always has the key, null included. When the
// read fails and cannot tell, it returns a note for the caller's generic error
// instead, since the API's own words may still say what happened.
func slackChannelModeUnsupported(ctx context.Context, c *client.Client, projectID int64, sent client.Fields, err error) (unsupported bool, note string) {
	if err == nil || !client.IsValidation(err) || client.HasCode(err, client.CodeSlackChannelUnusable) {
		return false, ""
	}
	if _, named := sent["slack_channel_mode"]; !named {
		return false, ""
	}
	sc, rerr := c.GetSlackChannel(ctx, projectID)
	if rerr != nil {
		return false, slackChannelModeUnknownNote
	}
	return !sc.ReportsMode(), ""
}

// addSlackChannelModeUnsupported reports a write that sent the mode to a
// Flightdeck older than it, on the attribute: an error on an update, a warning
// on the apply that creates the project, for the reason slackChannelApply
// gives. Flightdeck refused the whole write, so nothing in the block was saved.
func addSlackChannelModeUnsupported(err error, apply slackChannelApply, diags *diag.Diagnostics) {
	at := path.Root("slack_channel").AtName("mode")
	const summary = "This Flightdeck does not support slack_channel.mode yet"
	if apply == slackChannelOnCreate {
		diags.AddAttributeWarning(at, summary,
			"The project was created, but this Flightdeck refused slack_channel_mode as an unknown setting: it is "+
				"older than the release that added the Slack channel mode. None of this block's changes were saved. "+
				"The next plan shows them again, and the next apply fails on this until Flightdeck is upgraded or "+
				"`mode` is left out of the slack_channel block.\n\nThe API said: "+apiMessage(err))
		return
	}
	diags.AddAttributeError(at, summary,
		"This Flightdeck refused slack_channel_mode as an unknown setting: it is older than the release that added "+
			"the Slack channel mode. Nothing was saved, including this block's other changes, so the next plan still "+
			"shows them.\n\nUpgrade Flightdeck first, then apply again. Until then, leave `mode` out of the "+
			"slack_channel block: on this Flightdeck it reads as null, and the provider never sends it while it is "+
			"unset.\n\nThe API said: "+apiMessage(err))
}

// slackChannelProvisioningWarnings flags the two states that look like success
// and provision nothing, a provision that has already failed, and a channel
// that is linked but that Flightdeck is not in and cannot join. The
// configuration is stored in every case, so these are warnings: the first two
// need an interactive OAuth flow the API cannot perform, and an error for the
// others would leave state out of step with what Flightdeck saved. The last
// two name the project, because Terraform folds warnings that share a summary
// into one, and each project's channel needs its own fix.
func slackChannelProvisioningWarnings(sc *client.SlackChannel, diags *diag.Diagnostics) {
	if !sc.ChannelEnabled {
		return
	}
	said := func(fallback string) string {
		if sc.ProvisionNote != nil && strings.TrimSpace(*sc.ProvisionNote) != "" {
			return "Flightdeck said: " + strings.TrimSpace(*sc.ProvisionNote)
		}
		return fallback
	}
	status := ""
	if sc.ProvisionStatus != nil {
		status = *sc.ProvisionStatus
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
	case status == client.SlackProvisionFailed:
		diags.AddAttributeWarning(path.Root("slack_channel").AtName("enabled"),
			fmt.Sprintf("Slack channel enabled for project %d, but provisioning failed", sc.ProjectID),
			said("Flightdeck did not say why.")+"\n\n"+
				"The configuration is saved, so this apply still succeeded, and `provision_status` stays `failed` until "+
				"the channel is provisioned. Once the cause is fixed, provisioning runs again the next time this block "+
				"changes, or from the project's settings page.")
	case status == client.SlackProvisionNeedsInvite:
		diags.AddAttributeWarning(path.Root("slack_channel").AtName("enabled"),
			fmt.Sprintf("Slack channel linked for project %d, but Flightdeck is not in it", sc.ProjectID),
			said("Flightdeck is not a member of the channel and cannot join it by itself.")+"\n\n"+
				"The configuration is saved and the channel is linked, so this apply still succeeded, but Flightdeck "+
				"cannot invite the project's members until someone adds the Flightdeck app to the channel in Slack. "+
				"`provision_status` stays `needs_invite` until Flightdeck checks again, which happens the next time "+
				"this block changes, the next time a member is invited, or when the Slack settings are saved on the "+
				"project's settings page. Terraform writes this block only when its configuration changes, so after "+
				"inviting the app, save the settings page once rather than re-applying an unchanged configuration.")
	}
}

// slackChannelNameDerivesToNothing reports whether the API would refuse a
// name. Flightdeck derives the Slack channel name by lower-casing and turning
// every run of other characters into "-", so a name carrying no letter or
// digit derives to nothing at all — the one thing it will not store.
func slackChannelNameDerivesToNothing(name string) bool {
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// slackChannelNameValidator refuses a name that derives to nothing, which the
// API answers with a 422. Blank is fine: it is the reset.
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
	if strings.TrimSpace(name) == "" || !slackChannelNameDerivesToNothing(name) {
		return
	}
	resp.Diagnostics.AddAttributeError(req.Path, "Invalid Slack channel name",
		fmt.Sprintf("%q derives to an empty Slack channel name; it needs at least one letter or digit. "+
			"Omit the attribute to use the project-name default.", name))
}

// slackChannelNameType is a string type whose values compare after trimming,
// which is the one liberty the server takes with a channel name: it stores
// what you wrote, minus surrounding whitespace. Everything else about the
// spelling is significant, because two names that derive to the same Slack
// channel are still two different stored names.
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

// StringSemanticEquals treats two names as equal when they are equal once
// trimmed. A null and a blank name are equal too: both mean "no override",
// which the API reports back as null.
func (v slackChannelNameValue) StringSemanticEquals(_ context.Context, other basetypes.StringValuable) (bool, diag.Diagnostics) {
	o, ok := other.(slackChannelNameValue)
	if !ok {
		return false, nil
	}
	if v.IsUnknown() || o.IsUnknown() {
		return v.StringValue.Equal(o.StringValue), nil
	}
	return strings.TrimSpace(v.ValueString()) == strings.TrimSpace(o.ValueString()), nil
}
