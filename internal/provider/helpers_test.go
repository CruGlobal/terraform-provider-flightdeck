package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// applyDiagnostics collects the diagnostics every ApplyResourceChange call
// returned during a test, and separately those of every plan and refresh. The
// testing framework only surfaces errors (via ExpectError), so this is how a
// test asserts a warning the provider produced.
type applyDiagnostics struct {
	mu    sync.Mutex
	diags []*tfprotov6.Diagnostic
	// plans and reads are what PlanResourceChange and ReadResource returned.
	// Terraform plans and refreshes more than once per test step, so assert
	// that a diagnostic is there rather than how many times.
	plans []*tfprotov6.Diagnostic
	reads []*tfprotov6.Diagnostic
}

// bySeverity returns the recorded apply diagnostics of one severity.
func (a *applyDiagnostics) bySeverity(severity tfprotov6.DiagnosticSeverity) []*tfprotov6.Diagnostic {
	return a.filter(func(a *applyDiagnostics) []*tfprotov6.Diagnostic { return a.diags }, severity)
}

// planWarnings returns the warnings any plan returned.
func (a *applyDiagnostics) planWarnings() []*tfprotov6.Diagnostic {
	return a.filter(func(a *applyDiagnostics) []*tfprotov6.Diagnostic { return a.plans }, tfprotov6.DiagnosticSeverityWarning)
}

// readWarnings returns the warnings any refresh returned.
func (a *applyDiagnostics) readWarnings() []*tfprotov6.Diagnostic {
	return a.filter(func(a *applyDiagnostics) []*tfprotov6.Diagnostic { return a.reads }, tfprotov6.DiagnosticSeverityWarning)
}

func (a *applyDiagnostics) filter(from func(*applyDiagnostics) []*tfprotov6.Diagnostic, severity tfprotov6.DiagnosticSeverity) []*tfprotov6.Diagnostic {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*tfprotov6.Diagnostic
	for _, d := range from(a) {
		if d.Severity == severity {
			out = append(out, d)
		}
	}
	return out
}

func (a *applyDiagnostics) record(into *[]*tfprotov6.Diagnostic, diags []*tfprotov6.Diagnostic) {
	a.mu.Lock()
	defer a.mu.Unlock()
	*into = append(*into, diags...)
}

// diagnosticRecordingServer passes every call through to the provider and
// keeps a copy of what each apply, plan and refresh returned.
type diagnosticRecordingServer struct {
	tfprotov6.ProviderServer
	into *applyDiagnostics
}

func (s diagnosticRecordingServer) ApplyResourceChange(ctx context.Context, req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	resp, err := s.ProviderServer.ApplyResourceChange(ctx, req)
	if resp != nil {
		s.into.record(&s.into.diags, resp.Diagnostics)
	}
	return resp, err
}

func (s diagnosticRecordingServer) PlanResourceChange(ctx context.Context, req *tfprotov6.PlanResourceChangeRequest) (*tfprotov6.PlanResourceChangeResponse, error) {
	resp, err := s.ProviderServer.PlanResourceChange(ctx, req)
	if resp != nil {
		s.into.record(&s.into.plans, resp.Diagnostics)
	}
	return resp, err
}

func (s diagnosticRecordingServer) ReadResource(ctx context.Context, req *tfprotov6.ReadResourceRequest) (*tfprotov6.ReadResourceResponse, error) {
	resp, err := s.ProviderServer.ReadResource(ctx, req)
	if resp != nil {
		s.into.record(&s.into.reads, resp.Diagnostics)
	}
	return resp, err
}

// runTestRecordingApplyDiagnostics is runTest with every apply's, plan's and
// refresh's diagnostics recorded, for tests that need to see a warning.
func runTestRecordingApplyDiagnostics(t *testing.T, tc resource.TestCase) *applyDiagnostics {
	t.Helper()
	recorded := &applyDiagnostics{}
	inner := providerserver.NewProtocol6WithError(New("test")())
	tc.ProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
		"flightdeck": func() (tfprotov6.ProviderServer, error) {
			server, err := inner()
			if err != nil {
				return nil, err
			}
			return diagnosticRecordingServer{ProviderServer: server, into: recorded}, nil
		},
	}
	resource.UnitTest(t, tc)
	return recorded
}

func regexMust(pattern string) *regexp.Regexp { return regexp.MustCompile(pattern) }

// randIdentifier returns a project identifier that satisfies the model's
// /\A[A-Z][A-Z0-9]{0,9}\z/ rule and is unique enough for a shared workspace.
func randIdentifier() string {
	return "T" + strings.ToUpper(acctest.RandStringFromCharSet(7, "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"))
}

func randName(prefix string) string {
	return fmt.Sprintf("%s %s", prefix, strings.ToLower(acctest.RandString(6)))
}

// tfjsonPath builds a tfjsonpath for a top-level attribute.
func tfjsonPath(attr string) tfjsonpath.Path { return tfjsonpath.New(attr) }
