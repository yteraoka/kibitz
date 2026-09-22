package gitlab_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/webhook"
	gitlabhook "github.com/yteraoka/kibitz/internal/webhook/gitlab"
)

const (
	token        = "dev-token"
	signingToken = "whsec_" // completed in signingKey
)

var now = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

// key is the raw signing key; GitLab hands it out base64-encoded behind a
// "whsec_" prefix.
var key = []byte("kibitz-signing-key")

func signingTokenValue() string {
	return signingToken + base64.StdEncoding.EncodeToString(key)
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "webhooks", "gitlab", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return b
}

func newHandler(opts ...gitlabhook.Option) *gitlabhook.Handler {
	opts = append([]gitlabhook.Option{gitlabhook.WithClock(func() time.Time { return now })}, opts...)
	return gitlabhook.New([]string{token}, []string{signingTokenValue()}, opts...)
}

func request(eventName string, body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", strings.NewReader(string(body)))
	r.Header.Set(gitlabhook.HeaderEvent, eventName)
	r.Header.Set(gitlabhook.HeaderWebhookID, "f5e5f430-f57b-4e6e-9fac-d9128cd7232f")
	r.Header.Set(gitlabhook.HeaderInstance, "https://gitlab.example.com")
	r.Header.Set(gitlabhook.HeaderToken, token)
	r.Header.Set("Content-Type", "application/json")
	return r
}

// sign turns a request into a signed one, the way GitLab 19 does it.
func sign(r *http.Request, body []byte, at time.Time) {
	id := r.Header.Get(gitlabhook.HeaderWebhookID)
	timestamp := strconv.FormatInt(at.Unix(), 10)

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + timestamp + "." + string(body)))

	r.Header.Del(gitlabhook.HeaderToken)
	r.Header.Set(gitlabhook.HeaderWebhookTimestamp, timestamp)
	r.Header.Set(gitlabhook.HeaderWebhookSignature, "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
}

func TestVerifyToken(t *testing.T) {
	body := fixture(t, "merge_request.open.json")

	t.Run("accepts the configured token", func(t *testing.T) {
		if err := newHandler().Verify(request(gitlabhook.EventMergeRequest, body), body); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	})

	t.Run("accepts any token while rotating", func(t *testing.T) {
		h := gitlabhook.New([]string{"retired", token}, nil)
		if err := h.Verify(request(gitlabhook.EventMergeRequest, body), body); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	})

	t.Run("rejects the wrong token", func(t *testing.T) {
		r := request(gitlabhook.EventMergeRequest, body)
		r.Header.Set(gitlabhook.HeaderToken, "nope")
		if err := newHandler().Verify(r, body); !errors.Is(err, webhook.ErrInvalidSignature) {
			t.Fatalf("err = %v, want ErrInvalidSignature", err)
		}
	})

	t.Run("rejects a delivery with no credential at all", func(t *testing.T) {
		r := request(gitlabhook.EventMergeRequest, body)
		r.Header.Del(gitlabhook.HeaderToken)
		if err := newHandler().Verify(r, body); !errors.Is(err, webhook.ErrMissingSignature) {
			t.Fatalf("err = %v, want ErrMissingSignature", err)
		}
	})

	t.Run("reports its own misconfiguration", func(t *testing.T) {
		h := gitlabhook.New(nil, nil)
		if err := h.Verify(request(gitlabhook.EventMergeRequest, body), body); !errors.Is(err, webhook.ErrNoSecrets) {
			t.Fatalf("err = %v, want ErrNoSecrets", err)
		}
	})
}

