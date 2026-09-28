package egress_test

import (
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/egress"
)

func TestParseRuleRejects(t *testing.T) {
	for _, rule := range []string{
		"",
		"https://example.com",
		"exa*mple.com",
		"*example.com",
		"example.com:https",
		":443",
	} {
		if _, err := egress.ParseRule(rule); err == nil {
			t.Errorf("ParseRule(%q) accepted it", rule)
		}
	}
}

func TestParseRulesReportsEveryError(t *testing.T) {
	_, err := egress.ParseRules([]string{"https://a.example", "ok.example", "b*.example"})
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"https://a.example", "b*.example"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestDecide(t *testing.T) {
	policy, err := egress.NewPolicy(
		[]string{"api.example.com", "*.googleapis.com", "docs.example.com/public/*", "internal.example:8443", "[::1]"},
		[]string{"api.example.com/admin*", "evil.googleapis.com"},
	)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		host, port, path string
		allowed          bool
		rule             string
	}{
		{"api.example.com", "443", "/v1/items", true, "api.example.com"},
		{"API.Example.COM.", "443", "/v1", true, "api.example.com"},
		{"api.example.com", "443", "/admin/users", false, "api.example.com/admin*"},
		{"aiplatform.googleapis.com", "443", "/", true, "*.googleapis.com"},
		{"a.b.googleapis.com", "443", "/", true, "*.googleapis.com"},
		{"googleapis.com", "443", "/", false, ""},
		{"evil.googleapis.com", "443", "/", false, "evil.googleapis.com"},
		{"docs.example.com", "443", "/public/a/b", true, "docs.example.com/public/*"},
		{"docs.example.com", "443", "/private", false, ""},
		{"internal.example", "8443", "/", true, "internal.example:8443"},
		{"internal.example", "443", "/", false, ""},
		{"::1", "443", "/", true, "[::1]"},
		{"other.example", "443", "/", false, ""},
	}
	for _, c := range cases {
		d := policy.Decide(c.host, c.port, c.path)
		if d.Allowed != c.allowed || d.Rule != c.rule {
			t.Errorf("Decide(%s, %s, %s) = %+v, want allowed=%v rule=%q", c.host, c.port, c.path, d, c.allowed, c.rule)
		}
	}
}

func TestDecideWithoutAllowListAllowsAllButDenied(t *testing.T) {
	policy, err := egress.NewPolicy(nil, []string{"*.evil.example"})
	if err != nil {
		t.Fatal(err)
	}
	if d := policy.Decide("anything.example", "443", "/"); !d.Allowed || d.Reason() != "no allow list is configured" {
		t.Errorf("got %+v (%s)", d, d.Reason())
	}
	if d := policy.Decide("x.evil.example", "443", "/"); d.Allowed {
		t.Errorf("a denied host was allowed: %+v", d)
	}
}

func TestDecideConnectAndTunnel(t *testing.T) {
	policy, err := egress.NewPolicy(
		[]string{"docs.example.com/public/*", "api.example.com"},
		[]string{"api.example.com/admin*"},
	)
	if err != nil {
		t.Fatal(err)
	}

	// Inspected: a host with any allowed path is let in, and a deny rule for
	// one path does not shut the whole host.
	if d := policy.DecideConnect("docs.example.com", "443"); !d.Allowed {
		t.Errorf("DecideConnect(docs) = %+v", d)
	}
	if d := policy.DecideConnect("api.example.com", "443"); !d.Allowed {
		t.Errorf("DecideConnect(api) = %+v", d)
	}

	// Not inspected: no path will ever be seen, so both have to be said no to.
	if d := policy.DecideTunnel("docs.example.com", "443"); d.Allowed {
		t.Errorf("DecideTunnel(docs) = %+v, want denied: only some paths are allowed", d)
	}
	if d := policy.DecideTunnel("api.example.com", "443"); d.Allowed {
		t.Errorf("DecideTunnel(api) = %+v, want denied: some paths are denied", d)
	}
	if d := policy.DecideTunnel("other.example", "443"); d.Allowed {
		t.Errorf("DecideTunnel(other) = %+v", d)
	}
}
