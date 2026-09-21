// Package pubsub implements the queue on Google Cloud Pub/Sub, the backend
// kibitz runs on in production. See docs/queue.md.
package pubsub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	gcp "cloud.google.com/go/pubsub/v2"
	"google.golang.org/api/option"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/queue"
)

// Config configures both ends of the Pub/Sub backend.
type Config struct {
	ProjectID    string
	Topic        string
	Subscription string

	// Ordering serializes messages per pull request. Enabled by default; it is
	// what stops a review of an old commit from overtaking a newer push.
	DisableOrdering bool

	// MaxExtension is how long the client keeps extending the acknowledgement
	// deadline while a job runs. It must exceed the longest job, or Pub/Sub
	// redelivers a review that is still in progress.
	MaxExtension time.Duration

	// MaxOutstanding bounds how many messages are leased at once. It should
	// match the worker's concurrency, otherwise the client leases work it
	// cannot start and the deadline extension does the work of a queue.
	MaxOutstanding int
}

func (c Config) validate(needSubscription bool) error {
	var problems []string
	if c.ProjectID == "" {
		problems = append(problems, "project id is empty")
	}
	if c.Topic == "" {
		problems = append(problems, "topic is empty")
	}
	if needSubscription && c.Subscription == "" {
		problems = append(problems, "subscription is empty")
	}
	if len(problems) > 0 {
		return fmt.Errorf("pubsub config: %v", problems)
	}
	return nil
}

// Publisher publishes normalized events to a topic.
type Publisher struct {
	client    *gcp.Client
	publisher *gcp.Publisher
	ordering  bool
}

// NewPublisher connects to Pub/Sub. It honours PUBSUB_EMULATOR_HOST, which is
// what the local compose stack uses.
func NewPublisher(ctx context.Context, cfg Config, opts ...option.ClientOption) (*Publisher, error) {
	if err := cfg.validate(false); err != nil {
		return nil, err
	}

	client, err := gcp.NewClient(ctx, cfg.ProjectID, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating pubsub client: %w", err)
	}

	pub := client.Publisher(cfg.Topic)
	pub.EnableMessageOrdering = !cfg.DisableOrdering
	return &Publisher{client: client, publisher: pub, ordering: !cfg.DisableOrdering}, nil
}

// Publish implements [queue.Publisher].
func (p *Publisher) Publish(ctx context.Context, ev *event.ReviewEvent) (string, error) {
	if err := ev.Validate(); err != nil {
		return "", err
	}
	data, err := event.Encode(ev)
	if err != nil {
		return "", err
	}

	msg := &gcp.Message{Data: data, Attributes: queue.Attributes(ev)}
	key := queue.OrderingKey(ev)
	if p.ordering {
		msg.OrderingKey = key
	}

	id, err := p.publisher.Publish(ctx, msg).Get(ctx)
	if err != nil {
		if p.ordering {
			// Pub/Sub refuses every later message with the same ordering key
			// until publishing is resumed, so one transient failure would
			// silently wedge this pull request.
			p.publisher.ResumePublish(key)
		}
		return "", fmt.Errorf("publishing event %s: %w", ev.ID, err)
	}
	return id, nil
}

// Close flushes pending publishes and releases the client.
func (p *Publisher) Close() error {
	p.publisher.Stop()
	return p.client.Close()
}

// Subscriber pulls events from a subscription.
type Subscriber struct {
	client     *gcp.Client
	subscriber *gcp.Subscriber
	logger     *slog.Logger
}

// NewSubscriber connects to Pub/Sub and configures the lease settings.
func NewSubscriber(ctx context.Context, cfg Config, logger *slog.Logger, opts ...option.ClientOption) (*Subscriber, error) {
	if err := cfg.validate(true); err != nil {
		return nil, err
	}

	client, err := gcp.NewClient(ctx, cfg.ProjectID, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating pubsub client: %w", err)
	}

	sub := client.Subscriber(cfg.Subscription)
	if cfg.MaxExtension > 0 {
		sub.ReceiveSettings.MaxExtension = cfg.MaxExtension
	}
	if cfg.MaxOutstanding > 0 {
		sub.ReceiveSettings.MaxOutstandingMessages = cfg.MaxOutstanding
	}
	return &Subscriber{client: client, subscriber: sub, logger: logger}, nil
}

// Receive implements [queue.Subscriber].
func (s *Subscriber) Receive(ctx context.Context, fn func(context.Context, *queue.Message) error) error {
	err := s.subscriber.Receive(ctx, func(ctx context.Context, m *gcp.Message) {
		ev, err := event.Decode(m.Data)
		if err != nil {
			s.handleUndecodable(ctx, m, err)
			return
		}

		msg := &queue.Message{
			ID:         m.ID,
			Event:      ev,
			Attributes: m.Attributes,
		}
		if m.DeliveryAttempt != nil {
			msg.Deliveries = *m.DeliveryAttempt
		}

		if err := fn(ctx, msg); err != nil {
			s.logger.LogAttrs(ctx, slog.LevelWarn, "job failed; message will be redelivered",
				slog.String("message_id", m.ID),
				slog.String("event_id", ev.ID),
				slog.String("error", err.Error()),
			)
			m.Nack()
			return
		}
		m.Ack()
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("receiving from %s: %w", s.subscriber.ID(), err)
	}
	return nil
}

// handleUndecodable deals with a message this build cannot read: either from a
// newer producer or corrupted. Retrying cannot help, so it is negatively
// acknowledged and left to the subscription's dead letter policy, which is why
// that policy is not optional (see deploy/terraform).
func (s *Subscriber) handleUndecodable(ctx context.Context, m *gcp.Message, err error) {
	var unsupported *event.ErrUnsupportedSchema
	level, reason := slog.LevelError, "undecodable"
	if errors.As(err, &unsupported) {
		reason = "unsupported_schema"
	}

	s.logger.LogAttrs(ctx, level, "dropping message that cannot be decoded",
		slog.String("message_id", m.ID),
		slog.String("reason", reason),
		slog.String("error", err.Error()),
		slog.Int("bytes", len(m.Data)),
	)
	m.Nack()
}

// Close releases the client.
func (s *Subscriber) Close() error { return s.client.Close() }
