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
	"strings"
	"syscall"

	"github.com/yteraoka/kibitz/internal/config"
	"github.com/yteraoka/kibitz/internal/httpx"
	"github.com/yteraoka/kibitz/internal/policy"
	"github.com/yteraoka/kibitz/internal/queue"
	"github.com/yteraoka/kibitz/internal/queue/memory"
	"github.com/yteraoka/kibitz/internal/queue/pubsub"
	"github.com/yteraoka/kibitz/internal/run"
	"github.com/yteraoka/kibitz/internal/scale"
	"github.com/yteraoka/kibitz/internal/telemetry"
	"github.com/yteraoka/kibitz/internal/webhook"
	azdohook "github.com/yteraoka/kibitz/internal/webhook/azuredevops"
	githubhook "github.com/yteraoka/kibitz/internal/webhook/github"
	gitlabhook "github.com/yteraoka/kibitz/internal/webhook/gitlab"
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

	shutdownTracing, err := telemetry.SetupTracing(ctx, telemetry.TracingOptions{
		Endpoint:    cfg.Trace.Endpoint,
		Insecure:    cfg.Trace.Insecure,
		SampleRatio: cfg.Trace.SampleRatio,
		Service:     "kibitz-server",
		Version:     version,
	})
	if err != nil {
		return err
	}
	defer func() {
		// Flushing on the way out; otherwise the traces that explain a crash
		// are the ones that never leave the process.
		if err := shutdownTracing(context.Background()); err != nil {
			logger.LogAttrs(context.Background(), slog.LevelWarn, "flushing traces", slog.String("error", err.Error()))
		}
	}()

	logger.LogAttrs(ctx, slog.LevelInfo, "starting",
		slog.String("queue_backend", cfg.Queue.Backend),
		slog.String("blob_backend", cfg.Blob.Backend),
		slog.Bool("webhooks_configured", cfg.Webhook.Configured()),
	)
	if !cfg.Webhook.Configured() {
		logger.LogAttrs(ctx, slog.LevelWarn, "no webhook credentials configured; inbound webhooks will be rejected")
	}

	// The fingerprints are what make "the secret is correct" checkable: the
	// same digest can be taken of the value in the forge's settings, and the
	// two either match or they do not. See docs/deployment.md.
	githubHook := githubhook.New(reveal(cfg.Webhook.GitHubSecrets))
	if fps := githubHook.Fingerprints(); len(fps) > 0 {
		logger.LogAttrs(ctx, slog.LevelInfo, "github webhook secrets loaded",
			slog.Int("count", len(fps)),
			slog.String("fingerprints", strings.Join(fps, " ")),
		)
	}

	publisher, err := newPublisher(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := publisher.Close(); err != nil {
			logger.LogAttrs(context.Background(), slog.LevelWarn, "closing publisher", slog.String("error", err.Error()))
		}
	}()

	triggers := policy.New(policy.Config{
		AppID:        cfg.Policy.AppID,
		BotLogins:    cfg.Policy.BotLogins,
		AllowedRepos: cfg.Policy.AllowedRepos,
		Mention:      cfg.Policy.Mention,
		Keywords:     cfg.Policy.Keywords,
		MaxEventAge:  cfg.Policy.MaxEventAge,
	})

	metrics := telemetry.NewMetrics()
	health := httpx.NewHealth(version)

	// With the worker allowed to scale to zero, publishing is not enough on
	// its own: something has to tell the platform to start it. Doing it here
	// costs one API call per burst of deliveries and saves the review from
	// waiting for the next backlog sample.
	waker, err := newWaker(ctx, cfg, logger)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", health.Live)
	mux.HandleFunc("GET /readyz", health.Ready)
	// Where only one port is available, metrics ride on the main listener.
	// The counters carry no repository or user names, so this exposes
	// aggregate numbers and nothing else.
	if cfg.MetricsAddr == cfg.ListenAddr {
		mux.Handle("GET /metrics", metrics.Handler())
	}
	receiverOpts := []webhook.ReceiverOption{
		webhook.WithMaxBody(cfg.MaxBodyBytes),
		webhook.WithMetrics(metrics),
	}
	if waker != nil {
		// Added conditionally: a nil *scale.Waker passed as the interface
		// would not be a nil interface, and the receiver would call it.
		receiverOpts = append(receiverOpts, webhook.WithWaker(waker))
	}
	mux.Handle("POST /webhook/github", webhook.NewReceiver(
		githubHook,
		publisher,
		triggers,
		logger,
		receiverOpts...,
	))
	mux.Handle("POST /webhook/gitlab", webhook.NewReceiver(
		gitlabhook.New(reveal(cfg.Webhook.GitLabTokens), reveal(cfg.Webhook.GitLabSigningTokens)),
		publisher,
		triggers,
		logger,
		receiverOpts...,
	))
	mux.Handle("POST /webhook/azure-devops", webhook.NewReceiver(
		azdohook.New(
			cfg.Webhook.AzureDevOpsUser,
			reveal(cfg.Webhook.AzureDevOpsPasswords),
			azdohook.WithHeader(cfg.Webhook.AzureDevOpsHeader, reveal(cfg.Webhook.AzureDevOpsHeaderValues)),
		),
		publisher,
		triggers,
		logger,
		receiverOpts...,
	))

	handler := httpx.Chain(mux,
		httpx.RequestID,
		httpx.Recover(logger),
		httpx.Logging(logger),
	)

	var g run.Group
	if waker != nil {
		g.Add(waker.Run)
	}
	g.Add(func(ctx context.Context) error {
		srv := &http.Server{
			Addr:              cfg.ListenAddr,
			Handler:           handler,
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		}
		return httpx.Serve(ctx, logger, "http", srv, cfg.ShutdownTimeout)
	})
	if cfg.MetricsAddr != cfg.ListenAddr {
		g.Add(func(ctx context.Context) error {
			srv := &http.Server{
				Addr:              cfg.MetricsAddr,
				Handler:           metricsMux(health, metrics),
				ReadHeaderTimeout: cfg.ReadHeaderTimeout,
			}
			return httpx.Serve(ctx, logger, "metrics", srv, cfg.ShutdownTimeout)
		})
	}

	if err := g.Run(ctx); err != nil {
		return err
	}
	logger.LogAttrs(context.Background(), slog.LevelInfo, "stopped")
	return nil
}

