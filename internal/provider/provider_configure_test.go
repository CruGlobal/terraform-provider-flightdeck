package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// configure drives Configure directly with the given attribute values (nil
// means null) so the env-fallback and validation logic can be tested without
// a resource to hang a Terraform config on.
func configure(t *testing.T, endpoint, token *string) provider.ConfigureResponse {
	t.Helper()
	ctx := context.Background()
	p := New("test")()
	var schemaResp provider.SchemaResponse
	p.Schema(ctx, provider.SchemaRequest{}, &schemaResp)

	str := func(v *string) tftypes.Value {
		if v == nil {
			return tftypes.NewValue(tftypes.String, nil)
		}
		return tftypes.NewValue(tftypes.String, *v)
	}
	raw := tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), map[string]tftypes.Value{
		"endpoint": str(endpoint),
		"token":    str(token),
	})
	var resp provider.ConfigureResponse
	p.Configure(ctx, provider.ConfigureRequest{Config: tfsdk.Config{Raw: raw, Schema: schemaResp.Schema}}, &resp)
	return resp
}

func ptr(s string) *string { return &s }

func TestConfigure_MissingValuesAreAttributeErrors(t *testing.T) {
	t.Setenv(envEndpoint, "")
	t.Setenv(envToken, "")
	resp := configure(t, nil, nil)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected errors for missing endpoint and token")
	}
	var summaries []string
	for _, d := range resp.Diagnostics.Errors() {
		summaries = append(summaries, d.Summary())
	}
	joined := strings.Join(summaries, "; ")
	if !strings.Contains(joined, "Missing Flightdeck endpoint") || !strings.Contains(joined, "Missing Flightdeck token") {
		t.Errorf("diagnostics = %q", joined)
	}
}

func TestConfigure_EnvironmentFallback(t *testing.T) {
	t.Setenv(envEndpoint, "https://flightdeck.example.com")
	t.Setenv(envToken, "fd_pat_from_env")
	resp := configure(t, nil, nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	c, ok := resp.ResourceData.(*client.Client)
	if !ok || c == nil {
		t.Fatalf("ResourceData = %T, want *client.Client", resp.ResourceData)
	}
	if got := c.BaseURL(); got != "https://flightdeck.example.com/api/v1" {
		t.Errorf("BaseURL = %q", got)
	}
	if resp.DataSourceData != resp.ResourceData {
		t.Error("data sources and resources must share one client")
	}
}

func TestConfigure_ConfigWinsOverEnvironment(t *testing.T) {
	t.Setenv(envEndpoint, "https://env.example.com")
	t.Setenv(envToken, "fd_pat_from_env")
	resp := configure(t, ptr("https://config.example.com/"), ptr("fd_pat_from_config"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	c := resp.ResourceData.(*client.Client)
	if got := c.BaseURL(); got != "https://config.example.com/api/v1" {
		t.Errorf("BaseURL = %q, want the configured endpoint", got)
	}
}

func TestConfigure_InvalidEndpoint(t *testing.T) {
	t.Setenv(envEndpoint, "")
	t.Setenv(envToken, "")
	resp := configure(t, ptr("flightdeck.example.com"), ptr("fd_pat_x"))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error for an endpoint without a scheme")
	}
	if got := resp.Diagnostics.Errors()[0].Summary(); got != "Invalid Flightdeck endpoint" {
		t.Errorf("summary = %q", got)
	}
}
