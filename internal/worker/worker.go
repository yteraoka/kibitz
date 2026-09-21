// Package worker consumes normalized events and runs a review for each one.
//
// The review itself (clone, agent run, posting) lands in the rest of Phase 2;
// this is the loop around it: one message becomes one job, bounded by a
// timeout, with the outcome deciding whether the message is acknowledged.
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/yteraoka/kibitz/internal/config"
	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/queue"
)

// Handler runs one job. Returning an error redelivers the message.
type Handler interface {
	Handle(ctx context.Context, ev *event.ReviewEvent) error
}

// HandlerFunc adapts a function to [Handler].
type HandlerFunc func(context.Context, *event.ReviewEvent) error

// Handle implements [Handler].
func (f HandlerFunc) Handle(ctx context.Context, ev *event.ReviewEvent) error { return f(ctx, ev) }

// Worker pulls events from the queue and hands them to a handler.
type Worker struct {
	subscriber queue.Subscriber
	handler    Handler
	logger     *slog.Logger
	jobTimeout time.Duration
}

// New builds a worker. Concurrency is not enforced here: the subscriber leases
// at most [config.Worker.Concurrency] messages at a time, so the queue itself
// is the limit and no second bound can disagree with it.
func New(sub queue.Subscriber, h Handler, logger *slog.Logger, cfg *config.Worker) *Worker {
	return &Worker{
		subscriber: sub,
		handler:    h,
		logger:     logger,
		jobTimeout: cfg.JobTimeout,
	}
}

// Run consumes until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	return w.subscriber.Receive(ctx, w.process)
}

func (w *Worker) process(ctx context.Context, msg *queue.Message) error {
	ev := msg.Event
	start := time.Now()

	logger := w.logger.With(
		slog.String("event_id", ev.ID),
		slog.String("kind", string(ev.Kind)),
		slog.String("repository", ev.Repository.FullName),
		slog.String("message_id", msg.ID),
	)
	if ev.PullRequest != nil {
		logger = logger.With(slog.Int("pull_request", ev.PullRequest.Number))
	}
	if msg.Deliveries > 1 {
		logger = logger.With(slog.Int("delivery", msg.Deliveries))
	}

	// A job that outlives its timeout is worse than a failed one: it holds a
	// lease the queue will eventually take back and redeliver, so two copies
	// end up reviewing the same pull request.
	ctx, cancel := context.WithTimeout(ctx, w.jobTimeout)
	defer cancel()

	logger.LogAttrs(ctx, slog.LevelInfo, "job started")

	if err := w.handler.Handle(ctx, ev); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "job failed",
			slog.String("error", err.Error()),
			slog.Duration("duration", time.Since(start)),
		)
		return err
	}

	logger.LogAttrs(ctx, slog.LevelInfo, "job finished",
		slog.Duration("duration", time.Since(start)),
	)
	return nil
}
