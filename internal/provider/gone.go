package provider

import (
	"context"
	"fmt"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// A refresh that gets a 404 removes the resource from state, because a 404
// means "gone from this token's point of view". Flightdeck also answers 404,
// never 403, for anything in a project the token can no longer see: its user
// was removed from the project, or the project was made private. The removal
// is the same either way, but the next plan's offer to create the resource
// again is only right for the first. So a removal that may be the second
// comes with a warning saying how to get the resource back instead.

// hiddenProjectSummary is the summary of every such warning. It is the same
// for each, so Terraform folds the warnings of one refresh into one.
const hiddenProjectSummary = "A Flightdeck project is gone, or this token can no longer see it"

// hiddenProjectAdvice says what a 404 can mean and what to do when the
// project still exists.
func hiddenProjectAdvice(projectID int64) string {
	return fmt.Sprintf("Flightdeck gives the same 404 for a project that was deleted and for one the token's user can "+
		"no longer see, because they were removed from it or it was made private. If project %d still exists, don't "+
		"apply a plan that creates its resources again: it would fail, or make copies. Restore the token user's access "+
		"to the project instead, then bring each removed resource back into state with `terraform import`.", projectID)
}

// removeGoneProject removes a project Flightdeck answered 404 for from state,
// with the warning: for a project, a 404 can't tell deleted from hidden.
func removeGoneProject(ctx context.Context, resp *resource.ReadResponse, id int64, identifier string) {
	resp.State.RemoveResource(ctx)
	resp.Diagnostics.AddWarning(hiddenProjectSummary, fmt.Sprintf(
		"Flightdeck answered 404 for project %s (id %d), so it has been removed from Terraform state, and the next "+
			"plan offers to create it again. Its import id is `%d`.\n\n%s", identifier, id, id, hiddenProjectAdvice(id)))
}

// removeGone removes a resource in project projectID from state after
// Flightdeck answered 404 for it. When the project still reads back, the
// resource itself was deleted, and that is all. When it doesn't, the token may
// only have lost access to the project, so the removal comes with the warning.
// what names the resource ("label 17"), and importID is what `terraform
// import` takes for it.
func removeGone(ctx context.Context, c *client.Client, resp *resource.ReadResponse, projectID int64, what, importID string) {
	resp.State.RemoveResource(ctx)
	if _, err := c.GetProject(ctx, projectID); err == nil {
		return
	}
	resp.Diagnostics.AddWarning(hiddenProjectSummary, fmt.Sprintf(
		"Flightdeck answered 404 for %s, and for project %d as well, so it has been removed from Terraform state, and "+
			"the next plan offers to create it again. Its import id is `%s`.\n\n%s",
		what, projectID, importID, hiddenProjectAdvice(projectID)))
}
