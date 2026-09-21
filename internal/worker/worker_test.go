package worker_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/config"
	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/queue/memory"
	"github.com/yteraoka/kibitz/internal/worker"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func testEvent(id string) *event.ReviewEvent {
	return &event.ReviewEvent{
		SchemaVersion: event.SchemaVersion,
		ID:            id,
		OccurredAt:    time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
		Source:        event.Source{Platform: event.PlatformGitHub, DeliveryID: id},
		Kind:          event.KindPROpened,
		Repository:    event.Repository{Owner: "yteraoka", Name: "kibitz", FullName: "yteraoka/kibitz"},
		PullRequest:   &event.PullRequest{Number: 42},
		Actor:         event.Actor{Login: "yteraoka"},
	}
}

func TestRunHandlesEvents(t *testing.T) {
	q := memory.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := q.Publish(ctx, testEvent("a")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	handled := make(chan string, 1)
	w := worker.New(q, worker.HandlerFunc(func(_ context.Context, j *worker.Job) error {
		handled <- j.Event.ID
		return nil
	}), discardLogger(), &config.Worker{JobTimeout: time.Second})

	go func() { _ = w.Run(ctx) }()

	select {
	case got := <-handled:
		if got != "a" {
			t.Errorf("handled %q, want a", got)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the job")
	}
}

func TestFailedJobIsRedelivered(t *testing.T) {
	q := memory.New()
	q.MaxDeliveries = 3
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := q.Publish(ctx, testEvent("a")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	attempts := make(chan int, 4)
	count := 0
	w := worker.New(q, worker.HandlerFunc(func(_ context.Context, j *worker.Job) error {
		count++
		attempts <- count
		if j.Deliveries != count {
			t.Errorf("Deliveries = %d on attempt %d", j.Deliveries, count)
		}
		if count < 2 {
			return errors.New("transient failure")
		}
		return nil
	}), discardLogger(), &config.Worker{JobTimeout: time.Second})

	go func() { _ = w.Run(ctx) }()

	for want := 1; want <= 2; want++ {
		select {
		case got := <-attempts:
			if got != want {
				t.Fatalf("attempt = %d, want %d", got, want)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for attempt %d", want)
		}
	}
}

// A job that runs past its timeout must be cut off, not left holding a lease
// the queue will hand to a second worker.
func TestJobTimeout(t *testing.T) {
	q := memory.New()
	q.MaxDeliveries = 1
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := q.Publish(ctx, testEvent("a")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	deadline := make(chan error, 1)
	w := worker.New(q, worker.HandlerFunc(func(ctx context.Context, _ *worker.Job) error {
		<-ctx.Done()
		deadline <- ctx.Err()
		return ctx.Err()
	}), discardLogger(), &config.Worker{JobTimeout: 50 * time.Millisecond})

	go func() { _ = w.Run(ctx) }()

	select {
	case err := <-deadline:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want DeadlineExceeded", err)
		}
	case <-ctx.Done():
		t.Fatal("the job was not cut off at its timeout")
	}
}

// originRepo builds a repository whose pull request head is published the way
// a forge publishes it, so a review job can fetch it.
func originRepo(t *testing.T) (dir, headSHA, baseSHA string) {
	t.Helper()

	dir = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=kibitz", "GIT_AUTHOR_EMAIL=kibitz@example.com",
			"GIT_COMMITTER_NAME=kibitz", "GIT_COMMITTER_EMAIL=kibitz@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	run("init", "--quiet", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(dir, "queue.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("writing file: %v", err)
	}
	run("add", ".")
	run("commit", "--quiet", "-m", "initial")
	baseSHA = run("rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(dir, "queue.go"), []byte("package main\n\nfunc Receive() {}\n"), 0o600); err != nil {
		t.Fatalf("writing file: %v", err)
	}
	run("add", ".")
	run("commit", "--quiet", "-m", "add Receive")
	headSHA = run("rev-parse", "HEAD")

	run("update-ref", "refs/pull/42/head", headSHA)
	run("reset", "--quiet", "--hard", baseSHA)

	return dir, headSHA, baseSHA
}
