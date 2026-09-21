// Package queue abstracts the message queue. The same code runs on Cloud
// Pub/Sub and on Amazon SQS; only the implementation behind these interfaces
// changes. See docs/queue.md.
package queue

import (
	"context"
	"strconv"

	"github.com/yteraoka/kibitz/internal/event"
)

// Message attribute keys. Attributes are readable without decoding the body,
// which is what filters, metrics and tracing use.
const (
	AttrPlatform    = "platform"
	AttrKind        = "kind"
	AttrRepository  = "repository"
	AttrPullRequest = "pull_request"
	AttrDeliveryID  = "delivery_id"
	AttrTraceParent = "traceparent"
)

// Publisher sends normalized events to the queue.
type Publisher interface {
	// Publish returns the queue's own message id.
	Publish(ctx context.Context, ev *event.ReviewEvent) (string, error)
	Close() error
}

// Subscriber receives events from the queue.
type Subscriber interface {
	// Receive calls fn for each message until ctx is cancelled. Returning nil
	// from fn acknowledges the message; returning an error negatively
	// acknowledges it so the queue redelivers. The implementation keeps the
	// acknowledgement deadline extended while fn runs.
	Receive(ctx context.Context, fn func(context.Context, *Message) error) error
	Close() error
}

// Message is one delivery.
type Message struct {
	// ID is the queue's message id, not the event id.
	ID         string
	Event      *event.ReviewEvent
	Attributes map[string]string
	// Deliveries is how many times this message has been delivered, when the
	// backend reports it. Zero means unknown.
	Deliveries int
}

// Attributes derives the transport attributes for an event.
func Attributes(ev *event.ReviewEvent) map[string]string {
	if ev == nil {
		return nil
	}
	attrs := map[string]string{
		AttrPlatform:   string(ev.Source.Platform),
		AttrKind:       string(ev.Kind),
		AttrRepository: ev.Repository.FullName,
		AttrDeliveryID: ev.Source.DeliveryID,
	}
	if ev.PullRequest != nil {
		attrs[AttrPullRequest] = strconv.Itoa(ev.PullRequest.Number)
	}
	if ev.Trace != nil && ev.Trace.TraceParent != "" {
		attrs[AttrTraceParent] = ev.Trace.TraceParent
	}
	return attrs
}

// OrderingKey serializes work per pull request: Pub/Sub uses it as the
// ordering key and SQS as the message group id.
func OrderingKey(ev *event.ReviewEvent) string { return ev.Key() }
