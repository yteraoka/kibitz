package event

import (
	"encoding/json"
	"fmt"
)

// ErrUnsupportedSchema is returned when a message carries a schema version
// this build does not understand. It is permanent: retrying cannot help, so
// the consumer routes the message to the dead letter queue instead.
type ErrUnsupportedSchema struct {
	Got  int
	Want int
}

func (e *ErrUnsupportedSchema) Error() string {
	return fmt.Sprintf("unsupported event schema version %d (this build understands %d)", e.Got, e.Want)
}

// Encode serializes an event for transport.
func Encode(e *ReviewEvent) ([]byte, error) {
	if e == nil {
		return nil, fmt.Errorf("encoding event: event is nil")
	}
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("encoding event %s: %w", e.ID, err)
	}
	return b, nil
}

// Decode parses an event, rejecting schema versions from the future before
// looking at anything else.
func Decode(b []byte) (*ReviewEvent, error) {
	var probe struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, fmt.Errorf("decoding event: %w", err)
	}
	if probe.SchemaVersion != SchemaVersion {
		return nil, &ErrUnsupportedSchema{Got: probe.SchemaVersion, Want: SchemaVersion}
	}

	var e ReviewEvent
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("decoding event: %w", err)
	}
	return &e, nil
}
