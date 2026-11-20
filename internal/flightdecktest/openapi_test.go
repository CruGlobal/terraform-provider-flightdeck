package flightdecktest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// call sends one request to the fake, as the provider would, and returns the
// answer.
func call(t *testing.T, s *Server, method, path, body string) (int, http.Header, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, s.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.Token())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, got
}

// mutate decodes a JSON object, lets edit change it, and encodes it again.
func mutate(t *testing.T, body []byte, edit func(map[string]any)) []byte {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	edit(v)
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func testContract(t *testing.T) *contract {
	t.Helper()
	c, err := loadContract()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// expectProblem fails unless check finds a problem holding every want.
func expectProblem(t *testing.T, c *contract, a answer, wants ...string) {
	t.Helper()
	problem := c.check(a)
	if problem == "" {
		t.Fatalf("%s %s: the check passed an answer it should refuse:\n%s", a.method, a.path, a.body)
	}
	for _, want := range wants {
		if !strings.Contains(problem, want) {
			t.Errorf("%s %s: the problem doesn't say %q:\n%s", a.method, a.path, want, problem)
		}
	}
}

// TestContractEnforcesTheDocument takes real answers from the fake and shows
// the validator holds them to the document: each passes as it is, and each
// fails once it is changed the way a fake drifts. Project and the list rows
// are allOf schemas closed with unevaluatedProperties: false, the case a
// validator that skips unevaluatedProperties lets through.
func TestContractEnforcesTheDocument(t *testing.T) {
	s := New(t)
	c := testContract(t)

	status, header, created := call(t, s, http.MethodPost, "/api/v1/projects",
		`{"project":{"name":"Mobile App","identifier":"APP","description":"The app"}}`)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, created)
	}
	var project struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(created, &project); err != nil {
		t.Fatal(err)
	}
	createdAnswer := func(body []byte) answer {
		return answer{method: http.MethodPost, path: "/api/v1/projects", status: http.StatusCreated,
			contentType: header.Get("Content-Type"), body: body}
	}

	// (d) The fake's own answers pass.
	if problem := c.check(createdAnswer(created)); problem != "" {
		t.Fatalf("the fake's create answer fails the check:\n%s", problem)
	}
	_, listHeader, list := call(t, s, http.MethodGet, "/api/v1/projects", "")
	listAnswer := func(body []byte) answer {
		return answer{method: http.MethodGet, path: "/api/v1/projects", status: http.StatusOK,
			contentType: listHeader.Get("Content-Type"), body: body}
	}
	if problem := c.check(listAnswer(list)); problem != "" {
		t.Fatalf("the fake's list answer fails the check:\n%s", problem)
	}
	awPath := fmt.Sprintf("/api/v1/projects/%d/agent-work", project.ID)
	_, awHeader, agentWork := call(t, s, http.MethodGet, awPath, "")
	awAnswer := func(body []byte) answer {
		return answer{method: http.MethodGet, path: awPath, status: http.StatusOK,
			contentType: awHeader.Get("Content-Type"), body: body}
	}
	if problem := c.check(awAnswer(agentWork)); problem != "" {
		t.Fatalf("the fake's agent-work answer fails the check:\n%s", problem)
	}

	t.Run("an extra field on an allOf object fails", func(t *testing.T) {
		expectProblem(t, c, createdAnswer(mutate(t, created, func(v map[string]any) { v["colour"] = "red" })),
			"POST /api/v1/projects (createProject) answered 201",
			"/colour: a property the schema doesn't name (unevaluatedProperties is false)",
			"[schema #/components/schemas/ProjectDetail/unevaluatedProperties]")
		expectProblem(t, c, listAnswer(mutate(t, list, func(v map[string]any) {
			rows, _ := v["results"].([]any)
			row, _ := rows[0].(map[string]any)
			row["colour"] = "red"
		})),
			"GET /api/v1/projects (listProjects) answered 200",
			"/results/0/colour: a property the schema doesn't name",
			"[schema #/components/schemas/Project/unevaluatedProperties]")
	})

	t.Run("a wrong type on a [string, null] field fails", func(t *testing.T) {
		expectProblem(t, c, createdAnswer(mutate(t, created, func(v map[string]any) { v["description"] = 5 })),
			"/description: got number, want null or string",
			"[schema #/components/schemas/ProjectFields/properties/description/type]")
		expectProblem(t, c, awAnswer(mutate(t, agentWork, func(v map[string]any) { v["updated_at"] = true })),
			"GET /api/v1/projects/{project_id}/agent-work (getAgentWork) answered 200",
			"/updated_at: got boolean, want null or string")
	})

	t.Run("a missing required field fails", func(t *testing.T) {
		expectProblem(t, c, createdAnswer(mutate(t, created, func(v map[string]any) { delete(v, "identifier") })),
			"(root): missing property 'identifier'",
			"[schema #/components/schemas/ProjectFields/required]")
		expectProblem(t, c, awAnswer(mutate(t, agentWork, func(v map[string]any) { delete(v, "blockers") })),
			"(root): missing property 'blockers' [schema #/components/schemas/AgentWork/required]")
	})

	t.Run("a format, const and enum are enforced", func(t *testing.T) {
		expectProblem(t, c, createdAnswer(mutate(t, created, func(v map[string]any) { v["created_at"] = "yesterday" })),
			"/created_at:", "date-time")
		expectProblem(t, c, createdAnswer(mutate(t, created, func(v map[string]any) { v["network"] = "secret_project" })),
			`/network: got "secret_project", but the value must be one of 'private_project', 'public_project'`)
		expectProblem(t, c, answer{method: http.MethodDelete, path: fmt.Sprintf("/api/v1/projects/%d", project.ID),
			status: http.StatusAccepted, contentType: "application/json",
			body: []byte(`{"id":1,"status":"deleted","message":"gone"}`)},
			`/status: got "deleted", but the value must be 'deleting'`)
		expectProblem(t, c, answer{method: http.MethodGet, path: "/api/v1/projects/1", status: http.StatusNotFound,
			contentType: "application/json", body: []byte(`{"error":"Not found","code":"missing"}`)},
			`/code: got "missing", but the value must be one of`)
	})

	t.Run("statuses, bodies and content types are enforced", func(t *testing.T) {
		expectProblem(t, c, answer{method: http.MethodGet, path: "/api/v1/projects/1", status: http.StatusTeapot,
			contentType: "application/json", body: []byte(`{"error":"short and stout","code":"not_found"}`)},
			"GET /api/v1/projects/{id} (getProject) answered 418, a status the document doesn't list for it (it lists 200, 304, 400, 401, 403, 404, 422, 429).")
		expectProblem(t, c, answer{method: http.MethodDelete, path: "/api/v1/labels/7", status: http.StatusNoContent,
			contentType: "application/json", body: []byte(`{}`)},
			"DELETE /api/v1/labels/{id} (deleteLabel) answered 204 with a body, but the document says 204 has no body.")
		expectProblem(t, c, answer{method: http.MethodGet, path: "/api/v1/projects/1", status: http.StatusOK,
			contentType: "application/json"},
			"answered 200 with no body, but the document says 200 has a application/json body.")
		expectProblem(t, c, answer{method: http.MethodGet, path: "/api/v1/projects/1", status: http.StatusNotFound,
			contentType: "text/plain", body: []byte("404 page not found")},
			"answered 404 as text/plain, but the document says 404 is application/json.")
		expectProblem(t, c, answer{method: http.MethodGet, path: "/api/v1/projects/1/widgets", status: http.StatusNotFound,
			contentType: "application/json", body: []byte(`{"error":"Not found","code":"not_found"}`)},
			"GET /api/v1/projects/1/widgets answered 404, but no path in the document matches it.")
		expectProblem(t, c, answer{method: http.MethodPut, path: "/api/v1/me", status: http.StatusOK,
			contentType: "application/json", body: []byte(`{}`)},
			"the document has no PUT operation for /api/v1/me")
	})
}

