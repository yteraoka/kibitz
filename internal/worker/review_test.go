package worker_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/store/memory"
	"github.com/yteraoka/kibitz/internal/worker"
	"github.com/yteraoka/kibitz/internal/workspace"
)

// fakeForge records what was posted and serves a fixed pull request.
type fakeForge struct {
	pr       *event.PullRequest
	diff     *forge.Diff
	comments []forge.Comment

	prErr     error
	reviewErr error

	summaries []string
	reviews   []forge.Review
	replies   []string
}

func (f *fakeForge) Platform() event.Platform { return event.PlatformGitHub }

func (f *fakeForge) PullRequest(context.Context, forge.PRRef) (*event.PullRequest, error) {
	if f.prErr != nil {
		return nil, f.prErr
	}
	return f.pr, nil
}

func (f *fakeForge) Diff(context.Context, forge.PRRef) (*forge.Diff, error) { return f.diff, nil }

func (f *fakeForge) Comments(context.Context, forge.PRRef) ([]forge.Comment, error) {
	return f.comments, nil
}

func (f *fakeForge) CreateReview(_ context.Context, _ forge.PRRef, r forge.Review) error {
	if f.reviewErr != nil {
		return f.reviewErr
	}
	f.reviews = append(f.reviews, r)
	return nil
}

func (f *fakeForge) UpsertSummary(_ context.Context, _ forge.PRRef, _, body string) error {
	f.summaries = append(f.summaries, body)
	return nil
}

func (f *fakeForge) ReplyToThread(_ context.Context, _ forge.PRRef, _, body string) error {
	f.replies = append(f.replies, body)
	return nil
}

func (f *fakeForge) CloneAuth(context.Context, forge.PRRef) (forge.CloneCredential, error) {
	return forge.CloneCredential{Username: "x-access-token", Token: "t"}, nil
}

// fakeEngine returns canned results and records the requests it was given.
type fakeEngine struct {
	results  []*reviewer.Result
	errs     []error
	requests []reviewer.Request
}

func (e *fakeEngine) Run(_ context.Context, req reviewer.Request) (*reviewer.Result, error) {
	e.requests = append(e.requests, req)
	i := len(e.requests) - 1

	if i < len(e.errs) && e.errs[i] != nil {
		return nil, e.errs[i]
	}
	if i < len(e.results) {
		return e.results[i], nil
	}
	return nil, errors.New("fakeEngine: no result configured")
}

func reviewResult(comments ...reviewer.OutputComment) *reviewer.Result {
	out := &reviewer.Output{
		SchemaVersion: reviewer.OutputSchemaVersion,
		Summary:       "SQS subscriber を追加する変更",
		Comments:      comments,
	}
	return &reviewer.Result{Summary: out.Summary, RawOutput: out}
}

// queued wraps an event the way the worker does when it takes one off the
// queue.
func queued(ev *event.ReviewEvent) *worker.Job { return &worker.Job{Event: ev, Deliveries: 1} }

func pullRequestEvent(kind event.Kind, origin string) *event.ReviewEvent {
	return &event.ReviewEvent{
		SchemaVersion: event.SchemaVersion,
		ID:            "github:d1",
		Source:        event.Source{Platform: event.PlatformGitHub, DeliveryID: "d1"},
		Kind:          kind,
		Repository: event.Repository{
			Owner: "yteraoka", Name: "kibitz", FullName: "yteraoka/kibitz",
			CloneURL: origin,
		},
		PullRequest: &event.PullRequest{Number: 42},
		Actor:       event.Actor{Login: "yteraoka"},
	}
}

func newJob(t *testing.T, f *fakeForge, e *fakeEngine) *worker.ReviewJob {
	t.Helper()
	return &worker.ReviewJob{
		Forges:    map[event.Platform]forge.Client{event.PlatformGitHub: f},
		Engine:    e,
		Workspace: workspace.Config{Root: t.TempDir(), Depth: 1},
		Limits:    reviewer.Limits{MaxComments: 10},
		Logger:    discardLogger(),
		Language:  "日本語",
	}
}

