package opencode

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/reviewer"
)

// The fixtures under testdata/failures are what opencode 1.18.31 -- the
// version the worker image pins -- actually wrote to stdout and stderr when
// made to fail in each of these ways, captured by running it. They are not
// written by hand: the point of these tests is that kibitz reads what the
// real binary says, and a hand-written fixture would only test what kibitz
// expects it to say.

func fixture(t *testing.T, name string) (stdout, stderr []byte) {
	t.Helper()
	read := func(ext string) []byte {
		data, err := os.ReadFile(filepath.Join("testdata", "failures", name+"."+ext))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	return read("stdout"), read("stderr")
}

// exitError produces a real *exec.ExitError with the given status, because
// that is what the runner receives and what the status is read from.
func exitError(t *testing.T, code string) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+code).Run()
	if err == nil {
		t.Fatal("expected the command to fail")
	}
	return err
}

// This is the failure that was reported in production as
// "opencode: exit status 1:" -- the event on stdout says only "check the
// server logs", and the log is where the cause is.
func TestTheCauseIsReadFromTheLogByTheEventsRef(t *testing.T) {
	stdout, stderr := fixture(t, "badmodel")
	d := diagnose(stdout, stderr, exitError(t, "1"), false, 2*time.Second)

	if d.ErrorName != "UnknownError" || d.ErrorRef == "" {
		t.Fatalf("event = %q ref %q, want UnknownError with a ref", d.ErrorName, d.ErrorRef)
	}
	want := "ProviderModelNotFoundError: Model not found: nosuch/provider-model."
	if d.Cause != want {
		t.Errorf("Cause = %q, want %q", d.Cause, want)
	}

	summary := d.Summary()
	for _, part := range []string{want, "exit status 1", "ref " + d.ErrorRef} {
		if !strings.Contains(summary, part) {
			t.Errorf("Summary() = %q, missing %q", summary, part)
		}
	}
	// The generic message is what made the old error useless, and it must not
	// crowd out the real one.
	if strings.Contains(summary, "Check server logs") {
		t.Errorf("Summary() = %q repeats the message that says nothing", summary)
	}
}

// The case most likely in a real deployment: the model is named in a way the
// provider does not know. opencode's own suggestion reaches the summary.
func TestAWrongModelNameCarriesTheSuggestion(t *testing.T) {
	stdout, stderr := fixture(t, "nocreds")
	d := diagnose(stdout, stderr, exitError(t, "1"), false, time.Second)
	if !strings.Contains(d.Summary(), "Did you mean: claude-opus-4-5@20251101?") {
		t.Errorf("Summary() = %q", d.Summary())
	}
}

// An expired session is reported on stderr, for a person, and the runner
// retries without it only if it can recognize it. This must keep working.
func TestAnExpiredSessionIsStillRecognized(t *testing.T) {
	stdout, stderr := fixture(t, "nosession")
	err := &RunError{Diagnosis: diagnose(stdout, stderr, exitError(t, "1"), false, time.Second)}
	if !isMissingSession(err) {
		t.Errorf("isMissingSession(%q) = false; the retry without a session would never happen", err)
	}
	if strings.Contains(err.Error(), "\x1b[") {
		t.Errorf("error %q still carries terminal colour codes", err)
	}
}

// A missing agent definition is not a failure -- opencode runs the default
// agent instead -- and it is said only on stderr. It has to surface.
func TestAMissingAgentDefinitionIsNoticed(t *testing.T) {
	stdout, stderr := fixture(t, "noagent")
	d := diagnose(stdout, stderr, exitError(t, "1"), false, time.Second)

	found := false
	for _, notice := range d.Notices {
		if strings.Contains(notice, `agent "kibitz-missing" not found. Falling back to default agent`) {
			found = true
		}
	}
	if !found {
		t.Errorf("Notices = %q, want the fallback to be one of them", d.Notices)
	}
}

func TestATimeoutSaysSo(t *testing.T) {
	d := diagnose(nil, nil, exitError(t, "137"), true, 15*time.Minute)
	if got := d.Summary(); got != "timed out after 15m0s" {
		t.Errorf("Summary() = %q", got)
	}
}

func TestAnAgentThatSaidNothingSaysThat(t *testing.T) {
	d := diagnose(nil, nil, exitError(t, "1"), false, time.Second)
	if got := d.Summary(); got != "the agent exited without saying why (exit status 1)" {
		t.Errorf("Summary() = %q", got)
	}
}

// The event stream is counted and never copied: it carries the code the agent
// read and what it wrote about it, which belong to the repository and not in
// the worker's log.
func TestTheEventStreamIsCountedNotKept(t *testing.T) {
	stdout := []byte(`{"type":"text","part":{"type":"text","text":"func secret() { return \"hunter2\" }"}}
{"type":"step_finish","part":{"type":"step-finish"}}
`)
	d := diagnose(stdout, nil, nil, false, time.Second)
	if d.Events["text"] != 1 || d.Events["step_finish"] != 1 {
		t.Errorf("Events = %v", d.Events)
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).LogAttrs(context.Background(), slog.LevelError, "x", d.Attrs()...)
	if strings.Contains(buf.String(), "hunter2") {
		t.Errorf("the log carries the agent's text: %s", buf.String())
	}
}

