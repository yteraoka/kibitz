// Package opencode runs the OpenCode CLI as the agent engine.
//
// One job is one process with one disposable workspace. The agent's
// permissions are written out per job rather than taken from the repository,
// because a pull request must not be able to widen what the agent may do
// (see docs/security.md).
package opencode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/yteraoka/kibitz/internal/reviewer"
)

// Config configures the runner.
type Config struct {
	// Bin is the opencode executable.
	Bin string
	// Model is the provider/model to run, e.g. google-vertex-anthropic/claude-opus-5.
	Model string
	// ReviewAgent and AnswerAgent name the agent definitions to use.
	ReviewAgent string
	AnswerAgent string
	// MCPServers are the servers to enable, already filtered against the
	// operator's allow list.
	MCPServers map[string]MCPServer
	// Env is added to the process environment, for provider credentials such
	// as GOOGLE_CLOUD_PROJECT.
	Env []string
}

// MCPServer is one entry of OpenCode's mcp configuration.
type MCPServer struct {
	Type        string            `json:"type"`
	Command     []string          `json:"command,omitempty"`
	URL         string            `json:"url,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	Enabled     bool              `json:"enabled"`
}

// Runner implements [reviewer.Engine] by invoking the CLI.
type Runner struct {
	cfg    Config
	logger *slog.Logger
}

// New builds a runner.
func New(cfg Config, logger *slog.Logger) *Runner {
	if cfg.Bin == "" {
		cfg.Bin = "opencode"
	}
	if cfg.ReviewAgent == "" {
		cfg.ReviewAgent = "kibitz-review"
	}
	if cfg.AnswerAgent == "" {
		cfg.AnswerAgent = "kibitz-answer"
	}
	return &Runner{cfg: cfg, logger: logger}
}

// Run implements [reviewer.Engine].
func (r *Runner) Run(ctx context.Context, req reviewer.Request) (*reviewer.Result, error) {
	if req.WorkspaceDir == "" {
		return nil, errors.New("opencode: workspace directory is empty")
	}

	jobDir, err := os.MkdirTemp("", "kibitz-opencode-")
	if err != nil {
		return nil, fmt.Errorf("opencode: creating job directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(jobDir) }()

	configPath := filepath.Join(jobDir, "opencode.json")
	if err := r.writeConfig(configPath, req); err != nil {
		return nil, err
	}

	promptPath := filepath.Join(req.WorkspaceDir, ".kibitz", "prompt.md")
	if err := writeFile(promptPath, []byte(reviewer.BuildPrompt(req))); err != nil {
		return nil, fmt.Errorf("opencode: writing prompt: %w", err)
	}
	outputPath := filepath.Join(req.WorkspaceDir, reviewer.OutputPath)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o700); err != nil {
		return nil, fmt.Errorf("opencode: creating output directory: %w", err)
	}

	start := time.Now()
	stdout, err := r.exec(ctx, configPath, req)
	if err != nil {
		return nil, err
	}

	result := &reviewer.Result{SessionID: req.SessionID}
	transcript := parseEvents(stdout, r.logger)
	result.Usage = transcript.usage
	result.Usage.Duration = time.Since(start)
	if transcript.sessionID != "" {
		result.SessionID = transcript.sessionID
	}

	if req.Mode == reviewer.ModeAnswer {
		result.Reply = strings.TrimSpace(transcript.text)
		if result.Reply == "" {
			return nil, errors.New("opencode: the agent produced no answer")
		}
		return result, nil
	}

	data, err := os.ReadFile(outputPath) //nolint:gosec // a path this process just built
	if err != nil {
		return nil, fmt.Errorf("opencode: the agent did not write %s: %w", reviewer.OutputPath, err)
	}
	out, err := reviewer.ParseOutput(data)
	if err != nil {
		return nil, &reviewer.OutputError{Err: err}
	}

	result.Summary = out.Summary
	result.Findings = make([]reviewer.Finding, 0, len(out.Comments))
	for _, c := range out.Comments {
		result.Findings = append(result.Findings, reviewer.Finding{
			Path:       c.Path,
			Line:       c.Line,
			EndLine:    c.EndLine,
			Severity:   reviewer.Severity(strings.ToLower(c.Severity)),
			Category:   c.Category,
			Title:      c.Title,
			Body:       c.Body,
			Suggestion: c.Suggestion,
		})
	}
	result.RawOutput = out
	return result, nil
}

// exec runs the CLI and returns its stdout.
func (r *Runner) exec(ctx context.Context, configPath string, req reviewer.Request) ([]byte, error) {
	args := r.args(req)

	cmd := exec.CommandContext(ctx, r.cfg.Bin, args...) //nolint:gosec // the binary comes from configuration, not from the pull request
	cmd.Dir = req.WorkspaceDir
	cmd.Env = append(os.Environ(), r.cfg.Env...)
	cmd.Env = append(cmd.Env,
		"OPENCODE_CONFIG="+configPath,
		// The agent must not pick up the operator's own session history.
		"OPENCODE_DISABLE_AUTOUPDATE=1",
	)

	setupProcessGroup(cmd)

	var stderr strings.Builder
	cmd.Stderr = &stderr

	stdout, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 4000 {
			detail = detail[:4000] + "…"
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("opencode: timed out: %s", detail)
		}
		return nil, fmt.Errorf("opencode: %w: %s", err, detail)
	}
	return stdout, nil
}

// args builds the command line. It is separate so that the invocation can be
// asserted in tests without running anything.
func (r *Runner) args(req reviewer.Request) []string {
	agent := r.cfg.ReviewAgent
	if req.Mode == reviewer.ModeAnswer {
		agent = r.cfg.AnswerAgent
	}

	args := []string{
		"run",
		"--format", "json",
		"--agent", agent,
		"--dir", req.WorkspaceDir,
		// Permissions are decided in the config file. Without this, any rule
		// that resolves to "ask" would block forever in a headless run.
		"--auto",
	}
	model := req.Model
	if model == "" {
		model = r.cfg.Model
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if req.SessionID != "" {
		// Continuing the session is what lets a follow-up question refer to an
		// earlier finding.
		args = append(args, "--session", req.SessionID)
	}
	args = append(args,
		"--file", filepath.Join(".kibitz", "prompt.md"),
		"指示は添付された .kibitz/prompt.md に書かれています。その指示に従ってください。",
	)
	return args
}
