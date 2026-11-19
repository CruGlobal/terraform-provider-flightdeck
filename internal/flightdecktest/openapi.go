package flightdecktest

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// openapiDocument is Flightdeck's OpenAPI 3.1 description of /api/v1, the
// document a Flightdeck instance serves at /api/v1/openapi.json. Every answer
// the fake gives to an /api/v1 request is checked against it, so an answer the
// real API would never give fails the test that got it. openapi/README.md says
// where the copy came from and how to refresh it.
//
//go:embed openapi/flightdeck-v1.json
var openapiDocument []byte

const (
	documentName = "openapi/flightdeck-v1.json"
	// documentURL is only the name the schema compiler knows the document
	// by, so the document's own "#/components/..." references resolve.
	documentURL = "https://flightdeck.example.com/api/v1/openapi.json"
	// maxSchemaErrors caps how many mismatches one failure lists.
	maxSchemaErrors = 12
)

// english is the printer for the validator's messages.
var english = message.NewPrinter(language.English)

// loadContract reads and compiles the document once per test process.
var loadContract = sync.OnceValues(func() (*contract, error) {
	return newContract(openapiDocument)
})

// olderContracts caches, by gap list, the document as an older release
// would have it (contractFor).
var olderContracts = struct {
	sync.Mutex
	byGaps map[string]*contract
}{byGaps: map[string]*contract{}}

// contractFor is the document with gaps applied: each listed property taken
// out of the `required` list of the components/schemas entry it is listed
// under, because the fake is imitating an older Flightdeck that didn't send
// it yet (see olderReleaseGaps). Nothing else changes, so an answer that
// leaves such a property out is still refused for an extra property, a wrong
// type, another missing property or an undocumented status. A gap naming a
// property the document doesn't require is an error: the gap list is stale.
// Each distinct gap list is compiled once per test process.
func contractFor(gaps map[string][]string) (*contract, error) {
	if len(gaps) == 0 {
		return loadContract()
	}
	schemas := make([]string, 0, len(gaps))
	for name := range gaps {
		schemas = append(schemas, name)
	}
	sort.Strings(schemas)
	props := map[string][]string{}
	var key strings.Builder
	for _, name := range schemas {
		list := append([]string(nil), gaps[name]...)
		sort.Strings(list)
		props[name] = slices.Compact(list)
		key.WriteString(name + ":" + strings.Join(props[name], ",") + ";")
	}

	olderContracts.Lock()
	defer olderContracts.Unlock()
	if c := olderContracts.byGaps[key.String()]; c != nil {
		return c, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(openapiDocument, &doc); err != nil {
		return nil, err
	}
	components, _ := doc["components"].(map[string]any)
	all, _ := components["schemas"].(map[string]any)
	for _, name := range schemas {
		schema, _ := all[name].(map[string]any)
		required, _ := schema["required"].([]any)
		for _, prop := range props[name] {
			i := slices.Index(required, any(prop))
			if i < 0 {
				return nil, fmt.Errorf("%s: an older-release gap names %s.%s, which the document doesn't require; take it out of olderReleaseGaps", documentName, name, prop)
			}
			required = slices.Delete(required, i, i+1)
		}
		schema["required"] = required
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	c, err := newContract(raw)
	if err != nil {
		return nil, err
	}
	olderContracts.byGaps[key.String()] = c
	return c, nil
}

// contract answers "would Flightdeck's API give this answer?" for one parsed
// OpenAPI document. Its schemas are compiled when it is built, and it never
// changes after that, so it is safe to share between requests.
type contract struct {
	doc     map[string]any
	routes  []route
	schemas map[string]*jsonschema.Schema // by the JSON pointer of the schema
}

// httpVerbs are the keys of a path item that are operations.
var httpVerbs = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

// route is one path template, split into segments; a parameter segment
// ("{id}") is held as "".
type route struct {
	template string
	segments []string
	params   int
}

// answer is one response the fake sent.
type answer struct {
	method, path string // path as sent on the wire (escaped)
	status       int
	contentType  string
	body         []byte
}

func newContract(raw []byte) (*contract, error) {
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%s is not JSON: %w", documentName, err)
	}
	doc, ok := parsed.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is not a JSON object", documentName)
	}
	if v, _ := doc["openapi"].(string); !strings.HasPrefix(v, "3.1.") {
		return nil, fmt.Errorf("%s is openapi %q, not 3.1", documentName, v)
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s has no paths", documentName)
	}

	compiler := jsonschema.NewCompiler()
	// OpenAPI 3.1's schemas are JSON Schema 2020-12. A format (date-time,
	// date, uri) is checked too, not just read as a note.
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	if err := compiler.AddResource(documentURL, doc); err != nil {
		return nil, err
	}

	c := &contract{doc: doc, schemas: map[string]*jsonschema.Schema{}}
	for template, item := range paths {
		r := route{template: template, segments: strings.Split(strings.Trim(template, "/"), "/")}
		for i, seg := range r.segments {
			if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
				r.segments[i] = ""
				r.params++
			}
		}
		c.routes = append(c.routes, r)

		ops, _ := item.(map[string]any)
		for verb, op := range ops {
			if !httpVerbs[verb] {
				continue // the path's own parameters, summary, ...
			}
			opObj, _ := op.(map[string]any)
			responses, _ := opObj["responses"].(map[string]any)
			if responses == nil {
				return nil, fmt.Errorf("%s: %s %s lists no responses", documentName, strings.ToUpper(verb), template)
			}
			for key := range responses {
				response, ptr, err := c.response(template, verb, key)
				if err != nil {
					return nil, err
				}
				content, _ := response["content"].(map[string]any)
				for mediaType, media := range content {
					mediaObj, _ := media.(map[string]any)
					if _, ok := mediaObj["schema"]; !ok {
						continue
					}
					schemaPtr := ptr + "/content/" + escapePointer(mediaType) + "/schema"
					if _, done := c.schemas[schemaPtr]; done {
						continue
					}
					sch, err := compiler.Compile(documentURL + "#" + encodeFragment(schemaPtr))
					if err != nil {
						return nil, fmt.Errorf("%s: the schema at %s doesn't compile: %w", documentName, schemaPtr, err)
					}
					c.schemas[schemaPtr] = sch
				}
			}
		}
	}
	// Most literal first, so /projects/{id} never shadows a fixed path.
	sort.Slice(c.routes, func(i, j int) bool {
		if c.routes[i].params != c.routes[j].params {
			return c.routes[i].params < c.routes[j].params
		}
		return c.routes[i].template < c.routes[j].template
	})
	return c, nil
}

