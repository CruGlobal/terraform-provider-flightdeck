package provider

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	datasourceschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The auto_promote attribute holds a project's auto-promote settings: whether
// Flightdeck promotes the project's release candidates to production by
// itself, and when it may. It has no API resource of its own. The settings
// ride the self-healing resource, GET/PATCH
// /api/v1/projects/:project_id/self-healing, flat beside `rollback`, because
// auto-rollback is auto-promote's safety net. So:
//
//   - One read serves both attributes, and one write: when an apply changes
//     self_healing and auto_promote together, both go in a single PATCH
//     (writeSelfHealing). Flightdeck checks the whole request before it saves
//     anything, and both share the project's lock_version as the If-Match.
//   - Two names differ on the wire: `mode` is the API's `auto_promote`, and
//     `window` is its `promote_window`. The rest keep their names.
//   - The endpoint merges, and reads null or blank as "no opinion" for every
//     key and every part of the window, so only what is configured is sent,
//     and a setting left unset keeps whatever the project has.
//   - `auto_promote_blockers` changes with the project and every deploy, like
//     `rollback_blockers`. It is reported as warnings on refresh and after
//     apply while the mode is not "off", never stored.
//   - Flightdeck stores the window's days in week order and the freeze dates
//     sorted, and refuses either twice, so both are sets here: a list in
//     another order would never match what Flightdeck reads back.
//   - A Flightdeck older than auto-promote has none of the keys on a read, so
//     the attribute reads as null there, and it refuses a write naming any of
//     them as an unknown setting (addUnsupportedSettingError says to upgrade).
//
// Every attribute is Optional+Computed with UseStateForUnknown and no
// framework default: a default would manufacture a value for an unset
// attribute and write it, which on a merging endpoint overrides whatever the
// project has.

// autoPromoteModel is the Terraform shape of the attribute.
type autoPromoteModel struct {
	Mode         types.String `tfsdk:"mode"`
	SoakHours    types.Int64  `tfsdk:"soak_hours"`
	Window       types.Object `tfsdk:"window"`
	FreezeDates  types.Set    `tfsdk:"freeze_dates"`
	HeadsUpHours types.Int64  `tfsdk:"heads_up_hours"`
}

// promoteWindowModel is the Terraform shape of auto_promote.window.
type promoteWindowModel struct {
	Days     types.Set    `tfsdk:"days"`
	Start    types.String `tfsdk:"start"`
	End      types.String `tfsdk:"end"`
	TimeZone types.String `tfsdk:"time_zone"`
}

var promoteWindowAttrTypes = map[string]attr.Type{
	"days":      types.SetType{ElemType: types.StringType},
	"start":     types.StringType,
	"end":       types.StringType,
	"time_zone": types.StringType,
}

var autoPromoteAttrTypes = map[string]attr.Type{
	"mode":           types.StringType,
	"soak_hours":     types.Int64Type,
	"window":         types.ObjectType{AttrTypes: promoteWindowAttrTypes},
	"freeze_dates":   types.SetType{ElemType: types.StringType},
	"heads_up_hours": types.Int64Type,
}

// The API's bounds.
const (
	autoPromoteMinSoakHours     = 4
	autoPromoteMaxSoakHours     = 720
	autoPromoteMinHeadsUpHours  = 1
	autoPromoteMaxHeadsUpHours  = 72
	autoPromoteMaxFreezeDates   = 100
	promoteWindowStartPattern   = `^([01][0-9]|2[0-3]):[0-5][0-9]$`
	promoteWindowEndPattern     = `^(([01][0-9]|2[0-3]):[0-5][0-9]|24:00)$`
	promoteWindowTimeZoneFormat = `^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+)*$`
)

var (
	promoteWindowStart    = regexp.MustCompile(promoteWindowStartPattern)
	promoteWindowEnd      = regexp.MustCompile(promoteWindowEndPattern)
	promoteWindowTimeZone = regexp.MustCompile(promoteWindowTimeZoneFormat)
)

