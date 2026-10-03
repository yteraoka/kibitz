package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yteraoka/kibitz/internal/repoconfig"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/reviewer/opencode"
)

// readTools are what every mode starts from: pi's own file tools, whose
// arguments are a path and a pattern rather than a command line. There is no
// shell in any mode, for the reasons opencode's baseline gives.
var readTools = []string{"read", "grep", "find", "ls"}

// scratchWritable is where review and triage may write: the output directory
// and nothing else.
const scratchWritable = `^(?:\.kibitz/out/[^/]+)$`

// profile is what one mode may do: the tools it is given and where its writes
// may land.
type profile struct {
	tools []string
	// writable is a regular expression over the path relative to the
	// checkout. Empty means no write lands anywhere.
	writable string
}

// profileFor picks the profile for a request.
//
// The tool list is what pi declares to the model; the guard extension
// enforces the same list again, together with the paths, on every call.
func profileFor(req reviewer.Request) (profile, error) {
	switch req.Mode {
	case reviewer.ModeAnswer, reviewer.ModePlan:
		return profile{tools: readTools}, nil
	case reviewer.ModeImplement:
		allow, err := repoconfig.NewPathFilter(req.EditablePaths)
		if err != nil {
			return profile{}, fmt.Errorf("pi: editable paths: %w", err)
		}
		tools := append(append([]string{}, readTools...), "edit", "write")
		return profile{tools: tools, writable: allow.Expr()}, nil
	default:
		tools := append(append([]string{}, readTools...), "write")
		return profile{tools: tools, writable: scratchWritable}, nil
	}
}

// guardRules is what kibitz-guard reads from KIBITZ_PI_GUARD.
type guardRules struct {
	Root         string   `json:"root"`
	Tools        []string `json:"tools"`
	ToolPrefixes []string `json:"toolPrefixes"`
	Writable     string   `json:"writable,omitempty"`
}

// settings is pi's settings.json, written into the job's own agent directory
// so that nothing the operator or an earlier job left behind applies.
type settings struct {
	// DefaultTools rather than --tools: --tools also drops every MCP tool,
	// while the default selection leaves the tools of servers with direct
	// exposure in place. Built-in tools left out of it are registered but
	// inactive, and an inactive tool is not callable, not even from codemode.
	DefaultTools []string `json:"defaultTools"`
	// The rest turns off what has no business in a headless run.
	EnableInstallTelemetry bool   `json:"enableInstallTelemetry"`
	DefaultProjectTrust    string `json:"defaultProjectTrust"`
	QuietStartup           bool   `json:"quietStartup"`
}

// mcpFile is pi's mcp.json.
type mcpFile struct {
	Servers map[string]piServer `json:"mcpServers"`
	// Codemode is turned off. It would hide the servers' tools behind a
	// script the model writes, which is a second place tool calls come from
	// and a nesting the event stream reports differently.
	AutoEnableCodemode bool `json:"autoEnableCodemode"`
}

