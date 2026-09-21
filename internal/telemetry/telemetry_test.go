package telemetry_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/yteraoka/kibitz/internal/config"
	"github.com/yteraoka/kibitz/internal/telemetry"
)

func TestRedact(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		keep  string // substring that must survive
		leaks string // substring that must not survive
	}{
		{
			name:  "github token",
			in:    "remote rejected: ghp_0123456789abcdefghijABCDEFGHIJ is invalid",
			keep:  "remote rejected",
			leaks: "ghp_0123456789abcdefghijABCDEFGHIJ",
		},
		{
			name:  "fine grained pat",
			in:    "github_pat_11ABCDEFG0abcdefghijklmnop",
			leaks: "github_pat_11ABCDEFG0abcdefghijklmnop",
		},
		{
			name:  "gitlab token",
			in:    "token glpat-ABCDEFGHIJKLMNOPQR",
			leaks: "glpat-ABCDEFGHIJKLMNOPQR",
		},
		{
			name:  "anthropic key",
			in:    "sk-ant-api03-abcdefghijklmnopqrstuvwxyz",
			leaks: "sk-ant-api03-abcdefghijklmnopqrstuvwxyz",
		},
		{
			name:  "google access token",
			in:    "ya29.a0ARrdaM9abcdefghijklmnopqrstuvwxyz",
			leaks: "ya29.a0ARrdaM9abcdefghijklmnopqrstuvwxyz",
		},
		{
			name:  "private key block",
			in:    "key: -----BEGIN RSA PRIVATE KEY-----\nMIIEow==\n-----END RSA PRIVATE KEY-----",
			keep:  "key:",
			leaks: "MIIEow==",
		},
		{
			name:  "service account json",
			in:    `{"type":"service_account","private_key":"-----BEGIN PRIVATE KEY-----abc"}`,
			keep:  "service_account",
			leaks: "BEGIN PRIVATE KEY-----abc",
		},
		{
			name:  "authorization header",
			in:    "Authorization: Bearer abcdefghijklmnop",
			keep:  "Authorization",
			leaks: "abcdefghijklmnop",
		},
		{
			name:  "credentials in clone url",
			in:    "fatal: could not read https://x-access-token:ghs_secretvalue@github.com/o/r.git",
			keep:  "github.com/o/r.git",
			leaks: "ghs_secretvalue",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := telemetry.Redact(tc.in)
			if tc.leaks != "" && strings.Contains(got, tc.leaks) {
				t.Errorf("Redact leaked %q:\n%s", tc.leaks, got)
			}
			if tc.keep != "" && !strings.Contains(got, tc.keep) {
				t.Errorf("Redact dropped %q:\n%s", tc.keep, got)
			}
		})
	}
}

func TestRedactLeavesOrdinaryTextAlone(t *testing.T) {
	const in = "reviewed pull request 42 in yteraoka/kibitz (head abc1234)"
	if got := telemetry.Redact(in); got != in {
		t.Errorf("Redact changed ordinary text:\ngot  %s\nwant %s", got, in)
	}
}

func logAndDecode(t *testing.T, log func(*slog.Logger)) map[string]any {
	t.Helper()

	var buf bytes.Buffer
	logger := telemetry.New(&buf, telemetry.Options{Level: slog.LevelDebug, Service: "test", Version: "v0"})
	log(logger)

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("decoding log record %q: %v", buf.String(), err)
	}
	return record
}

func TestLoggerRedactsMessageAndAttrs(t *testing.T) {
	record := logAndDecode(t, func(l *slog.Logger) {
		l.Info("clone failed for ghp_0123456789abcdefghijABCDEFGHIJ",
			slog.String("url", "https://x-access-token:ghs_abcdefghij@github.com/o/r.git"),
			slog.String("repo", "yteraoka/kibitz"),
		)
	})

	if msg := record["msg"].(string); strings.Contains(msg, "ghp_") {
		t.Errorf("message leaked a token: %s", msg)
	}
	if url := record["url"].(string); strings.Contains(url, "ghs_") {
		t.Errorf("attribute leaked a token: %s", url)
	}
	if repo := record["repo"].(string); repo != "yteraoka/kibitz" {
		t.Errorf("repo = %q, want it untouched", repo)
	}
	if record["service"] != "test" || record["version"] != "v0" {
		t.Errorf("service/version attributes missing: %v", record)
	}
}

func TestLoggerRedactsSecretsGroupsAndErrors(t *testing.T) {
	record := logAndDecode(t, func(l *slog.Logger) {
		l.With(slog.String("ctx", "glpat-ABCDEFGHIJKLMNOPQR")).Info("start",
			slog.Any("secret", config.Secret("hunter2")),
			slog.Any("err", errors.New("push rejected: ghp_0123456789abcdefghijABCDEFGHIJ")),
			slog.Group("github", slog.String("key", "-----BEGIN PRIVATE KEY-----abc-----END PRIVATE KEY-----")),
		)
	})

	if got := record["secret"]; got != config.Redacted {
		t.Errorf("secret = %v, want %s", got, config.Redacted)
	}
	if got := record["err"].(string); strings.Contains(got, "ghp_") {
		t.Errorf("err leaked a token: %s", got)
	}
	if got := record["ctx"].(string); strings.Contains(got, "glpat-") {
		t.Errorf("WithAttrs value leaked a token: %s", got)
	}
	group, ok := record["github"].(map[string]any)
	if !ok {
		t.Fatalf("github group missing: %v", record)
	}
	if got := group["key"].(string); strings.Contains(got, "BEGIN PRIVATE KEY") {
		t.Errorf("group attribute leaked a private key: %s", got)
	}
}

func TestLoggerRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := telemetry.New(&buf, telemetry.Options{Level: slog.LevelWarn})
	logger.Info("should not appear")
	if buf.Len() != 0 {
		t.Errorf("info record emitted at warn level: %s", buf.String())
	}
	logger.Warn("should appear")
	if buf.Len() == 0 {
		t.Error("warn record was dropped")
	}
}

func TestTextFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := telemetry.New(&buf, telemetry.Options{Level: slog.LevelInfo, Format: "text"})
	logger.Info("hello", slog.String("token", "ghp_0123456789abcdefghijABCDEFGHIJ"))

	out := buf.String()
	if !strings.Contains(out, "msg=hello") {
		t.Errorf("text handler not used: %s", out)
	}
	if strings.Contains(out, "ghp_") {
		t.Errorf("text handler leaked a token: %s", out)
	}
}
