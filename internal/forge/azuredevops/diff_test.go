package azuredevops_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/reviewer"
)

const repoBase = "/Fabrikam/_apis/git/repositories/kibitz"

// blob registers the content a git object id resolves to.
func (s *stub) blob(objectID, content string) {
	s.raw["GET "+repoBase+"/blobs/"+objectID] = []byte(content)
}

// iteration registers one iteration and the changes in it.
func (s *stub) iteration(id int, common string, changes ...map[string]any) {
	s.responses["GET "+prBase+"/iterations"] = map[string]any{
		"value": []any{map[string]any{
			"id":              id,
			"commonRefCommit": map[string]any{"commitId": common},
		}},
	}
	s.responses[fmt.Sprintf("GET %s/iterations/%d/changes", prBase, id)] = map[string]any{
		"changeEntries": changes,
		"nextSkip":      0,
	}
}

func edit(path, oldID, newID string) map[string]any {
	return map[string]any{
		"changeType": "edit",
		"item":       map[string]any{"path": path, "objectId": newID, "originalObjectId": oldID},
	}
}

func fileIn(t *testing.T, d *forge.Diff, path string) forge.File {
	t.Helper()
	for _, f := range d.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("%q is not in the diff; it has %d files", path, len(d.Files))
	return forge.File{}
}

// TestDiffComputesThePatchFromTheTwoBlobs is the whole point of this file.
// Azure DevOps returns no patch, so the client has to fetch both versions of
// the file and produce one.
func TestDiffComputesThePatchFromTheTwoBlobs(t *testing.T) {
	s := newStub(t)
	s.iteration(3, "base111", edit("/main.go", "old1", "new1"))
	s.blob("old1", "package main\n\nfunc main() {}\n")
	s.blob("new1", "package main\n\nfunc main() { println(\"hi\") }\n")
	c, _ := s.client()

	diff, err := c.Diff(context.Background(), testRef())
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	f := fileIn(t, diff, "main.go")
	if f.Status != forge.FileModified {
		t.Errorf("status is %q, want modified", f.Status)
	}
	if f.Additions != 1 || f.Deletions != 1 {
		t.Errorf("counted +%d -%d, want +1 -1", f.Additions, f.Deletions)
	}
	if !strings.HasPrefix(f.Patch, "@@ ") {
		t.Errorf("the patch does not start with a hunk header:\n%s", f.Patch)
	}
	if !strings.Contains(f.Patch, `+func main() { println("hi") }`) {
		t.Errorf("the patch does not show the new line:\n%s", f.Patch)
	}
	if strings.Contains(f.Patch, "---") || strings.Contains(f.Patch, "+++") {
		t.Errorf("the patch carries a file header, which the other platforms do not send:\n%s", f.Patch)
	}
}

// TestTheDiffIsCheckedAgainstTheSameLinesTheReviewerWillCheck guards the
// reason the patch is computed at all: a finding is only posted if it lands
// on a line the patch covers.
func TestTheDiffIsCheckedAgainstTheSameLinesTheReviewerWillCheck(t *testing.T) {
	s := newStub(t)
	s.iteration(1, "base111", edit("/a.txt", "o", "n"))
	s.blob("o", "one\ntwo\nthree\n")
	s.blob("n", "one\nTWO\nthree\n")
	c, _ := s.client()

	diff, err := c.Diff(context.Background(), testRef())
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if got := diff.Lines(); got != 2 {
		t.Errorf("Lines() is %d, want 2", got)
	}
}

func TestAnAddedFileHasNoPreviousVersionToFetch(t *testing.T) {
	s := newStub(t)
	s.iteration(1, "base111", map[string]any{
		"changeType": "add",
		"item":       map[string]any{"path": "/new.txt", "objectId": "n1"},
	})
	s.blob("n1", "hello\nworld\n")
	c, _ := s.client()

	diff, err := c.Diff(context.Background(), testRef())
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	f := fileIn(t, diff, "new.txt")
	if f.Status != forge.FileAdded {
		t.Errorf("status is %q, want added", f.Status)
	}
	if f.Additions != 2 || f.Deletions != 0 {
		t.Errorf("counted +%d -%d, want +2 -0", f.Additions, f.Deletions)
	}
	// An added file has no previous version, so nothing should have gone
	// looking for one at the base commit.
	for _, rec := range s.calls {
		if strings.HasSuffix(rec.Path, "/items") {
			t.Errorf("an added file was looked up at the base commit: %+v", rec.Query)
		}
	}
}