// TestContractExcusesOnlyTheNamedGaps shows an older-release gap excuses the
// properties it names on the schema it names, and nothing else, on a plain
// schema (AgentWork) and on one built with allOf (ProjectDetail), where an
// excused property must not make the rest of the object read as unknown.
func TestContractExcusesOnlyTheNamedGaps(t *testing.T) {
	s := New(t)
	c := testContract(t)
	_, _, created := call(t, s, http.MethodPost, "/api/v1/projects", `{"project":{"name":"Mobile App","identifier":"APP"}}`)
	var project struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(created, &project); err != nil {
		t.Fatal(err)
	}
	gapContract := func(gaps map[string][]string) *contract {
		t.Helper()
		gc, err := contractFor(gaps)
		if err != nil {
			t.Fatal(err)
		}
		return gc
	}

	t.Run("a plain schema", func(t *testing.T) {
		path := fmt.Sprintf("/api/v1/projects/%d/agent-work", project.ID)
		_, _, body := call(t, s, http.MethodGet, path, "")
		withoutModels := mutate(t, body, func(v map[string]any) { delete(v, "models") })
		a := answer{method: http.MethodGet, path: path, status: http.StatusOK, contentType: "application/json", body: withoutModels}

		older := gapContract(map[string][]string{"AgentWork": {"models"}})
		if problem := older.check(a); problem != "" {
			t.Fatalf("a gap naming models didn't excuse its absence:\n%s", problem)
		}
		expectProblem(t, c, a, "(root): missing property 'models' [schema #/components/schemas/AgentWork/required]")

		a.body = mutate(t, withoutModels, func(v map[string]any) { delete(v, "enabled") })
		expectProblem(t, older, a, "(root): missing property 'enabled' [schema #/components/schemas/AgentWork/required]")
		a.body = mutate(t, withoutModels, func(v map[string]any) { v["colour"] = "red" })
		expectProblem(t, older, a, "(root): additional properties 'colour' not allowed")
		a.body = mutate(t, withoutModels, func(v map[string]any) { v["enabled"] = "yes" })
		expectProblem(t, older, a, "/enabled: got string, want boolean")
	})

	t.Run("an allOf schema", func(t *testing.T) {
		path := fmt.Sprintf("/api/v1/projects/%d", project.ID)
		_, _, body := call(t, s, http.MethodGet, path, "")
		withoutFlag := mutate(t, body, func(v map[string]any) { delete(v, "terraform_managed") })
		a := answer{method: http.MethodGet, path: path, status: http.StatusOK, contentType: "application/json", body: withoutFlag}

		older := gapContract(map[string][]string{"ProjectFields": {"terraform_managed"}})
		if problem := older.check(a); problem != "" {
			t.Fatalf("a gap naming terraform_managed didn't excuse its absence:\n%s", problem)
		}
		expectProblem(t, c, a, "(root): missing property 'terraform_managed' [schema #/components/schemas/ProjectFields/required]")
		a.body = mutate(t, withoutFlag, func(v map[string]any) { v["colour"] = "red" })
		expectProblem(t, older, a, "/colour: a property the schema doesn't name (unevaluatedProperties is false) [schema #/components/schemas/ProjectDetail/unevaluatedProperties]")
	})

	t.Run("a gap the document doesn't need is an error", func(t *testing.T) {
		if _, err := contractFor(map[string][]string{"SelfHealing": {"models"}}); err == nil ||
			!strings.Contains(err.Error(), "an older-release gap names SelfHealing.models, which the document doesn't require") {
			t.Fatalf("want a stale-gap error, got %v", err)
		}
	})

	t.Run("each knob opens its gap, and the older answer passes", func(t *testing.T) {
		s.SetAgentWorkModelsSupported(false)
		s.SetProjectsLegacy(true)
		s.mu.Lock()
		gaps := s.olderReleaseGaps()
		s.mu.Unlock()
		if got := strings.Join(gaps["AgentWork"], ","); got != "models,send_incidents,send_incidents_since" {
			t.Errorf("SetAgentWorkModelsSupported(false) opened the AgentWork gap %q", got)
		}
		if got := strings.Join(gaps["ProjectFields"], ","); got != "terraform_managed" {
			t.Errorf("SetProjectsLegacy(true) opened the ProjectFields gap %q", got)
		}
		older := gapContract(gaps)
		for _, path := range []string{fmt.Sprintf("/api/v1/projects/%d/agent-work", project.ID), fmt.Sprintf("/api/v1/projects/%d", project.ID)} {
			_, _, body := call(t, s, http.MethodGet, path, "")
			if problem := older.check(answer{method: http.MethodGet, path: path, status: http.StatusOK,
				contentType: "application/json", body: body}); problem != "" {
				t.Errorf("the older release's own answer fails the check:\n%s", problem)
			}
		}
	})
}