// autoPromoteDescription is the resource attribute's documentation.
const autoPromoteDescription = "Auto-promote settings: whether Flightdeck promotes this project's release " +
	"candidates to production by itself, and when it may. They are written through the project's `self-healing` " +
	"API resource, beside `self_healing.rollback`, because auto-rollback is auto-promote's safety net, so reading " +
	"and writing them requires the token's user to be a **workspace owner or admin**; for other tokens the attribute " +
	"is null. When an apply changes both `self_healing` and `auto_promote`, they go to Flightdeck in one request, " +
	"which Flightdeck checks whole before it saves anything.\n\n" +
	"Auto-promote is off until `mode` turns it on. A plan that turns it on carries a warning saying so. While it " +
	"is on, each thing that stops Flightdeck promoting right now (the API's `auto_promote_blockers`, such as " +
	"auto-rollback not being on, or anything that would stop auto-rollback) is shown as a warning on refresh and " +
	"after apply rather than stored, because it changes as the project does. A Flightdeck that can't promote yet " +
	"lists `not_yet_available` for every project; the settings are saved there and take effect once promoting " +
	"ships.\n\n" +
	"The endpoint merges, so this attribute only ever sends what you configure, including each part of `window` " +
	"on its own, and a setting you never name keeps whatever the project has. Each one you leave unset follows " +
	"Flightdeck's default until someone sets it. A write here bumps the project's `lock_version`.\n\n" +
	"Needs a Flightdeck that supports auto-promote. On an older one the attribute reads as null, and setting " +
	"anything in it fails with an error that asks you to upgrade Flightdeck."

// autoPromoteSchema is the resource attribute.
func autoPromoteSchema() schema.Attribute {
	hours := func(desc string, lower, upper int64) schema.Attribute {
		return schema.Int64Attribute{
			MarkdownDescription: desc + fmt.Sprintf(" A whole number from %d to %d. When unset, the project's current "+
				"value is kept.", lower, upper),
			Optional:      true,
			Computed:      true,
			Validators:    []validator.Int64{int64validator.Between(lower, upper)},
			PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
		}
	}
	timeOfDay := func(desc string, pattern *regexp.Regexp, rule string) schema.Attribute {
		return schema.StringAttribute{
			MarkdownDescription: desc,
			Optional:            true,
			Computed:            true,
			Validators:          []validator.String{stringvalidator.RegexMatches(pattern, rule)},
			PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
		}
	}
	return schema.SingleNestedAttribute{
		MarkdownDescription: autoPromoteDescription,
		Optional:            true,
		Computed:            true,
		PlanModifiers:       []planmodifier.Object{objectplanmodifier.UseStateForUnknown()},
		Attributes: map[string]schema.Attribute{
			"mode": schema.StringAttribute{
				MarkdownDescription: "Whether Flightdeck promotes release candidates by itself: `\"off\"` (the default) " +
					"never does, `\"deps_only\"` promotes only when every change waiting for production is a Dependabot " +
					"update, and `\"all\"` promotes any release candidate that passes every check. A plan that moves it " +
					"away from `\"off\"` carries a warning. A blocked mode is still accepted: the blockers are shown as " +
					"warnings instead. Flightdeck posts each change of mode to Slack (the project's channel, or the " +
					"workspace's for a project that isn't private). When unset, the project's current mode is kept, so " +
					"removing `mode`, or the whole `auto_promote` block, does not turn auto-promote off: set it to " +
					"`\"off\"`. A plan that leaves it unset while it is on warns that it stays on.",
				Optional:      true,
				Computed:      true,
				Validators:    []validator.String{stringvalidator.OneOf(client.AutoPromoteModes...)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"soak_hours": hours("How long a release candidate must run on stage before it may be promoted, in hours "+
				"(default 24).", autoPromoteMinSoakHours, autoPromoteMaxSoakHours),
			"window": schema.SingleNestedAttribute{
				MarkdownDescription: "When a promote may start. The default is Monday to Friday, 09:00 to 16:00, " +
					"`America/New_York`. Each part is sent only when configured, and Flightdeck merges it into the window " +
					"the project has, then checks the window as a whole, so setting only `start` can still be refused " +
					"for the `end` the project already has. A plan that sets both checks that `start` comes first. One " +
					"that sets only one checks it against the other: on a new project that is Flightdeck's default " +
					"window, and a window that would not fit fails the plan; on an existing project it is the value " +
					"stored at the last refresh, and a mismatch is a warning. When unset, the project's current window " +
					"is kept.",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Object{objectplanmodifier.UseStateForUnknown()},
				Attributes: map[string]schema.Attribute{
					"days": schema.SetAttribute{
						MarkdownDescription: "The days of the week a promote may start on: at least one of `" +
							joinBackticked(client.PromoteWindowDays) + "`. When unset, the current days are kept.",
						ElementType: types.StringType,
						Optional:    true,
						Computed:    true,
						Validators: []validator.Set{
							setvalidator.SizeAtLeast(1),
							setvalidator.ValueStringsAre(stringvalidator.OneOf(client.PromoteWindowDays...)),
						},
						PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()},
					},
					"start": timeOfDay("When the window opens each day, as 24-hour `HH:MM` with two-digit hours, from "+
						"`00:00` to `23:59`. It must come before `end`, since a window can't run past midnight. When unset, "+
						"the current start is kept.", promoteWindowStart,
						"must be a time of day as HH:MM with two-digit hours, from 00:00 to 23:59 (09:00, not 9:00)"),
					"end": timeOfDay("When the window closes each day, as 24-hour `HH:MM` with two-digit hours, up to "+
						"`24:00`, which is the end of the day. It must come after `start`. When unset, the current end is kept.",
						promoteWindowEnd, "must be a time of day as HH:MM with two-digit hours, from 00:00 to 24:00 (16:00, not 4pm)"),
					"time_zone": schema.StringAttribute{
						MarkdownDescription: "The time zone `start` and `end` are in, as an IANA name spelled exactly, " +
							"such as `America/New_York` (not `Eastern Time (US & Canada)`). Flightdeck checks the name. When " +
							"unset, the current time zone is kept.",
						Optional: true,
						Computed: true,
						Validators: []validator.String{stringvalidator.RegexMatches(promoteWindowTimeZone,
							"must be an IANA time zone name such as America/New_York, with no spaces")},
						PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
					},
				},
			},
			"freeze_dates": schema.SetAttribute{
				MarkdownDescription: "Days Flightdeck never promotes on, as ISO dates such as `\"2026-12-24\"`, at most " +
					strconv.Itoa(autoPromoteMaxFreezeDates) + ". A date that has passed stays until you remove it, so it " +
					"never shows as drift. `[]` clears the list. When unset, the project's current dates are kept.",
				ElementType: types.StringType,
				Optional:    true,
				Computed:    true,
				Validators: []validator.Set{
					setvalidator.SizeAtMost(autoPromoteMaxFreezeDates),
					setvalidator.ValueStringsAre(isoDateValidator{}),
				},
				PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()},
			},
			"heads_up_hours": hours("How long before a promote Flightdeck posts a heads-up in the project's Slack "+
				"channel, so someone can hold it, in hours (default 4).", autoPromoteMinHeadsUpHours, autoPromoteMaxHeadsUpHours),
		},
	}
}

