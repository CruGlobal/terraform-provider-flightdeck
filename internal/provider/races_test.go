package provider

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/flightdecktest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// The API refuses a GET or HEAD that carries any body with 400
// body_not_allowed, before it checks the token. The fake refuses it the same
// way, so every test in this package would fail on such a read; this one also
// walks the reads a lifecycle, an import and every data source make, and
// checks each one went out without a body.
func TestReads_neverSendABody(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	config := projectConfig(env, identifier, `  name = "No bodies"`) + `
data "flightdeck_project" "by_id" {
  id = flightdeck_project.test.id
}

data "flightdeck_project" "by_identifier" {
  identifier = flightdeck_project.test.identifier
}

data "flightdeck_states" "all" {
  project_id = flightdeck_project.test.id
}

data "flightdeck_workspace_member" "owner" {
  email = "owner@example.com"
}
`
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: config},
			{ResourceName: "flightdeck_project.test", ImportState: true, ImportStateId: identifier},
		},
	})
	reads := 0
	for _, r := range env.fake.Requests() {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			continue
		}
		reads++
		if len(r.Body) > 0 || r.Header.Get("Content-Type") != "" {
			t.Errorf("%s %s?%s sent a body (%q, Content-Type %q)", r.Method, r.Path, r.Query, r.Body, r.Header.Get("Content-Type"))
		}
		if r.Status == http.StatusBadRequest {
			t.Errorf("%s %s?%s was refused: %s", r.Method, r.Path, r.Query, r.Response)
		}
	}
	if reads < 5 {
		t.Errorf("only %d reads were made; the test is not exercising the read paths", reads)
	}
}

// Two racing deletes of one project: one gets the 202, the other a 404, and a
// 404 on destroy means the project is already gone.
func TestProject_destroyOfAProjectAlreadyDeletedSucceeds(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `  name = "Deleted twice"`),
				Check:  captureAttr("flightdeck_project.test", "id", &id),
			},
			{
				// The other delete lands between this apply's refresh and its DELETE.
				PreConfig: func() {
					env.fake.OnNextRequest(http.MethodDelete, "/api/v1/projects/"+id, func() {
						env.fake.DeleteProjectOutOfBand(mustID(t, id))
					})
				},
				Config: env.providerConfig(),
				Check: func(*terraform.State) error {
					if got := countRequests(env, http.MethodDelete, "/api/v1/projects/"+id); got != 1 {
						return fmt.Errorf("DELETEs = %d, want 1", got)
					}
					return nil
				},
			},
		},
	})
}

// A delete that loses a race to a write adding something to the project
// mid-delete is refused with 409 stale_object, and the API says to send the
// DELETE again. The provider does, once.
func TestProject_deleteThatLostARaceIsSentAgain(t *testing.T) {
	env := newTestEnv(t, "project")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `  name = "Raced"`),
				Check:  captureAttr("flightdeck_project.test", "id", &id),
			},
			{
				PreConfig: func() {
					env.fake.RefuseNext(http.MethodDelete, "/projects/"+id, http.StatusConflict, "stale_object",
						flightdecktest.LostRaceDeleteMessage)
				},
				Config: env.providerConfig(),
				Check: func(*terraform.State) error {
					if got := countRequests(env, http.MethodDelete, "/api/v1/projects/"+id); got != 2 {
						return fmt.Errorf("DELETEs = %d, want 2 (the refused one, then the one that deleted)", got)
					}
					return nil
				},
			},
		},
	})
}

// A delete wins over an update sent at the same moment. The update gets a
// 404, or (already part way through) a 409 whose re-read then finds nothing.
// Either way the error says the project is gone, not that someone edited it.
func TestProject_updateRacingADeleteSaysItIsGone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		raced func(env *testEnv, id string)
	}{
		{"404", func(*testEnv, string) {}},
		{"409 then 404", func(env *testEnv, id string) {
			env.fake.RefuseNext(http.MethodPatch, "/projects/"+id, http.StatusConflict, "stale_object",
				flightdecktest.LostRaceWriteMessage)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, "project")
			env.requireFake(t)
			identifier := randIdentifier()
			var id string
			runTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{
						Config: projectConfig(env, identifier, `  name = "Before"`),
						Check:  captureAttr("flightdeck_project.test", "id", &id),
					},
					{
						PreConfig: func() {
							env.fake.OnNextRequest(http.MethodPatch, "/api/v1/projects/"+id, func() {
								env.fake.DeleteProjectOutOfBand(mustID(t, id))
							})
							tc.raced(env, id)
						},
						Config:      projectConfig(env, identifier, `  name = "After"`),
						ExpectError: regexMust(`(?s)Project\s+` + identifier + `\s+is\s+gone.*deleted\s+while\s+this\s+apply`),
					},
				},
			})
		})
	}
}