// newWaker builds the thing that starts the worker when work is queued, or
// returns nil when no instance count is under kibitz's control.
func newWaker(ctx context.Context, cfg *config.Server, logger *slog.Logger) (*scale.Waker, error) {
	if !cfg.Scale.Enabled() {
		return nil, nil
	}

	pool, err := scale.NewCloudRunWorkerPool(ctx, cfg.Scale.ProjectID, cfg.Scale.Region, cfg.Scale.WorkerPool)
	if err != nil {
		return nil, fmt.Errorf("worker autoscaling: %w", err)
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "worker wake-up is enabled",
		slog.String("worker_pool", pool.String()),
	)
	return scale.NewWaker(pool, 1, cfg.Scale.WakeCooldown, logger), nil
}

// newPublisher builds the queue publisher for the configured backend.
func newPublisher(ctx context.Context, cfg *config.Server) (queue.Publisher, error) {
	switch cfg.Queue.Backend {
	case config.QueuePubSub:
		return pubsub.NewPublisher(ctx, pubsub.Config{
			ProjectID: cfg.Queue.PubSub.ProjectID,
			Topic:     cfg.Queue.PubSub.Topic,
		})
	case config.QueueMemory:
		// Development only: events go nowhere a worker can reach them.
		return memory.New(), nil
	default:
		// SQS arrives in Phase X (docs/roadmap.md). Failing here is
		// deliberate: a server that accepts webhooks and drops them would look
		// healthy while losing every review.
		return nil, fmt.Errorf("queue backend %q is not implemented yet", cfg.Queue.Backend)
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

// metricsMux serves the metrics listener, kept separate from the webhook
// listener so that scraping never competes with deliveries and so the metrics
// port can stay off the public network.
func metricsMux(health *httpx.Health, metrics *telemetry.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", health.Live)
	mux.Handle("GET /metrics", metrics.Handler())
	return mux
}
