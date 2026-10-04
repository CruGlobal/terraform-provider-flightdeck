package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/float64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/float64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The self_healing block rides its own API resource, GET/PATCH
// /api/v1/projects/:project_id/self-healing, workspace-admin for both. It is
// modelled as a block on flightdeck_project because it configures that
// project, but its transport is separate: the read nests the resolved values
// under `config` with `feature_enabled` and `rollback` alongside it, the write
// takes the writable keys flat, and the If-Match is the PROJECT's lock_version
// (the jsonb lives on the project row, so a write here bumps it).
//
// Two settings decide whether the loop acts, and both are settable here.
// `feature_enabled` puts the project in front of the loop at all. `rollback`
// is its mode: "report" (report only: it notes what it would do and never
// touches production) or "auto" (auto-rollback: it rolls production back by
// itself when every check passes, and hands the problem to a person when one
// fails). The loop acts only when both say so and Flightdeck's global kill
// switch, which nothing on the API can move, is off. `armed` is the API's
// old name for the mode and stays a computed, deprecated mirror of it: a
// change sent through `armed` is refused, and the provider never sends it.
// On a Flightdeck that predates `rollback`, the mode is derived from
// config.armed instead.
//
// Turning auto-rollback on is a plan-time warning, and the API's
// `rollback_blockers` (what would stop auto-rollback right now) are warnings
// on every refresh and apply while the mode is "auto", never state: they
// change with every deploy, so storing them would churn.
//
// The endpoint MERGES: an absent key keeps its stored value, so only the
// configured settings are ever sent and an unmanaged threshold is never
// disturbed. The corollary is the usual one — an attribute left out of
// configuration is not managed, and a console change to it wins silently.
// `features.self_healing` on PATCH /projects/:id is refused (422); this
// endpoint is the only way in.

// selfHealingModel is the Terraform shape of the block: the `config` object
// plus the `feature_enabled` switch that rides beside it.
type selfHealingModel struct {
	FeatureEnabled        types.Bool    `tfsdk:"feature_enabled"`
	Rollback              types.String  `tfsdk:"rollback"`
	CountBrowserErrors    types.Bool    `tfsdk:"count_browser_errors"`
	Armed                 types.Bool    `tfsdk:"armed"`
	BakeMinutes           types.Int64   `tfsdk:"bake_minutes"`
	BaselineMultiplier    types.Float64 `tfsdk:"baseline_multiplier"`
	AbsoluteFloor         types.Float64 `tfsdk:"absolute_floor"`
	LongWindowMinutes     types.Int64   `tfsdk:"long_window_minutes"`
	ShortWindowMinutes    types.Int64   `tfsdk:"short_window_minutes"`
	BurnRate              types.Float64 `tfsdk:"burn_rate"`
	SustainCount          types.Int64   `tfsdk:"sustain_count"`
	ConsecutiveErrorLimit types.Int64   `tfsdk:"consecutive_error_limit"`
	CooldownMinutes       types.Int64   `tfsdk:"cooldown_minutes"`
	MaxRollbacksPerHour   types.Int64   `tfsdk:"max_rollbacks_per_hour"`
	RecoveryWindowMinutes types.Int64   `tfsdk:"recovery_window_minutes"`
}

var selfHealingAttrTypes = map[string]attr.Type{
	"feature_enabled":         types.BoolType,
	"rollback":                types.StringType,
	"count_browser_errors":    types.BoolType,
	"armed":                   types.BoolType,
	"bake_minutes":            types.Int64Type,
	"baseline_multiplier":     types.Float64Type,
	"absolute_floor":          types.Float64Type,
	"long_window_minutes":     types.Int64Type,
	"short_window_minutes":    types.Int64Type,
	"burn_rate":               types.Float64Type,
	"sustain_count":           types.Int64Type,
	"consecutive_error_limit": types.Int64Type,
	"cooldown_minutes":        types.Int64Type,
	"max_rollbacks_per_hour":  types.Int64Type,
	"recovery_window_minutes": types.Int64Type,
}

// Limits mirror the API's per-setting ranges. Non-positive values are refused
// because the engine reads them as "no limit" (or "act on the first error"),
// never as "off".
const (
	maxMinutes = 1440
	maxCount   = 100
	maxRatio   = 1000
	maxFloor   = 100000
)

