package workspace_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/workspace"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=kibitz", "GIT_AUTHOR_EMAIL=kibitz@example.com",
		"GIT_COMMITTER_NAME=kibitz", "GIT_COMMITTER_EMAIL=kibitz@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// originRepo builds a repository that looks like a forge's: a main branch and
// a pull request head published under refs/pull/42/head, which is the only way
// a fork's changes are reachable.
func originRepo(t *testing.T) (dir, headSHA, baseSHA string) {
	t.Helper()

	dir = t.TempDir()
	git(t, dir, "init", "--quiet", "--initial-branch=main")
	git(t, dir, "config", "user.email", "kibitz@example.com")
	git(t, dir, "config", "user.name", "kibitz")

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("writing file: %v", err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "--quiet", "-m", "initial")
	baseSHA = git(t, dir, "rev-parse", "HEAD")

	git(t, dir, "checkout", "--quiet", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "queue.go"), []byte("package main // queue\n"), 0o600); err != nil {
		t.Fatalf("writing file: %v", err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "--quiet", "-m", "add the queue")
	headSHA = git(t, dir, "rev-parse", "HEAD")

	// Publish it the way GitHub does and put the branch out of reach, so the
	// test fails if the code fetches by branch name.
	git(t, dir, "update-ref", "refs/pull/42/head", headSHA)
	git(t, dir, "checkout", "--quiet", "main")
	git(t, dir, "branch", "--quiet", "-D", "feature")

	return dir, headSHA, baseSHA
}

func TestPrepare(t *testing.T) {
	origin, headSHA, baseSHA := originRepo(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ws, err := workspace.Prepare(ctx, workspace.Config{Root: t.TempDir(), Depth: 10, Timeout: 30 * time.Second},
		workspace.Spec{
			CloneURL:   origin,
			HeadRef:    forge.HeadRef(event.PlatformGitHub, 42),
			BaseBranch: "main",
		}, discardLogger())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer func() { _ = ws.Close() }()

	if ws.HeadSHA != headSHA {
		t.Errorf("HeadSHA = %s, want %s", ws.HeadSHA, headSHA)
	}
	if ws.BaseSHA != baseSHA {
		t.Errorf("BaseSHA = %s, want %s", ws.BaseSHA, baseSHA)
	}
	// The pull request's file must be on disk for the agent to read.
	if _, err := os.Stat(filepath.Join(ws.Dir, "queue.go")); err != nil {
		t.Errorf("the pull request's file is missing: %v", err)
	}

	size, err := ws.Size()
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if size <= 0 {
		t.Error("Size() = 0, want the checkout's size")
	}
}

func TestPrepareWithoutBaseBranchStillWorks(t *testing.T) {
	origin, headSHA, _ := originRepo(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A base branch that no longer exists must not fail the review: the diff
	// came from the API, not from the checkout.
	ws, err := workspace.Prepare(ctx, workspace.Config{Root: t.TempDir(), Depth: 1},
		workspace.Spec{
			CloneURL:   origin,
			HeadRef:    forge.HeadRef(event.PlatformGitHub, 42),
			BaseBranch: "deleted-branch",
		}, discardLogger())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer func() { _ = ws.Close() }()

	if ws.HeadSHA != headSHA {
		t.Errorf("HeadSHA = %s, want %s", ws.HeadSHA, headSHA)
	}
	if ws.BaseSHA != "" {
		t.Errorf("BaseSHA = %q, want it empty", ws.BaseSHA)
	}
}

func TestPrepareValidatesSpec(t *testing.T) {
	ctx := context.Background()
	cfg := workspace.Config{Root: t.TempDir()}

	if _, err := workspace.Prepare(ctx, cfg, workspace.Spec{HeadRef: "refs/pull/1/head"}, discardLogger()); err == nil {
		t.Error("Prepare accepted a spec with no clone url")
	}
	if _, err := workspace.Prepare(ctx, cfg, workspace.Spec{CloneURL: "https://example.invalid/x.git"}, discardLogger()); err == nil {
		t.Error("Prepare accepted a spec with no head ref")
	}
}

// A failed fetch must not leave a directory behind, or a worker that fails
// repeatedly fills its disk.
func TestPrepareCleansUpAfterFailure(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, err := workspace.Prepare(ctx, workspace.Config{Root: root, Depth: 1, Timeout: 20 * time.Second},
		workspace.Spec{
			CloneURL: filepath.Join(t.TempDir(), "does-not-exist"),
			HeadRef:  forge.HeadRef(event.PlatformGitHub, 42),
		}, discardLogger())
	if err == nil {
		t.Fatal("Prepare succeeded against a missing repository")
	}

	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		t.Fatalf("reading root: %v", readErr)
	}
	if len(entries) != 0 {
		t.Errorf("%d directories left behind, want none", len(entries))
	}
}

func TestCloseRemovesTheCheckout(t *testing.T) {
	origin, _, _ := originRepo(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ws, err := workspace.Prepare(ctx, workspace.Config{Root: t.TempDir(), Depth: 1},
		workspace.Spec{CloneURL: origin, HeadRef: forge.HeadRef(event.PlatformGitHub, 42)}, discardLogger())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	dir := ws.Dir

	if err := ws.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the workspace survived Close: %v", err)
	}
}

func TestHeadRefPerPlatform(t *testing.T) {
	tests := []struct {
		platform event.Platform
		want     string
	}{
		{event.PlatformGitHub, "refs/pull/42/head"},
		{event.PlatformGitLab, "refs/merge-requests/42/head"},
		{event.PlatformAzureDevOps, "refs/pull/42/merge"},
	}
	for _, tc := range tests {
		if got := forge.HeadRef(tc.platform, 42); got != tc.want {
			t.Errorf("HeadRef(%s) = %q, want %q", tc.platform, got, tc.want)
		}
	}
}