// The project update can land and the project be deleted before the next
// write, here its self-healing settings. That write's 404 is the project
// being gone, not a Flightdeck too old to have the endpoint.
func TestProject_settingsWriteAfterTheProjectWentSaysItIsGone(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	config := func(name string, bake int) string {
		return projectConfig(env, identifier, fmt.Sprintf(`
  name = %q
  self_healing = {
    bake_minutes = %d
  }`, name, bake))
	}
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: config("Before", 30),
				Check:  captureAttr("flightdeck_project.test", "id", &id),
			},
			{
				PreConfig: func() {
					env.fake.OnNextRequest(http.MethodPatch, "/api/v1/projects/"+id+"/self-healing", func() {
						env.fake.DeleteProjectOutOfBand(mustID(t, id))
					})
				},
				Config:      config("After", 45),
				ExpectError: regexMust(`(?s)Project\s+` + identifier + `\s+is\s+gone.*deleted\s+while\s+this\s+apply`),
			},
		},
	})
}

// A write that names something deleted at the same moment gets 409
// stale_object although the record itself did not change. The error says
// so, rather than claiming somebody edited it.
func TestLabel_writeThatLostARaceToADeleteSaysSo(t *testing.T) {
	env := newTestEnv(t, "label")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: labelConfig(env, identifier, `  name = "Before"`),
				Check:  captureAttr(labelRes, "id", &id),
			},
			{
				PreConfig: func() {
					env.fake.RefuseNext(http.MethodPatch, "/labels/"+id, http.StatusConflict, "stale_object",
						flightdecktest.LostRaceWriteMessage)
				},
				Config: labelConfig(env, identifier, `  name = "After"`),
				ExpectError: regexMust(`(?s)was\s+not\s+written:\s+the\s+write\s+lost\s+a\s+race.*still\s+reads\s+back\s+the\s+lock_version` +
					`.*likely\s+cause.*deleted\s+at\s+the\s+same\s+moment.*Something\s+this\s+request\s+refers\s+to\s+was\s+deleted`),
			},
		},
	})
}

// A 409 without a code (an older server's uniqueness conflict, say) is not
// read as a lost race, even when the record's version has not moved.
func TestLabel_uncodedConflictKeepsThePlainStaleMessage(t *testing.T) {
	env := newTestEnv(t, "label")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: labelConfig(env, identifier, `  name = "Before"`),
				Check:  captureAttr(labelRes, "id", &id),
			},
			{
				PreConfig: func() {
					env.fake.OmitErrorCodes(true)
					env.fake.RefuseNext(http.MethodPatch, "/labels/"+id, http.StatusConflict, "", "Name has already been taken")
				},
				Config:      labelConfig(env, identifier, `  name = "After"`),
				ExpectError: regexMust(`(?s)modified outside of Terraform.*Name\s+has\s+already\s+been\s+taken`),
			},
			{
				PreConfig: func() { env.fake.OmitErrorCodes(false) },
				Config:    labelConfig(env, identifier, `  name = "After"`),
			},
		},
	})
}

// A 404 on a write says what a 404 can mean, since the API answers one for a
// project the token cannot see as well as for an id that does not exist.
func TestLabel_updateAnsweredNotFoundExplainsWhy(t *testing.T) {
	env := newTestEnv(t, "label")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: labelConfig(env, identifier, `  name = "Before"`),
				Check:  captureAttr(labelRes, "id", &id),
			},
			{
				PreConfig: func() {
					env.fake.RefuseNext(http.MethodPatch, "/labels/"+id, http.StatusNotFound, "not_found", "Not found")
				},
				Config:      labelConfig(env, identifier, `  name = "After"`),
				ExpectError: regexMust(`(?s)Error updating Flightdeck label.*HTTP 404.*cannot\s+reach.*project\s+the\s+token\s+cannot\s+see`),
			},
		},
	})
}

func mustID(t *testing.T, raw string) int64 {
	t.Helper()
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("id %q: %v", raw, err)
	}
	return id
}

// countRequests counts the requests with exactly this method and path.
func countRequests(env *testEnv, method, path string) int {
	n := 0
	for _, r := range env.fake.Requests() {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}
