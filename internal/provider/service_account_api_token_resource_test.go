package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const saTokenRes = "flightdeck_service_account_api_token.test"

// saTokenOwner is the service account every token test's token belongs to.
func saTokenOwner(env *testEnv, email string) string {
	return env.providerConfig() + fmt.Sprintf(`
resource "flightdeck_service_account" "owner" {
  email_address = %q
}
`, email)
}

func saTokenConfig(env *testEnv, email, body string) string {
	return saTokenOwner(env, email) + fmt.Sprintf(`
resource "flightdeck_service_account_api_token" "test" {
  service_account_id = flightdeck_service_account.owner.id
%s
}
`, body)
}

// inAYear is an expiry a year from now, in UTC and whole seconds, as a
// configuration would write it.
func inAYear() string {
	return time.Now().Add(365 * 24 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339)
}

// tokenPath is the API path of the token in state.
func tokenPath(s *terraform.State) string {
	rs := s.RootModule().Resources[saTokenRes].Primary
	return fmt.Sprintf("/api/v1/service-accounts/%s/api-tokens/%s", rs.Attributes["service_account_id"], rs.ID)
}

func TestServiceAccountAPIToken_basicLifecycle(t *testing.T) {
	env := newTestEnv(t, "service_account")
	email := randBotEmail("token")
	expires := inAYear()
	config := saTokenConfig(env, email, fmt.Sprintf(`
  name       = "Deploy production"
  expires_at = %q`, expires))
	var saID, id, token string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(saTokenRes, "id"),
					resource.TestCheckResourceAttr(saTokenRes, "name", "Deploy production"),
					resource.TestCheckResourceAttr(saTokenRes, "expires_at", expires),
					resource.TestMatchResourceAttr(saTokenRes, "token", regexMust(`^fd_pat_`)),
					resource.TestMatchResourceAttr(saTokenRes, "created_at", regexMust(`^\d{4}-\d{2}-\d{2}T`)),
					captureAttr("flightdeck_service_account.owner", "id", &saID),
					captureAttr(saTokenRes, "id", &id),
					captureAttr(saTokenRes, "token", &token),
					func(s *terraform.State) error {
						rs := s.RootModule().Resources[saTokenRes].Primary
						tok, last, masked := rs.Attributes["token"], rs.Attributes["last_four"], rs.Attributes["masked"]
						if len(last) != 4 || !strings.HasSuffix(tok, last) || !strings.HasSuffix(masked, last) || strings.Contains(masked, tok) {
							return fmt.Errorf("token %q, last_four %q, masked %q do not fit together", tok, last, masked)
						}
						return nil
					},
				),
			},
			{
				// A refresh keeps the secret: the API never returns it again.
				RefreshState: true,
				Check:        resource.TestCheckResourceAttrPtr(saTokenRes, "token", &token),
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Let go of the token without deleting it, then import it.
				Config: saTokenOwner(env, email) + `
removed {
  from = flightdeck_service_account_api_token.test
  lifecycle {
    destroy = false
  }
}
`,
			},
			importStep(config, saTokenRes, func() string { return saID + "/" + id }, nil),
			{
				// The import holds no secret, and Flightdeck's spelling of the
				// expiry. The configuration's spelling of the same time is not
				// a change.
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(saTokenRes, "id", &id),
					resource.TestCheckNoResourceAttr(saTokenRes, "token"),
					resource.TestCheckResourceAttrSet(saTokenRes, "expires_at"),
				),
			},
			{
				// Any change replaces the token; the new one has a new value.
				Config: saTokenConfig(env, email, fmt.Sprintf(`
  name       = "Deploy staging"
  expires_at = %q`, expires)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(saTokenRes, plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(saTokenRes, "name", "Deploy staging"),
					resource.TestMatchResourceAttr(saTokenRes, "token", regexMust(`^fd_pat_`)),
					func(s *terraform.State) error {
						rs := s.RootModule().Resources[saTokenRes].Primary
						if rs.ID == id || rs.Attributes["token"] == token {
							return fmt.Errorf("the replacement kept the old token %s", id)
						}
						return nil
					},
				),
			},
		},
	})
	if !env.live() {
		if old := env.fake.ServiceAccountAPIToken(mustInt(id)); old != nil {
			t.Fatalf("the replaced token %s was not deleted: %+v", id, old)
		}
	}
}

