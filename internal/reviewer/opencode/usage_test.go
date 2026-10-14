package opencode_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/reviewer/opencode"
)

// exportCLI is a fake CLI that answers `session export` with export and
// anything else with run.
func exportCLI(run, export string) string {
	return `
if [ "$1" = session ]; then
` + export + `
exit 0
fi
` + run
}

// The fixtures are a real `opencode run --format json` (2.0.28) that
// answered in one step, and the real `opencode session export` of the session
// it made. The stream has the step's start and its text and not its end,
// which is the event carrying the tokens: opencode 2 stops relaying events
// once the session goes idle (anomalyco/opencode#53207).
func TestTheLastStepsUsageIsReadFromTheSession(t *testing.T) {
	stream, err := os.ReadFile(filepath.Join("testdata", "opencode-2.0.28-final-step.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stream, []byte("step_finish")) {
		t.Fatal("the captured stream has a step_finish; it no longer shows what this test is about")
	}
	export := filepath.Join("testdata", "opencode-2.0.28-export.json")
	abs, err := filepath.Abs(export)
	if err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, exportCLI(replay(t, stream), "cat '"+abs+"'"))
	runner := opencode.New(opencode.Config{Bin: h.bin}, discardLogger())
	result, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Usage.InputTokens != 1 || result.Usage.OutputTokens != 1 {
		t.Errorf("Usage = %+v, want the one step the session recorded", result.Usage)
	}
	// The export was asked for the session the run was in, with a server of
	// its own like the run itself.
	if got := strings.Join(h.args(t), " "); got != "session export --standalone ses_ec64d4d5fffeEKLENfube4ILO0" {
		t.Errorf("last invocation = %q", got)
	}
}

// Only the steps the stream left unfinished are taken from the session. The
// ones it finished were counted already, and a continued session holds the
// steps of earlier runs, which were counted when they ran.
func TestOnlyTheUnfinishedStepsAreTakenFromTheSession(t *testing.T) {
	run := `
mkdir -p .kibitz/out
echo '{"schema_version": 1, "summary": "s", "comments": []}' > .kibitz/out/review.json
echo '{"type":"step_start","sessionID":"ses_9","part":{"type":"step-start","messageID":"msg_2"}}'
echo '{"type":"step_finish","sessionID":"ses_9","part":{"type":"step-finish","messageID":"msg_2","reason":"tool-calls","tokens":{"input":100,"output":10,"reasoning":0,"cache":{"read":0,"write":0}}}}'
echo '{"type":"step_start","sessionID":"ses_9","part":{"type":"step-start","messageID":"msg_3"}}'
echo '{"type":"text","sessionID":"ses_9","part":{"type":"text","messageID":"msg_3","text":"done"}}'
`
	export := `cat <<'JSON'
{"info": {"id": "ses_9"}, "messages": [
  {"id": "msg_1", "type": "assistant", "tokens": {"input": 7000, "output": 700, "reasoning": 0, "cache": {"read": 0, "write": 0}}},
  {"id": "msg_u", "type": "user", "text": "x"},
  {"id": "msg_2", "type": "assistant", "tokens": {"input": 100, "output": 10, "reasoning": 0, "cache": {"read": 0, "write": 0}}},
  {"id": "msg_3", "type": "assistant", "tokens": {"input": 40, "output": 4, "reasoning": 2, "cache": {"read": 500, "write": 6}}}
]}
JSON`
	h := newHarness(t, exportCLI(run, export))
	runner := opencode.New(opencode.Config{Bin: h.bin}, discardLogger())
	result, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := reviewer.Usage{InputTokens: 140, OutputTokens: 14, ReasoningTokens: 2, CacheReadTokens: 500, CacheWriteTokens: 6}
	got := result.Usage
	got.Duration = 0
	if got != want {
		t.Errorf("Usage = %+v, want %+v", got, want)
	}
}

// A run whose stream finished every step is not followed by an export.
func TestNoExportWhenEveryStepFinished(t *testing.T) {
	h := newHarness(t, exportCLI(writeOutput, "echo exported > .kibitz/exported"))
	runner := opencode.New(opencode.Config{Bin: h.bin}, discardLogger())
	if _, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.workspace, ".kibitz", "exported")); err == nil {
		t.Error("the session was exported although the stream finished every step")
	}
}

// A review that worked is not thrown away over its token count. The count
// is what the stream had, and the log says the rest is unknown.
func TestAFailedExportKeepsTheReview(t *testing.T) {
	run := `
mkdir -p .kibitz/out
echo '{"schema_version": 1, "summary": "s", "comments": []}' > .kibitz/out/review.json
echo '{"type":"step_start","sessionID":"ses_9","part":{"type":"step-start","messageID":"msg_3"}}'
`
	h := newHarness(t, exportCLI(run, "echo 'no such session' >&2; exit 1"))
	var logs bytes.Buffer
	runner := opencode.New(opencode.Config{Bin: h.bin}, slog.New(slog.NewJSONHandler(&logs, nil)))
	result, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary != "s" {
		t.Errorf("Summary = %q", result.Summary)
	}
	if !strings.Contains(logs.String(), "the usage of the steps the agent did not report is unknown") ||
		!strings.Contains(logs.String(), "no such session") {
		t.Errorf("the log does not say why the usage is short:\n%s", logs.String())
	}
}