// TestEveryOlderReleaseGapIsOneTheDocumentNeeds turns every older-release
// knob on at once and compiles the document with the gaps they open, so a
// gap the document no longer needs (it dropped the requirement, or renamed
// the schema) fails here rather than in whichever test first used the knob.
func TestEveryOlderReleaseGapIsOneTheDocumentNeeds(t *testing.T) {
	s := New(t)
	s.OmitErrorCodes(true)
	s.SetProjectsLegacy(true)
	s.SetAgentWorkRelease(AgentWorkBeforeAutoMerge)
	s.SetSelfHealingLegacy(true)
	s.SetGithubCIWorkflowsSupport(CIWorkflowsUnknownKey)
	s.SetSlackChannelModeSupported(false)
	s.SetRoutingKeyFieldsSupported(false)
	s.mu.Lock()
	gaps := s.olderReleaseGaps()
	s.mu.Unlock()
	if _, err := contractFor(gaps); err != nil {
		t.Fatal(err)
	}
	for _, schema := range []string{"Error", "ProjectFields", "AgentWork", "SelfHealing", "SelfHealingConfig", "GithubIntegration", "SlackChannel", "RoutingKeyFields"} {
		if len(gaps[schema]) == 0 {
			t.Errorf("no gap opened for %s", schema)
		}
	}
}

