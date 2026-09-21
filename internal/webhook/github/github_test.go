package github_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/webhook"
	githubhook "github.com/yteraoka/kibitz/internal/webhook/github"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const secret = "dev-secret"

// fixedNow is used wherever a payload carries no timestamp, so golden files
// stay stable.
var fixedNow = time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)

func newHandler() *githubhook.Handler {
	return githubhook.New([]string{secret}, githubhook.WithClock(func() time.Time { return fixedNow }))
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "webhooks", "github", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return b
}

func sign(body []byte, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func request(eventName, delivery string, body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(string(body)))
	r.Header.Set(githubhook.HeaderEvent, eventName)
	r.Header.Set(githubhook.HeaderDelivery, delivery)
	r.Header.Set(githubhook.HeaderSignature, sign(body, secret))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestVerify(t *testing.T) {
	body := fixture(t, "pull_request.opened.json")

	t.Run("accepts a valid signature", func(t *testing.T) {
		if err := newHandler().Verify(request("pull_request", "d1", body), body); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	})

	t.Run("accepts any configured secret while rotating", func(t *testing.T) {
		h := githubhook.New([]string{"retired", secret})
		if err := h.Verify(request("pull_request", "d1", body), body); err != nil {
			t.Fatalf("Verify with a rotated secret: %v", err)
		}
	})

	t.Run("rejects a tampered body", func(t *testing.T) {
		r := request("pull_request", "d1", body)
		tampered := append([]byte(nil), body...)
		tampered[len(tampered)-2] = ' '
		if err := newHandler().Verify(r, tampered); !errors.Is(err, webhook.ErrInvalidSignature) {
			t.Fatalf("err = %v, want ErrInvalidSignature", err)
		}
	})

	t.Run("rejects the wrong secret", func(t *testing.T) {
		r := request("pull_request", "d1", body)
		r.Header.Set(githubhook.HeaderSignature, sign(body, "someone-elses-secret"))
		if err := newHandler().Verify(r, body); !errors.Is(err, webhook.ErrInvalidSignature) {
			t.Fatalf("err = %v, want ErrInvalidSignature", err)
		}
	})

	t.Run("rejects a missing signature", func(t *testing.T) {
		r := request("pull_request", "d1", body)
		r.Header.Del(githubhook.HeaderSignature)
		if err := newHandler().Verify(r, body); !errors.Is(err, webhook.ErrMissingSignature) {
			t.Fatalf("err = %v, want ErrMissingSignature", err)
		}
	})

	t.Run("rejects a signature without the algorithm prefix", func(t *testing.T) {
		r := request("pull_request", "d1", body)
		r.Header.Set(githubhook.HeaderSignature, strings.TrimPrefix(sign(body, secret), "sha256="))
		if err := newHandler().Verify(r, body); !errors.Is(err, webhook.ErrInvalidSignature) {
			t.Fatalf("err = %v, want ErrInvalidSignature", err)
		}
	})

	t.Run("rejects a non-hexadecimal signature", func(t *testing.T) {
		r := request("pull_request", "d1", body)
		r.Header.Set(githubhook.HeaderSignature, "sha256=not-hex")
		if err := newHandler().Verify(r, body); !errors.Is(err, webhook.ErrInvalidSignature) {
			t.Fatalf("err = %v, want ErrInvalidSignature", err)
		}
	})

	t.Run("reports its own misconfiguration", func(t *testing.T) {
		h := githubhook.New(nil)
		if err := h.Verify(request("pull_request", "d1", body), body); !errors.Is(err, webhook.ErrNoSecrets) {
			t.Fatalf("err = %v, want ErrNoSecrets", err)
		}
	})
}

// TestNormalizeGolden pins the normalized output of real payload shapes.
// Run `go test ./internal/webhook/github -update` to refresh the golden files.
func TestNormalizeGolden(t *testing.T) {
	cases := []struct {
		fixture string
		event   string
	}{
		{"pull_request.opened.json", "pull_request"},
		{"pull_request.synchronize.json", "pull_request"},
		{"pull_request.ready_for_review.json", "pull_request"},
		{"pull_request.closed_merged.json", "pull_request"},
		{"pull_request.opened_fork.json", "pull_request"},
		{"issue_comment.created.json", "issue_comment"},
		{"issue_comment.created_by_bot.json", "issue_comment"},
		{"pull_request_review_comment.created.json", "pull_request_review_comment"},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			body := fixture(t, tc.fixture)
			delivery := "7f3c" + strings.TrimSuffix(tc.fixture, ".json")

			ev, err := newHandler().Normalize(request(tc.event, delivery, body), body)
			if err != nil {
				t.Fatalf("Normalize: %v", err)
			}
			if ev == nil {
				t.Fatal("Normalize returned no event")
			}
			if err := ev.Validate(); err != nil {
				t.Fatalf("normalized event is invalid: %v", err)
			}

			got, err := json.MarshalIndent(ev, "", "  ")
			if err != nil {
				t.Fatalf("marshalling: %v", err)
			}
			got = append(got, '\n')

			golden := filepath.Join("..", "..", "..", "testdata", "events", "github",
				strings.TrimSuffix(tc.fixture, ".json")+".golden.json")
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatalf("writing golden: %v", err)
				}
				return
			}

			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("reading golden (run with -update to create it): %v", err)
			}
			if string(got) != string(want) {
				t.Errorf("normalized event differs from %s\n--- got ---\n%s\n--- want ---\n%s", golden, got, want)
			}
		})
	}
}

