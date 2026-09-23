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
	"strings"
	"syscall"
	"time"

	"github.com/yteraoka/kibitz/internal/config"
	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	azdoforge "github.com/yteraoka/kibitz/internal/forge/azuredevops"
	githubforge "github.com/yteraoka/kibitz/internal/forge/github"
	gitlabforge "github.com/yteraoka/kibitz/internal/forge/gitlab"
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

	"golang.org/x/oauth2/google"
)

// cloudPlatformScope is the OAuth scope Vertex AI requires.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

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
		BotLogins:     botLogins(ctx, cfg, job, logger),
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
// botIdentifier is a forge client that can say what account it posts as.
type botIdentifier interface {
	BotLogin(ctx context.Context) (string, error)
}

// botLogins works out which accounts are kibitz's own, so that the second
// loop check has something to match on without anybody writing it down.
//
// A hand-written value that is wrong is invisible until kibitz starts
// answering its own comments, so it is asked for instead. Anything configured
// is kept as well: a deployment that has just been renamed, or that posts as
// more than one account, still wants its own list honoured.
//
// Failing to ask is not fatal. The server drops kibitz's own events before
// they are ever queued, by app id rather than by name, and this is the belt
// to that pair of braces.
func botLogins(ctx context.Context, cfg *config.Worker, job *worker.ReviewJob, logger *slog.Logger) []string {
	logins := append([]string(nil), cfg.BotLogins...)

	client, ok := job.Forges[event.PlatformGitHub].(botIdentifier)
	if !ok {
		return logins
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	login, err := client.BotLogin(ctx)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "could not ask GitHub what this app posts as",
			slog.String("error", err.Error()),
		)
		return logins
	}

	logger.LogAttrs(ctx, slog.LevelInfo, "resolved the bot account", slog.String("login", login))
	for _, known := range logins {
		if strings.EqualFold(known, login) {
			return logins
		}
	}
	return append(logins, login)
}

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
	if cfg.GitLab.Configured() {
		client, err := gitlabforge.New(gitlabforge.Config{
			BaseURL: cfg.GitLab.BaseURL,
			Token:   cfg.GitLab.Token.Reveal(),
		})
		if err != nil {
			return nil, err
		}
		forges[event.PlatformGitLab] = client
	}
	if cfg.AzureDevOps.Configured() {
		client, err := azdoforge.New(azdoforge.Config{
			OrganizationURL: cfg.AzureDevOps.OrganizationURL,
			Token:           cfg.AzureDevOps.Token.Reveal(),
			TokenIsBearer:   cfg.AzureDevOps.TokenIsBearer,
		})
		if err != nil {
			return nil, err
		}
		forges[event.PlatformAzureDevOps] = client
	}
	if len(forges) == 0 {
		// Without a client the worker would acknowledge every event while
		// doing nothing, which looks healthy and reviews nothing. The
		// in-memory queue is the one exception: nothing can arrive on it, so
		// the local stack is allowed to come up unconfigured.
		if cfg.Queue.Backend != config.QueueMemory {
			return nil, fmt.Errorf("no forge credentials configured; set KIBITZ_GITHUB_APP_ID, KIBITZ_GITHUB_INSTALLATION_ID and KIBITZ_GITHUB_PRIVATE_KEY, or KIBITZ_GITLAB_TOKEN, or KIBITZ_AZDO_ORG_URL and KIBITZ_AZDO_TOKEN")
		}
		logger.LogAttrs(context.Background(), slog.LevelWarn,
			"no forge credentials configured; the worker will not be able to review anything")
	}

	budgets, err := worker.ParseBudgets(cfg.OpenCode.RepoBudgets)
	if err != nil {
		return nil, fmt.Errorf("KIBITZ_REPO_BUDGETS: %w", err)
	}

	prices, err := reviewer.ParsePrices(cfg.OpenCode.ModelPrices)
	if err != nil {
		return nil, fmt.Errorf("KIBITZ_MODEL_PRICES: %w", err)
	}

	mcp, err := opencode.ParseCatalog(cfg.MCPServers, cfg.MCPAllowlist)
	if err != nil {
		return nil, fmt.Errorf("KIBITZ_MCP_SERVERS: %w", err)
	}
	if len(mcp) > 0 {
		logger.LogAttrs(context.Background(), slog.LevelInfo, "MCP servers are available to repositories",
			slog.Any("servers", mcp.Names()))
	}

	engine := opencode.New(opencode.Config{
		Bin:            cfg.OpenCode.Bin,
		Model:          cfg.OpenCode.Model,
		ReviewAgent:    cfg.OpenCode.ReviewAgent,
		AnswerAgent:    cfg.OpenCode.AnswerAgent,
		TriageAgent:    cfg.OpenCode.TriageAgent,
		Env:            agentEnv(cfg, logger),
		EnvPassthrough: cfg.OpenCode.EnvPassthrough,
		ContextBin:     contextBin(cfg.OpenCode.ContextBin),
		MCPServers:     mcp,
		CustomProvider: vertexMaaSProvider(cfg, logger),
	}, logger)

	return &worker.ReviewJob{
		Forges:         forges,
		Engine:         engine,
		MCP:            mcp,
		GuidelineFiles: offOrList(cfg.GuidelineFiles),
		ReferenceDocs:  offOrList(cfg.ReferenceDocs),
		Workspace: workspace.Config{
			Root:    cfg.WorkspaceDir,
			Depth:   cfg.Limits.CloneDepth,
			Timeout: 5 * time.Minute,
		},
		MaxDiffLines: cfg.Limits.MaxDiffLines,
		TriageModel:  cfg.OpenCode.TriageModel,
		Limits: reviewer.Limits{
			MaxComments: cfg.Limits.MaxComments,
			MinSeverity: reviewer.Severity(cfg.Limits.MinSeverity),
		},
		Logger:           logger,
		Language:         cfg.Language,
		Model:            cfg.OpenCode.Model,
		Prices:           prices,
		Currency:         cfg.OpenCode.PriceCurrency,
		Budgets:          budgets,
		Mention:          cfg.Mention,
		SkipDraft:        cfg.SkipDraft,
		ImplementEnabled: cfg.ImplementEnabled,
		SessionTTL:       cfg.SessionTTL,
		Store:            state,
		MaxPostsPerHour:  cfg.MaxPostsPerHour,
		Metrics:          metrics,
	}, nil
}

