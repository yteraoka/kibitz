package opencode

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Catalog is the MCP servers a deployment makes available, by name.
type Catalog map[string]MCPServer

// ParseCatalog reads the configured form: one JSON object of name to server.
//
//	{
//	  "jira":   {"type": "remote", "url": "https://jira.example.com/mcp",
//	             "headers": {"Authorization": "Bearer {env:JIRA_TOKEN}"}},
//	  "sentry": {"type": "local", "command": ["sentry-mcp"],
//	             "environment": {"SENTRY_TOKEN": "{env:SENTRY_TOKEN}"}}
//	}
//
// The credentials are written as "{env:NAME}" rather than as values: opencode
// substitutes them from its own environment, so the secret reaches the server
// without being written to a file. A literal value works too and is a worse
// idea; nothing here stops it, because a deployment that has no other way to
// pass one should not be blocked by kibitz's preference.
//
// Allow narrows what a repository may then ask for. An empty list means every
// defined server, because defining one is already an act of the operator.
func ParseCatalog(definitions string, allow []string) (Catalog, error) {
	definitions = strings.TrimSpace(definitions)
	if definitions == "" {
		return nil, nil
	}

	var parsed Catalog
	decoder := json.NewDecoder(strings.NewReader(definitions))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("not a JSON object of name to server: %w", err)
	}

	allowed := make(map[string]bool, len(allow))
	for _, name := range allow {
		if name = strings.TrimSpace(name); name != "" {
			allowed[name] = true
		}
	}

	catalog := make(Catalog, len(parsed))
	var refused []string
	for name, server := range parsed {
		if name = strings.TrimSpace(name); name == "" {
			return nil, fmt.Errorf("a server with no name")
		}
		if err := server.Validate(); err != nil {
			return nil, fmt.Errorf("server %q: %w", name, err)
		}
		if len(allowed) > 0 && !allowed[name] {
			refused = append(refused, name)
			continue
		}
		// Whether it runs is decided per job, by the repository.
		server.Enabled = false
		catalog[name] = server
	}
	if len(refused) > 0 {
		sort.Strings(refused)
		return nil, fmt.Errorf("servers %s are defined but not in the allow list; remove them from one or add them to the other",
			strings.Join(quoted(refused), ", "))
	}
	if len(catalog) == 0 {
		return nil, nil
	}
	return catalog, nil
}

// Names lists what the catalog holds, sorted.
func (c Catalog) Names() []string {
	names := make([]string, 0, len(c))
	for name := range c {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Resolve splits the names a repository asked for into the ones this
// deployment has and the ones it does not.
//
// The refused half is the point: a repository that names a server nobody
// configured has asked for something and got nothing, and it should be told
// rather than left to wonder why the review never mentions its tickets.
func (c Catalog) Resolve(wanted []string) (enabled, refused []string) {
	seen := map[string]bool{}
	for _, name := range wanted {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if _, ok := c[name]; ok {
			enabled = append(enabled, name)
			continue
		}
		refused = append(refused, name)
	}
	sort.Strings(enabled)
	sort.Strings(refused)
	return enabled, refused
}

func quoted(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, fmt.Sprintf("%q", name))
	}
	return out
}
