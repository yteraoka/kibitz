package worker_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/policy"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/store"
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

	summaries  []string
	reviews    []forge.Review
	replies    []string
	compared   []string
	partial    *forge.Diff
	compareErr error
}

func (f *fakeForge) Platform() event.Platform { return event.PlatformGitHub }

func (f *fakeForge) PullRequest(context.Context, forge.PRRef) (*event.PullRequest, error) {
	if f.prErr != nil {
		return nil, f.prErr
	}
	return f.pr, nil
}

func (f *fakeForge) Diff(context.Context, forge.PRRef) (*forge.Diff, error) { return f.diff, nil }

func (f *fakeForge) Compare(_ context.Context, _ forge.PRRef, base, head string) (*forge.Diff, error) {
	f.compared = append(f.compared, base+"..."+head)
	if f.compareErr != nil {
		return nil, f.compareErr
	}
	if f.partial != nil {
		return f.partial, nil
	}
	return &forge.Diff{}, nil
}

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
	if !strings.Contains(f.summaries[0], policy.DefaultMention+" review") {
		t.Error("the notice does not say how to retry")
	}
}

// The help text is the only place kibitz explains itself, so it has to
// explain *this* deployment: a worker told that comments address it as
// "/kibitz" must not tell people to write "@kibitz".
func TestHelpUsesTheConfiguredMention(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()

	job := newJob(t, f, &fakeEngine{})
	job.Mention = "/kibitz"

	ev := pullRequestEvent(event.KindCommand, origin)
	ev.Command = &event.Command{Name: policy.CommandHelp}

	if err := job.Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.summaries) != 1 {
		t.Fatalf("%d summaries posted, want 1", len(f.summaries))
	}

	help := f.summaries[0]
	if !strings.Contains(help, "/kibitz review") {
		t.Errorf("help does not use the configured mention:\n%s", help)
	}
	if strings.Contains(help, "@kibitz") {
		t.Errorf("help still names the default mention:\n%s", help)
	}
	// And it states the rule that decides whether a comment acts or answers.
	if !strings.Contains(help, "先頭") {
		t.Errorf("help does not say where a command has to go:\n%s", help)
	}
}

// Left unset, it is still true for a default deployment.
func TestHelpFallsBackToTheDefaultMention(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()

	ev := pullRequestEvent(event.KindCommand, origin)
	ev.Command = &event.Command{Name: policy.CommandHelp}

	if err := newJob(t, f, &fakeEngine{}).Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.summaries) != 1 {
		t.Fatalf("%d summaries posted, want 1", len(f.summaries))
	}
	if !strings.Contains(f.summaries[0], policy.DefaultMention+" review") {
		t.Errorf("help does not use the default mention:\n%s", f.summaries[0])
	}
}

// The failure notice tells people how to retry, so it has to name the token
// this deployment actually answers to.
func TestFailureNoticeUsesTheConfiguredMention(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()

	job := newJob(t, f, &fakeEngine{})
	job.Mention = "/kibitz"

	ev := pullRequestEvent(event.KindPROpened, origin)
	if err := job.NotifyFailure(context.Background(), ev, errors.New("opencode: exit status 1")); err != nil {
		t.Fatalf("NotifyFailure: %v", err)
	}
	if len(f.summaries) != 1 {
		t.Fatalf("%d summaries posted, want 1", len(f.summaries))
	}

	notice := f.summaries[0]
	if !strings.Contains(notice, "/kibitz review") {
		t.Errorf("notice does not say how to retry:\n%s", notice)
	}
	if strings.Contains(notice, "@kibitz") {
		t.Errorf("notice names a mention this deployment ignores:\n%s", notice)
	}
	if !strings.Contains(notice, "opencode: exit status 1") {
		t.Errorf("notice does not carry the cause:\n%s", notice)
	}
}

