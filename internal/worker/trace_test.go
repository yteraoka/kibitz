package worker_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/yteraoka/kibitz/internal/config"
	"github.com/yteraoka/kibitz/internal/policy"
	"github.com/yteraoka/kibitz/internal/queue/memory"
	"github.com/yteraoka/kibitz/internal/telemetry"
	"github.com/yteraoka/kibitz/internal/webhook"
	githubhook "github.com/yteraoka/kibitz/internal/webhook/github"
	"github.com/yteraoka/kibitz/internal/worker"
)

// One review has to be one trace, from the delivery that caused it to the
// comment it produces. The queue hop in the middle is not an HTTP request, so
// this is the part that actually needs testing: the context has to survive as
// a field on the message.
func TestTraceSurvivesTheQueue(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := trace.NewTracerProvider(
		trace.WithSyncer(exporter),
		trace.WithSampler(trace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	if _, err := telemetry.SetupTracing(context.Background(), telemetry.TracingOptions{}); err != nil {
		t.Fatalf("SetupTracing: %v", err)
	}
	// SetupTracing installs the propagator, but with no endpoint it also
	// installs a no-op provider; the recording one is what this test needs.
	otel.SetTracerProvider(provider)

	const secret = "dev-secret"
	q := memory.New()
	receiver := webhook.NewReceiver(
		githubhook.New([]string{secret}), q,
		policy.New(policy.Config{AllowedRepos: []string{"*"}, Mention: "@kibitz"}),
		discardLogger(),
	)

	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "webhooks", "github", "pull_request.opened.json"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(string(body)))
	req.Header.Set(githubhook.HeaderEvent, "pull_request")
	req.Header.Set(githubhook.HeaderDelivery, "trace-1")
	req.Header.Set(githubhook.HeaderSignature, "sha256="+hex.EncodeToString(mac.Sum(nil)))

	rec := httptest.NewRecorder()
	receiver.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("the webhook returned %d", rec.Code)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	w := worker.New(q, worker.HandlerFunc(func(context.Context, *worker.Job) error {
		close(done)
		return nil
	}), discardLogger(), &config.Worker{JobTimeout: time.Second})
	go func() { _ = w.Run(ctx) }()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("the job never ran")
	}
	// The job's span ends after the handler returns.
	time.Sleep(100 * time.Millisecond)

	var webhookSpan, jobSpan tracetest.SpanStub
	for _, span := range exporter.GetSpans() {
		switch span.Name {
		case "webhook.receive":
			webhookSpan = span
		case "job":
			jobSpan = span
		}
	}

	if webhookSpan.Name == "" {
		t.Fatal("no span was recorded for the webhook")
	}
	if jobSpan.Name == "" {
		t.Fatal("no span was recorded for the job")
	}
	if webhookSpan.SpanContext.TraceID() != jobSpan.SpanContext.TraceID() {
		t.Errorf("the job started a new trace: %s vs %s",
			webhookSpan.SpanContext.TraceID(), jobSpan.SpanContext.TraceID())
	}
	if jobSpan.Parent.SpanID() != webhookSpan.SpanContext.SpanID() {
		t.Errorf("the job's parent is %s, want the webhook span %s",
			jobSpan.Parent.SpanID(), webhookSpan.SpanContext.SpanID())
	}
}

// With no collector configured the instrumentation still has to be harmless.
func TestTracingOffIsSafe(t *testing.T) {
	shutdown, err := telemetry.SetupTracing(context.Background(), telemetry.TracingOptions{})
	if err != nil {
		t.Fatalf("SetupTracing: %v", err)
	}

	ctx, span := telemetry.Tracer().Start(context.Background(), "test")
	span.End()

	carrier := telemetry.Carrier{}
	telemetry.InjectTrace(ctx, carrier)
	if len(carrier) != 0 {
		t.Errorf("a no-op tracer injected %v", carrier)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}
