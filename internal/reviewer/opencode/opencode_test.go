package opencode_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/jobcontext"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/reviewer/opencode"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// fakeCLI writes a shell script that stands in for the opencode binary. It
// records its arguments and environment, then produces whatever the test asked
// for. The real binary is exercised separately; what matters here is that
// kibitz invokes it correctly and reads its output correctly.
// The paths it records to are written into the script rather than passed in
// the environment: the runner builds the agent's environment from a fixed
// list, so a variable the test invented would not reach it — which is the
// point of that list.
func fakeCLI(t *testing.T, script, argsFile, configFile string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake CLI is a shell script")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "opencode")
	body := "#!/bin/sh\nset -e\n" +
		"printf '%s\\n' \"$@\" > '" + argsFile + "'\n" +
		"printenv OPENCODE_CONFIG > '" + configFile + "'\n" +
		"printenv > '" + configFile + ".env'\n" +
		// The runner deletes the job directory when it returns, so anything a
		// test wants to look at afterwards is copied out here.
		"cp \"$(dirname \"$OPENCODE_CONFIG\")/context.json\" '" + configFile + ".context' 2>/dev/null || true\n" +
		script
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil { //nolint:gosec // a test fixture
		t.Fatalf("writing the fake CLI: %v", err)
	}
	return path
}

type harness struct {
	bin        string
	workspace  string
	argsFile   string
	configFile string
}

func newHarness(t *testing.T, script string) harness {
	t.Helper()

	record := t.TempDir()
	h := harness{
		workspace:  t.TempDir(),
		argsFile:   filepath.Join(record, "args"),
		configFile: filepath.Join(record, "config-path"),
	}
	h.bin = fakeCLI(t, script, h.argsFile, h.configFile)
	return h
}

// contextCopy is where the fake CLI put the job context, which the runner
// deletes along with the job directory when it returns.
func (h harness) contextCopy() string { return h.configFile + ".context" }