func TestADeletedFileIsAllDeletions(t *testing.T) {
	s := newStub(t)
	s.iteration(1, "base111", map[string]any{
		"changeType": "delete",
		"item":       map[string]any{"path": "/gone.txt", "originalObjectId": "o1", "objectId": "o1"},
	})
	s.blob("o1", "a\nb\nc\n")
	c, _ := s.client()

	diff, err := c.Diff(context.Background(), testRef())
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	f := fileIn(t, diff, "gone.txt")
	if f.Status != forge.FileRemoved {
		t.Errorf("status is %q, want removed", f.Status)
	}
	// The change record names the same object on both sides. Treating that
	// as the current version too would make a deletion look like no change
	// at all.
	if f.Deletions != 3 || f.Additions != 0 {
		t.Errorf("counted +%d -%d, want +0 -3", f.Additions, f.Deletions)
	}
}

// TestAPreviousVersionThatIsNotNamedIsLookedUpAtTheBaseCommit covers the case
// that would otherwise be silent: without the lookup the file reads as wholly
// new, every line counts as added, and a finding could land anywhere in it.
func TestAPreviousVersionThatIsNotNamedIsLookedUpAtTheBaseCommit(t *testing.T) {
	s := newStub(t)
	s.iteration(2, "base999", map[string]any{
		"changeType": "edit",
		"item":       map[string]any{"path": "/a.txt", "objectId": "n1"},
	})
	s.responses["GET "+repoBase+"/items"] = map[string]any{"objectId": "o1"}
	s.blob("o1", "one\ntwo\n")
	s.blob("n1", "one\ntwo\nthree\n")
	c, _ := s.client()

	diff, err := c.Diff(context.Background(), testRef())
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	f := fileIn(t, diff, "a.txt")
	if f.Additions != 1 || f.Deletions != 0 {
		t.Errorf("counted +%d -%d, want +1 -0:\n%s", f.Additions, f.Deletions, f.Patch)
	}

	var found bool
	for _, rec := range s.calls {
		if !strings.HasSuffix(rec.Path, "/items") {
			continue
		}
		found = true
		if got := rec.query("versionDescriptor.version"); got != "base999" {
			t.Errorf("looked the file up at %q, want the base commit base999", got)
		}
		if got := rec.query("versionDescriptor.versionType"); got != "commit" {
			t.Errorf("version type is %q, want commit", got)
		}
	}
	if !found {
		t.Error("the previous version was never looked up")
	}
}

func TestChangeTypesMapToStatuses(t *testing.T) {
	// Azure DevOps combines these, and two of them contain another as a
	// substring: "undelete" is not a delete, and "sourceRename" is a rename.
	tests := []struct {
		changeType string
		want       forge.FileStatus
	}{
		{"add", forge.FileAdded},
		{"edit", forge.FileModified},
		{"delete", forge.FileRemoved},
		{"rename", forge.FileRenamed},
		{"edit, rename", forge.FileRenamed},
		{"sourceRename", forge.FileRenamed},
		{"undelete", forge.FileAdded},
		{"encoding", forge.FileModified},
	}

	for _, tc := range tests {
		t.Run(tc.changeType, func(t *testing.T) {
			s := newStub(t)
			s.iteration(1, "base111", map[string]any{
				"changeType":   tc.changeType,
				"originalPath": "/was.txt",
				"item":         map[string]any{"path": "/a.txt", "objectId": "n", "originalObjectId": "o"},
			})
			s.blob("o", "a\n")
			s.blob("n", "b\n")
			c, _ := s.client()

			diff, err := c.Diff(context.Background(), testRef())
			if err != nil {
				t.Fatalf("Diff: %v", err)
			}
			if got := fileIn(t, diff, "a.txt").Status; got != tc.want {
				t.Errorf("%q became %q, want %q", tc.changeType, got, tc.want)
			}
		})
	}
}

