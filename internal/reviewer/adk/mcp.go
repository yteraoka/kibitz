package adk

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"

	"github.com/yteraoka/kibitz/internal/egress"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/reviewer/opencode"
)

// serversFor picks the MCP servers this run may use. The rules are the other
// engines': nothing for triage, the repository's choice narrowed by the
// deployment's catalog, nothing a fork may not have, and kibitz's own server
// whenever there is a context for it.
func (r *Runner) serversFor(req reviewer.Request, contextPath string) map[string]opencode.MCPServer {
	if req.Mode == reviewer.ModeTriage {
		return nil
	}
	servers := map[string]opencode.MCPServer{}
	fork := req.PullRequest != nil && req.PullRequest.IsFork
	for _, name := range req.MCP {
		server, ok := r.cfg.MCPServers[name]
		if !ok || (fork && !server.AllowFork) {
			continue
		}
		servers[name] = server
	}
	if r.cfg.ContextBin != "" && contextPath != "" {
		servers[opencode.ContextServerName] = opencode.MCPServer{
			Type:    opencode.MCPLocal,
			Command: []string{r.cfg.ContextBin, "--context", contextPath},
			Enabled: true,
		}
	}
	return servers
}

// envPlaceholder is the catalog's substitution syntax, opencode's.
var envPlaceholder = regexp.MustCompile(`\{env:([^}]+)\}`)

// expand replaces "{env:NAME}" with the value of NAME. The other engines hand
// the placeholder to the agent, which resolves it; here kibitz is the agent.
func expand(s string) string {
	return envPlaceholder.ReplaceAllStringFunc(s, func(match string) string {
		return os.Getenv(strings.TrimSpace(envPlaceholder.FindStringSubmatch(match)[1]))
	})
}

// toolsets connects to the servers. A server that cannot be described to ADK
// is left out with a warning rather than failing the run, the way a server
// that fails to start is left out by the other engines.
func (r *Runner) toolsets(ctx context.Context, servers map[string]opencode.MCPServer, session *egress.Session) []tool.Toolset {
	names := slices.Sorted(maps.Keys(servers))
	var out []tool.Toolset
	for _, name := range names {
		ts, err := r.toolset(name, servers[name], session)
		if err != nil {
			r.logger.LogAttrs(ctx, slog.LevelWarn, "an MCP server was left out",
				slog.String("server", name), slog.String("error", err.Error()))
			continue
		}
		out = append(out, ts)
	}
	return out
}

func (r *Runner) toolset(name string, server opencode.MCPServer, session *egress.Session) (tool.Toolset, error) {
	if !server.Enabled {
		return nil, errors.New("disabled")
	}
	var transport mcp.Transport
	switch server.Type {
	case opencode.MCPLocal:
		cmd := exec.Command(server.Command[0], server.Command[1:]...) //nolint:gosec // the command comes from the deployment's catalog
		cmd.Dir = server.CWD
		cmd.Env = r.childEnv(server, session)
		cmd.Stderr = &logWriter{logger: r.logger, server: name}
		transport = &mcp.CommandTransport{Command: cmd}
	case opencode.MCPRemote:
		if len(server.OAuth) > 0 && string(server.OAuth) != "false" {
			return nil, errors.New("OAuth is not supported by the adk engine yet")
		}
		client, err := httpClient(server, session)
		if err != nil {
			return nil, err
		}
		transport = &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: client}
	default:
		return nil, fmt.Errorf("unknown type %q", server.Type)
	}
	return mcptoolset.New(mcptoolset.Config{Transport: transport})
}

// childEnv builds a local server's environment from a list, as the other
// engines build the agent's: the worker's secrets stay in the worker. A local
// MCP server is a program kibitz did not write, so it is the last place the
// GitHub App's key should turn up.
func (r *Runner) childEnv(server opencode.MCPServer, session *egress.Session) []string {
	wanted := map[string]bool{}
	for _, name := range opencode.BaseEnv() {
		wanted[name] = true
	}
	for _, name := range r.cfg.EnvPassthrough {
		if name = strings.TrimSpace(name); name != "" {
			wanted[name] = true
		}
	}
	env := []string{}
	for _, name := range slices.Sorted(maps.Keys(wanted)) {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(server.Environment)) {
		env = append(env, key+"="+expand(server.Environment[key]))
	}
	if session != nil {
		env = append(env, session.Env()...)
	}
	return env
}

// httpClient is a remote server's client: its headers on every request, and
// the job's egress session as its proxy when there is one.
func httpClient(server opencode.MCPServer, session *egress.Session) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if session != nil {
		transport.Proxy = session.ProxyFunc()
		if pem := session.CACertPEM(); len(pem) > 0 {
			pool, err := x509.SystemCertPool()
			if err != nil {
				pool = x509.NewCertPool()
			}
			pool.AppendCertsFromPEM(pem)
			transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		}
	}
	headers := http.Header{}
	for key, value := range server.Headers {
		headers.Set(key, expand(value))
	}
	client := &http.Client{Transport: &headerTransport{base: transport, headers: headers}}
	if server.TimeoutMS > 0 {
		client.Timeout = time.Duration(server.TimeoutMS) * time.Millisecond
	}
	return client, nil
}

type headerTransport struct {
	base    http.RoundTripper
	headers http.Header
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(t.headers) > 0 {
		req = req.Clone(req.Context())
		for key, values := range t.headers {
			req.Header[key] = values
		}
	}
	return t.base.RoundTrip(req)
}

// logWriter puts a local server's stderr into the worker's log, a line at a
// time.
type logWriter struct {
	logger *slog.Logger
	server string
}

func (w *logWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			w.logger.LogAttrs(context.Background(), slog.LevelDebug, "MCP server stderr",
				slog.String("server", w.server), slog.String("line", line))
		}
	}
	return len(p), nil
}

// uniqueToolset presents several toolsets as one in which every name is
// unique.
//
// ADK does not namespace a server's tools, and it refuses a request that
// declares the same name twice. So a later tool with a name already taken —
// by a workspace tool or by an earlier server, in sorted order — is dropped,
// and the drop is logged once, rather than letting one server's choice of
// name stop every review.
type uniqueToolset struct {
	reserved []string
	sets     []tool.Toolset
	logger   *slog.Logger
	warned   map[string]bool
}

func (*uniqueToolset) Name() string { return "kibitz_mcp" }

func (u *uniqueToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	seen := map[string]bool{}
	for _, name := range u.reserved {
		seen[name] = true
	}
	var out []tool.Tool
	for _, set := range u.sets {
		tools, err := set.Tools(ctx)
		if err != nil {
			// One server that cannot list its tools takes only its own tools
			// away.
			u.warn(ctx, "listing an MCP server's tools failed: "+err.Error(), set.Name())
			continue
		}
		for _, t := range tools {
			if seen[t.Name()] {
				u.warn(ctx, "an MCP tool was dropped because its name is taken", t.Name())
				continue
			}
			seen[t.Name()] = true
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

func (u *uniqueToolset) warn(ctx context.Context, msg, name string) {
	if u.warned == nil {
		u.warned = map[string]bool{}
	}
	key := msg + "\x00" + name
	if u.warned[key] {
		return
	}
	u.warned[key] = true
	u.logger.LogAttrs(ctx, slog.LevelWarn, msg, slog.String("name", name))
}

// Close closes the servers, which for a local one stops its process.
func (u *uniqueToolset) Close() error {
	var errs []error
	for _, set := range u.sets {
		if c, ok := set.(interface{ Close() error }); ok {
			errs = append(errs, c.Close())
		}
	}
	return errors.Join(errs...)
}
