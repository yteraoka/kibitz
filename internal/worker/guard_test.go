package worker_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/store"
	"github.com/yteraoka/kibitz/internal/store/memory"
	"github.com/yteraoka/kibitz/internal/worker"
)

type recordingNotifier struct {
	mu     sync.Mutex
	causes []error
}

func (n *recordingNotifier) NotifyFailure(_ context.Context, _ *event.ReviewEvent, cause error) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.causes = append(n.causes, cause)
	return nil
}

func (n *recordingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.causes)
}

func guardEvent(delivery string) *event.ReviewEvent {
	return &event.ReviewEvent{
		SchemaVersion: event.SchemaVersion,
		ID:            "github:" + delivery,
		OccurredAt:    time.Now(),
		Source:        event.Source{Platform: event.PlatformGitHub, DeliveryID: delivery},
		Kind:          event.KindPROpened,
		Repository:    event.Repository{Owner: "yteraoka", Name: "kibitz", FullName: "yteraoka/kibitz"},
		PullRequest:   &event.PullRequest{Number: 42},
		Actor:         event.Actor{Login: "yteraoka"},
	}
}

func newGuard(t *testing.T, s store.Store, next worker.Handler, n worker.Notifier) *worker.Guard {
	t.Helper()
	return &worker.Guard{
		Next:          next,
		Store:         s,
		Notifier:      n,
		Logger:        discardLogger(),
		ClaimTTL:      time.Minute,
		DoneTTL:       time.Hour,
		LockTTL:       time.Minute,
		MaxDeliveries: 3,
		BotLogins:     []string{"kibitz[bot]"},
	}
}

