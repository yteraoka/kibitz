package config_test

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/config"
)

func minimalServerEnv() map[string]string {
	return map[string]string{
		"KIBITZ_PUBSUB_PROJECT_ID":      "kibitz-dev",
		"KIBITZ_GITHUB_WEBHOOK_SECRETS": "s3cret",
	}
}

func minimalWorkerEnv() map[string]string {
	return map[string]string{
		"KIBITZ_PUBSUB_PROJECT_ID":    "kibitz-dev",
		"KIBITZ_FIRESTORE_PROJECT_ID": "kibitz-dev",
	}
}

func TestLoadServerDefaults(t *testing.T) {
	cfg, err := config.LoadServer(config.MapEnv(minimalServerEnv()))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}

	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", cfg.ListenAddr)
	}
	if cfg.MaxBodyBytes != 25<<20 {
		t.Errorf("MaxBodyBytes = %d, want %d", cfg.MaxBodyBytes, 25<<20)
	}
	if cfg.Queue.Backend != config.QueuePubSub {
		t.Errorf("Queue.Backend = %q, want pubsub", cfg.Queue.Backend)
	}
	if cfg.Queue.PubSub.Topic != "kibitz-events" {
		t.Errorf("Topic = %q, want kibitz-events", cfg.Queue.PubSub.Topic)
	}
	if cfg.Policy.Mention != "@kibitz" {
		t.Errorf("Mention = %q, want @kibitz", cfg.Policy.Mention)
	}
	if got, want := cfg.Policy.AllowedRepos, []string{"*"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("AllowedRepos = %v, want %v", got, want)
	}
	if !cfg.Webhook.Configured() {
		t.Error("Webhook.Configured() = false, want true")
	}
	if cfg.Log.Level != slog.LevelInfo {
		t.Errorf("Log.Level = %v, want info", cfg.Log.Level)
	}
}

// An explicitly empty variable falls back to the default rather than being
// treated as a configured empty value.
func TestEmptyValueFallsBackToDefault(t *testing.T) {
	env := minimalServerEnv()
	env["KIBITZ_PUBSUB_TOPIC"] = ""
	env["KIBITZ_MENTION"] = "   "

	cfg, err := config.LoadServer(config.MapEnv(env))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.Queue.PubSub.Topic != "kibitz-events" {
		t.Errorf("Topic = %q, want the default", cfg.Queue.PubSub.Topic)
	}
	if cfg.Policy.Mention != "@kibitz" {
		t.Errorf("Mention = %q, want the default", cfg.Policy.Mention)
	}
}

func TestLoadServerReportsEveryProblemAtOnce(t *testing.T) {
	_, err := config.LoadServer(config.MapEnv(map[string]string{
		"KIBITZ_QUEUE_BACKEND":     "sqs",
		"KIBITZ_LOG_LEVEL":         "loud",
		"KIBITZ_MAX_BODY_BYTES":    "not-a-number",
		"KIBITZ_BLOBSTORE_BACKEND": "gcs",
	}))
	if err == nil {
		t.Fatal("LoadServer succeeded, want error")
	}

	msg := err.Error()
	for _, want := range []string{
		"KIBITZ_LOG_LEVEL",
		"KIBITZ_MAX_BODY_BYTES",
		"KIBITZ_SQS_QUEUE_URL",
		"KIBITZ_AWS_REGION",
		"KIBITZ_BLOBSTORE_BUCKET",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %s:\n%s", want, msg)
		}
	}
}

func TestLoadServerAzureDevOpsNeedsUser(t *testing.T) {
	env := minimalServerEnv()
	env["KIBITZ_AZDO_BASIC_PASSWORDS"] = "pw1,pw2"

	_, err := config.LoadServer(config.MapEnv(env))
	if err == nil || !strings.Contains(err.Error(), "KIBITZ_AZDO_BASIC_USER") {
		t.Fatalf("err = %v, want it to mention KIBITZ_AZDO_BASIC_USER", err)
	}
}

