package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestFeatureKeys(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name                   string
		keys                   []string
		wantErrors, wantWarned int
		wantText               string
		// notText must not appear in any diagnostic.
		notText string
	}{
		{name: "settable", keys: []string{"epics", "cycles"}},
		{name: "renamed", keys: []string{"modules"}, wantWarned: 1, wantText: `The "modules" feature is now "epics"`},
		{name: "renamed and its replacement", keys: []string{"modules", "epics"}, wantErrors: 1, wantText: "Keep only \"epics\""},
		{name: "set on its own endpoint", keys: []string{"slack"}, wantErrors: 1, wantText: "slack_channel.notifications_enabled"},
		{name: "self-healing", keys: []string{"self_healing"}, wantErrors: 1, wantText: "self_healing.feature_enabled"},
		// One error, and it lists only current names.
		{name: "unknown", keys: []string{"epic"}, wantErrors: 1, wantText: `"epic" is not a settable feature`, notText: "modules"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			elements := map[string]attr.Value{}
			for _, k := range tc.keys {
				elements[k] = types.BoolValue(true)
			}
			value, d := types.MapValue(types.BoolType, elements)
			if d.HasError() {
				t.Fatal(d)
			}
			resp := &validator.MapResponse{}
			featureKeys{}.ValidateMap(ctx, validator.MapRequest{Path: path.Root("features"), ConfigValue: value}, resp)
			assertDiagnostics(t, resp.Diagnostics, tc.wantErrors, tc.wantWarned, tc.wantText, tc.notText)
		})
	}
}

func TestWebhookEvents(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		events     []string
		wantErrors int
		wantText   string
	}{
		{name: "known", events: []string{"epic.created", "work_item.created"}},
		{name: "renamed", events: []string{"module.updated"}, wantErrors: 1, wantText: `"module.updated" event is now "epic.updated"`},
		{name: "unknown", events: []string{"project.exploded"}, wantErrors: 1, wantText: `"project.exploded" is not a webhook event`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			elements := make([]attr.Value, 0, len(tc.events))
			for _, e := range tc.events {
				elements = append(elements, types.StringValue(e))
			}
			value, d := types.SetValue(types.StringType, elements)
			if d.HasError() {
				t.Fatal(d)
			}
			resp := &validator.SetResponse{}
			webhookEvents{}.ValidateSet(ctx, validator.SetRequest{Path: path.Root("events"), ConfigValue: value}, resp)
			assertDiagnostics(t, resp.Diagnostics, tc.wantErrors, 0, tc.wantText, "")
		})
	}
	for _, e := range client.WebhookEvents {
		if strings.HasPrefix(e, "module.") {
			t.Fatalf("client.WebhookEvents still lists the old event %q", e)
		}
	}
}

// The old feature key round-trips: it goes out as the new key, and a read
// that reports only the new key fills the old one in state.
func TestRenamedFeatureMapping(t *testing.T) {
	ctx := context.Background()
	var diags diag.Diagnostics
	features, d := types.MapValueFrom(ctx, types.BoolType, map[string]bool{"modules": false, "cycles": true})
	diags.Append(d...)
	plan := projectModel{Name: types.StringValue("n"), Identifier: types.StringValue("ID"), Features: features}

	sent, _ := projectFields(ctx, &plan, nil, &diags)["features"].(map[string]bool)
	if _, old := sent["modules"]; old || sent["epics"] != false || !sent["cycles"] {
		t.Fatalf("features sent = %v, want epics=false and cycles=true without modules", sent)
	}
	if _, has := sent["epics"]; !has {
		t.Fatalf("features sent = %v, want epics", sent)
	}

	read := &client.Project{Features: map[string]bool{"epics": false, "cycles": true, "intake": false}}
	state := projectToModel(ctx, read, &plan, featuresFromPrior, &diags)
	if diags.HasError() {
		t.Fatal(diags)
	}
	var got map[string]bool
	diags.Append(state.Features.ElementsAs(ctx, &got, false)...)
	if len(got) != 2 || got["modules"] != false || !got["cycles"] {
		t.Fatalf("state features = %v, want modules=false and cycles=true", got)
	}
	if _, has := got["modules"]; !has {
		t.Fatalf("state features = %v, want the old key kept", got)
	}
}

func assertDiagnostics(t *testing.T, diags diag.Diagnostics, wantErrors, wantWarnings int, wantText, notText string) {
	t.Helper()
	if diags.ErrorsCount() != wantErrors || diags.WarningsCount() != wantWarnings {
		t.Fatalf("got %d errors and %d warnings, want %d and %d: %v", diags.ErrorsCount(), diags.WarningsCount(), wantErrors, wantWarnings, diags)
	}
	var all strings.Builder
	for _, d := range diags {
		all.WriteString(d.Summary() + "\n" + d.Detail() + "\n")
	}
	if wantText != "" && !strings.Contains(all.String(), wantText) {
		t.Fatalf("diagnostics do not say %q:\n%s", wantText, all.String())
	}
	if notText != "" && strings.Contains(all.String(), notText) {
		t.Fatalf("diagnostics mention %q:\n%s", notText, all.String())
	}
}
