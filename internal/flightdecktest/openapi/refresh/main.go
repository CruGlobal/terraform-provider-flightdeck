// Command refresh turns the document Flightdeck serves at
// /api/v1/openapi.json, read from standard input, into the copy the fake
// checks its answers against (../flightdeck-v1.json), written to standard
// output.
//
// It changes two things and nothing else. The host names in the document's
// servers list are replaced with example.com names wherever they appear, so
// the copy names no real deployment. And the JSON is indented, with its keys
// left in the order Flightdeck wrote them, so a refresh reads as a diff.
//
//	curl -sf https://flightdeck.example.com/api/v1/openapi.json \
//	  | go run ./internal/flightdecktest/openapi/refresh \
//	  > internal/flightdecktest/openapi/flightdeck-v1.json
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
)

func main() {
	in, err := io.ReadAll(os.Stdin)
	if err == nil {
		var out []byte
		if out, err = refresh(in); err == nil {
			_, err = os.Stdout.Write(out)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "refresh:", err)
		os.Exit(1)
	}
}

func refresh(in []byte) ([]byte, error) {
	var head struct {
		OpenAPI string `json:"openapi"`
		Servers []struct {
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(in, &head); err != nil {
		return nil, fmt.Errorf("not a JSON document: %w", err)
	}
	if !strings.HasPrefix(head.OpenAPI, "3.1.") {
		return nil, fmt.Errorf("expected an OpenAPI 3.1 document, got openapi %q", head.OpenAPI)
	}

	// Each server's host gets an example.com name: the first plain
	// "flightdeck", the others named for their description.
	replace := map[string]string{}
	for i, server := range head.Servers {
		u, err := url.Parse(server.URL)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("server %d has no host in its url %q", i, server.URL)
		}
		if u.Host == "example.com" || strings.HasSuffix(u.Host, ".example.com") {
			continue
		}
		name := "flightdeck.example.com"
		if i > 0 {
			name = fmt.Sprintf("flightdeck-%s.example.com", slug(server.Description, i))
		}
		replace[u.Host] = name
	}

	// Longest first, so a host that contains another is replaced whole.
	hosts := make([]string, 0, len(replace))
	for host := range replace {
		hosts = append(hosts, host)
	}
	sort.Slice(hosts, func(i, j int) bool { return len(hosts[i]) > len(hosts[j]) })
	out := in
	for _, host := range hosts {
		out = bytes.ReplaceAll(out, []byte(host), []byte(replace[host]))
	}

	var indented bytes.Buffer
	if err := json.Indent(&indented, bytes.TrimSpace(out), "", "  "); err != nil {
		return nil, errors.New("the document stopped being JSON after its hosts were replaced: " + err.Error())
	}
	indented.WriteByte('\n')
	return indented.Bytes(), nil
}

// slug is a server's description as a host label ("Stage" is "stage"), or
// its position when the description has nothing usable.
func slug(description string, i int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(description) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	if s := strings.Trim(b.String(), "-"); s != "" {
		return s
	}
	return fmt.Sprint(i + 1)
}
