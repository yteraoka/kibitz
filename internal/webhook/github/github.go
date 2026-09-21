// Package github verifies and normalizes GitHub webhooks. It serves
// github.com and GitHub Enterprise Server alike; the instance is derived from
// the payload rather than configured.
package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/webhook"
)

// GitHub webhook headers.
const (
	HeaderEvent     = "X-GitHub-Event"
	HeaderDelivery  = "X-GitHub-Delivery"
	HeaderSignature = "X-Hub-Signature-256"
)

const signaturePrefix = "sha256="

// Handler implements [webhook.Handler] for GitHub.
type Handler struct {
	secrets []string
	now     func() time.Time
}

// Option customizes a handler.
type Option func(*Handler)

// WithClock replaces the clock. Tests use it to make normalization
// deterministic when a payload carries no timestamp.
func WithClock(now func() time.Time) Option {
	return func(h *Handler) { h.now = now }
}

// New creates a handler that accepts any of the given webhook secrets, which
// is what lets a secret be rotated without dropping deliveries.
func New(secrets []string, opts ...Option) *Handler {
	h := &Handler{now: time.Now}
	for _, s := range secrets {
		if s = strings.TrimSpace(s); s != "" {
			h.secrets = append(h.secrets, s)
		}
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Platform implements [webhook.Handler].
func (h *Handler) Platform() event.Platform { return event.PlatformGitHub }

// Verify checks the HMAC-SHA256 signature GitHub computes over the raw body.
func (h *Handler) Verify(r *http.Request, body []byte) error {
	if len(h.secrets) == 0 {
		return webhook.ErrNoSecrets
	}

	header := r.Header.Get(HeaderSignature)
	if header == "" {
		return webhook.ErrMissingSignature
	}
	if !strings.HasPrefix(header, signaturePrefix) {
		return fmt.Errorf("%w: expected the %s prefix", webhook.ErrInvalidSignature, signaturePrefix)
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, signaturePrefix))
	if err != nil {
		return fmt.Errorf("%w: signature is not hexadecimal", webhook.ErrInvalidSignature)
	}

	for _, secret := range h.secrets {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		if hmac.Equal(mac.Sum(nil), want) {
			return nil
		}
	}
	return webhook.ErrInvalidSignature
}

// Normalize implements [webhook.Handler].
func (h *Handler) Normalize(r *http.Request, body []byte) (*event.ReviewEvent, error) {
	name := r.Header.Get(HeaderEvent)
	delivery := r.Header.Get(HeaderDelivery)

	switch name {
	case "pull_request":
		return h.normalizePullRequest(name, delivery, body)
	case "issue_comment":
		return h.normalizeIssueComment(name, delivery, body)
	case "pull_request_review_comment":
		return h.normalizeReviewComment(name, delivery, body)
	default:
		// ping, push, check_run and everything else kibitz does not act on.
		return nil, nil
	}
}

func (h *Handler) normalizePullRequest(name, delivery string, body []byte) (*event.ReviewEvent, error) {
	var p pullRequestPayload
	if err := unmarshal(body, &p); err != nil {
		return nil, err
	}

	kind, ok := pullRequestKind(p.Action, p.PullRequest.Merged)
	if !ok {
		return nil, nil
	}

	ev := h.newEvent(name, p.Action, delivery, kind, p.Repository, p.Sender)
	ev.PullRequest = p.PullRequest.normalize(p.Repository)
	ev.OccurredAt = firstNonZero(p.PullRequest.UpdatedAt, p.PullRequest.CreatedAt, h.now())
	return ev, ev.Validate()
}

func (h *Handler) normalizeIssueComment(name, delivery string, body []byte) (*event.ReviewEvent, error) {
	var p issueCommentPayload
	if err := unmarshal(body, &p); err != nil {
		return nil, err
	}
	// The same event fires for issues; only the pull_request member tells them
	// apart.
	if p.Action != "created" || p.Issue.PullRequest == nil {
		return nil, nil
	}

	ev := h.newEvent(name, p.Action, delivery, event.KindCommentCreated, p.Repository, p.Sender)
	ev.PullRequest = p.Issue.normalize()
	ev.Comment = &event.Comment{
		ID:     strconv.FormatInt(p.Comment.ID, 10),
		Body:   p.Comment.Body,
		Author: p.Comment.User.normalize(),
		URL:    p.Comment.HTMLURL,
	}
	ev.OccurredAt = firstNonZero(p.Comment.CreatedAt, h.now())
	return ev, ev.Validate()
}

func (h *Handler) normalizeReviewComment(name, delivery string, body []byte) (*event.ReviewEvent, error) {
	var p reviewCommentPayload
	if err := unmarshal(body, &p); err != nil {
		return nil, err
	}
	if p.Action != "created" {
		return nil, nil
	}

	ev := h.newEvent(name, p.Action, delivery, event.KindCommentCreated, p.Repository, p.Sender)
	ev.PullRequest = p.PullRequest.normalize(p.Repository)
	ev.Comment = &event.Comment{
		ID:     strconv.FormatInt(p.Comment.ID, 10),
		Body:   p.Comment.Body,
		Author: p.Comment.User.normalize(),
		Path:   p.Comment.Path,
		Line:   firstNonZeroInt(p.Comment.Line, p.Comment.OriginalLine),
		URL:    p.Comment.HTMLURL,
	}
	// GitHub threads replies under the comment that started them.
	if p.Comment.InReplyToID != 0 {
		ev.Comment.InReplyTo = strconv.FormatInt(p.Comment.InReplyToID, 10)
		ev.Comment.ThreadID = strconv.FormatInt(p.Comment.InReplyToID, 10)
	} else {
		ev.Comment.ThreadID = ev.Comment.ID
	}
	ev.OccurredAt = firstNonZero(p.Comment.CreatedAt, h.now())
	return ev, ev.Validate()
}

// newEvent fills in the parts every GitHub event shares.
func (h *Handler) newEvent(name, action, delivery string, kind event.Kind, repo repository, sender user) *event.ReviewEvent {
	return &event.ReviewEvent{
		SchemaVersion: event.SchemaVersion,
		// Deriving the id from the delivery keeps it stable across
		// redeliveries of the same webhook, which is what makes duplicate
		// suppression in the worker straightforward.
		ID: string(event.PlatformGitHub) + ":" + delivery,
		Source: event.Source{
			Platform:    event.PlatformGitHub,
			InstanceURL: instanceURL(repo.HTMLURL),
			DeliveryID:  delivery,
			EventName:   eventName(name, action),
		},
		Kind:       kind,
		Repository: repo.normalize(),
		Actor:      sender.normalize(),
	}
}

func pullRequestKind(action string, merged bool) (event.Kind, bool) {
	switch action {
	case "opened", "reopened":
		return event.KindPROpened, true
	case "synchronize":
		return event.KindPRUpdated, true
	case "ready_for_review":
		return event.KindPRReadyForReview, true
	case "review_requested":
		return event.KindPRReviewRequested, true
	case "closed":
		if merged {
			return event.KindPRMerged, true
		}
		return event.KindPRClosed, true
	default:
		// edited, labeled, assigned, converted_to_draft and the rest.
		return "", false
	}
}

// eventName records GitHub's own vocabulary ("pull_request.synchronize"),
// which is what a delivery log and the hook settings page show.
func eventName(name, action string) string {
	if action == "" {
		return name
	}
	return name + "." + action
}

// instanceURL reduces a repository URL to its origin, which distinguishes
// github.com from a GitHub Enterprise Server instance.
func instanceURL(repoURL string) string {
	if repoURL == "" {
		return ""
	}
	u, err := url.Parse(repoURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func unmarshal(body []byte, v any) error {
	if err := json.Unmarshal(body, v); err != nil {
		return &webhook.ErrMalformedPayload{Platform: event.PlatformGitHub, Err: err}
	}
	return nil
}

func firstNonZero(times ...time.Time) time.Time {
	for _, t := range times {
		if !t.IsZero() {
			return t.UTC()
		}
	}
	return time.Time{}
}

func firstNonZeroInt(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}