// A reply of "why?" means nothing without what it is replying to, and what it
// is replying to is usually one of kibitz's own findings.
func TestAnswerCarriesTheThread(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	f.comments = []forge.Comment{
		{ID: "1", ThreadID: "7", Body: "ここで ctx を見ていません", Author: event.Actor{Login: "kibitz[bot]"}, Path: "queue.go", Line: 88},
		{ID: "5", ThreadID: "7", Body: "なぜ問題になりますか?", Author: event.Actor{Login: "yteraoka"}},
		{ID: "6", ThreadID: "other", Body: "無関係なスレッド", Author: event.Actor{Login: "someone"}},
		{ID: "9", ThreadID: "7", Body: "@kibitz なぜ競合するのですか?", Author: event.Actor{Login: "yteraoka"}},
	}
	e := &fakeEngine{results: []*reviewer.Result{{Reply: "ロックの期限が切れるためです。"}}}

	ev := pullRequestEvent(event.KindCommentCreated, origin)
	ev.Comment = &event.Comment{ID: "9", ThreadID: "7", Body: "@kibitz なぜ競合するのですか?"}

	if err := newJob(t, f, e).Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	thread := e.requests[0].Thread
	if len(thread) != 2 {
		t.Fatalf("thread = %+v, want the two comments of this thread", thread)
	}
	// kibitz's own finding is context here, unlike in a review.
	if thread[0].ID != "1" || thread[1].ID != "5" {
		t.Errorf("thread = %+v, want it oldest first without the question", thread)
	}
}

// A comment on the conversation has no thread of its own, so the conversation
// is the thread.
func TestAnswerWithoutAThreadUsesTheConversation(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	f.comments = []forge.Comment{
		{ID: "1", Body: "最初のコメント", Author: event.Actor{Login: "yteraoka"}},
		{ID: "9", Body: "@kibitz これは何をしていますか?", Author: event.Actor{Login: "yteraoka"}},
	}
	e := &fakeEngine{results: []*reviewer.Result{{Reply: "キューを読んでいます。"}}}

	ev := pullRequestEvent(event.KindCommentCreated, origin)
	ev.Comment = &event.Comment{ID: "9", Body: "@kibitz これは何をしていますか?"}

	if err := newJob(t, f, e).Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := e.requests[0].Thread; len(got) != 1 || got[0].ID != "1" {
		t.Errorf("thread = %+v, want the rest of the conversation", got)
	}
}

// The review opens the conversation a later question continues.
func TestReviewRemembersItsSession(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	s := memory.New()
	e := &fakeEngine{results: []*reviewer.Result{
		{Summary: "問題なし", SessionID: "ses_review", RawOutput: &reviewer.Output{SchemaVersion: 1, Summary: "問題なし"}},
		{Reply: "こうです。"},
	}}

	job := newJob(t, f, e)
	job.Store = s

	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("review: %v", err)
	}

	ev := pullRequestEvent(event.KindCommentCreated, origin)
	ev.Comment = &event.Comment{ID: "9", ThreadID: "7", Body: "@kibitz なぜ?"}
	if err := job.Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("answer: %v", err)
	}

	if got := e.requests[1].SessionID; got != "ses_review" {
		t.Errorf("session = %q, want the one the review opened", got)
	}
}

func TestIgnoreStopsReviewsButNotAnswers(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	s := memory.New()
	e := &fakeEngine{results: []*reviewer.Result{{Reply: "答えます。"}}}

	job := newJob(t, f, e)
	job.Store = s

	ignore := pullRequestEvent(event.KindCommand, origin)
	ignore.Command = &event.Command{Name: policy.CommandIgnore}
	if err := job.Handle(context.Background(), queued(ignore)); err != nil {
		t.Fatalf("ignore: %v", err)
	}
	if len(f.summaries) != 1 || !strings.Contains(f.summaries[0], "レビューしません") {
		t.Fatalf("summaries = %v, want the request acknowledged", f.summaries)
	}

	// A push is now ignored.
	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPRUpdated, origin))); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(f.reviews) != 0 {
		t.Errorf("reviews = %+v, want none while ignored", f.reviews)
	}

	// Questions are still answered: being told to stop reviewing is not being
	// told to stop talking.
	ev := pullRequestEvent(event.KindCommentCreated, origin)
	ev.Comment = &event.Comment{ID: "9", Body: "@kibitz これは?"}
	if err := job.Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if len(f.replies) != 1 {
		t.Errorf("replies = %v, want the question answered", f.replies)
	}
}

