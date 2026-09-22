package opencode_test

import (
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/reviewer/opencode"
)

const catalogJSON = `{
  "jira":   {"type": "remote", "url": "https://jira.example.com/mcp",
             "headers": {"Authorization": "Bearer {env:JIRA_TOKEN}"}},
  "sentry": {"type": "local", "command": ["sentry-mcp"],
             "environment": {"SENTRY_TOKEN": "{env:SENTRY_TOKEN}"}}
}`

func TestParseCatalog(t *testing.T) {
	catalog, err := opencode.ParseCatalog(catalogJSON, nil)
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	if got := catalog.Names(); len(got) != 2 || got[0] != "jira" || got[1] != "sentry" {
		t.Fatalf("Names = %v", got)
	}
	// Defining a server does not turn it on. A repository does that.
	if catalog["jira"].Enabled {
		t.Error("a defined server is enabled before anybody asked")
	}
	if catalog["jira"].Headers["Authorization"] != "Bearer {env:JIRA_TOKEN}" {
		t.Errorf("headers = %v", catalog["jira"].Headers)
	}
}

// The allow list narrows what a repository may reach. A server defined but not
// allowed is a contradiction, and it is refused at startup rather than
// silently dropped: the two settings disagreeing is the operator's mistake to
// see.
func TestParseCatalogAllowList(t *testing.T) {
	catalog, err := opencode.ParseCatalog(catalogJSON, []string{"jira", "sentry"})
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	if len(catalog) != 2 {
		t.Errorf("%d servers, want 2", len(catalog))
	}

	_, err = opencode.ParseCatalog(catalogJSON, []string{"jira"})
	if err == nil {
		t.Fatal("a server outside the allow list was accepted")
	}
	if !strings.Contains(err.Error(), "sentry") {
		t.Errorf("the error does not name the server: %v", err)
	}
}

func TestParseCatalogRejectsNonsense(t *testing.T) {
	for name, definitions := range map[string]string{
		"not JSON":                 `{`,
		"a server with no type":    `{"a": {"url": "https://x"}}`,
		"a type nobody defined":    `{"a": {"type": "grpc", "url": "https://x"}}`,
		"a local with no command":  `{"a": {"type": "local"}}`,
		"a remote with no url":     `{"a": {"type": "remote"}}`,
		"a local with a url":       `{"a": {"type": "local", "command": ["x"], "url": "https://x"}}`,
		"a remote with a command":  `{"a": {"type": "remote", "url": "https://x", "command": ["x"]}}`,
		"a field opencode has not": `{"a": {"type": "remote", "url": "https://x", "secret": "x"}}`,
		"a negative timeout":       `{"a": {"type": "remote", "url": "https://x", "timeout": -1}}`,
	} {
		if _, err := opencode.ParseCatalog(definitions, nil); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestParseCatalogEmpty(t *testing.T) {
	for _, definitions := range []string{"", "   ", "{}"} {
		catalog, err := opencode.ParseCatalog(definitions, nil)
		if err != nil {
			t.Fatalf("ParseCatalog(%q): %v", definitions, err)
		}
		if catalog != nil {
			t.Errorf("ParseCatalog(%q) = %v, want nil", definitions, catalog)
		}
	}
}

// A repository that names a server nobody configured has asked for something
// and got nothing. Both halves are reported so the caller can say so.
func TestCatalogResolve(t *testing.T) {
	catalog, err := opencode.ParseCatalog(catalogJSON, nil)
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}

	enabled, refused := catalog.Resolve([]string{"sentry", "confluence", "jira", "sentry", " "})
	if len(enabled) != 2 || enabled[0] != "jira" || enabled[1] != "sentry" {
		t.Errorf("enabled = %v", enabled)
	}
	if len(refused) != 1 || refused[0] != "confluence" {
		t.Errorf("refused = %v", refused)
	}

	enabled, refused = catalog.Resolve(nil)
	if len(enabled) != 0 || len(refused) != 0 {
		t.Errorf("asking for nothing gave %v / %v", enabled, refused)
	}
}
