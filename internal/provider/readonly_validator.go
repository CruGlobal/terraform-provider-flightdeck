package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// readOnlyAttribute rejects any configured value for an attribute the API only
// ever reports, with a message that says where the value is managed. It exists
// because a purely Computed attribute gets Terraform's generic "read-only
// attribute" error, which cannot point the user anywhere.
type readOnlyAttribute string

func (v readOnlyAttribute) Description(context.Context) string {
	return "read-only; " + string(v)
}

func (v readOnlyAttribute) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (v readOnlyAttribute) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() {
		return
	}
	resp.Diagnostics.AddAttributeError(req.Path, "Read-only attribute",
		"This attribute is reported by the API but cannot be set here; "+string(v)+".")
}