// selfHealingSchema is the resource attribute. Every threshold and switch is
// optional and computed (the server supplies the documented default when
// unset); `armed` is computed only. None of them carries a framework
// default: a default would manufacture a value for an unset attribute and
// write it, which on a merging endpoint silently overrides whatever the
// project already had.
func selfHealingSchema() schema.Attribute {
	intThreshold := func(desc string, upper int64) schema.Attribute {
		return schema.Int64Attribute{
			MarkdownDescription: desc + fmt.Sprintf(" Must be between 1 and %d; the API refuses non-positive values because the engine treats them as \"no limit\".", upper),
			Optional:            true,
			Computed:            true,
			Validators:          []validator.Int64{int64validator.Between(1, upper)},
			PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
		}
	}
	floatThreshold := func(desc string, upper float64) schema.Attribute {
		return schema.Float64Attribute{
			MarkdownDescription: desc + fmt.Sprintf(" Must be greater than 0 and at most %g; the API refuses non-positive values because the engine treats them as \"no limit\".", upper),
			Optional:            true,
			Computed:            true,
			Validators:          []validator.Float64{positiveFloat64{}, float64validator.AtMost(upper)},
			PlanModifiers:       []planmodifier.Float64{float64planmodifier.UseStateForUnknown()},
		}
	}
	return schema.SingleNestedAttribute{
		MarkdownDescription: "Self-healing (automated rollback) control-loop configuration, managed through the project's " +
			"`self-healing` API resource. Reading and writing it requires the token's user to be a **workspace admin**; " +
			"for other tokens, and on a Flightdeck version without the endpoint, the block is null.\n\n" +
			"Two settings decide whether the loop acts, and both are settable here. `feature_enabled` puts the project " +
			"in front of the loop at all. `rollback` is its mode: `\"report\"` (**report only**: it notes what it would " +
			"do and never touches production) or `\"auto\"` (**auto-rollback**: it rolls production back by itself when " +
			"every check passes, and pages a person when it can't). The loop acts only when `feature_enabled` is on, " +
			"`rollback` is `\"auto\"`, and Flightdeck's global kill switch is off. A plan that turns auto-rollback on " +
			"carries a warning saying so, and while `rollback` is `\"auto\"` each thing that would stop it right now " +
			"is shown as a warning on refresh and after apply. `armed` is the old name for the mode, kept read-only " +
			"and deprecated.\n\n" +
			"The endpoint merges, so this block only ever sends what you configure: a threshold you leave unset keeps " +
			"whatever the project has (the server's documented default, until someone overrides it), and a setting you " +
			"never name is never disturbed — including one changed in the console. `short_window_minutes` must not " +
			"exceed `long_window_minutes`, and the API checks that against the merged result — so a write naming only " +
			"one of them can still be refused by the other's stored value. Setting both incoherently fails the plan; " +
			"raising one past a stored value the configuration does not mention is a plan-time **warning**, since the " +
			"stored value is only as current as the last refresh. A write here bumps the project's `lock_version`. The `self_healing` key is refused in `features`; it is spelled " +
			"`feature_enabled` here.",
		Optional: true,
		Computed: true,
		PlanModifiers: []planmodifier.Object{
			objectplanmodifier.UseStateForUnknown(),
		},
		Attributes: map[string]schema.Attribute{
			"feature_enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether the self-healing control loop runs for this project at all. With `rollback` at " +
					"`\"report\"` (report only), turning this on starts the observation, not the automation: the loop notes " +
					"what it would do and never touches production, although it can still notify people and file work. " +
					"With `rollback` at `\"auto\"`, turning this on is going live, and Flightdeck refuses it unless the same " +
					"apply sets `rollback`, so a project stored as auto-rollback needs `rollback` in configuration before " +
					"this can turn it on. Setting this to `false` pauses the loop and leaves `rollback` where it is. When " +
					"unset, the project's current value is kept, so a switch flipped in the console survives an apply; set " +
					"it explicitly, even to `false`, for Terraform to own it.",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"rollback": schema.StringAttribute{
				MarkdownDescription: "The loop's mode: `\"report\"` for **report only**, where it notes what it would do and " +
					"never touches production, or `\"auto\"` for **auto-rollback**, where Flightdeck rolls production back " +
					"by itself when every check passes and pages a person when it can't. It acts only while " +
					"`feature_enabled` is on. A plan that moves this to `\"auto\"` carries a warning saying so. While it is " +
					"`\"auto\"`, each thing that would stop auto-rollback right now (the API's `rollback_blockers`, such as " +
					"a release that is not marked safe to roll back) is shown as a warning on refresh and after apply rather " +
					"than stored, because it changes with every deploy. `\"auto\"` is accepted even while something blocks " +
					"it. When unset, the project's current mode is kept, so a mode changed in the console survives an " +
					"apply; set it explicitly for Terraform to own it. Needs a Flightdeck that supports the setting; an " +
					"older one reports the mode through `armed` only, and a configured value fails the apply.",
				Optional:      true,
				Computed:      true,
				Validators:    []validator.String{stringvalidator.OneOf(client.RollbackModes...)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"count_browser_errors": schema.BoolAttribute{
				MarkdownDescription: "Whether errors sent with a browser token count toward the error rate the loop acts on, " +
					"and toward the baseline a release is compared against. The server's default is `false`, so only " +
					"errors sent with a server token count: a browser token ships inside every page, so anyone can read it " +
					"and post fake errors to push the rate over the trigger. Either way, browser errors still show on the " +
					"Errors pages and fire any error alert rule that counts them (see `flightdeck_error_alert_rule`'s " +
					"`condition.count_browser_errors`). When unset, the project's current value is kept. Null on a Flightdeck that predates the " +
					"setting, where a configured value fails the apply.",
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"armed": schema.BoolAttribute{
				MarkdownDescription: "Whether auto-rollback is on: the API's `armed` flag, true exactly when `rollback` is " +
					"`\"auto\"`. Read-only. Deprecated: read and set `rollback` instead.",
				DeprecationMessage: "Use `rollback` instead. `armed` is true exactly when `rollback` is \"auto\", and the mode is set through `rollback`.",
				Computed:           true,
				PlanModifiers:      []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"bake_minutes":            intThreshold("Eligibility window after a deploy, in minutes (default 20).", maxMinutes),
			"baseline_multiplier":     floatThreshold("Post-deploy error rate must be at least this multiple of the baseline (default 5.0).", maxRatio),
			"absolute_floor":          floatThreshold("…and at least this many errors per minute, guarding a near-zero baseline (default 5.0).", maxFloor),
			"long_window_minutes":     intThreshold("Long burn-rate window in minutes (default 60).", maxMinutes),
			"short_window_minutes":    intThreshold("Short burn-rate window in minutes (default 5).", maxMinutes),
			"burn_rate":               floatThreshold("Multi-window burn rate that counts as severe (default 14.4).", maxRatio),
			"sustain_count":           intThreshold("Consecutive trips required before acting (default 3).", maxCount),
			"consecutive_error_limit": intThreshold("Metrics-query failures tolerated before a decision is inconclusive (default 3).", maxCount),
			"cooldown_minutes":        intThreshold("No action on the same app within this window, in minutes (default 30).", maxMinutes),
			"max_rollbacks_per_hour":  intThreshold("Per-app blast-radius cap (default 1).", maxCount),
			"recovery_window_minutes": intThreshold("Grace period after a rollback before a still-severe signal escalates, in minutes (default 15).", maxMinutes),
		},
	}
}

// positiveFloat64 requires a value strictly greater than zero.
type positiveFloat64 struct{}

func (positiveFloat64) Description(context.Context) string { return "must be greater than 0" }
func (v positiveFloat64) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (positiveFloat64) ValidateFloat64(_ context.Context, req validator.Float64Request, resp *validator.Float64Response) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if req.ConfigValue.ValueFloat64() <= 0 {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid threshold",
			fmt.Sprintf("must be greater than 0, got %g. The API refuses non-positive values because the engine treats them as \"no limit\".", req.ConfigValue.ValueFloat64()))
	}
}

// validateSelfHealingConfig checks the cross-field rule the API enforces
// (short window <= long window) when both sides are known at plan time.
//
// The API checks the same rule against the MERGED result, so it can still
// refuse a write that names only one window because of the other's stored
// value. Terraform cannot see that stored value at plan time — a configuration
// omitting a threshold is saying nothing about it — so that case surfaces as
// an apply-time 422 rather than being guessed at here.
func validateSelfHealingConfig(ctx context.Context, block types.Object, diags *diag.Diagnostics) {
	if block.IsNull() || block.IsUnknown() {
		return
	}
	var m selfHealingModel
	diags.Append(block.As(ctx, &m, objectAsOptions)...)
	if m.ShortWindowMinutes.IsNull() || m.ShortWindowMinutes.IsUnknown() || m.LongWindowMinutes.IsNull() || m.LongWindowMinutes.IsUnknown() {
		return
	}
	if m.ShortWindowMinutes.ValueInt64() > m.LongWindowMinutes.ValueInt64() {
		diags.AddAttributeError(path.Root("self_healing").AtName("short_window_minutes"), "Incoherent burn-rate windows",
			fmt.Sprintf("short_window_minutes (%d) cannot exceed long_window_minutes (%d); the severity gate compares a short window against a longer one.",
				m.ShortWindowMinutes.ValueInt64(), m.LongWindowMinutes.ValueInt64()))
	}
}

// warnSelfHealingWindows checks short <= long against the MERGED pair, which
// the PLAN carries: a threshold dropped from configuration is Optional+Computed
// with UseStateForUnknown, so the plan fills it from prior state — the same
// value the API will merge the write into. That catches the case
// validateSelfHealingConfig cannot see, where a configuration raises one window
// past a stored value it never mentions.
//
// It WARNS rather than errors, on purpose. The merged value is only as current
// as the last refresh, and a plan run with -refresh=false can carry a stale one
// — so an error here could block an apply the API would have accepted, and
// nothing at plan time can tell a stale value from a current one. A warning
// surfaces the problem before the apply without standing in front of a valid
// one, which is how this provider handles the rest of its can't-be-certain
// cases. When BOTH windows are configured there is no staleness question and
// validateSelfHealingConfig still errors.
func warnSelfHealingWindows(ctx context.Context, configBlock, planBlock types.Object, diags *diag.Diagnostics) {
	if configBlock.IsNull() || configBlock.IsUnknown() || planBlock.IsNull() || planBlock.IsUnknown() {
		return
	}
	var cfg, planned selfHealingModel
	diags.Append(configBlock.As(ctx, &cfg, objectAsOptions)...)
	diags.Append(planBlock.As(ctx, &planned, objectAsOptions)...)
	if diags.HasError() {
		return
	}
	configured := func(v types.Int64) bool { return !v.IsNull() && !v.IsUnknown() }
	shortSet, longSet := configured(cfg.ShortWindowMinutes), configured(cfg.LongWindowMinutes)
	// Both configured is already an error; neither configured changes nothing.
	if shortSet == longSet {
		return
	}
	if !configured(planned.ShortWindowMinutes) || !configured(planned.LongWindowMinutes) {
		return
	}
	shortW, longW := planned.ShortWindowMinutes.ValueInt64(), planned.LongWindowMinutes.ValueInt64()
	if shortW <= longW {
		return
	}
	// Each name is carried with its own value. Swapping labels while the
	// values stayed in positional order is exactly how this sentence came to
	// report the wrong number for the stored window.
	from, fromValue := "short_window_minutes", shortW
	held, heldValue := "long_window_minutes", longW
	if longSet {
		from, fromValue = "long_window_minutes", longW
		held, heldValue = "short_window_minutes", shortW
	}
	diags.AddAttributeWarning(path.Root("self_healing").AtName(from), "Self-healing burn-rate windows will not be coherent",
		fmt.Sprintf("This configuration sets %s to %d, and the project's stored %s is %d. A short window may not "+
			"exceed a long one, and the API checks that against the merged pair rather than against what a write "+
			"names, so it will refuse this with a 422 during apply.\n\n"+
			"Set both windows in configuration, or leave %s where the stored value allows. This is a warning rather "+
			"than an error because the stored value comes from the last refresh: if it changed since, the apply may "+
			"well succeed.", from, fromValue, held, heldValue, from))
}

// planArmed keeps the deprecated `armed` in step with `rollback` in a plan.
// `armed` is computed with UseStateForUnknown, so a plan that changes the mode
// would otherwise promise the prior `armed` and the apply would end in an
// inconsistent result. The API documents `armed` as true exactly when
// `rollback` is "auto", so a known planned mode decides it, and a mode not
// known until apply makes it unknown too: UseStateForUnknown will already have
// filled it with the prior value, which the apply may not keep.
func planArmed(ctx context.Context, planBlock types.Object, diags *diag.Diagnostics) types.Object {
	if planBlock.IsNull() || planBlock.IsUnknown() {
		return planBlock
	}
	var m selfHealingModel
	diags.Append(planBlock.As(ctx, &m, objectAsOptions)...)
	if diags.HasError() || m.Rollback.IsNull() {
		return planBlock
	}
	if m.Rollback.IsUnknown() {
		m.Armed = types.BoolUnknown()
	} else {
		m.Armed = types.BoolValue(m.Rollback.ValueString() == client.RollbackAuto)
	}
	obj, d := types.ObjectValueFrom(ctx, selfHealingAttrTypes, m)
	diags.Append(d...)
	return obj
}

// warnAutoRollback is the plan-time warning for an apply that turns
// auto-rollback on: one that moves `rollback` to "auto", or one that turns
// `feature_enabled` on while `rollback` is or becomes "auto" (going live).
// priorBlock is null on create. Each summary names the project, because
// Terraform folds warnings that share a summary into one.
//
// Three cases get their own words. Going live over a stored "auto" without
// `rollback` in configuration is a write Flightdeck refuses, so the warning
// says that instead of promising rollbacks. A `rollback` configured from a
// value not known until apply MAY turn it on. And a mode moved to "auto"
// while the feature is not known to be on does not act yet.
func warnAutoRollback(ctx context.Context, identifier types.String, configBlock, priorBlock, planBlock types.Object, diags *diag.Diagnostics) {
	if planBlock.IsNull() || planBlock.IsUnknown() {
		return
	}
	var cfg, planned, prior selfHealingModel
	diags.Append(planBlock.As(ctx, &planned, objectAsOptions)...)
	if !priorBlock.IsNull() && !priorBlock.IsUnknown() {
		diags.Append(priorBlock.As(ctx, &prior, objectAsOptions)...)
	}
	if !configBlock.IsNull() && !configBlock.IsUnknown() {
		diags.Append(configBlock.As(ctx, &cfg, objectAsOptions)...)
	}
	if diags.HasError() {
		return
	}
	isAuto := func(v types.String) bool {
		return !v.IsNull() && !v.IsUnknown() && v.ValueString() == client.RollbackAuto
	}
	isOn := func(v types.Bool) bool { return !v.IsNull() && !v.IsUnknown() && v.ValueBool() }
	liveBefore := isAuto(prior.Rollback) && isOn(prior.FeatureEnabled)

	project := "this project"
	if !identifier.IsNull() && !identifier.IsUnknown() {
		project = "project " + identifier.ValueString()
	}
	at := path.Root("self_healing").AtName("rollback")
	const acts = "Flightdeck will roll production back by itself when every check passes, and page a person when it can't."

	// Configured, but from a value the plan cannot see. (An unset rollback
	// is unknown on create too, and that means the server's default, report.)
	if cfg.Rollback.IsUnknown() {
		if !liveBefore {
			diags.AddAttributeWarning(at, "This apply may turn on auto-rollback for "+project,
				fmt.Sprintf("self_healing.rollback for %s is not known until apply. If it is \"auto\" and feature_enabled "+
					"is on, this apply turns on auto-rollback: %s", project, acts))
		}
		return
	}

	modeToAuto := isAuto(planned.Rollback) && !isAuto(prior.Rollback)
	liveAfter := isAuto(planned.Rollback) && isOn(planned.FeatureEnabled)
	goingLive := liveAfter && !liveBefore
	switch {
	case !modeToAuto && !goingLive:
		return
	case goingLive && !modeToAuto && cfg.Rollback.IsNull():
		// The write names feature_enabled but not rollback, over a stored
		// "auto": the API's go-live guard refuses it.
		// A Flightdeck from before `rollback` has no such guard and takes
		// the write, so the warning says what happens there too.
		diags.AddAttributeWarning(path.Root("self_healing").AtName("feature_enabled"),
			"Flightdeck will refuse this apply for "+project+" unless rollback is set",
			fmt.Sprintf("%s is stored as auto-rollback, and this apply turns feature_enabled on without naming "+
				"rollback. That would start live rollbacks without anyone saying so, and Flightdeck refuses it. Set "+
				"self_healing.rollback to \"auto\" to go live with auto-rollback, or \"report\" to start in report "+
				"only. This is a warning rather than an error because the stored mode comes from the last refresh.\n\n"+
				"A Flightdeck from before the rollback setting has no such guard: there this apply turns on "+
				"auto-rollback, and %s To start in report only there, leave feature_enabled off and change the "+
				"mode in the console first.", capitalize(project), acts))
	case liveAfter:
		diags.AddAttributeWarning(at, "This apply turns on auto-rollback for "+project,
			fmt.Sprintf("This apply turns on auto-rollback for %s. %s", project, acts))
	case planned.FeatureEnabled.IsUnknown():
		diags.AddAttributeWarning(at, "This apply turns on auto-rollback for "+project,
			fmt.Sprintf("This apply sets %s to auto-rollback. It acts only while feature_enabled is on, which this plan "+
				"cannot show yet; once it is on, %s", project, acts))
	default:
		diags.AddAttributeWarning(at, "This apply turns on auto-rollback for "+project,
			fmt.Sprintf("This apply sets %s to auto-rollback. Nothing acts while feature_enabled is off; once it is on, %s", project, acts))
	}
}

// warnRollbackBlockers reports, one warning each, what would stop
// auto-rollback on a project whose mode is "auto" right now. They are
// warnings rather than state because they change with every deploy. Each
// summary names the project and the blocker's code: Terraform folds warnings
// that share a summary into one, which would hide every message but the first.
func warnRollbackBlockers(sh *client.SelfHealing, identifier string, diags *diag.Diagnostics) {
	if sh == nil || sh.RollbackMode() != client.RollbackAuto {
		return
	}
	seen := map[string]int{}
	for _, b := range sh.RollbackBlockers {
		seen[b.Code]++
		summary := fmt.Sprintf("Auto-rollback cannot act on project %s right now: %s", identifier, b.Code)
		if n := seen[b.Code]; n > 1 {
			summary += fmt.Sprintf(" (%d)", n)
		}
		diags.AddAttributeWarning(path.Root("self_healing").AtName("rollback"), summary,
			b.Message+"\n\nThis project is set to auto-rollback, and this is one of the things that would stop it "+
				"acting now. Flightdeck checks again with every deploy, so this is reported on each refresh and "+
				"apply rather than stored in state.")
	}
}

// selfHealingToObject maps the API's resolved config into the block.
func selfHealingToObject(sh *client.SelfHealing, diags *diag.Diagnostics) types.Object {
	if sh == nil {
		return types.ObjectNull(selfHealingAttrTypes)
	}
	cfg := sh.Config
	obj, d := types.ObjectValue(selfHealingAttrTypes, map[string]attr.Value{
		// feature_enabled and rollback ride alongside `config` in the response,
		// not inside it; rollback falls back to config.armed on an older server.
		"feature_enabled":         types.BoolValue(sh.FeatureEnabled),
		"rollback":                types.StringValue(sh.RollbackMode()),
		"count_browser_errors":    types.BoolPointerValue(cfg.CountBrowserErrors),
		"armed":                   types.BoolValue(cfg.Armed),
		"bake_minutes":            types.Int64Value(cfg.BakeMinutes),
		"baseline_multiplier":     types.Float64Value(cfg.BaselineMultiplier),
		"absolute_floor":          types.Float64Value(cfg.AbsoluteFloor),
		"long_window_minutes":     types.Int64Value(cfg.LongWindowMinutes),
		"short_window_minutes":    types.Int64Value(cfg.ShortWindowMinutes),
		"burn_rate":               types.Float64Value(cfg.BurnRate),
		"sustain_count":           types.Int64Value(cfg.SustainCount),
		"consecutive_error_limit": types.Int64Value(cfg.ConsecutiveErrorLimit),
		"cooldown_minutes":        types.Int64Value(cfg.CooldownMinutes),
		"max_rollbacks_per_hour":  types.Int64Value(cfg.MaxRollbacksPerHour),
		"recovery_window_minutes": types.Int64Value(cfg.RecoveryWindowMinutes),
	})
	diags.Append(d...)
	return obj
}

// selfHealingFields returns the settings to send from the CONFIGURED block, or
// nil when the configuration has no block (null/unknown). Only known, non-null
// settings are sent, which is what makes the merge safe; `armed` never is.
// `rollback` goes in the same write as `feature_enabled`, which is what lets a
// project stored as auto-rollback be turned on at all.
func selfHealingFields(ctx context.Context, block types.Object, diags *diag.Diagnostics) client.Fields {
	if block.IsNull() || block.IsUnknown() {
		return nil
	}
	var m selfHealingModel
	diags.Append(block.As(ctx, &m, objectAsOptions)...)
	fields := client.Fields{}
	putInt := func(key string, v types.Int64) {
		if !v.IsNull() && !v.IsUnknown() {
			fields[key] = v.ValueInt64()
		}
	}
	putFloat := func(key string, v types.Float64) {
		if !v.IsNull() && !v.IsUnknown() {
			fields[key] = v.ValueFloat64()
		}
	}
	putBool := func(key string, v types.Bool) {
		if !v.IsNull() && !v.IsUnknown() {
			fields[key] = v.ValueBool()
		}
	}
	putBool("feature_enabled", m.FeatureEnabled)
	if !m.Rollback.IsNull() && !m.Rollback.IsUnknown() {
		fields["rollback"] = m.Rollback.ValueString()
	}
	putBool("count_browser_errors", m.CountBrowserErrors)
	putInt("bake_minutes", m.BakeMinutes)
	putFloat("baseline_multiplier", m.BaselineMultiplier)
	putFloat("absolute_floor", m.AbsoluteFloor)
	putInt("long_window_minutes", m.LongWindowMinutes)
	putInt("short_window_minutes", m.ShortWindowMinutes)
	putFloat("burn_rate", m.BurnRate)
	putInt("sustain_count", m.SustainCount)
	putInt("consecutive_error_limit", m.ConsecutiveErrorLimit)
	putInt("cooldown_minutes", m.CooldownMinutes)
	putInt("max_rollbacks_per_hour", m.MaxRollbacksPerHour)
	putInt("recovery_window_minutes", m.RecoveryWindowMinutes)
	return fields
}

// readSelfHealing fetches the block for a project, and the API's answer for
// callers that report on it. A 403 (token is not a workspace admin) or a 404
// (project gone between calls, or a Flightdeck without the endpoint) leaves
// the block null and the answer nil; anything else is an error.
func readSelfHealing(ctx context.Context, c *client.Client, projectID int64, diags *diag.Diagnostics) (types.Object, *client.SelfHealing) {
	sh, err := c.GetSelfHealing(ctx, projectID)
	if err != nil {
		if client.IsForbidden(err) || client.IsNotFound(err) {
			return types.ObjectNull(selfHealingAttrTypes), nil
		}
		addAPIError(diags, "Error reading Flightdeck self-healing configuration", err)
		return types.ObjectNull(selfHealingAttrTypes), nil
	}
	return selfHealingToObject(sh, diags), sh
}

// writeSelfHealing PATCHes the configured settings (if any) with the
// project's current lock_version and returns the block plus the project's
// new lock_version. With no configured block it just reads. Either way, the
// result's rollback blockers are reported as warnings while the mode is
// "auto"; identifier names the project in them.
func writeSelfHealing(ctx context.Context, c *client.Client, projectID int64, identifier string, configBlock types.Object, lockVersion int64, diags *diag.Diagnostics) (types.Object, int64) {
	settings := selfHealingFields(ctx, configBlock, diags)
	if diags.HasError() {
		return types.ObjectNull(selfHealingAttrTypes), lockVersion
	}
	if len(settings) == 0 {
		block, sh := readSelfHealing(ctx, c, projectID, diags)
		warnRollbackBlockers(sh, identifier, diags)
		return block, lockVersion
	}
	sh, err := c.UpdateSelfHealing(ctx, projectID, settings, lockVersion)
	if err != nil {
		// A refused write that named a setting this Flightdeck is too old for
		// says so; any other refusal falls through to the cases below.
		if client.IsValidation(err) && addUnsupportedSettingError(ctx, c, projectID, settings, err, diags) {
			return types.ObjectNull(selfHealingAttrTypes), lockVersion
		}
		if addIfProjectGone(ctx, c, projectID, identifier, err, diags) {
			return types.ObjectNull(selfHealingAttrTypes), lockVersion
		}
		switch {
		case client.IsNotFound(err):
			diags.AddAttributeError(path.Root("self_healing"), "Self-healing configuration is not available on this Flightdeck",
				"The project exists but its self-healing endpoint answered 404, so this Flightdeck version does not expose "+
					"self-healing over the API yet. Remove the block or upgrade Flightdeck. "+apiMessage(err))
		case client.IsForbidden(err):
			diags.AddAttributeError(path.Root("self_healing"), "Self-healing configuration requires a workspace admin",
				"Only a workspace owner or admin may read or write a project's self-healing thresholds. "+apiMessage(err))
		case client.HasCode(err, client.CodeArmingRefused):
			diags.AddAttributeError(path.Root("self_healing").AtName("feature_enabled"), "Set rollback to turn this project's self-healing on",
				"This project is stored as auto-rollback, so turning feature_enabled on would start live rollbacks, and "+
					"Flightdeck refuses that unless the same write says which mode to go live in. Set "+
					"self_healing.rollback explicitly: \"auto\" to go live with auto-rollback, or \"report\" to start in "+
					"report only. Nothing was written. "+apiMessage(err))
		case client.IsStale(err):
			addStaleError(diags, "Project self-healing configuration", lockVersion, nil, err)
		default:
			addAPIError(diags, "Error updating Flightdeck self-healing configuration", err)
		}
		return types.ObjectNull(selfHealingAttrTypes), lockVersion
	}
	warnRollbackBlockers(sh, identifier, diags)
	return selfHealingToObject(sh, diags), sh.LockVersion
}

// selfHealingNewerSettings are the settings an older Flightdeck refuses as
// unknown. Each is configured as the attribute of the same name.
var selfHealingNewerSettings = []string{"rollback", "count_browser_errors"}

// addUnsupportedSettingError explains a refused write that named a setting
// this Flightdeck is too old to know. The API's refusal of an unknown key is
// prose, so rather than match on its wording, a fresh read is asked what the
// endpoint accepts. It reports whether it added a diagnostic; false leaves the
// error to the caller's generic handling.
func addUnsupportedSettingError(ctx context.Context, c *client.Client, projectID int64, sent client.Fields, err error, diags *diag.Diagnostics) bool {
	var missing []string
	for _, key := range selfHealingNewerSettings {
		if _, named := sent[key]; named {
			missing = append(missing, key)
		}
	}
	if len(missing) == 0 {
		return false
	}
	sh, rerr := c.GetSelfHealing(ctx, projectID)
	if rerr != nil {
		return false
	}
	// A setting is supported when the read reports it and writable_settings
	// does not leave it out.
	reported := map[string]bool{
		"rollback":             sh.Rollback != nil,
		"count_browser_errors": sh.Config.CountBrowserErrors != nil,
	}
	added := false
	for _, key := range missing {
		if reported[key] && sh.Accepts(key) {
			continue
		}
		diags.AddAttributeError(path.Root("self_healing").AtName(key), "This Flightdeck is too old for self_healing."+key,
			fmt.Sprintf("The Flightdeck at this endpoint does not accept %s on its self-healing API, so it cannot be set "+
				"from Terraform there. Remove %s from the configuration, or upgrade Flightdeck. Nothing was written. %s",
				key, key, apiMessage(err)))
		added = true
	}
	return added
}

// capitalize upper-cases the first letter, for a phrase that opens a sentence.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
