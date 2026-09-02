package provider

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// parseImportID parses a plain numeric import id.
func parseImportID(raw, what string, diags *diag.Diagnostics) (int64, bool) {
	raw = strings.TrimSpace(raw)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		diags.AddError("Invalid import id", fmt.Sprintf("Expected the numeric id of the %s (for example 17), got %q.", what, raw))
		return 0, false
	}
	return id, true
}