// env reads the environment the fake CLI was started with.
func (h harness) env(t *testing.T) map[string]string {
	t.Helper()

	data, err := os.ReadFile(h.configFile + ".env")
	if err != nil {
		t.Fatalf("reading the recorded environment: %v", err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if ok {
			env[name] = value
		}
	}
	return env
}

func (h harness) args(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(h.argsFile)
	if err != nil {
		t.Fatalf("reading recorded arguments: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// config reads the per-job configuration the runner generated. The runner
// deletes it when the job ends, so the fake CLI copies it aside first.
func (h harness) config(t *testing.T) map[string]any {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(h.workspace, ".kibitz", "config-copy.json"))
	if err != nil {
		t.Fatalf("reading the generated config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decoding the generated config: %v", err)
	}
	return cfg
}

func request(workspace string, mode reviewer.Mode) reviewer.Request {
	return reviewer.Request{
		Mode:         mode,
		WorkspaceDir: workspace,
		Event: &event.ReviewEvent{
			Repository: event.Repository{FullName: "yteraoka/kibitz"},
		},
		PullRequest: &event.PullRequest{Number: 42, Title: "Add the SQS subscriber"},
		Diff: &forge.Diff{Files: []forge.File{
			{Path: "queue.go", Status: forge.FileModified, Patch: "@@ -1,2 +1,3 @@\n+x"},
		}},
		HeadSHA: "abc1234",
	}
}

// writeOutput is the part of the fake CLI that plays a well-behaved agent.
const writeOutput = `
cp "$OPENCODE_CONFIG" .kibitz/config-copy.json
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
echo '{"type":"step_start","timestamp":1,"sessionID":"ses_123","part":{"id":"prt_1","sessionID":"ses_123","messageID":"msg_1","type":"step-start"}}'
echo '{"type":"tool_use","timestamp":2,"sessionID":"ses_123","part":{"id":"prt_2","type":"tool","tool":"read","state":{"status":"completed"}}}'
echo '{"type":"step_finish","timestamp":3,"sessionID":"ses_123","part":{"id":"prt_3","type":"step-finish","reason":"tool-calls","cost":0.021,"tokens":{"input":1200,"output":340,"reasoning":20,"cache":{"read":9000,"write":500}}}}'
echo '{"type":"step_finish","timestamp":4,"sessionID":"ses_123","part":{"id":"prt_4","type":"step-finish","reason":"stop","cost":0.004,"tokens":{"input":300,"output":60,"reasoning":0,"cache":{"read":1000,"write":0}}}}'
echo '{"type":"text","timestamp":5,"sessionID":"ses_123","part":{"id":"prt_5","type":"text","text":"done"}}'
`

func TestRunReview(t *testing.T) {
	h := newHarness(t, writeOutput)

	runner := opencode.New(opencode.Config{
		Bin:   h.bin,
		Model: "google-vertex-anthropic/claude-opus-5",
	}, discardLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := runner.Run(ctx, request(h.workspace, reviewer.ModeReview))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(result.Findings) != 1 || result.Findings[0].Path != "queue.go" {
		t.Fatalf("findings = %+v", result.Findings)
	}
	if result.Summary == "" {
		t.Error("summary is empty")
	}
	if result.SessionID != "ses_123" {
		t.Errorf("SessionID = %q, want the one the agent reported", result.SessionID)
	}
	// Every step is billed, so every step counts.
	want := reviewer.Usage{
		InputTokens:      1500,
		CacheReadTokens:  10000,
		CacheWriteTokens: 500,
		OutputTokens:     400,
		ReasoningTokens:  20,
	}
	got := result.Usage
	got.Duration = 0
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
	if result.Usage.Duration <= 0 {
		t.Error("duration was not recorded")
	}
}

func TestRunArguments(t *testing.T) {
	h := newHarness(t, writeOutput)
	runner := opencode.New(opencode.Config{Bin: h.bin, Model: "vertex/claude-opus-5"}, discardLogger())

	req := request(h.workspace, reviewer.ModeReview)
	req.SessionID = "ses_existing"
	if _, err := runner.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	args := strings.Join(h.args(t), " ")
	for _, want := range []string{
		"run",
		"--format json",
		"--agent kibitz-review",
		"--model vertex/claude-opus-5",
		// Headless runs must never be able to stop for an approval prompt.
		"--auto",
		// Continuing the session is what lets a follow-up refer to a finding.
		"--session ses_existing",
		"--file .kibitz/prompt.md",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("arguments do not contain %q:\n%s", want, args)
		}
	}
}

func TestGeneratedPermissions(t *testing.T) {
	h := newHarness(t, writeOutput)
	runner := opencode.New(opencode.Config{Bin: h.bin}, discardLogger())

	if _, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	cfg := h.config(t)
	permission, ok := cfg["permission"].(map[string]any)
	if !ok {
		t.Fatalf("no permission block: %v", cfg)
	}

	// Deny by default, read to review, and write only where the contract says.
	if permission["*"] != "deny" {
		t.Errorf("default permission = %v, want deny", permission["*"])
	}
	if permission["read"] != "allow" {
		t.Errorf("read = %v, want allow", permission["read"])
	}
	if permission["webfetch"] != "deny" {
		t.Errorf("webfetch = %v, want deny", permission["webfetch"])
	}

	edit, ok := permission["edit"].(map[string]any)
	if !ok {
		t.Fatalf("edit permission = %v, want a pattern map", permission["edit"])
	}
	if edit["*"] != "deny" {
		t.Errorf("edit default = %v, want deny: a reviewer must not change the code", edit["*"])
	}
	if edit[".kibitz/out/*"] != "allow" {
		t.Errorf("the output path is not writable: %v", edit)
	}

	bash, ok := permission["bash"].(map[string]any)
	if !ok || bash["*"] != "deny" {
		t.Errorf("bash = %v, want deny by default", permission["bash"])
	}
}

func TestAnswerModeIsReadOnly(t *testing.T) {
	h := newHarness(t, `
cp "$OPENCODE_CONFIG" .kibitz/config-copy.json
echo '{"type":"text","text":"ロックの期限が切れるためです。"}'
`)
	runner := opencode.New(opencode.Config{Bin: h.bin}, discardLogger())

	req := request(h.workspace, reviewer.ModeAnswer)
	req.Question = "なぜ競合するのですか?"

	result, err := runner.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(result.Reply, "ロック") {
		t.Errorf("Reply = %q, want the agent's answer", result.Reply)
	}

	cfg := h.config(t)
	permission := cfg["permission"].(map[string]any)
	if permission["edit"] != "deny" {
		t.Errorf("edit = %v, want deny: answering a question needs no writes", permission["edit"])
	}

	args := strings.Join(h.args(t), " ")
	if !strings.Contains(args, "--agent kibitz-answer") {
		t.Errorf("the answer agent was not selected: %s", args)
	}
}

// Only the servers the request named, and only those. A server the deployment
// merely offers costs tokens on every call for tool definitions nobody asked
// for.
func TestMCPServersAreWrittenIntoTheConfig(t *testing.T) {
	h := newHarness(t, writeOutput)
	runner := opencode.New(opencode.Config{
		Bin: h.bin,
		MCPServers: opencode.Catalog{
			"jira":   {Type: opencode.MCPRemote, URL: "https://jira.example.com/mcp"},
			"sentry": {Type: opencode.MCPLocal, Command: []string{"sentry-mcp"}},
		},
	}, discardLogger())

	req := request(h.workspace, reviewer.ModeReview)
	req.MCP = []string{"jira"}
	if _, err := runner.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mcp, ok := h.config(t)["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("no mcp block")
	}
	if len(mcp) != 1 {
		t.Fatalf("%d mcp servers, want only the one that was asked for: %v", len(mcp), mcp)
	}
	jira, ok := mcp["jira"].(map[string]any)
	if !ok {
		t.Fatalf("jira is missing: %v", mcp)
	}
	if jira["url"] != "https://jira.example.com/mcp" {
		t.Errorf("jira = %v", jira)
	}
	// Written as enabled: being in this file at all is the decision.
	if jira["enabled"] != true {
		t.Errorf("jira was written but not enabled: %v", jira)
	}
}

// A repository that asked for nothing gets nothing, and the block is left out
// rather than written empty.
func TestNoMCPServersWithoutARequest(t *testing.T) {
	h := newHarness(t, writeOutput)
	runner := opencode.New(opencode.Config{
		Bin:        h.bin,
		MCPServers: opencode.Catalog{"jira": {Type: opencode.MCPRemote, URL: "https://jira.example.com/mcp"}},
	}, discardLogger())

	if _, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := h.config(t)["mcp"]; ok {
		t.Error("a server was enabled although the repository asked for none")
	}
}

// Triage reads a list of file names to decide what is worth reading. Loading
// Jira's tool definitions into that pass would spend what the pass exists to
// save.
func TestTriageGetsNoMCPServers(t *testing.T) {
	h := newHarness(t, `
cp "$OPENCODE_CONFIG" .kibitz/config-copy.json
mkdir -p .kibitz/out
echo '{"schema_version":1,"paths":["queue.go"]}' > .kibitz/out/triage.json
`)
	runner := opencode.New(opencode.Config{
		Bin:        h.bin,
		MCPServers: opencode.Catalog{"jira": {Type: opencode.MCPRemote, URL: "https://jira.example.com/mcp"}},
	}, discardLogger())

	req := request(h.workspace, reviewer.ModeTriage)
	req.MCP = []string{"jira"}
	if _, err := runner.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := h.config(t)["mcp"]; ok {
		t.Error("triage was given an MCP server")
	}
}

// The agent's process is built from a list rather than inherited. The worker's
// environment holds the webhook secrets and the GitHub App's private key, and
// an MCP server is a binary kibitz did not write, started by opencode, with
// opencode's environment.
func TestTheAgentDoesNotInheritTheWorkersSecrets(t *testing.T) {
	t.Setenv("KIBITZ_WEBHOOK_SECRETS", "s3cr3t")
	t.Setenv("KIBITZ_GITHUB_PRIVATE_KEY", "-----BEGIN PRIVATE KEY-----")
	t.Setenv("JIRA_TOKEN", "jira-token")
	t.Setenv("SOMETHING_ELSE", "not asked for")

	h := newHarness(t, writeOutput)
	runner := opencode.New(opencode.Config{
		Bin: h.bin,
		MCPServers: opencode.Catalog{"jira": {
			Type:    opencode.MCPRemote,
			URL:     "https://jira.example.com/mcp",
			Headers: map[string]string{"Authorization": "Bearer {env:JIRA_TOKEN}"},
		}},
		Env: []string{"GOOGLE_CLOUD_PROJECT=kibitz-prod"},
	}, discardLogger())

	req := request(h.workspace, reviewer.ModeReview)
	req.MCP = []string{"jira"}
	if _, err := runner.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	env := h.env(t)
	for _, name := range []string{"KIBITZ_WEBHOOK_SECRETS", "KIBITZ_GITHUB_PRIVATE_KEY", "SOMETHING_ELSE"} {
		if _, leaked := env[name]; leaked {
			t.Errorf("%s reached the agent", name)
		}
	}
	// What the enabled server refers to does reach it: that is how the
	// credential gets there without being written into the config file.
	if env["JIRA_TOKEN"] != "jira-token" {
		t.Errorf("JIRA_TOKEN = %q, want the value the server refers to", env["JIRA_TOKEN"])
	}
	if env["GOOGLE_CLOUD_PROJECT"] != "kibitz-prod" {
		t.Errorf("the configured provider credentials did not reach the agent: %q", env["GOOGLE_CLOUD_PROJECT"])
	}
	if env["PATH"] == "" {
		t.Error("PATH did not reach the agent")
	}
	// And the secret is not in the file, which is the other half.
	data, err := os.ReadFile(filepath.Join(h.workspace, ".kibitz", "config-copy.json"))
	if err != nil {
		t.Fatalf("reading the generated config: %v", err)
	}
	if strings.Contains(string(data), "jira-token") {
		t.Error("the credential was written into the config file")
	}
	if !strings.Contains(string(data), "{env:JIRA_TOKEN}") {
		t.Errorf("the placeholder was not kept:\n%s", data)
	}
}

// A variable a server does not refer to is not forwarded just because it is
// set, but a deployment can name one.
func TestEnvPassthrough(t *testing.T) {
	t.Setenv("KIBITZ_EXTRA", "extra")

	h := newHarness(t, writeOutput)
	runner := opencode.New(opencode.Config{Bin: h.bin, EnvPassthrough: []string{"KIBITZ_EXTRA"}}, discardLogger())
	if _, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if h.env(t)["KIBITZ_EXTRA"] != "extra" {
		t.Error("a variable the deployment named did not reach the agent")
	}
}

func TestMissingOutputIsReported(t *testing.T) {
	h := newHarness(t, `echo '{"type":"message_update"}'`)
	runner := opencode.New(opencode.Config{Bin: h.bin}, discardLogger())

	_, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview))
	if err == nil {
		t.Fatal("Run succeeded although the agent wrote no output")
	}
	if !strings.Contains(err.Error(), reviewer.OutputPath) {
		t.Errorf("error %q does not name the expected output file", err)
	}
}

// Output that breaks the contract is reported as its own error type, because
// the job retries once with the message fed back to the agent.
func TestMalformedOutputIsTyped(t *testing.T) {
	h := newHarness(t, `
mkdir -p .kibitz/out
echo '{"schema_version": 1, "summary": ""}' > .kibitz/out/review.json
`)
	runner := opencode.New(opencode.Config{Bin: h.bin}, discardLogger())

	_, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview))
	var outErr *reviewer.OutputError
	if !errors.As(err, &outErr) {
		t.Fatalf("err = %v, want an OutputError", err)
	}
	if !strings.Contains(err.Error(), "summary") {
		t.Errorf("error %q does not say what was wrong", err)
	}
}

