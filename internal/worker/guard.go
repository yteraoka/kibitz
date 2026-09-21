package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/store"
)

// Notifier tells the pull request that its review failed. Silence is the worst
// outcome: the author waits for a review that is never coming.
type Notifier interface {
	NotifyFailure(ctx context.Context, ev *event.ReviewEvent, cause error) error
}

// Guard makes a handler safe to run against an at-least-once queue: one
// delivery is handled once, one pull request is worked on by one worker at a
// time, and a job that cannot succeed stops being retried and says so.
type Guard struct {
	Next     Handler
	Store    store.Store
	Notifier Notifier
	Logger   *slog.Logger

	// ClaimTTL is how long a delivery stays claimed while it is being worked
	// on. It bounds how long a crashed worker blocks a retry, so it is kept
	// close to the job timeout rather than to the deduplication window.
	ClaimTTL time.Duration
	// DoneTTL is how long a finished delivery is remembered.
	DoneTTL time.Duration
	// LockTTL is the initial lease on a pull request. The lease is extended
	// while the job runs.
	LockTTL time.Duration
	// MaxDeliveries is when to stop retrying and report the failure instead.
	MaxDeliveries int
	// BotLogins are kibitz's own accounts. The server already drops events
	// they authored; this is the second check, because a comment loop is
	// expensive and hard to notice.
	BotLogins []string
}

// Handle implements [Handler].
func (g *Guard) Handle(ctx context.Context, job *Job) error {
	ev := job.Event

	if g.isSelf(ev.Actor.Login) {
		g.Logger.LogAttrs(ctx, slog.LevelWarn, "dropping an event kibitz authored itself",
			slog.String("event_id", ev.ID),
			slog.String("actor", ev.Actor.Login),
		)
		return nil
	}

	claimKey := store.DeliveryKey(ev)
	claimed, err := g.Store.MarkProcessed(ctx, claimKey, g.claimTTL())
	if err != nil {
		return fmt.Errorf("claiming delivery %s: %w", ev.Source.DeliveryID, err)
	}
	if !claimed {
		done, err := g.finished(ctx, claimKey)
		if err != nil {
			return err
		}
		if done {
			g.Logger.LogAttrs(ctx, slog.LevelInfo, "delivery was already handled; skipping",
				slog.String("event_id", ev.ID),
				slog.String("delivery_id", ev.Source.DeliveryID),
			)
			return nil
		}
		// A claim with no completion behind it. Whoever made it either still
		// holds the lock, or died without releasing it -- a revision swap
		// while a review was running looks exactly like this. The lock, not
		// the claim, decides which it is; treating the claim as a finished
		// job here is how a review gets silently dropped.
		g.Logger.LogAttrs(ctx, slog.LevelInfo, "delivery was claimed but not finished; taking it over if the lock is free",
			slog.String("event_id", ev.ID),
			slog.String("delivery_id", ev.Source.DeliveryID),
		)
	}

	lease, err := g.Store.AcquireLock(ctx, store.LockKey(ev), g.lockTTL())
	if errors.Is(err, store.ErrLocked) {
		// Another worker holds this pull request. Give back a claim this call
		// created, so the redelivery is not mistaken for finished work; a
		// claim someone else owns is left alone.
		if claimed {
			g.release(ctx, claimKey)
		}
		return RetryAfter(fmt.Errorf("%s is already being reviewed", ev.Key()), 30*time.Second)
	}
	if err != nil {
		if claimed {
			g.release(ctx, claimKey)
		}
		return fmt.Errorf("locking %s: %w", ev.Key(), err)
	}

	// The lease has to outlive the job, and the job has to stop if the lease
	// is lost: continuing without it means two workers reviewing at once.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopKeepalive := g.keepalive(ctx, cancel, lease, ev)

	err = g.Next.Handle(ctx, job)
	stopKeepalive()
	if releaseErr := lease.Release(context.WithoutCancel(ctx)); releaseErr != nil && !errors.Is(releaseErr, store.ErrLeaseLost) {
		g.Logger.LogAttrs(ctx, slog.LevelWarn, "could not release the lock",
			slog.String("key", ev.Key()),
			slog.String("error", releaseErr.Error()),
		)
	}

	if err == nil {
		g.remember(ctx, claimKey)
		return nil
	}
	return g.fail(ctx, job, claimKey, err)
}

