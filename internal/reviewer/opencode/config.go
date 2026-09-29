package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yteraoka/kibitz/internal/reviewer"
)

// opencodeConfig is the per-job configuration handed to the CLI through
// OPENCODE_CONFIG. It is generated rather than taken from the repository: a
// pull request must not be able to grant its own reviewer more permissions.
type opencodeConfig struct {
	Schema     string               `json:"$schema"`
	Model      string               `json:"model,omitempty"`
	Permission permissions          `json:"permission"`
	MCP        map[string]MCPServer `json:"mcp,omitempty"`
	Provider   map[string]any       `json:"provider,omitempty"`
}

// permissions mirrors OpenCode's permission block. Each value is either an
// action ("allow", "deny") or a map of pattern to action.
type permissions map[string]any

// baseline is what every profile starts from: the agent reads the workspace
// with opencode's own tools and nothing else.
//
// There is no shell, not even a narrow one. opencode matches a bash rule
// against the command line as a string, so allowing a program allows every
// option it has, and the programs that look read-only have options that are
// not: between them they could run a file from the pull request, read the
// worker's own environment, and write outside the workspace. A pattern that
// tried to exclude each of those would be a list of the ones somebody thought
// of. opencode's read, grep and glob do the same work with arguments that are
// a path and a pattern, not options.
//
// external_directory is denied by name rather than left to "*", so that
// those tools stay inside the workspace however the default is written.
func baseline() permissions {
	return permissions{
		"*":                  "deny",
		"read":               "allow",
		"grep":               "allow",
		"glob":               "allow",
		"webfetch":           "deny",
		"bash":               "deny",
		"external_directory": "deny",
	}
}

// reviewPermissions is read-only except for the one file the contract asks
// for. The agent reviews code; it has no reason to change any of it.
func reviewPermissions() permissions {
	p := baseline()
	p["edit"] = map[string]string{
		"*":             "deny",
		".kibitz/out/*": "allow",
	}
	return p
}

// answerPermissions withhold even that: answering a question requires reading
// only.
func answerPermissions() permissions {
	p := baseline()
	p["edit"] = "deny"
	return p
}

// implementPermissions let the agent write, and only where the repository said
// it may.
//
// The patterns are opencode's own matching, not kibitz's, and that is why they
// are not what this feature relies on: the worker checks every path that
// actually changed against the same list before anything is committed
// (see worker.Editable). This is the cheap layer that stops an honest mistake
// early; the expensive one is the check afterwards, which stops the rest.
//
// There is no shell here either (see [baseline]). The commands that build and
// test the change run somewhere kibitz holds no credentials (ADR-0019), and an
// agent that could run them here would be running them in the process that
// holds the GitHub App's private key.
func implementPermissions(allow []string) permissions {
	edit := map[string]string{"*": "deny"}
	for _, pattern := range allow {
		if pattern = strings.TrimSpace(pattern); pattern != "" {
			edit[pattern] = "allow"
		}
	}

	p := baseline()
	p["edit"] = edit
	return p
}

func (r *Runner) writeConfig(ctx context.Context, path string, req reviewer.Request, servers map[string]MCPServer) error {
	model := req.Model
	if model == "" {
		model = r.cfg.Model
	}

	cfg := opencodeConfig{
		Schema:     "https://opencode.ai/config.json",
		Model:      model,
		Permission: reviewPermissions(),
		MCP:        servers,
	}
	// Answering a question and planning a change both read and write
	// nothing, so both run under the narrower profile. A plan that could edit
	// would be an implementation.
	if req.Mode == reviewer.ModeAnswer || req.Mode == reviewer.ModePlan {
		cfg.Permission = answerPermissions()
	}
	if req.Mode == reviewer.ModeImplement {
		cfg.Permission = implementPermissions(req.EditablePaths)
	}
	allowServerTools(cfg.Permission, servers)

	// A model OpenCode's catalog does not know about is declared here, with a
	// credential minted for this job.
	if r.cfg.CustomProvider.Serves(model) {
		block, err := r.cfg.CustomProvider.block(ctx, model)
		if err != nil {
			return err
		}
		cfg.Provider = block
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("opencode: encoding config: %w", err)
	}
	if err := writeFile(path, data); err != nil {
		return fmt.Errorf("opencode: writing config: %w", err)
	}
	return nil
}

