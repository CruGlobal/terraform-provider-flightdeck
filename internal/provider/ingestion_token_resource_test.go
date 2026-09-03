package provider

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const tokenRes = "flightdeck_ingestion_token.test"

func tokenConfig(env *testEnv, identifier, body string) string {
	return env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_project" "parent" {
  name       = "Parent %s"
  identifier = %q
  features = {
    errors = true
  }
}

resource "flightdeck_ingestion_token" "test" {
  project_id = flightdeck_project.parent.id
%s
}
`, identifier, identifier, body)
}

func TestIngestionToken_basicLifecycle(t *testing.T) {
	env := newTestEnv(t, "ingestion_token")
	identifier := randIdentifier()
	var id, token string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: tokenConfig(env, identifier, `  name = "api"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(tokenRes, "id"),
					resource.TestCheckResourceAttr(tokenRes, "name", "api"),
					resource.TestCheckResourceAttr(tokenRes, "environment", "production"),
					resource.TestCheckResourceAttr(tokenRes, "scope", "post_server_item"),
					resource.TestMatchResourceAttr(tokenRes, "token", regexMust(`^fd_post_`)),
					resource.TestCheckResourceAttrSet(tokenRes, "last_four"),
					captureAttr(tokenRes, "id", &id),
					captureAttr(tokenRes, "token", &token),
					func(s *terraform.State) error {
						rs := s.RootModule().Resources[tokenRes].Primary
						tok, last := rs.Attributes["token"], rs.Attributes["last_four"]
						if len(tok) < 4 || tok[len(tok)-4:] != last {
							return fmt.Errorf("last_four %q does not match token %q", last, tok)
						}
						return nil
					},
				),
			},
			{
				// A refresh must keep the token: the API never returns it again.
				RefreshState: true,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(tokenRes, "token", &token),
				),
			},
			{
				// Import cannot recover the value, and says so.
				ResourceName:            tokenRes,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"token"},
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs := s.RootModule().Resources[tokenRes].Primary
					return rs.Attributes["project_id"] + "/" + rs.ID, nil
				},
			},
			{
				// Any change replaces the token; the new one has a new value.
				Config: tokenConfig(env, identifier, `
  name        = "api"
  environment = "staging"
  scope       = "post_client_item"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(tokenRes, plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(tokenRes, "environment", "staging"),
					resource.TestCheckResourceAttr(tokenRes, "scope", "post_client_item"),
					func(s *terraform.State) error {
						rs := s.RootModule().Resources[tokenRes].Primary
						if rs.ID == id {
							return fmt.Errorf("expected a new token id after replacement")
						}
						if rs.Attributes["token"] == token {
							return fmt.Errorf("expected a new token value after replacement")
						}
						return nil
					},
				),
			},
		},
	})
	if !env.live() {
		if old := env.fake.IngestionToken(mustInt(id)); old == nil || old.RevokedAt == nil {
			t.Fatalf("replaced token %s should have been revoked, got %+v", id, old)
		}
	}
}

func TestIngestionToken_revokedOutOfBandIsRecreated(t *testing.T) {
	env := newTestEnv(t, "ingestion_token")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: tokenConfig(env, identifier, `  name = "api"`),
				Check:  captureAttr(tokenRes, "id", &id),
			},
			{
				PreConfig: func() { env.fake.RevokeIngestionTokenOutOfBand(mustInt(id)) },
				Config:    tokenConfig(env, identifier, `  name = "api"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(tokenRes, plancheck.ResourceActionCreate)},
				},
				Check: func(s *terraform.State) error {
					if s.RootModule().Resources[tokenRes].Primary.ID == id {
						return fmt.Errorf("expected a new token after out-of-band revoke")
					}
					return nil
				},
			},
		},
	})
}

func TestIngestionToken_validation(t *testing.T) {
	env := newTestEnv(t, "ingestion_token")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: tokenConfig(env, identifier, `
  name  = "x"
  scope = "post_everything"`),
				ExpectError: regexMust(`value must be one of`),
			},
			{
				Config:      tokenConfig(env, identifier, `  name = ""`),
				ExpectError: regexMust(`string length must be at least 1`),
			},
		},
	})
}

func TestIngestionToken_tokenIsSensitiveInPlanOutput(t *testing.T) {
	env := newTestEnv(t, "ingestion_token")
	env.requireFake(t)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: tokenConfig(env, identifier, `  name = "api"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectSensitiveValue(tokenRes, tfjsonPath("token"))},
				},
			},
		},
	})
}

func TestIngestionToken_replayedCreateIsRetiredAndMintedAfresh(t *testing.T) {
	env := newTestEnv(t, "ingestion_token")
	env.requireFake(t)
	identifier := randIdentifier()
	var first, firstToken string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: tokenConfig(env, identifier, `  name = "api"`),
				Check:  resource.ComposeAggregateTestCheckFunc(captureAttr(tokenRes, "id", &first), captureAttr(tokenRes, "token", &firstToken)),
			},
			{
				// Destroy: revokes the token; the fake still holds the redacted
				// cached 201 for this declaration's Idempotency-Key.
				Config: env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_project" "parent" {
  name       = "Parent %s"
  identifier = %q
  features = {
    errors = true
  }
}
`, identifier, identifier),
			},
			{
				// Recreate the identical declaration inside the window: the API
				// replays the revoked row WITHOUT its secret. The provider must not
				// record that; it revokes the replay (a no-op here) and mints a fresh
				// token under a new key, whose secret it does know.
				Config: tokenConfig(env, identifier, `  name = "api"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestMatchResourceAttr(tokenRes, "token", regexMust(`^fd_post_`)),
					func(s *terraform.State) error {
						rs := s.RootModule().Resources[tokenRes].Primary
						if rs.ID == first {
							return fmt.Errorf("recreate recorded the replayed, revoked token %s", first)
						}
						if rs.Attributes["token"] == firstToken {
							return fmt.Errorf("recreate recorded the old secret")
						}
						return nil
					},
				),
			},
		},
	})
	var posts, keys []string
	for _, r := range env.fake.RequestsMatching("POST", "/api/v1/projects/") {
		if strings.HasSuffix(r.Path, "/ingestion-tokens") {
			posts = append(posts, r.Path)
			keys = append(keys, r.Header.Get("Idempotency-Key"))
		}
	}
	// original, replayed, fresh-key — and the stable key is never re-sent after
	// the replay answered.
	if len(posts) != 3 || keys[0] != keys[1] || keys[2] == keys[0] {
		t.Fatalf("POSTs=%d keys=%v", len(posts), keys)
	}
	if old := env.fake.IngestionToken(mustInt(first)); old == nil || old.RevokedAt == nil {
		t.Fatalf("the replayed token should be revoked: %+v", old)
	}
}

func TestIngestionToken_revokeSendsIfMatchAndAnswers200(t *testing.T) {
	env := newTestEnv(t, "ingestion_token")
	env.requireFake(t)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: tokenConfig(env, identifier, `  name = "api"`)},
			{Config: projectFixture(env, identifier)},
		},
	})
	var deletes []flightdecktestRequest
	for _, r := range env.fake.RequestsMatching("DELETE", "/api/v1/projects/") {
		if strings.Contains(r.Path, "/ingestion-tokens/") {
			deletes = append(deletes, r)
		}
	}
	if len(deletes) != 1 || deletes[0].Header.Get("If-Match") == "" || deletes[0].Status != 200 {
		t.Fatalf("revoke DELETE = %+v", deletes)
	}
}