// Flightdeck answers with the expiry in its own zone. State keeps the
// configuration's spelling, nothing shows as a change, and the same time
// written another way is not a change either. Another time is.
func TestServiceAccountAPIToken_expiresAtInAnotherZoneIsNotAChange(t *testing.T) {
	env := newTestEnv(t, "service_account")
	email := randBotEmail("zone")
	at := time.Now().Add(400 * 24 * time.Hour).UTC().Truncate(time.Second)
	utc := at.Format(time.RFC3339)
	plusTwo := at.In(time.FixedZone("", 2*60*60)).Format(time.RFC3339)
	later := at.Add(time.Hour).Format(time.RFC3339)
	config := func(expires string) string {
		return saTokenConfig(env, email, fmt.Sprintf(`
  name       = "Zoned"
  expires_at = %q`, expires))
	}
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config(utc),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(saTokenRes, "expires_at", utc),
					captureAttr(saTokenRes, "id", &id),
					func(s *terraform.State) error {
						if env.live() {
							return nil
						}
						// The fake answers in its own zone, as Flightdeck does.
						reads := env.fake.RequestsMatching(http.MethodGet, tokenPath(s))
						if len(reads) == 0 {
							return fmt.Errorf("no read of the token was recorded")
						}
						var body struct {
							ExpiresAt string `json:"expires_at"`
						}
						if err := json.Unmarshal(reads[0].Response, &body); err != nil {
							return err
						}
						if body.ExpiresAt == utc || !sameInstant(body.ExpiresAt, utc) {
							return fmt.Errorf("the read answered expires_at %q; want %s in another zone", body.ExpiresAt, utc)
						}
						return nil
					},
				),
			},
			{
				RefreshState: true,
				Check:        resource.TestCheckResourceAttr(saTokenRes, "expires_at", utc),
			},
			{
				Config: config(plusTwo),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttrPtr(saTokenRes, "id", &id),
			},
			{
				Config: config(later),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(saTokenRes, plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: resource.TestCheckResourceAttr(saTokenRes, "expires_at", later),
			},
		},
	})
}

// A token in state whose expiry has passed is not replaced, since the
// replacement would get the same expiry and Flightdeck would refuse it. The
// plan warns instead, and names the fix.
func TestServiceAccountAPIToken_expiredTokenWarns(t *testing.T) {
	env := newTestEnv(t, "service_account")
	email := randBotEmail("expired")
	expires := inAYear()
	config := saTokenConfig(env, email, fmt.Sprintf(`
  name       = "Short lived"
  expires_at = %q`, expires))
	t.Cleanup(func() { timeNow = time.Now })
	var id string
	recorded := runTestRecordingApplyDiagnostics(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  captureAttr(saTokenRes, "id", &id),
			},
			{
				PreConfig: func() {
					// Two years on, the token has expired.
					timeNow = func() time.Time { return time.Now().Add(2 * 365 * 24 * time.Hour) }
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
	if !anyDiagnostic(recorded.planWarnings(), "This service account API token has expired",
		"API token "+id+" (\"Short lived\")", "expired at "+expires, "Set expires_at to a later time") {
		t.Errorf("no plan warned that token %s expired; plan warnings:\n%s", id, describe(recorded.planWarnings()))
	}
}

// A new token whose expiry has passed fails the plan: Flightdeck would
// refuse to make it.
func TestServiceAccountAPIToken_pastExpiryFailsThePlan(t *testing.T) {
	env := newTestEnv(t, "service_account")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: saTokenConfig(env, randBotEmail("past"), `
  name       = "Dead on arrival"
  expires_at = "2020-01-01T00:00:00Z"`),
				ExpectError: regexMust(`(?s)This expiry has already passed.*2020-01-01T00:00:00Z.*won't\s+make\s+a\s+token`),
			},
		},
	})
}

