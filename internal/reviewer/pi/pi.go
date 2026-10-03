// Package pi runs the pi coding agent as the agent engine.
//
// It is the trial engine ADR-0024 calls for: the same contract as the
// opencode runner, so that the two can be compared on the same requests, and
// kept out of the default until that comparison is done.
//
// pi has no permission model. What opencode's permission block does here is
// done in two parts: the tools a mode may use are the only ones pi activates,
// and the kibitz-guard extension (deploy/pi/kibitz-guard.ts) checks every call
// against the tool list and keeps paths inside the checkout and writes inside
// what the mode may write. Everything pi reads at startup comes from a
// directory this runner writes per job, never from the repository.
package pi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yteraoka/kibitz/internal/egress"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/jobcontext"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/reviewer/opencode"
)

// Config configures the runner. The fields shared with the opencode runner
// mean the same thing there.
type Config struct {
	// Bin is the pi executable.
	Bin string
	// Model is the provider/model to run. pi's Vertex AI provider is called
	// google-vertex, as opencode's is.
	Model string
	// AgentsDir holds the agent definitions, kibitz-review.md and the rest.
	// They are the opencode ones: the front matter is dropped and the body
	// appended to pi's system prompt.
	AgentsDir string
	// GuardExtension is the path of kibitz-guard.ts. It is required: without
	// it pi would run its tools on any path the model names.
	GuardExtension string
	// SessionDir is where sessions are kept between jobs, so a follow-up
	// question can continue the conversation. It outlives the job directory
	// but not the container, like opencode's own storage.
	SessionDir     string
	MCPServers     map[string]opencode.MCPServer
	Env            []string
	ContextBin     string
	EnvPassthrough []string
	CustomProvider *opencode.CustomProvider
	Egress         *egress.Proxy
}

// Runner implements [reviewer.Engine] by invoking the pi CLI.
type Runner struct {
	cfg    Config
	logger *slog.Logger
}

// New builds a runner.
func New(cfg Config, logger *slog.Logger) *Runner {
	if cfg.Bin == "" {
		cfg.Bin = "pi"
	}
	if cfg.SessionDir == "" {
		cfg.SessionDir = filepath.Join(os.TempDir(), "kibitz-pi-sessions")
	}
	return &Runner{cfg: cfg, logger: logger}
}

// promptMessage is the user message. The instructions are attached as a
// file, as they are for opencode, so that the command line stays short.
const promptMessage = "指示は添付された .kibitz/prompt.md に書かれています。その指示に従ってください。"

