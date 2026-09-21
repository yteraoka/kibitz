// Command kibitz-server receives webhooks from GitHub, GitLab and Azure
// DevOps, verifies them, normalizes them and publishes them to the queue.
//
// See docs/architecture.md for the wider design.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/yteraoka/kibitz/internal/config"
	"github.com/yteraoka/kibitz/internal/httpx"
	"github.com/yteraoka/kibitz/internal/run"
	"github.com/yteraoka/kibitz/internal/telemetry"
)

// version is set at build time with -ldflags.
var version = "dev"

func main() {
	if err := realMain(); err != nil {
		fmt.Fprintf(os.Stderr, "kibitz-server: %v\n", err)
		os.Exit(1)
	}
}

func realMain() error {
	cfg, err := config.LoadServer(config.OSEnv)
	if err != nil {
		return fmt.Errorf("configuration:\n%w", err)
	}

	logger := telemetry.Setup(os.Stdout, telemetry.Options{
		Level:   cfg.Log.Level,
		Format:  cfg.Log.Format,
		Service: "kibitz-server",
		Version: version,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.LogAttrs(ctx, slog.LevelInfo, "starting",
		slog.String("queue_backend", cfg.Queue.Backend),
		slog.String("blob_backend", cfg.Blob.Backend),
		slog.Bool("webhooks_configured", cfg.Webhook.Configured()),
	)
	if !cfg.Webhook.Configured() {
		// Not fatal yet: the webhook handlers arrive in Phase 1, and a health
		// check should still come up so the container can be smoke tested.
		logger.LogAttrs(ctx, slog.LevelWarn, "no webhook credentials configured; inbound webhooks will be rejected")
	}

	health := httpx.NewHealth(version)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", health.Live)
	mux.HandleFunc("GET /readyz", health.Ready)
	// Phase 1 registers POST /webhook/{github,gitlab,azuredevops} here.

	handler := httpx.Chain(mux,
		httpx.RequestID,
		httpx.Recover(logger),
		httpx.Logging(logger),
	)

	var g run.Group
	g.Add(func(ctx context.Context) error {
		srv := &http.Server{
			Addr:              cfg.ListenAddr,
			Handler:           handler,
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		}
		return httpx.Serve(ctx, logger, "http", srv, cfg.ShutdownTimeout)
	})
	g.Add(func(ctx context.Context) error {
		srv := &http.Server{
			Addr:              cfg.MetricsAddr,
			Handler:           metricsMux(health),
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		}
		return httpx.Serve(ctx, logger, "metrics", srv, cfg.ShutdownTimeout)
	})

	if err := g.Run(ctx); err != nil {
		return err
	}
	logger.LogAttrs(context.Background(), slog.LevelInfo, "stopped")
	return nil
}

// metricsMux serves the metrics listener. Metrics themselves are wired up in
// Phase 3; the endpoint exists now so that scrape configuration and probes can
// be written against a stable address.
func metricsMux(health *httpx.Health) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", health.Live)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprintln(w, "# metrics are implemented in Phase 3 (see docs/roadmap.md)")
	})
	return mux
}