// autoPromoteDataSourceSchema is the data source's read-only copy.
func autoPromoteDataSourceSchema() datasourceschema.Attribute {
	return datasourceschema.SingleNestedAttribute{
		MarkdownDescription: "Auto-promote settings (whether Flightdeck promotes the project's release candidates to " +
			"production by itself, and when), read from the project's `self-healing` API resource. Null unless the " +
			"token's user is a workspace owner or admin and the Flightdeck version supports auto-promote.",
		Computed: true,
		Attributes: map[string]datasourceschema.Attribute{
			"mode":       datasourceschema.StringAttribute{MarkdownDescription: "`\"off\"`, `\"deps_only\"` or `\"all\"`.", Computed: true},
			"soak_hours": datasourceschema.Int64Attribute{MarkdownDescription: "How long a release candidate must run on stage first, in hours.", Computed: true},
			"window": datasourceschema.SingleNestedAttribute{
				MarkdownDescription: "When a promote may start.",
				Computed:            true,
				Attributes: map[string]datasourceschema.Attribute{
					"days":      datasourceschema.SetAttribute{MarkdownDescription: "The days of the week a promote may start on.", ElementType: types.StringType, Computed: true},
					"start":     datasourceschema.StringAttribute{MarkdownDescription: "When the window opens each day (`HH:MM`).", Computed: true},
					"end":       datasourceschema.StringAttribute{MarkdownDescription: "When the window closes each day (`HH:MM`, up to `24:00`).", Computed: true},
					"time_zone": datasourceschema.StringAttribute{MarkdownDescription: "The IANA time zone of `start` and `end`.", Computed: true},
				},
			},
			"freeze_dates":   datasourceschema.SetAttribute{MarkdownDescription: "Days Flightdeck never promotes on (ISO dates).", ElementType: types.StringType, Computed: true},
			"heads_up_hours": datasourceschema.Int64Attribute{MarkdownDescription: "How long before a promote Flightdeck posts a heads-up, in hours.", Computed: true},
		},
	}
}

