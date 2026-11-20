package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/transcript"
	"github.com/yteraoka/kibitz/internal/worker"
)

func withTranscripts(t *testing.T, j *worker.ReviewJob) *transcript.DirStore {
	t.Helper()
	store := &transcript.DirStore{Root: t.TempDir()}
	j.Transcripts = store
	j.TranscriptBaseURL = "https://kibitz.example"
	j.EngineName = "opencode"
	return store
}

// The summary names the record of what the agent did, and the record has
// every run the review made — the failed first attempt as well as the one
// that was posted.
func TestReviewSummaryLinksToItsTranscript(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	good := reviewResult()
	good.Events = []json.RawMessage{json.RawMessage(`{"type":"text","part":{"type":"text","text":"done"}}`)}
	e := &fakeEngine{
		errs: []error{reviewer.WithEvents(
			&reviewer.OutputError{Err: errors.New("summary is empty")},
			[]json.RawMessage{json.RawMessage(`{"type":"step_finish"}`)},
		)},
		results: []*reviewer.Result{nil, good},
	}
	j := newJob(t, f, e)
	store := withTranscripts(t, j)

	if err := j.Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.summaries) != 1 {
		t.Fatalf("%d summaries, want 1", len(f.summaries))
	}

	id := transcript.ParseTrailer(f.summaries[0])
	if id == "" {
		t.Fatalf("the summary names no transcript:\n%s", f.summaries[0])
	}
	if !strings.Contains(f.summaries[0], "](https://kibitz.example/transcripts/"+id+")") {
		t.Errorf("the trailer is not a link to the server:\n%s", f.summaries[0])
	}

	rec, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("the record the summary names cannot be read: %v", err)
	}
	if rec.Ref != "yteraoka/kibitz#42" || rec.EventID != "github:d1" {
		t.Errorf("the record does not say what it is about: %+v", rec)
	}
	if len(rec.Runs) != 2 {
		t.Fatalf("%d runs recorded, want the failed attempt and the retry", len(rec.Runs))
	}
	first, second := rec.Runs[0], rec.Runs[1]
	if first.Error == "" || len(first.Events) != 1 {
		t.Errorf("the failed attempt lost its error or its events: %+v", first)
	}
	if second.Mode != "review" || second.Engine != "opencode" || second.Prompt == "" || len(second.Events) != 1 {
		t.Errorf("the posted run was not recorded whole: %+v", second)
	}
	if second.Output != good.Summary {
		t.Errorf("output = %q, want the summary", second.Output)
	}
}

func TestAnswerLinksToItsTranscript(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{{Reply: "ロックの期限が切れるためです。"}}}
	j := newJob(t, f, e)
	withTranscripts(t, j)

	ev := pullRequestEvent(event.KindCommentCreated, origin)
	ev.Comment = &event.Comment{ID: "9", ThreadID: "7", Body: "@kibitz なぜ?"}
	if err := j.Handle(context.Background(), queued(ev)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.replies) != 1 || transcript.ParseTrailer(f.replies[0]) == "" {
		t.Fatalf("the reply names no transcript: %q", f.replies)
	}
	if !strings.HasPrefix(f.replies[0], "ロックの期限が切れるためです。") {
		t.Errorf("the trailer displaced the answer: %q", f.replies[0])
	}
}

// A job that failed is the one whose transcript is worth reading, so the
// failure notice links to it.
func TestFailureNoticeLinksToTheFailedRunsTranscript(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{errs: []error{errors.New("the model is overloaded")}}
	j := newJob(t, f, e)
	store := withTranscripts(t, j)

	ev := pullRequestEvent(event.KindPROpened, origin)
	err := j.Handle(context.Background(), queued(ev))
	if err == nil {
		t.Fatal("Handle succeeded although the engine failed")
	}
	if err := j.NotifyFailure(context.Background(), ev, err); err != nil {
		t.Fatalf("NotifyFailure: %v", err)
	}

	id := transcript.ParseTrailer(f.summaries[len(f.summaries)-1])
	if id == "" {
		t.Fatalf("the failure notice names no transcript:\n%s", f.summaries[len(f.summaries)-1])
	}
	rec, err := store.Get(context.Background(), id)
	if err != nil || len(rec.Runs) != 1 || !strings.Contains(rec.Runs[0].Error, "overloaded") {
		t.Errorf("the failed run was not recorded: %v %+v", err, rec)
	}
}

// Without a store nothing changes: no trailer, nothing saved.
func TestNoTranscriptWithoutAStore(t *testing.T) {
	origin, _, _ := originRepo(t)
	f := defaultForge()
	e := &fakeEngine{results: []*reviewer.Result{reviewResult()}}

	if err := newJob(t, f, e).Handle(context.Background(), queued(pullRequestEvent(event.KindPROpened, origin))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if strings.Contains(f.summaries[0], transcript.TrailerKey) {
		t.Errorf("a trailer was written with nowhere to keep the record:\n%s", f.summaries[0])
	}
}
