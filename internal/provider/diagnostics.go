package provider

import (
	"errors"
	"fmt"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
)

// pathRoot is path.Root, for attribute-scoped diagnostics.
func pathRoot(name string) path.Path { return path.Root(name) }

// addAPIError turns a client error into a diagnostic. The summary names the
// operation; the detail carries the HTTP status, the machine-readable code
// when the API sent one, and the server's message, so a user can tell a
// permissions problem from a validation error without guessing.
func addAPIError(diags *diag.Diagnostics, summary string, err error) {
	var apiErr *client.Error
	if !errors.As(err, &apiErr) {
		diags.AddError(summary, err.Error())
		return
	}
	detail := apiErr.Error()
	switch {
	case client.IsUnauthorized(err):
		detail += "\n\nThe Flightdeck token was rejected. Check that it is a valid, unexpired personal access token and that its user is still a member of the workspace."
	case client.IsForbidden(err):
		detail += "\n\nThe token's user lacks the project or workspace role this operation requires."
	case apiErr.Code == client.CodeInvalidAttribute:
		detail += "\n\nThe API rejected the configuration outright; fix the attribute rather than retrying."
	case apiErr.Status == 429:
		detail += "\n\nThe API rate limit was still exceeded after the provider's retries; re-run the operation, or reduce parallelism with -parallelism."
	}
	diags.AddError(summary, detail)
}

// addStaleError reports a lost optimistic-locking race. The provider never
// retries over the other writer's change; the user re-plans to see it. The
// server's own message is quoted verbatim so a 409 that turns out to be
// something else (a uniqueness conflict on a deployment without error codes)
// is still readable.
func addStaleError(diags *diag.Diagnostics, what string, stateVersion int64, current *int64, err error) {
	detail := fmt.Sprintf("%s was changed outside of Terraform since the last refresh (state has lock_version %d", what, stateVersion)
	if current != nil {
		detail += fmt.Sprintf(", the server now has %d", *current)
	}
	detail += "). Nothing was overwritten. Run `terraform plan` again to pick up the current values, then re-apply."
	if apiErr, ok := client.AsError(err); ok && apiErr.Message != "" {
		detail += "\n\nThe API said: " + apiErr.Message
	}
	diags.AddError(what+" modified outside of Terraform", detail)
}

// apiMessage returns the server's message from an API error, or the error text.
func apiMessage(err error) string {
	if apiErr, ok := client.AsError(err); ok && apiErr.Message != "" {
		return apiErr.Message
	}
	return err.Error()
}
