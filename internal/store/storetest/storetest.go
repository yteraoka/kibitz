// Package storetest is the conformance suite every [store.Store] must pass.
// The in-memory store and Firestore have to behave identically, or a bug that
// only appears in production is exactly the kind a state store hides.
package storetest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/store"
)

// Factory creates a store for one test. Cleanup belongs on t.
type Factory func(t *testing.T) store.Store

// Run executes the suite against the store the factory builds.
func Run(t *testing.T, newStore Factory) {
	t.Helper()

	t.Run("MarkProcessed", func(t *testing.T) { testMarkProcessed(t, newStore) })
	t.Run("MarkProcessedExpires", func(t *testing.T) { testMarkProcessedExpires(t, newStore) })
	t.Run("Lock", func(t *testing.T) { testLock(t, newStore) })
	t.Run("LockExpires", func(t *testing.T) { testLockExpires(t, newStore) })
	t.Run("LeaseLost", func(t *testing.T) { testLeaseLost(t, newStore) })
	t.Run("LockIsExclusiveUnderLoad", func(t *testing.T) { testLockRace(t, newStore) })
	t.Run("Values", func(t *testing.T) { testValues(t, newStore) })
	t.Run("Incr", func(t *testing.T) { testIncr(t, newStore) })
}

func testMarkProcessed(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)

	first, err := s.MarkProcessed(ctx, key(t, "delivery"), time.Minute)
	if err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}
	if !first {
		t.Fatal("the first caller was not told it was first")
	}

	// A redelivered webhook, or a redelivered queue message, lands here.
	first, err = s.MarkProcessed(ctx, key(t, "delivery"), time.Minute)
	if err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}
	if first {
		t.Error("a repeat delivery was treated as new; it would be reviewed twice")
	}

	// A different delivery is unaffected.
	first, err = s.MarkProcessed(ctx, key(t, "other"), time.Minute)
	if err != nil || !first {
		t.Errorf("an unrelated key was blocked: first=%v err=%v", first, err)
	}

	// The record says the work was claimed, not that it finished. A worker
	// that dies mid-job leaves exactly this, and the difference is what lets
	// the next delivery pick the work up instead of skipping it.
	value, err := s.Get(ctx, key(t, "delivery"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(value) != store.MarkerClaim {
		t.Errorf("MarkProcessed wrote %q, want %q", value, store.MarkerClaim)
	}
}

func testMarkProcessedExpires(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.MarkProcessed(ctx, key(t, "ttl"), 50*time.Millisecond); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}
	time.Sleep(120 * time.Millisecond)

	first, err := s.MarkProcessed(ctx, key(t, "ttl"), time.Minute)
	if err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}
	if !first {
		t.Error("the record did not expire")
	}
}

func testLock(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)
	k := key(t, "lock")

	lease, err := s.AcquireLock(ctx, k, time.Minute)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}

	if _, err := s.AcquireLock(ctx, k, time.Minute); !errors.Is(err, store.ErrLocked) {
		t.Fatalf("a second lock returned %v, want ErrLocked", err)
	}

	if err := lease.Extend(ctx, time.Minute); err != nil {
		t.Errorf("Extend: %v", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}

	lease2, err := s.AcquireLock(ctx, k, time.Minute)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	_ = lease2.Release(ctx)
}

func testLockExpires(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)
	k := key(t, "lock-ttl")

	// A worker that dies without releasing must not block the pull request
	// forever, which is why the lock is a lease rather than a flag.
	if _, err := s.AcquireLock(ctx, k, 50*time.Millisecond); err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	time.Sleep(120 * time.Millisecond)

	lease, err := s.AcquireLock(ctx, k, time.Minute)
	if err != nil {
		t.Fatalf("the expired lock was not reclaimable: %v", err)
	}
	_ = lease.Release(ctx)
}

func testLeaseLost(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)
	k := key(t, "lease")

	lease, err := s.AcquireLock(ctx, k, time.Minute)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if err := lease.Release(ctx); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("releasing twice returned %v, want ErrLeaseLost", err)
	}
	if err := lease.Extend(ctx, time.Minute); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("extending a released lease returned %v, want ErrLeaseLost", err)
	}

	// Someone else now holds it, and this lease must not be able to drop it.
	other, err := s.AcquireLock(ctx, k, time.Minute)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if err := lease.Release(ctx); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("a stale lease released someone else's lock: %v", err)
	}
	_ = other.Release(ctx)
}

// Two workers racing for one pull request is the case this whole package
// exists for, so it is tested directly.
func testLockRace(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)
	k := key(t, "race")

	const contenders = 8
	var (
		wg      sync.WaitGroup
		winners atomic.Int32
		leases  = make(chan store.Lease, contenders)
	)

	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := s.AcquireLock(ctx, k, time.Minute)
			if errors.Is(err, store.ErrLocked) {
				return
			}
			if err != nil {
				t.Errorf("AcquireLock: %v", err)
				return
			}
			winners.Add(1)
			leases <- lease
		}()
	}
	wg.Wait()
	close(leases)

	if got := winners.Load(); got != 1 {
		t.Errorf("%d workers took the lock at once, want 1", got)
	}
	for lease := range leases {
		_ = lease.Release(ctx)
	}
}

func testValues(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)
	k := key(t, "value")

	if _, err := s.Get(ctx, k); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get on a missing key returned %v, want ErrNotFound", err)
	}

	if err := s.Put(ctx, k, []byte("ses_123"), time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx, k)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "ses_123" {
		t.Errorf("Get = %q, want ses_123", got)
	}

	if err := s.Delete(ctx, k); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, k); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get after Delete returned %v, want ErrNotFound", err)
	}
	// Deleting what is not there is not an error: cleanup runs on paths that
	// may already have cleaned up.
	if err := s.Delete(ctx, k); err != nil {
		t.Errorf("Delete on a missing key: %v", err)
	}
}

func testIncr(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)
	k := key(t, "counter")

	for want := int64(1); want <= 3; want++ {
		got, err := s.Incr(ctx, k, 1, time.Hour)
		if err != nil {
			t.Fatalf("Incr: %v", err)
		}
		if got != want {
			t.Fatalf("Incr = %d, want %d", got, want)
		}
	}

	other, err := s.Incr(ctx, key(t, "counter-2"), 5, time.Hour)
	if err != nil || other != 5 {
		t.Errorf("a separate counter = %d (err %v), want 5", other, err)
	}
}

// key keeps tests from colliding inside a shared backend such as an emulator.
func key(t *testing.T, name string) string {
	t.Helper()
	return t.Name() + ":" + name
}
