package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Serve runs srv until ctx is cancelled, then shuts it down gracefully. It
// returns nil on a clean shutdown so that a normal SIGTERM is not reported as
// a failure.
func Serve(ctx context.Context, logger *slog.Logger, name string, srv *http.Server, shutdownTimeout time.Duration) error {
	errc := make(chan error, 1)
	go func() {
		logger.LogAttrs(ctx, slog.LevelInfo, "listening",
			slog.String("component", name),
			slog.String("addr", srv.Addr),
		)
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	logger.LogAttrs(context.WithoutCancel(ctx), slog.LevelInfo, "shutting down",
		slog.String("component", name),
	)
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		// The listener is closed either way; force it so in-flight handlers
		// cannot keep the process alive past the deadline.
		_ = srv.Close()
		return err
	}
	return <-errc
}