// isoDateValidator holds a freeze date to the form Flightdeck stores, so what
// passes is exactly what it reads back: YYYY-MM-DD with two-digit month and
// day, a year from 1 to 9999, and a real calendar date. Flightdeck would take
// "2026-1-5" and store "2026-01-05", which the next read would show as a
// change.
type isoDateValidator struct{}

func (isoDateValidator) Description(context.Context) string {
	return "must be an ISO date, YYYY-MM-DD, that is a real calendar date"
}

func (v isoDateValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (isoDateValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if problem := isoDateProblem(req.ConfigValue.ValueString()); problem != "" {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid freeze date", problem)
	}
}

// isoDatePattern is the stored form of a date.
var isoDatePattern = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)

// isoDateProblem says what is wrong with a freeze date, or "" when Flightdeck
// stores it exactly as written.
func isoDateProblem(s string) string {
	m := isoDatePattern.FindStringSubmatch(s)
	if m == nil {
		return fmt.Sprintf("%q is not an ISO date. Write it as YYYY-MM-DD, with a two-digit month and day, such as 2026-12-24.", s)
	}
	if year, _ := strconv.Atoi(m[1]); year < 1 {
		return fmt.Sprintf("%q has year 0. Flightdeck takes years from 1 to 9999.", s)
	}
	if _, err := time.Parse(time.DateOnly, s); err != nil {
		return fmt.Sprintf("%q is not a real calendar date.", s)
	}
	return ""
}

// windowMinutes is a canonical HH:MM as minutes after midnight.
func windowMinutes(hhmm string) int {
	h, _ := strconv.Atoi(hhmm[:2])
	m, _ := strconv.Atoi(hhmm[3:])
	return h*60 + m
}

// windowTimesKnown reports whether both times are known canonical HH:MM, so
// they can be compared. A value the plan refuses is left to that refusal.
func windowTimesKnown(start, end types.String) bool {
	return known(start) && known(end) && promoteWindowStart.MatchString(start.ValueString()) &&
		promoteWindowEnd.MatchString(end.ValueString())
}

// autoPromoteWindowOf reads the window out of an auto_promote object, and
// reports whether there was one.
func autoPromoteWindowOf(ctx context.Context, block types.Object, diags *diag.Diagnostics) (promoteWindowModel, bool) {
	var w promoteWindowModel
	if block.IsNull() || block.IsUnknown() {
		return w, false
	}
	var m autoPromoteModel
	diags.Append(block.As(ctx, &m, objectAsOptions)...)
	if diags.HasError() || m.Window.IsNull() || m.Window.IsUnknown() {
		return w, false
	}
	diags.Append(m.Window.As(ctx, &w, objectAsOptions)...)
	return w, !diags.HasError()
}

// validateAutoPromoteConfig refuses a window whose start is not before its
// end when the configuration sets both. Flightdeck checks the MERGED window,
// so a configuration that sets only one can still be refused by the other's
// stored value; warnAutoPromoteWindow looks for that.
func validateAutoPromoteConfig(ctx context.Context, block types.Object, diags *diag.Diagnostics) {
	w, ok := autoPromoteWindowOf(ctx, block, diags)
	if !ok || !windowTimesKnown(w.Start, w.End) {
		return
	}
	start, end := w.Start.ValueString(), w.End.ValueString()
	if windowMinutes(start) >= windowMinutes(end) {
		diags.AddAttributeError(path.Root("auto_promote").AtName("window").AtName("start"), "The promote window must start before it ends",
			fmt.Sprintf("window.start (%s) must be before window.end (%s). A window can't run past midnight; use 24:00 for "+
				"the end of the day.", start, end))
	}
}

