package sandbox_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/sandbox"
)

// What is tested here is the worker's half: that the tree and the commands
// reach the store, that the result is waited for rather than read once, and
// that every way of not getting an answer comes back as "did not pass".

// fakeExecutor stands in for the platform. It can write the result the way a
// runner would, or not write one at all.
type fakeExecutor struct {
	// onExecute runs when the execution starts, with the store the runner
	// would read and write.
	onExecute func(ctx context.Context, location string) error
	// finished is what it claims about the execution having ended.
	finished bool
	err      error

	calls []map[string]string
}

func (e *fakeExecutor) Execute(ctx context.Context, env map[string]string, _ time.Duration) (bool, error) {
	e.calls = append(e.calls, env)
	if e.err != nil {
		return false, e.err
	}
	if e.onExecute != nil {
		if err := e.onExecute(ctx, env[sandbox.RunnerLocationEnv]); err != nil {
			return false, err
		}
	}
	return e.finished, nil
}

// writeResult is what a runner does: it puts a result where the worker looks.
func writeResult(result *sandbox.Result) func(context.Context, string) error {
	return func(ctx context.Context, location string) error {
		store, err := sandbox.OpenStore(ctx, location)
		if err != nil {
			return err
		}
		body, err := result.Marshal()
		if err != nil {
			return err
		}
		return store.Put(ctx, sandbox.ResultObject, strings.NewReader(string(body)))
	}
}