func TestServiceAccountAPIToken_planTimeValidation(t *testing.T) {
	env := newTestEnv(t, "service_account")
	email := randBotEmail("valid")
	step := func(body, want string) resource.TestStep {
		return resource.TestStep{Config: saTokenConfig(env, email, body), ExpectError: regexMust(want)}
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			step(`  name = " "`, `(?s)Blank value.*give the token a name`),
			step(`  name = "Deploy "`, `(?s)Blank space at either end`),
			step(fmt.Sprintf(`  name = %q`, strings.Repeat("n", 256)), `(?s)Value too long.*at most 255`),
			step(`
  name       = "Dated"
  expires_at = "2027-06-01"`, `(?s)Not an RFC 3339 timestamp`),
			step(`
  name       = "Zoneless"
  expires_at = "2027-06-01T00:00:00"`, `(?s)Not an RFC 3339 timestamp`),
			step(`
  name       = "Relative"
  expires_at = "tomorrow"`, `(?s)Not an RFC 3339 timestamp`),
			{
				Config: env.providerConfig() + `
resource "flightdeck_service_account_api_token" "test" {
  service_account_id = 0
  name               = "Nobody's"
}
`,
				ExpectError: regexMust(`(?s)service_account_id\s+value\s+must\s+be\s+at\s+least\s+1`),
			},
		},
	})
}

// nextSeptemberFirst is midnight on September 1st next year, in the given
// zone offset: always in the future, whatever the offset.
func nextSeptemberFirst(offset string) string {
	return fmt.Sprintf("%d-09-01T00:00:00%s", time.Now().Year()+1, offset)
}

// Go's RFC 3339 parser takes a zone offset of a day or more (+24:00, and
// even +23:60), which Flightdeck refuses. The plan refuses one too, and
// gives the same time in UTC. An offset just under a day either way works.
func TestServiceAccountAPIToken_zoneOffsetOfADayOrMoreFailsThePlan(t *testing.T) {
	env := newTestEnv(t, "service_account")
	email := randBotEmail("offset")
	withExpiry := func(expires string) string {
		return saTokenConfig(env, email, fmt.Sprintf(`
  name       = "Offset"
  expires_at = %q`, expires))
	}
	refused := func(offset, utc string) resource.TestStep {
		return resource.TestStep{
			Config: withExpiry(nextSeptemberFirst(offset)),
			ExpectError: regexMust(`(?s)Time zone offset out of range.*offset\s+of\s+24\s+hours\s+or\s+more.*` +
				`less\s+than\s+24\s+hours\s+either\s+way.*same\s+time\s+in\s+UTC\s+is\s+"` + utc + `"`),
		}
	}
	year := time.Now().Year() + 1
	justUnder := nextSeptemberFirst("+23:59")
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			refused("+24:00", fmt.Sprintf("%d-08-31T00:00:00Z", year)),
			refused("-24:00", fmt.Sprintf("%d-09-02T00:00:00Z", year)),
			refused("+23:60", fmt.Sprintf("%d-08-31T00:00:00Z", year)),
			refused("+24:30", fmt.Sprintf("%d-08-30T23:30:00Z", year)),
			{
				Config: withExpiry(justUnder),
				Check:  resource.TestCheckResourceAttr(saTokenRes, "expires_at", justUnder),
			},
			{
				Config: withExpiry(justUnder),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config: withExpiry(nextSeptemberFirst("-23:59")),
				Check:  resource.TestCheckResourceAttr(saTokenRes, "expires_at", nextSeptemberFirst("-23:59")),
			},
		},
	})
}