// Run implements [reviewer.Engine].
func (r *Runner) Run(ctx context.Context, req reviewer.Request) (*reviewer.Result, error) {
	if req.WorkspaceDir == "" {
		return nil, errors.New("pi: workspace directory is empty")
	}
	if r.cfg.GuardExtension == "" {
		return nil, errors.New("pi: no guard extension is configured, and pi has no permissions of its own")
	}
	workspace, err := filepath.Abs(req.WorkspaceDir)
	if err != nil {
		return nil, fmt.Errorf("pi: %w", err)
	}

	jobDir, err := os.MkdirTemp("", "kibitz-pi-")
	if err != nil {
		return nil, fmt.Errorf("pi: creating job directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(jobDir) }()

	contextPath := ""
	if r.cfg.ContextBin != "" && req.Mode != reviewer.ModeTriage {
		path, err := jobcontext.Build(req.Event, req.PullRequest, fullDiff(req), req.Diff,
			req.ExistingComments, req.WorkspaceDir, req.References).Write(jobDir)
		if err != nil {
			return nil, fmt.Errorf("pi: %w", err)
		}
		contextPath = path
	}
	servers := r.serversFor(req, contextPath)

	prof, err := profileFor(req)
	if err != nil {
		return nil, err
	}
	agentDir := filepath.Join(jobDir, "agent")
	providerEnv, err := r.writeAgentDir(ctx, agentDir, req, prof, servers)
	if err != nil {
		return nil, err
	}

	rules := guardRules{Root: workspace, Tools: prof.tools, Writable: prof.writable}
	for name := range servers {
		rules.ToolPrefixes = append(rules.ToolPrefixes, toolPrefix(name))
	}
	sort.Strings(rules.ToolPrefixes)
	guard, err := json.Marshal(rules)
	if err != nil {
		return nil, fmt.Errorf("pi: encoding guard rules: %w", err)
	}

	env := r.childEnv(servers)
	env = append(env, providerEnv...)
	env = append(env,
		"PI_CODING_AGENT_DIR="+agentDir,
		"KIBITZ_PI_GUARD="+string(guard),
		// No update checks, no catalog refreshes, no install telemetry and
		// no provider attribution headers. The model's own endpoint is the
		// only thing this process has reason to reach.
		"PI_OFFLINE=1",
		"PI_SKIP_VERSION_CHECK=1",
		"PI_TELEMETRY=0",
	)
	if r.cfg.Egress != nil {
		session, err := r.cfg.Egress.Start(egressAttrs(req)...)
		if err != nil {
			return nil, fmt.Errorf("pi: %w", err)
		}
		defer func() { _ = session.Close() }()
		env = append(env, session.Env()...)
	}

	promptPath := filepath.Join(workspace, ".kibitz", "prompt.md")
	if err := writeFile(promptPath, []byte(reviewer.BuildPrompt(req))); err != nil {
		return nil, fmt.Errorf("pi: writing prompt: %w", err)
	}
	outputPath := filepath.Join(workspace, outputPathFor(req.Mode))
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o700); err != nil {
		return nil, fmt.Errorf("pi: creating output directory: %w", err)
	}

	system, err := r.systemPrompt(req.Mode)
	if err != nil {
		return nil, err
	}
	systemPath := ""
	if system != "" {
		systemPath = filepath.Join(jobDir, "system.md")
		if err := writeFile(systemPath, []byte(system)); err != nil {
			return nil, fmt.Errorf("pi: writing system prompt: %w", err)
		}
	}

	session := r.session(ctx, req)
	start := time.Now()
	stdout, err := r.run(ctx, workspace, req, env, r.args(req, session, systemPath))
	if err != nil {
		return nil, err
	}

	t := parseEvents(stdout, r.logger)
	result := &reviewer.Result{
		SessionID: firstNonEmpty(t.sessionID, session.id),
		Usage:     t.usage,
		Tools:     t.tools,
	}
	result.Usage.Duration = time.Since(start)

	// pi exits 0 when the provider failed: the failure is the last
	// assistant message. Reading the output file would then report a
	// missing file, which is the symptom rather than the cause.
	if t.stopReason == "error" || t.stopReason == "aborted" {
		return nil, fmt.Errorf("pi: the model's turn ended with %s: %s", t.stopReason, tail(t.errorMessage, 500))
	}

	switch req.Mode {
	case reviewer.ModeAnswer, reviewer.ModePlan:
		result.Reply = t.text
		if result.Reply == "" {
			return nil, fmt.Errorf("pi: the agent produced no %s", outputNameFor(req.Mode))
		}
		return result, nil
	case reviewer.ModeImplement:
		result.Reply = t.text
		return result, nil
	}

	data, err := os.ReadFile(outputPath) //nolint:gosec // a path this process just built
	if err != nil {
		attrs := []slog.Attr{
			slog.String("mode", string(req.Mode)),
			slog.String("expected", outputPathFor(req.Mode)),
			slog.Int("steps", t.steps),
			slog.Int("tool_calls", t.tools.Total()),
			slog.String("said_instead", tail(t.text, 1000)),
		}
		if req.Event != nil {
			attrs = append([]slog.Attr{slog.String("event_id", req.Event.ID)}, attrs...)
		}
		r.logger.LogAttrs(ctx, slog.LevelError, "the agent finished without writing its output", attrs...)
		return nil, fmt.Errorf("pi: the agent finished without writing %s (%d steps, %d tool calls): %w",
			outputPathFor(req.Mode), t.steps, t.tools.Total(), err)
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

// sessionRef says which session a run is in: an existing file to continue,
// or a new id to create.
type sessionRef struct {
	id   string
	file string
}

// sessionIDPattern is what pi accepts as a session id.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// session finds the conversation to continue, or names a new one.
//
// pi files a session under the directory it was started in and finds one by
// id only there. Every job has a fresh checkout, so a continuation is opened
// by its file instead, which pi accepts from anywhere.
func (r *Runner) session(ctx context.Context, req reviewer.Request) sessionRef {
	if id := req.SessionID; id != "" && sessionIDPattern.MatchString(id) {
		matches, _ := filepath.Glob(filepath.Join(r.cfg.SessionDir, "*_"+id+".jsonl"))
		if len(matches) > 0 {
			// The names start with a timestamp, so the last one is the
			// newest.
			sort.Strings(matches)
			return sessionRef{id: id, file: matches[len(matches)-1]}
		}
		// The container that held it is gone. The prompt carries the
		// context the run needs, so it simply starts again.
		r.logger.LogAttrs(ctx, slog.LevelInfo, "the stored session is gone; starting a new one",
			slog.String("session_id", id))
	}
	return sessionRef{id: newSessionID()}
}

func newSessionID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "kibitz-" + hex.EncodeToString(b[:])
}

// args builds the command line. It is separate so that tests can assert the
// invocation without running anything.
func (r *Runner) args(req reviewer.Request, session sessionRef, systemPath string) []string {
	args := []string{
		"--mode", "json",
		// Nothing in the checkout is loaded as configuration or as
		// instructions. A pull request could otherwise write AGENTS.md, a
		// skill, or .pi/settings.json, and with them the reviewer's own
		// instructions (ADR-0010). The conventions still reach the agent,
		// read from the default branch by the worker.
		"--no-approve",
		"--no-context-files",
		"--no-skills",
		"--no-prompt-templates",
		"--extension", r.cfg.GuardExtension,
		"--session-dir", r.cfg.SessionDir,
	}
	if model := r.modelFor(req); model != "" {
		args = append(args, "--model", model)
	}
	if session.file != "" {
		args = append(args, "--session", session.file)
	} else {
		args = append(args, "--session-id", session.id)
	}
	if systemPath != "" {
		args = append(args, "--append-system-prompt", systemPath)
	}
	// "--" ends the options, so nothing after it is read as one.
	return append(args, "--", "@"+filepath.Join(".kibitz", "prompt.md"), promptMessage)
}

// run starts the CLI and returns its stdout.
func (r *Runner) run(ctx context.Context, workspace string, req reviewer.Request, env, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.cfg.Bin, args...) //nolint:gosec // the binary comes from configuration, not from the pull request
	cmd.Dir = workspace
	cmd.Env = env
	// pi reads a prompt from stdin when it is not a terminal; there is none.
	cmd.Stdin = nil
	setupProcessGroup(cmd)

	stderr := &tailBuffer{max: maxStderr}
	cmd.Stderr = stderr

	started := time.Now()
	stdout, err := cmd.Output()
	if err == nil {
		return stdout, nil
	}

	attrs := []slog.Attr{
		slog.String("mode", string(req.Mode)),
		slog.String("model", r.modelFor(req)),
		slog.Duration("elapsed", time.Since(started)),
		slog.String("error", err.Error()),
		slog.String("stderr", tail(strings.TrimSpace(stderr.String()), 2000)),
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		attrs = append(attrs, slog.Bool("timed_out", true))
	}
	if req.Event != nil {
		attrs = append([]slog.Attr{slog.String("event_id", req.Event.ID)}, attrs...)
	}
	r.logger.LogAttrs(ctx, slog.LevelError, "the agent failed", attrs...)
	return nil, fmt.Errorf("pi: %w: %s", err, tail(strings.TrimSpace(stderr.String()), 500))
}

// serversFor picks the MCP servers this run may use. The rules are the
// opencode runner's: nothing for triage, the repository's choice narrowed by
// the deployment's catalog, nothing a fork may not have, and kibitz's own
// server whenever there is a context for it.
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

// childEnv builds the agent's environment from a list, as the opencode
// runner does, so the worker's own secrets stay in the worker.
func (r *Runner) childEnv(servers map[string]opencode.MCPServer) []string {
	wanted := map[string]bool{}
	for _, name := range opencode.BaseEnv() {
		wanted[name] = true
	}
	for _, name := range r.cfg.EnvPassthrough {
		if name = strings.TrimSpace(name); name != "" {
			wanted[name] = true
		}
	}
	for _, server := range servers {
		for _, name := range server.EnvRefs() {
			wanted[name] = true
		}
	}
	names := make([]string, 0, len(wanted))
	for name := range wanted {
		names = append(names, name)
	}
	sort.Strings(names)

	env := make([]string, 0, len(names)+len(r.cfg.Env))
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return append(env, slices.Clip(r.cfg.Env)...)
}

func (r *Runner) modelFor(req reviewer.Request) string {
	if req.Model != "" {
		return req.Model
	}
	return r.cfg.Model
}

func egressAttrs(req reviewer.Request) []slog.Attr {
	attrs := []slog.Attr{slog.String("mode", string(req.Mode)), slog.String("engine", "pi")}
	if req.Event != nil {
		attrs = append([]slog.Attr{slog.String("event_id", req.Event.ID)}, attrs...)
	}
	return attrs
}

func outputPathFor(mode reviewer.Mode) string {
	if mode == reviewer.ModeTriage {
		return reviewer.TriageOutputPath
	}
	return reviewer.OutputPath
}

func outputNameFor(mode reviewer.Mode) string {
	if mode == reviewer.ModePlan {
		return "plan"
	}
	return "answer"
}

func fullDiff(req reviewer.Request) *forge.Diff {
	if req.FullDiff != nil {
		return req.FullDiff
	}
	return req.Diff
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// tail keeps the last n bytes of s without splitting a character.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return "…" + s[cut:]
}

// maxStderr is how much of pi's stderr is kept for the log.
const maxStderr = 64 << 10

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = b.buf[over:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string { return string(b.buf) }
