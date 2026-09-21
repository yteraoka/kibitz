package run_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/run"
)

func TestGroupStopsEveryoneWhenOneFails(t *testing.T) {
	var g run.Group
	want := errors.New("boom")

	var stopped atomic.Int32
	g.Add(func(context.Context) error { return want })
	for range 3 {
		g.Add(func(ctx context.Context) error {
			<-ctx.Done()
			stopped.Add(1)
			return nil
		})
	}

	done := make(chan error, 1)
	go func() { done <- g.Run(context.Background()) }()

	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Fatalf("Run returned %v, want %v", err, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}

	if got := stopped.Load(); got != 3 {
		t.Errorf("%d components stopped, want 3", got)
	}
}

func TestGroupStopsOnContextCancel(t *testing.T) {
	var g run.Group
	g.Add(func(ctx context.Context) error { <-ctx.Done(); return nil })
	g.Add(func(ctx context.Context) error { <-ctx.Done(); return nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Run(ctx) }()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// A component that returns nil still takes the group down: a worker whose
// subscriber exits should not leave the process up with only a health server.
func TestGroupFirstCleanExitStopsTheRest(t *testing.T) {
	var g run.Group
	g.Add(func(context.Context) error { return nil })

	var stopped atomic.Bool
	g.Add(func(ctx context.Context) error {
		<-ctx.Done()
		stopped.Store(true)
		return nil
	})

	done := make(chan error, 1)
	go func() { done <- g.Run(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if !stopped.Load() {
		t.Error("the remaining component was not stopped")
	}
}
