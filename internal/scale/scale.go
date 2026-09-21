// Package scale keeps the worker's instance count in step with the queue.
//
// A pull subscriber has no inbound traffic, so the serverless platforms it
// runs on cannot size it themselves: left alone they either keep one instance
// running forever or scale it away and leave the queue unattended. The
// backlog is the missing signal, and this package is what turns it into an
// instance count.
//
// Two paths use it, and they answer different questions:
//
//   - the server wakes the worker the moment it publishes, because a review
//     should not wait for the next metrics sample to be scraped;
//   - the scaler reconciles periodically from the backlog, which is what
//     eventually returns the worker to zero and what adds instances when work
//     piles up faster than one of them can drain it.
//
// See docs/deployment.md.
package scale

import (
	"context"
	"fmt"
	"time"
)

// Backlog is the state of the queue at one moment.
type Backlog struct {
	// Undelivered counts messages waiting to be delivered *and* messages
	// delivered but not yet acknowledged. That second half is what makes the
	// backlog safe to scale on: a worker that is three minutes into a review
	// still holds its message, so the queue does not look empty while the
	// work is in flight.
	Undelivered int64
	// EmptyFor is how long Undelivered has been zero, as far back as the
	// observed window goes. It is zero when the queue is not empty.
	EmptyFor time.Duration
	// Oldest is the age of the oldest unacknowledged message, reported for
	// logs and alerts rather than used in the decision.
	Oldest time.Duration
	// Known is false when the backlog could not be observed at all. The
	// difference between "empty" and "unknown" decides whether the worker is
	// allowed to go away, so it is not folded into Undelivered == 0.
	Known bool
}

// Reason explains a desired instance count. It is logged, so "why is the
// worker running" (or not) has an answer.
type Reason string

// Scaling reasons.
const (
	ReasonBacklog Reason = "backlog"
	ReasonCooling Reason = "cooling"
	ReasonIdle    Reason = "idle"
	ReasonUnknown Reason = "unknown"
)

// Policy turns a backlog into the number of instances the worker should have.
type Policy struct {
	// Min is the floor an idle queue falls back to. Zero is the point of the
	// exercise; an operator who would rather keep the worker warm sets 1.
	Min int
	// Max caps the fan-out.
	Max int
	// PerInstance is how many queued messages one instance is expected to
	// work through. One instance per message is not the goal: a worker runs
	// several reviews at once (KIBITZ_CONCURRENCY), so this is normally that
	// concurrency or a little more.
	PerInstance int
	// IdleAfter is how long the queue must have been empty before the worker
	// is scaled away. It has to outlast the lag between a message being
	// acknowledged and that showing up in the metric, or the worker would be
	// removed while it is still working.
	IdleAfter time.Duration
}

// DefaultPolicy is the shape of a small installation: one instance, which
// goes away after fifteen quiet minutes.
func DefaultPolicy() Policy {
	return Policy{Min: 0, Max: 3, PerInstance: 2, IdleAfter: 15 * time.Minute}
}

// Normalize fills in the values that cannot be zero and enforces Min <= Max.
func (p Policy) Normalize() Policy {
	if p.Min < 0 {
		p.Min = 0
	}
	if p.Max < 1 {
		p.Max = 1
	}
	if p.Min > p.Max {
		p.Max = p.Min
	}
	if p.PerInstance < 1 {
		p.PerInstance = 1
	}
	if p.IdleAfter <= 0 {
		p.IdleAfter = DefaultPolicy().IdleAfter
	}
	return p
}

// Desired is the instance count b calls for.
func (p Policy) Desired(b Backlog) (int, Reason) {
	p = p.Normalize()

	// Without a reading, keep the worker running. A monitoring outage must
	// not be able to stop reviews: the cost of being wrong here is one idle
	// instance, and the cost of the opposite is a queue nobody is draining.
	if !b.Known {
		return max(p.Min, 1), ReasonUnknown
	}

	if b.Undelivered > 0 {
		want := int((b.Undelivered + int64(p.PerInstance) - 1) / int64(p.PerInstance))
		return min(max(want, max(p.Min, 1)), p.Max), ReasonBacklog
	}

	// Empty, but not for long enough to be sure the metric is not simply
	// lagging behind a job that is still running.
	if b.EmptyFor < p.IdleAfter {
		return max(p.Min, 1), ReasonCooling
	}
	return p.Min, ReasonIdle
}

// Target is the thing whose instance floor is read and written: a Cloud Run
// service in production, a fake in tests.
type Target interface {
	// Instances reports the floor currently configured.
	Instances(ctx context.Context) (int, error)
	// SetInstances changes it. It is expected to be idempotent.
	SetInstances(ctx context.Context, n int) error
	// String names the target for logs.
	String() string
}

// Result is what one reconciliation did.
type Result struct {
	Backlog Backlog
	Current int
	Desired int
	Reason  Reason
	Changed bool
}

// String renders a result for a log line.
func (r Result) String() string {
	return fmt.Sprintf("backlog=%d desired=%d current=%d reason=%s changed=%t",
		r.Backlog.Undelivered, r.Desired, r.Current, r.Reason, r.Changed)
}
