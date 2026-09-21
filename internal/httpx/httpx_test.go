package httpx_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/httpx"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestRequestIDGenerated(t *testing.T) {
	var seen string
	h := httpx.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = httpx.RequestIDFrom(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen == "" {
		t.Fatal("no request ID in context")
	}
	if got := rec.Header().Get(httpx.RequestIDHeader); got != seen {
		t.Errorf("response header = %q, want %q", got, seen)
	}
}

func TestRequestIDReusesInboundValue(t *testing.T) {
	var seen string
	h := httpx.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = httpx.RequestIDFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(httpx.RequestIDHeader, "delivery-7f3c")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "delivery-7f3c" {
		t.Errorf("request ID = %q, want the inbound value", seen)
	}
}

func TestRequestIDRejectsOverlongInboundValue(t *testing.T) {
	var seen string
	h := httpx.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = httpx.RequestIDFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(httpx.RequestIDHeader, strings.Repeat("x", 200))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if len(seen) != 32 {
		t.Errorf("request ID = %q, want a generated one", seen)
	}
}

func TestRecoverTurnsPanicInto500(t *testing.T) {
	h := httpx.Chain(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }),
		httpx.RequestID,
		httpx.Recover(discardLogger()),
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestChainOrder(t *testing.T) {
	var order []string
	mw := func(name string) httpx.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}

	h := httpx.Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	}), mw("outer"), mw("inner"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"outer", "inner", "handler"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestLoggingRecordsStatus(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	h := httpx.Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("nope"))
	}), httpx.RequestID, httpx.Logging(logger))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/webhook/github", nil))

	var rec map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &rec); err != nil {
		t.Fatalf("decoding %q: %v", buf.String(), err)
	}
	if got := rec["status"].(float64); got != http.StatusTeapot {
		t.Errorf("status = %v, want 418", got)
	}
	if got := rec["bytes"].(float64); got != 4 {
		t.Errorf("bytes = %v, want 4", got)
	}
	if got := rec["path"].(string); got != "/webhook/github" {
		t.Errorf("path = %q", got)
	}
	if rec["request_id"] == "" {
		t.Error("request_id not logged")
	}
}

func TestHealthLive(t *testing.T) {
	h := httpx.NewHealth("v1.2.3")
	h.Register("queue", func(context.Context) error { return errors.New("unreachable") })

	rec := httptest.NewRecorder()
	h.Live(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	// Liveness must not depend on dependencies, or a queue outage restarts
	// every pod instead of just taking them out of rotation.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if body["version"] != "v1.2.3" {
		t.Errorf("version = %v", body["version"])
	}
}

func TestHealthReady(t *testing.T) {
	h := httpx.NewHealth("v1")

	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status with no checks = %d, want 200", rec.Code)
	}

	h.Register("ok", func(context.Context) error { return nil })
	h.Register("queue", func(context.Context) error { return errors.New("unreachable") })

	rec = httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status with a failing check = %d, want 503", rec.Code)
	}

	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if body.Status != "unavailable" {
		t.Errorf("status = %q", body.Status)
	}
	if body.Checks["queue"] != "unreachable" {
		t.Errorf("queue check = %q, want the failure reason", body.Checks["queue"])
	}
	if body.Checks["ok"] != "ok" {
		t.Errorf("ok check = %q", body.Checks["ok"])
	}
}
