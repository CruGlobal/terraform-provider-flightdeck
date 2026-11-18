package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = &serviceAccountResource{}
	_ resource.ResourceWithConfigure   = &serviceAccountResource{}
	_ resource.ResourceWithImportState = &serviceAccountResource{}
)

// NewServiceAccountResource returns the flightdeck_service_account resource.
func NewServiceAccountResource() resource.Resource { return &serviceAccountResource{} }

type serviceAccountResource struct {
	client *client.Client
}

// serviceAccountModel: id is the account's USER id, the one a project
// membership takes as user_id.
type serviceAccountModel struct {
	ID           types.Int64  `tfsdk:"id"`
	EmailAddress types.String `tfsdk:"email_address"`
	Name         types.String `tfsdk:"name"`
	Role         types.String `tfsdk:"role"`
	JoinedAt     types.String `tfsdk:"joined_at"`
}

func serviceAccountToModel(a *client.ServiceAccount) serviceAccountModel {
	return serviceAccountModel{
		ID:           types.Int64Value(a.ID),
		EmailAddress: types.StringValue(a.EmailAddress),
		Name:         types.StringValue(a.Name),
		Role:         types.StringValue(a.Role),
		JoinedAt:     types.StringValue(a.JoinedAt),
	}
}

func (r *serviceAccountResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_service_account"
}

