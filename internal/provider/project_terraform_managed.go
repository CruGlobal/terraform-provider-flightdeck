package provider

import (
	"context"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Flightdeck marks a project terraform_managed to say Terraform owns its
// settings. While the flag is on, the web app shows those settings read only
// and MCP refuses to change them, because a change made by hand would be
// undone by the next apply. /api/v1, which this provider writes through,
// still takes every write. Only a workspace owner or admin may set or change
// the flag; sending back the stored value is fine for anyone.
//
// The provider sets it on every project it manages unless the configuration
// says false. A Flightdeck older than the setting has no such key on a read
// and refuses any write naming it, so the provider stays out of its way: a
// project whose reads have never carried the key plans null rather than the
// default (planTerraformManaged) and is never sent the key. Only a create,
// which has no read to go by, or a configuration that sets the flag
// explicitly, asks such a Flightdeck for it, and the refusal says to upgrade.

// terraformManagedDescription is the resource attribute's documentation.
const terraformManagedDescription = "Whether Terraform manages this project's settings. While it is `true`, Flightdeck's " +
	"web app makes the settings Terraform owns read only (name, identifier, description, emoji, lead, visibility, " +
	"deployed app, feature toggles, the Slack channel and agent work settings) and turns off archiving and restoring " +
	"the project, and Flightdeck's MCP tools refuse to change them, because a change made by hand would be undone by " +
	"the next apply. Flightdeck's API, which this provider uses, still writes everything. Members, states, labels " +
	"and the rest stay editable in the app. Defaults to `true`; set it to `false` to leave the project editable in " +
	"the app.\n\n" +
	"Setting or changing it needs a token whose user is a **workspace owner or admin**, on a create too. Sending " +
	"the value Flightdeck already has is fine for any token, so a token that is not an owner or admin can still " +
	"update a project whose flag already matches the configuration. Such a token can create a project only with " +
	"`terraform_managed = false`.\n\n" +
	"Removing a project from Terraform without destroying it (a `removed` block, or `terraform state rm`) leaves " +
	"the flag on, so the project stays read only in the app. To hand a project back to the app, apply " +
	"`terraform_managed = false` first, then remove it from Terraform.\n\n" +
	"Importing a project that is not marked yet plans an update that sets it to `true`.\n\n" +
	"Needs a Flightdeck that supports `terraform_managed`. Against an older one, a project that already exists " +
	"keeps working while this is unset: it reads as null and the provider does not send it. Creating a project, " +
	"or setting this explicitly, fails there with an error asking you to upgrade Flightdeck."

// terraformManagedAttribute is the resource's terraform_managed attribute. Its
// default is planned by planTerraformManaged rather than a schema Default: a
// Default would plan true against the null a Flightdeck older than the
// setting leaves in state, a change that could never be applied.
func terraformManagedAttribute() schema.BoolAttribute {
	return schema.BoolAttribute{
		MarkdownDescription: terraformManagedDescription,
		Optional:            true,
		Computed:            true,
	}
}

// planTerraformManaged plans terraform_managed when the configuration leaves
// it unset (a configured value is planned as configured): true on a create
// and for a project whose Flightdeck reports the setting, and null for one
// whose Flightdeck has never reported it (null in state). That Flightdeck is
// older than the setting and refuses any write naming it, so the plan shows
// no change and the write does not send it. Once it is upgraded, the next
// refresh reads the stored value and the default applies again.
//
// prior is the state, nil on a create. A change this plans by itself turns the
// flag on for a project stored with it off; the write that makes it bumps the
// project's lock_version, so that is planned unknown too. (A change in the
// configuration has already had the framework do so.)
func planTerraformManaged(ctx context.Context, config types.Bool, prior *projectModel, plan *projectModel, resp *resource.ModifyPlanResponse) {
	if !config.IsNull() {
		return
	}
	want := types.BoolValue(true)
	if prior != nil && prior.TerraformManaged.IsNull() {
		want = types.BoolNull()
	}
	if !plan.TerraformManaged.Equal(want) {
		plan.TerraformManaged = want
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("terraform_managed"), want)...)
	}
	if prior != nil && !want.Equal(prior.TerraformManaged) && !plan.LockVersion.IsUnknown() {
		plan.LockVersion = types.Int64Unknown()
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("lock_version"), plan.LockVersion)...)
	}
}

// keepUnsentTerraformManaged keeps terraform_managed null in the state a write
// leaves when the plan had no value, so the write did not send one. A
// Flightdeck upgraded between the plan and the apply would report the stored
// value, which would not match the plan; the next refresh reads it instead.
func keepUnsentTerraformManaged(state, plan *projectModel) {
	if plan.TerraformManaged.IsNull() {
		state.TerraformManaged = types.BoolNull()
	}
}