// warnAutoPromoteWindow checks start < end against the MERGED window the plan
// carries when the configuration sets only one of them: the other is
// Optional+Computed with UseStateForUnknown, so the plan holds the stored
// value Flightdeck will merge into. It warns rather than errors because that
// stored value is only as current as the last refresh, the same reasoning as
// the self-healing windows.
func warnAutoPromoteWindow(ctx context.Context, configBlock, planBlock types.Object, diags *diag.Diagnostics) {
	cfg, ok := autoPromoteWindowOf(ctx, configBlock, diags)
	if !ok {
		return
	}
	planned, ok := autoPromoteWindowOf(ctx, planBlock, diags)
	if !ok {
		return
	}
	startSet, endSet := known(cfg.Start), known(cfg.End)
	if startSet == endSet || !windowTimesKnown(planned.Start, planned.End) {
		return
	}
	start, end := planned.Start.ValueString(), planned.End.ValueString()
	if windowMinutes(start) < windowMinutes(end) {
		return
	}
	at, set, setValue, held, heldValue := "start", "start", start, "end", end
	if endSet {
		at, set, setValue, held, heldValue = "end", "end", end, "start", start
	}
	diags.AddAttributeWarning(path.Root("auto_promote").AtName("window").AtName(at), "The promote window will not be coherent",
		fmt.Sprintf("This configuration sets window.%s to %s, and the project's stored window.%s is %s. A window must start "+
			"before it ends, and Flightdeck checks the window it would store, so it will refuse this with a 422 during "+
			"apply.\n\nSet both start and end in configuration. This is a warning rather than an error because the "+
			"stored value comes from the last refresh.", set, setValue, held, heldValue))
}

// The parts of Flightdeck's default window a partial window is merged into
// on a project that never set its own: a new one. If Flightdeck changes its
// default, a create that fits the new one but not these is refused here, at
// plan time, rather than accepted; that errs on the safe side, since the other
// way round the apply would fail after creating the project.
const (
	promoteWindowDefaultStart = "09:00"
	promoteWindowDefaultEnd   = "16:00"
)

// checkAutoPromoteWindowOnCreate refuses, at plan time, a new project's
// partial window that Flightdeck would refuse. A new project has no window of
// its own, so Flightdeck merges a window that sets only start, or only end,
// into its default one, and checks the result. Refused there, the write fails
// after the project is created, which leaves the project tainted, and every
// later apply would destroy and create it again. The configuration says all
// the plan needs to know, so this is an error rather than the warning an
// update gets (warnAutoPromoteWindow).
//
// A bound is set when the configuration does not leave it null, even if its
// value is not known until apply: Flightdeck gets that value, not the
// default. So a window that sets both bounds is left to
// validateAutoPromoteConfig, and one whose only set bound is not known yet
// has nothing to check.
func checkAutoPromoteWindowOnCreate(ctx context.Context, configBlock types.Object, diags *diag.Diagnostics) {
	cfg, ok := autoPromoteWindowOf(ctx, configBlock, diags)
	if !ok {
		return
	}
	startSet, endSet := !cfg.Start.IsNull(), !cfg.End.IsNull()
	if startSet == endSet || cfg.Start.IsUnknown() || cfg.End.IsUnknown() {
		return
	}
	start, end := types.StringValue(promoteWindowDefaultStart), types.StringValue(promoteWindowDefaultEnd)
	at, held, heldValue := "start", "end", promoteWindowDefaultEnd
	if startSet {
		start = cfg.Start
	} else {
		end = cfg.End
		at, held, heldValue = "end", "start", promoteWindowDefaultStart
	}
	if !windowTimesKnown(start, end) || windowMinutes(start.ValueString()) < windowMinutes(end.ValueString()) {
		return
	}
	setValue := start.ValueString()
	if endSet {
		setValue = end.ValueString()
	}
	diags.AddAttributeError(path.Root("auto_promote").AtName("window").AtName(at), "The promote window must start before it ends",
		fmt.Sprintf("This configuration sets only window.%s, to %s. A new project has no window of its own, so Flightdeck "+
			"fits that into its default window, whose %s is %s, and a window must start before it ends. Flightdeck would "+
			"refuse it after creating the project. Set window.%s too.", at, setValue, held, heldValue, held))
}

