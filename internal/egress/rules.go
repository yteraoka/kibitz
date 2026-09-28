package egress

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/yteraoka/kibitz/internal/policy"
)

// Rule is one entry of an allow or deny list, written host[:port][/path].
//
//	example.com              the host, any port, any path
//	*.example.com            any subdomain of it (not the host itself)
//	example.com:8443         that port only
//	api.example.com/v1/*     paths under /v1/ ("*" also crosses "/")
//	*                        every host
//
// There is no scheme. The proxy sees the same destination whether the client
// spoke HTTP or HTTPS to it, and a rule that looked like it restricted the
// scheme would restrict nothing.
type Rule struct {
	raw  string
	host string // lower case; "*" or a leading "*." is a wildcard
	port string // empty matches any port
	path string // empty matches any path
}

// String is the rule as it was written, which is what a log line names.
func (r Rule) String() string { return r.raw }

// HasPath reports whether the rule looks at the path, which only an inspected
// connection can show.
func (r Rule) HasPath() bool { return r.path != "" }

// ParseRule parses one rule.
func ParseRule(s string) (Rule, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Rule{}, errors.New("empty rule")
	}
	if strings.Contains(raw, "://") {
		return Rule{}, fmt.Errorf("%q: write host[:port][/path], without a scheme", raw)
	}

	hostport, path := raw, ""
	if i := strings.IndexByte(raw, '/'); i >= 0 {
		hostport, path = raw[:i], raw[i:]
	}

	host, port := hostport, ""
	if strings.HasPrefix(hostport, "[") || strings.Count(hostport, ":") == 1 {
		h, p, err := net.SplitHostPort(hostport)
		if err != nil {
			// "[::1]" without a port is a host on its own.
			if !strings.HasSuffix(hostport, "]") {
				return Rule{}, fmt.Errorf("%q: %w", raw, err)
			}
			h = strings.Trim(hostport, "[]")
		}
		host, port = h, p
	}
	host = normalizeHost(host)
	if host == "" {
		return Rule{}, fmt.Errorf("%q: the host is empty", raw)
	}
	if strings.Contains(strings.TrimPrefix(host, "*."), "*") {
		return Rule{}, fmt.Errorf("%q: a host may only start with \"*.\" or be \"*\"", raw)
	}
	if port != "" && strings.Trim(port, "0123456789") != "" {
		return Rule{}, fmt.Errorf("%q: the port %q is not a number", raw, port)
	}
	return Rule{raw: raw, host: host, port: port, path: path}, nil
}

// ParseRules parses a list, reporting every rule that is wrong at once.
func ParseRules(list []string) ([]Rule, error) {
	rules := make([]Rule, 0, len(list))
	var errs []error
	for _, s := range list {
		r, err := ParseRule(s)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		rules = append(rules, r)
	}
	return rules, errors.Join(errs...)
}

// matchesHost reports whether the rule's host and port cover a destination.
func (r Rule) matchesHost(host, port string) bool {
	if r.port != "" && r.port != port {
		return false
	}
	return matchHost(r.host, host)
}

// matches reports whether the rule covers a request.
func (r Rule) matches(host, port, path string) bool {
	if !r.matchesHost(host, port) {
		return false
	}
	return r.path == "" || policy.Match(r.path, path)
}

// matchHost matches a host against "*", "*.suffix" or an exact name.
func matchHost(pattern, host string) bool {
	switch {
	case pattern == "*":
		return true
	case strings.HasPrefix(pattern, "*."):
		return strings.HasSuffix(host, pattern[1:])
	default:
		return pattern == host
	}
}

// normalizeHost lower-cases a host and drops the brackets of an IPv6 literal
// and the trailing dot of a fully qualified name, so that one destination has
// one spelling.
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	return host
}

// Policy decides which destinations the agent may reach.
//
// A deny rule always wins. An empty allow list allows everything the deny list
// does not name, which is how the proxy runs as an audit log before anybody has
// written a list; a non-empty one allows only what it names.
type Policy struct {
	allow []Rule
	deny  []Rule
}

// NewPolicy parses both lists.
func NewPolicy(allow, deny []string) (*Policy, error) {
	a, errA := ParseRules(allow)
	d, errD := ParseRules(deny)
	if err := errors.Join(errA, errD); err != nil {
		return nil, err
	}
	return &Policy{allow: a, deny: d}, nil
}

// Restricted reports whether there is an allow list. When there is, every
// destination that gets through was named by the operator.
func (p *Policy) Restricted() bool { return len(p.allow) > 0 }

// HasPathRules reports whether any rule looks at the path.
func (p *Policy) HasPathRules() bool {
	for _, r := range append(append([]Rule(nil), p.allow...), p.deny...) {
		if r.HasPath() {
			return true
		}
	}
	return false
}

// Decision is the outcome for one destination, and the reason for it.
type Decision struct {
	Allowed bool
	// Rule is the rule that decided, empty when none did (an allowed request
	// with no allow list, or a denied one that no allow rule covered).
	Rule string
}

// Reason is the decision in words, for a log line and a 403.
func (d Decision) Reason() string {
	switch {
	case d.Allowed && d.Rule == "":
		return "no allow list is configured"
	case d.Allowed:
		return "allowed by " + d.Rule
	case d.Rule != "":
		return "denied by " + d.Rule
	default:
		return "not in the allow list"
	}
}

// Decide decides one request whose path is known.
func (p *Policy) Decide(host, port, path string) Decision {
	host = normalizeHost(host)
	for _, r := range p.deny {
		if r.matches(host, port, path) {
			return Decision{Rule: r.raw}
		}
	}
	if !p.Restricted() {
		return Decision{Allowed: true}
	}
	for _, r := range p.allow {
		if r.matches(host, port, path) {
			return Decision{Allowed: true, Rule: r.raw}
		}
	}
	return Decision{}
}

// DecideConnect decides whether to open a connection that will be inspected,
// before any path is known. It turns away only what no path could make
// allowed; every request on the connection is then decided on its own.
func (p *Policy) DecideConnect(host, port string) Decision {
	host = normalizeHost(host)
	for _, r := range p.deny {
		if !r.HasPath() && r.matchesHost(host, port) {
			return Decision{Rule: r.raw}
		}
	}
	if !p.Restricted() {
		return Decision{Allowed: true}
	}
	for _, r := range p.allow {
		if r.matchesHost(host, port) {
			return Decision{Allowed: true, Rule: r.raw}
		}
	}
	return Decision{}
}

// DecideTunnel decides a connection that will not be inspected, so no path
// will ever be seen. It errs toward saying no: a deny rule for any path on the
// host denies the host, and only an allow rule for every path allows it.
func (p *Policy) DecideTunnel(host, port string) Decision {
	host = normalizeHost(host)
	for _, r := range p.deny {
		if r.matchesHost(host, port) {
			return Decision{Rule: r.raw}
		}
	}
	if !p.Restricted() {
		return Decision{Allowed: true}
	}
	for _, r := range p.allow {
		if !r.HasPath() && r.matchesHost(host, port) {
			return Decision{Allowed: true, Rule: r.raw}
		}
	}
	return Decision{}
}

// hostList matches hosts against a list of "*", "*.suffix" or exact names.
type hostList []string

func parseHostList(list []string) hostList {
	out := make(hostList, 0, len(list))
	for _, h := range list {
		if h = normalizeHost(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}

func (l hostList) contains(host string) bool {
	host = normalizeHost(host)
	for _, pattern := range l {
		if matchHost(pattern, host) {
			return true
		}
	}
	return false
}
