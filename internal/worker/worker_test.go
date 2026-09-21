package worker_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/config"
	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/queue/memory"
	"github.com/yteraoka/kibitz/internal/worker"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func testEvent(id string) *event.ReviewEvent {
	return &event.ReviewEvent{
		SchemaVersion: event.SchemaVersion,
		ID:            id,
		OccurredAt:    time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
		Source:        event.Source{Platform: event.PlatformGitHub, DeliveryID: id},
		Kind:          event.KindPROpened,
		Repository:    event.Repository{Owner: "yteraoka", Name: "kibitz", FullName: "yteraoka/kibitz"},
		PullRequest:   &event.PullRequest{Number: 42},
		Actor:         event.Actor{Login: "yteraoka"},
	}
}

func TestRunHandlesEvents(t *testing.T) {
	q := memory.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := q.Publish(ctx, testEvent("a")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	handled := make(chan string, 1)
	w := worker.New(q, worker.HandlerFunc(func(_ context.Context, ev *event.ReviewEvent) error {
		handled <- ev.ID
		return nil
	}), discardLogger(), &config.Worker{JobTimeout: time.Second})

	go func() { _ = w.Run(ctx) }()

	select {
	case got := <-handled:
		if got != "a" {
			t.Errorf("handled %q, want a", got)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the job")
	}
}

func TestFailedJobIsRedelivered(t *testing.T) {
	q := memory.New()
	q.MaxDeliveries = 3
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := q.Publish(ctx, testEvent("a")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	attempts := make(chan int, 4)
	count := 0
	w := worker.New(q, worker.HandlerFunc(func(context.Context, *event.ReviewEvent) error {
		count++
		attempts <- count
		if count < 2 {
			return errors.New("transient failure")
		}
		return nil
	}), discardLogger(), &config.Worker{JobTimeout: time.Second})

	go func() { _ = w.Run(ctx) }()

	for want := 1; want <= 2; want++ {
		select {
		case got := <-attempts:
			if got != want {
				t.Fatalf("attempt = %d, want %d", got, want)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for attempt %d", want)
		}
	}
}

// A job that runs past its timeout must be cut off, not left holding a lease
// the queue will hand to a second worker.
func TestJobTimeout(t *testing.T) {
	q := memory.New()
	q.MaxDeliveries = 1
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := q.Publish(ctx, testEvent("a")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	deadline := make(chan error, 1)
	w := worker.New(q, worker.HandlerFunc(func(ctx context.Context, _ *event.ReviewEvent) error {
		<-ctx.Done()
		deadline <- ctx.Err()
		return ctx.Err()
	}), discardLogger(), &config.Worker{JobTimeout: 50 * time.Millisecond})

	go func() { _ = w.Run(ctx) }()

	select {
	case err := <-deadline:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want DeadlineExceeded", err)
		}
	case <-ctx.Done():
		t.Fatal("the job was not cut off at its timeout")
	}
}
