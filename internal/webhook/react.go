package webhook

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
)

// WithReactor makes the receiver react to a comment as soon as the work it
// asked for is queued, so the person who wrote it sees within seconds that
// kibitz has it — rather than hearing nothing until a worker has started,
// which after a scale from zero can take a minute.
//
// The reaction is best effort. It is attempted only after the forge has had
// its answer, it is given at most timeout, and a failure is logged and
// counted but changes nothing else: the work is already queued.
func WithReactor(reactor forge.Reactor, timeout time.Duration) ReceiverOption {
	return func(r *Receiver) {
		r.reactor = reactor
		if timeout > 0 {
			r.reactTimeout = timeout
		}
	}
}

// answerAndReact sends status and then, for a comment, puts reaction on it:
// 202 and "seen" for work that was queued, 204 and "refused" for a request
// its author was not allowed to make.
//
// The order matters. A forge gives up on a delivery after a few seconds
// (GitHub after ten), and the reaction is a call to that same forge, which may
// be slow. So the answer goes out first and is flushed, with its length
// declared so that the forge holds a complete response and has no reason to
// wait for the rest; only then is the reaction made. It is made here rather
// than on a goroutine of its own because Cloud Run is free to stop giving a
// container CPU once its requests have finished, and a reaction started after
// that may never run.
func (rc *Receiver) answerAndReact(w http.ResponseWriter, r *http.Request, ev *event.ReviewEvent, status int, reaction forge.Reaction) {
	ref, ok := forge.CommentOf(ev)
	if rc.reactor == nil || !ok {
		w.WriteHeader(status)
		return
	}

	// A 204 carries no Content-Length at all; anything else declares its
	// empty body.
	if status != http.StatusNoContent {
		w.Header().Set("Content-Length", "0")
	}
	w.WriteHeader(status)
	// A writer that cannot flush still gets its answer when this returns, a
	// few seconds later at worst, which is inside every forge's timeout.
	_ = http.NewResponseController(w).Flush()

	// The forge is free to hang up once it has its answer, which cancels the
	// request's context. The reaction must outlive that.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), rc.reactTimeout)
	defer cancel()

	start := time.Now()
	err := rc.reactor.React(ctx, ref, reaction)
	attrs := []slog.Attr{
		slog.String("platform", string(ev.Source.Platform)),
		slog.String("event_id", ev.ID),
		slog.String("repository", ev.Repository.FullName),
		slog.String("comment_id", ref.CommentID),
		slog.String("reaction", string(reaction)),
		slog.Duration("duration", time.Since(start)),
	}
	outcome := "added"
	switch {
	case errors.Is(err, forge.ErrNotSupported):
		// A forge with no emoji for this: nothing is wrong, there is just
		// nothing to say it with.
		outcome = "unsupported"
		rc.logger.LogAttrs(ctx, slog.LevelDebug, "the forge has no reaction for this", attrs...)
	case err != nil:
		outcome = "failed"
		rc.logger.LogAttrs(ctx, slog.LevelWarn, "could not react to the comment; nothing else depends on it",
			append(attrs, slog.String("error", err.Error()))...)
	default:
		rc.logger.LogAttrs(ctx, slog.LevelDebug, "reacted to the comment", attrs...)
	}
	if rc.metrics != nil {
		rc.metrics.Reactions.WithLabelValues(string(ev.Source.Platform), string(reaction), outcome).Inc()
	}
}