// warnAutoPromoteModeUnset warns when the configuration does not set
// auto_promote.mode (or auto_promote at all) while the project has it on.
// An unset setting keeps whatever the project has, so the plan is empty and
// auto-promote stays on, which is easy to read as having turned it off by
// removing it. Auto-promote is the one setting here that acts on production
// by itself, so the plan says so.
func warnAutoPromoteModeUnset(ctx context.Context, identifier types.String, configBlock, priorBlock types.Object, diags *diag.Diagnostics) {
	if priorBlock.IsNull() || priorBlock.IsUnknown() || configBlock.IsUnknown() {
		return
	}
	var prior autoPromoteModel
	diags.Append(priorBlock.As(ctx, &prior, objectAsOptions)...)
	if diags.HasError() || !known(prior.Mode) || prior.Mode.ValueString() == client.AutoPromoteOff {
		return
	}
	if !configBlock.IsNull() {
		var cfg autoPromoteModel
		diags.Append(configBlock.As(ctx, &cfg, objectAsOptions)...)
		if diags.HasError() || !cfg.Mode.IsNull() {
			return
		}
	}
	project := "this project"
	if known(identifier) {
		project = "project " + identifier.ValueString()
	}
	mode := prior.Mode.ValueString()
	diags.AddAttributeWarning(path.Root("auto_promote").AtName("mode"), "Auto-promote stays on for "+project,
		fmt.Sprintf("auto_promote.mode is not set in this configuration, so Flightdeck keeps it at %q for %s, and it "+
			"still promotes release candidates to production by itself. Leaving mode out does not turn auto-promote off. "+
			"Set mode = \"off\" to turn it off, or set mode = %q to keep it on and stop this warning.",
			mode, project, mode))
}

// autoPromoteActs is what the plan-time warning says Flightdeck will do in a
// mode that is on.
func autoPromoteActs(mode string) string {
	only := ""
	if mode == client.AutoPromoteDepsOnly {
		only = " In \"deps_only\" it does so only when every change waiting for production is a Dependabot update."
	}
	return "Flightdeck will then promote this project's release candidates to production by itself once every " +
		"check passes, among them the soak time on stage, the promote window and freeze dates, and auto-rollback " +
		"being on as its safety net." + only + " A Flightdeck that can't promote yet lists not_yet_available for " +
		"every project, and the setting takes effect once promoting ships. Anything that stands in the way is " +
		"shown as a warning after the apply."
}

// warnAutoPromote is the plan-time warning for an apply that moves the mode
// away from "off": to "deps_only" or "all", or possibly so (a value not known
// until apply). priorBlock is null on create, and on a Flightdeck that did
// not report auto-promote. The summary names the project, because Terraform
// folds warnings that share a summary into one.
func warnAutoPromote(ctx context.Context, identifier types.String, configBlock, priorBlock, planBlock types.Object, diags *diag.Diagnostics) {
	if planBlock.IsNull() {
		return
	}
	var cfg, planned, prior autoPromoteModel
	if !planBlock.IsUnknown() {
		diags.Append(planBlock.As(ctx, &planned, objectAsOptions)...)
	}
	if !priorBlock.IsNull() && !priorBlock.IsUnknown() {
		diags.Append(priorBlock.As(ctx, &prior, objectAsOptions)...)
	}
	configured := !configBlock.IsNull() && !configBlock.IsUnknown()
	if configured {
		diags.Append(configBlock.As(ctx, &cfg, objectAsOptions)...)
	}
	if diags.HasError() {
		return
	}
	isOn := func(v types.String) bool { return known(v) && v.ValueString() != client.AutoPromoteOff }
	if isOn(prior.Mode) {
		return
	}
	project := "this project"
	if known(identifier) {
		project = "project " + identifier.ValueString()
	}
	at := path.Root("auto_promote").AtName("mode")
	switch {
	case configBlock.IsUnknown() || (configured && cfg.Mode.IsUnknown()):
		diags.AddAttributeWarning(at, "This apply may turn on auto-promote for "+project,
			fmt.Sprintf("auto_promote.mode for %s is not known until apply. If it is \"deps_only\" or \"all\", this apply "+
				"turns on auto-promote. %s", project, autoPromoteActs(client.AutoPromoteAll)))
	case configured && isOn(planned.Mode):
		mode := planned.Mode.ValueString()
		diags.AddAttributeWarning(at, "This apply turns on auto-promote for "+project,
			fmt.Sprintf("This apply sets auto_promote.mode for %s to %q. %s", project, mode, autoPromoteActs(mode)))
	}
}

