package pubsub_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	gcp "cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/queue"
	kpubsub "github.com/yteraoka/kibitz/internal/queue/pubsub"
)

const projectID = "kibitz-test"

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// harness is a publisher and subscriber pair wired to a fake Pub/Sub. By
// default that is the in-process pstest server, so these tests run anywhere;
// setting PUBSUB_EMULATOR_HOST points them at the real emulator instead, which
// is the only way to exercise ordering faithfully.
type harness struct {
	cfg  kpubsub.Config
	opts []option.ClientOption
}

func newHarness(ctx context.Context, t *testing.T) harness {
	t.Helper()

	var opts []option.ClientOption
	if os.Getenv("PUBSUB_EMULATOR_HOST") == "" {
		srv := pstest.NewServer()
		t.Cleanup(func() { _ = srv.Close() })

		conn, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatalf("dialing the fake server: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		opts = []option.ClientOption{option.WithGRPCConn(conn), option.WithoutAuthentication()}
	}

	name := fmt.Sprintf("kibitz-%d", time.Now().UnixNano())
	admin, err := gcp.NewClient(ctx, projectID, opts...)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	topic := fmt.Sprintf("projects/%s/topics/%s", projectID, name)
	if _, err := admin.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatalf("creating topic: %v", err)
	}
	if _, err := admin.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name:                  fmt.Sprintf("projects/%s/subscriptions/%s", projectID, name),
		Topic:                 topic,
		EnableMessageOrdering: true,
		AckDeadlineSeconds:    10,
	}); err != nil {
		t.Fatalf("creating subscription: %v", err)
	}

	return harness{
		cfg: kpubsub.Config{
			ProjectID:      projectID,
			Topic:          name,
			Subscription:   name,
			MaxExtension:   time.Minute,
			MaxOutstanding: 1,
		},
		opts: opts,
	}
}

func (h harness) publisher(ctx context.Context, t *testing.T) *kpubsub.Publisher {
	t.Helper()
	pub, err := kpubsub.NewPublisher(ctx, h.cfg, h.opts...)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	t.Cleanup(func() { _ = pub.Close() })
	return pub
}

func (h harness) subscriber(ctx context.Context, t *testing.T) *kpubsub.Subscriber {
	t.Helper()
	sub, err := kpubsub.NewSubscriber(ctx, h.cfg, discardLogger(), h.opts...)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	return sub
}

