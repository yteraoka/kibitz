package jobcontext_test

import (
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/jobcontext"
)

func file(path string) forge.File {
	return forge.File{Path: path, Status: forge.FileModified, Additions: 2, Deletions: 1, Patch: "@@ -1 +1,2 @@\n+x"}
}

func TestBuildAndRoundTrip(t *testing.T) {
	ev := &event.ReviewEvent{
		Source:     event.Source{Platform: event.PlatformGitHub},
		Repository: event.Repository{FullName: "yteraoka/kibitz"},
	}
	pr := &event.PullRequest{
		Number: 42, Title: "Add the SQS subscriber", Description: "Closes #7",
		State: "open", Author: event.Actor{Login: "yteraoka"},
		Source: event.Ref{Branch: "feat/sqs", SHA: "abc1234"},
		Target: event.Ref{Branch: "main"},
	}
	// The whole pull request, and the smaller part the prompt carried.
	all := &forge.Diff{Files: []forge.File{file("queue.go"), file("docs/skipped.md")}}
	reviewed := &forge.Diff{Files: []forge.File{file("queue.go")}}
	comments := []forge.Comment{{Author: event.Actor{Login: "alice"}, Path: "queue.go", Line: 2, Body: "ctx?"}}

	dir := t.TempDir()
	path, err := jobcontext.Build(ev, pr, all, reviewed, comments, dir, nil).Write(dir)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := jobcontext.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Repository != "yteraoka/kibitz" || got.PullRequest.Number != 42 {
		t.Errorf("context = %+v", got)
	}
	// The file the prompt left out is still here, which is the point.
	if len(got.Files) != 2 {
		t.Fatalf("%d files, want the whole pull request", len(got.Files))
	}
	if _, ok := got.File("docs/skipped.md"); !ok {
		t.Error("the file triage left out is not in the context")
	}
	if !got.WasReviewed("queue.go") || got.WasReviewed("docs/skipped.md") {
		t.Errorf("Reviewed = %v", got.Reviewed)
	}
	if len(got.Comments) != 1 || got.Comments[0].Author != "alice" {
		t.Errorf("comments = %+v", got.Comments)
	}
}

// A leading "./" is what a model writes about as often as not.
func TestFileNormalizesThePath(t *testing.T) {
	ctx := jobcontext.Build(nil, nil, &forge.Diff{Files: []forge.File{file("queue.go")}}, nil, nil, "", nil)
	if _, ok := ctx.File(" ./queue.go "); !ok {
		t.Error("a path with a ./ prefix was not found")
	}
	if _, ok := ctx.File("nope.go"); ok {
		t.Error("a file that is not in the diff was found")
	}
}

// Reading a file from a version that wrote a different shape would be
// guessing at fields.
func TestLoadRefusesAnotherSchema(t *testing.T) {
	dir := t.TempDir()
	ctx := jobcontext.Build(nil, nil, nil, nil, nil, "", nil)
	ctx.SchemaVersion = jobcontext.SchemaVersion + 1
	path, err := ctx.Write(dir)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := jobcontext.Load(path); err == nil {
		t.Error("a context from another schema version was accepted")
	}
}

func TestLoadRejectsNonsense(t *testing.T) {
	if _, err := jobcontext.Load(t.TempDir() + "/missing.json"); err == nil {
		t.Error("a missing file was accepted")
	}
}
