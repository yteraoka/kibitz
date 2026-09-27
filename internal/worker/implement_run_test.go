package worker_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/sandbox"
	"github.com/yteraoka/kibitz/internal/worker"
	"github.com/yteraoka/kibitz/internal/workspace"
)

// The tests that matter here are the ones about what does NOT happen. Implement
// mode's whole value is that a change it wrote reaches a person only after it
// has been checked, so every way of not passing that check has a test saying no
// pull request was opened.

// editingEngine is an agent that actually writes to the workspace, which is
// what implement mode's decisions are made from: it asks git what changed
// rather than believing a report.
type editingEngine struct {
	// writes are the files it creates, keyed by path relative to the checkout.
	writes map[string]string
	// removes are paths it deletes.
	removes []string
	reply   string
	err     error

	requests []reviewer.Request
}

func (e *editingEngine) Run(_ context.Context, req reviewer.Request) (*reviewer.Result, error) {
	e.requests = append(e.requests, req)
	if e.err != nil {
		return nil, e.err
	}

	// The scaffolding the real engine leaves in the checkout: the prompt kibitz
	// wrote, and the output directory it creates for every mode. Neither is a
	// change the agent made, and both have to be gone before git is asked what
	// changed.
	if err := os.MkdirAll(filepath.Join(req.WorkspaceDir, reviewer.ScratchDir, "out"), 0o750); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(req.WorkspaceDir, reviewer.ScratchDir, "prompt.md"),
		[]byte(reviewer.BuildPrompt(req)), 0o600); err != nil {
		return nil, err
	}

	for path, body := range e.writes {
		full := filepath.Join(req.WorkspaceDir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			return nil, err
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			return nil, err
		}
	}
	for _, path := range e.removes {
		if err := os.Remove(filepath.Join(req.WorkspaceDir, path)); err != nil {
			return nil, err
		}
	}
	return &reviewer.Result{Reply: e.reply}, nil
}

// fakeSandbox answers without running anything.
type fakeSandbox struct {
	result *sandbox.Result
	err    error
	// trees records what each tree contained at the moment it was verified.
	// The listing is taken here rather than read afterwards because the
	// workspace is removed as soon as the job ends, which is the point of a
	// workspace.
	trees []map[string]bool
	// requests records what the verification was asked to run.
	requests []*sandbox.Request
}

func (s *fakeSandbox) Verify(_ context.Context, dir string, req *sandbox.Request) (*sandbox.Result, error) {
	s.trees = append(s.trees, listTree(dir))
	s.requests = append(s.requests, req)
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func (s *fakeSandbox) verifications() int { return len(s.trees) }

// listTree records the paths in a checkout, relative to it, with git's own
// directory left out.
func listTree(dir string) map[string]bool {
	paths := map[string]bool{}
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if rel == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		paths[rel] = true
		return nil
	})
	return paths
}

func passing() *sandbox.Result {
	return &sandbox.Result{Steps: []sandbox.Step{{Command: []string{"go", "test", "./..."}, ExitCode: 0}}}
}

func failing() *sandbox.Result {
	return &sandbox.Result{Steps: []sandbox.Step{{
		Command:  []string{"go", "test", "./..."},
		ExitCode: 1,
		Output:   "--- FAIL: TestReceive (0.00s)\n    queue_test.go:9: 受信できていません\nFAIL",
	}}}
}

// implementRun wires a job that can go all the way through: a real origin to
// push to, an engine that edits, and a sandbox that answers.
func implementRun(t *testing.T, e reviewer.Engine, box *fakeSandbox) (*worker.ReviewJob, *fakeForge, *event.ReviewEvent, string) {
	t.Helper()

	origin, _, _ := originRepo(t)
	f := implementSettings(t, workingSettings)
	j := &worker.ReviewJob{
		Forges:           map[event.Platform]forge.Client{event.PlatformGitHub: f},
		Engine:           e,
		Workspace:        workspace.Config{Root: t.TempDir(), Depth: 1},
		Limits:           reviewer.Limits{MaxComments: 10},
		Logger:           discardLogger(),
		Language:         "日本語",
		ImplementEnabled: true,
		Sandbox:          box,
	}
	return j, f, issueCommandIn("implement", "alice", origin), origin
}

func TestAFailingVerificationOpensNoPullRequest(t *testing.T) {
	engine := &editingEngine{
		writes: map[string]string{"internal/queue/sqs.go": "package sqs\n"},
		reply:  "SQS の subscriber を追加しました。",
	}
	box := &fakeSandbox{result: failing()}
	j, f, ev, origin := implementRun(t, engine, box)

	if err := j.Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// The one thing this whole arrangement exists for.
	if len(f.opened) != 0 {
		t.Fatalf("a pull request was opened for a change that did not pass: %+v", f.opened)
	}
	if branches := remoteBranches(t, origin); strings.Contains(branches, "kibitz/") {
		t.Errorf("a branch was pushed for a change that did not pass:\n%s", branches)
	}

	// And the failure has to be readable, or the comment is no use.
	comment := said(f)
	for _, want := range []string{"検証に失敗", "go test ./...", "受信できていません"} {
		if !strings.Contains(comment, want) {
			t.Errorf("the comment does not contain %q:\n%s", want, comment)
		}
	}
	if !strings.Contains(comment, "SQS の subscriber") {
		t.Errorf("the agent's own account was thrown away:\n%s", comment)
	}
}

