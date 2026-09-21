package queue_test

import (
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/queue"
)

func testEvent() *event.ReviewEvent {
	return &event.ReviewEvent{
		SchemaVersion: event.SchemaVersion,
		ID:            "github:7f3c",
		OccurredAt:    time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
		Source:        event.Source{Platform: event.PlatformGitHub, DeliveryID: "7f3c"},
		Kind:          event.KindPROpened,
		Repository:    event.Repository{Owner: "yteraoka", Name: "kibitz", FullName: "yteraoka/kibitz"},
		PullRequest:   &event.PullRequest{Number: 42},
		Actor:         event.Actor{Login: "yteraoka"},
	}
}

func TestAttributes(t *testing.T) {
	ev := testEvent()
	ev.Trace = &event.Trace{TraceParent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}

	attrs := queue.Attributes(ev)
	want := map[string]string{
		queue.AttrPlatform:    "github",
		queue.AttrKind:        "pr.opened",
		queue.AttrRepository:  "yteraoka/kibitz",
		queue.AttrPullRequest: "42",
		queue.AttrDeliveryID:  "7f3c",
		queue.AttrTraceParent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
	}
	for k, v := range want {
		if attrs[k] != v {
			t.Errorf("attribute %s = %q, want %q", k, attrs[k], v)
		}
	}
}

func TestAttributesWithoutTrace(t *testing.T) {
	if _, ok := queue.Attributes(testEvent())[queue.AttrTraceParent]; ok {
		t.Error("traceparent attribute set without trace context")
	}
	if queue.Attributes(nil) != nil {
		t.Error("Attributes(nil) should be nil")
	}
}

// One pull request means one ordering key, which is what serializes its work
// on both Pub/Sub and SQS.
func TestOrderingKeyIsPerPullRequest(t *testing.T) {
	a, b := testEvent(), testEvent()
	b.Source.DeliveryID = "other"
	b.Kind = event.KindPRUpdated

	if queue.OrderingKey(a) != queue.OrderingKey(b) {
		t.Errorf("ordering keys differ for the same pull request: %q vs %q",
			queue.OrderingKey(a), queue.OrderingKey(b))
	}

	c := testEvent()
	c.PullRequest.Number = 43
	if queue.OrderingKey(a) == queue.OrderingKey(c) {
		t.Error("different pull requests share an ordering key")
	}
}
