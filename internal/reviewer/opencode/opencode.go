// Package opencode runs the OpenCode CLI as the agent engine.
//
// One job is one process with one disposable workspace. The agent's
// permissions are written out per job rather than taken from the repository,
// because a pull request must not be able to widen what the agent may do
// (see docs/security.md).
package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/jobcontext"
	"github.com/yteraoka/kibitz/internal/reviewer"
)

// Config configures the runner.
type Config struct {
	// Bin is the opencode executable.
	Bin string
	// Model is the provider/model to run, e.g. google-vertex/gemini-3.1-pro-preview.
	Model string
	// ReviewAgent, AnswerAgent and TriageAgent name the agent definitions to
	// use.
	ReviewAgent string
	AnswerAgent string
	TriageAgent string
	// MCPServers are the servers to enable, already filtered against the
	// operator's allow list.
	MCPServers map[string]MCPServer
	// Env is added to the process environment, for provider credentials such
	// as GOOGLE_CLOUD_PROJECT.
	Env []string
	// ContextBin is the kibitz-mcp binary. When set, every review and answer
	// gets it as a local MCP server, so the agent can reach facts about the
	// pull request that the prompt does not carry — the diff of a file triage
	// left out, for one. Empty leaves it out entirely.
	ContextBin string
	// EnvPassthrough names further variables to copy from the worker's own
	// environment. The agent's process is otherwise built from a fixed list
	// rather than inherited, so that the worker's secrets stay in the worker
	// (see [baseEnv]).
	EnvPassthrough []string
	// CustomProvider declares a provider OpenCode's catalog does not list.
	CustomProvider *CustomProvider
}

// MCP server types.
const (
	MCPLocal  = "local"
	MCPRemote = "remote"
)