// GitLab 19 signs the body. Unlike the shared token, that says what arrived
// and not only who sent it.
func TestVerifySignature(t *testing.T) {
	body := fixture(t, "merge_request.open.json")

	t.Run("accepts a valid signature", func(t *testing.T) {
		r := request(gitlabhook.EventMergeRequest, body)
		sign(r, body, now)
		if err := newHandler().Verify(r, body); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	})

	t.Run("accepts one of several signatures", func(t *testing.T) {
		r := request(gitlabhook.EventMergeRequest, body)
		sign(r, body, now)
		r.Header.Set(gitlabhook.HeaderWebhookSignature, "v1,AAAA "+r.Header.Get(gitlabhook.HeaderWebhookSignature))
		if err := newHandler().Verify(r, body); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	})

	t.Run("rejects a tampered body", func(t *testing.T) {
		r := request(gitlabhook.EventMergeRequest, body)
		sign(r, body, now)

		tampered := append([]byte(nil), body...)
		tampered[len(tampered)-2] = ' '
		if err := newHandler().Verify(r, tampered); !errors.Is(err, webhook.ErrInvalidSignature) {
			t.Fatalf("err = %v, want ErrInvalidSignature", err)
		}
	})

	// The timestamp is inside the signed message, so bounding it is what
	// stops a captured delivery being replayed forever.
	t.Run("rejects a stale delivery", func(t *testing.T) {
		r := request(gitlabhook.EventMergeRequest, body)
		sign(r, body, now.Add(-time.Hour))
		if err := newHandler().Verify(r, body); !errors.Is(err, webhook.ErrInvalidSignature) {
			t.Fatalf("err = %v, want ErrInvalidSignature", err)
		}
	})

	t.Run("rejects a signature with no timestamp", func(t *testing.T) {
		r := request(gitlabhook.EventMergeRequest, body)
		sign(r, body, now)
		r.Header.Del(gitlabhook.HeaderWebhookTimestamp)
		if err := newHandler().Verify(r, body); !errors.Is(err, webhook.ErrMissingSignature) {
			t.Fatalf("err = %v, want ErrMissingSignature", err)
		}
	})

	// An instance too old to sign still sends the shared token, and kibitz
	// cannot tell it to do otherwise.
	t.Run("falls back to the token when nothing was signed", func(t *testing.T) {
		if err := newHandler().Verify(request(gitlabhook.EventMergeRequest, body), body); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	})
}

func TestNormalizeKinds(t *testing.T) {
	tests := []struct {
		fixture string
		event   string
		want    event.Kind
	}{
		{fixture: "merge_request.open.json", event: gitlabhook.EventMergeRequest, want: event.KindPROpened},
		{fixture: "merge_request.update_push.json", event: gitlabhook.EventMergeRequest, want: event.KindPRUpdated},
		{fixture: "merge_request.ready.json", event: gitlabhook.EventMergeRequest, want: event.KindPRReadyForReview},
		{fixture: "merge_request.review_requested.json", event: gitlabhook.EventMergeRequest, want: event.KindPRReviewRequested},
		{fixture: "merge_request.merged.json", event: gitlabhook.EventMergeRequest, want: event.KindPRMerged},
		{fixture: "note.merge_request.json", event: gitlabhook.EventNote, want: event.KindCommentCreated},
		{fixture: "note.diff.json", event: gitlabhook.EventNote, want: event.KindCommentCreated},
	}

	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			body := fixture(t, tc.fixture)
			ev, err := newHandler().Normalize(request(tc.event, body), body)
			if err != nil {
				t.Fatalf("Normalize: %v", err)
			}
			if ev == nil {
				t.Fatal("Normalize returned nothing")
			}
			if ev.Kind != tc.want {
				t.Errorf("kind = %s, want %s", ev.Kind, tc.want)
			}
			if err := ev.Validate(); err != nil {
				t.Errorf("the normalized event is not valid: %v", err)
			}
		})
	}
}

// Well formed, and nothing kibitz should act on.
func TestNormalizeIgnores(t *testing.T) {
	tests := []struct {
		fixture string
		event   string
		why     string
	}{
		{fixture: "merge_request.update_title.json", event: gitlabhook.EventMergeRequest, why: "an edited title is not new code"},
		{fixture: "merge_request.approved.json", event: gitlabhook.EventMergeRequest, why: "an approval is not a change"},
		{fixture: "note.issue.json", event: gitlabhook.EventNote, why: "notes fire for issues too"},
		{fixture: "note.system.json", event: gitlabhook.EventNote, why: "GitLab wrote it, not a person"},
		{fixture: "merge_request.open.json", event: "Pipeline Hook", why: "kibitz does not review pipelines"},
	}

	for _, tc := range tests {
		t.Run(tc.fixture+" "+tc.event, func(t *testing.T) {
			body := fixture(t, tc.fixture)
			ev, err := newHandler().Normalize(request(tc.event, body), body)
			if err != nil {
				t.Fatalf("Normalize: %v", err)
			}
			if ev != nil {
				t.Errorf("Normalize returned an event (%s), want nil: %s", ev.Kind, tc.why)
			}
		})
	}
}

