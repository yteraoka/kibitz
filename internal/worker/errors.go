package worker

import (
	"errors"
	"fmt"
	"time"
)

// permanentError marks a failure that retrying cannot fix: a deleted pull
// request, a payload this build cannot read, a repository kibitz has no
// access to. The message is acknowledged instead of being redelivered until
// it lands in the dead letter queue.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return "permanent: " + e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent marks err as not worth retrying.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

// IsPermanent reports whether err is marked as permanent.
func IsPermanent(err error) bool {
	var perm *permanentError
	return errors.As(err, &perm)
}

// retryAfterError asks for the message to come back later rather than at once.
// It is how a job that lost the race for a pull request steps aside without
// spinning.
type retryAfterError struct {
	err   error
	after time.Duration
}

func (e *retryAfterError) Error() string {
	return fmt.Sprintf("retry after %s: %v", e.after, e.err)
}

func (e *retryAfterError) Unwrap() error { return e.err }

// RetryAfter marks err as worth retrying, but not immediately.
func RetryAfter(err error, after time.Duration) error {
	if err == nil {
		return nil
	}
	return &retryAfterError{err: err, after: after}
}

// RetryDelay returns the delay err asked for, and whether it asked at all.
func RetryDelay(err error) (time.Duration, bool) {
	var retry *retryAfterError
	if errors.As(err, &retry) {
		return retry.after, true
	}
	return 0, false
}