// Asking for a review is asking to be reviewed again.
func TestReviewCommandLiftsIgnore(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	s := memory.New()
	e := &fakeEngine{results: []*reviewer.Result{
		{Summary: "見ました", RawOutput: &reviewer.Output{SchemaVersion: 1, Summary: "見ました"}},
	}}

	job := newJob(t, f, e)
	job.Store = s

	ignore := pullRequestEvent(event.KindCommand, origin)
	ignore.Command = &event.Command{Name: policy.CommandIgnore}
	if err := job.Handle(context.Background(), queued(ignore)); err != nil {
		t.Fatalf("ignore: %v", err)
	}

	review := pullRequestEvent(event.KindCommand, origin)
	review.Command = &event.Command{Name: policy.CommandReview}
	if err := job.Handle(context.Background(), queued(review)); err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(e.requests) != 1 || e.requests[0].Mode != reviewer.ModeReview {
		t.Fatalf("requests = %+v, want the review to have run", e.requests)
	}
}

// Closing the pull request ends the conversation.
func TestCloseForgetsThePullRequest(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	s := memory.New()

	job := newJob(t, f, &fakeEngine{})
	job.Store = s

	ignore := pullRequestEvent(event.KindCommand, origin)
	ignore.Command = &event.Command{Name: policy.CommandIgnore}
	if err := job.Handle(context.Background(), queued(ignore)); err != nil {
		t.Fatalf("ignore: %v", err)
	}

	closed := pullRequestEvent(event.KindPRMerged, origin)
	if err := job.Handle(context.Background(), queued(closed)); err != nil {
		t.Fatalf("merged: %v", err)
	}

	if _, err := s.Get(context.Background(), store.IgnoreKey(closed)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the ignore flag survived the pull request being merged (err = %v)", err)
	}
}

// A second review of the same pull request looks at what was pushed since the
// first one, not at the whole thing again.
func TestReviewIsIncrementalAfterTheFirstOne(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	f.partial = &forge.Diff{Files: []forge.File{
		{Path: "queue.go", Status: forge.FileModified, Patch: "@@ -1,2 +1,3 @@\n context\n+new"},
	}}
	s := memory.New()
	e := &fakeEngine{results: []*reviewer.Result{
		{Summary: "1 回目", RawOutput: &reviewer.Output{SchemaVersion: 1, Summary: "1 回目"}},
		{Summary: "2 回目", RawOutput: &reviewer.Output{SchemaVersion: 1, Summary: "2 回目"}},
	}}

	job := newJob(t, f, e)
	job.Store = s

	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("first review: %v", err)
	}
	// The first review has nothing to compare against.
	if len(f.compared) != 0 {
		t.Fatalf("compared = %v, want none on the first review", f.compared)
	}
	if e.requests[0].SinceSHA != "" {
		t.Errorf("since = %q, want empty on the first review", e.requests[0].SinceSHA)
	}

	// A push moves the head, so the second review compares.
	f.pr.Source.SHA = "newhead"
	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPRUpdated, origin))); err != nil {
		t.Fatalf("second review: %v", err)
	}
	if len(f.compared) != 1 {
		t.Fatalf("compared = %v, want one comparison", f.compared)
	}
	if got := e.requests[1].SinceSHA; got == "" {
		t.Error("the second review did not say what it was measured from")
	}
	if got := e.requests[1].Diff; got == nil || len(got.Files) != 1 || got.Files[0].Patch != f.partial.Files[0].Patch {
		t.Errorf("diff = %+v, want the incremental one", got)
	}
}