// The fake refuses a zone offset of a day or more with the API's own
// refusal, so it is no kinder than the API to a value that gets past the
// plan.
func TestServiceAccountAPIToken_fakeRefusesAZoneOffsetOfADayOrMore(t *testing.T) {
	env := newTestEnv(t, "service_account")
	env.requireFake(t)
	ctx := context.Background()
	c := webClient(t, env)
	account, _, err := c.CreateServiceAccount(ctx, client.Fields{"email_address": randBotEmail("fakeoffset")}, client.RandomIdempotencyKey())
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []string{"+24:00", "-24:00", "+23:60", "+24:59"} {
		_, err := c.CreateServiceAccountAPIToken(ctx, account.ID,
			client.Fields{"name": "Offset " + offset, "expires_at": nextSeptemberFirst(offset)}, client.RandomIdempotencyKey())
		if !client.HasCode(err, client.CodeInvalidAttribute) ||
			!strings.Contains(apiMessage(err), "expires_at must be an ISO 8601 timestamp") {
			t.Errorf("expires_at with offset %s: err = %v, want the API's 422 invalid_attribute refusal", offset, err)
		}
	}
	for _, offset := range []string{"+23:59", "-23:59"} {
		if _, err := c.CreateServiceAccountAPIToken(ctx, account.ID,
			client.Fields{"name": "Offset " + offset, "expires_at": nextSeptemberFirst(offset)}, client.RandomIdempotencyKey()); err != nil {
			t.Errorf("expires_at with offset %s: %v, want it taken", offset, err)
		}
	}
}

