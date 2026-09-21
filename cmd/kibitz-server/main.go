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
	"github.com/yteraoka/kibitz/internal/policy"
	"github.com/yteraoka/kibitz/internal/queue"
	"github.com/yteraoka/kibitz/internal/queue/memory"
	"github.com/yteraoka/kibitz/internal/run"
	"github.com/yteraoka/kibitz/internal/telemetry"
	"github.com/yteraoka/kibitz/internal/webhook"
	githubhook "github.com/yteraoka/kibitz/internal/webhook/github"
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
		logger.LogAttrs(ctx, slog.LevelWarn, "no webhook credentials configured; inbound webhooks will be rejected")
	}

	publisher, err := newPublisher(cfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := publisher.Close(); err != nil {
			logger.LogAttrs(context.Background(), slog.LevelWarn, "closing publisher", slog.String("error", err.Error()))
		}
	}()

	triggers := policy.New(policy.Config{
		BotLogins:    cfg.Policy.BotLogins,
		AllowedRepos: cfg.Policy.AllowedRepos,
		Mention:      cfg.Policy.Mention,
		MaxEventAge:  cfg.Policy.MaxEventAge,
	})

	health := httpx.NewHealth(version)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", health.Live)
	mux.HandleFunc("GET /readyz", health.Ready)
	mux.Handle("POST /webhook/github", webhook.NewReceiver(
		githubhook.New(reveal(cfg.Webhook.GitHubSecrets)),
		publisher,
		triggers,
		logger,
		webhook.WithMaxBody(cfg.MaxBodyBytes),
	))
	// GitLab lands in Phase 4 and Azure DevOps in Phase 5.

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

// newPublisher builds the queue publisher for the configured backend.
func newPublisher(cfg *config.Server) (queue.Publisher, error) {
	switch cfg.Queue.Backend {
	case config.QueueMemory:
		return memory.New(), nil
	default:
		// Pub/Sub arrives in Phase 2 and SQS in Phase X (docs/roadmap.md).
		// Failing here is deliberate: a server that accepts webhooks and drops
		// them would look healthy while losing every review.
		return nil, fmt.Errorf("queue backend %q is not implemented yet; set KIBITZ_QUEUE_BACKEND=memory for now", cfg.Queue.Backend)
	}
}

// reveal unwraps secrets at the point of use.
func reveal(secrets []config.Secret) []string {
	out := make([]string, 0, len(secrets))
	for _, s := range secrets {
		out = append(out, s.Reveal())
	}
	return out
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