func TestGuardRunsOncePerDelivery(t *testing.T) {
	ctx := context.Background()
	var runs atomic.Int32

	g := newGuard(t, memory.New(), worker.HandlerFunc(func(context.Context, *worker.Job) error {
		runs.Add(1)
		return nil
	}), nil)

	ev := guardEvent("d1")
	for range 3 {
		if err := g.Handle(ctx, &worker.Job{Event: ev, Deliveries: 1}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}

	if got := runs.Load(); got != 1 {
		t.Errorf("the job ran %d times, want 1: a redelivered message must not review again", got)
	}
}

// A failed job has to be retryable: if the claim were kept, the redelivery
// would be mistaken for a duplicate and the review would be lost.
func TestGuardReleasesTheClaimOnFailure(t *testing.T) {
	ctx := context.Background()
	var runs atomic.Int32

	g := newGuard(t, memory.New(), worker.HandlerFunc(func(context.Context, *worker.Job) error {
		if runs.Add(1) == 1 {
			return errors.New("the API was briefly down")
		}
		return nil
	}), nil)

	ev := guardEvent("d1")
	if err := g.Handle(ctx, &worker.Job{Event: ev, Deliveries: 1}); err == nil {
		t.Fatal("Handle succeeded although the job failed")
	}
	if err := g.Handle(ctx, &worker.Job{Event: ev, Deliveries: 2}); err != nil {
		t.Fatalf("the retry was skipped: %v", err)
	}
	if got := runs.Load(); got != 2 {
		t.Errorf("the job ran %d times, want 2", got)
	}
}

func TestGuardSerializesOnePullRequest(t *testing.T) {
	ctx := context.Background()
	s := memory.New()

	started := make(chan struct{})
	release := make(chan struct{})
	g := newGuard(t, s, worker.HandlerFunc(func(context.Context, *worker.Job) error {
		close(started)
		<-release
		return nil
	}), nil)

	go func() { _ = g.Handle(ctx, &worker.Job{Event: guardEvent("d1"), Deliveries: 1}) }()
	<-started

	// A second event for the same pull request must wait rather than run
	// alongside the first.
	err := g.Handle(ctx, &worker.Job{Event: guardEvent("d2"), Deliveries: 1})
	if err == nil {
		t.Fatal("a concurrent job for the same pull request was allowed to run")
	}
	delay, ok := worker.RetryDelay(err)
	if !ok || delay <= 0 {
		t.Errorf("err = %v, want a retry-after error", err)
	}
	close(release)

	// The second delivery must remain retryable.
	if _, err := s.Get(ctx, store.DeliveryKey(guardEvent("d2"))); !errors.Is(err, store.ErrNotFound) {
		t.Error("the claim of the deferred delivery was kept; its retry would be skipped")
	}
}

// Different pull requests are independent: one long review must not hold up
// every other repository.
func TestGuardDoesNotSerializeAcrossPullRequests(t *testing.T) {
	ctx := context.Background()
	s := memory.New()

	started := make(chan struct{})
	release := make(chan struct{})
	var second atomic.Bool

	g := newGuard(t, s, worker.HandlerFunc(func(_ context.Context, j *worker.Job) error {
		if j.Event.PullRequest.Number == 42 {
			close(started)
			<-release
			return nil
		}
		second.Store(true)
		return nil
	}), nil)

	go func() { _ = g.Handle(ctx, &worker.Job{Event: guardEvent("d1"), Deliveries: 1}) }()
	<-started

	other := guardEvent("d2")
	other.PullRequest.Number = 43
	if err := g.Handle(ctx, &worker.Job{Event: other, Deliveries: 1}); err != nil {
		t.Fatalf("a different pull request was blocked: %v", err)
	}
	if !second.Load() {
		t.Error("the second pull request did not run")
	}
	close(release)
}

func TestGuardGivesUpAfterTooManyDeliveries(t *testing.T) {
	ctx := context.Background()
	notifier := &recordingNotifier{}

	g := newGuard(t, memory.New(), worker.HandlerFunc(func(context.Context, *worker.Job) error {
		return errors.New("still failing")
	}), notifier)

	// Below the limit the failure is reported to the queue for a retry.
	if err := g.Handle(ctx, &worker.Job{Event: guardEvent("d1"), Deliveries: 1}); err == nil {
		t.Fatal("Handle succeeded although the job failed")
	}
	if notifier.count() != 0 {
		t.Error("the pull request was told about a failure that is still being retried")
	}

	// At the limit it stops retrying and says so instead of disappearing.
	err := g.Handle(ctx, &worker.Job{Event: guardEvent("d2"), Deliveries: 3})
	if err != nil {
		t.Fatalf("Handle = %v, want nil: the message should be acknowledged", err)
	}
	if notifier.count() != 1 {
		t.Fatalf("%d failure notices, want 1", notifier.count())
	}
}

func TestGuardDoesNotRetryPermanentFailures(t *testing.T) {
	ctx := context.Background()
	notifier := &recordingNotifier{}

	g := newGuard(t, memory.New(), worker.HandlerFunc(func(context.Context, *worker.Job) error {
		return worker.Permanent(errors.New("the pull request was deleted"))
	}), notifier)

	err := g.Handle(ctx, &worker.Job{Event: guardEvent("d1"), Deliveries: 1})
	if err != nil {
		t.Fatalf("Handle = %v, want nil: retrying cannot fix this", err)
	}
	if notifier.count() != 1 {
		t.Errorf("%d failure notices, want 1", notifier.count())
	}
}

// The server already drops kibitz's own events; this is the second check.
func TestGuardDropsItsOwnEvents(t *testing.T) {
	ctx := context.Background()
	var runs atomic.Int32

	g := newGuard(t, memory.New(), worker.HandlerFunc(func(context.Context, *worker.Job) error {
		runs.Add(1)
		return nil
	}), nil)

	ev := guardEvent("d1")
	ev.Actor = event.Actor{Login: "KIBITZ[bot]", IsBot: true}

	if err := g.Handle(ctx, &worker.Job{Event: ev, Deliveries: 1}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if runs.Load() != 0 {
		t.Error("kibitz reacted to its own event")
	}
}

// Losing the lock mid-job means someone else owns the pull request now, so the
// job has to stop rather than post a second review.
func TestGuardStopsTheJobWhenTheLeaseIsLost(t *testing.T) {
	ctx := context.Background()
	s := memory.New()

	stopped := make(chan error, 1)
	g := newGuard(t, s, worker.HandlerFunc(func(ctx context.Context, j *worker.Job) error {
		// Simulate the lock being taken over by deleting it outright.
		if err := s.Delete(ctx, store.LockKey(j.Event)); err != nil {
			return err
		}
		<-ctx.Done()
		stopped <- ctx.Err()
		return ctx.Err()
	}), nil)
	g.LockTTL = 150 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- g.Handle(ctx, &worker.Job{Event: guardEvent("d1"), Deliveries: 1}) }()

	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the job stopped with %v, want a cancelled context", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the job kept running after the lease was lost")
	}
	<-done
}

func TestGuardReportsStoreFailures(t *testing.T) {
	ctx := context.Background()
	g := newGuard(t, failingStore{}, worker.HandlerFunc(func(context.Context, *worker.Job) error {
		t.Error("the job ran although the store was unreachable")
		return nil
	}), nil)

	// A store that cannot be reached is a transient condition: the message is
	// retried rather than dropped.
	err := g.Handle(ctx, &worker.Job{Event: guardEvent("d1"), Deliveries: 1})
	if err == nil {
		t.Fatal("Handle succeeded although the store was unreachable")
	}
	if worker.IsPermanent(err) {
		t.Error("an unreachable store was treated as permanent")
	}
}

