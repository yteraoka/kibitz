// Package config loads kibitz configuration from the environment and validates
// it at startup, so that a misconfigured deployment fails immediately and says
// everything that is wrong rather than failing later on the first webhook.
package config

import (
	"errors"
	"log/slog"
	"time"

	"github.com/yteraoka/kibitz/internal/policy"
)

// Redacted is what a [Secret] renders as in logs and error messages.
const Redacted = "[REDACTED]"

// Secret is a string that never renders itself. Call [Secret.Reveal] at the
// point of use; everything else (fmt, slog, JSON via the String method) sees
// only [Redacted].
type Secret string

// String implements [fmt.Stringer].
func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return Redacted
}

// LogValue implements [slog.LogValuer].
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

// Reveal returns the underlying value.
func (s Secret) Reveal() string { return string(s) }

// Queue backends.
const (
	QueuePubSub = "pubsub"
	QueueSQS    = "sqs"
	QueueMemory = "memory"
)

// State store backends.
const (
	StateFirestore = "firestore"
	StateDynamoDB  = "dynamodb"
	StateMemory    = "memory"
)

// Blob store backends.
const (
	BlobGCS  = "gcs"
	BlobS3   = "s3"
	BlobNone = "none"
)

// Autoscaler backends. See docs/deployment.md.
const (
	ScaleCloudRun = "cloudrun"
	ScaleNone     = "none"
)

// OpenCode execution modes. See docs/worker.md.
const (
	OpenCodeModeRun    = "run"
	OpenCodeModeAttach = "attach"
)

// Trace configures trace export. With no endpoint, tracing is off and the
// instrumentation costs nothing.
type Trace struct {
	Endpoint    string
	Insecure    bool
	SampleRatio float64
}

// Log holds logging settings shared by both binaries.
type Log struct {
	Level  slog.Level
	Format string // json or text
}

// PubSub holds Cloud Pub/Sub settings. Subscription is only read by the worker.
type PubSub struct {
	ProjectID    string
	Topic        string
	Subscription string
}

// SQS holds Amazon SQS settings.
type SQS struct {
	QueueURL string
	Region   string
}

// Queue selects and configures the message queue backend.
type Queue struct {
	Backend string
	PubSub  PubSub
	SQS     SQS
}

// Blob configures the object store used for the claim-check pattern.
type Blob struct {
	Backend string
	Bucket  string
	// ClaimCheckThreshold is the raw payload size above which the payload is
	// offloaded to the blob store instead of travelling in the message.
	ClaimCheckThreshold int64
}

// State configures the state store used for idempotency, locks and sessions.
type State struct {
	Backend           string
	FirestoreProject  string
	FirestoreDatabase string
	DynamoDBTable     string
	AWSRegion         string
}

// Scale configures the worker autoscaler. A pull subscriber has no inbound
// traffic for the platform to scale on, so kibitz sizes it from the queue
// itself: see docs/deployment.md.
type Scale struct {
	// Backend is "cloudrun", or "none" to leave the instance count alone.
	Backend string
	// ProjectID, Region and WorkerPool address the worker pool.
	ProjectID  string
	Region     string
	WorkerPool string
	// MinInstances is where an idle queue lands. Zero is the point; set 1 to
	// keep the worker warm and still have it scale out under load.
	MinInstances int
	// MaxInstances caps the fan-out.
	MaxInstances int
	// MessagesPerInstance is how many queued messages one instance is
	// expected to absorb before another is added. It normally matches
	// KIBITZ_CONCURRENCY.
	MessagesPerInstance int
	// IdleAfter is how long the queue must be empty before the worker is
	// scaled away. It has to outlast Cloud Monitoring's own delay, which is
	// why the default is generous.
	IdleAfter time.Duration
	// Interval is how often the scaler reconciles.
	Interval time.Duration
	// WakeCooldown bounds how often the server asks the platform to start
	// the worker. It is not a delay on the first wake-up.
	WakeCooldown time.Duration
}

// Enabled reports whether an instance count is managed at all.
func (s Scale) Enabled() bool { return s.Backend == ScaleCloudRun }