// MCPServer is one entry of OpenCode's mcp configuration. The fields are the
// ones opencode 1.18 accepts: command, cwd and environment for a local server,
// url and headers for a remote one.
//
// A value of the form "{env:NAME}" anywhere in here is substituted by opencode
// itself from its own environment, which is how a credential reaches a server
// without ever being written to the config file. See [envRefs].
type MCPServer struct {
	Type        string            `json:"type"`
	Command     []string          `json:"command,omitempty"`
	CWD         string            `json:"cwd,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	// OAuth is passed through to opencode unread. kibitz has no opinion on
	// how a remote server authenticates, and refusing a shape it does not
	// model would block a deployment over nothing.
	OAuth     json.RawMessage `json:"oauth,omitempty"`
	TimeoutMS int             `json:"timeout,omitempty"`
	Enabled   bool            `json:"enabled"`
	// AllowFork marks a server that may also be used on a pull request from a
	// fork. It is off by default, because a fork's branch is written by
	// somebody who does not have commit access and the agent reads it: a
	// server holding a credential must not be reachable from there
	// (docs/security.md).
	//
	// It is read from the operator's catalog and cleared before the config is
	// written, so opencode never sees a field it does not define.
	AllowFork bool `json:"allow_fork,omitempty"`
}

// Validate rejects a definition opencode would refuse, at startup rather than
// on the first review that asks for it.
func (m MCPServer) Validate() error {
	switch m.Type {
	case MCPLocal:
		if len(m.Command) == 0 {
			return errors.New(`a "local" server needs a command`)
		}
		if m.URL != "" || len(m.Headers) > 0 || len(m.OAuth) > 0 {
			return errors.New(`a "local" server takes no url, headers or oauth`)
		}
	case MCPRemote:
		if m.URL == "" {
			return errors.New(`a "remote" server needs a url`)
		}
		if len(m.Command) > 0 || len(m.Environment) > 0 || m.CWD != "" {
			return errors.New(`a "remote" server takes no command, cwd or environment`)
		}
	case "":
		return errors.New(`type is required, either "local" or "remote"`)
	default:
		return fmt.Errorf("type %q is neither \"local\" nor \"remote\"", m.Type)
	}
	if m.TimeoutMS < 0 {
		return errors.New("timeout must not be negative")
	}
	return nil
}

// envPlaceholder matches opencode's own substitution syntax.
var envPlaceholder = regexp.MustCompile(`\{env:([^}]+)\}`)

// envRefs returns the environment variables a server's definition refers to.
//
// They are what has to reach opencode's own environment for the substitution
// to resolve, and naming them here is what keeps the rest of the worker's
// environment — the webhook secrets, the GitHub App key — out of a process
// kibitz did not write.
func (m MCPServer) envRefs() []string {
	encoded, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	var names []string
	seen := map[string]bool{}
	for _, match := range envPlaceholder.FindAllStringSubmatch(string(encoded), -1) {
		name := strings.TrimSpace(match[1])
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
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
	if cfg.TriageAgent == "" {
		cfg.TriageAgent = "kibitz-triage"
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

	// The context file is what kibitz's own MCP server answers from. It is
	// written before the config, because the config has to name it.
	contextPath := ""
	if r.cfg.ContextBin != "" && req.Mode != reviewer.ModeTriage {
		path, err := jobcontext.Build(req.Event, req.PullRequest, fullDiff(req), req.Diff,
			req.ExistingComments, req.WorkspaceDir, req.References).Write(jobDir)
		if err != nil {
			return nil, fmt.Errorf("opencode: %w", err)
		}
		contextPath = path
	}
	servers := r.serversFor(req, contextPath)

	configPath := filepath.Join(jobDir, "opencode.json")
	if err := r.writeConfig(ctx, configPath, req, servers); err != nil {
		return nil, err
	}

	promptPath := filepath.Join(req.WorkspaceDir, ".kibitz", "prompt.md")
	if err := writeFile(promptPath, []byte(reviewer.BuildPrompt(req))); err != nil {
		return nil, fmt.Errorf("opencode: writing prompt: %w", err)
	}
	outputPath := filepath.Join(req.WorkspaceDir, outputPathFor(req.Mode))
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o700); err != nil {
		return nil, fmt.Errorf("opencode: creating output directory: %w", err)
	}

	start := time.Now()
	stdout, err := r.exec(ctx, configPath, req, servers)
	if err != nil && req.SessionID != "" && isMissingSession(err) {
		// The session lives in the agent's own storage, inside a container
		// that is disposable. Losing it is ordinary, not a failure: the
		// prompt carries the context it needs, so the run is simply repeated
		// without it.
		r.logger.LogAttrs(ctx, slog.LevelInfo, "the stored session is gone; starting a new one",
			slog.String("session_id", req.SessionID),
		)
		req.SessionID = ""
		stdout, err = r.exec(ctx, configPath, req, servers)
	}
	if err != nil {
		return nil, err
	}

	result := &reviewer.Result{SessionID: req.SessionID}
	transcript := parseEvents(stdout, r.logger)
	result.Usage = transcript.usage
	result.Usage.Duration = time.Since(start)
	result.Tools = transcript.tools
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
		return nil, fmt.Errorf("opencode: the agent did not write %s: %w", outputPathFor(req.Mode), err)
	}

	if req.Mode == reviewer.ModeTriage {
		triage, err := reviewer.ParseTriage(data)
		if err != nil {
			return nil, &reviewer.OutputError{Err: err}
		}
		result.Triage = triage
		return result, nil
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
func (r *Runner) exec(ctx context.Context, configPath string, req reviewer.Request, servers map[string]MCPServer) ([]byte, error) {
	args := r.args(req)

	cmd := exec.CommandContext(ctx, r.cfg.Bin, args...) //nolint:gosec // the binary comes from configuration, not from the pull request
	cmd.Dir = req.WorkspaceDir
	cmd.Env = r.childEnv(servers)
	cmd.Env = append(cmd.Env,
		"OPENCODE_CONFIG="+configPath,
		// The agent must not pick up the operator's own session history.
		"OPENCODE_DISABLE_AUTOUPDATE=1",
		// opencode otherwise looks for AGENTS.md, CLAUDE.md and CONTEXT.md in
		// the directory it runs in and loads them as instructions. That
		// directory is the pull request's own checkout, so a branch could
		// write the reviewer's instructions — which is the thing every other
		// decision here is arranged to prevent. The same conventions still
		// reach the agent, read from the default branch instead
		// (see the worker's repoGuidelines).
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
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

// outputPathFor is the file the agent writes for this mode. Triage writes its
// own, so that a selection can never be read as a review that found nothing.
func outputPathFor(mode reviewer.Mode) string {
	if mode == reviewer.ModeTriage {
		return reviewer.TriageOutputPath
	}
	return reviewer.OutputPath
}

// isMissingSession reports whether opencode refused because the session id it
// was given no longer exists.
func isMissingSession(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "session not found")
}

// args builds the command line. It is separate so that the invocation can be
// asserted in tests without running anything.
func (r *Runner) args(req reviewer.Request) []string {
	agent := r.cfg.ReviewAgent
	switch req.Mode {
	case reviewer.ModeAnswer:
		agent = r.cfg.AnswerAgent
	case reviewer.ModeTriage:
		agent = r.cfg.TriageAgent
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
	// The message comes before --file, and --file goes last. opencode takes
	// --file as an array, so anything after it is read as another path to
	// attach: with the two the other way round the message itself was taken
	// for a file name and every run died with "File not found: 指示は…".
	args = append(args,
		"指示は添付された .kibitz/prompt.md に書かれています。その指示に従ってください。",
		"--file", filepath.Join(".kibitz", "prompt.md"),
	)
	return args
}

// fullDiff is the whole pull request when the caller narrowed what the prompt
// carries, and the prompt's own diff otherwise. It is what makes a file triage
// left out still reachable through a tool.
func fullDiff(req reviewer.Request) *forge.Diff {
	if req.FullDiff != nil {
		return req.FullDiff
	}
	return req.Diff
}