func TestAPassingVerificationOpensADraftPullRequest(t *testing.T) {
	engine := &editingEngine{
		writes: map[string]string{"internal/queue/sqs.go": "package sqs\n"},
		reply:  "SQS の subscriber を追加しました。テストは 1 件追加。",
	}
	box := &fakeSandbox{result: passing()}
	j, f, ev, origin := implementRun(t, engine, box)

	if err := j.Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(f.opened) != 1 {
		t.Fatalf("want one pull request, got %d", len(f.opened))
	}
	opened := f.opened[0]
	if !opened.Draft {
		t.Error("the pull request is not a draft; a change nobody has read should not be ready to merge")
	}
	if opened.Head != "kibitz/issue-12" {
		t.Errorf("Head = %q, want kibitz/issue-12", opened.Head)
	}
	if opened.Base != "main" {
		t.Errorf("Base = %q, want main", opened.Base)
	}
	for _, want := range []string{"kibitz (AI)", "Closes #12", "SQS の subscriber", "internal/queue/sqs.go"} {
		if !strings.Contains(opened.Body, want) {
			t.Errorf("the description does not contain %q:\n%s", want, opened.Body)
		}
	}

	// The branch has to be on the remote, with the change in it.
	if branches := remoteBranches(t, origin); !strings.Contains(branches, "kibitz/issue-12") {
		t.Fatalf("the branch was not pushed:\n%s", branches)
	}
	pushed := gitOutput(t, origin, "show", "--name-only", "--format=", "refs/heads/kibitz/issue-12")
	if !strings.Contains(pushed, "internal/queue/sqs.go") {
		t.Errorf("the pushed commit does not contain the change:\n%s", pushed)
	}
	// kibitz's own prompt lives in the checkout and is not part of the change.
	if strings.Contains(pushed, ".kibitz/") {
		t.Errorf("kibitz's own files were committed:\n%s", pushed)
	}

	if box.verifications() != 1 {
		t.Fatalf("want one verification, got %d", box.verifications())
	}
	if !box.trees[0][filepath.FromSlash("internal/queue/sqs.go")] {
		t.Error("the verification did not see the edited tree")
	}
	if len(box.requests[0].Commands) != 1 || strings.Join(box.requests[0].Commands[0], " ") != "go test ./..." {
		t.Errorf("the verification ran %v, want [[go test ./...]]", box.requests[0].Commands)
	}
}

func TestAChangeOutsideTheAllowedPathsIsDiscardedWhole(t *testing.T) {
	engine := &editingEngine{
		writes: map[string]string{
			"internal/queue/sqs.go":    "package sqs\n",
			".github/workflows/ci.yml": "on: push\n",
		},
		reply: "CI も直しておきました。",
	}
	box := &fakeSandbox{result: passing()}
	j, f, ev, origin := implementRun(t, engine, box)

	if err := j.Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(f.opened) != 0 {
		t.Fatalf("a pull request was opened for a change that reached outside: %+v", f.opened)
	}
	if box.verifications() != 0 {
		t.Error("the change was verified before it was checked; the check is meant to come first")
	}
	if branches := remoteBranches(t, origin); strings.Contains(branches, "kibitz/") {
		t.Errorf("a branch was pushed:\n%s", branches)
	}
	comment := said(f)
	if !strings.Contains(comment, ".github/workflows/ci.yml") {
		t.Errorf("the comment does not name the path that was refused:\n%s", comment)
	}
	// The allowed half is not kept either.
	if !strings.Contains(comment, "すべて") {
		t.Errorf("the comment does not say the whole change was discarded:\n%s", comment)
	}
}

func TestChangingNothingIsReportedAndNotAFailure(t *testing.T) {
	engine := &editingEngine{reply: "この Issue は既に実装されています。変更は不要です。"}
	box := &fakeSandbox{result: passing()}
	j, f, ev, _ := implementRun(t, engine, box)

	if err := j.Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(f.opened) != 0 {
		t.Fatalf("an empty change became a pull request: %+v", f.opened)
	}
	if box.verifications() != 0 {
		t.Error("nothing was changed and it was verified anyway")
	}
	comment := said(f)
	if !strings.Contains(comment, "変更なし") || !strings.Contains(comment, "既に実装されています") {
		t.Errorf("the comment does not report why nothing changed:\n%s", comment)
	}
}