func tree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal", "queue"), 0o750); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		"go.mod":                  "module example.com/tree\n",
		"internal/queue/queue.go": "package queue\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestJobStagesTheTreeAndTheCommands(t *testing.T) {
	staging := t.TempDir()
	var location string
	executor := &fakeExecutor{
		finished: true,
		onExecute: func(ctx context.Context, loc string) error {
			location = loc
			return writeResult(&sandbox.Result{
				Steps: []sandbox.Step{{Command: []string{"go", "build", "./..."}}},
			})(ctx, loc)
		},
	}
	job := &sandbox.Job{Location: staging, Executor: executor, Poll: time.Millisecond}

	result, err := job.Verify(context.Background(), tree(t), &sandbox.Request{
		Commands: [][]string{{"go", "build", "./..."}},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !result.Passed() {
		t.Errorf("Passed() = false, want true: %+v", result)
	}

	// The location is the only thing the runner is told, and it has to be the
	// one the objects were written to.
	if len(executor.calls) != 1 {
		t.Fatalf("want one execution, got %d", len(executor.calls))
	}
	if got := executor.calls[0][sandbox.RunnerLocationEnv]; got != location {
		t.Errorf("the runner was told %q but the objects went to %q", got, location)
	}
	if len(executor.calls[0]) != 1 {
		t.Errorf("the runner was told more than where to look: %v", executor.calls[0])
	}
	// One run, one directory: two verifications must not be able to read each
	// other's objects.
	if !strings.HasPrefix(location, staging) || location == staging {
		t.Errorf("location = %q, want a subdirectory of %q", location, staging)
	}

	for _, name := range []string{sandbox.RequestObject, sandbox.WorkspaceObject} {
		if _, err := os.Stat(filepath.Join(location, name)); err != nil {
			t.Errorf("%s was not staged: %v", name, err)
		}
	}

	// The archive has to be the tree, or the runner verifies nothing.
	unpacked := t.TempDir()
	archive, err := os.Open(filepath.Join(location, sandbox.WorkspaceObject)) //nolint:gosec // a path this test built
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()
	if err := sandbox.Unpack(archive, unpacked); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if _, err := os.Stat(filepath.Join(unpacked, "internal", "queue", "queue.go")); err != nil {
		t.Errorf("the nested file did not survive the round trip: %v", err)
	}
}

// The result may land after the execution is reported done, and it may land
// after it is reported not done. Both have to be waited through.
func TestJobWaitsForAResultThatArrivesLate(t *testing.T) {
	for _, finished := range []bool{true, false} {
		t.Run(map[bool]string{true: "finished", false: "unknown"}[finished], func(t *testing.T) {
			// The location travels on a channel rather than in a variable: two
			// goroutines sharing one is a race, and -race is right about it.
			started := make(chan string, 1)
			executor := &fakeExecutor{
				finished: finished,
				onExecute: func(_ context.Context, loc string) error {
					started <- loc
					return nil
				},
			}
			job := &sandbox.Job{
				Location: t.TempDir(),
				Executor: executor,
				Poll:     time.Millisecond,
				Grace:    5 * time.Second,
			}

			// Written while the job is already waiting for it.
			go func() {
				location := <-started
				time.Sleep(20 * time.Millisecond)
				_ = writeResult(&sandbox.Result{Steps: []sandbox.Step{{Command: []string{"go", "vet"}}}})(
					context.Background(), location)
			}()

			result, err := job.Verify(context.Background(), tree(t), &sandbox.Request{
				Commands: [][]string{{"go", "vet"}},
				Timeout:  time.Second,
			})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if !result.Passed() {
				t.Errorf("Passed() = false, want true: %+v", result)
			}
		})
	}
}

// A runner that wrote nothing is not a pass. This is the case that has to fail
// closed: it is indistinguishable from "the tests would have failed".
func TestAMissingResultIsReportedAsAFailure(t *testing.T) {
	job := &sandbox.Job{
		Location: t.TempDir(),
		Executor: &fakeExecutor{finished: true},
		Poll:     time.Millisecond,
		Grace:    10 * time.Millisecond,
	}

	result, err := job.Verify(context.Background(), tree(t), &sandbox.Request{
		Commands: [][]string{{"go", "test", "./..."}},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.Passed() {
		t.Error("Passed() = true for a verification that produced nothing")
	}
	if result.Failure == "" {
		t.Error("Failure is empty; nothing would tell anybody what happened")
	}
}

// A result this build cannot read is the same answer: not a pass, and not
// something a retry improves.
func TestAnUnreadableResultIsReportedAsAFailure(t *testing.T) {
	executor := &fakeExecutor{
		finished: true,
		onExecute: func(ctx context.Context, location string) error {
			store, err := sandbox.OpenStore(ctx, location)
			if err != nil {
				return err
			}
			return store.Put(ctx, sandbox.ResultObject, strings.NewReader("{not json"))
		},
	}
	job := &sandbox.Job{Location: t.TempDir(), Executor: executor, Poll: time.Millisecond}

	result, err := job.Verify(context.Background(), tree(t), &sandbox.Request{
		Commands: [][]string{{"go", "test", "./..."}},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.Passed() || result.Failure == "" {
		t.Errorf("an unreadable result was not reported as a failure: %+v", result)
	}
}

// An execution that could not be started is kibitz's own machinery failing, so
// it is an error and not an answer about the code.
func TestAnExecutionThatCannotStartIsAnError(t *testing.T) {
	job := &sandbox.Job{
		Location: t.TempDir(),
		Executor: &fakeExecutor{err: errors.New("permission denied")},
		Poll:     time.Millisecond,
	}

	if _, err := job.Verify(context.Background(), tree(t), &sandbox.Request{
		Commands: [][]string{{"go", "test", "./..."}},
	}); err == nil {
		t.Fatal("Verify returned nil; a job that could not be started is worth retrying")
	}
}

func TestJobNeedsSomewhereToRun(t *testing.T) {
	for name, job := range map[string]*sandbox.Job{
		"no executor": {Location: "/tmp"},
		"no location": {Executor: &fakeExecutor{}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := job.Verify(context.Background(), t.TempDir(), &sandbox.Request{}); err == nil {
				t.Error("Verify returned nil for a job that cannot run")
			}
		})
	}
}

func TestACancelledContextStopsTheWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	job := &sandbox.Job{
		Location: t.TempDir(),
		Executor: &fakeExecutor{onExecute: func(context.Context, string) error {
			cancel()
			return nil
		}},
		Poll:  time.Millisecond,
		Grace: time.Hour,
	}

	if _, err := job.Verify(ctx, tree(t), &sandbox.Request{Commands: [][]string{{"go", "vet"}}}); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestLocalRunsInThisProcess(t *testing.T) {
	result, err := sandbox.Local{}.Verify(context.Background(), t.TempDir(), &sandbox.Request{
		Commands: [][]string{{"true"}},
		Timeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !result.Passed() {
		t.Errorf("Passed() = false, want true: %+v", result)
	}
}

func TestAMissingObjectIsNotFound(t *testing.T) {
	store, err := sandbox.OpenStore(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), sandbox.ResultObject); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("error = %v, want sandbox.ErrNotFound", err)
	}
}

func TestACloudRunJobNeedsAResourceName(t *testing.T) {
	for _, name := range []string{"", "kibitz-runner", "projects/p/jobs/j"} {
		if _, err := sandbox.NewCloudRun(context.Background(), sandbox.CloudRunConfig{Job: name}); err == nil {
			t.Errorf("NewCloudRun(%q) returned nil error", name)
		}
	}
}
