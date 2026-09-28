package provider

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Flightdeck replaced modules with epics, and two sets of names followed: the
// project feature key (modules -> epics) and the webhook events (module.* ->
// epic.*). The API still takes the old names on a write for now, but never
// reports them back. The two are handled differently because state behaves
// differently: a feature key the provider can keep under its old name (see
// renamedFeatures), but a webhook's events are a Required set the read must
// reproduce exactly, so an old event name can only be refused.

// featureKeys validates the keys of flightdeck_project.features: each must be
// settable, a renamed key is accepted with a deprecation warning, and naming a
// renamed key together with its replacement is an error. It is one validator
// rather than a OneOf plus a separate check, so an unknown key's error lists
// only the current names.
type featureKeys struct{}

func (featureKeys) Description(context.Context) string {
	return "keys must be settable feature keys: " + strings.Join(toggleableFeatures, ", ")
}

func (v featureKeys) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (featureKeys) ValidateMap(_ context.Context, req validator.MapRequest, resp *validator.MapResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	elements := req.ConfigValue.Elements()
	keys := make([]string, 0, len(elements))
	for k := range elements {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		at := req.Path.AtMapKey(key)
		if slices.Contains(toggleableFeatures, key) {
			continue
		}
		if replacement, renamed := renamedFeatures[key]; renamed {
			if _, both := elements[replacement]; both {
				resp.Diagnostics.AddAttributeError(at, "Feature named twice",
					fmt.Sprintf("%q is the old name of %q, and both are set. Keep only %q.", key, replacement, replacement))
				continue
			}
			resp.Diagnostics.AddAttributeWarning(at, "Feature renamed",
				fmt.Sprintf("The %q feature is now %q: Flightdeck replaced %s with %s. The provider sends this key as %q, "+
					"so it keeps working, but rename it in features.", key, replacement, key, replacement, replacement))
			continue
		}
		if where, elsewhere := featuresSetElsewhere[key]; elsewhere {
			resp.Diagnostics.AddAttributeError(at, "Feature not settable in features",
				fmt.Sprintf("The %q feature is set as %s, not in features.", key, where))
			continue
		}
		resp.Diagnostics.AddAttributeError(at, "Invalid feature key",
			fmt.Sprintf("%q is not a settable feature. Settable keys: %s.", key, strings.Join(toggleableFeatures, ", ")))
	}
}

// featuresSetElsewhere are feature keys the API reports but settable only on
// another endpoint, with the attribute that sets each.
var featuresSetElsewhere = map[string]string{
	"self_healing": "self_healing.feature_enabled",
	"slack":        "slack_channel.notifications_enabled",
}

// renamedWebhookEvents maps an old webhook event to the one that replaced it.
var renamedWebhookEvents = map[string]string{
	"module.created": "epic.created",
	"module.updated": "epic.updated",
	"module.deleted": "epic.deleted",
}

// webhookEvents validates flightdeck_webhook.events. A renamed event is
// refused with its new name: Flightdeck stores it under the new name and
// reads it back that way, so keeping the old one would end every apply in an
// inconsistent result.
type webhookEvents struct{}

func (webhookEvents) Description(context.Context) string {
	return "each event must be one of: " + strings.Join(client.WebhookEvents, ", ")
}

func (v webhookEvents) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (webhookEvents) ValidateSet(_ context.Context, req validator.SetRequest, resp *validator.SetResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	for _, element := range req.ConfigValue.Elements() {
		event, ok := element.(types.String)
		if !ok || event.IsNull() || event.IsUnknown() {
			continue
		}
		name := event.ValueString()
		at := req.Path.AtSetValue(event)
		switch replacement, renamed := renamedWebhookEvents[name]; {
		case slices.Contains(client.WebhookEvents, name):
		case renamed:
			resp.Diagnostics.AddAttributeError(at, "Webhook event renamed",
				fmt.Sprintf("The %q event is now %q: Flightdeck replaced module events with epic events. It stores %q "+
					"as %q and reports it back that way, so the old name cannot be kept in configuration. Rename it.",
					name, replacement, name, replacement))
		default:
			resp.Diagnostics.AddAttributeError(at, "Invalid webhook event",
				fmt.Sprintf("%q is not a webhook event. Known events: %s.", name, strings.Join(client.WebhookEvents, ", ")))
		}
	}
}