// Webhook holds the credentials used to verify inbound webhooks. Each platform
// accepts a list so secrets can be rotated without downtime.
type Webhook struct {
	GitHubSecrets []Secret
	// GitLabTokens are the shared secrets GitLab sends in X-Gitlab-Token.
	// They say who sent a delivery and nothing about what it contains.
	GitLabTokens []Secret
	// GitLabSigningTokens verify the HMAC GitLab 19 and later send, which
	// does cover the body. Prefer them where the instance is new enough.
	GitLabSigningTokens  []Secret
	AzureDevOpsUser      string
	AzureDevOpsPasswords []Secret
	// AzureDevOpsHeader and AzureDevOpsHeaderValues are an optional second
	// credential. Azure DevOps does not sign its deliveries, so basic auth is
	// all there is; requiring a fixed header as well narrows what a leaked
	// password on its own is good for.
	AzureDevOpsHeader       string
	AzureDevOpsHeaderValues []Secret
}

// Configured reports whether at least one platform can be verified. A server
// with no webhook credentials at all would accept nothing, which is always a
// misconfiguration.
func (w Webhook) Configured() bool {
	return len(w.GitHubSecrets) > 0 || len(w.GitLabTokens) > 0 ||
		len(w.GitLabSigningTokens) > 0 || len(w.AzureDevOpsPasswords) > 0 ||
		len(w.AzureDevOpsHeaderValues) > 0
}

// Policy holds the trigger rules the server applies before publishing.
type Policy struct {
	// AppID is kibitz's own GitHub App id. Where a payload names the app that
	// acted, it is what recognizes kibitz's own writing — no account name to
	// keep in step, and nothing that breaks when the app is renamed. It is not
	// a secret: it is the number in the app's settings URL.
	AppID string
	// BotLogins are the accounts kibitz itself posts as. Events they author are
	// dropped so the bot never reacts to its own comments. With AppID set this
	// is a fallback, for the events GitHub does not attribute to an app.
	BotLogins []string
	// AllowedRepos are glob patterns matched against "owner/name".
	AllowedRepos []string
	Mention      string
	// Keywords gate pull request events. Empty means every pull request is
	// reviewed; setting it makes a repository opt in per pull request, which
	// is also what keeps the queue empty enough for the worker to scale to
	// zero.
	Keywords []string
	// MaxEventAge bounds how old a webhook may be. It is disabled by default:
	// the signature already authenticates the payload, duplicate deliveries
	// are suppressed by delivery id in the worker, and an operator pressing
	// "Redeliver" on a failed hook hours later must still get a review rather
	// than a silent no-op.
	MaxEventAge time.Duration
}

// GitHubApp holds GitHub App credentials. kibitz authenticates as an App only;
// there is no personal access token path (see docs/security.md).
type GitHubApp struct {
	AppID          int64
	InstallationID int64
	PrivateKey     Secret
	BaseURL        string // set for GitHub Enterprise Server
}

// GitLabAuth holds the credentials kibitz posts to GitLab with. GitLab has no
// equivalent of a GitHub App installation token, so this is a long-lived
// access token and is treated as one (docs/security.md).
type GitLabAuth struct {
	// BaseURL is the instance. Empty means gitlab.com.
	BaseURL string
	// Token is a personal, group or project access token with api scope.
	Token Secret
}

// Configured reports whether kibitz can talk to GitLab at all.
func (g GitLabAuth) Configured() bool { return g.Token != "" }

// AzureDevOpsAuth holds the credentials kibitz posts to Azure DevOps with.
//
// Unlike the other two there is no way to derive the instance from an event:
// a service hook names the organization inside the payload, and Azure DevOps
// Server can be at any address at all. So the organization is configured, and
// a worker that has one talks only to it.
type AzureDevOpsAuth struct {
	// OrganizationURL is the account root: "https://dev.azure.com/{org}" for
	// the hosted service, or "https://{server}/{collection}" for Azure DevOps
	// Server.
	OrganizationURL string
	// Token is a personal access token with Code (read) and Pull Request
	// Threads (read & write), or an Entra ID access token.
	Token Secret
	// TokenIsBearer says the token is an Entra ID access token rather than a
	// personal access token. The two go in different places — a PAT is the
	// password of basic authentication, a bearer token is a bearer — and each
	// is rejected in the other's position.
	TokenIsBearer bool
}