// Reviewing too much is a cost; reviewing too little is a missed bug. Every
// way of failing to narrow the diff falls back to the whole thing.
func TestReviewFallsBackToTheWholeDiff(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*fakeForge, *event.ReviewEvent)
	}{
		{
			name: "the commits cannot be compared",
			prepare: func(f *fakeForge, _ *event.ReviewEvent) {
				f.compareErr = forge.ErrNoCompare
			},
		},
		{
			name:    "the comparison is empty",
			prepare: func(f *fakeForge, _ *event.ReviewEvent) { f.partial = &forge.Diff{} },
		},
		{
			name: "an explicit --full",
			prepare: func(f *fakeForge, ev *event.ReviewEvent) {
				f.partial = &forge.Diff{Files: []forge.File{{Path: "queue.go"}}}
				ev.Kind = event.KindCommand
				ev.Command = &event.Command{Name: policy.CommandReview, Args: []string{"--full"}}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origin, _, _ := originRepo(t)
			f := defaultForge()
			s := memory.New()
			e := &fakeEngine{results: []*reviewer.Result{
				{Summary: "1", RawOutput: &reviewer.Output{SchemaVersion: 1, Summary: "1"}},
				{Summary: "2", RawOutput: &reviewer.Output{SchemaVersion: 1, Summary: "2"}},
			}}

			job := newJob(t, f, e)
			job.Store = s

			if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
				t.Fatalf("first review: %v", err)
			}

			second := pullRequestEvent(event.KindPRUpdated, origin)
			f.pr.Source.SHA = "newhead"
			tc.prepare(f, second)

			if err := job.Handle(context.Background(), queued(second)); err != nil {
				t.Fatalf("second review: %v", err)
			}
			if got := e.requests[1].SinceSHA; got != "" {
				t.Errorf("since = %q, want the whole diff to have been reviewed", got)
			}
			if got := e.requests[1].Diff; got == nil || len(got.Files) != len(f.diff.Files) {
				t.Errorf("diff = %+v, want the whole one", got)
			}
		})
	}
}

// The summary says what was actually looked at.
func TestSummarySaysWhatWasReviewed(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	f.partial = &forge.Diff{Files: []forge.File{{Path: "queue.go", Patch: "@@ -1 +1,2 @@\n a\n+b"}}}
	s := memory.New()
	e := &fakeEngine{results: []*reviewer.Result{
		{Summary: "1", RawOutput: &reviewer.Output{SchemaVersion: 1, Summary: "1"}},
		{Summary: "2", RawOutput: &reviewer.Output{SchemaVersion: 1, Summary: "2"}},
	}}

	job := newJob(t, f, e)
	job.Store = s

	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("first review: %v", err)
	}
	f.pr.Source.SHA = "newhead0000000"
	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPRUpdated, origin))); err != nil {
		t.Fatalf("second review: %v", err)
	}

	if len(f.summaries) != 2 {
		t.Fatalf("%d summaries, want 2", len(f.summaries))
	}
	if !strings.Contains(f.summaries[1], "前回レビューからの差分") {
		t.Errorf("the summary does not say it was incremental:\n%s", f.summaries[1])
	}
}

// bigDiff is a change too large to put in front of a model in one go.
func bigDiff(files int) *forge.Diff {
	d := &forge.Diff{}
	for i := range files {
		d.Files = append(d.Files, forge.File{
			Path:      fmt.Sprintf("file%02d.go", i),
			Status:    forge.FileModified,
			Additions: 100,
			Patch:     "@@ -1 +1,2 @@\n a\n+b",
		})
	}
	return d
}