func TestARenameCarriesWhereTheFileWas(t *testing.T) {
	s := newStub(t)
	s.iteration(1, "base111", map[string]any{
		"changeType":   "rename",
		"originalPath": "/old/name.txt",
		"item":         map[string]any{"path": "/new/name.txt", "objectId": "n", "originalObjectId": "o"},
	})
	s.blob("o", "same\n")
	s.blob("n", "same\n")
	c, _ := s.client()

	diff, err := c.Diff(context.Background(), testRef())
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if got := fileIn(t, diff, "new/name.txt").PreviousPath; got != "old/name.txt" {
		t.Errorf("previous path is %q, want old/name.txt", got)
	}
}

func TestFoldersAreNotFiles(t *testing.T) {
	s := newStub(t)
	s.iteration(1, "base111",
		map[string]any{
			"changeType": "add",
			"item":       map[string]any{"path": "/dir", "isFolder": true, "gitObjectType": "tree"},
		},
		map[string]any{
			"changeType": "add",
			"item":       map[string]any{"path": "/dir/a.txt", "objectId": "n"},
		},
	)
	s.blob("n", "x\n")
	c, _ := s.client()

	diff, err := c.Diff(context.Background(), testRef())
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(diff.Files) != 1 {
		t.Fatalf("the diff has %d files, want 1: %+v", len(diff.Files), diff.Files)
	}
	if diff.Files[0].Path != "dir/a.txt" {
		t.Errorf("the file is %q, want dir/a.txt", diff.Files[0].Path)
	}
}

func TestABinaryFileIsReportedWithoutAPatch(t *testing.T) {
	s := newStub(t)
	s.iteration(1, "base111", edit("/logo.png", "o", "n"))
	s.blob("o", "\x89PNG\x00\x01old")
	s.blob("n", "\x89PNG\x00\x01new")
	c, _ := s.client()

	diff, err := c.Diff(context.Background(), testRef())
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	f := fileIn(t, diff, "logo.png")
	if f.Patch != "" {
		t.Errorf("a binary file got a patch:\n%q", f.Patch)
	}
	if f.Additions != 0 || f.Deletions != 0 {
		t.Errorf("a binary file counted +%d -%d", f.Additions, f.Deletions)
	}
}

func TestDiffFollowsThePagingTheServiceDescribes(t *testing.T) {
	s := newStub(t)
	s.responses["GET "+prBase+"/iterations"] = map[string]any{
		"value": []any{map[string]any{"id": 1, "commonRefCommit": map[string]any{"commitId": "b"}}},
	}
	// The stub answers one body per address, so paging is exercised by the
	// first page asking for a second one that returns nothing more: what is
	// under test is that nextSkip is honoured at all.
	s.responses["GET "+prBase+"/iterations/1/changes"] = map[string]any{
		"changeEntries": []any{map[string]any{
			"changeType": "add",
			"item":       map[string]any{"path": "/a.txt", "objectId": "n"},
		}},
		"nextSkip": 0,
	}
	s.blob("n", "x\n")
	c, _ := s.client()

	if _, err := c.Diff(context.Background(), testRef()); err != nil {
		t.Fatalf("Diff: %v", err)
	}

	var asked bool
	for _, rec := range s.calls {
		if strings.HasSuffix(rec.Path, "/changes") {
			asked = true
			if got := rec.query("$top"); got != "2000" {
				t.Errorf("asked for $top=%q, want the maximum page of 2000", got)
			}
		}
	}
	if !asked {
		t.Error("the changes were never listed")
	}
}

func TestCompareAsksForTheRangeBetweenTwoCommits(t *testing.T) {
	s := newStub(t)
	s.responses["GET "+repoBase+"/diffs/commits"] = map[string]any{
		"allChangesIncluded": true,
		"changes": []any{map[string]any{
			"changeType": "edit",
			"item":       map[string]any{"path": "/a.txt", "objectId": "n", "originalObjectId": "o"},
		}},
	}
	s.blob("o", "one\n")
	s.blob("n", "one\ntwo\n")
	c, _ := s.client()

	diff, err := c.Compare(context.Background(), testRef(), "aaa111", "bbb222")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if f := fileIn(t, diff, "a.txt"); f.Additions != 1 {
		t.Errorf("counted +%d, want +1:\n%s", f.Additions, f.Patch)
	}

	var asked bool
	for _, rec := range s.calls {
		if !strings.HasSuffix(rec.Path, "/diffs/commits") {
			continue
		}
		asked = true
		for key, want := range map[string]string{
			"baseVersion":       "aaa111",
			"baseVersionType":   "commit",
			"targetVersion":     "bbb222",
			"targetVersionType": "commit",
		} {
			if got := rec.query(key); got != want {
				t.Errorf("%s is %q, want %q", key, got, want)
			}
		}
	}
	if !asked {
		t.Error("the commit range was never asked for")
	}
}

