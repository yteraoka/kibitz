package scale

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Source observes the queue. window is how far back the caller wants to look,
// which bounds how long "empty" can be reported as.
type Source interface {
	Backlog(ctx context.Context, window time.Duration) (Backlog, error)
	String() string
}

// Scaler reconciles one target against one queue.
type Scaler struct {
	Source Source
	Target Target
	Policy Policy
	Logger *slog.Logger
}

// Reconcile observes the backlog once and applies the instance count it calls
// for. It returns what it decided even when applying fails, so the caller can
// log the decision that was not carried out.
func (s *Scaler) Reconcile(ctx context.Context) (Result, error) {
	policy := s.Policy.Normalize()

	// Looking back twice as far as the idle threshold is what lets a window
	// of zeroes be believed: a shorter window could not tell "empty for
	// fifteen minutes" from "empty for as long as we bothered to look".
	backlog, err := s.Source.Backlog(ctx, 2*policy.IdleAfter)
	if err != nil {
		// Not fatal: an unreadable backlog is a [Backlog] with Known false,
		// which the policy answers by keeping the worker up.
		s.log().LogAttrs(ctx, slog.LevelWarn, "could not read the backlog",
			slog.String("source", s.Source.String()),
			slog.String("error", err.Error()),
		)
		backlog = Backlog{}
	}

	desired, reason := policy.Desired(backlog)
	result := Result{Backlog: backlog, Desired: desired, Reason: reason}

	current, err := s.Target.Instances(ctx)
	if err != nil {
		return result, err
	}
	result.Current = current

	if current == desired {
		s.log().LogAttrs(ctx, slog.LevelDebug, "worker scale is unchanged",
			slog.String("target", s.Target.String()),
			slog.Int("instances", current),
			slog.String("reason", string(reason)),
			slog.Int64("backlog", backlog.Undelivered),
		)
		return result, nil
	}

	if err := s.Target.SetInstances(ctx, desired); err != nil {
		return result, err
	}
	result.Changed = true

	s.log().LogAttrs(ctx, slog.LevelInfo, "worker scaled",
		slog.String("target", s.Target.String()),
		slog.Int("from", current),
		slog.Int("to", desired),
		slog.String("reason", string(reason)),
		slog.Int64("backlog", backlog.Undelivered),
		slog.Duration("oldest_unacked", backlog.Oldest),
	)
	return result, nil
}

// Run reconciles every interval until ctx is cancelled. Errors are logged, not
// returned: a scaler that exits on the first transient API error would leave
// the worker wherever it happened to be.
func (s *Scaler) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if _, err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
			s.log().LogAttrs(ctx, slog.LevelError, "could not reconcile the worker scale",
				slog.String("target", s.Target.String()),
				slog.String("error", err.Error()),
			)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Scaler) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// Waker starts the worker as soon as there is something to do.
//
// It exists because the backlog is a metric, and a metric is minutes behind
// reality. Waiting for it would mean a pull request sitting unreviewed while
// the numbers catch up; the server knows it has published something, so it
// says so directly and the scaler is left to handle the way down.
type Waker struct {
	target   Target
	floor    int
	cooldown time.Duration
	timeout  time.Duration
	logger   *slog.Logger

	// A buffered channel of one: several deliveries arriving together are one
	// wake-up, and a full channel means one is already pending.
	signal chan struct{}

	mu   sync.Mutex
	last time.Time
	now  func() time.Time
}

// NewWaker returns a waker that keeps target at floor (at least one) instances
// whenever it is signalled, and does not call the platform more than once per
// cooldown.
func NewWaker(target Target, floor int, cooldown time.Duration, logger *slog.Logger) *Waker {
	if floor < 1 {
		floor = 1
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Waker{
		target:   target,
		floor:    floor,
		cooldown: cooldown,
		timeout:  30 * time.Second,
		logger:   logger,
		signal:   make(chan struct{}, 1),
		now:      time.Now,
	}
}

// Wake asks for the worker to be running. It never blocks and never fails:
// the webhook it is called from has a forge waiting on the other end, and a
// scaling API is not something to make that wait.
func (w *Waker) Wake() {
	select {
	case w.signal <- struct{}{}:
	default:
	}
}

// Run serves wake-ups until ctx is cancelled.
func (w *Waker) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.signal:
			w.ensure(ctx)
		}
	}
}

func (w *Waker) ensure(ctx context.Context) {
	if !w.due() {
		return
	}

	// The wake-up outlives the request that caused it: the webhook has been
	// answered by now, and the worker still has to come up.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.timeout)
	defer cancel()

	current, err := w.target.Instances(ctx)
	if err != nil {
		w.logger.LogAttrs(ctx, slog.LevelWarn, "could not read the worker scale",
			slog.String("target", w.target.String()),
			slog.String("error", err.Error()),
		)
		return
	}
	if current >= w.floor {
		return
	}
	if err := w.target.SetInstances(ctx, w.floor); err != nil {
		w.logger.LogAttrs(ctx, slog.LevelError, "could not wake the worker",
			slog.String("target", w.target.String()),
			slog.String("error", err.Error()),
		)
		// The scaler reconciles from the backlog too, so a failed wake-up
		// delays the review rather than losing it. Forget the cooldown so the
		// next delivery tries again.
		w.forget()
		return
	}
	w.logger.LogAttrs(ctx, slog.LevelInfo, "worker woken",
		slog.String("target", w.target.String()),
		slog.Int("from", current),
		slog.Int("to", w.floor),
	)
}

// due reports whether enough time has passed since the last attempt, and
// records this one.
func (w *Waker) due() bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := w.now()
	if !w.last.IsZero() && now.Sub(w.last) < w.cooldown {
		return false
	}
	w.last = now
	return true
}

func (w *Waker) forget() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.last = time.Time{}
}