// warnAutoPromoteBlockers reports, one warning each, what would stop
// auto-promote on a project whose mode is not "off" right now. They are
// warnings rather than state because they change as the project does. Each
// summary names the project and the blocker's code, because Terraform folds
// warnings that share a summary into one.
//
// Auto-promote's list passes every rollback blocker through. While the
// rollback mode is "auto", warnRollbackBlockers has already warned of each of
// those, saying it stops auto-promote too, so they are not repeated here; the
// codes auto-promote has of its own never reuse a rollback blocker's.
func warnAutoPromoteBlockers(sh *client.SelfHealing, identifier string, diags *diag.Diagnostics) {
	if !autoPromoteOn(sh) {
		return
	}
	warnedAsRollback := map[string]bool{}
	if sh.RollbackMode() == client.RollbackAuto {
		for _, b := range sh.RollbackBlockers {
			warnedAsRollback[b.Code] = true
		}
	}
	seen := map[string]int{}
	for _, b := range sh.AutoPromoteBlockers {
		if warnedAsRollback[b.Code] {
			continue
		}
		seen[b.Code]++
		summary := fmt.Sprintf("Auto-promote cannot act on project %s right now: %s", identifier, b.Code)
		if n := seen[b.Code]; n > 1 {
			summary += fmt.Sprintf(" (%d)", n)
		}
		diags.AddAttributeWarning(path.Root("auto_promote").AtName("mode"), summary,
			b.Message+"\n\nThis project is set to auto-promote, and this is one of the things that would stop it "+
				"promoting now. Flightdeck checks again every time the settings are read, so this is reported on each "+
				"refresh and apply rather than stored in state.")
	}
}

// autoPromoteOn reports whether an answer says auto-promote is on.
func autoPromoteOn(sh *client.SelfHealing) bool {
	return sh != nil && sh.ReportsAutoPromote() && *sh.AutoPromote != client.AutoPromoteOff
}

// autoPromoteToObject maps the API's auto-promote settings into the
// attribute: null when the answer is missing or comes from a Flightdeck
// without auto-promote.
func autoPromoteToObject(sh *client.SelfHealing, diags *diag.Diagnostics) types.Object {
	if sh == nil || !sh.ReportsAutoPromote() {
		return types.ObjectNull(autoPromoteAttrTypes)
	}
	window := types.ObjectNull(promoteWindowAttrTypes)
	if w := sh.PromoteWindow; w != nil {
		obj, d := types.ObjectValue(promoteWindowAttrTypes, map[string]attr.Value{
			"days":      stringSet(w.Days),
			"start":     types.StringValue(w.Start),
			"end":       types.StringValue(w.End),
			"time_zone": types.StringValue(w.TimeZone),
		})
		diags.Append(d...)
		window = obj
	}
	obj, d := types.ObjectValue(autoPromoteAttrTypes, map[string]attr.Value{
		"mode":           types.StringValue(*sh.AutoPromote),
		"soak_hours":     types.Int64PointerValue(sh.SoakHours),
		"window":         window,
		"freeze_dates":   stringSet(sh.FreezeDates),
		"heads_up_hours": types.Int64PointerValue(sh.HeadsUpHours),
	})
	diags.Append(d...)
	return obj
}

// stringSet is a set of strings; nil is the empty set.
func stringSet(values []string) types.Set {
	elems := make([]attr.Value, 0, len(values))
	for _, v := range values {
		elems = append(elems, types.StringValue(v))
	}
	return types.SetValueMust(types.StringType, elems)
}

// knownStrings returns a set's strings, and false when the set or any element
// is not known.
func knownStrings(ctx context.Context, set types.Set, diags *diag.Diagnostics) ([]string, bool) {
	if !known(set) {
		return nil, false
	}
	for _, e := range set.Elements() {
		if e.IsUnknown() {
			return nil, false
		}
	}
	out := []string{}
	diags.Append(set.ElementsAs(ctx, &out, false)...)
	return out, !diags.HasError()
}