func TestCompareOfACommitWithItselfIsEmpty(t *testing.T) {
	s := newStub(t)
	c, _ := s.client()

	diff, err := c.Compare(context.Background(), testRef(), "same", "same")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(diff.Files) != 0 {
		t.Errorf("comparing a commit with itself found %d files", len(diff.Files))
	}
	if len(s.calls) != 0 {
		t.Errorf("comparing a commit with itself made %d requests", len(s.calls))
	}
}

// TestACommitThatIsGoneIsNotAFailure covers what happens after a force push:
// the older commit no longer exists, and reviewing the whole diff instead is
// correct, only more expensive.
func TestACommitThatIsGoneIsNotAFailure(t *testing.T) {
	s := newStub(t)
	s.status["GET "+repoBase+"/diffs/commits"] = http.StatusNotFound
	c, _ := s.client()

	_, err := c.Compare(context.Background(), testRef(), "gone", "head")
	if !errors.Is(err, forge.ErrNoCompare) {
		t.Fatalf("Compare returned %v, want forge.ErrNoCompare", err)
	}
}

func TestCompareWithoutBothEndsIsNotAComparison(t *testing.T) {
	s := newStub(t)
	c, _ := s.client()

	for _, tc := range [][2]string{{"", "head"}, {"base", ""}} {
		if _, err := c.Compare(context.Background(), testRef(), tc[0], tc[1]); !errors.Is(err, forge.ErrNoCompare) {
			t.Errorf("Compare(%q, %q) returned %v, want forge.ErrNoCompare", tc[0], tc[1], err)
		}
	}
}

func TestDiffCapsTheNumberOfFiles(t *testing.T) {
	s := newStub(t)

	changes := make([]map[string]any, 0, 5)
	for i := 0; i < 5; i++ {
		changes = append(changes, map[string]any{
			"changeType": "add",
			"item":       map[string]any{"path": fmt.Sprintf("/f%d.txt", i), "objectId": "n"},
		})
	}
	s.iteration(1, "base111", changes...)
	s.blob("n", "x\n")
	c, _ := s.clientWith(2)

	diff, err := c.Diff(context.Background(), testRef())
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(diff.Files) != 2 {
		t.Errorf("the diff has %d files, want the cap of 2", len(diff.Files))
	}
	if !diff.Truncated {
		t.Error("a capped diff did not say it was truncated")
	}
}

// TestTheReviewerAcceptsFindingsOnTheComputedPatch is the end of the chain.
// The patch is computed here precisely so that reviewer.NewPositions has
// something to check findings against; if the two disagree about which lines
// changed, every finding on this platform is silently dropped.
func TestTheReviewerAcceptsFindingsOnTheComputedPatch(t *testing.T) {
	s := newStub(t)
	s.iteration(1, "base111", edit("/svc.go", "o", "n"))
	s.blob("o", "package svc\n\nfunc A() {}\nfunc B() {}\n")
	s.blob("n", "package svc\n\nfunc A() { panic(\"x\") }\nfunc B() {}\nfunc C() {}\n")
	c, _ := s.client()

	diff, err := c.Diff(context.Background(), testRef())
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	positions := reviewer.NewPositions(diff)
	if positions.Files() != 1 {
		t.Fatalf("the reviewer found %d files with commentable lines, want 1", positions.Files())
	}
	// Line 3 is the changed function, line 5 the added one.
	for _, line := range []int{3, 5} {
		if !positions.Allows("svc.go", line) {
			t.Errorf("a finding on svc.go:%d would be dropped; the patch is:\n%s", line, diff.Files[0].Patch)
		}
	}
	if positions.Allows("svc.go", 99) {
		t.Error("a finding past the end of the file was accepted")
	}
}
