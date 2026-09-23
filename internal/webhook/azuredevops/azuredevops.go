// Package azuredevops verifies and normalizes Azure DevOps service hooks. It
// serves Azure DevOps Services and Azure DevOps Server alike; the instance is
// taken from the delivery rather than configured.
//
// This platform is the asymmetric one. Azure DevOps does not sign its
// deliveries: the web hook consumer offers HTTP basic authentication and
// optional custom headers, and nothing else. A shared credential says who
// sent a delivery; it says nothing about what the delivery contains, and a
// credential that leaks anywhere lets anyone say anything. So the payload is
// treated as a claim about what happened rather than as the record of it: the
// worker re-reads the pull request from the API before reviewing it, which it
// does on every platform but which is load-bearing only here.
//
// See docs/security.md and docs/event-schema.md.
package azuredevops

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/webhook"
)

// Handler implements [webhook.Handler] for Azure DevOps.
type Handler struct {
	// user is the basic auth user the subscription is configured with.
	user string
	// passwords are accepted alternatives, so one can be rotated without
	// dropping deliveries.
	passwords []string
	// headerName and headerValues are an optional second credential: Azure
	// DevOps can be told to send a fixed header, and requiring one narrows
	// what a leaked basic auth credential alone is good for.
	headerName   string
	headerValues []string

	now func() time.Time
}

// Option customizes a handler.
type Option func(*Handler)

// WithClock replaces the clock, which dates a delivery whose own timestamp is
// missing or unparseable.
func WithClock(now func() time.Time) Option {
	return func(h *Handler) { h.now = now }
}

// WithHeader requires a fixed header in addition to basic authentication.
// Several values are accepted so that one can be rotated.
func WithHeader(name string, values []string) Option {
	return func(h *Handler) {
		h.headerName = http.CanonicalHeaderKey(strings.TrimSpace(name))
		for _, v := range values {
			if v = strings.TrimSpace(v); v != "" {
				h.headerValues = append(h.headerValues, v)
			}
		}
	}
}

// New creates a handler.
func New(user string, passwords []string, opts ...Option) *Handler {
	h := &Handler{user: strings.TrimSpace(user), now: time.Now}
	for _, p := range passwords {
		if p = strings.TrimSpace(p); p != "" {
			h.passwords = append(h.passwords, p)
		}
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Platform implements [webhook.Handler].
func (h *Handler) Platform() event.Platform { return event.PlatformAzureDevOps }

// Verify implements [webhook.Handler].
//
// Both credentials are compared in constant time, and both must match when
// both are configured. There is no signature to fall back to, so a delivery
// that carries neither is refused rather than trusted.
func (h *Handler) Verify(r *http.Request, _ []byte) error {
	if len(h.passwords) == 0 && len(h.headerValues) == 0 {
		return webhook.ErrNoSecrets
	}

	if len(h.passwords) > 0 {
		user, password, ok := r.BasicAuth()
		if !ok {
			return fmt.Errorf("%w: no basic authentication", webhook.ErrMissingSignature)
		}
		if subtle.ConstantTimeCompare([]byte(user), []byte(h.user)) != 1 {
			// The user is not a secret, so naming the mismatch costs nothing
			// and saves an afternoon.
			return fmt.Errorf("%w: basic auth user is %q, kibitz expects %q",
				webhook.ErrInvalidSignature, user, h.user)
		}
		if !matchAny(password, h.passwords) {
			return fmt.Errorf("%w (kibitz holds %d password(s), fingerprint %s; see docs/deployment.md)",
				webhook.ErrInvalidSignature, len(h.passwords), strings.Join(webhook.Fingerprints(h.passwords), " "))
		}
	}

	if len(h.headerValues) > 0 {
		got := r.Header.Get(h.headerName)
		if got == "" {
			return fmt.Errorf("%w: no %s header", webhook.ErrMissingSignature, h.headerName)
		}
		if !matchAny(got, h.headerValues) {
			return fmt.Errorf("%w (the %s header does not match any of kibitz's %d value(s))",
				webhook.ErrInvalidSignature, h.headerName, len(h.headerValues))
		}
	}
	return nil
}

// matchAny compares in constant time against every candidate, and does not
// stop at the first match: returning early would leak which one it was
// through timing.
func matchAny(got string, want []string) bool {
	sum := sha256.Sum256([]byte(got))
	matched := 0
	for _, w := range want {
		wsum := sha256.Sum256([]byte(w))
		matched |= subtle.ConstantTimeCompare(sum[:], wsum[:])
	}
	return matched == 1
}

// DeliveryFromBody implements [webhook.BodyDeliveryDescriber].
//
// Azure DevOps puts the delivery id and the event name in the payload rather
// than in headers, so there is nothing to report from a request alone. The
// receiver hands over the body only after [Handler.Verify] has accepted it,
// which is what makes reading identifiers out of it reasonable: an
// unauthenticated body is still not a source of anything.
func (h *Handler) DeliveryFromBody(body []byte) (id, name string) {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return "", ""
	}
	return p.ID, p.EventType
}