// response is the response object documented for a template, verb and status
// key, following a $ref into components.responses, and its JSON pointer.
func (c *contract) response(template, verb, key string) (map[string]any, string, error) {
	ptr := "/paths/" + escapePointer(template) + "/" + verb + "/responses/" + escapePointer(key)
	response, _ := c.lookup(ptr).(map[string]any)
	if ref, ok := response["$ref"].(string); ok {
		if !strings.HasPrefix(ref, "#/") {
			return nil, "", fmt.Errorf("%s: %s refers outside the document (%s)", documentName, ptr, ref)
		}
		ptr = ref[1:]
		response, _ = c.lookup(ptr).(map[string]any)
	}
	if response == nil {
		return nil, "", fmt.Errorf("%s: no response object at %s", documentName, ptr)
	}
	return response, ptr, nil
}

// lookup follows a JSON pointer into the document.
func (c *contract) lookup(ptr string) any {
	var node any = c.doc
	for _, tok := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		node = m[tok]
	}
	return node
}

// match finds the path template a router would send this path to: segment by
// segment, a parameter matching any one non-empty segment and a fixed segment
// only itself, the most literal template winning. Like Flightdeck's router it
// ignores a trailing slash and repeated slashes.
func (c *contract) match(path string) (string, bool) {
	var segments []string
	for _, seg := range strings.Split(path, "/") {
		if seg != "" {
			segments = append(segments, seg)
		}
	}
	for _, r := range c.routes {
		if len(r.segments) != len(segments) {
			continue
		}
		matched := true
		for i, seg := range r.segments {
			if seg != "" && seg != segments[i] {
				matched = false
				break
			}
		}
		if matched {
			return r.template, true
		}
	}
	return "", false
}

