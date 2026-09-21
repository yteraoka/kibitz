package webhook

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/httpx"
	"github.com/yteraoka/kibitz/internal/policy"
	"github.com/yteraoka/kibitz/internal/queue"
	"github.com/yteraoka/kibitz/internal/telemetry"

	"go.opentelemetry.io/otel/trace"
)

// Receiver is the HTTP entry point for one platform. It does as little as
// possible: verify, normalize, decide, publish, answer. Everything expensive
// happens in the worker, because forges time out webhook deliveries in
// seconds while a review takes minutes.
type Receiver struct {
	handler   Handler
	publisher queue.Publisher
	policy    *policy.Engine
	logger    *slog.Logger

	maxBody         int64
	publishTimeout  time.Duration
	publishAttempts int
	publishBackoff  time.Duration
	now             func() time.Time
	metrics         *telemetry.Metrics
	waker           Waker
}

// Waker is told that something was queued. On a platform where the worker has
// no inbound traffic to scale on, that notice is what starts it: the backlog
// metric the autoscaler otherwise runs on is minutes behind, and a review
// should not wait for it.
//
// Wake is called on the webhook's own goroutine, so it must not block.
type Waker interface {
	Wake()
}

// ReceiverOption customizes a receiver.
type ReceiverOption func(*Receiver)

// WithMaxBody caps the request body. GitHub sends up to 25 MB.
func WithMaxBody(n int64) ReceiverOption {
	return func(r *Receiver) {
		if n > 0 {
			r.maxBody = n
		}
	}
}

// WithPublishRetry configures how hard the receiver tries to publish before
// giving up. GitHub does not redeliver automatically, so a few in-process
// retries are the only thing standing between a transient queue error and a
// lost review.
func WithPublishRetry(attempts int, backoff, timeout time.Duration) ReceiverOption {
	return func(r *Receiver) {
		if attempts > 0 {
			r.publishAttempts = attempts
		}
		if backoff > 0 {
			r.publishBackoff = backoff
		}
		if timeout > 0 {
			r.publishTimeout = timeout
		}
	}
}

// WithMetrics records webhook outcomes.
func WithMetrics(m *telemetry.Metrics) ReceiverOption {
	return func(r *Receiver) { r.metrics = m }
}

// WithWaker makes the receiver announce published work, so the worker can be
// started before the queue metrics notice it.
func WithWaker(w Waker) ReceiverOption {
	return func(r *Receiver) { r.waker = w }
}

// WithReceiverClock replaces the clock used for staleness checks.
func WithReceiverClock(now func() time.Time) ReceiverOption {
	return func(r *Receiver) { r.now = now }
}

// NewReceiver wires a platform handler to the queue.
func NewReceiver(h Handler, pub queue.Publisher, pol *policy.Engine, logger *slog.Logger, opts ...ReceiverOption) *Receiver {
	rc := &Receiver{
		handler:         h,
		publisher:       pub,
		policy:          pol,
		logger:          logger,
		maxBody:         25 << 20,
		publishTimeout:  5 * time.Second,
		publishAttempts: 3,
		publishBackoff:  50 * time.Millisecond,
		now:             time.Now,
	}
	for _, opt := range opts {
		opt(rc)
	}
	return rc
}

func (rc *Receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The trace starts here and is carried on the event, so a posted comment
	// can be followed back to the delivery that caused it.
	inbound := headerCarrier(r)
	ctx := telemetry.ExtractTrace(r.Context(), inbound)
	ctx, span := telemetry.Tracer().Start(ctx, "webhook.receive",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(telemetry.AttrPlatform.String(string(rc.handler.Platform()))),
	)
	defer span.End()
	r = r.WithContext(ctx)

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := rc.readBody(w, r)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			rc.reject(r, w, nil, http.StatusRequestEntityTooLarge, "payload too large", err)
			return
		}
		rc.reject(r, w, nil, http.StatusBadRequest, "could not read body", err)
		return
	}

	// Verification happens on the raw bytes, before anything parses them.
	if err := rc.handler.Verify(r, body); err != nil {
		switch {
		case errors.Is(err, ErrNoSecrets):
			// kibitz's own misconfiguration, not the caller's fault.
			rc.reject(r, w, nil, http.StatusInternalServerError, "webhook verification is not configured", err)
		case errors.Is(err, ErrMissingSignature):
			rc.reject(r, w, nil, http.StatusBadRequest, "signature is missing", err)
		default:
			rc.reject(r, w, nil, http.StatusUnauthorized, "signature is invalid", err)
		}
		return
	}

	ev, err := rc.handler.Normalize(r, body)
	if err != nil {
		rc.reject(r, w, nil, http.StatusBadRequest, "could not normalize payload", err)
		return
	}
	if ev == nil {
		rc.skip(r, w, "unsupported_event", nil)
		return
	}
	rc.attachTrace(r.Context(), ev, inbound)
	span.SetAttributes(
		telemetry.AttrKind.String(string(ev.Kind)),
		telemetry.AttrRepository.String(ev.Repository.FullName),
		telemetry.AttrEventID.String(ev.ID),
	)

	decision := rc.policy.Evaluate(ev, rc.now())
	if !decision.Publish {
		rc.skip(r, w, string(decision.Reason), ev)
		return
	}

	msgID, err := rc.publish(r.Context(), ev)
	if err != nil {
		if rc.metrics != nil {
			rc.metrics.PublishFailures.WithLabelValues(string(ev.Source.Platform)).Inc()
		}
		// Answering 5xx is the only way to ask the forge to try again, and on
		// GitHub it at least makes the failure visible in the hook's delivery
		// log for a manual redelivery.
		rc.reject(r, w, ev, http.StatusServiceUnavailable, "could not publish event", err)
		return
	}

	if rc.metrics != nil {
		platform := string(ev.Source.Platform)
		rc.metrics.WebhooksReceived.WithLabelValues(platform, "published", "").Inc()
		rc.metrics.EventsPublished.WithLabelValues(platform, string(ev.Kind)).Inc()
	}
	if rc.waker != nil {
		rc.waker.Wake()
	}
	rc.logger.LogAttrs(r.Context(), slog.LevelInfo, "event published",
		append(rc.deliveryAttrs(r, ev),
			slog.Bool("published", true),
			slog.String("message_id", msgID),
		)...,
	)
	w.WriteHeader(http.StatusAccepted)
}

