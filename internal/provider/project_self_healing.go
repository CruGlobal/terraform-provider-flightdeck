package provider

import (
	"context"
	"fmt"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/float64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/float64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The self_healing block rides its own API resource, GET/PATCH
// /api/v1/projects/:project_id/self-healing, workspace-admin for both. It is
// modelled as a block on flightdeck_project because it configures that
// project, but its transport is separate: the read nests the resolved values
// under `config`, the write takes the threshold keys, and the If-Match is the
// PROJECT's lock_version (the jsonb lives on the project row, so a write here
// bumps it). `armed` is read-only: the API refuses a write that would change
// it with code arming_refused. Reversing that is a two-line change here (make
// `armed` Optional and include it in selfHealingFields).

// selfHealingModel is the Terraform shape of the `config` object.
type selfHealingModel struct {
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

// selfHealingSchema is the resource attribute. Every threshold is optional
// and computed (the server supplies the documented default when unset);
// `armed` is computed only.
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
			"for other tokens, and on a Flightdeck version without the endpoint, the block is null. Thresholds you " +
			"leave unset take the server's documented defaults. `armed` is read-only: arming a project is a " +
			"console-only operation and the API refuses a write that would change it. `short_window_minutes` must " +
			"not exceed `long_window_minutes`. A write here bumps the project's `lock_version`.",
		Optional: true,
		Computed: true,
		PlanModifiers: []planmodifier.Object{
			objectplanmodifier.UseStateForUnknown(),
		},
		Attributes: map[string]schema.Attribute{
			"armed": schema.BoolAttribute{
				MarkdownDescription: "Whether live rollback (as opposed to shadow mode) is armed for this project. Read-only; set from the console.",
				Computed:            true,
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
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

// selfHealingToObject maps the API's resolved config into the block.
func selfHealingToObject(sh *client.SelfHealing, diags *diag.Diagnostics) types.Object {
	if sh == nil {
		return types.ObjectNull(selfHealingAttrTypes)
	}
	cfg := sh.Config
	obj, d := types.ObjectValue(selfHealingAttrTypes, map[string]attr.Value{
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

// selfHealingFields returns the thresholds to send from the CONFIGURED block,
// or nil when the configuration has no block (null/unknown). Only known,
// non-null thresholds are sent; `armed` never is.
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

// readSelfHealing fetches the block for a project. A 403 (token is not a
// workspace admin) or a 404 (project gone between calls, or a Flightdeck
// without the endpoint) leaves the block null; anything else is an error.
func readSelfHealing(ctx context.Context, c *client.Client, projectID int64, diags *diag.Diagnostics) types.Object {
	sh, err := c.GetSelfHealing(ctx, projectID)
	if err != nil {
		if client.IsForbidden(err) || client.IsNotFound(err) {
			return types.ObjectNull(selfHealingAttrTypes)
		}
		addAPIError(diags, "Error reading Flightdeck self-healing configuration", err)
		return types.ObjectNull(selfHealingAttrTypes)
	}
	return selfHealingToObject(sh, diags)
}

// writeSelfHealing PATCHes the configured thresholds (if any) with the
// project's current lock_version and returns the block plus the project's
// new lock_version. With no configured block it just reads.
func writeSelfHealing(ctx context.Context, c *client.Client, projectID int64, configBlock types.Object, lockVersion int64, diags *diag.Diagnostics) (types.Object, int64) {
	settings := selfHealingFields(ctx, configBlock, diags)
	if diags.HasError() {
		return types.ObjectNull(selfHealingAttrTypes), lockVersion
	}
	if len(settings) == 0 {
		return readSelfHealing(ctx, c, projectID, diags), lockVersion
	}
	sh, err := c.UpdateSelfHealing(ctx, projectID, settings, lockVersion)
	if err != nil {
		switch {
		case client.IsNotFound(err):
			diags.AddAttributeError(path.Root("self_healing"), "Self-healing configuration is not available on this Flightdeck",
				"The project exists but its self-healing endpoint answered 404, so this Flightdeck version does not expose "+
					"self-healing over the API yet. Remove the block or upgrade Flightdeck. "+apiMessage(err))
		case client.IsForbidden(err):
			diags.AddAttributeError(path.Root("self_healing"), "Self-healing configuration requires a workspace admin",
				"Only a workspace owner or admin may read or write a project's self-healing thresholds. "+apiMessage(err))
		case client.HasCode(err, client.CodeArmingRefused):
			diags.AddAttributeError(path.Root("self_healing").AtName("armed"), "Arming is console-only", apiMessage(err))
		case client.IsStale(err):
			addStaleError(diags, "Project self-healing configuration", lockVersion, nil, err)
		default:
			addAPIError(diags, "Error updating Flightdeck self-healing configuration", err)
		}
		return types.ObjectNull(selfHealingAttrTypes), lockVersion
	}
	return selfHealingToObject(sh, diags), sh.LockVersion
}