// check returns "" when the document describes the answer, and otherwise
// what is wrong with it: the operation, the status and each mismatch.
func (c *contract) check(a answer) string {
	verb := strings.ToLower(a.method)
	if verb == "head" {
		verb = "get"
	}
	template, ok := c.match(a.path)
	if !ok {
		return fmt.Sprintf("%s %s answered %d, but no path in the document matches it.", a.method, a.path, a.status)
	}
	op, _ := c.lookup("/paths/" + escapePointer(template) + "/" + verb).(map[string]any)
	if op == nil {
		return fmt.Sprintf("%s %s answered %d, but the document has no %s operation for %s.", a.method, a.path, a.status, a.method, template)
	}
	name := a.method + " " + template
	if id, _ := op["operationId"].(string); id != "" {
		name += " (" + id + ")"
	}
	name += " answered " + strconv.Itoa(a.status)

	responses, _ := op["responses"].(map[string]any)
	key := ""
	for _, candidate := range []string{strconv.Itoa(a.status), strconv.Itoa(a.status/100) + "XX", "default"} {
		if _, ok := responses[candidate]; ok {
			key = candidate
			break
		}
	}
	if key == "" {
		listed := make([]string, 0, len(responses))
		for k := range responses {
			listed = append(listed, k)
		}
		sort.Strings(listed)
		return fmt.Sprintf("%s, a status the document doesn't list for it (it lists %s).", name, strings.Join(listed, ", "))
	}
	response, ptr, err := c.response(template, verb, key)
	if err != nil {
		return err.Error()
	}
	if a.method == http.MethodHead {
		return ""
	}

	content, _ := response["content"].(map[string]any)
	if len(content) == 0 {
		if len(bytes.TrimSpace(a.body)) > 0 {
			return fmt.Sprintf("%s with a body, but the document says %s has no body.", name, key)
		}
		return ""
	}
	mediaType, _, _ := mime.ParseMediaType(a.contentType)
	if _, ok := content[mediaType]; !ok {
		documented := make([]string, 0, len(content))
		for k := range content {
			documented = append(documented, k)
		}
		sort.Strings(documented)
		shown := a.contentType
		if shown == "" {
			shown = "no Content-Type"
		}
		return fmt.Sprintf("%s as %s, but the document says %s is %s.", name, shown, key, strings.Join(documented, ", "))
	}
	sch := c.schemas[ptr+"/content/"+escapePointer(mediaType)+"/schema"]
	if sch == nil {
		return ""
	}
	if len(bytes.TrimSpace(a.body)) == 0 {
		return fmt.Sprintf("%s with no body, but the document says %s has a %s body.", name, key, mediaType)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(a.body))
	if err != nil {
		return fmt.Sprintf("%s with a body that isn't JSON: %v", name, err)
	}
	verr, ok := sch.Validate(instance).(*jsonschema.ValidationError)
	if !ok || verr == nil {
		return ""
	}
	mismatches := schemaMismatches(verr)
	if len(mismatches) == 0 {
		return ""
	}
	more := ""
	if len(mismatches) > maxSchemaErrors {
		more = fmt.Sprintf("\n  (and %d more)", len(mismatches)-maxSchemaErrors)
		mismatches = mismatches[:maxSchemaErrors]
	}
	return fmt.Sprintf("%s with a body the document's schema for %s (#%s) refuses:\n  %s%s",
		name, key, ptr, strings.Join(mismatches, "\n  "), more)
}

// mismatch is one leaf of a validation error.
type mismatch struct {
	at, message, schema string
	unevaluated         bool
}

// schemaMismatches flattens a validation error into one line per failure:
// where in the body (a JSON pointer), what is wrong, and which keyword of the
// document said so.
//
// When part of an allOf fails, JSON Schema forgets the properties that part
// matched, so each of them also shows up as an unevaluated property. Those
// lines only echo the real failure, so they are left out while there is one;
// once it is fixed, a truly unknown field still shows.
func schemaMismatches(verr *jsonschema.ValidationError) []string {
	var leaves []mismatch
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, cause := range e.Causes {
				walk(cause)
			}
			return
		}
		m := mismatch{
			at:      instancePointer(e.InstanceLocation),
			message: "does not match",
			schema:  strings.TrimPrefix(e.SchemaURL, documentURL),
		}
		switch k := e.ErrorKind.(type) {
		case *kind.FalseSchema:
			m.message = k.LocalizedString(english)
			if strings.HasSuffix(e.SchemaURL, "/unevaluatedProperties") {
				m.message = "a property the schema doesn't name (unevaluatedProperties is false)"
				m.unevaluated = true
			}
		case *kind.Enum:
			m.message = "got " + shownValue(k.Got) + ", but the " + k.LocalizedString(english)
		case *kind.Const:
			m.message = "got " + shownValue(k.Got) + ", but the " + k.LocalizedString(english)
		default:
			if k != nil {
				m.message = k.LocalizedString(english)
			}
		}
		if kw := e.ErrorKind.KeywordPath(); len(kw) > 0 && !m.unevaluated {
			m.schema += "/" + strings.Join(kw, "/")
		}
		leaves = append(leaves, m)
	}
	walk(verr)

	failures := 0
	for _, m := range leaves {
		if !m.unevaluated {
			failures++
		}
	}
	out := make([]string, 0, len(leaves))
	for _, m := range leaves {
		if failures > 0 && m.unevaluated {
			continue
		}
		at := m.at
		if at == "" {
			at = "(root)"
		}
		out = append(out, fmt.Sprintf("%s: %s [schema %s]", at, m.message, m.schema))
	}
	sort.Strings(out)
	return out
}

// shownValue is a value from the body as JSON, cut short when it is long.
func shownValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	if len(b) > 60 {
		return string(b[:57]) + "..."
	}
	return string(b)
}

