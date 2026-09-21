// Package webhook turns platform-specific webhooks into normalized events.
// Each platform implements [Handler]; everything downstream sees only
// [event.ReviewEvent].
package webhook

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"github.com/yteraoka/kibitz/internal/event"
)

// Verification failures. They are separated because they mean different
// things to the caller: a missing signature is a misconfigured hook, a bad one
// may be an attack, and no secrets at all is kibitz's own misconfiguration.
var (
	ErrMissingSignature = errors.New("webhook: signature is missing")
	ErrInvalidSignature = errors.New("webhook: signature does not match")
	ErrNoSecrets        = errors.New("webhook: no secrets configured for this platform")
)

// ErrMalformedPayload reports a payload that could not be parsed. It is
// permanent: redelivering the same bytes cannot help.
type ErrMalformedPayload struct {
	Platform event.Platform
	Err      error
}

func (e *ErrMalformedPayload) Error() string {
	return fmt.Sprintf("webhook: malformed %s payload: %v", e.Platform, e.Err)
}

func (e *ErrMalformedPayload) Unwrap() error { return e.Err }

// Handler verifies and normalizes the webhooks of one platform.
type Handler interface {
	// Platform identifies which forge this handler serves.
	Platform() event.Platform

	// Verify authenticates the delivery. It is given the raw body and must be
	// called before the payload is parsed: re-serializing parsed JSON does not
	// reproduce the bytes the signature was computed over.
	Verify(r *http.Request, body []byte) error

	// Normalize converts the payload into a review event. It returns
	// (nil, nil) for deliveries that are well formed but not interesting,
	// such as a ping or a label being added.
	Normalize(r *http.Request, body []byte) (*event.ReviewEvent, error)
}

// DeliveryDescriber reports the forge's own identifiers for a delivery: the
// id it can be found and redelivered by, and the name of the event in the
// forge's vocabulary. A handler implements it so that deliveries which never
// became an event — an unsupported action, a payload that did not parse, a
// signature that did not match — are still traceable to a line in the forge's
// delivery log.
//
// It reads headers only: a payload that could not be trusted or parsed is not
// a source of identifiers.
type DeliveryDescriber interface {
	Delivery(r *http.Request) (id, name string)
}

// Fingerprint identifies a secret without revealing it, so that "is the value
// in the running container the one I pasted into the forge" can be answered
// from a log line. It is the first bytes of the SHA-256 of the secret, which
// the operator can reproduce anywhere:
//
//	printf '%s' 'YOUR_SECRET' | sha256sum | cut -c1-12
//
// An empty secret has no fingerprint; a secret weak enough to be guessed from
// one was already weak enough to be guessed from a delivery's signature.
func Fingerprint(secret string) string {
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])[:12]
}

// Fingerprints identifies a set of secrets.
func Fingerprints(secrets []string) []string {
	out := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if fp := Fingerprint(s); fp != "" {
			out = append(out, fp)
		}
	}
	return out
}
