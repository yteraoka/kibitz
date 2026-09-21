// Package config loads kibitz configuration from the environment and validates
// it at startup, so that a misconfigured deployment fails immediately and says
// everything that is wrong rather than failing later on the first webhook.
package config

import (
	"errors"
	"log/slog"
	"time"
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

// Webhook holds the credentials used to verify inbound webhooks. Each platform
// accepts a list so secrets can be rotated without downtime.
type Webhook struct {
	GitHubSecrets        []Secret
	GitLabTokens         []Secret
	AzureDevOpsUser      string
	AzureDevOpsPasswords []Secret
}

// Configured reports whether at least one platform can be verified. A server
// with no webhook credentials at all would accept nothing, which is always a
// misconfiguration.
func (w Webhook) Configured() bool {
	return len(w.GitHubSecrets) > 0 || len(w.GitLabTokens) > 0 || len(w.AzureDevOpsPasswords) > 0
}

// Policy holds the trigger rules the server applies before publishing.
type Policy struct {
	// BotLogins are the accounts kibitz itself posts as. Events they author are
	// dropped so the bot never reacts to its own comments.
	BotLogins []string
	// AllowedRepos are glob patterns matched against "owner/name".
	AllowedRepos []string
	Mention      string
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

// Vertex holds the Google Cloud settings OpenCode needs to reach Claude on
// Vertex AI. Authentication is ADC (Workload Identity), so there is no key.
type Vertex struct {
	ProjectID string
	Location  string
}

// OpenCode configures how the worker drives the agent engine.
type OpenCode struct {
	Bin           string
	Mode          string
	ServerURL     string // used when Mode is attach
	Model         string
	TriageModel   string
	FallbackModel string
	Vertex        Vertex
	// ReviewAgent and AnswerAgent name the agent definitions shipped in the
	// worker image. An empty value falls back to OpenCode's default agent.
	ReviewAgent string
	AnswerAgent string
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
}

// Worker is the kibitz-worker configuration.
type Worker struct {
	HealthAddr       string
	Log              Log
	Trace            Trace
	ShutdownTimeout  time.Duration
	Queue            Queue
	State            State
	Blob             Blob
	Concurrency      int
	JobTimeout       time.Duration
	WorkspaceDir     string
	OpenCode         OpenCode
	Limits           Limits
	MCPAllowlist     []string
	GitHub           GitHubApp
	ImplementEnabled bool
	// SkipDraft leaves draft pull requests alone until they are marked ready.
	SkipDraft bool
	// Language is the language findings and answers are written in.
	Language string
	// MaxDeliveries is how many times a job is retried before the failure is
	// reported on the pull request instead. It should match the queue's own
	// dead letter threshold.
	MaxDeliveries int
	// MaxPostsPerHour caps what kibitz writes to one pull request per hour.
	MaxPostsPerHour int
	// BotLogins are kibitz's own accounts, used as the second loop check.
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
			GitHubSecrets:        l.secrets("KIBITZ_GITHUB_WEBHOOK_SECRETS"),
			GitLabTokens:         l.secrets("KIBITZ_GITLAB_WEBHOOK_TOKENS"),
			AzureDevOpsUser:      l.str("KIBITZ_AZDO_BASIC_USER", ""),
			AzureDevOpsPasswords: l.secrets("KIBITZ_AZDO_BASIC_PASSWORDS"),
		},
		Policy: Policy{
			BotLogins:    l.list("KIBITZ_BOT_LOGINS", nil),
			AllowedRepos: l.list("KIBITZ_ALLOWED_REPOS", []string{"*"}),
			Mention:      l.str("KIBITZ_MENTION", "@kibitz"),
			MaxEventAge:  l.durationOrZero("KIBITZ_MAX_EVENT_AGE", 0),
		},
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
			Model:         l.str("KIBITZ_MODEL", "google-vertex-anthropic/claude-opus-5"),
			TriageModel:   l.str("KIBITZ_TRIAGE_MODEL", ""),
			FallbackModel: l.str("KIBITZ_MODEL_FALLBACK", ""),
			Vertex: Vertex{
				ProjectID: l.str("GOOGLE_CLOUD_PROJECT", ""),
				Location:  l.str("VERTEX_LOCATION", "global"),
			},
			ReviewAgent: l.str("KIBITZ_OPENCODE_REVIEW_AGENT", "kibitz-review"),
			AnswerAgent: l.str("KIBITZ_OPENCODE_ANSWER_AGENT", "kibitz-answer"),
		},
		Limits: Limits{
			MaxComments:  l.positiveInt("KIBITZ_MAX_COMMENTS", 20),
			MaxDiffLines: l.positiveInt("KIBITZ_MAX_DIFF_LINES", 10000),
			CloneDepth:   l.positiveInt("KIBITZ_CLONE_DEPTH", 50),
			MinSeverity:  l.enum("KIBITZ_MIN_SEVERITY", "medium", "critical", "high", "medium", "low", "info"),
		},
		MCPAllowlist: l.list("KIBITZ_MCP_ALLOWLIST", nil),
		GitHub: GitHubApp{
			AppID:          l.int64("KIBITZ_GITHUB_APP_ID", 0),
			InstallationID: l.int64("KIBITZ_GITHUB_INSTALLATION_ID", 0),
			PrivateKey:     l.secret("KIBITZ_GITHUB_PRIVATE_KEY"),
			BaseURL:        l.str("KIBITZ_GITHUB_BASE_URL", ""),
		},
		ImplementEnabled: l.bool("KIBITZ_IMPLEMENT_ENABLED", false),
		SkipDraft:        l.bool("KIBITZ_SKIP_DRAFT", true),
		Language:         l.str("KIBITZ_LANGUAGE", "日本語"),
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

// loadQueue reads the queue settings. Only values without a default are
// validated here: the others cannot be empty by construction, since an empty
// environment variable falls back to the default.
func loadTrace(l *loader) Trace {
	return Trace{
		Endpoint:    l.str("KIBITZ_OTEL_ENDPOINT", ""),
		Insecure:    l.bool("KIBITZ_OTEL_INSECURE", false),
		SampleRatio: l.ratio("KIBITZ_OTEL_SAMPLE_RATIO", 1),
	}
}

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
