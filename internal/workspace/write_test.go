package workspace_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/workspace"
)

// prepared is a workspace checked out at the origin's main branch, which is
// what implement mode works from.
func prepared(t *testing.T, origin string) *workspace.Workspace {
	t.Helper()

	ws, err := workspace.Prepare(context.Background(), workspace.Config{Root: t.TempDir()}, workspace.Spec{
		CloneURL:   origin,
		HeadRef:    "refs/heads/main",
		BaseBranch: "main",
	}, discardLogger())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

func write(t *testing.T, ws *workspace.Workspace, path, body string) {
	t.Helper()
	full := filepath.Join(ws.Dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestChangesReportsEveryKindOfEdit(t *testing.T) {
	origin, _, _ := originRepo(t)
	ws := prepared(t, origin)

	write(t, ws, "internal/queue/queue.go", "package queue\n") // added
	write(t, ws, "main.go", "package main // edited\n")        // modified
	if err := os.Remove(filepath.Join(ws.Dir, "README.md")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	changed, err := ws.Changes(context.Background())
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}

	got := strings.Join(changed, " ")
	for _, want := range []string{"internal/queue/queue.go", "main.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("Changes() = %v, missing %q", changed, want)
		}
	}
}

// A rename has to arrive as two paths. Every path is checked against what may
// be edited, and a pair that travelled as one entry would have half of it
// checked.
func TestChangesReportsARenameAsTwoPaths(t *testing.T) {
	origin, _, _ := originRepo(t)
	ws := prepared(t, origin)

	body, err := os.ReadFile(filepath.Join(ws.Dir, "main.go")) //nolint:gosec // a path this test built
	if err != nil {
		t.Fatal(err)
	}
	write(t, ws, "cmd/main.go", string(body))
	if err := os.Remove(filepath.Join(ws.Dir, "main.go")); err != nil {
		t.Fatal(err)
	}

	changed, err := ws.Changes(context.Background())
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	got := strings.Join(changed, " ")
	if !strings.Contains(got, "main.go") || !strings.Contains(got, "cmd/main.go") {
		t.Errorf("Changes() = %v, want both sides of the rename", changed)
	}
}

func TestChangesIsEmptyWhenNothingWasTouched(t *testing.T) {
	origin, _, _ := originRepo(t)
	ws := prepared(t, origin)

	changed, err := ws.Changes(context.Background())
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("Changes() = %v, want none", changed)
	}
}

// Discard takes kibitz's own files back out without touching what the
// repository tracks under the same directory.
func TestDiscardKeepsTrackedFiles(t *testing.T) {
	origin, _, _ := originRepo(t)
	// A repository that keeps its conventions where kibitz keeps its prompt.
	if err := os.MkdirAll(filepath.Join(origin, ".kibitz"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, ".kibitz", "guidelines.md"), []byte("エラーは包む\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "checkout", "--quiet", "main")
	git(t, origin, "add", ".")
	git(t, origin, "commit", "--quiet", "-m", "add guidelines")

	ws := prepared(t, origin)
	write(t, ws, ".kibitz/prompt.md", "# 依頼\n")

	if err := ws.Discard(context.Background(), ".kibitz"); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	if _, err := os.Stat(filepath.Join(ws.Dir, ".kibitz", "prompt.md")); !os.IsNotExist(err) {
		t.Errorf("kibitz's own prompt was left behind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, ".kibitz", "guidelines.md")); err != nil {
		t.Errorf("the repository's own file was deleted: %v", err)
	}

	changed, err := ws.Changes(context.Background())
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("Changes() = %v after discarding kibitz's own files, want none", changed)
	}
}

func TestRecordAndPush(t *testing.T) {
	origin, _, _ := originRepo(t)
	ws := prepared(t, origin)

	write(t, ws, "internal/queue/queue.go", "package queue\n")
	if _, err := ws.Changes(context.Background()); err != nil {
		t.Fatalf("Changes: %v", err)
	}

	sha, err := ws.Record(context.Background(), workspace.Commit{
		Message: "feat: implement issue #12",
		Name:    "kibitz",
		Email:   "kibitz@users.noreply.github.com",
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(sha) != 40 {
		t.Errorf("Record returned %q, want a commit id", sha)
	}

	if err := ws.Push(context.Background(), "kibitz/issue-12"); err != nil {
		t.Fatalf("Push: %v", err)
	}

	if got := git(t, origin, "rev-parse", "refs/heads/kibitz/issue-12"); got != sha {
		t.Errorf("the origin has %s, want %s", got, sha)
	}
	if author := git(t, origin, "log", "-1", "--format=%an <%ae>", sha); author != "kibitz <kibitz@users.noreply.github.com>" {
		t.Errorf("author = %q", author)
	}
}

func TestRecordRefusesWithNothingStaged(t *testing.T) {
	origin, _, _ := originRepo(t)
	ws := prepared(t, origin)

	_, err := ws.Record(context.Background(), workspace.Commit{
		Message: "feat: nothing", Name: "kibitz", Email: "kibitz@example.com",
	})
	if !errors.Is(err, workspace.ErrNoChanges) {
		t.Errorf("error = %v, want workspace.ErrNoChanges", err)
	}
}

func TestRecordNeedsAMessageAndAnAuthor(t *testing.T) {
	origin, _, _ := originRepo(t)
	ws := prepared(t, origin)
	write(t, ws, "internal/queue/queue.go", "package queue\n")

	for name, commit := range map[string]workspace.Commit{
		"no message": {Name: "kibitz", Email: "k@example.com"},
		"no name":    {Message: "feat: x", Email: "k@example.com"},
		"no email":   {Message: "feat: x", Name: "kibitz"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ws.Record(context.Background(), commit); err == nil {
				t.Error("Record returned nil error")
			}
		})
	}
}

func TestBranchExists(t *testing.T) {
	origin, _, _ := originRepo(t)
	git(t, origin, "branch", "kibitz/issue-7", "main")
	ws := prepared(t, origin)

	for branch, want := range map[string]bool{
		"kibitz/issue-7":  true,
		"kibitz/issue-12": false,
		"main":            true,
	} {
		got, err := ws.BranchExists(context.Background(), branch)
		if err != nil {
			t.Fatalf("BranchExists(%q): %v", branch, err)
		}
		if got != want {
			t.Errorf("BranchExists(%q) = %v, want %v", branch, got, want)
		}
	}
}

// A push never overwrites. The branch moving would mean somebody's commits are
// only in a reflog kibitz cannot reach.
func TestPushDoesNotOverwriteAnExistingBranch(t *testing.T) {
	origin, _, _ := originRepo(t)
	git(t, origin, "branch", "kibitz/issue-12", "refs/pull/42/head")
	before := git(t, origin, "rev-parse", "refs/heads/kibitz/issue-12")

	ws := prepared(t, origin)
	write(t, ws, "internal/queue/queue.go", "package queue\n")
	if _, err := ws.Changes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Record(context.Background(), workspace.Commit{
		Message: "feat: implement issue #12", Name: "kibitz", Email: "k@example.com",
	}); err != nil {
		t.Fatal(err)
	}

	if err := ws.Push(context.Background(), "kibitz/issue-12"); err == nil {
		t.Error("Push returned nil error over an existing branch")
	}
	if after := git(t, origin, "rev-parse", "refs/heads/kibitz/issue-12"); after != before {
		t.Errorf("the branch moved: %s -> %s", before, after)
	}
}

func TestPushRefusesABranchNameGitWouldReadAsSomethingElse(t *testing.T) {
	origin, _, _ := originRepo(t)
	ws := prepared(t, origin)

	for _, branch := range []string{
		"", "  ", "--force", "/leading", "trailing/",
		"kibitz/../main", "kibitz//issue-1", "kibitz/issue 1\n--force",
		"kibitz/issue:1", "kibitz/issue~1", "kibitz/issue^1", "kibitz/issue?1",
		"kibitz/issue*", "kibitz/issue[1]", "kibitz/issue\\1",
	} {
		if err := ws.Push(context.Background(), branch); err == nil {
			t.Errorf("Push(%q) returned nil error", branch)
		}
		if _, err := ws.BranchExists(context.Background(), branch); err == nil {
			t.Errorf("BranchExists(%q) returned nil error", branch)
		}
	}
}