func TestNormalizeIgnoresUninterestingDeliveries(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		event   string
	}{
		{"ping", "ping.json", "ping"},
		{"label added", "pull_request.labeled.json", "pull_request"},
		{"comment on an issue", "issue_comment.created_on_issue.json", "issue_comment"},
		{"unhandled event type", "pull_request.opened.json", "check_run"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fixture(t, tc.fixture)
			ev, err := newHandler().Normalize(request(tc.event, "d1", body), body)
			if err != nil {
				t.Fatalf("Normalize: %v", err)
			}
			if ev != nil {
				t.Errorf("Normalize returned %s, want no event", ev.Kind)
			}
		})
	}
}

func TestNormalizeRejectsMalformedPayload(t *testing.T) {
	body := []byte(`{"action": "opened", "pull_request": `)

	_, err := newHandler().Normalize(request("pull_request", "d1", body), body)
	var malformed *webhook.ErrMalformedPayload
	if !errors.As(err, &malformed) {
		t.Fatalf("err = %v, want ErrMalformedPayload", err)
	}
	if malformed.Platform != event.PlatformGitHub {
		t.Errorf("platform = %q", malformed.Platform)
	}
}

func TestNormalizeDetailsWorthCallingOut(t *testing.T) {
	t.Run("a fork is flagged", func(t *testing.T) {
		body := fixture(t, "pull_request.opened_fork.json")
		ev, err := newHandler().Normalize(request("pull_request", "d1", body), body)
		if err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		if !ev.PullRequest.IsFork {
			t.Error("IsFork = false, want true for a pull request from a fork")
		}
	})

	t.Run("a same-repo branch is not a fork", func(t *testing.T) {
		body := fixture(t, "pull_request.opened.json")
		ev, err := newHandler().Normalize(request("pull_request", "d1", body), body)
		if err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		if ev.PullRequest.IsFork {
			t.Error("IsFork = true, want false for a branch in the same repository")
		}
	})

	t.Run("a merged pull request is not merely closed", func(t *testing.T) {
		body := fixture(t, "pull_request.closed_merged.json")
		ev, err := newHandler().Normalize(request("pull_request", "d1", body), body)
		if err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		if ev.Kind != event.KindPRMerged {
			t.Errorf("kind = %s, want %s", ev.Kind, event.KindPRMerged)
		}
	})

	t.Run("a bot author is marked", func(t *testing.T) {
		body := fixture(t, "issue_comment.created_by_bot.json")
		ev, err := newHandler().Normalize(request("issue_comment", "d1", body), body)
		if err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		if !ev.Actor.IsBot {
			t.Error("Actor.IsBot = false, want true")
		}
		if ev.Actor.Login != "kibitz[bot]" {
			t.Errorf("Actor.Login = %q", ev.Actor.Login)
		}
	})

	t.Run("a review comment keeps its thread and position", func(t *testing.T) {
		body := fixture(t, "pull_request_review_comment.created.json")
		ev, err := newHandler().Normalize(request("pull_request_review_comment", "d1", body), body)
		if err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		if ev.Comment.Path != "internal/queue/sqs/subscriber.go" || ev.Comment.Line != 88 {
			t.Errorf("comment position = %s:%d", ev.Comment.Path, ev.Comment.Line)
		}
		if ev.Comment.InReplyTo != "666000111" || ev.Comment.ThreadID != "666000111" {
			t.Errorf("thread = %q, in reply to %q", ev.Comment.ThreadID, ev.Comment.InReplyTo)
		}
	})

	t.Run("the event id follows the delivery id", func(t *testing.T) {
		body := fixture(t, "pull_request.opened.json")
		ev, err := newHandler().Normalize(request("pull_request", "abc-123", body), body)
		if err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		if ev.Source.DeliveryID != "abc-123" {
			t.Errorf("delivery id = %q", ev.Source.DeliveryID)
		}
		if ev.ID != "github:abc-123" {
			t.Errorf("event id = %q, want it derived from the delivery id", ev.ID)
		}
		if got, want := ev.Key(), "github/yteraoka/kibitz/42"; got != want {
			t.Errorf("Key() = %q, want %q", got, want)
		}
	})

	t.Run("the instance url is taken from the payload", func(t *testing.T) {
		body := fixture(t, "pull_request.opened.json")
		ev, err := newHandler().Normalize(request("pull_request", "d1", body), body)
		if err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		if ev.Source.InstanceURL != "https://github.com" {
			t.Errorf("instance url = %q", ev.Source.InstanceURL)
		}
	})
}

