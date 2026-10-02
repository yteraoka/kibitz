package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/scale"
)

type fakeReconciler struct {
	result scale.Result
	err    error

	// running and overlapped detect two reconciles in flight at once.
	running    atomic.Int32
	overlapped atomic.Bool
	calls      atomic.Int32
}

func (f *fakeReconciler) Reconcile(context.Context) (scale.Result, error) {
	f.calls.Add(1)
	if f.running.Add(1) > 1 {
		f.overlapped.Store(true)
	}
	defer f.running.Add(-1)
	time.Sleep(10 * time.Millisecond)
	return f.result, f.err
}

func newTestHandler(r reconciler) http.Handler {
	return &reconcileHandler{scaler: r, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestReconcileReportsTheDecision(t *testing.T) {
	h := newTestHandler(&fakeReconciler{result: scale.Result{
		Backlog: scale.Backlog{Undelivered: 5, Known: true},
		Current: 1,
		Desired: 3,
		Reason:  scale.Reason("backlog"),
		Changed: true,
	}})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reconcile", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got reconcileResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	want := reconcileResponse{Backlog: 5, Known: true, Current: 1, Desired: 3, Reason: "backlog", Changed: true}
	if got != want {
		t.Errorf("response = %+v, want %+v", got, want)
	}
}

// Cloud Scheduler only knows a run failed from the status code, and the alert
// on the scaler counts those. A 200 with an error in the body would hide it.
func TestAFailedReconcileIsAServerError(t *testing.T) {
	h := newTestHandler(&fakeReconciler{err: errors.New("permission denied")})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reconcile", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var got reconcileResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.Error != "permission denied" {
		t.Errorf("error = %q, want the cause", got.Error)
	}
}

// A retry can arrive while the attempt it retries is still running. Both
// writing the instance count at once would race, so they are taken in turn.
func TestReconcilesDoNotOverlap(t *testing.T) {
	f := &fakeReconciler{}
	h := newTestHandler(f)

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/reconcile", nil))
		}()
	}
	wg.Wait()

	if f.calls.Load() != 5 {
		t.Errorf("reconciled %d times, want 5", f.calls.Load())
	}
	if f.overlapped.Load() {
		t.Error("two reconciles ran at once")
	}
}
