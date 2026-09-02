package provider

import (
	"context"
	"regexp"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// toggleableFeatures are the project feature keys the API accepts on write —
// Project::TOGGLEABLE_FEATURES, the same allowlist the settings UI uses.
// self_healing and slack are reported on read but are not settable here.
var toggleableFeatures = []string{
	"cycles", "modules", "milestones", "views", "pages", "meeting_notes",
	"decisions", "intake", "errors", "incidents", "estimates",
}

var (
	// identifierPattern mirrors the model validation (1–10 uppercase
	// letters/digits, starting with a letter). The API upcases input, so the
	// provider requires the canonical form to avoid a perpetual diff.
	identifierPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}$`)
	// repoFullNamePattern mirrors Api::ProjectAttributes::REPO_FULL_NAME.
	repoFullNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
)

// projectModel is the Terraform state shape shared by the resource and the
// data source (the data source has no self_healing block).
type projectModel struct {
	ID                 types.Int64  `tfsdk:"id"`
	Name               types.String `tfsdk:"name"`
	Identifier         types.String `tfsdk:"identifier"`
	Description        types.String `tfsdk:"description"`
	Emoji              types.String `tfsdk:"emoji"`
	Archived           types.Bool   `tfsdk:"archived"`
	Features           types.Map    `tfsdk:"features"`
	GithubRepoFullName types.String `tfsdk:"github_repo_full_name"`
	LockVersion        types.Int64  `tfsdk:"lock_version"`
	SelfHealing        types.Object `tfsdk:"self_healing"`
}

// featureKeyFilter says which feature keys to keep when mapping the API's
// full effective map into state.
type featureKeyFilter int

const (
	// featuresFromPrior keeps only the keys present in the prior state, so a
	// configuration that manages a subset of features never diffs against the
	// keys it left alone. A null prior stays null.
	featuresFromPrior featureKeyFilter = iota
	// featuresToggleable keeps every settable key (import, where there is no
	// prior configuration to defer to).
	featuresToggleable
	// featuresAll keeps everything the API reports, including read-only keys
	// (the data source).
	featuresAll
)

// projectToModel maps an API project into state. prior supplies the feature
// keys to keep when filter is featuresFromPrior.
func projectToModel(ctx context.Context, p *client.Project, prior *projectModel, filter featureKeyFilter, diags *diag.Diagnostics) projectModel {
	m := projectModel{
		ID:                 types.Int64Value(p.ID),
		Name:               types.StringValue(p.Name),
		Identifier:         types.StringValue(p.Identifier),
		Description:        stringPointerValue(p.Description),
		Emoji:              stringPointerValue(p.Emoji),
		Archived:           types.BoolValue(p.Archived),
		GithubRepoFullName: stringPointerValue(p.GithubRepoFullName),
		LockVersion:        types.Int64Value(p.LockVersion),
		SelfHealing:        selfHealingToObject(p.SelfHealing, diags),
	}

	var keep map[string]bool
	switch filter {
	case featuresFromPrior:
		if prior == nil || prior.Features.IsNull() || prior.Features.IsUnknown() {
			m.Features = types.MapNull(types.BoolType)
			return m
		}
		keep = map[string]bool{}
		for k := range prior.Features.Elements() {
			keep[k] = true
		}
	case featuresToggleable:
		keep = map[string]bool{}
		for _, k := range toggleableFeatures {
			keep[k] = true
		}
	case featuresAll:
		keep = nil
	}

	selected := map[string]bool{}
	for k, v := range p.Features {
		if keep == nil || keep[k] {
			selected[k] = v
		}
	}
	features, d := types.MapValueFrom(ctx, types.BoolType, selected)
	diags.Append(d...)
	m.Features = features
	return m
}

func stringPointerValue(s *string) types.String {
	if s == nil {
		return types.StringNull()
	}
	return types.StringValue(*s)
}

// projectFields builds the write body from a plan. Every known attribute is
// sent; description and github_repo_full_name are sent as null when unset so
// removing them from configuration clears them server-side. features is sent
// only when configured: an absent block means "not managed here".
func projectFields(ctx context.Context, plan *projectModel, diags *diag.Diagnostics) client.Fields {
	fields := client.Fields{
		"name":       plan.Name.ValueString(),
		"identifier": plan.Identifier.ValueString(),
	}
	if !plan.Description.IsUnknown() {
		fields["description"] = plan.Description.ValueStringPointer()
	}
	if !plan.Emoji.IsNull() && !plan.Emoji.IsUnknown() {
		fields["emoji"] = plan.Emoji.ValueString()
	}
	if !plan.Archived.IsNull() && !plan.Archived.IsUnknown() {
		fields["archived"] = plan.Archived.ValueBool()
	}
	if !plan.GithubRepoFullName.IsUnknown() {
		fields["github_repo_full_name"] = plan.GithubRepoFullName.ValueStringPointer()
	}
	if !plan.Features.IsNull() && !plan.Features.IsUnknown() {
		var features map[string]bool
		diags.Append(plan.Features.ElementsAs(ctx, &features, false)...)
		fields["features"] = features
	}
	// Only sent when the block is configured with at least one known
	// threshold, so a token without workspace-admin rights that leaves the
	// block alone never trips the admin gate.
	if thresholds := selfHealingFields(ctx, plan.SelfHealing, diags); len(thresholds) > 0 {
		fields["self_healing"] = thresholds
	}
	return fields
}