// receive starts a subscriber and reports each handled event on the returned
// channel. The handler's verdict comes from fn.
func receive(ctx context.Context, t *testing.T, sub *kpubsub.Subscriber, fn func(*queue.Message) error) <-chan *queue.Message {
	t.Helper()

	out := make(chan *queue.Message, 8)
	go func() {
		err := sub.Receive(ctx, func(_ context.Context, m *queue.Message) error {
			out <- m
			return fn(m)
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Receive: %v", err)
		}
	}()
	return out
}

func testEvent(id string, pr int) *event.ReviewEvent {
	return &event.ReviewEvent{
		SchemaVersion: event.SchemaVersion,
		ID:            id,
		OccurredAt:    time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
		Source:        event.Source{Platform: event.PlatformGitHub, DeliveryID: id},
		Kind:          event.KindPROpened,
		Repository:    event.Repository{Owner: "yteraoka", Name: "kibitz", FullName: "yteraoka/kibitz"},
		PullRequest:   &event.PullRequest{Number: pr, Title: "Add the SQS subscriber"},
		Actor:         event.Actor{Login: "yteraoka"},
	}
}

func TestConfigValidation(t *testing.T) {
	ctx := context.Background()

	if _, err := kpubsub.NewPublisher(ctx, kpubsub.Config{Topic: "t"}); err == nil {
		t.Error("NewPublisher accepted a config with no project")
	}
	if _, err := kpubsub.NewPublisher(ctx, kpubsub.Config{ProjectID: "p"}); err == nil {
		t.Error("NewPublisher accepted a config with no topic")
	}
	if _, err := kpubsub.NewSubscriber(ctx, kpubsub.Config{ProjectID: "p", Topic: "t"}, discardLogger()); err == nil {
		t.Error("NewSubscriber accepted a config with no subscription")
	}
}

func TestRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h := newHarness(ctx, t)
	pub, sub := h.publisher(ctx, t), h.subscriber(ctx, t)

	if _, err := pub.Publish(ctx, testEvent("a", 42)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	received := receive(ctx, t, sub, func(*queue.Message) error { return nil })
	select {
	case m := <-received:
		if m.Event.ID != "a" {
			t.Errorf("event id = %q, want a", m.Event.ID)
		}
		if m.ID == "" {
			t.Error("message id is empty")
		}
		// Attributes travel with the message so that filters, metrics and
		// tracing do not have to decode the body.
		if got := m.Attributes[queue.AttrRepository]; got != "yteraoka/kibitz" {
			t.Errorf("repository attribute = %q", got)
		}
		if got := m.Attributes[queue.AttrPullRequest]; got != "42" {
			t.Errorf("pull request attribute = %q", got)
		}
		if got := m.Attributes[queue.AttrKind]; got != string(event.KindPROpened) {
			t.Errorf("kind attribute = %q", got)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the message")
	}
}

func TestFailedJobIsRedelivered(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	h := newHarness(ctx, t)
	pub, sub := h.publisher(ctx, t), h.subscriber(ctx, t)

	if _, err := pub.Publish(ctx, testEvent("a", 42)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	attempts := 0
	received := receive(ctx, t, sub, func(*queue.Message) error {
		attempts++
		if attempts < 2 {
			return errors.New("transient failure")
		}
		return nil
	})

	for i := 1; i <= 2; i++ {
		select {
		case m := <-received:
			if m.Event.ID != "a" {
				t.Fatalf("received %q, want the redelivered event", m.Event.ID)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for delivery %d", i)
		}
	}
}

// A message this build cannot decode must not be retried forever; it is left
// to the subscription's dead letter policy instead of looping.
func TestUndecodableMessageIsNotRetriedForever(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	h := newHarness(ctx, t)
	sub := h.subscriber(ctx, t)

	client, err := gcp.NewClient(ctx, projectID, h.opts...)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = client.Close() }()

	raw := client.Publisher(h.cfg.Topic)
	raw.EnableMessageOrdering = true
	defer raw.Stop()

	// A message from a newer producer, and one that is not an event at all.
	for _, data := range [][]byte{[]byte(`{"schema_version": 99}`), []byte("not json")} {
		if _, err := raw.Publish(ctx, &gcp.Message{Data: data, OrderingKey: "k"}).Get(ctx); err != nil {
			t.Fatalf("publishing raw message: %v", err)
		}
	}

	handled := make(chan *queue.Message, 4)
	recvCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	go func() {
		_ = sub.Receive(recvCtx, func(_ context.Context, m *queue.Message) error {
			handled <- m
			return nil
		})
	}()

	select {
	case m := <-handled:
		t.Fatalf("an undecodable message reached the handler: %+v", m)
	case <-recvCtx.Done():
	}
}

func TestPublishRejectsInvalidEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	h := newHarness(ctx, t)
	pub := h.publisher(ctx, t)

	ev := testEvent("a", 42)
	ev.Repository.FullName = ""
	if _, err := pub.Publish(ctx, ev); err == nil {
		t.Error("Publish accepted an invalid event")
	}
}

// Ordering is only faithful on the real emulator: pstest makes no promise
// about delivery order, so this runs where `make up-pubsub` has been used.
func TestOrderingIsPerPullRequest(t *testing.T) {
	if os.Getenv("PUBSUB_EMULATOR_HOST") == "" {
		t.Skip("ordering is only guaranteed by the emulator; set PUBSUB_EMULATOR_HOST")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h := newHarness(ctx, t)
	pub, sub := h.publisher(ctx, t), h.subscriber(ctx, t)

	for _, id := range []string{"first", "second", "third"} {
		if _, err := pub.Publish(ctx, testEvent(id, 42)); err != nil {
			t.Fatalf("Publish(%s): %v", id, err)
		}
	}

	received := receive(ctx, t, sub, func(*queue.Message) error { return nil })
	for _, want := range []string{"first", "second", "third"} {
		select {
		case m := <-received:
			if m.Event.ID != want {
				t.Fatalf("received %q, want %q: events for one pull request must stay in order", m.Event.ID, want)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}
