package scale

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func testPolicy() Policy {
	return Policy{Min: 0, Max: 4, PerInstance: 2, IdleAfter: 15 * time.Minute}
}

func TestPolicyDesired(t *testing.T) {
	tests := []struct {
		name    string
		backlog Backlog
		want    int
		reason  Reason
	}{
		{
			name:    "no reading keeps the worker running",
			backlog: Backlog{},
			want:    1,
			reason:  ReasonUnknown,
		},
		{
			name:    "empty for long enough",
			backlog: Backlog{Known: true, EmptyFor: 20 * time.Minute},
			want:    0,
			reason:  ReasonIdle,
		},
		{
			name:    "empty but not yet",
			backlog: Backlog{Known: true, EmptyFor: 3 * time.Minute},
			want:    1,
			reason:  ReasonCooling,
		},
		{
			name:    "one message",
			backlog: Backlog{Known: true, Undelivered: 1},
			want:    1,
			reason:  ReasonBacklog,
		},
		{
			name:    "a message per instance is still one instance",
			backlog: Backlog{Known: true, Undelivered: 2},
			want:    1,
			reason:  ReasonBacklog,
		},
		{
			name:    "a backlog is divided",
			backlog: Backlog{Known: true, Undelivered: 5},
			want:    3,
			reason:  ReasonBacklog,
		},
		{
			name:    "a flood is capped",
			backlog: Backlog{Known: true, Undelivered: 500},
			want:    4,
			reason:  ReasonBacklog,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := testPolicy().Desired(tc.backlog)
			if got != tc.want {
				t.Errorf("desired = %d, want %d", got, tc.want)
			}
			if reason != tc.reason {
				t.Errorf("reason = %q, want %q", reason, tc.reason)
			}
		})
	}
}

// An operator who does not want scale to zero sets a floor, and an idle queue
// must not undercut it.
func TestPolicyRespectsTheFloor(t *testing.T) {
	p := Policy{Min: 2, Max: 4, PerInstance: 2, IdleAfter: time.Minute}

	if got, _ := p.Desired(Backlog{Known: true, EmptyFor: time.Hour}); got != 2 {
		t.Errorf("idle = %d, want the floor of 2", got)
	}
	if got, _ := p.Desired(Backlog{Known: true, Undelivered: 1}); got != 2 {
		t.Errorf("one message = %d, want the floor of 2", got)
	}
}

func TestPolicyNormalize(t *testing.T) {
	p := Policy{Min: 3, Max: 1, PerInstance: 0, IdleAfter: -time.Second}.Normalize()

	if p.Max < p.Min {
		t.Errorf("max = %d, want at least min %d", p.Max, p.Min)
	}
	if p.PerInstance != 1 {
		t.Errorf("per instance = %d, want 1", p.PerInstance)
	}
	if p.IdleAfter != DefaultPolicy().IdleAfter {
		t.Errorf("idle after = %s, want the default", p.IdleAfter)
	}
}

func TestEmptyFor(t *testing.T) {
	newest := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	at := func(minutesAgo int) time.Time { return newest.Add(-time.Duration(minutesAgo) * time.Minute) }

	tests := []struct {
		name   string
		points []sample
		want   time.Duration
	}{
		{name: "no points", want: 0},
		{
			name:   "the queue is not empty",
			points: []sample{{at: at(0), value: 3}, {at: at(1), value: 0}},
			want:   0,
		},
		{
			name: "empty since the last message",
			points: []sample{
				{at: at(0), value: 0}, {at: at(1), value: 0}, {at: at(2), value: 0},
				{at: at(3), value: 2}, {at: at(4), value: 0},
			},
			want: 3 * time.Minute,
		},
		{
			name:   "empty for the whole window",
			points: []sample{{at: at(0), value: 0}, {at: at(1), value: 0}, {at: at(2), value: 0}},
			want:   3 * time.Minute,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := emptyFor(tc.points); got != tc.want {
				t.Errorf("emptyFor = %s, want %s", got, tc.want)
			}
		})
	}
}

// fakeTarget records what the scaler asked for.
type fakeTarget struct {
	mu      sync.Mutex
	count   int
	sets    []int
	getErr  error
	setErr  error
	setCall int
}

func (f *fakeTarget) Instances(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count, f.getErr
}

