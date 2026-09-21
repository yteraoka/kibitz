package event_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
)

func valid() *event.ReviewEvent {
	return &event.ReviewEvent{
		SchemaVersion: event.SchemaVersion,
		ID:            "github:7f3c",
		OccurredAt:    time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
		Source: event.Source{
			Platform:   event.PlatformGitHub,
			DeliveryID: "7f3c",
			EventName:  "pull_request.opened",
		},
		Kind:        event.KindPROpened,
		Repository:  event.Repository{Owner: "yteraoka", Name: "kibitz", FullName: "yteraoka/kibitz"},
		PullRequest: &event.PullRequest{Number: 42, Title: "Add the SQS subscriber"},
		Actor:       event.Actor{Login: "yteraoka"},
	}
}

func TestValidate(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	tests := []struct {
		name   string
		broken func(*event.ReviewEvent)
		want   string
	}{
		{"no id", func(e *event.ReviewEvent) { e.ID = "" }, "id is empty"},
		{"no platform", func(e *event.ReviewEvent) { e.Source.Platform = "" }, "source.platform"},
		{"no delivery id", func(e *event.ReviewEvent) { e.Source.DeliveryID = "" }, "source.delivery_id"},
		{"unknown kind", func(e *event.ReviewEvent) { e.Kind = "pr.exploded" }, "unknown"},
		{"no repository", func(e *event.ReviewEvent) { e.Repository.FullName = "" }, "repository.full_name"},
		{"no timestamp", func(e *event.ReviewEvent) { e.OccurredAt = time.Time{} }, "occurred_at"},
		{"no pull request", func(e *event.ReviewEvent) { e.PullRequest = nil }, "pull_request is missing"},
		{"bad number", func(e *event.ReviewEvent) { e.PullRequest.Number = 0 }, "number"},
		{"wrong schema", func(e *event.ReviewEvent) { e.SchemaVersion = 99 }, "schema_version"},
		{
			name:   "comment kind without a comment",
			broken: func(e *event.ReviewEvent) { e.Kind = event.KindCommentCreated },
			want:   "comment is missing",
		},
		{
			name: "command kind without a command",
			broken: func(e *event.ReviewEvent) {
				e.Kind = event.KindCommand
				e.Comment = &event.Comment{ID: "1", Body: "@kibitz review"}
			},
			want: "command is missing",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := valid()
			tc.broken(ev)

			err := ev.Validate()
			if err == nil {
				t.Fatal("Validate accepted an invalid event")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// Validate collects every problem so a normalization bug is fixed in one pass.
func TestValidateReportsEveryProblem(t *testing.T) {
	ev := valid()
	ev.ID = ""
	ev.Repository.FullName = ""
	ev.PullRequest = nil

	err := ev.Validate()
	if err == nil {
		t.Fatal("Validate accepted an invalid event")
	}
	for _, want := range []string{"id is empty", "repository.full_name", "pull_request is missing"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	ev := valid()
	ev.Comment = &event.Comment{ID: "1", Body: "@kibitz review", Path: "main.go", Line: 12}
	ev.Command = &event.Command{Name: "review", Args: []string{"--focus", "security"}}
	ev.Kind = event.KindCommand

	encoded, err := event.Encode(ev)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := event.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if got.ID != ev.ID || got.Kind != ev.Kind {
		t.Errorf("identity lost: %s %s", got.ID, got.Kind)
	}
	if !got.OccurredAt.Equal(ev.OccurredAt) {
		t.Errorf("OccurredAt = %v, want %v", got.OccurredAt, ev.OccurredAt)
	}
	if got.Comment == nil || got.Comment.Line != 12 {
		t.Errorf("comment lost: %+v", got.Comment)
	}
	if got.Command == nil || len(got.Command.Args) != 2 {
		t.Errorf("command lost: %+v", got.Command)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("decoded event is invalid: %v", err)
	}
}

// A message from a future version must not be retried forever; the consumer
// needs to recognize it as permanent and dead letter it.
func TestDecodeRejectsFutureSchema(t *testing.T) {
	_, err := event.Decode([]byte(`{"schema_version": 99, "id": "x"}`))

	var unsupported *event.ErrUnsupportedSchema
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want ErrUnsupportedSchema", err)
	}
	if unsupported.Got != 99 || unsupported.Want != event.SchemaVersion {
		t.Errorf("err = %+v", unsupported)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := event.Decode([]byte("not json")); err == nil {
		t.Fatal("Decode accepted garbage")
	}
}

func TestKey(t *testing.T) {
	ev := valid()
	if got, want := ev.Key(), "github/yteraoka/kibitz/42"; got != want {
		t.Errorf("Key() = %q, want %q", got, want)
	}

	ev.PullRequest = nil
	if got, want := ev.Key(), "github/yteraoka/kibitz"; got != want {
		t.Errorf("Key() without a pull request = %q, want %q", got, want)
	}
}

func TestKindKnown(t *testing.T) {
	for _, k := range []event.Kind{
		event.KindPROpened, event.KindPRUpdated, event.KindPRReadyForReview,
		event.KindPRReviewRequested, event.KindPRClosed, event.KindPRMerged,
		event.KindCommentCreated, event.KindCommand,
	} {
		if !k.Known() {
			t.Errorf("%s should be known", k)
		}
	}
	if event.Kind("pr.exploded").Known() {
		t.Error("an unknown kind reported itself as known")
	}
}
