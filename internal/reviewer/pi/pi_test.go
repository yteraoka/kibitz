package pi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/reviewer/opencode"
	"github.com/yteraoka/kibitz/internal/reviewer/pi"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// harness runs the runner against a shell script standing in for pi. The
// script records its arguments, its environment and the agent directory the
// runner wrote, because the runner deletes that directory when it returns.
type harness struct {
	bin       string
	workspace string
	record    string
	sessions  string
}

func newHarness(t *testing.T, script string) harness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake CLI is a shell script")
	}
	h := harness{workspace: t.TempDir(), record: t.TempDir(), sessions: t.TempDir()}
	h.bin = filepath.Join(t.TempDir(), "pi")
	body := "#!/bin/sh\nset -e\n" +
		"printf '%s\\n' \"$@\" > '" + h.record + "/args'\n" +
		"printenv > '" + h.record + "/env'\n" +
		"cp -r \"$PI_CODING_AGENT_DIR\" '" + h.record + "/agent'\n" +
		script
	if err := os.WriteFile(h.bin, []byte(body), 0o700); err != nil { //nolint:gosec // a test fixture
		t.Fatalf("writing the fake CLI: %v", err)
	}
	return h
}

func (h harness) runner(cfg pi.Config) *pi.Runner {
	cfg.Bin = h.bin
	if cfg.GuardExtension == "" {
		cfg.GuardExtension = "/etc/kibitz/pi/kibitz-guard.ts"
	}
	cfg.SessionDir = h.sessions
	return pi.New(cfg, discardLogger())
}