func TestServiceAccountAPIToken_tokenIsSensitiveInPlanOutput(t *testing.T) {
	env := newTestEnv(t, "service_account")
	env.requireFake(t)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: saTokenConfig(env, randBotEmail("secret"), `  name = "api"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectSensitiveValue(saTokenRes, tfjsonPath("token"))},
				},
			},
		},
	})
}

// A create whose response was lost is recovered: the client sends it again
// under the same key, Flightdeck refuses to replay the live token it made
// (nobody holds its value), and the provider deletes that token and makes
// another.
func TestServiceAccountAPIToken_lostCreateResponseIsRecovered(t *testing.T) {
	env := newTestEnv(t, "service_account")
	env.requireFake(t)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				PreConfig: func() { env.fake.DropNextResponse(http.MethodPost, "/api-tokens") },
				Config:    saTokenConfig(env, randBotEmail("lost"), `  name = "lost"`),
				Check: func(s *terraform.State) error {
					lost := lostCreateID(t, env, "/api-tokens")
					rs := s.RootModule().Resources[saTokenRes].Primary
					if rs.ID == fmt.Sprint(lost) || !strings.HasPrefix(rs.Attributes["token"], "fd_pat_") {
						return fmt.Errorf("state = %s (%q), want a fresh token, not the lost %d", rs.ID, rs.Attributes["last_four"], lost)
					}
					if row := env.fake.ServiceAccountAPIToken(lost); row != nil {
						return fmt.Errorf("the token whose response was lost (%d) should be deleted: %+v", lost, row)
					}
					var withheld bool
					for _, r := range env.fake.RequestsMatching(http.MethodPost, "/api/v1/service-accounts/") {
						withheld = withheld || (r.Status == http.StatusConflict && strings.Contains(string(r.Response), "idempotency_replay_withheld"))
					}
					if !withheld {
						return fmt.Errorf("the retry was never refused as withheld; the test is not exercising the recovery")
					}
					return nil
				},
			},
		},
	})
}

// Two tokens with the same name and expiry for one service account are one
// create to the API. While the first is live the second is refused, naming
// it, and nothing is deleted.
func TestServiceAccountAPIToken_duplicateDeclarationIsRefused(t *testing.T) {
	env := newTestEnv(t, "service_account")
	email := randBotEmail("dup")
	config := saTokenConfig(env, email, `  name = "api"`)
	twin := `
resource "flightdeck_service_account_api_token" "twin" {
  service_account_id = flightdeck_service_account.owner.id
  name               = "api"
  depends_on         = [flightdeck_service_account_api_token.test]
}
`
	var id, token string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  resource.ComposeAggregateTestCheckFunc(captureAttr(saTokenRes, "id", &id), captureAttr(saTokenRes, "token", &token)),
			},
			{
				Config:      config + twin,
				ExpectError: regexMust(`(?s)A service account API token with these values already exists.*it\s+holds\s+that\s+token`),
			},
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(saTokenRes, "id", &id),
					resource.TestCheckResourceAttrPtr(saTokenRes, "token", &token),
					func(*terraform.State) error {
						if env.live() {
							return nil
						}
						if row := env.fake.ServiceAccountAPIToken(mustInt(id)); row == nil {
							return fmt.Errorf("the first token %s was deleted", id)
						}
						return nil
					},
				),
			},
		},
	})
}

// Declaring a token again within a day of an identical one that was deleted
// replays the first create, without its secret (`token: null`). The token it
// names is gone, so nothing is deleted, and a fresh create under a new key
// makes a token whose value the provider does know.
func TestServiceAccountAPIToken_replayWithoutTheSecretMakesAFreshToken(t *testing.T) {
	env := newTestEnv(t, "service_account")
	env.requireFake(t)
	email := randBotEmail("replay")
	config := saTokenConfig(env, email, `  name = "api"`)
	var first, firstToken string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  resource.ComposeAggregateTestCheckFunc(captureAttr(saTokenRes, "id", &first), captureAttr(saTokenRes, "token", &firstToken)),
			},
			{Config: saTokenOwner(env, email)},
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestMatchResourceAttr(saTokenRes, "token", regexMust(`^fd_pat_`)),
					func(s *terraform.State) error {
						rs := s.RootModule().Resources[saTokenRes].Primary
						if rs.ID == first || rs.Attributes["token"] == firstToken {
							return fmt.Errorf("the recreate recorded the replayed, deleted token %s", first)
						}
						return nil
					},
				),
			},
		},
	})
	var keys []string
	var replayed bool
	for _, r := range env.fake.RequestsMatching(http.MethodPost, "/api/v1/service-accounts/") {
		if strings.HasSuffix(r.Path, "/api-tokens") {
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			replayed = replayed || strings.Contains(string(r.Response), `"secret_available":false`)
		}
	}
	// The original, its replay, and the fresh-key create.
	if len(keys) != 3 || keys[0] != keys[1] || keys[2] == keys[0] || !replayed {
		t.Fatalf("token creates: keys=%v, a replay without the secret answered: %v", keys, replayed)
	}
}

// A replay is refused only while the token it names is live, and an
// expired token no longer is. Here a token expires, leaves state without
// being deleted, and is declared again within the day the API remembers its
// create: the replay comes back without the secret, nothing is deleted, and
// a fresh create makes a token whose value the provider knows.
func TestServiceAccountAPIToken_replayOfAnExpiredTokenMakesAFreshToken(t *testing.T) {
	env := newTestEnv(t, "service_account")
	env.requireFake(t)
	email := randBotEmail("lapsed")
	config := saTokenConfig(env, email, fmt.Sprintf(`
  name       = "api"
  expires_at = %q`, inAYear()))
	var first string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config, Check: captureAttr(saTokenRes, "id", &first)},
			{
				Config: saTokenOwner(env, email) + `
removed {
  from = flightdeck_service_account_api_token.test
  lifecycle {
    destroy = false
  }
}
`,
			},
			{
				PreConfig: func() { env.fake.ExpireServiceAccountAPITokenOutOfBand(mustInt(first)) },
				Config:    config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestMatchResourceAttr(saTokenRes, "token", regexMust(`^fd_pat_`)),
					func(s *terraform.State) error {
						if id := s.RootModule().Resources[saTokenRes].Primary.ID; id == first {
							return fmt.Errorf("the recreate recorded the replayed, expired token %s", first)
						}
						if env.fake.ServiceAccountAPIToken(mustInt(first)) == nil {
							return fmt.Errorf("the expired token %s was deleted; nothing should be", first)
						}
						return nil
					},
				),
			},
		},
	})
}

// Removing the service account from the workspace deletes its tokens. The
// next refresh finds both gone and removes both from state, and the next
// apply adds the account back (the same id) and makes a new token.
func TestServiceAccountAPIToken_goneWhenItsAccountIsRemoved(t *testing.T) {
	env := newTestEnv(t, "service_account")
	email := randBotEmail("orphan")
	config := saTokenConfig(env, email, `  name = "api"`)
	var saID, id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr("flightdeck_service_account.owner", "id", &saID),
					captureAttr(saTokenRes, "id", &id),
				),
			},
			{
				PreConfig: func() {
					if err := webClient(t, env).DeleteServiceAccount(context.Background(), mustInt(saID)); err != nil {
						t.Fatal(err)
					}
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check: func(s *terraform.State) error {
					if n := len(s.RootModule().Resources); n != 0 {
						return fmt.Errorf("want nothing left in state, have %d resources", n)
					}
					return nil
				},
			},
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr("flightdeck_service_account.owner", "id", &saID),
					resource.TestMatchResourceAttr(saTokenRes, "token", regexMust(`^fd_pat_`)),
					func(s *terraform.State) error {
						if s.RootModule().Resources[saTokenRes].Primary.ID == id {
							return fmt.Errorf("the token deleted with its account came back as %s", id)
						}
						return nil
					},
				),
			},
		},
	})
}

// A token deleted outside Terraform leaves state and is made again. A
// token deleted a moment before its destroy is no error: the DELETE's 404
// means it is gone already.
func TestServiceAccountAPIToken_deletedOutsideTerraform(t *testing.T) {
	env := newTestEnv(t, "service_account")
	env.requireFake(t)
	email := randBotEmail("deleted")
	config := saTokenConfig(env, email, `  name = "api"`)
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config, Check: captureAttr(saTokenRes, "id", &id)},
			{
				PreConfig: func() { env.fake.DeleteServiceAccountAPITokenOutOfBand(mustInt(id)) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(saTokenRes, plancheck.ResourceActionCreate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr(saTokenRes, "id", &id),
					resource.TestMatchResourceAttr(saTokenRes, "token", regexMust(`^fd_pat_`)),
				),
			},
			{
				PreConfig: func() {
					for _, r := range env.fake.Requests() {
						if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/api-tokens") {
							path := r.Path + "/" + id
							env.fake.OnNextRequest(http.MethodDelete, path, func() {
								env.fake.DeleteServiceAccountAPITokenOutOfBand(mustInt(id))
							})
							return
						}
					}
				},
				Config: saTokenOwner(env, email),
				Check: func(*terraform.State) error {
					var statuses []int
					for _, r := range env.fake.Requests() {
						if r.Method == http.MethodDelete && strings.HasSuffix(r.Path, "/api-tokens/"+id) {
							statuses = append(statuses, r.Status)
						}
					}
					if len(statuses) != 1 || statuses[0] != http.StatusNotFound {
						return fmt.Errorf("DELETE statuses = %v, want one 404", statuses)
					}
					return nil
				},
			},
		},
	})
}

// An imported token has no secret. An expired one can't be imported at all.
func TestServiceAccountAPIToken_importHasNoSecretAndRefusesAnExpiredToken(t *testing.T) {
	env := newTestEnv(t, "service_account")
	email := randBotEmail("imported")
	expires := inAYear()
	config := saTokenConfig(env, email, fmt.Sprintf(`
  name       = "api"
  expires_at = %q`, expires))
	t.Cleanup(func() { timeNow = time.Now })
	var saID, id string
	importID := func(*terraform.State) (string, error) { return saID + "/" + id, nil }
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					captureAttr("flightdeck_service_account.owner", "id", &saID),
					captureAttr(saTokenRes, "id", &id),
				),
			},
			{
				ResourceName:            saTokenRes,
				ImportState:             true,
				ImportStateIdFunc:       importID,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"token", "expires_at"},
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("imported %d resources", len(states))
					}
					if tok, ok := states[0].Attributes["token"]; ok && tok != "" {
						return fmt.Errorf("the imported token has a value")
					}
					if !sameInstant(states[0].Attributes["expires_at"], expires) {
						return fmt.Errorf("imported expires_at %q, want %s", states[0].Attributes["expires_at"], expires)
					}
					return nil
				},
			},
			{
				PreConfig:         func() { timeNow = func() time.Time { return time.Now().Add(2 * 365 * 24 * time.Hour) } },
				ResourceName:      saTokenRes,
				ImportState:       true,
				ImportStateIdFunc: importID,
				ExpectError:       regexMust(`(?s)Service account API token has expired.*can't\s+be\s+imported`),
			},
			{
				PreConfig: func() { timeNow = time.Now },
				Config:    config,
			},
		},
	})
}