func TestFailureIncludesStderr(t *testing.T) {
	h := newHarness(t, `echo "no provider configured" >&2; exit 3`)
	runner := opencode.New(opencode.Config{Bin: h.bin}, discardLogger())

	_, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview))
	if err == nil {
		t.Fatal("Run succeeded although the CLI failed")
	}
	if !strings.Contains(err.Error(), "no provider configured") {
		t.Errorf("error %q does not carry the CLI's own message", err)
	}
}

func TestTimeout(t *testing.T) {
	h := newHarness(t, `sleep 5`)
	runner := opencode.New(opencode.Config{Bin: h.bin}, discardLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := runner.Run(ctx, request(h.workspace, reviewer.ModeReview)); err == nil {
		t.Fatal("Run succeeded although it should have timed out")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("the run took %s; the timeout did not cut it off", elapsed)
	}
}

func TestRunRejectsEmptyWorkspace(t *testing.T) {
	runner := opencode.New(opencode.Config{Bin: "/bin/true"}, discardLogger())
	if _, err := runner.Run(context.Background(), reviewer.Request{}); err == nil {
		t.Error("Run accepted a request with no workspace")
	}
}

// opencode declares --file as an array, so every argument after it is read as
// another path to attach. With the message last, opencode looked for a file
// named "指示は添付された …" and every run failed with "File not found".
// The message goes first, and --file is the final flag.
func TestPromptFileIsTheLastArgument(t *testing.T) {
	h := newHarness(t, writeOutput)
	runner := opencode.New(opencode.Config{Bin: h.bin, Model: "vertex/claude-opus-5"}, discardLogger())

	req := request(h.workspace, reviewer.ModeReview)
	req.SessionID = "ses_existing"
	if _, err := runner.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	args := h.args(t)
	if len(args) < 2 {
		t.Fatalf("arguments = %v", args)
	}

	prompt := filepath.Join(".kibitz", "prompt.md")
	if got := args[len(args)-1]; got != prompt {
		t.Errorf("last argument = %q, want %q", got, prompt)
	}
	if got := args[len(args)-2]; got != "--file" {
		t.Errorf("second to last argument = %q, want --file", got)
	}

	// And the message is a message, not something --file could swallow.
	message := args[len(args)-3]
	if strings.HasPrefix(message, "-") {
		t.Fatalf("argument before --file = %q, want the message", message)
	}
	if !strings.Contains(message, prompt) {
		t.Errorf("message = %q, want it to point at %s", message, prompt)
	}
}

// A fork's branch is written by somebody without commit access, and the agent
// reads it. A server holding a credential is not reachable from there unless
// the operator said so.
func TestForkPullRequestsGetNoMCPServersByDefault(t *testing.T) {
	h := newHarness(t, writeOutput)
	runner := opencode.New(opencode.Config{
		Bin: h.bin,
		MCPServers: opencode.Catalog{
			"jira":   {Type: opencode.MCPRemote, URL: "https://jira.example.com/mcp"},
			"public": {Type: opencode.MCPRemote, URL: "https://public.example.com/mcp", AllowFork: true},
		},
	}, discardLogger())

	req := request(h.workspace, reviewer.ModeReview)
	req.PullRequest.IsFork = true
	req.MCP = []string{"jira", "public"}
	if _, err := runner.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mcp, ok := h.config(t)["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("no mcp block, want the one marked safe for forks")
	}
	if _, leaked := mcp["jira"]; leaked {
		t.Error("a fork pull request reached a server holding a credential")
	}
	server, ok := mcp["public"].(map[string]any)
	if !ok {
		t.Fatalf("the server marked safe for forks was dropped: %v", mcp)
	}
	// allow_fork is kibitz's own bookkeeping, not part of opencode's schema.
	if _, written := server["allow_fork"]; written {
		t.Errorf("allow_fork was written into the config: %v", server)
	}
}

// kibitz's own server is registered for every review, without a repository
// asking: it holds no credential and answers about the pull request the agent
// is already looking at.
func TestContextServerIsAlwaysRegistered(t *testing.T) {
	h := newHarness(t, writeOutput)
	runner := opencode.New(opencode.Config{Bin: h.bin, ContextBin: "/usr/local/bin/kibitz-mcp"}, discardLogger())

	req := request(h.workspace, reviewer.ModeReview)
	// The prompt carried one file; the pull request has two.
	req.FullDiff = &forge.Diff{Files: []forge.File{
		req.Diff.Files[0],
		{Path: "docs/skipped.md", Status: forge.FileModified, Patch: "@@ -1 +1,2 @@\n+y"},
	}}
	if _, err := runner.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mcp, ok := h.config(t)["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("no mcp block")
	}
	server, ok := mcp[opencode.ContextServerName].(map[string]any)
	if !ok {
		t.Fatalf("kibitz's own server is missing: %v", mcp)
	}
	command := server["command"].([]any)
	if command[0] != "/usr/local/bin/kibitz-mcp" || command[1] != "--context" {
		t.Errorf("command = %v", command)
	}

	// The file it points at holds the whole pull request, not the part the
	// prompt carried.
	job, err := jobcontext.Load(h.contextCopy())
	if err != nil {
		t.Fatalf("loading the context the runner wrote: %v", err)
	}
	if len(job.Files) != 2 {
		t.Errorf("%d files in the context, want the whole pull request", len(job.Files))
	}
	if !job.WasReviewed("queue.go") || job.WasReviewed("docs/skipped.md") {
		t.Errorf("Reviewed = %v", job.Reviewed)
	}
}

// Triage reads file names to decide what is worth reading. Handing it a tool
// that returns diffs would spend what the pass exists to save.
func TestTriageGetsNoContextServer(t *testing.T) {
	h := newHarness(t, `
cp "$OPENCODE_CONFIG" .kibitz/config-copy.json
mkdir -p .kibitz/out
echo '{"schema_version":1,"paths":["queue.go"]}' > .kibitz/out/triage.json
`)
	runner := opencode.New(opencode.Config{Bin: h.bin, ContextBin: "kibitz-mcp"}, discardLogger())

	if _, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeTriage)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := h.config(t)["mcp"]; ok {
		t.Error("triage was given a tool server")
	}
}

// A deployment whose image does not carry the binary leaves it out.
func TestNoContextServerWithoutABinary(t *testing.T) {
	h := newHarness(t, writeOutput)
	runner := opencode.New(opencode.Config{Bin: h.bin}, discardLogger())

	if _, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := h.config(t)["mcp"]; ok {
		t.Error("a server was registered although no binary is configured")
	}
}
