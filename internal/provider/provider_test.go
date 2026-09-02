package provider

import (
	"fmt"
	"os"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// protoV6ProviderFactories is wired into every resource.TestCase.
var protoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"flightdeck": providerserver.NewProtocol6WithError(New("test")()),
}

// Environment variables the acceptance tests read. With TF_ACC=1 and
// FLIGHTDECK_ENDPOINT / FLIGHTDECK_TOKEN set, the test bodies run against
// that live Flightdeck (a dedicated test workspace — they create and delete
// projects). Otherwise they run against the in-process fake.
const (
	envAccMemberEmail = "FLIGHTDECK_ACC_MEMBER_EMAIL"
)

// liveReady records which resources' acceptance tests may run against a live
// Flightdeck. A resource is flipped to true once the Flightdeck API change it
// depends on has merged and been deployed to the target instance; until then
// its tests run only against the fake. The unit-test run (no TF_ACC) is
// unaffected by this table.
var liveReady = map[string]bool{
	"project":          false, // FD-786: POST/DELETE /api/v1/projects, lock_version
	"state":            false, // FD-787
	"label":            false, // FD-787
	"project_member":   false, // FD-788
	"workspace_member": false, // FD-788
	"ingestion_token":  false, // FD-788
	"error_alert_rule": false, // FD-788
	"webhook":          false, // FD-788
	"self_healing":     false, // FD-789
}

// testEnv is what a test needs to point the provider at a backend.
type testEnv struct {
	endpoint string
	token    string
	// fake is nil when running against a live Flightdeck.
	fake *flightdecktest.Server
}

func (e *testEnv) live() bool { return e.fake == nil }

// newTestEnv picks the backend. resourceKey gates live runs via liveReady.
func newTestEnv(t *testing.T, resourceKey string) *testEnv {
	t.Helper()
	if os.Getenv(resource.EnvTfAcc) != "" && os.Getenv(envEndpoint) != "" {
		if !liveReady[resourceKey] {
			t.Skipf("live acceptance test for %q is gated until its Flightdeck API change merges (see liveReady)", resourceKey)
		}
		if os.Getenv(envToken) == "" {
			t.Fatalf("%s must be set when %s is set", envToken, envEndpoint)
		}
		return &testEnv{endpoint: os.Getenv(envEndpoint), token: os.Getenv(envToken)}
	}
	fake := flightdecktest.New(t)
	return &testEnv{endpoint: fake.URL, token: fake.Token(), fake: fake}
}

// requireFake skips a test that only makes sense against the fake (fault
// injection, request-log assertions).
func (e *testEnv) requireFake(t *testing.T) {
	t.Helper()
	if e.live() {
		t.Skip("exercises fake-only fault injection")
	}
}

// providerConfig renders a provider block pointing at the backend.
func (e *testEnv) providerConfig() string {
	return fmt.Sprintf(`
provider "flightdeck" {
  endpoint = %q
  token    = %q
}
`, e.endpoint, e.token)
}

// runTest executes a TestCase. resource.UnitTest skips the TF_ACC gate, so the
// same test body runs against the fake in `go test` and against a live
// Flightdeck under `task testacc`; both still need the terraform CLI on PATH.
func runTest(t *testing.T, tc resource.TestCase) {
	t.Helper()
	tc.ProtoV6ProviderFactories = protoV6ProviderFactories
	resource.UnitTest(t, tc)
}
