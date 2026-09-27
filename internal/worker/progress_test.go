package worker_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/store/memory"
	"github.com/yteraoka/kibitz/internal/worker"
)

// watchingEngine notes how many "reviewing" notices had been written when the
// agent was started, which is the only way to tell "announced, then reviewed"
// from "reviewed, then announced" -- the second would say nothing through the
// part that takes the time.
type watchingEngine struct {
	inner          *fakeEngine
	forge          *fakeForge
	announcedAtRun []int
}

func (e *watchingEngine) Run(ctx context.Context, req reviewer.Request) (*reviewer.Result, error) {
	e.announcedAtRun = append(e.announcedAtRun, len(e.forge.progress))
	return e.inner.Run(ctx, req)
}

func TestAReviewAnnouncesItselfBeforeTheAgentRuns(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &watchingEngine{inner: &fakeEngine{results: []*reviewer.Result{reviewResult()}}, forge: f}
	j := newJob(t, f, &fakeEngine{})
	j.Engine = e

	if err := j.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(f.progress) != 1 {
		t.Fatalf("%d notices written, want 1", len(f.progress))
	}
	if len(e.announcedAtRun) != 1 || e.announcedAtRun[0] != 1 {
		t.Errorf("the agent started with %v notices written; the pull request heard nothing while it ran", e.announcedAtRun)
	}
	if !strings.Contains(f.progress[0], "対象ファイル: 1 件") {
		t.Errorf("the notice does not say what is being reviewed:\n%s", f.progress[0])
	}
	// And the same comment then became the review.
	if len(f.summaries) != 1 || len(f.markers) != 1 || f.markers[0] != worker.SummaryMarker {
		t.Fatalf("summaries = %d with markers %v, want one summary", len(f.summaries), f.markers)
	}
	if strings.Contains(f.summaries[0], "レビュー中") {
		t.Errorf("the finished summary still says it is reviewing:\n%s", f.summaries[0])
	}
}

// Nothing is announced for a review that is not going to happen. The fake
// keeps notices apart from summaries, so the tests that check a skipped
// review posts nothing would not notice an announcement; this one does.
func TestASkippedReviewDoesNotAnnounce(t *testing.T) {
	for name, setup := range map[string]func(*fakeForge){
		"closed": func(f *fakeForge) { f.pr.State = "closed" },
		"draft":  func(f *fakeForge) { f.pr.Draft = true },
		"empty":  func(f *fakeForge) { f.diff.Files = nil },
	} {
		t.Run(name, func(t *testing.T) {
			origin, _, _ := originRepo(t)
			f := defaultForge()
			setup(f)
			e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}
			j := newJob(t, f, e)
			j.SkipDraft = true

			if err := j.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if len(f.progress) != 0 {
				t.Errorf("a review that did not happen was announced:\n%s", f.progress[0])
			}
			if len(e.requests) != 0 {
				t.Errorf("the agent ran for a skipped review")
			}
		})
	}
}

// Over the posting limit, the review does not start at all. Checked late, as
// it used to be, the model ran to the end for a result nobody would see, and
// the "reviewing" notice would have stayed up for good.
func TestOverThePostingLimitNothingStarts(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	results := make([]*reviewer.Result, 4)
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

	if len(e.requests) != 2 {
		t.Errorf("the agent ran %d times, want 2: runs over the limit are paid for and thrown away", len(e.requests))
	}
	if len(f.progress) != len(f.summaries) {
		t.Errorf("%d notices and %d summaries: a notice with no summary to replace it stays up for good",
			len(f.progress), len(f.summaries))
	}
}

// A retry says so, because it is the case where somebody has already been
// waiting.
func TestARetriedReviewSaysItIsARetry(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	j := newJob(t, f, &fakeEngine{results: []*reviewer.Result{reviewResult()}})

	job := queued(pullRequestEvent(event.KindPROpened, origin))
	job.Deliveries = 3
	if err := j.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.progress) != 1 || !strings.Contains(f.progress[0], "再試行: 3 回目") {
		t.Errorf("the notice does not say it is a retry:\n%v", f.progress)
	}
}

// When kibitz gives up on a review, the comment that said "reviewing" says it
// failed. Anything that is not a review keeps its own notice: it never wrote
// to the summary and must not replace it.
func TestGivingUpReplacesTheNoticeOnlyForAReview(t *testing.T) {
	origin, _, _ := originRepo(t)

	review := pullRequestEvent(event.KindPROpened, origin)
	question := pullRequestEvent(event.KindCommentCreated, origin)
	question.Comment = &event.Comment{ID: "9", Body: "@kibitz なぜ?"}

	for _, tc := range []struct {
		name   string
		ev     *event.ReviewEvent
		marker string
	}{
		{"review", review, worker.SummaryMarker},
		{"question", question, worker.FailureMarker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := defaultForge()
			j := newJob(t, f, &fakeEngine{})
			tc.ev.ID = "github:e1"
			if err := j.NotifyFailure(context.Background(), tc.ev, errors.New("opencode: timed out after 15m0s")); err != nil {
				t.Fatalf("NotifyFailure: %v", err)
			}
			if len(f.markers) != 1 || f.markers[0] != tc.marker {
				t.Fatalf("markers = %v, want [%s]", f.markers, tc.marker)
			}
			for _, want := range []string{"opencode: timed out after 15m0s", "event_id=github:e1", "review"} {
				if !strings.Contains(f.summaries[0], want) {
					t.Errorf("the notice does not contain %q:\n%s", want, f.summaries[0])
				}
			}
		})
	}
}
