package main

import (
	"strings"
	"testing"
)

func TestRefreshReplacesTheServersHostsAndIndents(t *testing.T) {
	in := `{"openapi":"3.1.0","info":{"description":"Help: https://pm.corp.test/help and https://pm-stage.corp.test."},` +
		`"servers":[{"url":"https://pm.corp.test","description":"Production"},{"url":"https://pm-stage.corp.test","description":"Stage"}],` +
		`"paths":{"/api/v1/me":{"get":{"externalDocs":{"url":"https://pm.corp.test/help/api#me"}}}}}`
	out, err := refresh([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if strings.Contains(got, "corp.test") {
		t.Errorf("a real host is left:\n%s", got)
	}
	for _, want := range []string{
		`"url": "https://flightdeck.example.com"`,
		`"url": "https://flightdeck-stage.example.com"`,
		`"url": "https://flightdeck.example.com/help/api#me"`,
		`Help: https://flightdeck.example.com/help and https://flightdeck-stage.example.com.`,
		"{\n  \"openapi\": \"3.1.0\",\n  \"info\": {",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the copy doesn't hold %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "}\n") {
		t.Errorf("the copy doesn't end in a newline")
	}
}

func TestRefreshRefusesWhatIsNotAnOpenAPI31Document(t *testing.T) {
	for _, in := range []string{`not json`, `{"openapi":"3.0.3","servers":[]}`, `{"openapi":"3.1.0","servers":[{"url":"/relative"}]}`} {
		if _, err := refresh([]byte(in)); err == nil {
			t.Errorf("refresh took %s", in)
		}
	}
}
