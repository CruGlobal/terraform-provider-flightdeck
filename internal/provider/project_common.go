package provider

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// toggleableFeatures are the project feature keys the API accepts on write —
// the same allowlist the settings UI uses.
// self_healing and slack are reported on read but are not settable here.
var toggleableFeatures = []string{
	"cycles", "modules", "milestones", "views", "pages", "meeting_notes",
	"decisions", "intake", "errors", "incidents", "estimates",
}

// identifierPattern mirrors the model validation (1–10 uppercase
// letters/digits, starting with a letter). The API upcases input, so the
// provider requires the canonical form to avoid a perpetual diff.
var identifierPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}$`)

// projectModel is the Terraform state shape shared by the resource and the
// data source.
type projectModel struct {
	ID                 types.Int64  `tfsdk:"id"`
	Name               types.String `tfsdk:"name"`
	Identifier         types.String `tfsdk:"identifier"`
	Description        types.String `tfsdk:"description"`
	Emoji              types.String `tfsdk:"emoji"`
	Archived           types.Bool   `tfsdk:"archived"`
	Features           types.Map    `tfsdk:"features"`
	GithubRepoFullName types.String `tfsdk:"github_repo_full_name"`
	LeadID             types.Int64  `tfsdk:"lead_id"`
	Network            types.String `tfsdk:"network"`
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
// keys to keep when filter is featuresFromPrior. The self_healing block is
// filled separately (see selfHealingToObject); it starts null here.
func projectToModel(ctx context.Context, p *client.Project, prior *projectModel, filter featureKeyFilter, diags *diag.Diagnostics) projectModel {
	m := projectModel{
		ID:                 types.Int64Value(p.ID),
		Name:               types.StringValue(p.Name),
		Identifier:         types.StringValue(p.Identifier),
		Description:        stringPointerValue(p.Description),
		Emoji:              stringPointerValue(p.Emoji),
		Archived:           types.BoolValue(p.Archived),
		GithubRepoFullName: stringPointerValue(p.GithubRepoFullName),
		LeadID:             types.Int64Null(),
		Network:            types.StringNull(),
		LockVersion:        types.Int64Value(p.LockVersion),
		SelfHealing:        types.ObjectNull(selfHealingAttrTypes),
	}
	if p.LeadID != nil {
		m.LeadID = types.Int64Value(*p.LeadID)
	}
	if p.Network != "" {
		m.Network = types.StringValue(p.Network)
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

// projectFields builds the project write body from a plan. Every known
// writable attribute is sent; description is sent as null when unset so
// removing it from configuration clears it server-side. features is sent only
// when configured: an absent block means "not managed here". lead_id is sent
// only when configured. github_repo_full_name is read-only over the API and
// never sent; the self_healing block travels on its own endpoint.
//
// network is sent when configured and, on an update (prior != nil), only when
// it differs from the prior state: re-sending private_project re-runs the
// server's lock-out guard (it re-creates the actor's and lead's admin
// membership rows if missing), a side effect an unchanged apply should not have.
func projectFields(ctx context.Context, plan, prior *projectModel, diags *diag.Diagnostics) client.Fields {
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
	if !plan.LeadID.IsNull() && !plan.LeadID.IsUnknown() {
		fields["lead_id"] = plan.LeadID.ValueInt64()
	}
	if !plan.Network.IsNull() && !plan.Network.IsUnknown() {
		if prior == nil || !plan.Network.Equal(prior.Network) {
			fields["network"] = plan.Network.ValueString()
		}
	}
	if !plan.Features.IsNull() && !plan.Features.IsUnknown() {
		var features map[string]bool
		diags.Append(plan.Features.ElementsAs(ctx, &features, false)...)
		fields["features"] = features
	}
	return fields
}

// reconcileFeatures makes a server-side override of a managed feature toggle
// surface as a diff rather than an apply error. The API may refuse to honour
// a toggle (a plan-gated feature, say) or omit a key entirely; Terraform
// requires the post-apply state to equal the plan for known values, so the
// plan's toggles are written to state as requested and a warning names the
// keys the server reports differently. The next refresh reads the server's
// values back, and the difference shows up as a plan.
func reconcileFeatures(ctx context.Context, state, plan *projectModel, diags *diag.Diagnostics) {
	if plan.Features.IsNull() || plan.Features.IsUnknown() {
		return
	}
	var wanted, got map[string]bool
	diags.Append(plan.Features.ElementsAs(ctx, &wanted, false)...)
	if !state.Features.IsNull() && !state.Features.IsUnknown() {
		diags.Append(state.Features.ElementsAs(ctx, &got, false)...)
	}
	var differing []string
	for k, v := range wanted {
		if actual, ok := got[k]; !ok || actual != v {
			differing = append(differing, fmt.Sprintf("%s (asked %t, server reports %s)", k, v, reportValue(got, k)))
		}
	}
	if len(differing) == 0 {
		return
	}
	sort.Strings(differing)
	state.Features = plan.Features
	diags.AddWarning("Flightdeck did not apply every feature toggle as requested",
		"The API reports a different effective value for: "+strings.Join(differing, "; ")+
			". State records the requested values; the next `terraform plan` will show the difference. "+
			"A toggle the API keeps overriding is not settable for this project through the API.")
}

func reportValue(m map[string]bool, k string) string {
	v, ok := m[k]
	if !ok {
		return "no value"
	}
	return fmt.Sprint(v)
}
