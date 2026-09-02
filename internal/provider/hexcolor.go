package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// hexColorPattern mirrors State::HEX_COLOR / Label::HEX_COLOR.
var hexColorPattern = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// hexColorType is a string type whose values compare by colour rather than by
// spelling: `#ABC`, `#abc` and `#aabbcc` are the same colour. A server that
// stores a canonical form (lower-case, six digits) then reads back as equal to
// whatever the configuration wrote, so the apply is consistent and no
// perpetual diff appears.
type hexColorType struct {
	basetypes.StringType
}

var _ basetypes.StringTypable = hexColorType{}

func (t hexColorType) Equal(o attr.Type) bool {
	_, ok := o.(hexColorType)
	return ok
}

func (t hexColorType) String() string { return "hexColorType" }

func (t hexColorType) ValueFromString(_ context.Context, in basetypes.StringValue) (basetypes.StringValuable, diag.Diagnostics) {
	return hexColorValue{StringValue: in}, nil
}

func (t hexColorType) ValueFromTerraform(ctx context.Context, in tftypes.Value) (attr.Value, error) {
	v, err := t.StringType.ValueFromTerraform(ctx, in)
	if err != nil {
		return nil, err
	}
	sv, ok := v.(basetypes.StringValue)
	if !ok {
		return nil, fmt.Errorf("unexpected value type %T", v)
	}
	return hexColorValue{StringValue: sv}, nil
}

func (t hexColorType) ValueType(context.Context) attr.Value { return hexColorValue{} }

// hexColorValue is the value side of hexColorType.
type hexColorValue struct {
	basetypes.StringValue
}

var _ basetypes.StringValuableWithSemanticEquals = hexColorValue{}

func (v hexColorValue) Type(context.Context) attr.Type { return hexColorType{} }

func (v hexColorValue) Equal(o attr.Value) bool {
	other, ok := o.(hexColorValue)
	if !ok {
		return false
	}
	return v.StringValue.Equal(other.StringValue)
}

// StringSemanticEquals treats two colours as equal when they denote the same
// RGB value, so a server-normalised spelling keeps the configured one.
func (v hexColorValue) StringSemanticEquals(_ context.Context, other basetypes.StringValuable) (bool, diag.Diagnostics) {
	o, ok := other.(hexColorValue)
	if !ok {
		return false, nil
	}
	if v.IsNull() || v.IsUnknown() || o.IsNull() || o.IsUnknown() {
		return v.StringValue.Equal(o.StringValue), nil
	}
	return canonicalHexColor(v.ValueString()) == canonicalHexColor(o.ValueString()), nil
}

// canonicalHexColor lower-cases a hex colour and expands the 3-digit shorthand.
// Anything that is not a hex colour is returned unchanged.
func canonicalHexColor(s string) string {
	if !hexColorPattern.MatchString(s) {
		return s
	}
	s = strings.ToLower(s)
	if len(s) == 4 {
		return "#" + strings.Repeat(string(s[1]), 2) + strings.Repeat(string(s[2]), 2) + strings.Repeat(string(s[3]), 2)
	}
	return s
}
