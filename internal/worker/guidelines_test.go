package worker_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/repoconfig"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/worker"
)

// A repository's conventions reach the agent as instructions, read from the
// default branch — never from the branch under review, which is written by
// whoever opened the pull request.
func TestConventionsReachTheAgent(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	f.files = map[string]string{
		"AGENTS.md":             "- エラーは %w でラップすること",
		".kibitz/guidelines.md": "- 新しい公開 API には doc コメントを付けること",
	}
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}

	job := newJob(t, f, e)
	job.Guidelines = "- 運用側のルール"

	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got := e.requests[0].Guidelines
	for _, want := range []string{"運用側のルール", "%w でラップ", "doc コメント"} {
		if !strings.Contains(got, want) {
			t.Errorf("guidelines do not carry %q:\n%s", want, got)
		}
	}
	// The deployment's rules come first: a repository adds to them, which is
	// the only reason reading these as instructions is safe.
	if strings.Index(got, "運用側のルール") > strings.Index(got, "%w でラップ") {
		t.Errorf("the repository's rules came before the deployment's:\n%s", got)
	}
	// Named, so a cited rule can be traced to the file somebody edits.
	if !strings.Contains(got, "AGENTS.md") {
		t.Errorf("the source file is not named:\n%s", got)
	}
}

// .kibitz.yaml has the last word, after the convention files.
func TestKibitzYamlGuidelinesComeLast(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	f.files = map[string]string{
		"AGENTS.md":     "- AGENTS のルール",
		repoconfig.Path: "guidelines: |\n  - kibitz.yaml のルール\n",
	}
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}

	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got := e.requests[0].Guidelines
	if !strings.Contains(got, "AGENTS のルール") || !strings.Contains(got, "kibitz.yaml のルール") {
		t.Fatalf("guidelines = %q", got)
	}
	if strings.Index(got, "AGENTS のルール") > strings.Index(got, "kibitz.yaml のルール") {
		t.Errorf("the order is wrong:\n%s", got)
	}
}

// A repository with none of these files is the ordinary case.
func TestNoConventionsIsFine(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}

	job := newJob(t, f, e)
	job.Guidelines = "- 運用側のルール"

	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := e.requests[0].Guidelines; got != "- 運用側のルール" {
		t.Errorf("guidelines = %q, want the deployment's alone", got)
	}
}

// A deployment can refuse to let a repository write any part of the
// instructions.
func TestConventionsCanBeTurnedOff(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	f.files = map[string]string{"AGENTS.md": "- AGENTS のルール"}
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}

	job := newJob(t, f, e)
	job.GuidelineFiles = []string{}

	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if strings.Contains(e.requests[0].Guidelines, "AGENTS") {
		t.Errorf("a convention file was read although they are off:\n%s", e.requests[0].Guidelines)
	}
	for _, path := range f.filesRead {
		if path != repoconfig.Path {
			t.Errorf("%s was read although convention files are off", path)
		}
	}
}

// A manual is not a set of conventions, and the part past the cap would crowd
// out the diff it is meant to inform.
func TestALongConventionFileIsTruncated(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	f.files = map[string]string{"AGENTS.md": strings.Repeat("あ", 40_000) + "END_MARKER"}
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}

	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := e.requests[0].Guidelines
	if strings.Contains(got, "END_MARKER") {
		t.Error("the file was not truncated")
	}
	if !strings.Contains(got, "打ち切り") {
		t.Errorf("the truncation is not stated:\n%s", got[len(got)-200:])
	}
	// And the pull request is told, because a rule that was cut off is a rule
	// the review did not apply.
	if !strings.Contains(f.summaries[0], "AGENTS.md") {
		t.Errorf("the summary does not mention it:\n%s", f.summaries[0])
	}
}

// A forge that cannot be read is not a reason to skip the review.
func TestUnreadableConventionsDoNotFailTheJob(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	f.fileErr = errors.New("500 from the forge")
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}

	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(e.requests) != 1 {
		t.Fatal("the review was skipped")
	}
	if !strings.Contains(f.summaries[0], "AGENTS.md") {
		t.Errorf("the summary does not say what could not be read:\n%s", f.summaries[0])
	}
}

func TestDefaultGuidelineFiles(t *testing.T) {
	// CONTRIBUTING.md is deliberately absent: it is written for a person
	// opening their first pull request, and paying for it on every review
	// buys nothing.
	for _, path := range worker.DefaultGuidelineFiles {
		if path == "CONTRIBUTING.md" {
			t.Error("CONTRIBUTING.md is read by default")
		}
	}
	if len(worker.DefaultGuidelineFiles) == 0 {
		t.Error("no convention files are read by default")
	}
}
