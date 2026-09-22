package worker_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/worker"
)

// withDocs puts decision records on the pull request's head, which is the
// commit the workspace checks out.
func withDocs(t *testing.T, repo string, docs map[string]string) {
	t.Helper()

	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
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

	base := git("rev-parse", "HEAD")
	git("checkout", "--quiet", "-B", "docs-tmp", "refs/pull/42/head")
	for path, body := range docs {
		full := filepath.Join(repo, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil { //nolint:gosec // a test fixture
			t.Fatalf("write %s: %v", path, err)
		}
	}
	git("add", ".")
	git("commit", "--quiet", "-m", "add decision records")
	git("update-ref", "refs/pull/42/head", git("rev-parse", "HEAD"))
	git("checkout", "--quiet", "main")
	git("reset", "--quiet", "--hard", base)
}

// The index is what turns "could read them" into "knows they exist".
func TestDecisionRecordsAreIndexedInThePrompt(t *testing.T) {
	origin, _, _ := originRepo(t)
	withDocs(t, origin, map[string]string{
		"docs/adr/0003-ordering.md": "# Pub/Sub の ordering key で PR 単位の順序を保つ\n\n本文。\n",
		"docs/adr/0007-no-pat.md":   "# PAT 認証は実装しない\n\nGitHub App だけを使う。\n",
		"docs/worker.md":            "# これは ADR ではない\n",
	})

	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}
	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	refs := e.requests[0].References
	if len(refs) != 2 {
		t.Fatalf("%d references, want the two ADRs: %+v", len(refs), refs)
	}
	// The title is what gives the agent the vocabulary to search with: the
	// ADR says "ordering key" where the diff says PublishOrdered.
	var titles []string
	for _, ref := range refs {
		titles = append(titles, ref.Title)
	}
	joined := strings.Join(titles, " / ")
	if !strings.Contains(joined, "ordering key") || !strings.Contains(joined, "PAT 認証") {
		t.Errorf("titles = %q", joined)
	}
}

// Only the configured places. A repository's whole docs tree is not a set of
// decisions, and the index is sent with every prompt.
func TestOnlyTheConfiguredPlacesAreIndexed(t *testing.T) {
	origin, _, _ := originRepo(t)
	withDocs(t, origin, map[string]string{
		"docs/adr/0001-a.md": "# ADR\n",
		"docs/guide.md":      "# ガイド\n",
		"README.md":          "# readme\n",
	})

	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}
	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	for _, ref := range e.requests[0].References {
		if !strings.HasPrefix(ref.Path, "docs/adr/") {
			t.Errorf("%s was indexed", ref.Path)
		}
	}
}

// A repository without decision records pays nothing.
func TestNoDecisionRecordsCostsNothing(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}

	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := e.requests[0].References; len(got) != 0 {
		t.Errorf("references = %+v, want none", got)
	}
}

func TestReferenceDocsCanBeTurnedOff(t *testing.T) {
	origin, _, _ := originRepo(t)
	withDocs(t, origin, map[string]string{"docs/adr/0001-a.md": "# ADR\n"})

	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}
	job := newJob(t, f, e)
	job.ReferenceDocs = []string{}

	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := e.requests[0].References; len(got) != 0 {
		t.Errorf("references = %+v, want none", got)
	}
}

// A file with no heading is still listed, by its name: leaving it out would
// hide a decision because somebody forgot a "#".
func TestADocumentWithoutAHeading(t *testing.T) {
	origin, _, _ := originRepo(t)
	withDocs(t, origin, map[string]string{"docs/adr/0009-untitled.md": "見出しのない本文。\n"})

	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}
	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	refs := e.requests[0].References
	if len(refs) != 1 || refs[0].Title != "0009-untitled.md" {
		t.Errorf("references = %+v", refs)
	}
}

func TestDefaultReferenceDocsAreDecisionRecords(t *testing.T) {
	for _, pattern := range worker.DefaultReferenceDocs {
		if !strings.Contains(pattern, "adr") && !strings.Contains(pattern, "decision") {
			t.Errorf("%q is not a decision-record location", pattern)
		}
	}
}
