package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/store"
	"github.com/yteraoka/kibitz/internal/store/memory"
	"github.com/yteraoka/kibitz/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		t.Helper()
		s := memory.New()
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

// The in-memory store can be driven by a fake clock, so expiry is exercised
// without waiting for it.
func TestExpiryWithAFakeClock(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	s := memory.New()
	s.Now = func() time.Time { return now }

	if _, err := s.MarkProcessed(ctx, "k", time.Hour); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}

	now = now.Add(59 * time.Minute)
	if first, _ := s.MarkProcessed(ctx, "k", time.Hour); first {
		t.Error("the record expired early")
	}

	now = now.Add(2 * time.Minute)
	if first, _ := s.MarkProcessed(ctx, "k", time.Hour); !first {
		t.Error("the record did not expire")
	}
}

// A counter must not renew its own window on every increment, or an hourly
// posting limit would never reset.
func TestIncrKeepsItsWindow(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	s := memory.New()
	s.Now = func() time.Time { return now }

	for range 3 {
		if _, err := s.Incr(ctx, "posts", 1, time.Hour); err != nil {
			t.Fatalf("Incr: %v", err)
		}
		now = now.Add(20 * time.Minute)
	}

	// An hour has passed since the first increment, so the window is over.
	now = now.Add(time.Minute)
	got, err := s.Incr(ctx, "posts", 1, time.Hour)
	if err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if got != 1 {
		t.Errorf("the counter = %d, want it reset to 1", got)
	}
}

func TestZeroTTLDoesNotExpire(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	s := memory.New()
	s.Now = func() time.Time { return now }

	if err := s.Put(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	now = now.Add(100 * 24 * time.Hour)

	if _, err := s.Get(ctx, "k"); err != nil {
		t.Errorf("a value stored without a ttl expired: %v", err)
	}
}
