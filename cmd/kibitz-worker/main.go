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
	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	githubforge "github.com/yteraoka/kibitz/internal/forge/github"
	"github.com/yteraoka/kibitz/internal/httpx"
	"github.com/yteraoka/kibitz/internal/queue"
	"github.com/yteraoka/kibitz/internal/queue/memory"
	"github.com/yteraoka/kibitz/internal/queue/pubsub"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/reviewer/opencode"
	"github.com/yteraoka/kibitz/internal/run"
	"github.com/yteraoka/kibitz/internal/store"
	storefirestore "github.com/yteraoka/kibitz/internal/store/firestore"
	storememory "github.com/yteraoka/kibitz/internal/store/memory"
	"github.com/yteraoka/kibitz/internal/telemetry"
	"github.com/yteraoka/kibitz/internal/worker"
	"github.com/yteraoka/kibitz/internal/workspace"
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

	shutdownTracing, err := telemetry.SetupTracing(ctx, telemetry.TracingOptions{
		Endpoint:    cfg.Trace.Endpoint,
		Insecure:    cfg.Trace.Insecure,
		SampleRatio: cfg.Trace.SampleRatio,
		Service:     "kibitz-worker",
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
		slog.String("state_backend", cfg.State.Backend),
		slog.String("model", cfg.OpenCode.Model),
		slog.String("opencode_mode", cfg.OpenCode.Mode),
		slog.Int("concurrency", cfg.Concurrency),
		slog.Duration("job_timeout", cfg.JobTimeout),
		slog.Bool("implement_enabled", cfg.ImplementEnabled),
	)

	subscriber, err := newSubscriber(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer func() {
		if err := subscriber.Close(); err != nil {
			logger.LogAttrs(context.Background(), slog.LevelWarn, "closing subscriber", slog.String("error", err.Error()))
		}
	}()

	state, err := newStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := state.Close(); err != nil {
			logger.LogAttrs(context.Background(), slog.LevelWarn, "closing the state store", slog.String("error", err.Error()))
		}
	}()

	metrics := telemetry.NewMetrics()

	job, err := newReviewJob(cfg, logger, state, metrics)
	if err != nil {
		return err
	}

	// The guard is what makes the worker safe to run in more than one copy:
	// one delivery is handled once, one pull request at a time.
	guard := &worker.Guard{
		Next:          job,
		Store:         state,
		Notifier:      job,
		Logger:        logger,
		ClaimTTL:      cfg.JobTimeout * 2,
		DoneTTL:       7 * 24 * time.Hour,
		LockTTL:       cfg.JobTimeout,
		MaxDeliveries: cfg.MaxDeliveries,
		BotLogins:     cfg.BotLogins,
	}
	w := worker.New(subscriber, guard, logger, cfg, worker.WithMetrics(metrics))

	health := httpx.NewHealth(version)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", health.Live)
	mux.HandleFunc("GET /readyz", health.Ready)
	mux.Handle("GET /metrics", metrics.Handler())

	var g run.Group
	g.Add(func(ctx context.Context) error {
		srv := &http.Server{
			Addr:              cfg.HealthAddr,
			Handler:           httpx.Chain(mux, httpx.RequestID, httpx.Recover(logger)),
			ReadHeaderTimeout: 10 * time.Second,
		}
		return httpx.Serve(ctx, logger, "health", srv, cfg.ShutdownTimeout)
	})
	g.Add(w.Run)

	if err := g.Run(ctx); err != nil {
		return err
	}
	logger.LogAttrs(context.Background(), slog.LevelInfo, "stopped")
	return nil
}

// newSubscriber builds the queue subscriber for the configured backend.
func newSubscriber(ctx context.Context, cfg *config.Worker, logger *slog.Logger) (queue.Subscriber, error) {
	switch cfg.Queue.Backend {
	case config.QueuePubSub:
		return pubsub.NewSubscriber(ctx, pubsub.Config{
			ProjectID:    cfg.Queue.PubSub.ProjectID,
			Topic:        cfg.Queue.PubSub.Topic,
			Subscription: cfg.Queue.PubSub.Subscription,
			// Leasing more messages than the worker can start would mean the
			// deadline extension, rather than the queue, holds the backlog.
			MaxOutstanding: cfg.Concurrency,
			// The lease has to outlive the longest job, or a review still in
			// progress is handed to a second worker.
			MaxExtension: cfg.JobTimeout + time.Minute,
		}, logger)
	case config.QueueMemory:
		// Development only: nothing publishes into this worker's queue.
		return memory.New(), nil
	default:
		return nil, fmt.Errorf("queue backend %q is not implemented yet", cfg.Queue.Backend)
	}
}