// autoPromoteFields returns the settings to send, under the API's keys, from
// the CONFIGURED attribute, or nil when the configuration does not set it.
// Only known, non-null settings are sent, and only the configured parts of
// the window. Days go in week order and freeze dates sorted, the order
// Flightdeck stores them in, so a refusal's freeze_dates[i] names the i-th
// date sent.
func autoPromoteFields(ctx context.Context, block types.Object, diags *diag.Diagnostics) client.Fields {
	if block.IsNull() || block.IsUnknown() {
		return nil
	}
	var m autoPromoteModel
	diags.Append(block.As(ctx, &m, objectAsOptions)...)
	if diags.HasError() {
		return nil
	}
	fields := client.Fields{}
	if known(m.Mode) {
		fields["auto_promote"] = m.Mode.ValueString()
	}
	if known(m.SoakHours) {
		fields["soak_hours"] = m.SoakHours.ValueInt64()
	}
	if known(m.HeadsUpHours) {
		fields["heads_up_hours"] = m.HeadsUpHours.ValueInt64()
	}
	if dates, ok := knownStrings(ctx, m.FreezeDates, diags); ok {
		sort.Strings(dates)
		fields["freeze_dates"] = dates
	}
	if known(m.Window) {
		var w promoteWindowModel
		diags.Append(m.Window.As(ctx, &w, objectAsOptions)...)
		parts := map[string]any{}
		if days, ok := knownStrings(ctx, w.Days, diags); ok {
			parts["days"] = daysInWeekOrder(days)
		}
		for key, v := range map[string]types.String{"start": w.Start, "end": w.End, "time_zone": w.TimeZone} {
			if known(v) {
				parts[key] = v.ValueString()
			}
		}
		// An empty window names no part, which Flightdeck reads as no opinion.
		if len(parts) > 0 {
			fields["promote_window"] = parts
		}
	}
	return fields
}

// daysInWeekOrder returns days in client.PromoteWindowDays order. A day the
// API does not list (the plan refuses one) goes last, in name order.
func daysInWeekOrder(days []string) []string {
	rank := func(day string) int {
		for i, d := range client.PromoteWindowDays {
			if d == day {
				return i
			}
		}
		return len(client.PromoteWindowDays)
	}
	out := append([]string{}, days...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(out[i]), rank(out[j])
		if ri != rj {
			return ri < rj
		}
		return out[i] < out[j]
	})
	return out
}

// autoPromoteRefusal finds the field a 422 names at the start of its message,
// in the API's spelling: auto_promote, soak_hours, heads_up_hours,
// freeze_dates or freeze_dates[i], promote_window or promote_window.<part>.
// The name must end there, so auto_promote_blockers is not auto_promote; the
// character after it is matched rather than a word boundary, which there is
// none of after freeze_dates[i].
var autoPromoteRefusal = regexp.MustCompile(`^(auto_promote|soak_hours|heads_up_hours|freeze_dates(?:\[(\d+)\])?|promote_window(?:\.(days|start|end|time_zone))?)(?:[^A-Za-z0-9_.\[]|$)`)

// autoPromoteRefusedAt maps a refused write to the attribute it is about,
// when the refusal names an auto-promote setting the write sent. The API's
// names differ from the attribute's for the mode (`auto_promote`) and the
// window (`promote_window`). A freeze date is named by its place in the list
// sent, which is sorted, so it maps back to that date. A window that does not
// start before it ends is named as the start's problem; when the write sent
// only the end, it lands on the end. ok reports whether it found one.
func autoPromoteRefusedAt(sent client.Fields, err error) (at path.Path, name string, ok bool) {
	if !client.HasCode(err, client.CodeInvalidAttribute) {
		return path.Empty(), "", false
	}
	msg := apiMessage(err)
	m := autoPromoteRefusal.FindStringSubmatch(msg)
	if m == nil {
		return path.Empty(), "", false
	}
	root := path.Root("auto_promote")
	field := m[1]
	switch {
	case field == "auto_promote":
		at, name = root.AtName("mode"), "mode"
	case field == "soak_hours" || field == "heads_up_hours":
		at, name = root.AtName(field), field
	case m[2] != "" || field == "freeze_dates":
		at, name = root.AtName("freeze_dates"), "freeze_dates"
		if dates, isList := sent["freeze_dates"].([]string); isList && m[2] != "" {
			if i, convErr := strconv.Atoi(m[2]); convErr == nil && i < len(dates) {
				at = at.AtSetValue(types.StringValue(dates[i]))
			}
		}
	default:
		at, name = root.AtName("window"), "window"
		if part := m[3]; part != "" {
			window, _ := sent["promote_window"].(map[string]any)
			_, startSent := window["start"]
			_, endSent := window["end"]
			if part == "start" && !startSent && endSent {
				part = "end"
			}
			at, name = at.AtName(part), "window."+part
		}
	}
	wire := field
	if i := strings.IndexAny(field, ".["); i >= 0 {
		wire = field[:i]
	}
	if _, sentIt := sent[wire]; !sentIt {
		return path.Empty(), "", false
	}
	return at, name, true
}
