// Package store holds the small amount of state a worker needs to be safe to
// run more than once: which deliveries were already handled, which pull
// request is being worked on right now, and how much has been posted lately.
//
// See docs/architecture.md for the key layout.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
)

// Errors returned by every implementation.
var (
	// ErrLocked reports that someone else holds the lock.
	ErrLocked = errors.New("store: already locked")
	// ErrNotFound reports a missing or expired key.
	ErrNotFound = errors.New("store: key not found")
	// ErrLeaseLost reports that a lease was taken over or already released,
	// which means the work it protected may now be running twice.
	ErrLeaseLost = errors.New("store: lease is no longer held")
)

// Lease is a held lock.
type Lease interface {
	// Extend pushes the expiry out. A job that runs longer than its lease
	// would otherwise let a second worker start on the same pull request.
	Extend(ctx context.Context, ttl time.Duration) error
	// Release gives the lock up. Releasing a lease that was already taken
	// over returns [ErrLeaseLost].
	Release(ctx context.Context) error
}

// Store is the state a worker keeps between jobs.
type Store interface {
	// MarkProcessed records that key has been handled and reports whether
	// this caller was the first. It is the guard against the queue's
	// at-least-once delivery turning into a second review.
	MarkProcessed(ctx context.Context, key string, ttl time.Duration) (first bool, err error)

	// AcquireLock takes a lease, or returns [ErrLocked].
	AcquireLock(ctx context.Context, key string, ttl time.Duration) (Lease, error)

	// Get returns a stored value, or [ErrNotFound].
	Get(ctx context.Context, key string) ([]byte, error)

	// Put stores a value with an expiry. A zero ttl means it does not expire.
	Put(ctx context.Context, key string, value []byte, ttl time.Duration) error

	// Delete removes a key. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error

	// Incr adds delta to a counter and returns the new value, creating it
	// with the given expiry when it does not exist yet. It is how posting
	// rate limits and token budgets are tracked.
	Incr(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error)

	Close() error
}

// Key prefixes. They are spelled out here so that every implementation, and
// anyone reading a dump of the store, sees the same vocabulary.
const (
	prefixDelivery = "delivery"
	prefixJob      = "job"
	prefixLock     = "lock"
	prefixSession  = "session"
	prefixPosts    = "posts"
)

// DeliveryKey identifies one webhook delivery. It is what stops a redelivered
// webhook, or a redelivered queue message, from reviewing twice.
func DeliveryKey(ev *event.ReviewEvent) string {
	return fmt.Sprintf("%s:%s:%s", prefixDelivery, ev.Source.Platform, ev.Source.DeliveryID)
}

// JobKey identifies a unit of work: this kind of review, of this commit, of
// this pull request. Two different deliveries that would produce the same
// review share a job key.
func JobKey(ev *event.ReviewEvent, headSHA string) string {
	return fmt.Sprintf("%s:%s:%s:%d:%s", prefixJob, ev.Source.Platform,
		ev.Repository.FullName, pullRequestNumber(ev), headSHA)
}

// LockKey serializes work on one pull request.
func LockKey(ev *event.ReviewEvent) string {
	return fmt.Sprintf("%s:%s", prefixLock, ev.Key())
}

// SessionKey stores the agent session for a pull request, so a follow-up
// question continues the conversation instead of starting over.
func SessionKey(ev *event.ReviewEvent) string {
	return fmt.Sprintf("%s:%s", prefixSession, ev.Key())
}

// PostsKey counts what kibitz has posted to one pull request within an hour,
// which is the backstop against a comment loop.
func PostsKey(ev *event.ReviewEvent, now time.Time) string {
	return fmt.Sprintf("%s:%s:%s", prefixPosts, ev.Key(), now.UTC().Format("2006010215"))
}

func pullRequestNumber(ev *event.ReviewEvent) int {
	if ev.PullRequest == nil {
		return 0
	}
	return ev.PullRequest.Number
}