// piServer is one mcpServers entry, in the shape pi and most MCP clients
// share.
type piServer struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	CWD     string            `json:"cwd,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	OAuth   json.RawMessage   `json:"oauth,omitempty"`
	// Timeout is in seconds; opencode's is in milliseconds.
	Timeout int `json:"timeout,omitempty"`
	// Exposure is always direct: the tools are declared to the model like
	// built-in ones, as opencode does.
	Exposure string `json:"exposure"`
}

// envPlaceholder is opencode's substitution syntax, which the catalog is
// written in.
var envPlaceholder = regexp.MustCompile(`\{env:([^}]+)\}`)

// convertPlaceholders rewrites "{env:NAME}" as pi's "${NAME}". pi substitutes
// it in env and headers only, which is where a credential goes.
func convertPlaceholders(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = envPlaceholder.ReplaceAllStringFunc(value, func(match string) string {
			name := strings.TrimSpace(envPlaceholder.FindStringSubmatch(match)[1])
			return "${" + name + "}"
		})
	}
	return out
}

// toPiServer translates a catalog entry. The catalog stays in opencode's
// shape so that a deployment does not have to describe its servers twice to
// try the other engine.
func toPiServer(server opencode.MCPServer) piServer {
	out := piServer{Exposure: "direct"}
	switch server.Type {
	case opencode.MCPLocal:
		out.Command = server.Command[0]
		out.Args = server.Command[1:]
		out.Env = convertPlaceholders(server.Environment)
		out.CWD = server.CWD
	case opencode.MCPRemote:
		out.URL = server.URL
		out.Headers = convertPlaceholders(server.Headers)
		out.OAuth = server.OAuth
	}
	if server.TimeoutMS > 0 {
		out.Timeout = int(math.Ceil(float64(server.TimeoutMS) / 1000))
	}
	return out
}

// serverName is the name pi accepts for a server: letters, digits, "_" and
// "-". Anything else becomes "_", the way opencode treats it.
func serverName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// toolPrefix is how pi names a server's tools: "mcp__<server>__", with every
// character other than letters, digits and "_" replaced by "_".
func toolPrefix(server string) string {
	return "mcp__" + strings.ReplaceAll(serverName(server), "-", "_") + "__"
}

// customModel is one entry of models.json.
type customModel struct {
	ID string `json:"id"`
}

// customProvider is one provider in models.json.
type customProvider struct {
	BaseURL string        `json:"baseUrl"`
	API     string        `json:"api"`
	APIKey  string        `json:"apiKey"`
	Models  []customModel `json:"models"`
}

// providerTokenEnv carries the minted credential of a custom provider. The
// file names the variable and pi reads it at request time, so the token is
// never written to disk.
const providerTokenEnv = "KIBITZ_PI_PROVIDER_TOKEN" //nolint:gosec // the name of a variable, not its value

// writeAgentDir writes the job's agent directory: settings, MCP servers and,
// when the model needs one, a provider pi's catalog does not list. It returns
// what has to be added to the process environment.
func (r *Runner) writeAgentDir(ctx context.Context, dir string, req reviewer.Request, prof profile, servers map[string]opencode.MCPServer) ([]string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("pi: creating agent directory: %w", err)
	}

	s := settings{
		DefaultTools:           prof.tools,
		EnableInstallTelemetry: false,
		DefaultProjectTrust:    "never",
		QuietStartup:           true,
	}
	if err := writeJSON(filepath.Join(dir, "settings.json"), s); err != nil {
		return nil, err
	}

	mcp := mcpFile{Servers: map[string]piServer{}, AutoEnableCodemode: false}
	for name, server := range servers {
		mcp.Servers[serverName(name)] = toPiServer(server)
	}
	if err := writeJSON(filepath.Join(dir, "mcp.json"), mcp); err != nil {
		return nil, err
	}

	var env []string
	model := r.modelFor(req)
	if p := r.cfg.CustomProvider; p.Serves(model) {
		if p.BaseURL == "" || p.Token == nil {
			return nil, fmt.Errorf("pi: provider %q has no base url or credential source", p.ID)
		}
		token, err := p.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("pi: getting a credential for %q: %w", p.ID, err)
		}
		_, id, _ := strings.Cut(model, "/")
		models := map[string]any{"providers": map[string]customProvider{
			p.ID: {
				BaseURL: p.BaseURL,
				API:     "openai-completions",
				APIKey:  "${" + providerTokenEnv + "}",
				Models:  []customModel{{ID: id}},
			},
		}}
		if err := writeJSON(filepath.Join(dir, "models.json"), models); err != nil {
			return nil, err
		}
		env = append(env, providerTokenEnv+"="+token)
	}
	return env, nil
}

// systemPrompt reads the agent definition for a mode and returns its body.
//
// The definitions are opencode's, shipped in the image: markdown with a
// front matter block that only opencode reads. Sharing them keeps one set of
// instructions for both engines, which is what makes a comparison between
// them mean something.
func (r *Runner) systemPrompt(mode reviewer.Mode) (string, error) {
	if r.cfg.AgentsDir == "" {
		return "", nil
	}
	path := filepath.Join(r.cfg.AgentsDir, agentFor(mode)+".md")
	data, err := os.ReadFile(path) //nolint:gosec // a path from configuration, not from the pull request
	if err != nil {
		return "", fmt.Errorf("pi: reading the agent definition: %w", err)
	}
	return strings.TrimSpace(stripFrontMatter(string(data))), nil
}

// agentFor names the definition each mode runs with.
func agentFor(mode reviewer.Mode) string {
	switch mode {
	case reviewer.ModeAnswer:
		return "kibitz-answer"
	case reviewer.ModePlan:
		return "kibitz-plan"
	case reviewer.ModeImplement:
		return "kibitz-implement"
	case reviewer.ModeTriage:
		return "kibitz-triage"
	default:
		return "kibitz-review"
	}
}

// stripFrontMatter removes a leading "---" block.
func stripFrontMatter(s string) string {
	rest, ok := strings.CutPrefix(s, "---\n")
	if !ok {
		return s
	}
	_, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return s
	}
	return body
}

// writeJSON writes v where only this user can read it: the MCP definitions
// can carry credentials.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("pi: encoding %s: %w", filepath.Base(path), err)
	}
	return writeFile(path, data)
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