func (h harness) args(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.record, "args"))
	if err != nil {
		t.Fatalf("reading recorded arguments: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func (h harness) env(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.record, "env"))
	if err != nil {
		t.Fatalf("reading the recorded environment: %v", err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			env[name] = value
		}
	}
	return env
}

func (h harness) file(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.record, "agent", name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return data
}

func (h harness) settings(t *testing.T) map[string]any {
	t.Helper()
	var s map[string]any
	if err := json.Unmarshal(h.file(t, "settings.json"), &s); err != nil {
		t.Fatalf("decoding settings.json: %v", err)
	}
	return s
}

type guardRules struct {
	Root         string   `json:"root"`
	Tools        []string `json:"tools"`
	ToolPrefixes []string `json:"toolPrefixes"`
	Writable     string   `json:"writable"`
}

func (h harness) guard(t *testing.T) guardRules {
	t.Helper()
	var rules guardRules
	if err := json.Unmarshal([]byte(h.env(t)["KIBITZ_PI_GUARD"]), &rules); err != nil {
		t.Fatalf("decoding KIBITZ_PI_GUARD: %v", err)
	}
	return rules
}

func request(workspace string, mode reviewer.Mode) reviewer.Request {
	return reviewer.Request{
		Mode:         mode,
		WorkspaceDir: workspace,
		Event:        &event.ReviewEvent{ID: "evt-1", Repository: event.Repository{FullName: "yteraoka/kibitz"}},
		PullRequest:  &event.PullRequest{Number: 42, Title: "Add the SQS subscriber"},
		Diff: &forge.Diff{Files: []forge.File{
			{Path: "queue.go", Status: forge.FileModified, Patch: "@@ -1,2 +1,3 @@\n+x"},
		}},
		HeadSHA: "abc1234",
	}
}

// events is a pi --mode json stream in the shape pi 1.0 writes, trimmed to
// the fields kibitz reads. The tool calls are a read and kibitz's get_doc.
const events = `
echo '{"type":"session","version":3,"id":"kibitz-new","timestamp":"2026-10-02T00:00:00.000Z","cwd":"/ws"}'
echo '{"type":"agent_start"}'
echo '{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"x"}]}}'
echo '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"まず読みます"},{"type":"toolCall","id":"c1","name":"read","arguments":{}}],"stopReason":"toolUse","usage":{"input":1200,"output":340,"cacheRead":9000,"cacheWrite":500,"reasoning":20,"totalTokens":11040}}}'
echo '{"type":"tool_execution_start","toolCallId":"c1","toolName":"read","args":{"path":"queue.go"}}'
echo '{"type":"tool_execution_end","toolCallId":"c1","toolName":"read","result":{"content":[]},"isError":false}'
echo '{"type":"tool_execution_start","toolCallId":"c2","toolName":"mcp__kibitz__get_doc","args":{"path":"docs/adr/0010-data-and-instruction-positions.md"}}'
echo '{"type":"tool_execution_end","toolCallId":"c2","toolName":"mcp__kibitz__get_doc","result":{"content":[]},"isError":false}'
echo '{"type":"tool_execution_start","toolCallId":"c3","toolName":"read","args":{"path":"/etc/passwd"}}'
echo '{"type":"tool_execution_end","toolCallId":"c3","toolName":"read","result":{"content":[]},"isError":true}'
echo '{"type":"message_end","message":{"role":"assistant","content":[{"type":"thinking","thinking":"..."},{"type":"text","text":"回答です"}],"stopReason":"stop","usage":{"input":300,"output":60,"cacheRead":1000,"cacheWrite":0,"totalTokens":1360}}}'
echo '{"type":"agent_end","messages":[]}'
`

const writeOutput = `
mkdir -p .kibitz/out
cat > .kibitz/out/review.json <<'JSON'
{
  "schema_version": 1,
  "summary": "SQS subscriber を追加する変更",
  "comments": [
    {"path": "queue.go", "line": 2, "severity": "high", "title": "漏れる", "body": "ctx を見ていない"}
  ]
}
JSON
` + events

func TestRunReview(t *testing.T) {
	h := newHarness(t, writeOutput)
	result, err := h.runner(pi.Config{Model: "google-vertex/gemini-3.1-pro-preview"}).
		Run(context.Background(), request(h.workspace, reviewer.ModeReview))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(result.Findings) != 1 || result.Findings[0].Severity != reviewer.SeverityHigh {
		t.Errorf("findings = %+v", result.Findings)
	}
	if result.SessionID != "kibitz-new" {
		t.Errorf("session = %q, want the one the stream announced", result.SessionID)
	}
	// Reasoning is part of pi's output count and is taken out of it.
	want := reviewer.Usage{InputTokens: 1500, CacheReadTokens: 10000, CacheWriteTokens: 500, OutputTokens: 380, ReasoningTokens: 20}
	got := result.Usage
	got.Duration = 0
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
	if result.Tools.Calls["read"] != 2 || result.Tools.Failed["read"] != 1 {
		t.Errorf("tool calls = %v, failed = %v", result.Tools.Calls, result.Tools.Failed)
	}
	if !slices.Equal(result.Tools.Documents, []string{"docs/adr/0010-data-and-instruction-positions.md"}) {
		t.Errorf("documents = %v", result.Tools.Documents)
	}
}

func TestArguments(t *testing.T) {
	h := newHarness(t, writeOutput)
	if _, err := h.runner(pi.Config{Model: "google-vertex/gemini-3.1-pro-preview"}).
		Run(context.Background(), request(h.workspace, reviewer.ModeReview)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	args := h.args(t)
	for _, want := range []string{"--no-approve", "--no-context-files", "--no-skills", "--no-prompt-templates"} {
		if !slices.Contains(args, want) {
			t.Errorf("%s is missing: the checkout could configure or instruct the agent", want)
		}
	}
	// --tools would drop the MCP servers' tools as well; the selection is
	// made in settings.json instead.
	if slices.Contains(args, "--tools") || slices.Contains(args, "--no-extensions") {
		t.Errorf("args = %v", args)
	}
	if i := slices.Index(args, "--extension"); i < 0 || args[i+1] != "/etc/kibitz/pi/kibitz-guard.ts" {
		t.Errorf("the guard extension is not loaded: %v", args)
	}
	if i := slices.Index(args, "--model"); i < 0 || args[i+1] != "google-vertex/gemini-3.1-pro-preview" {
		t.Errorf("model: %v", args)
	}
	sep := slices.Index(args, "--")
	if sep < 0 || args[sep+1] != "@.kibitz/prompt.md" {
		t.Errorf("the prompt file is not attached after --: %v", args)
	}

	env := h.env(t)
	for name, want := range map[string]string{"PI_OFFLINE": "1", "PI_TELEMETRY": "0", "PI_SKIP_VERSION_CHECK": "1"} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
	if !strings.HasPrefix(env["PI_CODING_AGENT_DIR"], os.TempDir()) {
		t.Errorf("the agent directory is not the job's own: %q", env["PI_CODING_AGENT_DIR"])
	}
	settings := h.settings(t)
	if settings["defaultProjectTrust"] != "never" || settings["enableInstallTelemetry"] != false {
		t.Errorf("settings = %v", settings)
	}
}

func TestNoModeHasAShell(t *testing.T) {
	for _, mode := range []reviewer.Mode{reviewer.ModeReview, reviewer.ModeAnswer, reviewer.ModePlan, reviewer.ModeImplement, reviewer.ModeTriage} {
		t.Run(string(mode), func(t *testing.T) {
			h := newHarness(t, writeOutput+`
mkdir -p .kibitz/out && echo '{"schema_version":1,"files":[]}' > .kibitz/out/triage.json`)
			req := request(h.workspace, mode)
			req.EditablePaths = []string{"docs/**"}
			_, _ = h.runner(pi.Config{}).Run(context.Background(), req)

			tools, _ := h.settings(t)["defaultTools"].([]any)
			for _, tool := range tools {
				if tool == "bash" || tool == "powershell" || tool == "codemode" {
					t.Errorf("%s is enabled", tool)
				}
			}
			if !slices.Equal(anyStrings(tools), h.guard(t).Tools) {
				t.Errorf("the guard's tools %v differ from the enabled ones %v", h.guard(t).Tools, tools)
			}
			root, _ := filepath.Abs(h.workspace)
			if h.guard(t).Root != root {
				t.Errorf("guard root = %q, want %q", h.guard(t).Root, root)
			}
		})
	}
}

func TestWhatEachModeMayWrite(t *testing.T) {
	cases := []struct {
		mode     reviewer.Mode
		editable []string
		may      []string
		mayNot   []string
		tools    []string
	}{
		{reviewer.ModeReview, nil, []string{".kibitz/out/review.json"}, []string{"queue.go", ".kibitz/prompt.md", ".kibitz/out/a/b"}, []string{"write"}},
		{reviewer.ModeTriage, nil, []string{".kibitz/out/triage.json"}, []string{"queue.go"}, []string{"write"}},
		{reviewer.ModeAnswer, nil, nil, []string{".kibitz/out/review.json"}, nil},
		{reviewer.ModePlan, nil, nil, []string{".kibitz/out/review.json"}, nil},
		{reviewer.ModeImplement, []string{"docs/**", "*.md"}, []string{"docs/a/b.md", "README.md"}, []string{"queue.go", "src/README.md"}, []string{"edit", "write"}},
		{reviewer.ModeImplement, nil, nil, []string{"README.md"}, []string{"edit", "write"}},
	}
	for _, c := range cases {
		t.Run(string(c.mode), func(t *testing.T) {
			h := newHarness(t, writeOutput)
			req := request(h.workspace, c.mode)
			req.EditablePaths = c.editable
			_, _ = h.runner(pi.Config{}).Run(context.Background(), req)

			rules := h.guard(t)
			var writable *regexp.Regexp
			if rules.Writable != "" {
				writable = regexp.MustCompile(rules.Writable)
			}
			for _, path := range c.may {
				if writable == nil || !writable.MatchString(path) {
					t.Errorf("%s may not be written, want it to be", path)
				}
			}
			for _, path := range c.mayNot {
				if writable != nil && writable.MatchString(path) {
					t.Errorf("%s may be written", path)
				}
			}
			for _, tool := range []string{"edit", "write"} {
				if slices.Contains(rules.Tools, tool) != slices.Contains(c.tools, tool) {
					t.Errorf("%s enabled = %v", tool, slices.Contains(rules.Tools, tool))
				}
			}
		})
	}
}

func TestMCPServers(t *testing.T) {
	t.Setenv("JIRA_TOKEN", "jira-token")
	t.Setenv("KIBITZ_GITHUB_PRIVATE_KEY", "-----BEGIN PRIVATE KEY-----")

	h := newHarness(t, writeOutput)
	runner := h.runner(pi.Config{
		ContextBin: "/usr/local/bin/kibitz-mcp",
		MCPServers: opencode.Catalog{
			"jira": {
				Type:      opencode.MCPRemote,
				URL:       "https://jira.example.com/mcp",
				Headers:   map[string]string{"Authorization": "Bearer {env:JIRA_TOKEN}"},
				TimeoutMS: 1500,
			},
			"sentry.io": {Type: opencode.MCPLocal, Command: []string{"sentry-mcp", "--stdio"}},
			"unasked":   {Type: opencode.MCPLocal, Command: []string{"x"}},
		},
	})
	req := request(h.workspace, reviewer.ModeReview)
	req.MCP = []string{"jira", "sentry.io"}
	if _, err := runner.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var mcp struct {
		Servers            map[string]map[string]any `json:"mcpServers"`
		AutoEnableCodemode *bool                     `json:"autoEnableCodemode"`
	}
	data := h.file(t, "mcp.json")
	if err := json.Unmarshal(data, &mcp); err != nil {
		t.Fatalf("decoding mcp.json: %v", err)
	}
	if mcp.AutoEnableCodemode == nil || *mcp.AutoEnableCodemode {
		t.Error("codemode is not turned off")
	}
	if len(mcp.Servers) != 3 {
		t.Errorf("servers = %v, want jira, sentry_io and kibitz", mcp.Servers)
	}
	for name, server := range mcp.Servers {
		if server["exposure"] != "direct" {
			t.Errorf("%s exposure = %v", name, server["exposure"])
		}
	}
	jira := mcp.Servers["jira"]
	if headers, _ := jira["headers"].(map[string]any); headers["Authorization"] != "Bearer ${JIRA_TOKEN}" {
		t.Errorf("jira headers = %v", jira["headers"])
	}
	if jira["timeout"] != float64(2) {
		t.Errorf("jira timeout = %v, want 2 seconds", jira["timeout"])
	}
	if sentry := mcp.Servers["sentry_io"]; sentry["command"] != "sentry-mcp" || !slices.Equal(anyStrings(sentry["args"].([]any)), []string{"--stdio"}) {
		t.Errorf("sentry = %v", sentry)
	}
	if strings.Contains(string(data), "jira-token") {
		t.Error("the credential was written into mcp.json")
	}

	env := h.env(t)
	if env["JIRA_TOKEN"] != "jira-token" {
		t.Error("the variable the server refers to did not reach the agent")
	}
	if _, leaked := env["KIBITZ_GITHUB_PRIVATE_KEY"]; leaked {
		t.Error("the worker's secret reached the agent")
	}
	want := []string{"mcp__jira__", "mcp__kibitz__", "mcp__sentry_io__"}
	if got := h.guard(t).ToolPrefixes; !slices.Equal(got, want) {
		t.Errorf("tool prefixes = %v, want %v", got, want)
	}
}

func TestForkGetsOnlyForkServers(t *testing.T) {
	h := newHarness(t, writeOutput)
	runner := h.runner(pi.Config{MCPServers: opencode.Catalog{
		"jira": {Type: opencode.MCPRemote, URL: "https://jira.example.com/mcp"},
		"docs": {Type: opencode.MCPRemote, URL: "https://docs.example.com/mcp", AllowFork: true},
	}})
	req := request(h.workspace, reviewer.ModeReview)
	req.MCP = []string{"jira", "docs"}
	req.PullRequest.IsFork = true
	if _, err := runner.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := h.guard(t).ToolPrefixes; !slices.Equal(got, []string{"mcp__docs__"}) {
		t.Errorf("tool prefixes = %v", got)
	}
}

func TestTriageGetsNoServers(t *testing.T) {
	h := newHarness(t, `mkdir -p .kibitz/out && echo '{"schema_version":1,"files":[]}' > .kibitz/out/triage.json`+events)
	runner := h.runner(pi.Config{
		ContextBin: "/usr/local/bin/kibitz-mcp",
		MCPServers: opencode.Catalog{"jira": {Type: opencode.MCPRemote, URL: "https://jira.example.com/mcp"}},
	})
	req := request(h.workspace, reviewer.ModeTriage)
	req.MCP = []string{"jira"}
	_, _ = runner.Run(context.Background(), req)
	if got := h.guard(t).ToolPrefixes; len(got) != 0 {
		t.Errorf("triage got servers: %v", got)
	}
}

func TestAnswerIsTheLastMessage(t *testing.T) {
	h := newHarness(t, events)
	result, err := h.runner(pi.Config{}).Run(context.Background(), request(h.workspace, reviewer.ModeAnswer))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Reply != "回答です" {
		t.Errorf("reply = %q, want the last message's text only", result.Reply)
	}
}

func TestAProviderFailureIsAnError(t *testing.T) {
	h := newHarness(t, `echo '{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"403 permission denied on Vertex AI"}}'`)
	_, err := h.runner(pi.Config{}).Run(context.Background(), request(h.workspace, reviewer.ModeReview))
	if err == nil || !strings.Contains(err.Error(), "403 permission denied") {
		t.Errorf("err = %v, want the provider's message", err)
	}
}

func TestMissingOutputIsReported(t *testing.T) {
	h := newHarness(t, events)
	_, err := h.runner(pi.Config{}).Run(context.Background(), request(h.workspace, reviewer.ModeReview))
	if err == nil || !strings.Contains(err.Error(), "without writing") {
		t.Errorf("err = %v", err)
	}
}

func TestAFailedProcessIsAnError(t *testing.T) {
	h := newHarness(t, `echo 'Error: no API key for google-vertex' >&2; exit 1`)
	_, err := h.runner(pi.Config{}).Run(context.Background(), request(h.workspace, reviewer.ModeReview))
	if err == nil || !strings.Contains(err.Error(), "no API key") {
		t.Errorf("err = %v, want stderr in it", err)
	}
}

func TestNoGuardNoRun(t *testing.T) {
	h := newHarness(t, writeOutput)
	runner := pi.New(pi.Config{Bin: h.bin}, discardLogger())
	if _, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview)); err == nil {
		t.Error("ran without the guard extension")
	}
	if _, err := os.Stat(filepath.Join(h.record, "args")); err == nil {
		t.Error("pi was started")
	}
}

