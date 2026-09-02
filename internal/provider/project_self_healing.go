package provider

import (
	"context"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/float64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The self_healing block is the provider's one provisional surface. The
// Flightdeck decision (FD-789) is: the resolved config is readable by
// workspace admins, the thresholds are writable by workspace admins, and
// `armed` is refused on write so arming stays console-only. The block is
// modelled so that reversing that decision is a two-line change: make `armed`
// Optional in selfHealingSchema and include it in selfHealingFields.

// selfHealingModel is the Terraform shape of SelfHealing::Config#to_h.
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

// selfHealingSchema is the resource attribute. Every threshold is optional
// and computed (the server supplies the documented default when unset);
// `armed` is computed only.
func selfHealingSchema() schema.Attribute {
	intThreshold := func(desc string) schema.Attribute {
		return schema.Int64Attribute{
			MarkdownDescription: desc,
			Optional:            true,
			Computed:            true,
			PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
		}
	}
	floatThreshold := func(desc string) schema.Attribute {
		return schema.Float64Attribute{
			MarkdownDescription: desc,
			Optional:            true,
			Computed:            true,
			PlanModifiers:       []planmodifier.Float64{float64planmodifier.UseStateForUnknown()},
		}
	}
	return schema.SingleNestedAttribute{
		MarkdownDescription: "Self-healing (automated rollback) control-loop configuration. Reading and writing this " +
			"block requires the token's user to be a **workspace admin**; for other tokens it is null. Thresholds you " +
			"leave unset take the server's documented defaults. `armed` is read-only: arming a project is a " +
			"console-only operation, and the API refuses a write to it. The `self_healing` *feature* flag is likewise " +
			"not settable through the API.",
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
			"bake_minutes":            intThreshold("Eligibility window after a deploy, in minutes (default 20)."),
			"baseline_multiplier":     floatThreshold("Post-deploy error rate must be at least this multiple of the baseline (default 5.0)."),
			"absolute_floor":          floatThreshold("…and at least this many errors per minute, guarding a near-zero baseline (default 5.0)."),
			"long_window_minutes":     intThreshold("Long burn-rate window in minutes (default 60)."),
			"short_window_minutes":    intThreshold("Short burn-rate window in minutes (default 5)."),
			"burn_rate":               floatThreshold("Multi-window burn rate that counts as severe (default 14.4)."),
			"sustain_count":           intThreshold("Consecutive trips required before acting (default 3)."),
			"consecutive_error_limit": intThreshold("Metrics-query failures tolerated before a decision is inconclusive (default 3)."),
			"cooldown_minutes":        intThreshold("No action on the same app within this window, in minutes (default 30)."),
			"max_rollbacks_per_hour":  intThreshold("Per-app blast-radius cap (default 1)."),
			"recovery_window_minutes": intThreshold("Grace period after a rollback before a still-severe signal escalates, in minutes (default 15)."),
		},
	}
}

// selfHealingToObject maps the API's resolved config into the block; nil
// (not reported to this token) becomes a null object.
func selfHealingToObject(cfg *client.SelfHealingConfig, diags *diag.Diagnostics) types.Object {
	if cfg == nil {
		return types.ObjectNull(selfHealingAttrTypes)
	}
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

// selfHealingFields returns the thresholds to send, or nil when the block is
// not configured (null/unknown). Only known, non-null thresholds are sent;
// `armed` never is.
func selfHealingFields(ctx context.Context, block types.Object, diags *diag.Diagnostics) map[string]any {
	if block.IsNull() || block.IsUnknown() {
		return nil
	}
	var m selfHealingModel
	diags.Append(block.As(ctx, &m, objectAsOptions)...)
	fields := map[string]any{}
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
