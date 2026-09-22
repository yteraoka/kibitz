package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/store"
)

// maxThreadComments bounds how much of a conversation is handed to the agent.
// The comments nearest the question are the ones that explain it, and a
// thread that has run for fifty turns is not worth paying for in full.
const maxThreadComments = 20

// defaultSessionTTL is how long the agent's conversation about one pull
// request is remembered. Long enough for a review and the questions it
// provokes; short enough that a forgotten pull request does not keep state
// forever.
const defaultSessionTTL = 7 * 24 * time.Hour

// session returns the agent session to continue, if there is one worth
// trying.
//
// It is an optimization, not a guarantee. The agent keeps the conversation
// itself, inside a container that is disposable and often scaled to zero
// between a review and the question about it, so the session is frequently
// gone even though the id is remembered. That is why the prompt is built to
// stand on its own: the thread is in it either way, and continuing a session
// only saves the agent from reading what it already knows.
func (j *ReviewJob) session(ctx context.Context, ev *event.ReviewEvent) string {
	if j.Store == nil {
		return ""
	}

	value, err := j.Store.Get(ctx, store.SessionKey(ev))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			j.Logger.LogAttrs(ctx, slog.LevelWarn, "could not read the stored session",
				slog.String("error", err.Error()),
			)
		}
		return ""
	}
	return string(value)
}

// rememberSession records the conversation so the next question can try to
// continue it.
func (j *ReviewJob) rememberSession(ctx context.Context, ev *event.ReviewEvent, id string) {
	if j.Store == nil || id == "" {
		return
	}

	ttl := j.SessionTTL
	if ttl <= 0 {
		ttl = defaultSessionTTL
	}
	if err := j.Store.Put(ctx, store.SessionKey(ev), []byte(id), ttl); err != nil {
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "could not remember the session",
			slog.String("error", err.Error()),
		)
	}
}

// lastReviewed returns the commit kibitz last reviewed on this pull request,
// or "" when it has not reviewed one yet.
func (j *ReviewJob) lastReviewed(ctx context.Context, ev *event.ReviewEvent) string {
	if j.Store == nil {
		return ""
	}

	value, err := j.Store.Get(ctx, store.ReviewedKey(ev))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			j.Logger.LogAttrs(ctx, slog.LevelWarn, "could not read the last reviewed commit",
				slog.String("error", err.Error()),
			)
		}
		return ""
	}
	return string(value)
}

// rememberReviewed records what was reviewed, which is where the next review
// starts from.
func (j *ReviewJob) rememberReviewed(ctx context.Context, ev *event.ReviewEvent, headSHA string) {
	if j.Store == nil || headSHA == "" {
		return
	}

	ttl := j.SessionTTL
	if ttl <= 0 {
		ttl = defaultSessionTTL
	}
	if err := j.Store.Put(ctx, store.ReviewedKey(ev), []byte(headSHA), ttl); err != nil {
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "could not record the reviewed commit",
			slog.String("error", err.Error()),
		)
	}
}

// forget drops everything kibitz remembers about a pull request. It runs when
// the pull request is closed or merged: the conversation is over, and an
// "ignore" that outlives it would silently apply to nothing.
func (j *ReviewJob) forget(ctx context.Context, ev *event.ReviewEvent) error {
	if j.Store == nil {
		return nil
	}

	for _, key := range []string{store.SessionKey(ev), store.IgnoreKey(ev), store.ReviewedKey(ev)} {
		if err := j.Store.Delete(ctx, key); err != nil {
			return err
		}
	}
	j.Logger.LogAttrs(ctx, slog.LevelDebug, "forgot the pull request",
		slog.String("key", ev.Key()),
	)
	return nil
}

// ignored reports whether someone asked kibitz to stay out of this pull
// request.
func (j *ReviewJob) ignored(ctx context.Context, ev *event.ReviewEvent) bool {
	if j.Store == nil {
		return false
	}

	if _, err := j.Store.Get(ctx, store.IgnoreKey(ev)); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			// Erring towards reviewing: an unreadable store should not be
			// able to silence kibitz everywhere.
			j.Logger.LogAttrs(ctx, slog.LevelWarn, "could not read the ignore flag",
				slog.String("error", err.Error()),
			)
		}
		return false
	}
	return true
}

// ignore stops kibitz reviewing this pull request until someone asks again.
// It does not silence questions: being told to stop reviewing is not being
// told to stop talking.
func (j *ReviewJob) ignore(ctx context.Context, ev *event.ReviewEvent) error {
	if j.Store == nil {
		return nil
	}

	ttl := j.SessionTTL
	if ttl <= 0 {
		ttl = defaultSessionTTL
	}
	return j.Store.Put(ctx, store.IgnoreKey(ev), []byte("1"), ttl)
}

// unignore lifts it, which is what an explicit request to review means.
func (j *ReviewJob) unignore(ctx context.Context, ev *event.ReviewEvent) {
	if j.Store == nil {
		return
	}
	if err := j.Store.Delete(ctx, store.IgnoreKey(ev)); err != nil {
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "could not lift the ignore flag",
			slog.String("error", err.Error()),
		)
	}
}
