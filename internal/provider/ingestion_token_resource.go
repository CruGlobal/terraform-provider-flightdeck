package provider

import (
	"context"
	"strconv"
	"strings"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = &ingestionTokenResource{}
	_ resource.ResourceWithConfigure   = &ingestionTokenResource{}
	_ resource.ResourceWithImportState = &ingestionTokenResource{}
)

// NewIngestionTokenResource returns the flightdeck_ingestion_token resource.
func NewIngestionTokenResource() resource.Resource { return &ingestionTokenResource{} }

type ingestionTokenResource struct {
	client *client.Client
}

type ingestionTokenModel struct {
	ID          types.Int64  `tfsdk:"id"`
	ProjectID   types.Int64  `tfsdk:"project_id"`
	Name        types.String `tfsdk:"name"`
	Environment types.String `tfsdk:"environment"`
	Scope       types.String `tfsdk:"scope"`
	Token       types.String `tfsdk:"token"`
	LastFour    types.String `tfsdk:"last_four"`
}

// ingestionTokenToModel maps an API token. The plaintext comes from the create
// response and otherwise from prior state; it is never re-read.
func ingestionTokenToModel(t *client.IngestionToken, token types.String) ingestionTokenModel {
	if t.Token != "" {
		token = types.StringValue(t.Token)
	}
	return ingestionTokenModel{
		ID:          types.Int64Value(t.ID),
		ProjectID:   types.Int64Value(t.ProjectID),
		Name:        types.StringValue(t.Name),
		Environment: types.StringValue(t.Environment),
		Scope:       types.StringValue(t.Scope),
		Token:       token,
		LastFour:    types.StringValue(t.LastFour),
	}
}

func (r *ingestionTokenResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_ingestion_token"
}

func (r *ingestionTokenResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *ingestionTokenResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an error-ingestion token for a Flightdeck project — the credential an application uses " +
			"to report exceptions to the project's error tracking.\n\n" +
			"The token value is returned by the API **once, on create**, and stored in Terraform state as a sensitive " +
			"attribute so it can be handed to the application (for example through a secret manager). It is never " +
			"re-read; an imported token has no `token` value. Tokens cannot be edited: changing any attribute " +
			"replaces the token (the old one is revoked). Deleting the resource revokes the token.\n\n" +
			"Import by numeric id: `terraform import flightdeck_ingestion_token.prod 31`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				MarkdownDescription: "Numeric id of the token.",
				Computed:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"project_id": schema.Int64Attribute{
				MarkdownDescription: "Id of the project the token reports errors into.",
				Required:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Label shown in the project's token list (for example the deploying service's name).",
				Required:            true,
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"environment": schema.StringAttribute{
				MarkdownDescription: "Environment the token reports for (for example `production`, `staging`). Defaults to `production`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("production"),
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"scope": schema.StringAttribute{
				MarkdownDescription: "What the token may post: `post_server_item` (server-side SDKs; default) or " +
					"`post_client_item` (browser SDKs, where the token is public).",
				Optional:      true,
				Computed:      true,
				Default:       stringdefault.StaticString("post_server_item"),
				Validators:    []validator.String{stringvalidator.OneOf(client.IngestionTokenScopes...)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"token": schema.StringAttribute{
				MarkdownDescription: "The token value (`fd_post_…`). Available only when the resource created the token.",
				Computed:            true,
				Sensitive:           true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"last_four": schema.StringAttribute{
				MarkdownDescription: "Last four characters of the token, as shown in the UI's token list.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *ingestionTokenResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan ingestionTokenModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	projectID := plan.ProjectID.ValueInt64()
	fields := client.Fields{
		"name":        plan.Name.ValueString(),
		"environment": plan.Environment.ValueString(),
		"scope":       plan.Scope.ValueString(),
	}
	created, err := r.client.CreateIngestionToken(ctx, projectID, fields, client.PayloadKey("ingestion_token", strconv.FormatInt(projectID, 10), fields))
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error creating Flightdeck ingestion token", err)
		return
	}
	state := ingestionTokenToModel(created, types.StringNull())
	if state.Token.IsNull() {
		resp.Diagnostics.AddWarning("Ingestion token value not returned",
			"The API did not include the token value in the create response, so `token` is null in state. "+
				"Check the Flightdeck API version; the value is only ever available at creation.")
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *ingestionTokenResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state ingestionTokenModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	t, err := r.client.FindIngestionToken(ctx, state.ProjectID.ValueInt64(), state.ID.ValueInt64())
	if err != nil {
		if client.IsNotFound(err) {
			// Revoked (or the project is gone): recreate on next apply.
			resp.State.RemoveResource(ctx)
			return
		}
		addAPIError(&resp.Diagnostics, "Error reading Flightdeck ingestion token", err)
		return
	}
	newState := ingestionTokenToModel(t, state.Token)
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Update is unreachable: every configurable attribute requires replacement.
// It exists to satisfy the interface and simply carries the plan into state.
func (r *ingestionTokenResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan ingestionTokenModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *ingestionTokenResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state ingestionTokenModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.RevokeIngestionToken(ctx, state.ID.ValueInt64()); err != nil {
		addAPIError(&resp.Diagnostics, "Error revoking Flightdeck ingestion token", err)
	}
}

// ImportState accepts `<project_id>/<token_id>` (the token list is per project)
// or a bare token id, in which case every readable project is searched.
func (r *ingestionTokenResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	raw := strings.TrimSpace(req.ID)
	var (
		t   *client.IngestionToken
		err error
	)
	if parts := strings.Split(raw, "/"); len(parts) == 2 {
		projectID, ok := parseImportID(parts[0], "project", &resp.Diagnostics)
		if !ok {
			return
		}
		tokenID, ok := parseImportID(parts[1], "ingestion token", &resp.Diagnostics)
		if !ok {
			return
		}
		t, err = r.client.FindIngestionToken(ctx, projectID, tokenID)
	} else {
		tokenID, ok := parseImportID(raw, "ingestion token", &resp.Diagnostics)
		if !ok {
			return
		}
		t, err = r.findIngestionTokenAcrossProjects(ctx, tokenID)
	}
	if err != nil {
		addAPIError(&resp.Diagnostics, "Error importing Flightdeck ingestion token", err)
		return
	}
	state := ingestionTokenToModel(t, types.StringNull())
	resp.Diagnostics.AddWarning("Imported ingestion token has no token value",
		"The API returns a token's value only when it is created, so `token` is null for an imported token. "+
			"Replace the resource (taint it) if Terraform needs to hand the value to the application.")
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *ingestionTokenResource) findIngestionTokenAcrossProjects(ctx context.Context, tokenID int64) (*client.IngestionToken, error) {
	projects, err := r.client.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		t, ferr := r.client.FindIngestionToken(ctx, p.ID, tokenID)
		if ferr == nil {
			return t, nil
		}
		if !client.IsNotFound(ferr) {
			return nil, ferr
		}
	}
	return nil, &client.Error{Status: 404, Code: client.CodeNotFound, Method: "GET", Path: "/projects/*/ingestion_tokens",
		Message: "No active ingestion token " + strconv.FormatInt(tokenID, 10) + " in any readable project"}
}