// newStore builds the state store for the configured backend.
func newStore(ctx context.Context, cfg *config.Worker) (store.Store, error) {
	switch cfg.State.Backend {
	case config.StateFirestore:
		return storefirestore.New(ctx, storefirestore.Config{
			ProjectID: cfg.State.FirestoreProject,
			Database:  cfg.State.FirestoreDatabase,
		})
	case config.StateMemory:
		// Development only: a worker that forgets what it has done reviews
		// everything twice, so this is never right in production.
		return storememory.New(), nil
	default:
		return nil, fmt.Errorf("state backend %q is not implemented yet", cfg.State.Backend)
	}
}

// newReviewJob assembles the pieces one review needs: a client per platform,
// the agent engine, and the limits the output is held to.
func newReviewJob(cfg *config.Worker, logger *slog.Logger, state store.Store, metrics *telemetry.Metrics) (*worker.ReviewJob, error) {
	forges := make(map[event.Platform]forge.Client)

	if cfg.GitHub.AppID != 0 {
		client, err := githubforge.New(githubforge.Config{
			AppID:          cfg.GitHub.AppID,
			InstallationID: cfg.GitHub.InstallationID,
			PrivateKey:     cfg.GitHub.PrivateKey.Reveal(),
			BaseURL:        cfg.GitHub.BaseURL,
		})
		if err != nil {
			return nil, err
		}
		forges[event.PlatformGitHub] = client
	}
	if len(forges) == 0 {
		// Without a client the worker would acknowledge every event while
		// doing nothing, which looks healthy and reviews nothing. The
		// in-memory queue is the one exception: nothing can arrive on it, so
		// the local stack is allowed to come up unconfigured.
		if cfg.Queue.Backend != config.QueueMemory {
			return nil, fmt.Errorf("no forge credentials configured; set KIBITZ_GITHUB_APP_ID, KIBITZ_GITHUB_INSTALLATION_ID and KIBITZ_GITHUB_PRIVATE_KEY")
		}
		logger.LogAttrs(context.Background(), slog.LevelWarn,
			"no forge credentials configured; the worker will not be able to review anything")
	}

	engine := opencode.New(opencode.Config{
		Bin:         cfg.OpenCode.Bin,
		Model:       cfg.OpenCode.Model,
		ReviewAgent: cfg.OpenCode.ReviewAgent,
		AnswerAgent: cfg.OpenCode.AnswerAgent,
		Env:         vertexEnv(cfg.OpenCode.Vertex),
	}, logger)

	return &worker.ReviewJob{
		Forges: forges,
		Engine: engine,
		Workspace: workspace.Config{
			Root:    cfg.WorkspaceDir,
			Depth:   cfg.Limits.CloneDepth,
			Timeout: 5 * time.Minute,
		},
		Limits: reviewer.Limits{
			MaxComments: cfg.Limits.MaxComments,
			MinSeverity: reviewer.Severity(cfg.Limits.MinSeverity),
		},
		Logger:          logger,
		Language:        cfg.Language,
		Model:           cfg.OpenCode.Model,
		SkipDraft:       cfg.SkipDraft,
		Store:           state,
		MaxPostsPerHour: cfg.MaxPostsPerHour,
		Metrics:         metrics,
	}, nil
}

// vertexEnv passes the Vertex AI settings through to the agent. There is no
// API key: authentication is the worker's own service account.
func vertexEnv(v config.Vertex) []string {
	var env []string
	if v.ProjectID != "" {
		env = append(env, "GOOGLE_CLOUD_PROJECT="+v.ProjectID)
	}
	if v.Location != "" {
		env = append(env, "VERTEX_LOCATION="+v.Location)
	}
	return env
}
