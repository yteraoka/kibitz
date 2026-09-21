package webhook_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/policy"
	"github.com/yteraoka/kibitz/internal/queue"
	"github.com/yteraoka/kibitz/internal/queue/memory"
	"github.com/yteraoka/kibitz/internal/webhook"
	githubhook "github.com/yteraoka/kibitz/internal/webhook/github"
)

const secret = "dev-secret"

var now = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "webhooks", "github", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return b
}

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newReceiver(t *testing.T, pub queue.Publisher, opts ...webhook.ReceiverOption) *webhook.Receiver {
	t.Helper()

	triggers := policy.New(policy.Config{
		BotLogins:    []string{"kibitz[bot]"},
		AllowedRepos: []string{"yteraoka/*"},
		Mention:      "@kibitz",
		// The fixtures carry their own timestamps, so staleness is switched
		// off here and tested on its own.
		MaxEventAge: 0,
	})
	opts = append([]webhook.ReceiverOption{webhook.WithReceiverClock(func() time.Time { return now })}, opts...)
	return webhook.NewReceiver(githubhook.New([]string{secret}), pub, triggers, discardLogger(), opts...)
}

func post(body []byte, eventName string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(string(body)))
	r.Header.Set(githubhook.HeaderEvent, eventName)
	r.Header.Set(githubhook.HeaderDelivery, "7f3c")
	r.Header.Set(githubhook.HeaderSignature, sign(body))
	return r
}

func TestReceiverPublishes(t *testing.T) {
	q := memory.New()
	rc := newReceiver(t, q)
	body := fixture(t, "pull_request.opened.json")

	rec := httptest.NewRecorder()
	rc.ServeHTTP(rec, post(body, "pull_request"))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body)
	}
	if q.Len() != 1 {
		t.Fatalf("%d messages queued, want 1", q.Len())
	}
}

func TestReceiverPromotesCommand(t *testing.T) {
	q := memory.New()
	rc := newReceiver(t, q)
	body := fixture(t, "issue_comment.created.json")

	rec := httptest.NewRecorder()
	rc.ServeHTTP(rec, post(body, "issue_comment"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got := make(chan *event.ReviewEvent, 1)
	go func() {
		_ = q.Receive(ctx, func(_ context.Context, m *queue.Message) error {
			got <- m.Event
			return nil
		})
	}()

	select {
	case ev := <-got:
		if ev.Kind != event.KindCommand {
			t.Errorf("kind = %s, want %s", ev.Kind, event.KindCommand)
		}
		if ev.Command == nil || ev.Command.Name != policy.CommandReview {
			t.Errorf("command = %+v, want review", ev.Command)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the published event")
	}
}

func TestReceiverTracePropagation(t *testing.T) {
	q := memory.New()
	rc := newReceiver(t, q)
	body := fixture(t, "pull_request.opened.json")

	r := post(body, "pull_request")
	r.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	rec := httptest.NewRecorder()
	rc.ServeHTTP(rec, r)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := make(chan *event.ReviewEvent, 1)
	go func() {
		_ = q.Receive(ctx, func(_ context.Context, m *queue.Message) error {
			got <- m.Event
			return nil
		})
	}()

	select {
	case ev := <-got:
		if ev.Trace == nil || !strings.HasPrefix(ev.Trace.TraceParent, "00-0af765") {
			t.Errorf("trace = %+v, want the inbound traceparent", ev.Trace)
		}
	case <-ctx.Done():
		t.Fatal("timed out")
	}
}

func TestReceiverSkips(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		event   string
	}{
		{"ping", "ping.json", "ping"},
		{"label added", "pull_request.labeled.json", "pull_request"},
		{"comment on an issue", "issue_comment.created_on_issue.json", "issue_comment"},
		{"kibitz's own comment", "issue_comment.created_by_bot.json", "issue_comment"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := memory.New()
			rc := newReceiver(t, q)
			body := fixture(t, tc.fixture)

			rec := httptest.NewRecorder()
			rc.ServeHTTP(rec, post(body, tc.event))

			if rec.Code != http.StatusNoContent {
				t.Errorf("status = %d, want 204: %s", rec.Code, rec.Body)
			}
			if q.Len() != 0 {
				t.Errorf("%d messages queued, want none", q.Len())
			}
		})
	}
}

func TestReceiverRejects(t *testing.T) {
	body := fixture(t, "pull_request.opened.json")

	tests := []struct {
		name    string
		request func() *http.Request
		status  int
	}{
		{
			name: "invalid signature",
			request: func() *http.Request {
				r := post(body, "pull_request")
				r.Header.Set(githubhook.HeaderSignature, "sha256="+strings.Repeat("00", 32))
				return r
			},
			status: http.StatusUnauthorized,
		},
		{
			name: "missing signature",
			request: func() *http.Request {
				r := post(body, "pull_request")
				r.Header.Del(githubhook.HeaderSignature)
				return r
			},
			status: http.StatusBadRequest,
		},
		{
			name: "malformed payload",
			request: func() *http.Request {
				broken := []byte(`{"action":"opened","pull_request":`)
				return post(broken, "pull_request")
			},
			status: http.StatusBadRequest,
		},
		{
			name: "wrong method",
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/webhook/github", nil)
			},
			status: http.StatusMethodNotAllowed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := memory.New()
			rc := newReceiver(t, q)

			rec := httptest.NewRecorder()
			rc.ServeHTTP(rec, tc.request())

			if rec.Code != tc.status {
				t.Errorf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
			if q.Len() != 0 {
				t.Errorf("%d messages queued, want none", q.Len())
			}
		})
	}
}