func TestTriageNarrowsAVeryLargeChange(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	f.diff = bigDiff(20)

	e := &fakeEngine{results: []*reviewer.Result{
		{Triage: &reviewer.TriageOutput{
			SchemaVersion: 1,
			Paths:         []string{"file01.go", "file07.go"},
			Notes:         "生成物を除外しました",
		}},
		{Summary: "見ました", RawOutput: &reviewer.Output{SchemaVersion: 1, Summary: "見ました"}},
	}}

	job := newJob(t, f, e)
	job.MaxDiffLines = 100
	job.TriageModel = "cheap/model"

	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(e.requests) != 2 {
		t.Fatalf("%d engine runs, want a triage pass and a review", len(e.requests))
	}
	if e.requests[0].Mode != reviewer.ModeTriage {
		t.Errorf("first run mode = %s, want triage", e.requests[0].Mode)
	}
	if e.requests[0].Model != "cheap/model" {
		t.Errorf("triage model = %q, want the cheap one", e.requests[0].Model)
	}

	reviewed := e.requests[1].Diff
	if reviewed == nil || len(reviewed.Files) != 2 {
		t.Fatalf("reviewed diff = %+v, want the two selected files", reviewed)
	}
	if reviewed.Files[0].Path != "file01.go" || reviewed.Files[1].Path != "file07.go" {
		t.Errorf("reviewed = %+v, want the selection in diff order", reviewed.Files)
	}

	// The author is told what was not read.
	if len(f.summaries) != 1 {
		t.Fatalf("%d summaries, want 1", len(f.summaries))
	}
	for _, want := range []string{"未レビュー", "生成物を除外しました", "file02.go"} {
		if !strings.Contains(f.summaries[0], want) {
			t.Errorf("the summary does not mention %q:\n%s", want, f.summaries[0])
		}
	}
}

// A change that fits is reviewed whole, with no triage pass at all.
func TestTriageIsSkippedForAnOrdinaryChange(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{
		{Summary: "見ました", RawOutput: &reviewer.Output{SchemaVersion: 1, Summary: "見ました"}},
	}}

	job := newJob(t, f, e)
	job.MaxDiffLines = 10000

	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(e.requests) != 1 || e.requests[0].Mode != reviewer.ModeReview {
		t.Fatalf("runs = %+v, want one review and no triage", e.requests)
	}
	if strings.Contains(f.summaries[0], "未レビュー") {
		t.Errorf("the summary claims files were skipped:\n%s", f.summaries[0])
	}
}

// Reviewing too much wastes tokens; reviewing too little loses findings, so
// every way triage can fail falls back to the whole change.
func TestTriageFallsBackToTheWholeChange(t *testing.T) {
	tests := []struct {
		name   string
		result *reviewer.Result
		err    error
	}{
		{name: "the triage pass failed", err: errors.New("the agent died")},
		{name: "it produced no selection", result: &reviewer.Result{}},
		{
			name:   "it selected files that are not in the diff",
			result: &reviewer.Result{Triage: &reviewer.TriageOutput{SchemaVersion: 1, Paths: []string{"invented.go"}}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origin, _, _ := originRepo(t)
			f := defaultForge()
			f.diff = bigDiff(20)

			e := &fakeEngine{
				results: []*reviewer.Result{tc.result, {Summary: "見ました", RawOutput: &reviewer.Output{SchemaVersion: 1, Summary: "見ました"}}},
				errs:    []error{tc.err},
			}

			job := newJob(t, f, e)
			job.MaxDiffLines = 100

			if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if len(e.requests) != 2 {
				t.Fatalf("%d engine runs, want the review to have run anyway", len(e.requests))
			}
			if got := e.requests[1].Diff; got == nil || len(got.Files) != 20 {
				t.Errorf("reviewed %d files, want the whole change", len(got.Files))
			}
			if strings.Contains(f.summaries[0], "未レビュー") {
				t.Errorf("the summary claims files were skipped:\n%s", f.summaries[0])
			}
		})
	}
}