// fail decides what to do with a failed job: retry it, or give up and say so
// on the pull request.
func (g *Guard) fail(ctx context.Context, job *Job, claimKey string, cause error) error {
	ev := job.Event
	giveUp := IsPermanent(cause) || (g.MaxDeliveries > 0 && job.Deliveries >= g.MaxDeliveries)

	if !giveUp {
		// Hand the claim back so the redelivery is not mistaken for a repeat.
		g.release(ctx, claimKey)
		return cause
	}

	g.Logger.LogAttrs(ctx, slog.LevelError, "giving up on the job",
		slog.String("event_id", ev.ID),
		slog.String("repository", ev.Repository.FullName),
		slog.Int("deliveries", job.Deliveries),
		slog.Bool("permanent", IsPermanent(cause)),
		slog.String("error", cause.Error()),
	)

	// The pull request is told, because an author waiting for a review that
	// silently died is worse than an error message.
	if g.Notifier != nil {
		if err := g.Notifier.NotifyFailure(context.WithoutCancel(ctx), ev, cause); err != nil {
			g.Logger.LogAttrs(ctx, slog.LevelWarn, "could not report the failure on the pull request",
				slog.String("error", err.Error()),
			)
		}
	}

	g.remember(ctx, claimKey)
	return nil
}

// keepalive extends the lease until the job finishes. If the lease is lost,
// the job's context is cancelled: whatever took the lock is now the owner.
func (g *Guard) keepalive(ctx context.Context, cancel context.CancelFunc, lease store.Lease, ev *event.ReviewEvent) func() {
	interval := g.lockTTL() / 3
	if interval <= 0 {
		interval = time.Minute
	}

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := lease.Extend(ctx, g.lockTTL()); err != nil {
					g.Logger.LogAttrs(ctx, slog.LevelError, "lost the lock while working; stopping the job",
						slog.String("key", ev.Key()),
						slog.String("error", err.Error()),
					)
					cancel()
					return
				}
			}
		}
	}()

	var stopped bool
	return func() {
		if !stopped {
			stopped = true
			close(done)
		}
	}
}

// finished reports whether the delivery record says the job completed, rather
// than merely that someone started it. A store that cannot be read is treated
// as unfinished and retried: dropping a review is worse than running one
// twice, and the lock still prevents the second run from overlapping.
func (g *Guard) finished(ctx context.Context, key string) (bool, error) {
	value, err := g.Store.Get(ctx, key)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// It expired between the claim and this read.
		return false, nil
	case err != nil:
		return false, fmt.Errorf("reading the delivery record: %w", err)
	}
	return string(value) == store.MarkerDone, nil
}

// remember marks a delivery as finished for the full deduplication window.
func (g *Guard) remember(ctx context.Context, key string) {
	ctx = context.WithoutCancel(ctx)
	if err := g.Store.Put(ctx, key, []byte(store.MarkerDone), g.doneTTL()); err != nil {
		g.Logger.LogAttrs(ctx, slog.LevelWarn, "could not record the delivery as handled",
			slog.String("key", key),
			slog.String("error", err.Error()),
		)
	}
}

// release gives a claim back so that a redelivery is treated as new work.
func (g *Guard) release(ctx context.Context, key string) {
	ctx = context.WithoutCancel(ctx)
	if err := g.Store.Delete(ctx, key); err != nil {
		g.Logger.LogAttrs(ctx, slog.LevelWarn, "could not release the delivery claim; the retry will be skipped",
			slog.String("key", key),
			slog.String("error", err.Error()),
		)
	}
}

func (g *Guard) isSelf(login string) bool {
	for _, bot := range g.BotLogins {
		if strings.EqualFold(bot, login) && login != "" {
			return true
		}
	}
	return false
}

func (g *Guard) claimTTL() time.Duration {
	if g.ClaimTTL > 0 {
		return g.ClaimTTL
	}
	return 30 * time.Minute
}

func (g *Guard) doneTTL() time.Duration {
	if g.DoneTTL > 0 {
		return g.DoneTTL
	}
	return 7 * 24 * time.Hour
}

func (g *Guard) lockTTL() time.Duration {
	if g.LockTTL > 0 {
		return g.LockTTL
	}
	return 15 * time.Minute
}
