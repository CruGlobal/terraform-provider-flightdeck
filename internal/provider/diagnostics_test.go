package provider

import (
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// A stale refusal after an earlier send went unanswered most likely means
// that earlier send was applied, so the error must not say someone outside
// Terraform changed the record, or that nothing was overwritten.
func TestAddStaleError_afterAnUnansweredSend(t *testing.T) {
	current := int64(4)
	stale := &client.Error{Method: "PATCH", Path: "/labels/7", Status: 409, Code: client.CodeStaleObject,
		Message: "This resource was modified by someone else.", Preconditioned: true}

	var plain diag.Diagnostics
	addStaleError(&plain, `Label "Bug"`, 3, &current, stale)
	if got := plain[0].Summary(); !strings.Contains(got, "modified outside of Terraform") {
		t.Errorf("an answered write's summary = %q, want the usual one", got)
	}

	unanswered := *stale
	unanswered.EarlierSendUnanswered = true
	var retried diag.Diagnostics
	addStaleError(&retried, `Label "Bug"`, 3, &current, &unanswered)
	summary, detail := retried[0].Summary(), retried[0].Detail()
	if summary != `Label "Bug" may already have this change` {
		t.Errorf("summary = %q", summary)
	}
	for _, want := range []string{"got no answer", "lock_version 3", "the server now has 4", "earlier attempt was applied", "terraform plan"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, detail)
		}
	}
	for _, wrong := range []string{"outside of Terraform", "Nothing was overwritten"} {
		if strings.Contains(summary+detail, wrong) {
			t.Errorf("the message still says %q:\n%s", wrong, detail)
		}
	}
}