func TestNormalizeMergeRequest(t *testing.T) {
	body := fixture(t, "merge_request.open.json")

	ev, err := newHandler().Normalize(request(gitlabhook.EventMergeRequest, body), body)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	if ev.Source.Platform != event.PlatformGitLab {
		t.Errorf("platform = %s", ev.Source.Platform)
	}
	if ev.Source.InstanceURL != "https://gitlab.example.com" {
		t.Errorf("instance = %q", ev.Source.InstanceURL)
	}
	if ev.Source.DeliveryID != "f5e5f430-f57b-4e6e-9fac-d9128cd7232f" {
		t.Errorf("delivery = %q, want the idempotency key", ev.Source.DeliveryID)
	}
	if ev.Source.EventName != "merge_request.open" {
		t.Errorf("event name = %q", ev.Source.EventName)
	}
	if ev.Repository.FullName != "yteraoka/kibitz" || ev.Repository.Owner != "yteraoka" {
		t.Errorf("repository = %+v", ev.Repository)
	}
	if ev.Repository.Visibility != "private" {
		t.Errorf("visibility = %q, want private for level 0", ev.Repository.Visibility)
	}
	if ev.Repository.CloneURL == "" {
		t.Error("no clone url")
	}
	// The internal id is what a merge request is called in its URL, not the
	// database id.
	if ev.PullRequest.Number != 16 {
		t.Errorf("number = %d, want the iid", ev.PullRequest.Number)
	}
	if ev.PullRequest.State != "open" {
		t.Errorf("state = %q, want open for GitLab's opened", ev.PullRequest.State)
	}
	if ev.PullRequest.Source.SHA == "" {
		t.Error("no head sha")
	}
	if ev.PullRequest.IsFork {
		t.Error("a merge request within one project is not from a fork")
	}
	if ev.Actor.Login != "yteraoka" {
		t.Errorf("actor = %+v", ev.Actor)
	}
	if ev.OccurredAt.IsZero() {
		t.Error("no timestamp")
	}
}

func TestNormalizeDiffNote(t *testing.T) {
	body := fixture(t, "note.diff.json")

	ev, err := newHandler().Normalize(request(gitlabhook.EventNote, body), body)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if ev.Comment == nil {
		t.Fatal("no comment")
	}
	if ev.Comment.Path != "internal/queue/sqs/subscriber.go" || ev.Comment.Line != 88 {
		t.Errorf("position = %s:%d", ev.Comment.Path, ev.Comment.Line)
	}
	// GitLab calls a thread a discussion, and a reply has to go back to it.
	if ev.Comment.ThreadID != "d7a0e5b1c2f34567" {
		t.Errorf("thread = %q, want the discussion id", ev.Comment.ThreadID)
	}
	if ev.PullRequest == nil || ev.PullRequest.Number != 16 {
		t.Errorf("pull request = %+v", ev.PullRequest)
	}
}

func TestNormalizeRejectsMalformedPayloads(t *testing.T) {
	var malformed *webhook.ErrMalformedPayload

	_, err := newHandler().Normalize(request(gitlabhook.EventMergeRequest, []byte("{")), []byte("{"))
	if !errors.As(err, &malformed) {
		t.Fatalf("err = %v, want ErrMalformedPayload", err)
	}
}

// A delivery that never became an event is still traceable.
func TestDelivery(t *testing.T) {
	body := fixture(t, "merge_request.open.json")
	r := request(gitlabhook.EventMergeRequest, body)

	id, name := newHandler().Delivery(r)
	if id != "f5e5f430-f57b-4e6e-9fac-d9128cd7232f" {
		t.Errorf("id = %q", id)
	}
	if name != gitlabhook.EventMergeRequest {
		t.Errorf("name = %q", name)
	}
}

// The event UUID is shared by recursive webhooks, so it is the last resort
// rather than the first choice.
func TestDeliveryFallsBackToTheEventUUID(t *testing.T) {
	body := fixture(t, "merge_request.open.json")
	r := request(gitlabhook.EventMergeRequest, body)
	r.Header.Del(gitlabhook.HeaderWebhookID)
	r.Header.Set(gitlabhook.HeaderEventUUID, "13792a34-cac6-4fda-95a8-c58e00a3954e")

	if id, _ := newHandler().Delivery(r); id != "13792a34-cac6-4fda-95a8-c58e00a3954e" {
		t.Errorf("id = %q, want the event uuid", id)
	}
}
