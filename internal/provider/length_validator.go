package provider

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// charLengthAtMost caps a string at n characters (Unicode code points), the
// way Flightdeck counts them. stringvalidator.LengthAtMost counts bytes, so it
// would refuse a name in a non-Latin script that Flightdeck accepts.
type charLengthAtMost int

func (v charLengthAtMost) Description(context.Context) string {
	return fmt.Sprintf("at most %d characters", int(v))
}

func (v charLengthAtMost) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (v charLengthAtMost) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if n := utf8.RuneCountInString(req.ConfigValue.ValueString()); n > int(v) {
		resp.Diagnostics.AddAttributeError(req.Path, "Value too long",
			fmt.Sprintf("This value is %d characters long; Flightdeck accepts at most %d.", n, int(v)))
	}
}
