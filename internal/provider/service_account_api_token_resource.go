package provider

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = &serviceAccountAPITokenResource{}
	_ resource.ResourceWithConfigure   = &serviceAccountAPITokenResource{}
	_ resource.ResourceWithImportState = &serviceAccountAPITokenResource{}
	_ resource.ResourceWithModifyPlan  = &serviceAccountAPITokenResource{}
)

// timeNow is the clock the plan checks a token's expiry against. Tests move
// it to see a token expire without waiting.
var timeNow = time.Now

// NewServiceAccountAPITokenResource returns the
// flightdeck_service_account_api_token resource.
func NewServiceAccountAPITokenResource() resource.Resource { return &serviceAccountAPITokenResource{} }

type serviceAccountAPITokenResource struct {
	client *client.Client
}

type serviceAccountAPITokenModel struct {
	ID               types.Int64  `tfsdk:"id"`
	ServiceAccountID types.Int64  `tfsdk:"service_account_id"`
	Name             types.String `tfsdk:"name"`
	ExpiresAt        types.String `tfsdk:"expires_at"`
	Token            types.String `tfsdk:"token"`
	Masked           types.String `tfsdk:"masked"`
	LastFour         types.String `tfsdk:"last_four"`
	CreatedAt        types.String `tfsdk:"created_at"`
}

// serviceAccountAPITokenToModel maps an API token. The secret comes from the
// create response and otherwise from prior state; it is never re-read. The
// expiry keeps the spelling already held when the server reports the same
// instant in its own zone.
func serviceAccountAPITokenToModel(t *client.ServiceAccountAPIToken, token, expiresAt types.String) serviceAccountAPITokenModel {
	if t.Token != "" {
		token = types.StringValue(t.Token)
	}
	return serviceAccountAPITokenModel{
		ID:               types.Int64Value(t.ID),
		ServiceAccountID: types.Int64Value(t.ServiceAccountID),
		Name:             types.StringValue(t.Name),
		ExpiresAt:        keepSpellingOfSameInstant(expiresAt, t.ExpiresAt),
		Token:            token,
		Masked:           types.StringValue(t.Masked),
		LastFour:         types.StringValue(t.LastFour),
		CreatedAt:        types.StringValue(t.CreatedAt),
	}
}

func (r *serviceAccountAPITokenResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_service_account_api_token"
}

