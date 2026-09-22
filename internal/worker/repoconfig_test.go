package worker_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/policy"
	"github.com/yteraoka/kibitz/internal/repoconfig"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/reviewer/opencode"
)

func withConfig(f *fakeForge, yaml string) *fakeForge {
	f.files = map[string]string{repoconfig.Path: yaml}
	return f
}

// The file is read from the default branch, not from the pull request, which
// is what keeps a pull request from deciding how it gets reviewed.
func TestSettingsAreReadFromTheDefaultBranch(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := withConfig(defaultForge(), "review:\n  language: English\n")
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}

	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !slices.Contains(f.filesRead, repoconfig.Path) {
		t.Fatalf("files read = %v, want %s among them", f.filesRead, repoconfig.Path)
	}
	if got := e.requests[0].Language; got != "English" {
		t.Errorf("Language = %q, want the repository's", got)
	}
}

// A repository can turn kibitz off without uninstalling it.
func TestSettingsCanDisableReviews(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := withConfig(defaultForge(), "review:\n  enabled: false\n")
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}

	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(e.requests) != 0 {
		t.Errorf("the agent ran although reviews are disabled: %d requests", len(e.requests))
	}
	if len(f.summaries) != 0 || len(f.reviews) != 0 {
		t.Error("something was posted although reviews are disabled")
	}
}

func TestSettingsLimitTriggers(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := withConfig(defaultForge(), "review:\n  triggers: [pr_opened]\n")
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}
	job := newJob(t, f, e)

	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPRUpdated, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(e.requests) != 0 {
		t.Fatalf("pr_updated was reviewed although only pr_opened is listed")
	}

	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(e.requests) != 1 {
		t.Errorf("pr_opened was not reviewed")
	}
}

// Excluded files must not reach the agent, and must not be commentable
// either: the findings are checked against the same diff the agent saw.
func TestSettingsExcludePaths(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := withConfig(defaultForge(), "review:\n  paths_ignore:\n    - \"**/*.md\"\n")
	f.diff.Files = append(f.diff.Files, forgeFile("docs/worker.md"), forgeFile("README.md"))

	e := &fakeEngine{results: []*reviewer.Result{reviewResult(
		reviewer.OutputComment{Path: "queue.go", Line: 2, Severity: "high", Title: "ok", Body: "in the diff"},
		reviewer.OutputComment{Path: "README.md", Line: 2, Severity: "high", Title: "no", Body: "excluded"},
	)}}

	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	for _, file := range e.requests[0].Diff.Files {
		if strings.HasSuffix(file.Path, ".md") {
			t.Errorf("%s reached the agent although it is excluded", file.Path)
		}
	}
	if len(f.reviews[0].Comments) != 1 || f.reviews[0].Comments[0].Path != "queue.go" {
		t.Errorf("comments = %+v, want only the one in a reviewed file", f.reviews[0].Comments)
	}
	if !strings.Contains(f.summaries[0], "除外: 2 件") {
		t.Errorf("the summary does not say what was excluded:\n%s", f.summaries[0])
	}
}

// A settings file nobody can parse is not a reason to skip the review, but it
// is a reason to say so where the person who can fix it will see it.
func TestBrokenSettingsFallBackAndSaySo(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := withConfig(defaultForge(), "review:\n  min_severity: urgent\n")
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}

	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(e.requests) != 1 {
		t.Fatalf("the review was skipped over a bad settings file")
	}
	if !strings.Contains(f.summaries[0], "min_severity") {
		t.Errorf("the summary does not name the problem:\n%s", f.summaries[0])
	}
}

// A repository that cannot be read at all is the deployment's problem, not the
// pull request's.
func TestUnreadableSettingsDoNotFailTheJob(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	f.fileErr = errors.New("500 from the forge")
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}

	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(e.requests) != 1 {
		t.Fatalf("the review was skipped because the settings could not be read")
	}
	if !strings.Contains(f.summaries[0], repoconfig.Path) {
		t.Errorf("the summary does not mention the unreadable settings:\n%s", f.summaries[0])
	}
}

// What a review concentrates on comes from the repository, and from the
// command when one asked.
func TestFocusReachesThePrompt(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := withConfig(defaultForge(), "review:\n  focus: [correctness]\n")
	e := &fakeEngine{results: []*reviewer.Result{reviewResult(), reviewResult()}}
	job := newJob(t, f, e)

	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := e.requests[0].Focus; len(got) != 1 || got[0] != "correctness" {
		t.Errorf("Focus = %v, want the repository's", got)
	}

	// The command applies to its own run only, and overrides the standing
	// setting rather than adding to it.
	ev := pullRequestEvent(event.KindCommand, origin)
	ev.Command = &event.Command{Name: policy.CommandReview, Args: []string{"--focus", "security,performance"}}
	ev.ID = "github:d2"
	ev.Source.DeliveryID = "d2"

	if err := job.Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := e.requests[1].Focus; len(got) != 2 || got[0] != "security" || got[1] != "performance" {
		t.Errorf("Focus = %v, want the command's", got)
	}
}

// forgeFile is a changed file with a patch the reviewer can anchor to.
func forgeFile(path string) forge.File {
	return forge.File{
		Path:   path,
		Status: forge.FileModified,
		Patch:  "@@ -1,2 +1,3 @@\n context\n+added\n+more",
	}
}

// A repository turns on the servers it wants by name, and gets only what the
// deployment offers.
func TestMCPServersFromTheRepository(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := withConfig(defaultForge(), "mcp:\n  allow: [jira, confluence]\n")
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}

	job := newJob(t, f, e)
	job.MCP = opencode.Catalog{
		"jira":   {Type: opencode.MCPRemote, URL: "https://jira.example.com/mcp"},
		"sentry": {Type: opencode.MCPLocal, Command: []string{"sentry-mcp"}},
	}

	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got := e.requests[0].MCP
	if len(got) != 1 || got[0] != "jira" {
		t.Errorf("MCP = %v, want only the one this deployment offers", got)
	}
	// Named and not delivered: the repository is told rather than left to
	// wonder why the review never mentions its tickets.
	if !strings.Contains(f.summaries[0], "confluence") {
		t.Errorf("the summary does not name the server that was refused:\n%s", f.summaries[0])
	}
	// And a server the repository did not ask for stays off.
	if strings.Contains(strings.Join(got, ","), "sentry") {
		t.Errorf("MCP = %v, want nothing the repository did not ask for", got)
	}
}

// Without a .kibitz.yaml nothing is enabled, however much the deployment
// offers: tool definitions cost tokens on every call.
func TestNoMCPWithoutTheRepositoryAsking(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}

	job := newJob(t, f, e)
	job.MCP = opencode.Catalog{"jira": {Type: opencode.MCPRemote, URL: "https://jira.example.com/mcp"}}

	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := e.requests[0].MCP; len(got) != 0 {
		t.Errorf("MCP = %v, want none", got)
	}
}