func (r *serviceAccountResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *serviceAccountResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Flightdeck service account: a login for an application or a bot, not a person. " +
			"Give it access to a project with `flightdeck_project_member`, using this resource's `id` as `user_id`, and " +
			"give the application a token with `flightdeck_service_account_api_token`.\n\n" +
			"**Every read and write needs a token whose user is a workspace owner or admin.**\n\n" +
			"`email_address` names the account and can't be changed, so a new address replaces it. Flightdeck stores " +
			"addresses in lower case, so write it in lower case, with no spaces at either end. An address that " +
			"belongs to a person is refused. So is one that is already a service account in this workspace, and the " +
			"error gives the id to import it with.\n\n" +
			"`name` defaults to the part of the address before the `@`. **A service account can be in more than one " +
			"workspace, and its name is the same in all of them.** If the address belongs to a service account that " +
			"is already in another workspace, creating this resource adds that account to this workspace with the " +
			"name it already has, and Flightdeck won't rename it while it is in the other workspace. So if `name` is " +
			"set to something else, the apply fails, takes the account back out of this workspace, and names the name " +
			"to use. A later rename is refused the same way. An account that was removed from every workspace comes " +
			"back with the configured `name`, or the name it had when `name` is left out.\n\n" +
			"Deleting the resource removes the account from this workspace only, along with its API tokens and " +
			"project memberships here. Flightdeck never deletes the account itself, so creating it again with the " +
			"same address brings back the same `id`.\n\n" +
			"A create carries an idempotency key built from `email_address` and `name`, so a create retried after a " +
			"lost response doesn't fail. Flightdeck keeps those keys per provider token. So two resources with the " +
			"same address and name, created with the same provider token within 24 hours of each other, are one " +
			"create: both manage one account, and destroying either removes it. Created with different provider " +
			"tokens, the second create is refused because the address is already a service account in this " +
			"workspace, and the error gives the id to import.\n\n" +
			"Import by numeric id: `terraform import flightdeck_service_account.deploy 980190966`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				MarkdownDescription: "Numeric id of the service account. It is the account's user id, so it is what " +
					"`flightdeck_project_member.user_id` takes.",
				Computed:      true,
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"email_address": schema.StringAttribute{
				MarkdownDescription: "The account's email address. It only names the account, so it need not reach a " +
					"real inbox, but it must have one `@` and no spaces. Write it in lower case. At most 255 characters. " +
					"Changing it replaces the account.",
				Required:      true,
				Validators:    []validator.String{serviceAccountEmail{}, charLengthAtMost(255)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Display name, at most 255 characters. Leave it out to use the part of the address " +
					"before the `@`, or the name the account already has when Flightdeck already has one with this " +
					"address. Removing it keeps the current name. The name can't be changed while the account is also in " +
					"another workspace, so then it must be the name the account has, or left out.",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					notBlank("leave `name` out to use the part of the address before the `@`"),
					noSurroundingBlank{},
					charLengthAtMost(255),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"role": schema.StringAttribute{
				MarkdownDescription: "The account's workspace role. A service account is always a `member`.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"joined_at": schema.StringAttribute{
				MarkdownDescription: "When the account joined this workspace (RFC 3339, in the server's time zone).",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *serviceAccountResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan serviceAccountModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	email := plan.EmailAddress.ValueString()
	fields := client.Fields{"email_address": email}
	wantName := ""
	if !plan.Name.IsNull() && !plan.Name.IsUnknown() {
		wantName = plan.Name.ValueString()
		fields["name"] = wantName
	}
	created, answered, err := r.client.CreateServiceAccount(ctx, fields, client.PayloadKey("service_account", "", fields))
	if err != nil {
		if r.addAlreadyHereError(ctx, &resp.Diagnostics, email, err) {
			return
		}
		addServiceAccountError(&resp.Diagnostics, "Error creating Flightdeck service account", err)
		return
	}

	// The account can come back with another name in two ways. An address
	// that is a service account in another workspace joins with the name it
	// has there, whatever name was sent, and then the create's own answer has
	// that other name. A replayed create answers with the original 201, which
	// has the configured name, while the account may have been renamed since:
	// the read back has the name it has now. Either way, rename it to the
	// configured name, so the apply ends with the planned name.
	if wantName != "" && created.Name != wantName {
		renamed, rerr := r.client.UpdateServiceAccount(ctx, created.ID, client.Fields{"name": wantName})
		if rerr != nil {
			// Undo only the first case, and only when the rename was refused
			// because the name is shared. A replay names an account that was
			// already here before this apply, perhaps in use, and any other
			// failure (a 502, a timeout) says nothing about whether the name
			// can be set: removing the account then would take its tokens and
			// project access with it, for nothing.
			if answered.Name != wantName && isSharedNameRefusal(rerr) {
				r.undoJoin(ctx, &resp.Diagnostics, created, rerr)
				return
			}
			addRenameAfterCreateError(&resp.Diagnostics, created, answered, wantName, rerr)
			return
		}
		created = renamed
	}
	state := serviceAccountToModel(created)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// undoJoin handles a create that added a service account from another
// workspace with its shared name, when Flightdeck refused to rename it
// because that name is shared. The account is taken back out of this
// workspace, so the apply leaves nothing behind that Terraform doesn't know
// about, and the error says which name to use.
func (r *serviceAccountResource) undoJoin(ctx context.Context, diags *diag.Diagnostics, created *client.ServiceAccount, renameErr error) {
	undone := "Creating this resource is what added it to this workspace, so it was taken back out of this workspace."
	if derr := r.client.DeleteServiceAccount(ctx, created.ID); derr != nil {
		undone = fmt.Sprintf("Creating this resource added it to this workspace, and taking it back out of this "+
			"workspace failed, so it is still here as service account %d. Import it with "+
			"`terraform import <resource address> %d`, or remove it in Flightdeck, then apply again. "+
			"The removal failed with: %s", created.ID, created.ID, apiMessage(derr))
	}
	diags.AddAttributeError(pathRoot("name"), "This address belongs to a service account in another workspace",
		fmt.Sprintf("%s already belongs to a service account named %q, and that account is also in another "+
			"workspace. Its name is the same in every workspace it is in, so Flightdeck won't rename it here. Set "+
			"`name = %q`, or leave `name` out to use the name it has, and apply again. %s\n\nThe API said: %s",
			created.EmailAddress, created.Name, created.Name, undone, apiMessage(renameErr)))
}

// addRenameAfterCreateError reports a rename after a create that failed in a
// way that is not undone (see Create). The account stays in this workspace,
// as it is, with its tokens and project access, and the error gives the id
// to import it with.
func addRenameAfterCreateError(diags *diag.Diagnostics, created, answered *client.ServiceAccount, wantName string, renameErr error) {
	how := fmt.Sprintf("Creating this resource added service account %d (%s) to this workspace with the name it "+
		"already had, %q.", created.ID, created.EmailAddress, created.Name)
	if answered.Name == wantName {
		how = fmt.Sprintf("Service account %d (%s) was already in this workspace: Flightdeck answered with a create it "+
			"had already done, in the last 24 hours, and the account has been renamed %q since.",
			created.ID, created.EmailAddress, created.Name)
	}
	left := fmt.Sprintf("It was left in this workspace as it is, with its API tokens and project access, and "+
		"Terraform is not managing it. To manage it, import it:\n\n"+
		"  terraform import <resource address> %d\n\n"+
		"or add an `import` block with `id = \"%d\"`.", created.ID, created.ID)
	if isSharedNameRefusal(renameErr) {
		diags.AddAttributeError(pathRoot("name"), "This service account's name can't be changed here",
			fmt.Sprintf("%s It is also in another workspace, and its name is the same in every workspace it is in, "+
				"so Flightdeck won't rename it to %q here. %s Then set `name = %q`, or leave `name` out, and apply "+
				"again.\n\nThe API said: %s",
				how, wantName, left, created.Name, apiMessage(renameErr)))
		return
	}
	addServiceAccountErrorNote(diags, "Error renaming Flightdeck service account", renameErr, fmt.Sprintf(
		"%s Renaming it to %q failed. %s Or apply again to retry the rename.", how, wantName, left))
}

// addAlreadyHereError explains a create refused because the address is
// already a service account in this workspace, or because a create for it
// won a race with this one, and names the id to import. It reports whether
// err was that refusal.
func (r *serviceAccountResource) addAlreadyHereError(ctx context.Context, diags *diag.Diagnostics, email string, err error) bool {
	alreadyHere := client.HasCode(err, client.CodeValidationFailed) &&
		strings.Contains(apiMessage(err), "already a service account in this workspace")
	if !alreadyHere && !client.HasCode(err, client.CodeUniquenessConflict) {
		return false
	}
	existing, ferr := r.client.FindServiceAccountByEmail(ctx, email)
	if ferr != nil {
		if !alreadyHere {
			return false
		}
		diags.AddAttributeError(pathRoot("email_address"), "This service account is already in the workspace",
			fmt.Sprintf("%s is already a service account in this workspace, so it can't be created again. Nothing was "+
				"changed. Looking up its id to import it failed: %s\n\nThe API said: %s", email, apiMessage(ferr), apiMessage(err)))
		return true
	}
	diags.AddAttributeError(pathRoot("email_address"), "This service account is already in the workspace",
		fmt.Sprintf("%s is already a service account in this workspace (id %d, named %q), so it can't be created "+
			"again. Nothing was changed. To manage it with Terraform, import it:\n\n"+
			"  terraform import <resource address> %d\n\n"+
			"or add an `import` block with `id = \"%d\"`. If another flightdeck_service_account in this configuration "+
			"already has this address, remove one of them.\n\nThe API said: %s",
			existing.EmailAddress, existing.ID, existing.Name, existing.ID, existing.ID, apiMessage(err)))
	return true
}

func (r *serviceAccountResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state serviceAccountModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	a, err := r.client.GetServiceAccount(ctx, state.ID.ValueInt64())
	if err != nil {
		if client.IsNotFound(err) {
			// Removed from this workspace (the account itself is never
			// deleted). The next plan adds it again.
			resp.State.RemoveResource(ctx)
			return
		}
		addServiceAccountError(&resp.Diagnostics, "Error reading Flightdeck service account", err)
		return
	}
	newState := serviceAccountToModel(a)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Update renames the account: the name is the only argument that changes in
// place.
func (r *serviceAccountResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state serviceAccountModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueInt64()
	fields := client.Fields{}
	if !plan.Name.IsNull() && !plan.Name.IsUnknown() && plan.Name.ValueString() != state.Name.ValueString() {
		fields["name"] = plan.Name.ValueString()
	}
	var (
		updated *client.ServiceAccount
		err     error
	)
	if len(fields) == 0 {
		updated, err = r.client.GetServiceAccount(ctx, id)
	} else {
		updated, err = r.client.UpdateServiceAccount(ctx, id, fields)
	}
	if err != nil {
		if isSharedNameRefusal(err) {
			resp.Diagnostics.AddAttributeError(pathRoot("name"), "This service account's name can't be changed here",
				fmt.Sprintf("Service account %q (%s) is also in another workspace. Its name is the same in every "+
					"workspace it is in, so Flightdeck won't rename it here. Set `name = %q`, the name it has, or "+
					"leave `name` out. Nothing was changed.\n\nThe API said: %s",
					state.Name.ValueString(), state.EmailAddress.ValueString(), state.Name.ValueString(), apiMessage(err)))
			return
		}
		addServiceAccountError(&resp.Diagnostics, "Error updating Flightdeck service account", err)
		return
	}
	newState := serviceAccountToModel(updated)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *serviceAccountResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state serviceAccountModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteServiceAccount(ctx, state.ID.ValueInt64()); err != nil {
		addServiceAccountError(&resp.Diagnostics, "Error removing Flightdeck service account", err)
	}
}

func (r *serviceAccountResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, ok := parseImportID(req.ID, "service account", &resp.Diagnostics)
	if !ok {
		return
	}
	a, err := r.client.GetServiceAccount(ctx, id)
	if err != nil {
		addServiceAccountError(&resp.Diagnostics, "Error importing Flightdeck service account", err)
		return
	}
	state := serviceAccountToModel(a)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// isSharedNameRefusal reports Flightdeck refusing a rename because the
// account is also in another workspace.
func isSharedNameRefusal(err error) bool {
	if !client.HasCode(err, client.CodeInvalidAttribute) {
		return false
	}
	msg := apiMessage(err)
	return strings.HasPrefix(msg, "name ") && strings.Contains(msg, "another workspace")
}

// serviceAccountsNeedAdmin is what a 403 on the service-account routes means.
const serviceAccountsNeedAdmin = "Service accounts and their API tokens can only be read or changed with a token " +
	"whose user is a workspace owner or admin. Use such a token, or ask a workspace admin to make this user one."

// addServiceAccountError is addAPIError for the service-account routes,
// where every 403 is the workspace owner or admin rule.
func addServiceAccountError(diags *diag.Diagnostics, summary string, err error) {
	addServiceAccountErrorNote(diags, summary, err, "")
}

func addServiceAccountErrorNote(diags *diag.Diagnostics, summary string, err error, note string) {
	if client.IsForbidden(err) {
		if note != "" {
			note += "\n\n"
		}
		note += serviceAccountsNeedAdmin
	}
	addAPIErrorNote(diags, summary, err, note)
}

// serviceAccountEmail refuses, at plan time, an address Flightdeck would not
// store as written: one with blank space at either end or a capital letter
// (Flightdeck strips and lowercases it, so the next plan would show a
// change), or one without the single `@` it requires.
type serviceAccountEmail struct{}

func (serviceAccountEmail) Description(context.Context) string {
	return "a lower-case email address with one @ and no spaces"
}

func (v serviceAccountEmail) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (serviceAccountEmail) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	value := req.ConfigValue.ValueString()
	stored := strings.ToLower(strings.Trim(value, apiBlank))
	switch {
	case stored != value && stored != "":
		resp.Diagnostics.AddAttributeError(req.Path, "Write the address as Flightdeck stores it",
			fmt.Sprintf("Flightdeck stores %q as %q (in lower case, with no spaces at either end), so the next plan "+
				"would show a change. Write %q instead.", value, stored, stored))
	case !isServiceEmailShape(value):
		resp.Diagnostics.AddAttributeError(req.Path, "Not an email address",
			fmt.Sprintf("%q is not an address Flightdeck takes for a service account. It needs exactly one `@`, with "+
				"something on each side and no spaces, such as deploy-bot@machineusers.example.com.", value))
	}
}

// isServiceEmailShape is Flightdeck's rule for a service account's address:
// one `@`, something on each side, and no ASCII white space anywhere.
func isServiceEmailShape(s string) bool {
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 || strings.Count(s, "@") != 1 {
		return false
	}
	return !strings.ContainsAny(s, " \t\r\n\f\v")
}