func (f *fakeTarget) SetInstances(_ context.Context, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCall++
	if f.setErr != nil {
		return f.setErr
	}
	f.count = n
	f.sets = append(f.sets, n)
	return nil
}

func (f *fakeTarget) String() string { return "fake" }

func (f *fakeTarget) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setCall
}

type fakeSource struct {
	backlog Backlog
	err     error
}

func (f fakeSource) Backlog(context.Context, time.Duration) (Backlog, error) {
	return f.backlog, f.err
}

func (f fakeSource) String() string { return "fake" }

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestScalerReconcile(t *testing.T) {
	target := &fakeTarget{count: 1}
	s := &Scaler{
		Source: fakeSource{backlog: Backlog{Known: true, EmptyFor: time.Hour}},
		Target: target,
		Policy: testPolicy(),
		Logger: quiet(),
	}

	res, err := s.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !res.Changed || res.Desired != 0 || res.Reason != ReasonIdle {
		t.Fatalf("result = %+v, want a change down to zero", res)
	}
	if target.count != 0 {
		t.Errorf("instances = %d, want 0", target.count)
	}

	// Already where it should be: no second call to the platform.
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := target.calls(); got != 1 {
		t.Errorf("SetInstances calls = %d, want 1", got)
	}
}

// A backlog that cannot be read is not a reason to scale anything away.
func TestScalerUnreadableBacklogKeepsTheWorker(t *testing.T) {
	target := &fakeTarget{count: 0}
	s := &Scaler{
		Source: fakeSource{err: errors.New("monitoring is having a day")},
		Target: target,
		Policy: testPolicy(),
		Logger: quiet(),
	}

	res, err := s.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Desired != 1 || res.Reason != ReasonUnknown {
		t.Fatalf("result = %+v, want one instance for an unknown backlog", res)
	}
	if target.count != 1 {
		t.Errorf("instances = %d, want 1", target.count)
	}
}

func TestScalerScalesOut(t *testing.T) {
	target := &fakeTarget{count: 1}
	s := &Scaler{
		Source: fakeSource{backlog: Backlog{Known: true, Undelivered: 7, Oldest: 4 * time.Minute}},
		Target: target,
		Policy: testPolicy(),
		Logger: quiet(),
	}

	res, err := s.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Desired != 4 || res.Reason != ReasonBacklog {
		t.Errorf("result = %+v, want the cap of 4", res)
	}
}

func TestWakerRaisesTheFloorOnce(t *testing.T) {
	target := &fakeTarget{count: 0}
	w := NewWaker(target, 1, time.Minute, quiet())

	w.ensure(context.Background())
	if target.count != 1 {
		t.Fatalf("instances = %d, want the worker woken", target.count)
	}

	// Inside the cooldown nothing is asked of the platform, however many
	// deliveries arrive.
	target.count = 0
	for range 5 {
		w.ensure(context.Background())
	}
	if got := target.calls(); got != 1 {
		t.Errorf("SetInstances calls = %d, want 1 while cooling down", got)
	}
}

// A worker that is already running is left alone.
func TestWakerLeavesARunningWorkerAlone(t *testing.T) {
	target := &fakeTarget{count: 2}
	w := NewWaker(target, 1, time.Minute, quiet())

	w.ensure(context.Background())
	if got := target.calls(); got != 0 {
		t.Errorf("SetInstances calls = %d, want none", got)
	}
}

// A failed wake-up is retried by the next delivery rather than waiting out
// the cooldown.
func TestWakerForgetsTheCooldownAfterAFailure(t *testing.T) {
	target := &fakeTarget{count: 0, setErr: errors.New("quota")}
	w := NewWaker(target, 1, time.Hour, quiet())

	w.ensure(context.Background())
	target.mu.Lock()
	target.setErr = nil
	target.mu.Unlock()

	w.ensure(context.Background())
	if target.count != 1 {
		t.Errorf("instances = %d, want the retry to have woken the worker", target.count)
	}
}

func TestWakeDoesNotBlock(t *testing.T) {
	w := NewWaker(&fakeTarget{}, 1, time.Minute, quiet())

	// More wake-ups than the channel can hold: the extra ones are the same
	// request, and none of them may block the webhook that sent them.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			w.Wake()
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wake blocked")
	}
}