// Configured reports whether kibitz can talk to Azure DevOps at all. Both
// halves are needed: a token without an instance has nothing to authenticate
// against, and an instance without a token cannot be read.
func (a AzureDevOpsAuth) Configured() bool {
	return a.Token != "" && a.OrganizationURL != ""
}

// Vertex holds the Google Cloud settings OpenCode needs to reach Vertex AI.
// Authentication is ADC (Workload Identity), so there is no key.
type Vertex struct {
	ProjectID string
	Location  string
	// MaaSProviderID is the provider id kibitz declares for Vertex AI's Model
	// as a Service partner models that OpenCode's own catalog does not list,
	// such as GLM. A model named "<id>/publisher/model" is served through
	// Vertex's OpenAI-compatible endpoint with a token minted from ADC.
	MaaSProviderID string
	// MaaSBaseURL overrides the endpoint kibitz derives from the project and
	// location. It exists because the derived form is the part most likely to
	// need adjusting, and adjusting configuration beats waiting for a release.
	MaaSBaseURL string
}

// OpenCode configures how the worker drives the agent engine.
type OpenCode struct {
	Bin         string
	Mode        string
	ServerURL   string // used when Mode is attach
	Model       string
	TriageModel string
	// ModelPrices is what each model costs, as "model=input/output" per
	// million tokens, so the review summary can say what it spent. kibitz
	// does not know prices: they differ by provider, region and contract,
	// and they change.
	ModelPrices []string
	// RepoBudgets cap what a repository may cost in a calendar month, as
	// "pattern=amount" where the pattern matches "owner/name" with the same
	// wildcards the allow list uses and the amount is in the currency the
	// prices are quoted in. The first matching pattern decides, so specific
	// entries go before "*". Empty means nothing is capped.
	//
	// It is deployment configuration on purpose. A repository that could
	// raise its own ceiling does not have one.
	RepoBudgets []string
	// PriceCurrency is the symbol the estimate is written with. It follows
	// the prices; a dollar sign in front of a yen figure is worse than none.
	PriceCurrency string
	FallbackModel string
	Vertex        Vertex
	// ReviewAgent, AnswerAgent and TriageAgent name the agent definitions shipped in the
	// worker image. An empty value falls back to OpenCode's default agent.
	ReviewAgent string
	AnswerAgent string
	// PlanAgent names the definition that plans a change without making one.
	PlanAgent   string
	TriageAgent string
	// ProviderEnv names environment variables to forward to the agent, for
	// providers that authenticate with an API key (ZHIPU_API_KEY for GLM,
	// OPENROUTER_API_KEY, and so on). Only the names are configured; the
	// values come from the process environment, so a credential never has to
	// be written into kibitz's own configuration.
	ProviderEnv []string
	// EnvPassthrough names further environment variables to copy into the
	// agent's process. The agent's environment is built from a fixed list
	// rather than inherited, so that kibitz's own secrets — the webhook
	// secrets, the GitHub App key — stay in the worker; this is the escape
	// hatch for a deployment that needs one more variable.
	EnvPassthrough []string
	// ContextBin is the kibitz-mcp binary, shipped in the same image. It
	// serves the agent facts about the pull request that the prompt does not
	// carry. Setting it to "off" leaves it out.
	ContextBin string
}

// Limits bounds a single review job.
type Limits struct {
	MaxComments  int
	MaxDiffLines int
	CloneDepth   int
	// MinSeverity drops findings below this level.
	MinSeverity string
}