func (r *serviceAccountAPITokenResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *serviceAccountAPITokenResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a REST API token (`fd_pat_…`) of a Flightdeck service account: the credential an " +
			"application uses to call Flightdeck's API as that account. The token can do whatever the account can, so " +
			"give the account only the project access the application needs.\n\n" +
			"**Every read and write needs a token whose user is a workspace owner or admin.**\n\n" +
			"The token value is returned by the API **once, on create**, and stored in Terraform state as a sensitive " +
			"attribute so it can be handed to the application (for example through a secret manager). It is never " +
			"re-read; an imported token has no `token` value. Tokens can't be edited: changing any argument replaces " +
			"the token.\n\n" +
			"**For rotation, set `lifecycle { create_before_destroy = true }`.** By default Terraform deletes the old " +
			"token before it makes the new one, so the application is cut off until it gets the new value. With " +
			"`create_before_destroy`, Terraform makes the new token first, updates whatever uses its value (such as a " +
			"secret that holds it), and deletes the old token last.\n\n" +
			"`expires_at` is an RFC 3339 time with a time zone, such as `2027-06-01T00:00:00Z`. Leave it out for a " +
			"token that never expires. Flightdeck answers with the same time in its own zone, and the provider compares " +
			"the two as times and keeps what the configuration says. A time that has passed fails the plan of a new " +
			"token, because Flightdeck won't make a token that is already expired. When a token in state has expired, " +
			"the plan warns: set a later `expires_at` to replace it. The provider can't replace it on its own, since " +
			"the new token would get the same time that has passed.\n\n" +
			"A create is made idempotent with a key derived from `service_account_id`, `name` and `expires_at`, and " +
			"Flightdeck keeps those keys per provider token, so two tokens declared for one service account with all " +
			"three the same, and made with the same provider token, are one create as far as the API is concerned, for " +
			"the 24 hours it remembers a create. While the first token is live, Flightdeck refuses the " +
			"second create rather than replay it without its secret, and the apply fails naming the token that already " +
			"exists: change `name` or `expires_at` on the resource the error is reported on. The same error follows an " +
			"apply that failed after Flightdeck made the token; then delete the token it names and apply again. A " +
			"replacement that destroys first is not affected (the old token is gone by then), but one under " +
			"`create_before_destroy` with the same values (`terraform apply -replace`), within 24 hours of the old " +
			"token's create, is refused the same way. A rotation changes `expires_at` or `name`, so it is not " +
			"affected.\n\n" +
			"Deleting the resource deletes the token, and it stops working at once. Removing the service account from " +
			"the workspace deletes its tokens too, and a token whose account is gone leaves state on the next refresh.\n\n" +
			"Import with `<service_account_id>/<token_id>`: " +
			"`terraform import flightdeck_service_account_api_token.deploy 980190966/2129`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				MarkdownDescription: "Numeric id of the token.",
				Computed:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"service_account_id": schema.Int64Attribute{
				MarkdownDescription: "Id of the service account the token belongs to (`flightdeck_service_account.id`). " +
					"Changing it replaces the token.",
				Required:      true,
				Validators:    []validator.Int64{int64validator.AtLeast(1)},
				PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Label shown in the service account's token list, at most 255 characters (for " +
					"example the application's name and environment). Changing it replaces the token.",
				Required: true,
				Validators: []validator.String{
					notBlank("give the token a name"),
					noSurroundingBlank{},
					charLengthAtMost(255),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"expires_at": schema.StringAttribute{
				MarkdownDescription: "When the token stops working: an RFC 3339 time with a time zone, such as " +
					"`2027-06-01T00:00:00Z`. Leave it out for a token that never expires. It must be in the future when " +
					"the token is made. The same time written another way, for example in another time zone, is not a " +
					"change. Changing it to another time replaces the token.",
				Optional:   true,
				Validators: []validator.String{rfc3339Timestamp{}},
				PlanModifiers: []planmodifier.String{
					keepStateWhenSameInstant{},
					stringplanmodifier.RequiresReplace(),
				},
			},
			"token": schema.StringAttribute{
				MarkdownDescription: "The token value (`fd_pat_…`). Available only when the resource created the token.",
				Computed:            true,
				Sensitive:           true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"masked": schema.StringAttribute{
				MarkdownDescription: "The token with all but its last four characters hidden, as the token list shows it.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"last_four": schema.StringAttribute{
				MarkdownDescription: "Last four characters of the token.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"created_at": schema.StringAttribute{
				MarkdownDescription: "When the token was made (RFC 3339, in the server's time zone).",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// ModifyPlan checks the token's expiry against now. A plan that makes a new
// token with an expiry that has passed fails here, since Flightdeck refuses
// it. A token in state that has expired is not replaced: the replacement
// would carry the same expiry and be refused the same way. The plan warns
// instead, and names the fix.
func (r *serviceAccountAPITokenResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy
	}
	var plan serviceAccountAPITokenModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	now := timeNow()
	var state serviceAccountAPITokenModel
	creating := req.State.Raw.IsNull()
	if !creating {
		resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
		if resp.Diagnostics.HasError() {
			return
		}
		// Every argument forces replacement, so any change makes a new token.
		creating = !plan.ServiceAccountID.Equal(state.ServiceAccountID) || !plan.Name.Equal(state.Name) ||
			!plan.ExpiresAt.Equal(state.ExpiresAt)
	}
	if creating {
		if plan.ExpiresAt.IsNull() || plan.ExpiresAt.IsUnknown() {
			return
		}
		if at, ok := parseInstant(plan.ExpiresAt.ValueString()); ok && !at.After(now) {
			resp.Diagnostics.AddAttributeError(pathRoot("expires_at"), "This expiry has already passed",
				fmt.Sprintf("expires_at is %s, which has passed, and Flightdeck won't make a token that is already "+
					"expired. Set a later time, or leave expires_at out for a token that never expires.",
					plan.ExpiresAt.ValueString()))
		}
		return
	}
	if state.ExpiresAt.IsNull() || state.ExpiresAt.IsUnknown() {
		return
	}
	if at, ok := parseInstant(state.ExpiresAt.ValueString()); ok && !at.After(now) {
		resp.Diagnostics.AddAttributeWarning(pathRoot("expires_at"), "This service account API token has expired",
			fmt.Sprintf("API token %d (%q) of service account %d expired at %s and no longer works, so whatever uses "+
				"it can't call Flightdeck. Set expires_at to a later time, or leave it out for a token that never "+
				"expires, and apply: that replaces the token. Then give the application the new token.",
				state.ID.ValueInt64(), state.Name.ValueString(), state.ServiceAccountID.ValueInt64(), state.ExpiresAt.ValueString()))
	}
}

func (r *serviceAccountAPITokenResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan serviceAccountAPITokenModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	serviceAccountID := plan.ServiceAccountID.ValueInt64()
	fields := client.Fields{"name": plan.Name.ValueString()}
	if !plan.ExpiresAt.IsNull() {
		fields["expires_at"] = plan.ExpiresAt.ValueString()
	}
	created, err := r.client.CreateServiceAccountAPIToken(ctx, serviceAccountID, fields,
		client.PayloadKey("service_account_api_token", strconv.FormatInt(serviceAccountID, 10), fields))
	if err != nil {
		if addReplayWithheldError(&resp.Diagnostics, pathRoot("name"), "A service account API token with these values already exists",
			func(id int64) replayWithheldText {
				expires := "none"
				if !plan.ExpiresAt.IsNull() {
					expires = fmt.Sprintf("%q", plan.ExpiresAt.ValueString())
				}
				return replayWithheldText{
					record: fmt.Sprintf("API token %d of service account %d", id, serviceAccountID),
					distinct: fmt.Sprintf("If another flightdeck_service_account_api_token for this service account has the "+
						"name %q and expires_at %s, it holds that token: change the name or expires_at on this resource.",
						plan.Name.ValueString(), expires),
					retire: fmt.Sprintf("delete API token %d of service account %d in Flightdeck", id, serviceAccountID),
				}
			}, err) {
			return
		}
		addServiceAccountError(&resp.Diagnostics, "Error creating Flightdeck service account API token", err)
		return
	}
	// The client guarantees the token carries its secret or fails.
	state := serviceAccountAPITokenToModel(created, types.StringNull(), plan.ExpiresAt)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *serviceAccountAPITokenResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state serviceAccountAPITokenModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	t, err := r.client.GetServiceAccountAPIToken(ctx, state.ServiceAccountID.ValueInt64(), state.ID.ValueInt64())
	if err != nil {
		if client.IsNotFound(err) {
			// The token was deleted, or its service account left the
			// workspace, which deletes its tokens. Either way it is gone.
			resp.State.RemoveResource(ctx)
			return
		}
		addServiceAccountError(&resp.Diagnostics, "Error reading Flightdeck service account API token", err)
		return
	}
	newState := serviceAccountAPITokenToModel(t, state.Token, state.ExpiresAt)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Update changes nothing in Flightdeck: every argument forces replacement,
// and an expiry written another way plans no change at all. It exists to
// satisfy the interface and carries the plan into state.
func (r *serviceAccountAPITokenResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan serviceAccountAPITokenModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *serviceAccountAPITokenResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state serviceAccountAPITokenModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteServiceAccountAPIToken(ctx, state.ServiceAccountID.ValueInt64(), state.ID.ValueInt64()); err != nil {
		addServiceAccountError(&resp.Diagnostics, "Error deleting Flightdeck service account API token", err)
	}
}

// ImportState accepts `<service_account_id>/<token_id>`. An expired token
// can't be imported: it no longer works.
func (r *serviceAccountAPITokenResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	serviceAccountID, tokenID, ok := parseImportPair(req.ID, "service_account", "token", &resp.Diagnostics)
	if !ok {
		return
	}
	t, err := r.client.GetServiceAccountAPIToken(ctx, serviceAccountID, tokenID)
	if err != nil {
		addServiceAccountError(&resp.Diagnostics, "Error importing Flightdeck service account API token", err)
		return
	}
	if t.IsExpired(timeNow()) {
		resp.Diagnostics.AddError("Service account API token has expired",
			fmt.Sprintf("Token %d expired at %s and no longer works, so it can't be imported; create a new one.",
				t.ID, valueOr(t.ExpiresAt, "an unknown time")))
		return
	}
	state := serviceAccountAPITokenToModel(t, types.StringNull(), types.StringNull())
	resp.Diagnostics.AddWarning("Imported service account API token has no token value",
		"The API returns a token's value only when it is created, so `token` is null for an imported token. "+
			"Replace the resource (taint it) if Terraform needs to hand the value to the application.")
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