func TestParseLogLine(t *testing.T) {
	fields, ok := parseLogLine(`timestamp=2026-09-27T08:23:28Z level=ERROR message=failed ref=err_1 error="a \"quoted\" word" cause="line one\nline two"`)
	if !ok {
		t.Fatal("not recognized as a log line")
	}
	for key, want := range map[string]string{
		"level": "ERROR", "message": "failed", "ref": "err_1",
		"error": `a "quoted" word`, "cause": "line one\nline two",
	} {
		if fields[key] != want {
			t.Errorf("%s = %q, want %q", key, fields[key], want)
		}
	}
	if _, ok := parseLogLine("Error: Session not found"); ok {
		t.Error("a line printed for a person was read as a log line")
	}
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	b := &tailBuffer{max: 8}
	for _, chunk := range []string{"0123", "4567", "89ab"} {
		if _, err := b.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if got := string(b.Bytes()); got != "456789ab" {
		t.Errorf("Bytes() = %q, want the last 8", got)
	}
}

// A process that could not be started is a different failure from one that
// ran and exited, and says so.
func TestAProcessThatNeverRanSaysWhy(t *testing.T) {
	d := diagnose(nil, nil, errors.New(`exec: "opencode": executable file not found in $PATH`), false, 0)
	if !strings.Contains(d.Summary(), "executable file not found") {
		t.Errorf("Summary() = %q", d.Summary())
	}
}

// Through the runner, the way the worker calls it: a CLI that fails the way
// the real one does must come back as an error that names the cause, and as
// one log record that carries everything else.
func TestTheRunnerReportsTheCauseAndLogsTheRest(t *testing.T) {
	stdout, stderr := fixture(t, "badmodel")
	dir := t.TempDir()
	stdoutPath := filepath.Join(dir, "stdout")
	stderrPath := filepath.Join(dir, "stderr")
	if err := os.WriteFile(stdoutPath, stdout, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderrPath, stderr, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "opencode")
	script := "#!/bin/sh\necho \"$@\" > " + filepath.Join(dir, "args") +
		"\ncat " + stdoutPath + "\ncat " + stderrPath + " >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	runner := New(Config{Bin: bin, Model: "nosuch/provider-model"}, logger)

	req := testRequest(t.TempDir())
	_, err := runner.Run(context.Background(), req)

	var runErr *RunError
	if !errors.As(err, &runErr) {
		t.Fatalf("error = %v (%T), want a *RunError", err, err)
	}
	if !strings.Contains(err.Error(), "ProviderModelNotFoundError: Model not found: nosuch/provider-model.") {
		t.Errorf("error = %q, want the cause", err)
	}

	record := logs.String()
	for _, want := range []string{
		`"msg":"the agent failed"`,
		`"event_id":"github:e1"`,
		`"cause":"ProviderModelNotFoundError: Model not found: nosuch/provider-model."`,
		`"error_ref":"err_`,
		`"exit_code":1`,
	} {
		if !strings.Contains(record, want) {
			t.Errorf("the log does not contain %s:\n%s", want, record)
		}
	}

	args, err := os.ReadFile(filepath.Join(dir, "args")) //nolint:gosec // a path this test built
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--print-logs --log-level WARN") {
		t.Errorf("args = %q; without --print-logs the cause never reaches stderr", args)
	}
}

func testRequest(workspace string) reviewer.Request {
	return reviewer.Request{
		Mode:         reviewer.ModeReview,
		WorkspaceDir: workspace,
		Event: &event.ReviewEvent{
			ID:         "github:e1",
			Repository: event.Repository{FullName: "yteraoka/kibitz"},
		},
		PullRequest: &event.PullRequest{Number: 42, Title: "Add the SQS subscriber"},
		Diff: &forge.Diff{Files: []forge.File{
			{Path: "queue.go", Status: forge.FileModified, Patch: "@@ -1,2 +1,3 @@\n+x"},
		}},
		HeadSHA: "abc1234",
	}
}

// Gemini ended a turn with a reason opencode does not know, opencode asked
// again with the model's turn last, and Vertex AI refused. Captured from the
// pinned binary against a server answering as Gemini does, with finishReason
// OTHER; the log lines are the ones production wrote.
func TestAModelTurnThatBrokeOffIsRecognized(t *testing.T) {
	stdout, stderr := fixture(t, "modelturn")
	d := diagnose(stdout, stderr, exitError(t, "1"), false, time.Second)

	if !d.EndedOnModelTurn() {
		t.Fatalf("EndedOnModelTurn() = false for %q / %q", d.ErrorMessage, d.Cause)
	}
	if d.Finish != "unknown" {
		t.Errorf("Finish = %q, want unknown", d.Finish)
	}
	if !strings.HasPrefix(d.SessionID, "ses_") {
		t.Errorf("SessionID = %q, want the session the run was in", d.SessionID)
	}
}

func TestOtherFailuresAreNotTakenForABrokenTurn(t *testing.T) {
	for _, name := range []string{"badmodel", "nocreds", "nosession", "noagent"} {
		stdout, stderr := fixture(t, name)
		if d := diagnose(stdout, stderr, exitError(t, "1"), false, time.Second); d.EndedOnModelTurn() {
			t.Errorf("%s was taken for a broken turn", name)
		}
	}
}
