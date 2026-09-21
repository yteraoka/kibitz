// Package webhook turns platform-specific webhooks into normalized events.
// Each platform implements [Handler]; everything downstream sees only
// [event.ReviewEvent].
package webhook

import (
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