func instancePointer(tokens []string) string {
	var b strings.Builder
	for _, tok := range tokens {
		b.WriteByte('/')
		b.WriteString(escapePointer(tok))
	}
	return b.String()
}

// escapePointer escapes one JSON pointer token (RFC 6901).
func escapePointer(tok string) string {
	return strings.ReplaceAll(strings.ReplaceAll(tok, "~", "~0"), "/", "~1")
}

// encodeFragment percent-encodes a JSON pointer for use as a URL fragment,
// keeping its slashes.
func encodeFragment(ptr string) string {
	tokens := strings.Split(ptr, "/")
	for i, tok := range tokens {
		tokens[i] = url.PathEscape(tok)
	}
	return strings.Join(tokens, "/")
}

// checkAnswer holds the fake's answer to an /api/v1 request to Flightdeck's
// OpenAPI document and fails the test when the document doesn't describe it.
// A lost or late answer is checked too: the fake still gave it. A refusal
// standing for a gateway in front of Flightdeck is not (see RefuseNext).
func (s *Server) checkAnswer(r *http.Request, rec *statusRecorder) {
	path := r.URL.EscapedPath()
	if rec.gateway || (path != "/api/v1" && !strings.HasPrefix(path, "/api/v1/")) {
		return
	}
	s.mu.Lock()
	gaps := s.olderReleaseGaps()
	s.mu.Unlock()
	c, err := contractFor(gaps)
	problem := ""
	if err != nil {
		problem = err.Error()
	} else {
		problem = c.check(answer{
			method:      r.Method,
			path:        path,
			status:      rec.status,
			contentType: rec.answerHeader().Get("Content-Type"),
			body:        rec.body.Bytes(),
		})
	}
	if problem == "" {
		return
	}
	// Each mismatch is reported once per test, at the first request that
	// showed it, however many requests repeat it.
	s.mu.Lock()
	seen := s.mismatches[problem]
	s.mismatches[problem] = true
	s.mu.Unlock()
	if !seen {
		s.t.Errorf("flightdecktest: the fake gave an answer Flightdeck's OpenAPI document (%s) doesn't describe. "+
			"Fix the fake, or, if the real API really answers this, the document:\n%s\n  (first seen on %s %s)",
			documentName, problem, r.Method, r.URL.RequestURI())
	}
}

// olderReleaseGaps lists, by schema, the properties the document requires
// that the fake leaves out while a knob has it imitate a Flightdeck release
// older than the document (OmitErrorCodes, SetProjectsLegacy,
// SetAgentWorkRelease and the other Set...Supported(false) and
// Set...Legacy(true) knobs). The document describes only the current API; an
// older release didn't send these yet, and its answers are otherwise held to
// the document like any other. Called with s.mu held.
func (s *Server) olderReleaseGaps() map[string][]string {
	gaps := map[string][]string{}
	add := func(schema string, props ...string) { gaps[schema] = append(gaps[schema], props...) }

	if s.codesOmitted() {
		add("Error", "code")
	}
	if s.projects().legacy {
		add("ProjectFields", "terraform_managed")
	}

	aw := s.agentWorkStore()
	if !aw.knowsModels() {
		add("AgentWork", "models", "send_incidents", "send_incidents_since")
	}
	if !aw.knowsSendErrors() {
		add("AgentWork", "send_errors", "send_errors_since")
	}
	if !aw.knowsWaitForChecks() {
		add("AgentWork", "wait_for_checks")
	}
	if !aw.knowsSendCIFailures() {
		add("AgentWork", "send_ci_failures")
	}
	if !aw.knowsAutoMerge() {
		add("AgentWork", "auto_merge")
	}

	sh := s.selfHealingStore()
	if sh.legacy {
		add("SelfHealing", "rollback", "rollback_blockers")
		add("SelfHealingConfig", "count_browser_errors")
	}
	if !sh.knowsAutoPromote() {
		add("SelfHealing", "auto_promote", "soak_hours", "promote_window", "freeze_dates", "heads_up_hours",
			"require_recent_errors", "auto_promote_blockers", "auto_promote_status")
	}
	if !sh.knowsMonitorRollback() {
		add("SelfHealing", "monitor_rollback", "monitor_rollback_blockers")
	}
	if !sh.knowsHotfixIncidentFixes() {
		add("SelfHealing", hotfixIncidentFixesKey)
	}
	if !sh.knowsRequireRecentErrors() {
		add("SelfHealing", "require_recent_errors")
	}

	if !s.githubIntegrations().readsCIWorkflows() {
		add("GithubIntegration", "ci_workflows")
	}
	if !s.slackChannelStore().modeSupported {
		add("SlackChannel", "slack_channel_mode")
	}
	if s.routingKeys().olderFields {
		add("RoutingKeyFields", "environment", "self_healing_trigger")
	}
	return gaps
}