func defaultForge() *fakeForge {
	return &fakeForge{
		pr: &event.PullRequest{
			Number: 42, Title: "Add the SQS subscriber", State: "open",
			Target: event.Ref{Branch: "main"},
		},
		diff: &forge.Diff{Files: []forge.File{
			{Path: "queue.go", Status: forge.FileModified, Patch: "@@ -1,2 +1,3 @@\n context\n+added\n+more"},
		}},
	}
}

func TestReviewPostsSummaryAndFindings(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{reviewResult(
		reviewer.OutputComment{Path: "queue.go", Line: 2, Severity: "high", Title: "漏れる", Body: "ctx を見ていない"},
	)}}

	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(f.summaries) != 1 {
		t.Fatalf("%d summaries posted, want 1", len(f.summaries))
	}
	if !strings.Contains(f.summaries[0], "指摘: 1 件") {
		t.Errorf("summary does not report the finding count:\n%s", f.summaries[0])
	}
	if len(f.reviews) != 1 || len(f.reviews[0].Comments) != 1 {
		t.Fatalf("reviews = %+v", f.reviews)
	}
	if got := f.reviews[0].Comments[0]; got.Path != "queue.go" || got.Line != 2 {
		t.Errorf("comment position = %s:%d", got.Path, got.Line)
	}
	// The review is anchored to the commit that was actually checked out.
	if f.reviews[0].CommitSHA == "" {
		t.Error("the review is not anchored to a commit")
	}
}

// A finding on a line outside the diff would make the forge reject the whole
// review, so it is dropped and accounted for in the summary.
func TestReviewDropsFindingsOutsideTheDiff(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{reviewResult(
		reviewer.OutputComment{Path: "queue.go", Line: 2, Severity: "high", Title: "ok", Body: "in the diff"},
		reviewer.OutputComment{Path: "queue.go", Line: 900, Severity: "high", Title: "no", Body: "not in the diff"},
		reviewer.OutputComment{Path: "untouched.go", Line: 1, Severity: "high", Title: "no", Body: "not in the diff"},
	)}}

	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(f.reviews[0].Comments) != 1 {
		t.Fatalf("%d comments posted, want 1", len(f.reviews[0].Comments))
	}
	if !strings.Contains(f.summaries[0], "2 件の指摘は差分に含まれない行") {
		t.Errorf("the summary does not account for the dropped findings:\n%s", f.summaries[0])
	}
}

func TestReviewWithNoFindingsStillPostsASummary(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}

	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.summaries) != 1 || !strings.Contains(f.summaries[0], "指摘はありません") {
		t.Errorf("summaries = %v", f.summaries)
	}
	if len(f.reviews) != 0 {
		t.Errorf("%d reviews posted, want none when there is nothing to say", len(f.reviews))
	}
}

// Output that breaks the contract earns one retry, with the reason fed back.
func TestReviewRetriesOnceOnMalformedOutput(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{
		errs: []error{&reviewer.OutputError{Err: errors.New("summary is empty")}},
		results: []*reviewer.Result{nil, reviewResult(
			reviewer.OutputComment{Path: "queue.go", Line: 2, Severity: "high", Title: "ok", Body: "b"},
		)},
	}

	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(e.requests) != 2 {
		t.Fatalf("%d engine runs, want 2", len(e.requests))
	}
	if !strings.Contains(e.requests[1].Feedback, "summary is empty") {
		t.Errorf("the retry does not tell the agent what was wrong: %q", e.requests[1].Feedback)
	}
	if len(f.reviews) != 1 {
		t.Errorf("%d reviews posted, want 1", len(f.reviews))
	}
}

func TestReviewFailsAfterASecondBadOutput(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{errs: []error{
		&reviewer.OutputError{Err: errors.New("summary is empty")},
		&reviewer.OutputError{Err: errors.New("still empty")},
	}}

	err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin)))
	if err == nil {
		t.Fatal("Handle succeeded although the agent never produced usable output")
	}
	if len(f.summaries) != 0 {
		t.Error("a summary was posted despite the failure")
	}
}