func (rc *Receiver) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, rc.maxBody)
	return io.ReadAll(r.Body)
}

// publish retries briefly: the queue being unavailable for a moment should not
// cost a review.
func (rc *Receiver) publish(ctx context.Context, ev *event.ReviewEvent) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, rc.publishTimeout)
	defer cancel()

	var err error
	backoff := rc.publishBackoff
	for attempt := 1; attempt <= rc.publishAttempts; attempt++ {
		var id string
		id, err = rc.publisher.Publish(ctx, ev)
		if err == nil {
			return id, nil
		}
		if attempt == rc.publishAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return "", errors.Join(err, ctx.Err())
		case <-time.After(backoff):
			backoff *= 2
		}
	}
	return "", err
}

// attachTrace writes the current trace context onto the event, so the worker
// continues this trace instead of starting its own.
//
// With tracing switched off there is nothing to inject, and the caller's own
// traceparent is passed through instead: kibitz not being traced is no reason
// to break someone else's trace.
func (rc *Receiver) attachTrace(ctx context.Context, ev *event.ReviewEvent, inbound telemetry.Carrier) {
	carrier := telemetry.Carrier{}
	telemetry.InjectTrace(ctx, carrier)

	if carrier["traceparent"] == "" {
		carrier = inbound
	}
	if carrier["traceparent"] == "" {
		return
	}
	ev.Trace = &event.Trace{
		TraceParent: carrier["traceparent"],
		TraceState:  carrier["tracestate"],
	}
}

// headerCarrier exposes the request headers to the propagator.
func headerCarrier(r *http.Request) telemetry.Carrier {
	carrier := telemetry.Carrier{}
	for _, key := range []string{"traceparent", "tracestate", "baggage"} {
		if v := r.Header.Get(key); v != "" {
			carrier[key] = v
		}
	}
	return carrier
}

// skip answers 204: the delivery was valid but kibitz has nothing to do.
//
// It logs at info rather than debug, because "nothing was published" is the
// answer to the question an operator actually asks — why did my pull request
// not get reviewed — and it is not an answer they can get from anywhere else.
func (rc *Receiver) skip(r *http.Request, w http.ResponseWriter, reason string, ev *event.ReviewEvent) {
	if rc.metrics != nil {
		rc.metrics.WebhooksReceived.WithLabelValues(string(rc.handler.Platform()), "skipped", reason).Inc()
	}
	rc.logger.LogAttrs(r.Context(), slog.LevelInfo, "event skipped",
		append(rc.deliveryAttrs(r, ev),
			slog.Bool("published", false),
			slog.String("reason", reason),
		)...,
	)
	w.WriteHeader(http.StatusNoContent)
}

func (rc *Receiver) reject(r *http.Request, w http.ResponseWriter, ev *event.ReviewEvent, status int, msg string, err error) {
	if rc.metrics != nil {
		rc.metrics.WebhooksReceived.WithLabelValues(
			string(rc.handler.Platform()), "rejected", strconv.Itoa(status)).Inc()
	}
	level := slog.LevelWarn
	if status >= 500 {
		level = slog.LevelError
	}
	rc.logger.LogAttrs(r.Context(), level, "webhook rejected",
		append(rc.deliveryAttrs(r, ev),
			slog.Bool("published", false),
			slog.Int("status", status),
			slog.String("error", err.Error()),
		)...,
	)
	http.Error(w, msg, status)
}

// deliveryAttrs describes one delivery for the log: what arrived, about which
// pull request, and whose doing it was. Every outcome carries the same set,
// so that one query answers "what did this delivery do" whether the event was
// published, skipped or refused.
//
// Before a payload has been normalized there is no event to describe, and the
// handler is asked for the forge's own identifiers instead: enough to find
// the delivery in the forge's own log and redeliver it.
func (rc *Receiver) deliveryAttrs(r *http.Request, ev *event.ReviewEvent) []slog.Attr {
	attrs := make([]slog.Attr, 0, 10)
	attrs = append(attrs,
		slog.String("platform", string(rc.handler.Platform())),
		slog.String("request_id", httpx.RequestIDFrom(r.Context())),
	)

	if ev == nil {
		if d, ok := rc.handler.(DeliveryDescriber); ok {
			id, name := d.Delivery(r)
			if id != "" {
				attrs = append(attrs, slog.String("delivery_id", id))
			}
			if name != "" {
				attrs = append(attrs, slog.String("event_name", name))
			}
		}
		return attrs
	}

	attrs = append(attrs,
		slog.String("delivery_id", ev.Source.DeliveryID),
		slog.String("event_id", ev.ID),
		// The forge's own vocabulary ("pull_request.synchronize"), which is
		// what the delivery log and the hook settings page show.
		slog.String("event_name", ev.Source.EventName),
		slog.String("kind", string(ev.Kind)),
		slog.String("repository", ev.Repository.FullName),
		slog.String("actor", ev.Actor.Login),
	)
	if ev.PullRequest != nil {
		attrs = append(attrs, slog.Int("pr", ev.PullRequest.Number))
	}
	if ev.Command != nil {
		attrs = append(attrs, slog.String("command", ev.Command.Name))
	}
	return attrs
}
