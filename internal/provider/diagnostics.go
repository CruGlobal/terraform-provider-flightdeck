package provider

import (
	"errors"
	"fmt"
	"strings"

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
	case client.IsNotFound(err):
		detail += "\n\nFlightdeck answers 404 for anything this token cannot reach: an id that does not exist or was " +
			"just deleted, one in another workspace, or anything in a project the token cannot see (a private project " +
			"it is not a member of, or one being deleted). Check the id and the token's access, then run `terraform " +
			"plan` again."
	case apiErr.Code == client.CodeStaleObject:
		detail += "\n\nThis write lost a race: the record, or something it refers to, changed or was deleted at the " +
			"same moment, and nothing was written. Run `terraform plan` again to re-read it, then re-apply."
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
//
// current is the version a re-read found, or nil when it could not be read.
// The API also answers stale_object to a write whose target, or something
// the write names (a user, a label, a team), was deleted at the same moment.
// When the refusal carries that code and the re-read still finds the state's
// version, that is the likelier cause, and the message leads with it rather
// than with an edit that may not have happened. A 409 without the code (an
// older server's uniqueness conflict, say) keeps the plain message. A refusal
// that follows an earlier, unanswered send of the same write is most likely
// that write's own doing, and says so first.
func addStaleError(diags *diag.Diagnostics, what string, stateVersion int64, current *int64, err error) {
	// An earlier attempt at this same write got no answer (it timed out, or
	// the connection dropped) and was sent again. That earlier attempt may
	// have been applied, and then it is what moved the version: blaming
	// someone outside Terraform, or saying nothing was overwritten, would be
	// wrong.
	if apiErr, ok := client.AsError(err); ok && apiErr.EarlierSendUnanswered {
		detail := fmt.Sprintf("An earlier attempt at this write to %s got no answer (it timed out, or the connection "+
			"dropped), so it was sent again. The retry was refused because %s had changed since the last refresh "+
			"(state has lock_version %d", what, what, stateVersion)
		if current != nil {
			detail += fmt.Sprintf(", the server now has %d", *current)
		}
		detail += "), most likely because the earlier attempt was applied. Run `terraform plan` again: if it shows " +
			"no changes, the write was applied; otherwise it shows what is different."
		if apiErr.Message != "" {
			detail += "\n\nThe API said: " + apiErr.Message
		}
		diags.AddError(what+" may already have this change", detail)
		return
	}
	var summary, detail string
	if current != nil && *current == stateVersion && client.HasCode(err, client.CodeStaleObject) {
		summary = what + " was not written: the write lost a race"
		detail = fmt.Sprintf("Flightdeck refused the write as a lost race, but %s still reads back the lock_version in "+
			"state (%d), so the likely cause is that something this write refers to was deleted at the same moment. "+
			"Nothing was written. Run `terraform plan` again to re-read it, then re-apply.", what, stateVersion)
	} else {
		summary = what + " modified outside of Terraform"
		detail = fmt.Sprintf("%s was changed outside of Terraform since the last refresh (state has lock_version %d", what, stateVersion)
		if current != nil {
			detail += fmt.Sprintf(", the server now has %d", *current)
		}
		detail += "). Nothing was overwritten. Run `terraform plan` again to pick up the current values, then re-apply."
	}
	if apiErr, ok := client.AsError(err); ok && apiErr.Message != "" {
		detail += "\n\nThe API said: " + apiErr.Message
	}
	diags.AddError(summary, detail)
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

// addRefusedID reports a 422 invalid_attribute refusing the id sent as attr,
// against that attribute, so the error points at the line to fix and reads as
// a bad argument rather than a missing resource. Flightdeck starts each such
// refusal with the field's name ("lead_id 7 is not a member of this
// workspace; ..."); that prefix is only used to pick the attribute, and a
// refusal without it (or without the code, from an older deployment) is left
// to the caller's generic handling. why says what the attribute must name and
// what happened to the write. It reports whether it added the error.
func addRefusedID(diags *diag.Diagnostics, attr, summary, why string, err error) bool {
	apiErr, ok := client.AsError(err)
	if !ok || apiErr.Code != client.CodeInvalidAttribute || !strings.HasPrefix(apiErr.Message, attr+" ") {
		return false
	}
	diags.AddAttributeError(path.Root(attr), summary, why+"\n\nThe API said: "+apiErr.Message)
	return true
}

// apiMessage returns the server's message from an API error, or the error text.
func apiMessage(err error) string {
	if apiErr, ok := client.AsError(err); ok && apiErr.Message != "" {
		return apiErr.Message
	}
	return err.Error()
}