func TestSessions(t *testing.T) {
	t.Run("a new run names its session", func(t *testing.T) {
		h := newHarness(t, writeOutput)
		_, _ = h.runner(pi.Config{}).Run(context.Background(), request(h.workspace, reviewer.ModeReview))
		args := h.args(t)
		i := slices.Index(args, "--session-id")
		if i < 0 || !strings.HasPrefix(args[i+1], "kibitz-") {
			t.Errorf("args = %v", args)
		}
		if j := slices.Index(args, "--session-dir"); j < 0 || args[j+1] != h.sessions {
			t.Errorf("args = %v", args)
		}
	})
	t.Run("a stored session is opened by its file", func(t *testing.T) {
		h := newHarness(t, writeOutput)
		older := filepath.Join(h.sessions, "2026-10-01T00-00-00-000Z_kibitz-abc.jsonl")
		newer := filepath.Join(h.sessions, "2026-10-02T00-00-00-000Z_kibitz-abc.jsonl")
		for _, f := range []string{older, newer} {
			if err := os.WriteFile(f, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		req := request(h.workspace, reviewer.ModeReview)
		req.SessionID = "kibitz-abc"
		_, _ = h.runner(pi.Config{}).Run(context.Background(), req)
		args := h.args(t)
		if i := slices.Index(args, "--session"); i < 0 || args[i+1] != newer {
			t.Errorf("args = %v, want --session %s", args, newer)
		}
	})
	t.Run("a session that is gone starts again", func(t *testing.T) {
		h := newHarness(t, writeOutput)
		req := request(h.workspace, reviewer.ModeReview)
		req.SessionID = "ses_from_opencode"
		_, _ = h.runner(pi.Config{}).Run(context.Background(), req)
		args := h.args(t)
		if slices.Contains(args, "--session") {
			t.Errorf("args = %v", args)
		}
		if i := slices.Index(args, "--session-id"); i < 0 || args[i+1] == "ses_from_opencode" {
			t.Errorf("args = %v, want a new id", args)
		}
	})
}

func TestTheAgentDefinitionIsAppended(t *testing.T) {
	h := newHarness(t, writeOutput+`
i=0; prev=""; for a in "$@"; do if [ "$prev" = "--append-system-prompt" ]; then cp "$a" '`+"RECORD"+`/system.md'; fi; prev="$a"; done`)
	// The script needs the record directory spelled out.
	script, _ := os.ReadFile(h.bin)
	_ = os.WriteFile(h.bin, []byte(strings.ReplaceAll(string(script), "RECORD", h.record)), 0o700) //nolint:gosec // a test fixture

	agents := t.TempDir()
	def := "---\ndescription: x\nmode: primary\n---\n\nあなたはコードレビュアーです。\n"
	if err := os.WriteFile(filepath.Join(agents, "kibitz-review.md"), []byte(def), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.runner(pi.Config{AgentsDir: agents}).Run(context.Background(), request(h.workspace, reviewer.ModeReview)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(h.record, "system.md"))
	if err != nil {
		t.Fatalf("the system prompt was not passed: %v", err)
	}
	if string(data) != "あなたはコードレビュアーです。" {
		t.Errorf("system prompt = %q, want the body without front matter", data)
	}
}

func TestCustomProviderTokenStaysOutOfFiles(t *testing.T) {
	h := newHarness(t, writeOutput)
	runner := h.runner(pi.Config{
		Model: "vertex-maas/zai-org/glm-5.2-maas",
		CustomProvider: &opencode.CustomProvider{
			ID:      "vertex-maas",
			BaseURL: "https://aiplatform.googleapis.com/v1beta1/projects/p/locations/global/endpoints/openapi",
			Token:   func(context.Context) (string, error) { return "ya29.secret", nil },
		},
	})
	if _, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data := h.file(t, "models.json")
	if strings.Contains(string(data), "ya29.secret") {
		t.Error("the token was written into models.json")
	}
	var models struct {
		Providers map[string]struct {
			API    string `json:"api"`
			APIKey string `json:"apiKey"`
			Models []struct {
				ID string `json:"id"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &models); err != nil {
		t.Fatal(err)
	}
	p := models.Providers["vertex-maas"]
	if p.API != "openai-completions" || p.APIKey != "${KIBITZ_PI_PROVIDER_TOKEN}" || len(p.Models) != 1 || p.Models[0].ID != "zai-org/glm-5.2-maas" {
		t.Errorf("provider = %+v", p)
	}
	if h.env(t)["KIBITZ_PI_PROVIDER_TOKEN"] != "ya29.secret" {
		t.Error("the token did not reach the agent")
	}
}

func anyStrings(values []any) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}