// Server is the kibitz-server configuration.
type Server struct {
	ListenAddr string
	// MetricsAddr is a second listener for /metrics, kept off the public
	// network. Setting it to the same value as ListenAddr, or to "off",
	// serves /metrics on the main listener instead, which is what a platform
	// that exposes only one port (Cloud Run) needs.
	MetricsAddr       string
	Log               Log
	Trace             Trace
	MaxBodyBytes      int64
	ReadHeaderTimeout time.Duration
	ShutdownTimeout   time.Duration
	Queue             Queue
	Blob              Blob
	Webhook           Webhook
	Policy            Policy
	// Scale lets the server start the worker the moment it publishes,
	// instead of leaving it to wait for the next metrics sample.
	Scale Scale
}

// Worker is the kibitz-worker configuration.
type Worker struct {
	HealthAddr      string
	Log             Log
	Trace           Trace
	ShutdownTimeout time.Duration
	Queue           Queue
	State           State
	Blob            Blob
	Concurrency     int
	JobTimeout      time.Duration
	WorkspaceDir    string
	OpenCode        OpenCode
	Limits          Limits
	// MCPServers defines the external tool servers this deployment offers, as
	// one JSON object of name to server. MCPAllowlist narrows which of them a
	// repository may enable; empty means all of them.
	MCPServers   string
	MCPAllowlist []string
	// GuidelineFiles are the repository's own convention files, read from its
	// default branch. Nil uses the built-in list; "off" reads none.
	GuidelineFiles []string
	// ReferenceDocs are glob patterns for the repository's decision records,
	// indexed from the checkout so the agent knows what it can consult. Nil
	// uses the built-in list; "off" indexes none.
	ReferenceDocs    []string
	GitHub           GitHubApp
	GitLab           GitLabAuth
	AzureDevOps      AzureDevOpsAuth
	ImplementEnabled bool
	// SkipDraft leaves draft pull requests alone until they are marked ready.
	SkipDraft bool
	// SessionTTL is how long the agent's conversation about one pull request,
	// and an "ignore" asked for on it, are remembered.
	SessionTTL time.Duration
	// Language is the language findings and answers are written in.
	Language string
	// Mention is how a comment addresses kibitz. The worker only needs it to
	// write the help text; the decision of what counts as an address is the
	// server's. Both read the same variable, so they cannot disagree.
	Mention string
	// MaxDeliveries is how many times a job is retried before the failure is
	// reported on the pull request instead. It should match the queue's own
	// dead letter threshold.
	MaxDeliveries int
	// MaxPostsPerHour caps what kibitz writes to one pull request per hour.
	MaxPostsPerHour int
	// BotLogins are kibitz's own accounts, used as the second loop check. It
	// is optional: the worker asks GitHub what the app posts as, and anything
	// configured is added to what it learns.
	BotLogins []string
}

