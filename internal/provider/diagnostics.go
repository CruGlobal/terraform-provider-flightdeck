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

// replayWithheldText is what a resource says about the record a refused
// replay names: what it is, how two declarations come to collide, and how to
// remove it.
type replayWithheldText struct {
	record   string // "Routing key 7 in project 42"
	distinct string // the fix for a second declaration with the same values
	retire   string // the fix for a record a failed apply left: "revoke routing key 7 in Flightdeck"
}

// addReplayWithheldError explains a create refused with
// idempotency_replay_withheld (or the client's own reading of an older
// server's secret-less replay of a live record): the create's key names a live
// record that this create did not make, and whose secret cannot be sent again.
// It reports whether err was that error.
func addReplayWithheldError(diags *diag.Diagnostics, at path.Path, summary string, text func(id int64) replayWithheldText, err error) bool {
	apiErr, ok := client.AsError(err)
	if !ok || apiErr.Code != client.CodeIdempotencyReplayWithheld {
		return false
	}
	t := text(apiErr.ID)
	detail := fmt.Sprintf("%s was made by a create identical to this one, and it is still live. Flightdeck returns its "+
		"secret only to the create that made it, so this resource cannot take it over. Nothing was created or "+
		"removed.\n\n"+
		"- %s\n"+
		"- If it was left behind by an earlier apply that failed, %s, then apply again.\n"+
		"- If this is a `create_before_destroy` replacement of a resource created less than 24 hours ago, it is "+
		"the one being replaced. Replace it once without `create_before_destroy`, or wait until a day has passed "+
		"since it was created.", t.record, t.distinct, t.retire)
	// The API's own words, when the refusal came from the API rather than
	// from the client reading an older server's replay.
	if cause, ok := client.AsError(apiErr.Unwrap()); ok && cause.Message != "" {
		detail += "\n\nThe API said: " + cause.Message
	}
	diags.AddAttributeError(at, summary, detail)
	return true
}

// apiMessage returns the server's message from an API error, or the error text.
func apiMessage(err error) string {
	if apiErr, ok := client.AsError(err); ok && apiErr.Message != "" {
		return apiErr.Message
	}
	return err.Error()
}