func TestReceiverRejectsOversizedBody(t *testing.T) {
	q := memory.New()
	rc := newReceiver(t, q, webhook.WithMaxBody(64))
	body := fixture(t, "pull_request.opened.json")

	rec := httptest.NewRecorder()
	rc.ServeHTTP(rec, post(body, "pull_request"))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

func TestReceiverRejectsStaleDelivery(t *testing.T) {
	q := memory.New()
	triggers := policy.New(policy.Config{
		AllowedRepos: []string{"*"},
		Mention:      "@kibitz",
		MaxEventAge:  time.Minute,
	})
	rc := webhook.NewReceiver(
		githubhook.New([]string{secret}), q, triggers, discardLogger(),
		// The fixture is from 2026-09-19; "now" is two days later.
		webhook.WithReceiverClock(func() time.Time { return now }),
	)

	rec := httptest.NewRecorder()
	rc.ServeHTTP(rec, post(fixture(t, "pull_request.opened.json"), "pull_request"))

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 for a replayed delivery", rec.Code)
	}
}

func TestReceiverMisconfiguredVerification(t *testing.T) {
	q := memory.New()
	triggers := policy.New(policy.Config{AllowedRepos: []string{"*"}, Mention: "@kibitz"})
	rc := webhook.NewReceiver(githubhook.New(nil), q, triggers, discardLogger())

	rec := httptest.NewRecorder()
	rc.ServeHTTP(rec, post(fixture(t, "pull_request.opened.json"), "pull_request"))

	// kibitz's own fault, so it must not look like the caller sent something
	// wrong: a 4xx here would be silently swallowed by the forge.
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// failingPublisher fails a fixed number of times before succeeding.
type failingPublisher struct {
	failures int32
	calls    atomic.Int32
}

func (p *failingPublisher) Publish(context.Context, *event.ReviewEvent) (string, error) {
	if p.calls.Add(1) <= p.failures {
		return "", errors.New("queue unavailable")
	}
	return "msg-1", nil
}

func (p *failingPublisher) Close() error { return nil }

func TestReceiverRetriesPublish(t *testing.T) {
	pub := &failingPublisher{failures: 2}
	rc := newReceiver(t, pub, webhook.WithPublishRetry(3, time.Millisecond, time.Second))

	rec := httptest.NewRecorder()
	rc.ServeHTTP(rec, post(fixture(t, "pull_request.opened.json"), "pull_request"))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 after a retry succeeded", rec.Code)
	}
	if got := pub.calls.Load(); got != 3 {
		t.Errorf("%d publish attempts, want 3", got)
	}
}

func TestReceiverReportsPublishFailure(t *testing.T) {
	pub := &failingPublisher{failures: 100}
	rc := newReceiver(t, pub, webhook.WithPublishRetry(2, time.Millisecond, time.Second))

	rec := httptest.NewRecorder()
	rc.ServeHTTP(rec, post(fixture(t, "pull_request.opened.json"), "pull_request"))

	// 5xx is what asks the forge to redeliver, and on GitHub it at least marks
	// the delivery as failed so it can be replayed by hand.
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if got := pub.calls.Load(); got != 2 {
		t.Errorf("%d publish attempts, want 2", got)
	}
}

// With tracing enabled the event carries kibitz's own span context, so the
// worker's job becomes a child of the webhook rather than a sibling.
func TestReceiverInjectsItsOwnTraceContext(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
	})

	q := memory.New()
	rc := newReceiver(t, q)

	r := post(fixture(t, "pull_request.opened.json"), "pull_request")
	rec := httptest.NewRecorder()
	rc.ServeHTTP(rec, r)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := make(chan *event.ReviewEvent, 1)
	go func() {
		_ = q.Receive(ctx, func(_ context.Context, m *queue.Message) error {
			got <- m.Event
			return nil
		})
	}()

	select {
	case ev := <-got:
		if ev.Trace == nil || ev.Trace.TraceParent == "" {
			t.Fatal("the event carries no trace context")
		}
		spans := exporter.GetSpans()
		if len(spans) == 0 {
			t.Fatal("no span was recorded")
		}
		if !strings.Contains(ev.Trace.TraceParent, spans[0].SpanContext.TraceID().String()) {
			t.Errorf("traceparent %q does not carry the recorded trace %s",
				ev.Trace.TraceParent, spans[0].SpanContext.TraceID())
		}
	case <-ctx.Done():
		t.Fatal("timed out")
	}
}
