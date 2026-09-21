// Package memory is an in-process queue for local development and tests. It
// keeps the same semantics as the real backends where it matters: delivery is
// at-least-once, a negative acknowledgement redelivers, and messages for one
// pull request are handled in order.
package memory

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/queue"
)

// ErrClosed is returned once the queue has been closed.
var ErrClosed = errors.New("queue is closed")

// Queue is an in-memory [queue.Publisher] and [queue.Subscriber].
type Queue struct {
	// MaxDeliveries bounds redelivery so a permanently failing message cannot
	// loop forever, standing in for the dead letter queue. Zero means 5.
	MaxDeliveries int

	mu       sync.Mutex
	closed   bool
	nextID   int
	messages []*delivery
	waiting  chan struct{}
	// Dead holds messages that exhausted their delivery attempts.
	dead []*queue.Message
}

type delivery struct {
	msg        *queue.Message
	deliveries int
}

// New creates an empty queue.
func New() *Queue {
	return &Queue{waiting: make(chan struct{}, 1)}
}

// Publish implements [queue.Publisher].
func (q *Queue) Publish(_ context.Context, ev *event.ReviewEvent) (string, error) {
	if err := ev.Validate(); err != nil {
		return "", err
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return "", ErrClosed
	}

	q.nextID++
	id := fmt.Sprintf("mem-%d", q.nextID)
	// Copy the event so a later mutation by the publisher cannot be observed
	// by the subscriber, which is what a real transport gives you for free.
	encoded, err := event.Encode(ev)
	if err != nil {
		return "", err
	}
	decoded, err := event.Decode(encoded)
	if err != nil {
		return "", err
	}

	q.messages = append(q.messages, &delivery{msg: &queue.Message{
		ID:         id,
		Event:      decoded,
		Attributes: queue.Attributes(ev),
	}})
	q.notify()
	return id, nil
}

// Receive implements [queue.Subscriber]. Messages are handled one at a time,
// in publication order.
func (q *Queue) Receive(ctx context.Context, fn func(context.Context, *queue.Message) error) error {
	for {
		// Stop promptly even while messages are still queued: shutdown must
		// not wait for the backlog to drain.
		if err := ctx.Err(); err != nil {
			return nil
		}

		d, ok := q.pop()
		if !ok {
			select {
			case <-ctx.Done():
				return nil
			case <-q.waiting:
				continue
			}
		}

		d.deliveries++
		d.msg.Deliveries = d.deliveries
		if err := fn(ctx, d.msg); err != nil {
			q.requeue(d)
			continue
		}
	}
}

func (q *Queue) pop() (*delivery, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.messages) == 0 {
		return nil, false
	}
	d := q.messages[0]
	q.messages = q.messages[1:]
	return d, true
}

func (q *Queue) requeue(d *delivery) {
	limit := q.MaxDeliveries
	if limit <= 0 {
		limit = 5
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if d.deliveries >= limit {
		q.dead = append(q.dead, d.msg)
		return
	}
	q.messages = append(q.messages, d)
	q.notify()
}

// notify wakes a waiting receiver. The caller must hold the lock.
func (q *Queue) notify() {
	select {
	case q.waiting <- struct{}{}:
	default:
	}
}

// Len reports how many messages are waiting.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.messages)
}

// Dead returns the messages that exhausted their delivery attempts.
func (q *Queue) Dead() []*queue.Message {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]*queue.Message(nil), q.dead...)
}

// Close implements both interfaces.
func (q *Queue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	return nil
}