// LoadServer reads the kibitz-server configuration.
func LoadServer(env Lookup) (*Server, error) {
	l := newLoader(env)

	cfg := &Server{
		ListenAddr:        l.str("KIBITZ_LISTEN_ADDR", ":8080"),
		MetricsAddr:       l.str("KIBITZ_METRICS_ADDR", ":9090"),
		Log:               loadLog(l),
		Trace:             loadTrace(l),
		MaxBodyBytes:      l.int64("KIBITZ_MAX_BODY_BYTES", 25<<20),
		ReadHeaderTimeout: l.duration("KIBITZ_READ_HEADER_TIMEOUT", 10*time.Second),
		ShutdownTimeout:   l.duration("KIBITZ_SHUTDOWN_TIMEOUT", 20*time.Second),
		Queue:             loadQueue(l),
		Blob:              loadBlob(l),
		Webhook: Webhook{
			GitHubSecrets:           l.secrets("KIBITZ_GITHUB_WEBHOOK_SECRETS"),
			GitLabTokens:            l.secrets("KIBITZ_GITLAB_WEBHOOK_TOKENS"),
			GitLabSigningTokens:     l.secrets("KIBITZ_GITLAB_SIGNING_TOKENS"),
			AzureDevOpsUser:         l.str("KIBITZ_AZDO_BASIC_USER", ""),
			AzureDevOpsPasswords:    l.secrets("KIBITZ_AZDO_BASIC_PASSWORDS"),
			AzureDevOpsHeader:       l.str("KIBITZ_AZDO_HEADER_NAME", ""),
			AzureDevOpsHeaderValues: l.secrets("KIBITZ_AZDO_HEADER_VALUES"),
		},
		Policy: Policy{
			AppID:        l.str("KIBITZ_GITHUB_APP_ID", ""),
			BotLogins:    l.list("KIBITZ_BOT_LOGINS", nil),
			AllowedRepos: l.list("KIBITZ_ALLOWED_REPOS", []string{"*"}),
			Mention:      l.str("KIBITZ_MENTION", policy.DefaultMention),
			Keywords:     l.list("KIBITZ_TRIGGER_KEYWORDS", nil),
			MaxEventAge:  l.durationOrZero("KIBITZ_MAX_EVENT_AGE", 0),
		},
		Scale: loadScale(l),
	}

	if cfg.MetricsAddr == "off" {
		cfg.MetricsAddr = cfg.ListenAddr
	}
	if cfg.MaxBodyBytes <= 0 {
		l.fail("KIBITZ_MAX_BODY_BYTES", "must be greater than 0, got %d", cfg.MaxBodyBytes)
	}
	if len(cfg.Webhook.AzureDevOpsPasswords) > 0 && cfg.Webhook.AzureDevOpsUser == "" {
		l.fail("KIBITZ_AZDO_BASIC_USER", "is required when KIBITZ_AZDO_BASIC_PASSWORDS is set")
	}
	if len(cfg.Webhook.AzureDevOpsHeaderValues) > 0 && cfg.Webhook.AzureDevOpsHeader == "" {
		l.fail("KIBITZ_AZDO_HEADER_NAME", "is required when KIBITZ_AZDO_HEADER_VALUES is set")
	}

	if err := errors.Join(l.errs...); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadWorker reads the kibitz-worker configuration.
func LoadWorker(env Lookup) (*Worker, error) {
	l := newLoader(env)

	cfg := &Worker{
		HealthAddr:      l.str("KIBITZ_WORKER_HEALTH_ADDR", ":8081"),
		Log:             loadLog(l),
		Trace:           loadTrace(l),
		ShutdownTimeout: l.duration("KIBITZ_SHUTDOWN_TIMEOUT", 20*time.Second),
		Queue:           loadQueue(l),
		State:           loadState(l),
		Blob:            loadBlob(l),
		Concurrency:     l.positiveInt("KIBITZ_CONCURRENCY", 2),
		JobTimeout:      l.duration("KIBITZ_JOB_TIMEOUT", 15*time.Minute),
		WorkspaceDir:    l.str("KIBITZ_WORKSPACE_DIR", "/var/tmp/kibitz"),
		OpenCode: OpenCode{
			Bin:           l.str("KIBITZ_OPENCODE_BIN", "opencode"),
			Mode:          l.enum("KIBITZ_OPENCODE_MODE", OpenCodeModeRun, OpenCodeModeRun, OpenCodeModeAttach),
			ServerURL:     l.str("KIBITZ_OPENCODE_SERVER_URL", "http://127.0.0.1:4096"),
			Model:         l.str("KIBITZ_MODEL", "google-vertex/gemini-3.1-pro-preview"),
			TriageModel:   l.str("KIBITZ_TRIAGE_MODEL", ""),
			ModelPrices:   l.list("KIBITZ_MODEL_PRICES", nil),
			RepoBudgets:   l.list("KIBITZ_REPO_BUDGETS", nil),
			PriceCurrency: l.str("KIBITZ_MODEL_PRICE_CURRENCY", "$"),
			FallbackModel: l.str("KIBITZ_MODEL_FALLBACK", ""),
			Vertex: Vertex{
				ProjectID:      l.str("GOOGLE_CLOUD_PROJECT", ""),
				Location:       l.str("VERTEX_LOCATION", "global"),
				MaaSProviderID: l.str("KIBITZ_VERTEX_MAAS_PROVIDER_ID", "vertex-maas"),
				MaaSBaseURL:    l.str("KIBITZ_VERTEX_MAAS_BASE_URL", ""),
			},
			ReviewAgent:    l.str("KIBITZ_OPENCODE_REVIEW_AGENT", "kibitz-review"),
			AnswerAgent:    l.str("KIBITZ_OPENCODE_ANSWER_AGENT", "kibitz-answer"),
			PlanAgent:      l.str("KIBITZ_OPENCODE_PLAN_AGENT", "kibitz-plan"),
			TriageAgent:    l.str("KIBITZ_OPENCODE_TRIAGE_AGENT", "kibitz-triage"),
			ProviderEnv:    l.list("KIBITZ_PROVIDER_ENV", nil),
			EnvPassthrough: l.list("KIBITZ_AGENT_ENV_PASSTHROUGH", nil),
			ContextBin:     l.str("KIBITZ_MCP_CONTEXT_BIN", "kibitz-mcp"),
		},
		Limits: Limits{
			MaxComments:  l.positiveInt("KIBITZ_MAX_COMMENTS", 20),
			MaxDiffLines: l.positiveInt("KIBITZ_MAX_DIFF_LINES", 10000),
			CloneDepth:   l.positiveInt("KIBITZ_CLONE_DEPTH", 50),
			MinSeverity:  l.enum("KIBITZ_MIN_SEVERITY", "medium", "critical", "high", "medium", "low", "info"),
		},
		MCPServers:     l.str("KIBITZ_MCP_SERVERS", ""),
		MCPAllowlist:   l.list("KIBITZ_MCP_ALLOWLIST", nil),
		GuidelineFiles: l.list("KIBITZ_REPO_GUIDELINE_FILES", nil),
		ReferenceDocs:  l.list("KIBITZ_REFERENCE_DOCS", nil),
		GitHub: GitHubApp{
			AppID:          l.int64("KIBITZ_GITHUB_APP_ID", 0),
			InstallationID: l.int64("KIBITZ_GITHUB_INSTALLATION_ID", 0),
			PrivateKey:     l.secret("KIBITZ_GITHUB_PRIVATE_KEY"),
			BaseURL:        l.str("KIBITZ_GITHUB_BASE_URL", ""),
		},
		GitLab: GitLabAuth{
			BaseURL: l.str("KIBITZ_GITLAB_BASE_URL", ""),
			Token:   l.secret("KIBITZ_GITLAB_TOKEN"),
		},
		AzureDevOps: AzureDevOpsAuth{
			OrganizationURL: l.str("KIBITZ_AZDO_ORG_URL", ""),
			Token:           l.secret("KIBITZ_AZDO_TOKEN"),
			TokenIsBearer:   l.bool("KIBITZ_AZDO_TOKEN_IS_BEARER", false),
		},
		ImplementEnabled: l.bool("KIBITZ_IMPLEMENT_ENABLED", false),
		SkipDraft:        l.bool("KIBITZ_SKIP_DRAFT", true),
		SessionTTL:       l.duration("KIBITZ_SESSION_TTL", 7*24*time.Hour),
		Language:         l.str("KIBITZ_LANGUAGE", "日本語"),
		Mention:          l.str("KIBITZ_MENTION", policy.DefaultMention),
		MaxDeliveries:    l.positiveInt("KIBITZ_MAX_DELIVERIES", 5),
		MaxPostsPerHour:  l.positiveInt("KIBITZ_MAX_POSTS_PER_HOUR", 10),
		BotLogins:        l.list("KIBITZ_BOT_LOGINS", nil),
	}

	// A GitHub App is either fully configured or not configured at all; half of
	// one fails at the first API call instead of at startup.
	if cfg.GitHub.AppID != 0 || cfg.GitHub.PrivateKey != "" {
		l.requireIf(cfg.GitHub.AppID == 0, "KIBITZ_GITHUB_APP_ID", "", "when a GitHub App private key is set")
		l.requireIf(cfg.GitHub.PrivateKey == "", "KIBITZ_GITHUB_PRIVATE_KEY", "", "when a GitHub App ID is set")
	}

	// Azure DevOps needs both halves. A token with no instance has nothing to
	// authenticate against, and an instance with no token cannot be read;
	// either on its own fails at the first API call rather than at startup.
	if cfg.AzureDevOps.OrganizationURL != "" || cfg.AzureDevOps.Token != "" {
		l.requireIf(cfg.AzureDevOps.OrganizationURL == "", "KIBITZ_AZDO_ORG_URL", "", "when an Azure DevOps token is set")
		l.requireIf(cfg.AzureDevOps.Token == "", "KIBITZ_AZDO_TOKEN", "", "when an Azure DevOps organization URL is set")
	}

	if err := errors.Join(l.errs...); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Scaler is the kibitz-scaler configuration. It is a small tool with a small
// configuration: which queue to watch, and which worker pool to size from it.
type Scaler struct {
	Log   Log
	Trace Trace
	Queue Queue
	Scale Scale
}

// LoadScaler reads the kibitz-scaler configuration.
func LoadScaler(env Lookup) (*Scaler, error) {
	l := newLoader(env)

	cfg := &Scaler{
		Log:   loadLog(l),
		Trace: loadTrace(l),
		Queue: loadQueue(l),
		Scale: loadScale(l),
	}

	// The scaler has nothing else to do, so an unset backend is a mistake
	// rather than a choice not to scale.
	if cfg.Scale.Backend == ScaleNone {
		l.fail("KIBITZ_SCALE_BACKEND", "must be %q for kibitz-scaler", ScaleCloudRun)
	}
	if cfg.Queue.Backend != QueuePubSub {
		l.fail("KIBITZ_QUEUE_BACKEND", "must be %q for kibitz-scaler, got %q", QueuePubSub, cfg.Queue.Backend)
	}

	if err := errors.Join(l.errs...); err != nil {
		return nil, err
	}
	return cfg, nil
}

func loadLog(l *loader) Log {
	return Log{
		Level:  l.logLevel("KIBITZ_LOG_LEVEL", slog.LevelInfo),
		Format: l.enum("KIBITZ_LOG_FORMAT", "json", "json", "text"),
	}
}

func loadTrace(l *loader) Trace {
	return Trace{
		Endpoint:    l.str("KIBITZ_OTEL_ENDPOINT", ""),
		Insecure:    l.bool("KIBITZ_OTEL_INSECURE", false),
		SampleRatio: l.ratio("KIBITZ_OTEL_SAMPLE_RATIO", 1),
	}
}

// loadQueue reads the queue settings. Only values without a default are
// validated here: the others cannot be empty by construction, since an empty
// environment variable falls back to the default.
func loadQueue(l *loader) Queue {
	q := Queue{
		Backend: l.enum("KIBITZ_QUEUE_BACKEND", QueuePubSub, QueuePubSub, QueueSQS, QueueMemory),
		PubSub: PubSub{
			ProjectID:    l.str("KIBITZ_PUBSUB_PROJECT_ID", ""),
			Topic:        l.str("KIBITZ_PUBSUB_TOPIC", "kibitz-events"),
			Subscription: l.str("KIBITZ_PUBSUB_SUBSCRIPTION", "kibitz-worker"),
		},
		SQS: SQS{
			QueueURL: l.str("KIBITZ_SQS_QUEUE_URL", ""),
			Region:   l.str("KIBITZ_AWS_REGION", ""),
		},
	}

	switch q.Backend {
	case QueuePubSub:
		l.requireIf(true, "KIBITZ_PUBSUB_PROJECT_ID", q.PubSub.ProjectID, "when KIBITZ_QUEUE_BACKEND is pubsub")
	case QueueSQS:
		l.requireIf(true, "KIBITZ_SQS_QUEUE_URL", q.SQS.QueueURL, "when KIBITZ_QUEUE_BACKEND is sqs")
		l.requireIf(true, "KIBITZ_AWS_REGION", q.SQS.Region, "when KIBITZ_QUEUE_BACKEND is sqs")
	}
	return q
}

func loadScale(l *loader) Scale {
	s := Scale{
		Backend:             l.enum("KIBITZ_SCALE_BACKEND", ScaleNone, ScaleNone, ScaleCloudRun),
		ProjectID:           l.str("KIBITZ_SCALE_PROJECT_ID", l.str("KIBITZ_PUBSUB_PROJECT_ID", "")),
		Region:              l.str("KIBITZ_SCALE_REGION", ""),
		WorkerPool:          l.str("KIBITZ_SCALE_WORKER_POOL", ""),
		MinInstances:        l.int("KIBITZ_SCALE_MIN_INSTANCES", 0),
		MaxInstances:        l.positiveInt("KIBITZ_SCALE_MAX_INSTANCES", 3),
		MessagesPerInstance: l.positiveInt("KIBITZ_SCALE_MESSAGES_PER_INSTANCE", 2),
		IdleAfter:           l.duration("KIBITZ_SCALE_IDLE_AFTER", 15*time.Minute),
		Interval:            l.duration("KIBITZ_SCALE_INTERVAL", time.Minute),
		WakeCooldown:        l.duration("KIBITZ_SCALE_WAKE_COOLDOWN", 30*time.Second),
	}

	if s.Backend == ScaleCloudRun {
		l.requireIf(true, "KIBITZ_SCALE_REGION", s.Region, "when KIBITZ_SCALE_BACKEND is cloudrun")
		l.requireIf(true, "KIBITZ_SCALE_WORKER_POOL", s.WorkerPool, "when KIBITZ_SCALE_BACKEND is cloudrun")
		l.requireIf(true, "KIBITZ_SCALE_PROJECT_ID", s.ProjectID, "when KIBITZ_SCALE_BACKEND is cloudrun")
	}
	if s.MinInstances < 0 {
		l.fail("KIBITZ_SCALE_MIN_INSTANCES", "must not be negative, got %d", s.MinInstances)
	}
	if s.MinInstances > s.MaxInstances {
		l.fail("KIBITZ_SCALE_MAX_INSTANCES", "must be at least KIBITZ_SCALE_MIN_INSTANCES (%d), got %d",
			s.MinInstances, s.MaxInstances)
	}
	return s
}

func loadState(l *loader) State {
	s := State{
		Backend:           l.enum("KIBITZ_STATE_BACKEND", StateFirestore, StateFirestore, StateDynamoDB, StateMemory),
		FirestoreProject:  l.str("KIBITZ_FIRESTORE_PROJECT_ID", ""),
		FirestoreDatabase: l.str("KIBITZ_FIRESTORE_DATABASE", "(default)"),
		DynamoDBTable:     l.str("KIBITZ_DYNAMODB_TABLE", ""),
		AWSRegion:         l.str("KIBITZ_AWS_REGION", ""),
	}

	switch s.Backend {
	case StateFirestore:
		l.requireIf(true, "KIBITZ_FIRESTORE_PROJECT_ID", s.FirestoreProject, "when KIBITZ_STATE_BACKEND is firestore")
	case StateDynamoDB:
		l.requireIf(true, "KIBITZ_DYNAMODB_TABLE", s.DynamoDBTable, "when KIBITZ_STATE_BACKEND is dynamodb")
		l.requireIf(true, "KIBITZ_AWS_REGION", s.AWSRegion, "when KIBITZ_STATE_BACKEND is dynamodb")
	}
	return s
}

func loadBlob(l *loader) Blob {
	b := Blob{
		Backend:             l.enum("KIBITZ_BLOBSTORE_BACKEND", BlobNone, BlobGCS, BlobS3, BlobNone),
		Bucket:              l.str("KIBITZ_BLOBSTORE_BUCKET", ""),
		ClaimCheckThreshold: l.int64("KIBITZ_CLAIM_CHECK_THRESHOLD", 64<<10),
	}
	l.requireIf(b.Backend != BlobNone, "KIBITZ_BLOBSTORE_BUCKET", b.Bucket, "when a blob store backend is selected")
	return b
}
