// Package gitlab verifies and normalizes GitLab webhooks. It serves
// gitlab.com and self-managed instances alike; the instance is taken from the
// delivery rather than configured.
package gitlab

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/webhook"
)

// GitLab webhook headers.
const (
	HeaderEvent     = "X-Gitlab-Event"
	HeaderEventUUID = "X-Gitlab-Event-UUID"
	HeaderInstance  = "X-Gitlab-Instance"
	// The name of the header, not a credential.
	HeaderToken = "X-Gitlab-Token" //nolint:gosec // a header name

	// The Standard Webhooks headers, which is what a signing token signs.
	HeaderWebhookID        = "webhook-id"
	HeaderWebhookTimestamp = "webhook-timestamp"
	HeaderWebhookSignature = "webhook-signature"
	// HeaderIdempotencyKey carries the same value as webhook-id, and is what
	// older instances send.
	HeaderIdempotencyKey = "Idempotency-Key"
)

// signaturePrefix is the version every signature carries.
const signaturePrefix = "v1,"

// signingTokenPrefix is what GitLab puts in front of a signing token. The key
// is the base64 that follows it.
const signingTokenPrefix = "whsec_"

// maxSkew bounds how old a signed delivery may be. The timestamp is part of
// the signed message, so without a bound a captured delivery could be replayed
// forever.
const maxSkew = 5 * time.Minute

// Handler implements [webhook.Handler] for GitLab.
type Handler struct {
	// tokens are the plain shared secrets, compared in constant time. They
	// authenticate the sender and nothing else: they do not cover the body.
	tokens []string
	// signingKeys verify the HMAC-SHA256 signature GitLab sends when a
	// signing token is configured (GitLab 19.0 and later). That one does
	// cover the body, which is the only way to know a payload arrived intact.
	signingKeys [][]byte

	now func() time.Time
}

// Option customizes a handler.
type Option func(*Handler)

// WithClock replaces the clock, which bounds how old a signed delivery may be.
func WithClock(now func() time.Time) Option {
	return func(h *Handler) { h.now = now }
}

// New creates a handler.
//
// Both kinds of credential are accepted, and each may be a list so that either
// can be rotated without dropping deliveries. A signing token is preferred
// where the instance is new enough to send one: a shared token says who sent
// the delivery, a signature says what was sent.
func New(tokens, signingTokens []string, opts ...Option) *Handler {
	h := &Handler{now: time.Now}
	for _, t := range tokens {
		if t = strings.TrimSpace(t); t != "" {
			h.tokens = append(h.tokens, t)
		}
	}
	for _, t := range signingTokens {
		if key, err := decodeSigningToken(t); err == nil {
			h.signingKeys = append(h.signingKeys, key)
		}
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// decodeSigningToken turns "whsec_<base64>" into the key GitLab signs with.
func decodeSigningToken(token string) ([]byte, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("signing token is empty")
	}

	raw := strings.TrimPrefix(token, signingTokenPrefix)
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("signing token is not base64: %w", err)
	}
	if len(key) == 0 {
		return nil, fmt.Errorf("signing token decoded to nothing")
	}
	return key, nil
}

// Platform implements [webhook.Handler].
func (h *Handler) Platform() event.Platform { return event.PlatformGitLab }

// Delivery implements [webhook.DeliveryDescriber].
func (h *Handler) Delivery(r *http.Request) (id, name string) {
	return deliveryID(r), r.Header.Get(HeaderEvent)
}

// Verify implements [webhook.Handler].
//
// A signature is checked whenever one is configured and one was sent; the
// shared token is the fallback for an instance too old to sign. Either is
// enough on its own, because an instance sends whichever it was configured
// with and kibitz cannot tell it what to do.
func (h *Handler) Verify(r *http.Request, body []byte) error {
	if len(h.tokens) == 0 && len(h.signingKeys) == 0 {
		return webhook.ErrNoSecrets
	}

	signature := r.Header.Get(HeaderWebhookSignature)
	if len(h.signingKeys) > 0 && signature != "" {
		return h.verifySignature(r, body, signature)
	}

	token := r.Header.Get(HeaderToken)
	if token == "" {
		if len(h.tokens) == 0 {
			// Configured only for signatures, and none arrived.
			return fmt.Errorf("%w: no %s header", webhook.ErrMissingSignature, HeaderWebhookSignature)
		}
		return fmt.Errorf("%w: no %s header", webhook.ErrMissingSignature, HeaderToken)
	}
	for _, want := range h.tokens {
		if subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1 {
			return nil
		}
	}
	return fmt.Errorf("%w (kibitz holds %d token(s), fingerprint %s; see docs/deployment.md)",
		webhook.ErrInvalidSignature, len(h.tokens), strings.Join(webhook.Fingerprints(h.tokens), " "))
}

// verifySignature implements the Standard Webhooks scheme GitLab follows: the
// HMAC covers "<id>.<timestamp>.<body>", so a replay with a different body or
// an old timestamp does not verify.
func (h *Handler) verifySignature(r *http.Request, body []byte, header string) error {
	id := r.Header.Get(HeaderWebhookID)
	if id == "" {
		id = r.Header.Get(HeaderIdempotencyKey)
	}
	timestamp := r.Header.Get(HeaderWebhookTimestamp)
	if id == "" || timestamp == "" {
		return fmt.Errorf("%w: a signed delivery needs %s and %s",
			webhook.ErrMissingSignature, HeaderWebhookID, HeaderWebhookTimestamp)
	}
	if err := h.checkTimestamp(timestamp); err != nil {
		return err
	}

	message := []byte(id + "." + timestamp + "." + string(body))
	received := strings.Fields(header)

	for _, key := range h.signingKeys {
		mac := hmac.New(sha256.New, key)
		mac.Write(message)
		want := signaturePrefix + base64.StdEncoding.EncodeToString(mac.Sum(nil))

		for _, got := range received {
			// GitLab may send several signatures; any match is enough.
			if subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1 {
				return nil
			}
		}
	}
	return fmt.Errorf("%w (kibitz holds %d signing token(s))", webhook.ErrInvalidSignature, len(h.signingKeys))
}

// checkTimestamp rejects a delivery whose signed timestamp is too far from
// now, in either direction.
func (h *Handler) checkTimestamp(value string) error {
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: %s is not a unix timestamp", webhook.ErrInvalidSignature, HeaderWebhookTimestamp)
	}

	skew := h.now().Sub(time.Unix(seconds, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > maxSkew {
		return fmt.Errorf("%w: the delivery is %s away from now", webhook.ErrInvalidSignature, skew.Round(time.Second))
	}
	return nil
}

// deliveryID identifies the delivery for duplicate suppression.
//
// The idempotency key is the right one: GitLab keeps it across retries of the
// same delivery, which is exactly what must not be reviewed twice. The event
// UUID is a fallback, and a poor one — recursive webhooks share it — so it is
// only used when nothing better arrived.
func deliveryID(r *http.Request) string {
	for _, header := range []string{HeaderWebhookID, HeaderIdempotencyKey, HeaderEventUUID} {
		if v := r.Header.Get(header); v != "" {
			return v
		}
	}
	return ""
}

// instanceURL is where the delivery came from. GitLab says so in a header,
// which is more reliable than deriving it from a project URL.
func instanceURL(r *http.Request, projectURL string) string {
	if v := strings.TrimSpace(r.Header.Get(HeaderInstance)); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	if projectURL == "" {
		return ""
	}
	u, err := url.Parse(projectURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