// TestContractReadsTheDocument checks a real answer against a copy of the
// document with one property taken out of ProjectFields: the same answer
// now fails, because Project closes its allOf with unevaluatedProperties.
func TestContractReadsTheDocument(t *testing.T) {
	s := New(t)
	_, header, created := call(t, s, http.MethodPost, "/api/v1/projects", `{"project":{"name":"Mobile App","identifier":"APP"}}`)

	var doc map[string]any
	if err := json.Unmarshal(openapiDocument, &doc); err != nil {
		t.Fatal(err)
	}
	components, _ := doc["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	fields, _ := schemas["ProjectFields"].(map[string]any)
	properties, _ := fields["properties"].(map[string]any)
	delete(properties, "emoji")
	names, _ := fields["required"].([]any)
	var required []any
	for _, name := range names {
		if name != "emoji" {
			required = append(required, name)
		}
	}
	fields["required"] = required
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	mutated, err := newContract(raw)
	if err != nil {
		t.Fatal(err)
	}
	a := answer{method: http.MethodPost, path: "/api/v1/projects", status: http.StatusCreated,
		contentType: header.Get("Content-Type"), body: created}
	if problem := testContract(t).check(a); problem != "" {
		t.Fatalf("the answer fails the real document:\n%s", problem)
	}
	expectProblem(t, mutated, a, "/emoji: a property the schema doesn't name (unevaluatedProperties is false)")
}

// recordingT stands in for the test a fake serves, and keeps what the fake
// reports instead of failing.
type recordingT struct {
	testing.TB
	mu     sync.Mutex
	errors []string
}

func (r *recordingT) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recordingT) reported() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.errors...)
}

// TestFakeFailsTheTestOnABadAnswer runs requests through the fake's own
// middleware with a handler that answers a shape the document doesn't
// describe, and shows the test it serves is failed, once per mismatch, with
// the operation, the status and the mismatch.
func TestFakeFailsTheTestOnABadAnswer(t *testing.T) {
	rec := &recordingT{TB: t}
	s := New(rec)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "name": 42})
	})
	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"user":      map[string]any{"id": 1, "name": "Token Owner", "email": "owner@example.com"},
			"workspace": map[string]any{"id": 1, "slug": "acme", "name": "Acme"},
			"token":     map[string]any{"id": 1, "name": "Terraform", "last_used_at": nil},
		})
	})
	handler := s.middleware(mux)
	serve := func(method, path string) {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+s.Token())
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	serve(http.MethodGet, "/api/v1/projects/12")
	serve(http.MethodGet, "/api/v1/projects/13") // the same mismatch again
	serve(http.MethodGet, "/not-the-api")        // not checked

	got := rec.reported()
	if len(got) != 1 {
		t.Fatalf("want one report, got %d:\n%s", len(got), strings.Join(got, "\n---\n"))
	}
	for _, want := range []string{
		"flightdecktest: the fake gave an answer Flightdeck's OpenAPI document (openapi/flightdeck-v1.json) doesn't describe.",
		"GET /api/v1/projects/{id} (getProject) answered 200 with a body the document's schema for 200",
		"/id: got string, want integer [schema #/components/schemas/ProjectFields/properties/id/type]",
		"/name: got number, want string [schema #/components/schemas/ProjectFields/properties/name/type]",
		"(root): missing properties",
		"(first seen on GET /api/v1/projects/12)",
	} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the report doesn't say %q:\n%s", want, got[0])
		}
	}

	// A refusal the real API never sends (a status getMe doesn't list).
	s.RefuseNext(http.MethodGet, "/api/v1/me", http.StatusTeapot, "not_found", "short and stout")
	serve(http.MethodGet, "/api/v1/me")
	got = rec.reported()
	if len(got) != 2 || !strings.Contains(got[1], "GET /api/v1/me (getMe) answered 418, a status the document doesn't list for it") {
		t.Fatalf("want a report of the 418, got:\n%s", strings.Join(got, "\n---\n"))
	}

	// A good answer reports nothing, and nor does a gateway's error, which
	// stands for something in front of Flightdeck, not the API.
	serve(http.MethodGet, "/api/v1/me")
	s.RefuseNext(http.MethodGet, "/api/v1/me", http.StatusBadGateway, "", "Bad Gateway")
	serve(http.MethodGet, "/api/v1/me")
	if got := rec.reported(); len(got) != 2 {
		t.Fatalf("a good answer or a gateway error was reported:\n%s", got[len(got)-1])
	}
}

