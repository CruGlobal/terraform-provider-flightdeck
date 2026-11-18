package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Flightdeck reads a timestamp as an instant and writes it back in its own
// zone, in whole seconds: `2027-06-01T00:00:00Z` comes back as
// `2027-05-31T20:00:00-04:00`. Compared as text, the two would show as a
// change on every plan, and for an attribute that forces replacement, as a
// new resource. These helpers compare them as times instead, and keep the
// spelling the configuration used.

// parseInstant parses an RFC 3339 timestamp. Go's parser takes a fraction of
// a second too, though the layout has none.
func parseInstant(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// sameInstant reports whether two RFC 3339 timestamps name the same instant,
// to the second, since Flightdeck writes whole seconds and drops any
// fraction. Two values that are not both timestamps are compared as text.
func sameInstant(a, b string) bool {
	ta, okA := parseInstant(a)
	tb, okB := parseInstant(b)
	if !okA || !okB {
		return a == b
	}
	return ta.Truncate(time.Second).Equal(tb.Truncate(time.Second))
}

// keepSpellingOfSameInstant returns the value state should hold for a
// timestamp the server reported as reported: the one already held (the
// configuration's spelling) when both name the same instant, and the
// server's otherwise, because then the time itself changed.
func keepSpellingOfSameInstant(held types.String, reported *string) types.String {
	if reported == nil || *reported == "" {
		return types.StringNull()
	}
	if !held.IsNull() && !held.IsUnknown() && sameInstant(held.ValueString(), *reported) {
		return held
	}
	return types.StringValue(*reported)
}

// rfc3339Timestamp refuses a value that is not an RFC 3339 timestamp with a
// time zone. Flightdeck also takes a date alone, or a time with no zone, but
// it reads either in its own zone, which the provider cannot know, so the
// instant would be a guess. It also refuses a zone offset of 24 hours or more
// either way, such as +24:00 or +23:60: Go's parser takes one, and Flightdeck
// refuses it.
type rfc3339Timestamp struct{}

// maxZoneOffset is the smallest zone offset, either way, that Flightdeck
// refuses: Ruby's Time takes an offset of less than a day.
const maxZoneOffset = 24 * time.Hour

// zoneOffsetTooBig reports whether t's zone offset is a day or more either
// way.
func zoneOffsetTooBig(t time.Time) bool {
	_, offset := t.Zone()
	d := time.Duration(offset) * time.Second
	return d >= maxZoneOffset || d <= -maxZoneOffset
}

func (rfc3339Timestamp) Description(context.Context) string {
	return "an RFC 3339 timestamp with a time zone, such as 2027-06-01T00:00:00Z"
}

func (v rfc3339Timestamp) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (rfc3339Timestamp) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	value := req.ConfigValue.ValueString()
	t, ok := parseInstant(value)
	if !ok {
		resp.Diagnostics.AddAttributeError(req.Path, "Not an RFC 3339 timestamp",
			fmt.Sprintf("%q is not an RFC 3339 timestamp. Write a date, a time and a time zone, such as "+
				"2027-06-01T00:00:00Z or 2027-06-01T09:00:00-05:00.", value))
		return
	}
	if zoneOffsetTooBig(t) {
		resp.Diagnostics.AddAttributeError(req.Path, "Time zone offset out of range",
			fmt.Sprintf("%q has a time zone offset of 24 hours or more. Flightdeck takes an offset of less than "+
				"24 hours either way, such as -05:00 or +09:30. The same time in UTC is %q.",
				value, t.UTC().Format(time.RFC3339)))
	}
}

// keepStateWhenSameInstant plans the value in state, rather than the
// configuration's, when both name the same instant. That is the case after
// an import (state holds the server's spelling) or when the configuration
// changes only how it writes the time. Terraform accepts a planned value
// that equals the prior one in place of the configuration's, as the
// provider's word that the two mean the same thing, and then nothing
// changes. It must come before RequiresReplace, which then sees no change.
type keepStateWhenSameInstant struct{}

func (keepStateWhenSameInstant) Description(context.Context) string {
	return "keeps the value in state when the configuration names the same instant"
}

func (v keepStateWhenSameInstant) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (keepStateWhenSameInstant) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.StateValue.IsNull() || req.StateValue.IsUnknown() || req.PlanValue.IsNull() || req.PlanValue.IsUnknown() {
		return
	}
	if req.PlanValue.ValueString() != req.StateValue.ValueString() &&
		sameInstant(req.PlanValue.ValueString(), req.StateValue.ValueString()) {
		resp.PlanValue = req.StateValue
	}
}