// allowServerTools lets the agent call the tools of the MCP servers this run
// enabled.
//
// opencode checks an MCP tool against the permission block like any other,
// by the name it gives it, so under "*": "deny" a server that is enabled and
// connected still offers the model nothing: its tools are left out of the
// request. Enabling a server was the decision -- the operator defined it, the
// repository asked for it, and a fork got it only if it is marked for forks
// -- so its tools are allowed by that name and no others.
//
// The key sorts after "*" in the encoded object, which is the order opencode
// reads the rules in, the last match winning. A server's name cannot put it
// before: the characters that sort before "*" are the ones sanitized away.
func allowServerTools(p permissions, servers map[string]MCPServer) {
	for name := range servers {
		p[mcpToolPrefix(name)+"*"] = "allow"
	}
}

// mcpToolPrefix is how opencode prefixes a server's tools: the server's name
// with everything outside [A-Za-z0-9_-] replaced by "_", and a "_". Doing the
// same here keeps the pattern free of wildcards whatever the name is.
func mcpToolPrefix(server string) string {
	var b strings.Builder
	for _, r := range server {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String() + "_"
}

// writeFile creates the parent directory and writes the file with permissions
// that keep it to this user: the config can carry credentials for MCP servers.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// serversFor picks the MCP servers this run may use: the ones the deployment
// defined, narrowed to the ones the repository asked for.
//
// Nothing is enabled unless a repository asked. Tool definitions are sent with
// every request and cost tokens whether or not the model calls them, so a
// server that is merely available is a bill nobody agreed to.
//
// Triage never gets any. It reads a list of file names to decide what is worth
// reading, and a pass that pulled in Jira would spend exactly what it exists
// to save.
func (r *Runner) serversFor(req reviewer.Request, contextPath string) map[string]MCPServer {
	if req.Mode == reviewer.ModeTriage {
		return nil
	}
	servers := r.repositoryServers(req)

	// kibitz's own server is not something a repository asks for: it holds no
	// credential, costs one process, and answers questions about the pull
	// request the agent is already looking at.
	if r.cfg.ContextBin != "" && contextPath != "" {
		if servers == nil {
			servers = map[string]MCPServer{}
		}
		servers[ContextServerName] = MCPServer{
			Type:    MCPLocal,
			Command: []string{r.cfg.ContextBin, "--context", contextPath},
			Enabled: true,
		}
	}
	return servers
}

// ContextServerName is how kibitz's own MCP server appears in the config, and
// therefore the prefix the agent sees on its tools.
const ContextServerName = "kibitz"

// repositoryServers picks the third-party servers this run may use: the ones
// the deployment defined, narrowed to the ones the repository asked for.
func (r *Runner) repositoryServers(req reviewer.Request) map[string]MCPServer {
	if len(r.cfg.MCPServers) == 0 || len(req.MCP) == 0 {
		return nil
	}
	fork := req.PullRequest != nil && req.PullRequest.IsFork

	servers := make(map[string]MCPServer, len(req.MCP))
	for _, name := range req.MCP {
		server, ok := r.cfg.MCPServers[name]
		if !ok {
			// The worker resolves the names against the same catalog before
			// it gets here and reports what it dropped, so this is a
			// belt-and-braces check rather than the one that matters.
			continue
		}
		if fork && !server.AllowFork {
			// A fork's branch is written by somebody without commit access,
			// and the agent reads it. A server holding a credential is not
			// reachable from there unless the operator said it is safe.
			continue
		}
		server.Enabled = true
		// Not a field opencode defines; it decided whether we are here.
		server.AllowFork = false
		servers[name] = server
	}
	if len(servers) == 0 {
		return nil
	}
	return servers
}
