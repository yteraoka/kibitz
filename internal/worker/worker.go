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
	"github.com/yteraoka/kibitz/internal/telemetry"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Job is one unit of work taken off the queue.
type Job struct {
	Event *event.ReviewEvent
	// Deliveries is how many times this message has been delivered, when the
	// backend reports it. It is what tells a handler that retrying is not
	// getting anywhere.
	Deliveries int
}

// Handler runs one job. Returning an error redelivers the message.
type Handler interface {
	Handle(ctx context.Context, job *Job) error
}

// HandlerFunc adapts a function to [Handler].
type HandlerFunc func(context.Context, *Job) error

// Handle implements [Handler].
func (f HandlerFunc) Handle(ctx context.Context, job *Job) error { return f(ctx, job) }

// Worker pulls events from the queue and hands them to a handler.
type Worker struct {
	subscriber queue.Subscriber
	handler    Handler
	logger     *slog.Logger
	jobTimeout time.Duration
	metrics    *telemetry.Metrics
}

// Option customizes a worker.
type Option func(*Worker)

// WithMetrics records job outcomes and durations.
func WithMetrics(m *telemetry.Metrics) Option {
	return func(w *Worker) { w.metrics = m }
}

// New builds a worker. Concurrency is not enforced here: the subscriber leases
// at most [config.Worker.Concurrency] messages at a time, so the queue itself
// is the limit and no second bound can disagree with it.
func New(sub queue.Subscriber, h Handler, logger *slog.Logger, cfg *config.Worker, opts ...Option) *Worker {
	w := &Worker{
		subscriber: sub,
		handler:    h,
		logger:     logger,
		jobTimeout: cfg.JobTimeout,
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
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

	ctx = continueTrace(ctx, ev)
	ctx, span := telemetry.Tracer().Start(ctx, "job",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			telemetry.AttrPlatform.String(string(ev.Source.Platform)),
			telemetry.AttrKind.String(string(ev.Kind)),
			telemetry.AttrRepository.String(ev.Repository.FullName),
			telemetry.AttrEventID.String(ev.ID),
		),
	)
	defer span.End()

	logger.LogAttrs(ctx, slog.LevelInfo, "job started")
	if w.metrics != nil {
		w.metrics.JobsInFlight.Inc()
		defer w.metrics.JobsInFlight.Dec()
	}

	if err := w.handler.Handle(ctx, &Job{Event: ev, Deliveries: msg.Deliveries}); err != nil {
		level := slog.LevelWarn
		if delay, ok := RetryDelay(err); ok {
			// Stepping aside for another worker is routine, not a failure.
			level = slog.LevelInfo
			logger = logger.With(slog.Duration("retry_after", delay))
		}
		logger.LogAttrs(ctx, level, "job failed",
			slog.String("error", err.Error()),
			slog.Duration("duration", time.Since(start)),
		)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		w.observe(ev, "failed", time.Since(start))
		return err
	}

	logger.LogAttrs(ctx, slog.LevelInfo, "job finished",
		slog.Duration("duration", time.Since(start)),
	)
	w.observe(ev, "succeeded", time.Since(start))
	return nil
}

// continueTrace picks the trace back up from the event. The queue hop is not
// an HTTP request, so the context travelled as a field on the message.
func continueTrace(ctx context.Context, ev *event.ReviewEvent) context.Context {
	if ev.Trace == nil || ev.Trace.TraceParent == "" {
		return ctx
	}
	carrier := telemetry.Carrier{"traceparent": ev.Trace.TraceParent}
	if ev.Trace.TraceState != "" {
		carrier["tracestate"] = ev.Trace.TraceState
	}
	return telemetry.ExtractTrace(ctx, carrier)
}

func (w *Worker) observe(ev *event.ReviewEvent, outcome string, d time.Duration) {
	if w.metrics == nil {
		return
	}
	w.metrics.ObserveJob(string(ev.Source.Platform), string(ev.Kind), outcome, d)
}