func TestSecretsRotationList(t *testing.T) {
	env := minimalServerEnv()
	env["KIBITZ_GITHUB_WEBHOOK_SECRETS"] = " old , new ,"

	cfg, err := config.LoadServer(config.MapEnv(env))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if len(cfg.Webhook.GitHubSecrets) != 2 {
		t.Fatalf("got %d secrets, want 2", len(cfg.Webhook.GitHubSecrets))
	}
	if got := cfg.Webhook.GitHubSecrets[0].Reveal(); got != "old" {
		t.Errorf("first secret = %q, want old", got)
	}
	if got := cfg.Webhook.GitHubSecrets[1].Reveal(); got != "new" {
		t.Errorf("second secret = %q, want new", got)
	}
}

func TestLoadWorkerDefaults(t *testing.T) {
	cfg, err := config.LoadWorker(config.MapEnv(minimalWorkerEnv()))
	if err != nil {
		t.Fatalf("LoadWorker: %v", err)
	}

	if cfg.Concurrency != 2 {
		t.Errorf("Concurrency = %d, want 2", cfg.Concurrency)
	}
	if cfg.JobTimeout != 15*time.Minute {
		t.Errorf("JobTimeout = %v, want 15m", cfg.JobTimeout)
	}
	if cfg.OpenCode.Mode != config.OpenCodeModeRun {
		t.Errorf("OpenCode.Mode = %q, want run", cfg.OpenCode.Mode)
	}
	if cfg.OpenCode.Vertex.Location != "global" {
		t.Errorf("Vertex.Location = %q, want global", cfg.OpenCode.Vertex.Location)
	}
	if !strings.Contains(cfg.OpenCode.Model, "claude-opus-5") {
		t.Errorf("Model = %q, want the Vertex claude-opus-5 default", cfg.OpenCode.Model)
	}
	if cfg.ImplementEnabled {
		t.Error("ImplementEnabled = true, want false by default")
	}
}

func TestLoadWorkerRequiresPubSubProject(t *testing.T) {
	env := minimalWorkerEnv()
	delete(env, "KIBITZ_PUBSUB_PROJECT_ID")

	_, err := config.LoadWorker(config.MapEnv(env))
	if err == nil || !strings.Contains(err.Error(), "KIBITZ_PUBSUB_PROJECT_ID") {
		t.Fatalf("err = %v, want it to mention KIBITZ_PUBSUB_PROJECT_ID", err)
	}
}

func TestLoadWorkerMemoryBackendsNeedNothing(t *testing.T) {
	cfg, err := config.LoadWorker(config.MapEnv(map[string]string{
		"KIBITZ_QUEUE_BACKEND": config.QueueMemory,
		"KIBITZ_STATE_BACKEND": config.StateMemory,
	}))
	if err != nil {
		t.Fatalf("LoadWorker: %v", err)
	}
	if cfg.Queue.Backend != config.QueueMemory {
		t.Errorf("Queue.Backend = %q, want memory", cfg.Queue.Backend)
	}
}

func TestLoadWorkerPartialGitHubApp(t *testing.T) {
	env := minimalWorkerEnv()
	env["KIBITZ_GITHUB_APP_ID"] = "12345"

	_, err := config.LoadWorker(config.MapEnv(env))
	if err == nil || !strings.Contains(err.Error(), "KIBITZ_GITHUB_PRIVATE_KEY") {
		t.Fatalf("err = %v, want it to mention KIBITZ_GITHUB_PRIVATE_KEY", err)
	}
}

func TestLoadWorkerRejectsUnknownOpenCodeMode(t *testing.T) {
	env := minimalWorkerEnv()
	env["KIBITZ_OPENCODE_MODE"] = "serve"

	_, err := config.LoadWorker(config.MapEnv(env))
	if err == nil || !strings.Contains(err.Error(), "KIBITZ_OPENCODE_MODE") {
		t.Fatalf("err = %v, want it to mention KIBITZ_OPENCODE_MODE", err)
	}
}

func TestLoadWorkerRejectsNonPositiveConcurrency(t *testing.T) {
	env := minimalWorkerEnv()
	env["KIBITZ_CONCURRENCY"] = "0"

	_, err := config.LoadWorker(config.MapEnv(env))
	if err == nil || !strings.Contains(err.Error(), "KIBITZ_CONCURRENCY") {
		t.Fatalf("err = %v, want it to mention KIBITZ_CONCURRENCY", err)
	}
}