// vertexMaaSProvider declares Vertex AI's Model as a Service partner models,
// which OpenCode's catalog does not list. GLM is the reason this exists: it is
// served on Vertex, so it needs no API key -- the credential is an OAuth token
// minted from the worker's own service account, the same identity Gemini uses.
//
// Returns nil when the configured model is not one of them, which leaves the
// generated config untouched.
func vertexMaaSProvider(cfg *config.Worker, logger *slog.Logger) *opencode.CustomProvider {
	v := cfg.OpenCode.Vertex
	provider := &opencode.CustomProvider{
		ID:      v.MaaSProviderID,
		Name:    "Vertex AI Model Garden",
		BaseURL: v.MaaSBaseURL,
	}
	if !provider.Serves(cfg.OpenCode.Model) {
		return nil
	}
	if provider.BaseURL == "" {
		provider.BaseURL = opencode.VertexMaaSBaseURL(v.ProjectID, v.Location)
	}

	provider.Token = func(ctx context.Context) (string, error) {
		source, err := google.DefaultTokenSource(ctx, cloudPlatformScope)
		if err != nil {
			return "", fmt.Errorf("finding application default credentials: %w", err)
		}
		token, err := source.Token()
		if err != nil {
			return "", fmt.Errorf("minting an access token: %w", err)
		}
		return token.AccessToken, nil
	}

	logger.LogAttrs(context.Background(), slog.LevelInfo, "declaring a Vertex AI Model Garden provider",
		slog.String("provider", provider.ID),
		slog.String("base_url", provider.BaseURL),
		slog.String("model", cfg.OpenCode.Model),
	)
	return provider
}

// agentEnv builds the environment the agent runs with: the Vertex AI settings,
// plus any provider credentials the deployment forwards by name.
//
// On Vertex there is no API key at all -- authentication is the worker's own
// service account. Providers that do need a key (GLM through Zhipu, for
// instance) are named in KIBITZ_PROVIDER_ENV and their values are taken from
// the process environment, so no credential is ever written into kibitz's
// configuration or logged.
func agentEnv(cfg *config.Worker, logger *slog.Logger) []string {
	var env []string
	if v := cfg.OpenCode.Vertex; v.ProjectID != "" {
		env = append(env, "GOOGLE_CLOUD_PROJECT="+v.ProjectID)
	}
	if v := cfg.OpenCode.Vertex; v.Location != "" {
		env = append(env, "VERTEX_LOCATION="+v.Location)
	}

	var forwarded, missing []string
	for _, name := range cfg.OpenCode.ProviderEnv {
		value, ok := os.LookupEnv(name)
		if !ok || value == "" {
			missing = append(missing, name)
			continue
		}
		env = append(env, name+"="+value)
		forwarded = append(forwarded, name)
	}

	if len(forwarded) > 0 {
		// Names only. The values are credentials.
		logger.LogAttrs(context.Background(), slog.LevelInfo, "forwarding provider credentials to the agent",
			slog.Any("variables", forwarded))
	}
	if len(missing) > 0 {
		logger.LogAttrs(context.Background(), slog.LevelWarn, "provider credentials are named but not set",
			slog.Any("variables", missing))
	}
	return env
}

// contextBin resolves the kibitz-mcp setting. "off" turns the server off,
// which is the way out for a deployment whose image does not carry it.
func contextBin(configured string) string {
	if strings.EqualFold(strings.TrimSpace(configured), "off") {
		return ""
	}
	return strings.TrimSpace(configured)
}

// offOrList resolves a list setting that also accepts "off": nil keeps the
// built-in default, "off" turns the feature off, anything else replaces the
// default.
func offOrList(configured []string) []string {
	if len(configured) == 0 {
		return nil // the built-in list
	}
	if len(configured) == 1 && strings.EqualFold(strings.TrimSpace(configured[0]), "off") {
		return []string{}
	}
	return configured
}
