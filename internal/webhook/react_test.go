package webhook_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/queue/memory"
	"github.com/yteraoka/kibitz/internal/telemetry"
	"github.com/yteraoka/kibitz/internal/webhook"
)

type recordingReactor struct {
	mu   sync.Mutex
	refs []forge.CommentRef
	err  error
}

func (r *recordingReactor) React(_ context.Context, ref forge.CommentRef) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = append(r.refs, ref)
	return r.err
}

func (r *recordingReactor) seen() []forge.CommentRef {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]forge.CommentRef(nil), r.refs...)
}

func TestReceiverReactsToTheCommentItQueued(t *testing.T) {
	cases := []struct {
		fixture, event string
		inline         bool
	}{
		{"issue_comment.created.json", "issue_comment", false},
		// A comment on a line of the diff is served from another endpoint on
		// GitHub, and reacting through the wrong one is a 404.
		{"pull_request_review_comment.created.json", "pull_request_review_comment", true},
	}
	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			reactor := &recordingReactor{}
			rc := newReceiver(t, memory.New(), webhook.WithReactor(reactor, time.Second))

			rec := httptest.NewRecorder()
			rc.ServeHTTP(rec, post(fixture(t, tc.fixture), tc.event))

			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", rec.Code)
			}
			refs := reactor.seen()
			if len(refs) != 1 {
				t.Fatalf("%d reactions, want 1", len(refs))
			}
			ref := refs[0]
			if ref.Owner != "yteraoka" || ref.Repo != "kibitz" || ref.Number == 0 || ref.CommentID == "" {
				t.Errorf("reacted to %+v", ref)
			}
			if ref.Inline != tc.inline {
				t.Errorf("Inline = %v, want %v", ref.Inline, tc.inline)
			}
			if ref.OnIssue {
				t.Error("a comment on a pull request was taken for one on an issue")
			}
		})
	}
}

func TestReceiverReactsOnlyToCommentsItQueued(t *testing.T) {
	cases := []struct{ fixture, event string }{
		// Not a comment: there is nothing to react to.
		{"pull_request.opened.json", "pull_request"},
		// A comment, but kibitz's own, so nothing was queued.
		{"issue_comment.created_by_bot.json", "issue_comment"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			reactor := &recordingReactor{}
			rc := newReceiver(t, memory.New(), webhook.WithReactor(reactor, time.Second))

			rc.ServeHTTP(httptest.NewRecorder(), post(fixture(t, tc.fixture), tc.event))

			if n := len(reactor.seen()); n != 0 {
				t.Errorf("%d reactions, want none", n)
			}
		})
	}
}

// A failed reaction costs the acknowledgement and nothing else: the event is
// queued and the forge is told so.
func TestAFailedReactionDoesNotFailTheDelivery(t *testing.T) {
	q := memory.New()
	metrics := telemetry.NewMetrics()
	reactor := &recordingReactor{err: errors.New("403 Resource not accessible by integration")}
	rc := newReceiver(t, q, webhook.WithReactor(reactor, time.Second), webhook.WithMetrics(metrics))

	rec := httptest.NewRecorder()
	rc.ServeHTTP(rec, post(fixture(t, "issue_comment.created.json"), "issue_comment"))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if q.Len() != 1 {
		t.Errorf("%d messages queued, want 1", q.Len())
	}
	var m dto.Metric
	if err := metrics.Reactions.WithLabelValues("github", "failed").Write(&m); err != nil {
		t.Fatal(err)
	}
	if got := m.GetCounter().GetValue(); got != 1 {
		t.Errorf("failed reactions = %v, want 1", got)
	}
}

// blockingReactor holds the reaction until it is released, and reports what
// its context looked like by then.
type blockingReactor struct {
	release chan struct{}
	done    chan error
}

func (r *blockingReactor) React(ctx context.Context, _ forge.CommentRef) error {
	<-r.release
	// The server notices a hang-up a moment after it happens; give it that
	// moment to cancel, if it is going to.
	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
	}
	r.done <- ctx.Err()
	return nil
}

// The forge has its answer before the reaction is made, and the reaction is
// not cancelled by the forge hanging up. Over a real connection, because an
// in-memory recorder shows neither.
func TestTheForgeIsAnsweredBeforeTheReaction(t *testing.T) {
	reactor := &blockingReactor{release: make(chan struct{}), done: make(chan error, 1)}
	rc := newReceiver(t, memory.New(), webhook.WithReactor(reactor, 5*time.Second))
	srv := httptest.NewServer(rc)
	defer srv.Close()

	body := fixture(t, "issue_comment.created.json")
	req := post(body, "issue_comment")
	out, err := http.NewRequest(http.MethodPost, srv.URL+"/webhook/github", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	out.Header = req.Header
	// Hang up as soon as the answer is in, the way a forge may.
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

	resp, err := client.Do(out)
	if err == nil {
		// The whole response, not just its headers: a forge reads to the end
		// before it counts a delivery as answered.
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if err != nil {
		close(reactor.release)
		t.Fatalf("the forge was kept waiting on the reaction: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202", resp.StatusCode)
	}

	close(reactor.release)
	select {
	case err := <-reactor.done:
		if err != nil {
			t.Errorf("the reaction's context ended with the request: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the reaction was never made")
	}
}

// slowReactor never finishes on its own.
type slowReactor struct{}

func (slowReactor) React(ctx context.Context, _ forge.CommentRef) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestAReactionIsCutOffAtItsTimeout(t *testing.T) {
	rc := newReceiver(t, memory.New(), webhook.WithReactor(slowReactor{}, 50*time.Millisecond))

	finished := make(chan struct{})
	go func() {
		rc.ServeHTTP(httptest.NewRecorder(), post(fixture(t, "issue_comment.created.json"), "issue_comment"))
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler waited on the reaction past its timeout")
	}
}