func TestAnExistingPullRequestIsReportedWithoutRunningTheAgent(t *testing.T) {
	engine := &editingEngine{writes: map[string]string{"internal/queue/sqs.go": "package sqs\n"}}
	box := &fakeSandbox{result: passing()}
	j, f, ev, _ := implementRun(t, engine, box)
	f.existing = &forge.PullRequestInfo{Number: 77, URL: "https://example.com/pull/77", Draft: true}

	if err := j.Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(engine.requests) != 0 {
		t.Error("the agent ran although the issue already had a pull request; that is a model call for nothing")
	}
	if !strings.Contains(said(f), "https://example.com/pull/77") {
		t.Errorf("the comment does not point at the existing pull request:\n%s", said(f))
	}
}

func TestAnExistingBranchIsNotOverwritten(t *testing.T) {
	engine := &editingEngine{writes: map[string]string{"internal/queue/sqs.go": "package sqs\n"}}
	box := &fakeSandbox{result: passing()}
	j, f, ev, origin := implementRun(t, engine, box)

	// Somebody's branch, with something in it kibitz must not destroy.
	gitRun(t, origin, "branch", "kibitz/issue-12")
	before := gitOutput(t, origin, "rev-parse", "refs/heads/kibitz/issue-12")

	if err := j.Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	after := gitOutput(t, origin, "rev-parse", "refs/heads/kibitz/issue-12")
	if after != before {
		t.Errorf("the branch was overwritten: %s -> %s", before, after)
	}
	if len(f.opened) != 0 {
		t.Errorf("a pull request was opened onto somebody else's branch: %+v", f.opened)
	}
	if !strings.Contains(said(f), "上書きはしません") {
		t.Errorf("the comment does not say the branch was left alone:\n%s", said(f))
	}
}

func TestNoSandboxRefusesRatherThanSkippingVerification(t *testing.T) {
	engine := &editingEngine{writes: map[string]string{"internal/queue/sqs.go": "package sqs\n"}}
	j, f, ev, _ := implementRun(t, engine, nil)
	j.Sandbox = nil

	if err := j.Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(f.opened) != 0 {
		t.Fatalf("a pull request was opened with no verification configured: %+v", f.opened)
	}
	if len(engine.requests) != 0 {
		t.Error("the agent ran although there was nowhere to verify what it wrote")
	}
	if !strings.Contains(said(f), "KIBITZ_SANDBOX_LOCATION") {
		t.Errorf("the comment does not say what is missing:\n%s", said(f))
	}
}

// A verification that could not be carried out is not a pass. The runner
// reports that with Failure rather than with a failing step, and the two must
// come to the same answer.
func TestAVerificationThatCouldNotRunIsNotAPass(t *testing.T) {
	engine := &editingEngine{writes: map[string]string{"internal/queue/sqs.go": "package sqs\n"}}
	box := &fakeSandbox{result: &sandbox.Result{Failure: "ランナーが結果を書きませんでした"}}
	j, f, ev, _ := implementRun(t, engine, box)

	if err := j.Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(f.opened) != 0 {
		t.Fatalf("a pull request was opened although nothing was verified: %+v", f.opened)
	}
	if !strings.Contains(said(f), "ランナーが結果を書きませんでした") {
		t.Errorf("the comment does not say what went wrong:\n%s", said(f))
	}
}

// Machinery failing is different from a change failing: the message goes back
// to the queue, and nothing is said on the issue.
func TestAVerifierThatBreaksRetriesRatherThanRefusing(t *testing.T) {
	engine := &editingEngine{writes: map[string]string{"internal/queue/sqs.go": "package sqs\n"}}
	box := &fakeSandbox{err: errors.New("the bucket is not reachable")}
	j, f, ev, _ := implementRun(t, engine, box)

	err := j.Handle(context.Background(), queued(ev))
	if err == nil {
		t.Fatal("Handle returned nil; a broken verifier has to redeliver the message")
	}
	if len(f.opened) != 0 {
		t.Errorf("a pull request was opened: %+v", f.opened)
	}
}

// The prompt has to carry what the agent is allowed to do, or it is being asked
// to guess at the rules it will be held to.
func TestTheAgentIsToldWhatItMayEditAndWhatWillCheckIt(t *testing.T) {
	engine := &editingEngine{writes: map[string]string{"internal/queue/sqs.go": "package sqs\n"}}
	box := &fakeSandbox{result: passing()}
	j, _, ev, _ := implementRun(t, engine, box)

	if err := j.Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(engine.requests) != 1 {
		t.Fatalf("want one agent run, got %d", len(engine.requests))
	}

	req := engine.requests[0]
	if req.Mode != reviewer.ModeImplement {
		t.Errorf("Mode = %q, want %q", req.Mode, reviewer.ModeImplement)
	}
	if len(req.EditablePaths) != 1 || req.EditablePaths[0] != "internal/**" {
		t.Errorf("EditablePaths = %v, want [internal/**]", req.EditablePaths)
	}
	if len(req.VerifyCommands) != 1 || req.VerifyCommands[0] != "go test ./..." {
		t.Errorf("VerifyCommands = %v, want [go test ./...]", req.VerifyCommands)
	}
}

func remoteBranches(t *testing.T, dir string) string {
	t.Helper()
	return gitOutput(t, dir, "for-each-ref", "--format=%(refname)", "refs/heads/")
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	gitOutput(t, dir, args...)
}
