package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

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

// reviewPermissions is read-only except for the one file the contract asks
// for. The agent reviews code; it has no reason to change any of it.
func reviewPermissions() permissions {
	return permissions{
		"*":        "deny",
		"read":     "allow",
		"grep":     "allow",
		"glob":     "allow",
		"webfetch": "deny",
		"edit": map[string]string{
			"*":             "deny",
			".kibitz/out/*": "allow",
		},
		"bash": map[string]string{
			"*":         "deny",
			"git diff*": "allow",
			"git log*":  "allow",
			"git show*": "allow",
			"rg *":      "allow",
		},
	}
}

// answerPermissions withhold even that: answering a question requires reading
// only.
func answerPermissions() permissions {
	return permissions{
		"*":        "deny",
		"read":     "allow",
		"grep":     "allow",
		"glob":     "allow",
		"edit":     "deny",
		"webfetch": "deny",
		"bash": map[string]string{
			"*":         "deny",
			"git log*":  "allow",
			"git show*": "allow",
			"rg *":      "allow",
		},
	}
}

func (r *Runner) writeConfig(ctx context.Context, path string, req reviewer.Request) error {
	model := req.Model
	if model == "" {
		model = r.cfg.Model
	}

	cfg := opencodeConfig{
		Schema:     "https://opencode.ai/config.json",
		Model:      model,
		Permission: reviewPermissions(),
		MCP:        r.serversFor(req),
	}
	if req.Mode == reviewer.ModeAnswer {
		cfg.Permission = answerPermissions()
	}

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
func (r *Runner) serversFor(req reviewer.Request) map[string]MCPServer {
	if len(r.cfg.MCPServers) == 0 || len(req.MCP) == 0 || req.Mode == reviewer.ModeTriage {
		return nil
	}

	servers := make(map[string]MCPServer, len(req.MCP))
	for _, name := range req.MCP {
		server, ok := r.cfg.MCPServers[name]
		if !ok {
			// The worker resolves the names against the same catalog before
			// it gets here and reports what it dropped, so this is a
			// belt-and-braces check rather than the one that matters.
			continue
		}
		server.Enabled = true
		servers[name] = server
	}
	if len(servers) == 0 {
		return nil
	}
	return servers
}
