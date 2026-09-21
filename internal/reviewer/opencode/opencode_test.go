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
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/reviewer/opencode"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// fakeCLI writes a shell script that stands in for the opencode binary. It
// records its arguments and environment, then produces whatever the test asked
// for. The real binary is exercised separately; what matters here is that
// kibitz invokes it correctly and reads its output correctly.
func fakeCLI(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake CLI is a shell script")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "opencode")
	body := "#!/bin/sh\nset -e\n" +
		"printf '%s\\n' \"$@\" > \"$KIBITZ_TEST_ARGS\"\n" +
		"printenv OPENCODE_CONFIG > \"$KIBITZ_TEST_CONFIG_PATH\"\n" +
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

	dir := t.TempDir()
	h := harness{
		bin:        fakeCLI(t, script),
		workspace:  dir,
		argsFile:   filepath.Join(t.TempDir(), "args"),
		configFile: filepath.Join(t.TempDir(), "config-path"),
	}
	t.Setenv("KIBITZ_TEST_ARGS", h.argsFile)
	t.Setenv("KIBITZ_TEST_CONFIG_PATH", h.configFile)
	return h
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
echo '{"type":"session.created","sessionID":"ses_123"}'
echo '{"type":"message_update","usage":{"input":1200,"output":340}}'
echo '{"type":"tool_execution_start","toolName":"read"}'
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
	if result.Usage.InputTokens != 1200 || result.Usage.OutputTokens != 340 {
		t.Errorf("usage = %+v", result.Usage)
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

func TestMCPServersAreWrittenIntoTheConfig(t *testing.T) {
	h := newHarness(t, writeOutput)
	runner := opencode.New(opencode.Config{
		Bin: h.bin,
		MCPServers: map[string]opencode.MCPServer{
			"kibitz-context": {Type: "local", Command: []string{"kibitz-mcp"}, Enabled: true},
			"jira":           {Type: "remote", URL: "https://jira.example.com/mcp", Enabled: true},
		},
	}, discardLogger())

	if _, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mcp, ok := h.config(t)["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("no mcp block")
	}
	if len(mcp) != 2 {
		t.Errorf("%d mcp servers, want 2", len(mcp))
	}
	jira := mcp["jira"].(map[string]any)
	if jira["url"] != "https://jira.example.com/mcp" {
		t.Errorf("jira = %v", jira)
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
