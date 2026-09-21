// Command kibitz-worker consumes normalized review events from the queue and
// runs the agent engine against the pull request they refer to.
//
// See docs/worker.md for the wider design.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yteraoka/kibitz/internal/config"
	"github.com/yteraoka/kibitz/internal/httpx"
	"github.com/yteraoka/kibitz/internal/run"
	"github.com/yteraoka/kibitz/internal/telemetry"
)

// version is set at build time with -ldflags.
var version = "dev"

func main() {
	if err := realMain(); err != nil {
		fmt.Fprintf(os.Stderr, "kibitz-worker: %v\n", err)
		os.Exit(1)
	}
}

func realMain() error {
	cfg, err := config.LoadWorker(config.OSEnv)
	if err != nil {
		return fmt.Errorf("configuration:\n%w", err)
	}

	logger := telemetry.Setup(os.Stdout, telemetry.Options{
		Level:   cfg.Log.Level,
		Format:  cfg.Log.Format,
		Service: "kibitz-worker",
		Version: version,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.LogAttrs(ctx, slog.LevelInfo, "starting",
		slog.String("queue_backend", cfg.Queue.Backend),
		slog.String("state_backend", cfg.State.Backend),
		slog.String("model", cfg.OpenCode.Model),
		slog.String("opencode_mode", cfg.OpenCode.Mode),
		slog.Int("concurrency", cfg.Concurrency),
		slog.Duration("job_timeout", cfg.JobTimeout),
		slog.Bool("implement_enabled", cfg.ImplementEnabled),
	)

	health := httpx.NewHealth(version)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", health.Live)
	mux.HandleFunc("GET /readyz", health.Ready)

	var g run.Group
	g.Add(func(ctx context.Context) error {
		srv := &http.Server{
			Addr:              cfg.HealthAddr,
			Handler:           httpx.Chain(mux, httpx.RequestID, httpx.Recover(logger)),
			ReadHeaderTimeout: 10 * time.Second,
		}
		return httpx.Serve(ctx, logger, "health", srv, cfg.ShutdownTimeout)
	})
	g.Add(func(ctx context.Context) error { return consume(ctx, logger, cfg) })

	if err := g.Run(ctx); err != nil {
		return err
	}
	logger.LogAttrs(context.Background(), slog.LevelInfo, "stopped")
	return nil
}

// consume will subscribe to the queue and run review jobs. The subscriber and
// the review engine arrive in Phase 2 (see docs/roadmap.md); until then the
// worker starts, reports its configuration and waits for a signal, which is
// what the compose smoke test exercises.
func consume(ctx context.Context, logger *slog.Logger, cfg *config.Worker) error {
	logger.LogAttrs(ctx, slog.LevelWarn, "queue subscriber is not implemented yet; idling",
		slog.String("queue_backend", cfg.Queue.Backend),
	)
	<-ctx.Done()
	return nil
}
