package provider

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// parseImportPair parses a `<first_id>/<second_id>` import id, for a link
// resource addressed by the two things it joins.
func parseImportPair(raw, first, second string, diags *diag.Diagnostics) (int64, int64, bool) {
	parts := strings.Split(strings.TrimSpace(raw), "/")
	if len(parts) != 2 {
		diags.AddError("Invalid import id", fmt.Sprintf(
			"Expected <%s_id>/<%s_id> (for example 12/7), got %q.", first, second, raw))
		return 0, 0, false
	}
	a, ok := parseImportID(parts[0], first, diags)
	if !ok {
		return 0, 0, false
	}
	b, ok := parseImportID(parts[1], second, diags)
	if !ok {
		return 0, 0, false
	}
	return a, b, true
}

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
