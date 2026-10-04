package provider

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// apiBlank is what the API strips from a string before asking whether it is
// blank, and before measuring a text filter: ASCII white space and NUL, as
// Ruby's String#strip does.
const apiBlank = "\x00\t\n\v\f\r "

// notBlank refuses a value that holds nothing but blank space. The API reads
// such a value as "no opinion" or as empty, so it would never read back the
// way it was configured. Blank here is any Unicode white space and NUL, which
// covers both what the API strips and what its presence checks treat as
// empty. The string is what to do instead, for the message.
type notBlank string

func (v notBlank) Description(context.Context) string { return "must not be blank" }

func (v notBlank) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (v notBlank) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if isBlank(req.ConfigValue.ValueString()) {
		resp.Diagnostics.AddAttributeError(req.Path, "Blank value", "This value must not be blank; "+string(v)+".")
	}
}

// isBlank reports a string with nothing in it but white space and NUL.
func isBlank(s string) bool {
	return strings.TrimFunc(s, func(r rune) bool { return r == 0 || unicode.IsSpace(r) }) == ""
}

// trimmedLengthAtMost caps a string at n characters, not counting the blank
// space the API strips from either end before it measures. The API counts
// characters (Unicode code points), not bytes.
type trimmedLengthAtMost int

func (v trimmedLengthAtMost) Description(context.Context) string {
	return fmt.Sprintf("at most %d characters, not counting blank space at either end", int(v))
}

func (v trimmedLengthAtMost) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v trimmedLengthAtMost) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if n := utf8.RuneCountInString(strings.Trim(req.ConfigValue.ValueString(), apiBlank)); n > int(v) {
		resp.Diagnostics.AddAttributeError(req.Path, "Value too long",
			fmt.Sprintf("This value is %d characters long; Flightdeck accepts at most %d here, not counting blank space at either end.", n, int(v)))
	}
}