func TestSecretNeverRenders(t *testing.T) {
	s := config.Secret("ghp_0123456789abcdefghijABCDEFGHIJ")

	if got := s.String(); got != config.Redacted {
		t.Errorf("String() = %q, want %q", got, config.Redacted)
	}
	if got := fmt.Sprintf("%v", s); got != config.Redacted {
		t.Errorf("%%v = %q, want %q", got, config.Redacted)
	}
	if got := fmt.Sprintf("secret=%s", s); got != "secret="+config.Redacted {
		t.Errorf("interpolated = %q, want secret=%s", got, config.Redacted)
	}
	if got := s.Reveal(); got != "ghp_0123456789abcdefghijABCDEFGHIJ" {
		t.Errorf("Reveal() lost the value: %q", got)
	}
	if got := config.Secret("").String(); got != "" {
		t.Errorf("empty Secret rendered as %q, want empty", got)
	}
}

// The replay window is off by default so that redelivering a failed webhook
// hours later still produces a review.
func TestMaxEventAgeDefaultsToDisabled(t *testing.T) {
	cfg, err := config.LoadServer(config.MapEnv(minimalServerEnv()))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.Policy.MaxEventAge != 0 {
		t.Errorf("MaxEventAge = %v, want it disabled", cfg.Policy.MaxEventAge)
	}

	env := minimalServerEnv()
	env["KIBITZ_MAX_EVENT_AGE"] = "10m"
	cfg, err = config.LoadServer(config.MapEnv(env))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.Policy.MaxEventAge != 10*time.Minute {
		t.Errorf("MaxEventAge = %v, want 10m", cfg.Policy.MaxEventAge)
	}

	env["KIBITZ_MAX_EVENT_AGE"] = "-1m"
	if _, err := config.LoadServer(config.MapEnv(env)); err == nil {
		t.Error("a negative event age was accepted")
	}
}

func TestTraceConfiguration(t *testing.T) {
	cfg, err := config.LoadServer(config.MapEnv(minimalServerEnv()))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	// Tracing is off unless a collector is named, so the instrumentation costs
	// nothing by default.
	if cfg.Trace.Endpoint != "" || cfg.Trace.SampleRatio != 1 {
		t.Errorf("trace defaults = %+v", cfg.Trace)
	}

	env := minimalServerEnv()
	env["KIBITZ_OTEL_ENDPOINT"] = "otel-collector:4317"
	env["KIBITZ_OTEL_INSECURE"] = "true"
	env["KIBITZ_OTEL_SAMPLE_RATIO"] = "0.25"

	cfg, err = config.LoadServer(config.MapEnv(env))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.Trace.Endpoint != "otel-collector:4317" || !cfg.Trace.Insecure || cfg.Trace.SampleRatio != 0.25 {
		t.Errorf("trace config = %+v", cfg.Trace)
	}

	for _, bad := range []string{"2", "-0.5", "most"} {
		env["KIBITZ_OTEL_SAMPLE_RATIO"] = bad
		if _, err := config.LoadServer(config.MapEnv(env)); err == nil {
			t.Errorf("a sample ratio of %q was accepted", bad)
		}
	}
}

func TestWorkerReliabilityDefaults(t *testing.T) {
	cfg, err := config.LoadWorker(config.MapEnv(minimalWorkerEnv()))
	if err != nil {
		t.Fatalf("LoadWorker: %v", err)
	}
	if cfg.MaxDeliveries != 5 {
		t.Errorf("MaxDeliveries = %d, want 5", cfg.MaxDeliveries)
	}
	if cfg.MaxPostsPerHour != 10 {
		t.Errorf("MaxPostsPerHour = %d, want 10", cfg.MaxPostsPerHour)
	}
	if !cfg.SkipDraft {
		t.Error("SkipDraft = false, want drafts skipped by default")
	}
	if cfg.Limits.MinSeverity != "medium" {
		t.Errorf("MinSeverity = %q, want medium", cfg.Limits.MinSeverity)
	}
}

// Cloud Run exposes a single port, so the metrics listener can be folded into
// the main one.
func TestMetricsOnTheMainListener(t *testing.T) {
	env := minimalServerEnv()
	env["KIBITZ_METRICS_ADDR"] = "off"

	cfg, err := config.LoadServer(config.MapEnv(env))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.MetricsAddr != cfg.ListenAddr {
		t.Errorf("MetricsAddr = %q, want it folded into %q", cfg.MetricsAddr, cfg.ListenAddr)
	}
}