// TestFakeHoldsItsRealHandlersToTheDocument drives a few of the fake's real
// handlers, errors included, through a recording test: none is reported.
func TestFakeHoldsItsRealHandlersToTheDocument(t *testing.T) {
	rec := &recordingT{TB: t}
	s := New(rec)
	status, _, created := call(t, s, http.MethodPost, "/api/v1/projects", `{"project":{"name":"Mobile App","identifier":"APP"}}`)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, created)
	}
	call(t, s, http.MethodGet, "/api/v1/projects", "")
	call(t, s, http.MethodGet, "/api/v1/projects/999999", "")
	call(t, s, http.MethodPost, "/api/v1/projects", `{"nope":{}}`)
	s.OmitErrorCodes(true)
	call(t, s, http.MethodGet, "/api/v1/projects/999999", "")
	s.OmitErrorCodes(false)
	s.ThrottleNext(1, 0)
	call(t, s, http.MethodGet, "/api/v1/me", "")
	if got := rec.reported(); len(got) != 0 {
		t.Fatalf("the fake's real answers were reported:\n%s", strings.Join(got, "\n---\n"))
	}
}

func TestContractMatchesPathsLikeARouter(t *testing.T) {
	c := testContract(t)
	for path, want := range map[string]string{
		"/api/v1/projects":                      "/api/v1/projects",
		"/api/v1/projects/":                     "/api/v1/projects",
		"/api/v1/projects/12":                   "/api/v1/projects/{id}",
		"//api/v1/projects/12/":                 "/api/v1/projects/{id}",
		"/api/v1/projects/12/production-lag":    "/api/v1/projects/{project_id}/production-lag",
		"/api/v1/production-lag":                "/api/v1/production-lag",
		"/api/v1/openapi.json":                  "/api/v1/openapi.json",
		"/api/v1/teamspaces/3/members/4":        "/api/v1/teamspaces/{teamspace_id}/members/{user_id}",
		"/api/v1/service-accounts/9/api-tokens": "/api/v1/service-accounts/{service_account_id}/api-tokens",
		"/api/v1/nope":                          "",
		"/api/v1/projects/12/states/4":          "",
	} {
		got, _ := c.match(path)
		if got != want {
			t.Errorf("%s matched %q, want %q", path, got, want)
		}
	}
}

// TestVendoredDocumentNamesNoRealHost keeps the copy in this public
// repository free of real deployments: refresh/ replaces the hosts in its
// servers list, and a refresh that skipped that step fails here.
func TestVendoredDocumentNamesNoRealHost(t *testing.T) {
	allowed := regexp.MustCompile(`^([a-z0-9-]+\.)*example\.com$`)
	for _, m := range regexp.MustCompile(`https?://([^/"#?\s):\\]+)`).FindAllSubmatch(openapiDocument, -1) {
		if host := string(m[1]); !allowed.MatchString(host) {
			t.Errorf("%s names the host %q. Refresh it with refresh/, which replaces the servers' hosts; see openapi/README.md.", documentName, host)
		}
	}
	if !bytes.Contains(openapiDocument, []byte(`"openapi": "3.1.0"`)) {
		t.Errorf("%s is not the indented 3.1 document refresh/ writes", documentName)
	}
}