// The person deciding whether the bot is worth having is the one reading its
// comments, so what it spent goes in the comment.
func TestSummaryReportsTokensAndCost(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{{
		Summary:   "見ました",
		RawOutput: &reviewer.Output{SchemaVersion: 1, Summary: "見ました"},
		Usage:     reviewer.Usage{InputTokens: 123456, OutputTokens: 7890},
	}}}

	prices, err := reviewer.ParsePrices([]string{"test/model=1.25/10"})
	if err != nil {
		t.Fatalf("ParsePrices: %v", err)
	}

	job := newJob(t, f, e)
	job.Model = "test/model"
	job.Prices = prices

	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.summaries) != 1 {
		t.Fatalf("%d summaries, want 1", len(f.summaries))
	}

	summary := f.summaries[0]
	// Grouped, because six figures are not readable otherwise.
	if !strings.Contains(summary, "入力 123,456") || !strings.Contains(summary, "出力 7,890") {
		t.Errorf("the summary does not report the tokens:\n%s", summary)
	}
	// 123456/1e6*1.25 + 7890/1e6*10 = 0.23332
	if !strings.Contains(summary, "$0.233") {
		t.Errorf("the summary does not report the cost:\n%s", summary)
	}
}

// A price nobody configured is not a review that was free.
func TestSummaryReportsTokensWithoutAPrice(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{{
		Summary:   "見ました",
		RawOutput: &reviewer.Output{SchemaVersion: 1, Summary: "見ました"},
		Usage:     reviewer.Usage{InputTokens: 1000, OutputTokens: 100},
	}}}

	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	summary := f.summaries[0]
	if !strings.Contains(summary, "入力 1,000") {
		t.Errorf("the summary does not report the tokens:\n%s", summary)
	}
	if strings.Contains(summary, "概算") || strings.Contains(summary, "$") {
		t.Errorf("the summary invented a cost:\n%s", summary)
	}
}

// An agent that reported nothing is not an agent that used nothing, and a
// line of zeroes is worse than no line.
func TestSummaryOmitsUsageWhenNoneWasReported(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{{
		Summary:   "見ました",
		RawOutput: &reviewer.Output{SchemaVersion: 1, Summary: "見ました"},
	}}}

	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if strings.Contains(f.summaries[0], "トークン") {
		t.Errorf("the summary reports usage nobody measured:\n%s", f.summaries[0])
	}
}

// A review that needed a triage pass paid for two runs, and reporting only
// the second one understates the bill.
func TestSummaryIncludesTheTriagePass(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	f.diff = bigDiff(20)

	e := &fakeEngine{results: []*reviewer.Result{
		{
			Triage: &reviewer.TriageOutput{SchemaVersion: 1, Paths: []string{"file01.go"}},
			Usage:  reviewer.Usage{InputTokens: 2000, OutputTokens: 100},
		},
		{
			Summary:   "見ました",
			RawOutput: &reviewer.Output{SchemaVersion: 1, Summary: "見ました"},
			Usage:     reviewer.Usage{InputTokens: 8000, OutputTokens: 400},
		},
	}}

	job := newJob(t, f, e)
	job.MaxDiffLines = 100

	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	summary := f.summaries[0]
	if !strings.Contains(summary, "入力 10,000") || !strings.Contains(summary, "出力 500") {
		t.Errorf("the summary does not add up both passes:\n%s", summary)
	}
}

// Even a triage pass that failed spent what it spent before failing.
func TestSummaryIncludesAFailedTriagePass(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	f.diff = bigDiff(20)

	e := &fakeEngine{
		results: []*reviewer.Result{
			{Usage: reviewer.Usage{InputTokens: 2000}},
			{
				Summary:   "見ました",
				RawOutput: &reviewer.Output{SchemaVersion: 1, Summary: "見ました"},
				Usage:     reviewer.Usage{InputTokens: 8000},
			},
		},
	}

	job := newJob(t, f, e)
	job.MaxDiffLines = 100

	if err := job.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(f.summaries[0], "入力 10,000") {
		t.Errorf("a failed triage pass was left out of the bill:\n%s", f.summaries[0])
	}
}
