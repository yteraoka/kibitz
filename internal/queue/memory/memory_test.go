package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/queue"
	"github.com/yteraoka/kibitz/internal/queue/memory"
)

func testEvent(id string) *event.ReviewEvent {
	return &event.ReviewEvent{
		SchemaVersion: event.SchemaVersion,
		ID:            id,
		OccurredAt:    time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
		Source: event.Source{
			Platform:   event.PlatformGitHub,
			DeliveryID: id,
		},
		Kind:        event.KindPROpened,
		Repository:  event.Repository{Owner: "yteraoka", Name: "kibitz", FullName: "yteraoka/kibitz"},
		PullRequest: &event.PullRequest{Number: 42},
		Actor:       event.Actor{Login: "yteraoka"},
	}
}

func TestPublishReceive(t *testing.T) {
	q := memory.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, id := range []string{"a", "b", "c"} {
		if _, err := q.Publish(ctx, testEvent(id)); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}

	received := make(chan string, 3)
	go func() {
		_ = q.Receive(ctx, func(_ context.Context, m *queue.Message) error {
			received <- m.Event.ID
			return nil
		})
	}()

	for _, want := range []string{"a", "b", "c"} {
		select {
		case got := <-received:
			if got != want {
				t.Errorf("received %q, want %q (order is not preserved)", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}

func TestPublishRejectsInvalidEvent(t *testing.T) {
	q := memory.New()
	ev := testEvent("a")
	ev.Repository.FullName = ""

	if _, err := q.Publish(context.Background(), ev); err == nil {
		t.Fatal("Publish accepted an invalid event")
	}
}

func TestPublishIsolatesTheEvent(t *testing.T) {
	q := memory.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ev := testEvent("a")
	if _, err := q.Publish(ctx, ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// A real transport serializes; mutating after publishing must not be
	// visible to the subscriber.
	ev.Repository.FullName = "someone/else"

	got := make(chan string, 1)
	go func() {
		_ = q.Receive(ctx, func(_ context.Context, m *queue.Message) error {
			got <- m.Event.Repository.FullName
			return nil
		})
	}()

	select {
	case name := <-got:
		if name != "yteraoka/kibitz" {
			t.Errorf("subscriber saw %q, want the value as published", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}

func TestNackRedeliversThenDeadLetters(t *testing.T) {
	q := memory.New()
	q.MaxDeliveries = 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := q.Publish(ctx, testEvent("a")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	attempts := make(chan int, 8)
	go func() {
		_ = q.Receive(ctx, func(_ context.Context, m *queue.Message) error {
			attempts <- m.Deliveries
			return errors.New("still failing")
		})
	}()

	for want := 1; want <= 3; want++ {
		select {
		case got := <-attempts:
			if got != want {
				t.Errorf("delivery count = %d, want %d", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for delivery %d", want)
		}
	}

	select {
	case got := <-attempts:
		t.Fatalf("message was delivered a %dth time, want it dead lettered", got)
	case <-time.After(200 * time.Millisecond):
	}

	dead := q.Dead()
	if len(dead) != 1 || dead[0].Event.ID != "a" {
		t.Fatalf("dead letters = %v, want the failing message", dead)
	}
}

func TestReceiveStopsOnContextCancel(t *testing.T) {
	q := memory.New()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- q.Receive(ctx, func(context.Context, *queue.Message) error { return nil }) }()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Receive returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Receive did not return after cancel")
	}
}

func TestPublishAfterClose(t *testing.T) {
	q := memory.New()
	if err := q.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := q.Publish(context.Background(), testEvent("a")); !errors.Is(err, memory.ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}