// projectAdminChanges says which settings a project write sets or changes
// that only a workspace owner or admin may. It is what decides whether a 403
// is that bar or the project role every other update needs.
type projectAdminChanges struct {
	// app is whether the write sets or changes the app. The app is only
	// sent when it does.
	app bool
	// terraformManaged is the value the write changes terraform_managed to,
	// or nil when it sends none or sends back what Flightdeck has.
	terraformManaged *bool
}

// projectAdminChangesOf reads a write's admin-only changes from its body.
// prior is the state an update starts from, nil for a create. A create stores
// terraform_managed false unless it says otherwise, so only true changes it
// there. An update changes it when the value differs from the one in state,
// or when the state has none (that Flightdeck never reported one).
func projectAdminChangesOf(fields client.Fields, prior *projectModel) projectAdminChanges {
	var changes projectAdminChanges
	_, changes.app = fields["app"]
	managed, sent := fields["terraform_managed"].(bool)
	switch {
	case !sent:
	case prior == nil:
		if managed {
			changes.terraformManaged = &managed
		}
	case prior.TerraformManaged.IsNull() || prior.TerraformManaged.IsUnknown() || prior.TerraformManaged.ValueBool() != managed:
		changes.terraformManaged = &managed
	}
	return changes
}

// addTerraformManagedForbidden reports a 403 to a write that sets or changes
// terraform_managed, against the attribute, saying how to get past it: a
// workspace owner's or admin's token, or a configuration that leaves the flag
// as Flightdeck has it.
func addTerraformManagedForbidden(diags *diag.Diagnostics, managed bool, apiErr *client.Error) {
	what, instead := "turns terraform_managed on", "set `terraform_managed = false` to leave the project's flag off"
	if !managed {
		what, instead = "turns terraform_managed off", "remove `terraform_managed = false` from the configuration to leave the flag on"
	}
	diags.AddAttributeError(path.Root("terraform_managed"), "Changing terraform_managed requires a workspace owner or admin",
		"This write "+what+". The provider's token must belong to a workspace owner or admin to set or change "+
			"terraform_managed, even where the token could otherwise update the project. Use such a token, or "+
			instead+". Nothing was saved.\n\nThe API said: "+apiErr.Error())
}

// addTerraformManagedUnsupportedError explains a refused write that named
// terraform_managed to a Flightdeck too old to know it. Such a Flightdeck
// refuses the key as unknown, in prose, so rather than match the wording a
// read is asked whether it reports the setting, as the self-healing block
// does for its newer settings. projectID is 0 for a create. It reports
// whether it added a diagnostic; false leaves the error to the caller.
func addTerraformManagedUnsupportedError(ctx context.Context, c *client.Client, projectID int64, sent client.Fields, err error, diags *diag.Diagnostics) bool {
	if !client.IsValidation(err) {
		return false
	}
	if _, named := sent["terraform_managed"]; !named {
		return false
	}
	if supported, known := terraformManagedSupported(ctx, c, projectID); supported || !known {
		return false
	}
	detail := "This Flightdeck refused terraform_managed as an unknown setting: it is older than the version that " +
		"added it. Upgrade Flightdeck first, then apply again."
	if projectID == 0 {
		detail += " Nothing was created.\n\nThe provider sends terraform_managed on every project create, so " +
			"creating a project needs a Flightdeck that supports it. Projects that already exist keep working " +
			"against this one while their configuration leaves terraform_managed unset."
	} else {
		detail += " Nothing was saved.\n\nUntil then, leave terraform_managed unset for this project: while this " +
			"Flightdeck has never reported it, the provider does not send it."
	}
	diags.AddAttributeError(path.Root("terraform_managed"), "This Flightdeck does not support terraform_managed yet",
		detail+"\n\nThe API said: "+apiMessage(err))
	return true
}

// terraformManagedSupported asks Flightdeck whether it knows terraform_managed:
// it does when a project read carries the key. A create has no project to
// read yet (projectID 0), so it asks the project list, which tells only when
// it lists at least one project. known is false when the answer can't tell,
// including when the read itself fails.
func terraformManagedSupported(ctx context.Context, c *client.Client, projectID int64) (supported, known bool) {
	if projectID != 0 {
		p, err := c.GetProject(ctx, projectID)
		if err != nil {
			return false, false
		}
		return p.ReportsTerraformManaged(), true
	}
	projects, err := c.ListProjects(ctx)
	if err != nil || len(projects) == 0 {
		return false, false
	}
	for i := range projects {
		if projects[i].ReportsTerraformManaged() {
			return true, true
		}
	}
	return false, true
}
