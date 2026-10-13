// Package adk runs the agent in-process with Google's Agent Development Kit
// for Go (google.golang.org/adk).
//
// It is a trial engine, as pi is: the same contract as the opencode runner,
// so that the engines can be compared on the same requests, and kept out of
// the default until that comparison is done.
//
// Unlike the other two it starts no agent process. The loop, the tools and
// the model client all live in the worker, which changes where the safety
// properties come from:
//
//   - The model has the tools this package registers and no others. There is
//     no shell, and nothing reads AGENTS.md, a skill or a settings file from
//     the checkout, because ADK reads none of those: there is no switch to
//     forget to turn off.
//   - The file tools are confined to the checkout by os.Root, at the moment a
//     file is opened, rather than by an engine's permission rules.
//   - Third-party MCP servers still run as their own processes, with the same
//     trimmed environment and egress session the other engines give them.
//
// Only Gemini is supported, because ADK has no model client for anything else
// on Vertex AI yet. Implementation mode is not supported either: it needs an
// edit tool whose rules have not been written for this engine.
package adk

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/yteraoka/kibitz/internal/egress"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/jobcontext"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/reviewer/opencode"
)

// Config configures the runner. The fields shared with the other runners mean
// the same thing there.
type Config struct {
	// Model is the model to run, "google-vertex/gemini-…" as the other
	// engines spell it or a bare "gemini-…".
	Model string
	// Project and Location say where on Vertex AI the model runs.
	// Authentication is ADC.
	Project  string
	Location string
	// AgentsDir holds the agent definitions, kibitz-review.md and the rest.
	// They are the opencode ones: the front matter is dropped and the body
	// becomes the instruction.
	AgentsDir      string
	MCPServers     map[string]opencode.MCPServer
	ContextBin     string
	EnvPassthrough []string
	// Egress is the proxy MCP servers reach the network through. The model
	// client does not use it; see [Runner.Run].
	Egress *egress.Proxy
	// MaxModelCalls stops a run that keeps calling the model. Zero is
	// [DefaultMaxModelCalls].
	MaxModelCalls int
	// MaxSessions bounds how many conversations are kept for follow-up
	// questions. Zero is [DefaultMaxSessions].
	MaxSessions int

	// newModel builds the model client. Tests replace it.
	newModel func(ctx context.Context, name string) (model.LLM, error)
}

// Defaults.
const (
	DefaultMaxModelCalls = 100
	DefaultMaxSessions   = 200
)

// Runner implements [reviewer.Engine] with ADK.
type Runner struct {
	cfg    Config
	logger *slog.Logger

	// sessions keeps conversations in memory, so that a follow-up question
	// on the same pull request continues the review. It lives as long as the
	// container, like opencode's own storage, which is to say it is an
	// optimization and not something a run can count on.
	sessions session.Service
	mu       sync.Mutex
	order    []string
}

// New builds a runner.
func New(cfg Config, logger *slog.Logger) *Runner {
	if cfg.MaxModelCalls <= 0 {
		cfg.MaxModelCalls = DefaultMaxModelCalls
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = DefaultMaxSessions
	}
	r := &Runner{cfg: cfg, logger: logger, sessions: session.InMemoryService()}
	if r.cfg.newModel == nil {
		r.cfg.newModel = r.vertexModel
	}
	return r
}

const (
	appName = "kibitz"
	userID  = "kibitz"
)

// ModelName returns the name Vertex AI knows a model by, or an error for a
// model this engine cannot run.
func ModelName(m string) (string, error) {
	name := strings.TrimPrefix(m, "google-vertex/")
	if !strings.HasPrefix(name, "gemini-") || strings.Contains(name, "/") {
		return "", fmt.Errorf("the adk engine runs Gemini on Vertex AI only, got %q", m)
	}
	return name, nil
}

// vertexModel builds a Gemini client on Vertex AI with ADC.
//
// It does not go through the egress proxy. The proxy exists to see and limit
// where the agent goes, and the model's endpoint is not the agent's choice:
// it is this worker's own call, to a host fixed by configuration, made with a
// credential the agent never sees. With opencode the call happened to come
// from the agent's process and so went through the proxy too; here it does
// not, and the deployment no longer needs to allow Vertex AI there.
func (r *Runner) vertexModel(ctx context.Context, name string) (model.LLM, error) {
	return gemini.NewModel(ctx, name, &genai.ClientConfig{
		Backend:  genai.BackendVertexAI,
		Project:  r.cfg.Project,
		Location: r.cfg.Location,
	})
}