// A mismatch is nearly always the wrong value rather than an attack, and
// neither GitHub nor kibitz will show the secret it holds. The fingerprint in
// the error is what lets an operator compare the two.
func TestVerifyReportsWhichSecretItHolds(t *testing.T) {
	body := fixture(t, "pull_request.opened.json")
	r := request("pull_request", "d1", body)

	err := githubhook.New([]string{"not-the-secret"}).Verify(r, body)
	if !errors.Is(err, webhook.ErrInvalidSignature) {
		t.Fatalf("err = %v, want ErrInvalidSignature", err)
	}

	fingerprint := webhook.Fingerprint("not-the-secret")
	if fingerprint == "" {
		t.Fatal("Fingerprint returned nothing")
	}
	if !strings.Contains(err.Error(), fingerprint) {
		t.Errorf("error = %q, want it to name the fingerprint %s", err, fingerprint)
	}
	if strings.Contains(err.Error(), "not-the-secret") {
		t.Errorf("error = %q, want it to keep the secret to itself", err)
	}
}

func TestFingerprintIsStableAndShort(t *testing.T) {
	// The value an operator reproduces with:
	//   printf '%s' 'kibitz' | sha256sum | cut -c1-12
	if got, want := webhook.Fingerprint("kibitz"), "1dac8899aa71"; got != want {
		t.Errorf("Fingerprint = %q, want %q", got, want)
	}
	if got := webhook.Fingerprint(""); got != "" {
		t.Errorf("Fingerprint of nothing = %q, want empty", got)
	}
}

// GitHub names the app that wrote an issue comment. It is the only part of a
// delivery that identifies kibitz without kibitz being told what it is called,
// so it has to survive normalization.
func TestNormalizeCarriesTheAppThatWroteTheComment(t *testing.T) {
	body := fixture(t, "issue_comment.created_by_bot.json")

	ev, err := newHandler().Normalize(request("issue_comment", "d1", body), body)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if ev == nil || ev.Comment == nil {
		t.Fatalf("event = %+v", ev)
	}
	if got := ev.Comment.Author.AppID; got != "123456" {
		t.Errorf("app id = %q, want 123456", got)
	}
	if got := ev.Comment.Author.AppSlug; got != "kibitz" {
		t.Errorf("app slug = %q, want kibitz", got)
	}
}

// A person's comment has no app, and must not be given one.
func TestNormalizeLeavesHumanCommentsUnattributed(t *testing.T) {
	body := fixture(t, "issue_comment.created.json")

	ev, err := newHandler().Normalize(request("issue_comment", "d1", body), body)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if ev == nil || ev.Comment == nil {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Comment.Author.AppID != "" {
		t.Errorf("app id = %q, want none", ev.Comment.Author.AppID)
	}
}