type failingStore struct{}

func (failingStore) MarkProcessed(context.Context, string, time.Duration) (bool, error) {
	return false, fmt.Errorf("firestore is unreachable")
}

func (failingStore) AcquireLock(context.Context, string, time.Duration) (store.Lease, error) {
	return nil, fmt.Errorf("firestore is unreachable")
}
func (failingStore) Get(context.Context, string) ([]byte, error) { return nil, store.ErrNotFound }
func (failingStore) Put(context.Context, string, []byte, time.Duration) error {
	return fmt.Errorf("firestore is unreachable")
}
func (failingStore) Delete(context.Context, string) error { return nil }
func (failingStore) Incr(context.Context, string, int64, time.Duration) (int64, error) {
	return 0, fmt.Errorf("firestore is unreachable")
}
func (failingStore) Close() error { return nil }

func TestErrorClassification(t *testing.T) {
	base := errors.New("boom")

	if !worker.IsPermanent(worker.Permanent(base)) {
		t.Error("Permanent was not recognized")
	}
	if worker.IsPermanent(base) {
		t.Error("an ordinary error was treated as permanent")
	}
	if !errors.Is(worker.Permanent(base), base) {
		t.Error("Permanent lost the underlying error")
	}

	delay, ok := worker.RetryDelay(worker.RetryAfter(base, 30*time.Second))
	if !ok || delay != 30*time.Second {
		t.Errorf("RetryDelay = %v, %v", delay, ok)
	}
	if _, ok := worker.RetryDelay(base); ok {
		t.Error("an ordinary error asked for a retry delay")
	}
	if worker.Permanent(nil) != nil || worker.RetryAfter(nil, time.Second) != nil {
		t.Error("wrapping nil produced an error")
	}
}

// A worker killed mid-review -- which is what a revision swap does -- leaves a
// claim with no completion behind it. The redelivery has to pick that work up,
// not mistake it for a job that already finished.
func TestGuardTakesOverAfterAWorkerDies(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	ev := guardEvent("d1")

	// What the dead worker left behind: a claim, and no lock (its lease
	// expired) and no completion.
	if _, err := s.MarkProcessed(ctx, store.DeliveryKey(ev), time.Hour); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}

	var runs atomic.Int32
	g := newGuard(t, s, worker.HandlerFunc(func(context.Context, *worker.Job) error {
		runs.Add(1)
		return nil
	}), nil)

	if err := g.Handle(ctx, &worker.Job{Event: ev, Deliveries: 2}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := runs.Load(); got != 1 {
		t.Errorf("the job ran %d times, want 1: the abandoned work was dropped", got)
	}

	// And once it really is finished, a further redelivery is a duplicate.
	if err := g.Handle(ctx, &worker.Job{Event: ev, Deliveries: 3}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := runs.Load(); got != 1 {
		t.Errorf("the job ran %d times, want 1 after completion", got)
	}
}

// A claim whose owner is still working is different: the lock is held, so the
// redelivery waits instead of running a second copy.
func TestGuardWaitsWhileTheClaimHolderIsStillWorking(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	ev := guardEvent("d1")

	if _, err := s.MarkProcessed(ctx, store.DeliveryKey(ev), time.Hour); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}
	lease, err := s.AcquireLock(ctx, store.LockKey(ev), time.Hour)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer func() { _ = lease.Release(ctx) }()

	var runs atomic.Int32
	g := newGuard(t, s, worker.HandlerFunc(func(context.Context, *worker.Job) error {
		runs.Add(1)
		return nil
	}), nil)

	err = g.Handle(ctx, &worker.Job{Event: ev, Deliveries: 2})
	if err == nil {
		t.Fatal("Handle succeeded while another worker held the pull request")
	}
	if _, ok := worker.RetryDelay(err); !ok {
		t.Errorf("err = %v, want a retry-after error", err)
	}
	if runs.Load() != 0 {
		t.Error("a second copy of the review ran")
	}

	// The other worker's claim must survive: deleting it would let the next
	// redelivery start a duplicate review.
	if _, err := s.Get(ctx, store.DeliveryKey(ev)); err != nil {
		t.Errorf("the claim was released by a worker that does not own it: %v", err)
	}
}