// Run implements [reviewer.Engine].
func (r *Runner) Run(ctx context.Context, req reviewer.Request) (*reviewer.Result, error) {
	if req.WorkspaceDir == "" {
		return nil, errors.New("adk: workspace directory is empty")
	}
	if req.Mode == reviewer.ModeImplement {
		return nil, errors.New("adk: implementation mode is not supported by this engine")
	}
	modelName, err := ModelName(r.modelFor(req))
	if err != nil {
		return nil, fmt.Errorf("adk: %w", err)
	}
	llm, err := r.cfg.newModel(ctx, modelName)
	if err != nil {
		return nil, fmt.Errorf("adk: creating the model client: %w", err)
	}

	jobDir, err := os.MkdirTemp("", "kibitz-adk-")
	if err != nil {
		return nil, fmt.Errorf("adk: creating job directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(jobDir) }()

	contextPath := ""
	if r.cfg.ContextBin != "" && req.Mode != reviewer.ModeTriage {
		p, err := jobcontext.Build(req.Event, req.PullRequest, fullDiff(req), req.Diff,
			req.ExistingComments, req.WorkspaceDir, req.References).Write(jobDir)
		if err != nil {
			return nil, fmt.Errorf("adk: %w", err)
		}
		contextPath = p
	}

	ws, err := openWorkspace(req.WorkspaceDir, writableFor(req.Mode))
	if err != nil {
		return nil, fmt.Errorf("adk: opening the workspace: %w", err)
	}
	defer func() { _ = ws.Close() }()
	builtin, err := ws.tools()
	if err != nil {
		return nil, fmt.Errorf("adk: %w", err)
	}

	var egressSession *egress.Session
	if r.cfg.Egress != nil {
		egressSession, err = r.cfg.Egress.Start(egressAttrs(req)...)
		if err != nil {
			return nil, fmt.Errorf("adk: %w", err)
		}
		defer func() { _ = egressSession.Close() }()
	}
	servers := &uniqueToolset{
		reserved: toolNames(builtin),
		sets:     r.toolsets(ctx, r.serversFor(req, contextPath), egressSession),
		logger:   r.logger,
	}
	defer func() { _ = servers.Close() }()

	system, err := r.systemPrompt(req.Mode)
	if err != nil {
		return nil, err
	}

	// The output file is the contract, as with the other engines. A file
	// left from an earlier attempt would read as this attempt's answer.
	outputPath := ""
	if writes(req.Mode) {
		outputPath = outputPathFor(req.Mode)
		_ = ws.root.Remove(outputPath)
	}

	calls := 0
	a, err := llmagent.New(llmagent.Config{
		Name:        agentFor(req.Mode),
		Description: "kibitz " + string(req.Mode),
		Model:       llm,
		// A provider, not Instruction: Instruction is a template, and the
		// definitions are full of braces from the JSON they describe.
		InstructionProvider: func(agent.ReadonlyContext) (string, error) { return system, nil },
		Tools:               builtin,
		Toolsets:            []tool.Toolset{servers},
		BeforeModelCallbacks: []llmagent.BeforeModelCallback{
			func(agent.Context, *model.LLMRequest) (*model.LLMResponse, error) {
				calls++
				if calls > r.cfg.MaxModelCalls {
					return nil, fmt.Errorf("the run called the model more than %d times", r.cfg.MaxModelCalls)
				}
				return nil, nil
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("adk: building the agent: %w", err)
	}
	run, err := runner.New(runner.Config{
		AppName:           appName,
		Agent:             a,
		SessionService:    r.sessions,
		AutoCreateSession: true,
	})
	if err != nil {
		return nil, fmt.Errorf("adk: building the runner: %w", err)
	}

	sessionID := r.session(ctx, req)
	message := genai.NewContentFromText(reviewer.BuildPrompt(req), genai.RoleUser)

	start := time.Now()
	var t transcript
	var runErr error
	for ev, err := range run.Run(ctx, userID, sessionID, message, agent.RunConfig{StreamingMode: agent.StreamingModeNone}) {
		if err != nil {
			runErr = err
			break
		}
		t.record(ev)
	}
	r.remember(ctx, sessionID)

	result := &reviewer.Result{SessionID: sessionID, Usage: t.usage, Tools: t.tools}
	result.Usage.Duration = time.Since(start)

	if runErr != nil {
		r.logFailure(ctx, req, modelName, &t, runErr)
		return nil, fmt.Errorf("adk: %w", runErr)
	}
	if t.errorCode != "" {
		r.logFailure(ctx, req, modelName, &t, nil)
		return nil, fmt.Errorf("adk: the model returned %s: %s", t.errorCode, tail(t.errorMessage, 500))
	}

	switch req.Mode {
	case reviewer.ModeAnswer, reviewer.ModePlan:
		result.Reply = strings.TrimSpace(t.text)
		if result.Reply == "" {
			r.logFailure(ctx, req, modelName, &t, nil)
			return nil, fmt.Errorf("adk: the agent produced no %s (finish reason %q)", outputNameFor(req.Mode), t.finishReason)
		}
		return result, nil
	}

	data, err := ws.root.ReadFile(outputPath)
	if err != nil {
		attrs := []slog.Attr{
			slog.String("mode", string(req.Mode)),
			slog.String("expected", outputPath),
			slog.Int("model_calls", t.modelCalls),
			slog.Int("tool_calls", t.tools.Total()),
			slog.String("finish_reason", string(t.finishReason)),
			slog.String("said_instead", tail(t.text, 1000)),
		}
		if req.Event != nil {
			attrs = append([]slog.Attr{slog.String("event_id", req.Event.ID)}, attrs...)
		}
		r.logger.LogAttrs(ctx, slog.LevelError, "the agent finished without writing its output", attrs...)
		return nil, fmt.Errorf("adk: the agent finished without writing %s (%d model calls, %d tool calls): %w",
			outputPath, t.modelCalls, t.tools.Total(), err)
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

// session names the conversation a run belongs to: the stored one when the
// request carries an id, a new one otherwise. A stored id whose session this
// container never held — it was another container's, or another engine's —
// starts a new conversation under that id, which costs nothing: the prompt
// carries what the run needs either way.
func (r *Runner) session(ctx context.Context, req reviewer.Request) string {
	id := req.SessionID
	if id == "" {
		return newSessionID()
	}
	if _, err := r.sessions.Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: id}); err != nil {
		r.logger.LogAttrs(ctx, slog.LevelInfo, "the stored session is gone; starting a new one",
			slog.String("session_id", id))
	}
	return id
}

// remember records that a session was used and forgets the oldest ones past
// the limit, so a long-lived worker does not keep every conversation it ever
// had.
func (r *Runner) remember(ctx context.Context, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, known := range r.order {
		if known == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	r.order = append(r.order, id)
	for len(r.order) > r.cfg.MaxSessions {
		oldest := r.order[0]
		r.order = r.order[1:]
		_ = r.sessions.Delete(ctx, &session.DeleteRequest{AppName: appName, UserID: userID, SessionID: oldest})
	}
}

func newSessionID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "kibitz-" + hex.EncodeToString(b[:])
}

func (r *Runner) logFailure(ctx context.Context, req reviewer.Request, modelName string, t *transcript, err error) {
	attrs := []slog.Attr{
		slog.String("mode", string(req.Mode)),
		slog.String("model", modelName),
		slog.Int("model_calls", t.modelCalls),
		slog.Int("tool_calls", t.tools.Total()),
		slog.String("finish_reason", string(t.finishReason)),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	if t.errorCode != "" {
		attrs = append(attrs, slog.String("error_code", t.errorCode), slog.String("error_message", tail(t.errorMessage, 2000)))
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		attrs = append(attrs, slog.Bool("timed_out", true))
	}
	if req.Event != nil {
		attrs = append([]slog.Attr{slog.String("event_id", req.Event.ID)}, attrs...)
	}
	r.logger.LogAttrs(ctx, slog.LevelError, "the agent failed", attrs...)
}

// systemPrompt reads the agent definition for a mode and returns its body.
// The definitions are opencode's, shared by every engine so that a comparison
// between them compares the engines and not the instructions.
func (r *Runner) systemPrompt(mode reviewer.Mode) (string, error) {
	if r.cfg.AgentsDir == "" {
		return "", nil
	}
	p := filepath.Join(r.cfg.AgentsDir, agentFor(mode)+".md")
	data, err := os.ReadFile(p) //nolint:gosec // a path from configuration, not from the pull request
	if err != nil {
		return "", fmt.Errorf("adk: reading the agent definition: %w", err)
	}
	return strings.TrimSpace(stripFrontMatter(string(data))), nil
}

// agentFor names the definition each mode runs with. It is also the agent's
// name in ADK, which requires an identifier.
func agentFor(mode reviewer.Mode) string {
	switch mode {
	case reviewer.ModeAnswer:
		return "kibitz-answer"
	case reviewer.ModePlan:
		return "kibitz-plan"
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

// writes reports whether a mode's answer is a file.
func writes(mode reviewer.Mode) bool {
	return mode == reviewer.ModeReview || mode == reviewer.ModeTriage
}

// writableFor is where a mode may write: its own output file and nothing
// else. Answer and plan write nothing, so they get no write tool at all.
func writableFor(mode reviewer.Mode) func(string) bool {
	if !writes(mode) {
		return nil
	}
	want := path.Clean(outputPathFor(mode))
	return func(p string) bool { return p == want }
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

func toolNames(tools []tool.Tool) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name()
	}
	return names
}

func (r *Runner) modelFor(req reviewer.Request) string {
	if req.Model != "" {
		return req.Model
	}
	return r.cfg.Model
}

func egressAttrs(req reviewer.Request) []slog.Attr {
	attrs := []slog.Attr{slog.String("mode", string(req.Mode)), slog.String("engine", "adk")}
	if req.Event != nil {
		attrs = append([]slog.Attr{slog.String("event_id", req.Event.ID)}, attrs...)
	}
	return attrs
}

func fullDiff(req reviewer.Request) *forge.Diff {
	if req.FullDiff != nil {
		return req.FullDiff
	}
	return req.Diff
}

// tail keeps the last n bytes of s without splitting a character.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut++
	}
	return "…" + s[cut:]
}