// Losing every finding because one position was rejected would be the worst
// outcome, so they are posted as text instead.
func TestReviewFallsBackWhenPositionsAreRejected(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	f.reviewErr = fmt.Errorf("%w: 422", forge.ErrInvalidPosition)
	e := &fakeEngine{results: []*reviewer.Result{reviewResult(
		reviewer.OutputComment{Path: "queue.go", Line: 2, Severity: "high", Title: "漏れる", Body: "ctx を見ていない"},
	)}}

	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(f.summaries) != 2 {
		t.Fatalf("%d summaries posted, want the fallback", len(f.summaries))
	}
	if !strings.Contains(f.summaries[1], "漏れる") {
		t.Errorf("the fallback summary does not carry the findings:\n%s", f.summaries[1])
	}
}

func TestReviewSkipsClosedAndDraft(t *testing.T) {
	origin, _, _ := originRepo(t)

	t.Run("closed", func(t *testing.T) {
		f := defaultForge()
		f.pr.State = "closed"
		e := &fakeEngine{}

		if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if len(e.requests) != 0 {
			t.Error("a closed pull request was reviewed")
		}
	})

	t.Run("draft", func(t *testing.T) {
		f := defaultForge()
		f.pr.Draft = true
		e := &fakeEngine{}

		job := newJob(t, f, e)
		job.SkipDraft = true
		if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if len(e.requests) != 0 {
			t.Error("a draft pull request was reviewed although drafts are skipped")
		}
	})

	t.Run("draft reviewed on request", func(t *testing.T) {
		f := defaultForge()
		f.pr.Draft = true
		e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}

		job := newJob(t, f, e)
		job.SkipDraft = true

		ev := pullRequestEvent(event.KindCommand, origin)
		ev.Command = &event.Command{Name: "review"}
		ev.Comment = &event.Comment{ID: "1", Body: "@kibitz review"}

		if err := job.Handle(context.Background(), queued(ev)); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if len(e.requests) != 1 {
			t.Error("an explicit review command on a draft was ignored")
		}
	})
}

func TestReviewSkipsEmptyDiff(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	f.diff = &forge.Diff{}
	e := &fakeEngine{}

	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(e.requests) != 0 {
		t.Error("a pull request with no changed files was reviewed")
	}
}

// Fetching the pull request failing is a transient condition: the job fails so
// the message is redelivered.
func TestReviewFailsWhenThePullRequestCannotBeRead(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	f.prErr = errors.New("503 from the API")

	err := newJob(t, f, &fakeEngine{}).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin)))
	if err == nil {
		t.Fatal("Handle succeeded although the pull request could not be read")
	}
}

func TestAnswerRepliesInTheThread(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{{Reply: "ロックの期限が切れるためです。"}}}

	ev := pullRequestEvent(event.KindCommentCreated, origin)
	ev.Comment = &event.Comment{ID: "9", ThreadID: "7", Body: "@kibitz なぜ競合するのですか?"}

	if err := newJob(t, f, e).Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.replies) != 1 || !strings.Contains(f.replies[0], "ロック") {
		t.Fatalf("replies = %v", f.replies)
	}
	if e.requests[0].Mode != reviewer.ModeAnswer {
		t.Errorf("mode = %s, want answer", e.requests[0].Mode)
	}
	if e.requests[0].Question == "" {
		t.Error("the question was not passed to the engine")
	}
}

// kibitz's own summary must not come back as context, or it would treat its
// previous findings as another reviewer's.
func TestReviewExcludesItsOwnComments(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	f.comments = []forge.Comment{
		{ID: "1", Body: worker.SummaryMarker + "\nprevious review"},
		{ID: "2", Body: "a human's comment"},
	}
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}

	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	existing := e.requests[0].ExistingComments
	if len(existing) != 1 || existing[0].ID != "2" {
		t.Errorf("existing comments = %+v, want only the human's", existing)
	}
}

