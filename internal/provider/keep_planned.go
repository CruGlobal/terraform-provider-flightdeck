package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// plannedWhereConfigured returns what a write to one of a project's settings
// endpoints sends, as the block its fields are built from: for each setting
// the configuration sets, the PLAN's value, and null for every other.
//
// The configuration decides WHETHER a setting is sent: only one it sets,
// because the endpoints merge, and an unset setting plans the value in state,
// which sending would put in front of a Flightdeck too old for it, or (for
// agent_work's label and account ids) clear on a null. The plan decides WHAT
// is sent: it equals the configuration, except under `lifecycle {
// ignore_changes = [...] }`, where it is the value in state. Terraform applies
// ignore_changes to the plan only, never to the configuration it hands the
// apply, so sending the configuration there would overwrite a value changed in
// Flightdeck's web app and fail the apply with "Provider produced inconsistent
// result". This is the rule flightdeck_github_integration's ci_workflows
// follows (ciWorkflowsToSend), with one difference: for a value the plan does
// not know, ciWorkflowsToSend sends nothing and this sends the configuration.
// An apply never reaches that case, because Terraform plans again during the
// apply, with the configuration fully known.
//
// A setting the configuration sets but the plan holds as null is not sent
// either. That is a setting ignored while state has no value for it, as on a
// Flightdeck older than the setting, and sending nothing leaves it as the plan
// says. A whole block the plan holds as null (ignored while it read as null)
// sends nothing, so the result is null too. A value the plan does not know is
// sent from the configuration, as before, since Terraform takes any result for
// it. A nested object is matched part by part.
//
// Passed in place of the configuration to the keep-as-planned helpers
// (keepUnconfiguredAsPlanned, keepUnsetAsPlanned), it also says which
// settings went unsent, so their planned values are kept.
func plannedWhereConfigured(ctx context.Context, config, planned types.Object, diags *diag.Diagnostics) types.Object {
	if config.IsNull() || config.IsUnknown() || planned.IsUnknown() {
		return config
	}
	if planned.IsNull() {
		return types.ObjectNull(config.AttributeTypes(ctx))
	}
	plannedAttrs := planned.Attributes()
	out := make(map[string]attr.Value, len(config.Attributes()))
	for name, set := range config.Attributes() {
		want, inPlan := plannedAttrs[name]
		switch {
		case set.IsNull() || set.IsUnknown() || !inPlan || want.IsUnknown():
			out[name] = set
		case want.IsNull():
			out[name] = nullOf(ctx, set.Type(ctx), diags)
		default:
			setObj, isObj := set.(types.Object)
			wantObj, wantIsObj := want.(types.Object)
			if isObj && wantIsObj {
				out[name] = plannedWhereConfigured(ctx, setObj, wantObj, diags)
				continue
			}
			out[name] = want
		}
	}
	obj, d := types.ObjectValue(config.AttributeTypes(ctx), out)
	diags.Append(d...)
	if diags.HasError() {
		return types.ObjectNull(config.AttributeTypes(ctx))
	}
	return obj
}

// nullOf is the null value of an attribute type, custom types included.
func nullOf(ctx context.Context, typ attr.Type, diags *diag.Diagnostics) attr.Value {
	null, err := typ.ValueFromTerraform(ctx, tftypes.NewValue(typ.TerraformType(ctx), nil))
	if err != nil {
		diags.AddError("Error building a null value", err.Error())
	}
	return null
}

// keepUnconfiguredAsPlanned returns the value an apply leaves for an
// Optional+Computed object attribute whose settings are sent only when
// configured, such as self_healing and auto_promote.
//
// A setting the configuration leaves null is never sent, so the apply
// changes nothing about it, and the plan holds the value from state
// (UseStateForUnknown). What Flightdeck reports after the write can still
// differ: a release that changes a default changes every project that never
// set the key, without moving any lock_version, and one that adds or drops a
// setting between the plan and the apply reports a value where the plan held
// null, or null where it held a value. A saved plan applied later makes that
// window real. Terraform refuses a result that differs from a known planned
// value ("Provider produced inconsistent result after apply"), so for each
// attribute the configuration leaves null, and each part of a nested object
// it sets, the planned value is kept when the plan knew it; the next refresh
// adopts Flightdeck's, which plans nothing because the configuration does not
// set it.
//
// Everything the configuration sets is taken as Flightdeck reports it, so a
// value Flightdeck did not honor still fails the apply. So is everything the
// plan did not know: Terraform refuses an apply result that still holds an
// unknown value. A configuration that names nothing at all (no attribute, or
// one whose every part is null) sent nothing, so the planned value is kept
// whole, null included, when the plan knew all of it. On a create it did not:
// a configured object that sets nothing, such as `self_healing = {}`, is
// planned as a known object whose every part is unknown, so each part is
// taken from Flightdeck like any other the plan did not know.
func keepUnconfiguredAsPlanned(ctx context.Context, config, planned, result types.Object, diags *diag.Diagnostics) types.Object {
	if planned.IsUnknown() || config.IsUnknown() {
		return result
	}
	if namesNothing(config) && fullyKnown(ctx, planned) {
		return planned
	}
	if planned.IsNull() || result.IsNull() || result.IsUnknown() {
		return result
	}
	configured, plannedAttrs := config.Attributes(), planned.Attributes()
	out := make(map[string]attr.Value, len(result.Attributes()))
	for name, got := range result.Attributes() {
		want, inPlan := plannedAttrs[name]
		set, inConfig := configured[name]
		switch {
		case !inPlan || want.IsUnknown():
			out[name] = got
		case (!inConfig || set.IsNull()) && fullyKnown(ctx, want):
			out[name] = want
		default:
			// A part the configuration sets, or one it leaves null whose plan
			// holds something unknown inside: an object is merged part by
			// part, and anything else is taken as Flightdeck reports it.
			setObj, isObj := set.(types.Object)
			wantObj, _ := want.(types.Object)
			gotObj, _ := got.(types.Object)
			if isObj && !gotObj.IsNull() && !wantObj.IsNull() {
				out[name] = keepUnconfiguredAsPlanned(ctx, setObj, wantObj, gotObj, diags)
				continue
			}
			out[name] = got
		}
	}
	obj, d := types.ObjectValue(result.AttributeTypes(ctx), out)
	diags.Append(d...)
	return obj
}

// namesNothing reports whether a configured object sets nothing: it is null,
// or every attribute in it is null or an object that sets nothing. A value
// not known until apply names something.
func namesNothing(config types.Object) bool {
	if config.IsNull() {
		return true
	}
	if config.IsUnknown() {
		return false
	}
	for _, v := range config.Attributes() {
		if v.IsNull() {
			continue
		}
		if obj, ok := v.(types.Object); ok && namesNothing(obj) {
			continue
		}
		return false
	}
	return true
}

// fullyKnown reports whether v and everything inside it is known, at any
// depth and in any kind of collection. A value that cannot be converted to
// check counts as not known, which takes Flightdeck's answer for it.
func fullyKnown(ctx context.Context, v attr.Value) bool {
	tfValue, err := v.ToTerraformValue(ctx)
	return err == nil && tfValue.IsFullyKnown()
}