func TestHandleUnknownPlatformIsAcknowledged(t *testing.T) {
	origin, _, _ := originRepo(t)
	ev := pullRequestEvent(event.KindPROpened, origin)
	ev.Source.Platform = event.PlatformGitLab

	job := newJob(t, defaultForge(), &fakeEngine{})
	if err := job.Handle(context.Background(), queued(ev)); err != nil {
		t.Errorf("Handle = %v, want nil: retrying will not add a client", err)
	}
}

func TestHandleClosedEventDoesNothing(t *testing.T) {
	origin, _, _ := originRepo(t)
	e := &fakeEngine{}

	if err := newJob(t, defaultForge(), e).Handle(context.Background(), queued(pullRequestEvent(event.KindPRMerged, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(e.requests) != 0 {
		t.Error("a merged pull request was reviewed")
	}
}

func TestReviewSkipsACommitItAlreadyReviewed(t *testing.T) {
	origin, headSHA, _ := originRepo(t)
	f := defaultForge()
	f.pr.Source.SHA = headSHA
	e := &fakeEngine{results: []*reviewer.Result{reviewResult(), reviewResult()}}

	j := newJob(t, f, e)
	j.Store = memory.New()

	ev := pullRequestEvent(event.KindPRUpdated, origin)
	if err := j.Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// A different delivery for the same commit: nothing changed, so there is
	// nothing new to say.
	second := pullRequestEvent(event.KindPRUpdated, origin)
	second.Source.DeliveryID = "d2"
	if err := j.Handle(context.Background(), queued(second)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(e.requests) != 1 {
		t.Errorf("the agent ran %d times, want 1 for one commit", len(e.requests))
	}
}

// An explicit command means "review it again", even for a commit that was
// already reviewed.
func TestCommandReviewsAnAlreadyReviewedCommit(t *testing.T) {
	origin, headSHA, _ := originRepo(t)
	f := defaultForge()
	f.pr.Source.SHA = headSHA
	e := &fakeEngine{results: []*reviewer.Result{reviewResult(), reviewResult()}}

	j := newJob(t, f, e)
	j.Store = memory.New()

	if err := j.Handle(context.Background(), queued(pullRequestEvent(event.KindPRUpdated, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	cmd := pullRequestEvent(event.KindCommand, origin)
	cmd.Source.DeliveryID = "d2"
	cmd.Command = &event.Command{Name: "review"}
	cmd.Comment = &event.Comment{ID: "1", Body: "@kibitz review"}
	if err := j.Handle(context.Background(), queued(cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(e.requests) != 2 {
		t.Errorf("the agent ran %d times, want 2: a command forces a re-review", len(e.requests))
	}
}

// If kibitz ever starts reacting to itself, the hourly cap stops it before the
// thread fills up.
func TestPostingIsCapped(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()

	results := make([]*reviewer.Result, 6)
	for i := range results {
		results[i] = reviewResult()
	}
	e := &fakeEngine{results: results}

	j := newJob(t, f, e)
	j.Store = memory.New()
	j.MaxPostsPerHour = 2

	for i := range 4 {
		ev := pullRequestEvent(event.KindCommand, origin)
		ev.Source.DeliveryID = fmt.Sprintf("d%d", i)
		ev.Command = &event.Command{Name: "review"}
		ev.Comment = &event.Comment{ID: "1", Body: "@kibitz review"}

		if err := j.Handle(context.Background(), queued(ev)); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}

	if len(f.summaries) != 2 {
		t.Errorf("%d comments posted, want the cap of 2", len(f.summaries))
	}
}

func TestNotifyFailurePostsToThePullRequest(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	j := newJob(t, f, &fakeEngine{})

	ev := pullRequestEvent(event.KindPROpened, origin)
	if err := j.NotifyFailure(context.Background(), ev, errors.New("the agent timed out")); err != nil {
		t.Fatalf("NotifyFailure: %v", err)
	}

	if len(f.summaries) != 1 {
		t.Fatalf("%d comments posted, want 1", len(f.summaries))
	}
	if !strings.Contains(f.summaries[0], "the agent timed out") {
		t.Errorf("the notice does not say what happened:\n%s", f.summaries[0])
	}
	if !strings.Contains(f.summaries[0], "@kibitz review") {
		t.Error("the notice does not say how to retry")
	}
}
