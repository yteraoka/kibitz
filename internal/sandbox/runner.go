package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

// RunnerLocationEnv is how the runner is told which run it is carrying out.
// It is the only thing the job is given (see deploy/terraform/gcp/sandbox.tf).
const RunnerLocationEnv = "KIBITZ_RUNNER_LOCATION"

// How long the worker waits for a result, beyond the run's own timeout.
const (
	// DefaultPoll is how often the result is looked for.
	DefaultPoll = 5 * time.Second
	// DefaultGrace is how long a finished execution is still given to have
	// written its result. It covers starting the container, unpacking the
	// tree and uploading the answer, none of which the run's own timeout
	// bounds.
	DefaultGrace = 3 * time.Minute
)

// Runner carries out one verification.
//
// The two return values mean different things and the caller has to keep them
// apart. An error is kibitz's own machinery failing — the tree could not be
// uploaded, the job could not be started — and a retry may fix it. A [Result]
// is an answer about the code: it may say the tests failed, or, with Failure
// set, that the verification could not be carried out at all. Neither of those
// is worth retrying, and neither of them is a pass.
type Runner interface {
	Verify(ctx context.Context, dir string, req *Request) (*Result, error)
}

// Local verifies in the worker's own process.
//
// It exists because the alternative needs a Cloud Run job, and a deployment
// running under docker compose has none. It is not a sandbox and is not
// described as one: [Verify] withholds the worker's environment, but a process
// that can start a process can ask the metadata server for the worker's own
// service account token. Running other people's test suites this way means
// trusting every repository kibitz is installed on with everything kibitz can
// reach (ADR-0019).
type Local struct{}

// Verify implements [Runner].
func (Local) Verify(ctx context.Context, dir string, req *Request) (*Result, error) {
	return Verify(ctx, dir, req), nil
}

// Executor starts one execution of the runner and, where the platform says so,
// waits for it to finish.
type Executor interface {
	// Execute starts one execution with env added to the container's
	// environment, and a timeout for the execution itself.
	//
	// finished reports whether the execution is known to have ended. False is
	// not a failure: it means the platform did not say, so the caller keeps
	// waiting for the result rather than concluding there is none.
	Execute(ctx context.Context, env map[string]string, timeout time.Duration) (finished bool, err error)
}

// Job verifies a tree in an execution that holds none of kibitz's credentials.
//
// The worker writes the tree and the commands to a shared location, starts the
// execution, and reads the result back. It is three objects and one process,
// and the reason it is arranged this way is that the process at the other end
// runs code somebody else wrote (ADR-0019).
type Job struct {
	// Location is where runs are staged: "gs://bucket/prefix", or a directory
	// on disk. Each run gets its own subdirectory, so two verifications can
	// never read each other's objects.
	Location string
	// Executor starts the execution.
	Executor Executor
	Logger   *slog.Logger
	// Poll is how often the result is looked for. Zero means [DefaultPoll].
	Poll time.Duration
	// Grace is how long a finished execution is still given to produce its
	// result. Zero means [DefaultGrace].
	Grace time.Duration
	// Stores opens one run's location. Nil means [OpenStore]; it is a field so
	// that this can be tested without a bucket.
	Stores func(ctx context.Context, location string) (Store, error)
}

// Verify implements [Runner].
func (j *Job) Verify(ctx context.Context, dir string, req *Request) (*Result, error) {
	if j.Executor == nil {
		return nil, errors.New("sandbox: no executor configured")
	}
	if strings.TrimSpace(j.Location) == "" {
		return nil, errors.New("sandbox: no location configured")
	}

	id, err := runID()
	if err != nil {
		return nil, err
	}
	location := strings.TrimSuffix(j.Location, "/") + "/" + id

	store, err := j.stores()(ctx, location)
	if err != nil {
		return nil, err
	}

	body, err := req.Marshal()
	if err != nil {
		return nil, fmt.Errorf("sandbox: encoding the request: %w", err)
	}
	if err := store.Put(ctx, RequestObject, bytes.NewReader(body)); err != nil {
		return nil, err
	}
	if err := j.upload(ctx, store, dir); err != nil {
		return nil, err
	}

	timeout := req.timeout()
	j.log(ctx, slog.LevelInfo, "starting a verification",
		slog.String("location", location),
		slog.Int("commands", len(req.Commands)),
		slog.Duration("timeout", timeout),
	)

	finished, err := j.Executor.Execute(ctx, map[string]string{RunnerLocationEnv: location}, timeout)
	if err != nil {
		return nil, fmt.Errorf("sandbox: starting the verification: %w", err)
	}

	return j.await(ctx, store, timeout, finished)
}

// upload streams the tree into the store.
//
// It is streamed rather than built in memory or written to a temporary file:
// a checkout is measured in hundreds of megabytes, the worker holds several
// jobs at once, and the archive is written once and read once.
func (j *Job) upload(ctx context.Context, store Store, dir string) error {
	pr, pw := io.Pipe()
	packed := make(chan error, 1)
	go func() {
		err := Pack(dir, pw)
		// Closing with the error is what makes the reader see it, so a failed
		// pack becomes a failed upload rather than a truncated archive that
		// uploads cleanly.
		_ = pw.CloseWithError(err)
		packed <- err
	}()

	putErr := store.Put(ctx, WorkspaceObject, pr)
	// Unblocks Pack if the upload gave up while it was still writing.
	_ = pr.Close()
	packErr := <-packed

	// The pack error is reported first when there is one: an upload that
	// failed because the thing being uploaded failed should say so.
	if packErr != nil {
		return fmt.Errorf("sandbox: packing the workspace: %w", packErr)
	}
	return putErr
}

// await waits for the runner's result.
//
// The result is polled for rather than read once, and that is the point. The
// job API's operation may complete when the execution is created or when it
// ends, depending on the platform and on what this code was told; polling
// gives the same answer either way, and gives it immediately in the second
// case. A verification that reported "no result" because it looked too early
// would refuse every change kibitz wrote, which fails closed and is therefore
// the kind of bug that stays in for a while.
func (j *Job) await(ctx context.Context, store Store, timeout time.Duration, finished bool) (*Result, error) {
	// A finished execution has had its time; an unfinished one may still be
	// using it.
	wait := j.grace()
	if !finished {
		wait += timeout
	}
	deadline := time.Now().Add(wait)

	for {
		data, err := read(ctx, store, ResultObject)
		switch {
		case err == nil:
			result, err := UnmarshalResult(data)
			if err != nil {
				// The runner wrote something this build cannot read. Not an
				// error to retry: the same bytes parse the same way.
				return &Result{Failure: "検証結果を読めませんでした: " + err.Error()}, nil
			}
			return result, nil
		case !errors.Is(err, ErrNotFound):
			return nil, err
		}

		if !time.Now().Before(deadline) {
			j.log(ctx, slog.LevelWarn, "the verification produced no result",
				slog.Duration("waited", wait),
				slog.Bool("execution_finished", finished),
			)
			return &Result{Failure: fmt.Sprintf(
				"検証が結果を書きませんでした (%s 待機)。ランナーのログを確認してください。",
				wait.Round(time.Second))}, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(j.poll()):
		}
	}
}

// read fetches one object whole. The objects are a request, a result and an
// archive; only the first two go through here, and both are small.
func read(ctx context.Context, store Store, name string) ([]byte, error) {
	body, err := store.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	return io.ReadAll(io.LimitReader(body, maxResultBytes))
}

// maxResultBytes bounds the result. The runner caps each step's output at
// [MaxOutputBytes] and the step count at [MaxCommands], so anything past this
// did not come from a runner this build agrees with.
const maxResultBytes = MaxOutputBytes*MaxCommands + 64<<10

// runID names one run's directory.
//
// It is random rather than derived from the repository or the issue: the
// directory name is visible to the runner, and the runner is told nothing
// about what it is verifying on purpose (see [Request]).
func runID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("sandbox: generating a run id: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

func (j *Job) stores() func(context.Context, string) (Store, error) {
	if j.Stores != nil {
		return j.Stores
	}
	return OpenStore
}

func (j *Job) poll() time.Duration {
	if j.Poll > 0 {
		return j.Poll
	}
	return DefaultPoll
}

func (j *Job) grace() time.Duration {
	if j.Grace > 0 {
		return j.Grace
	}
	return DefaultGrace
}

func (j *Job) log(ctx context.Context, level slog.Level, msg string, attrs ...slog.Attr) {
	if j.Logger == nil {
		return
	}
	j.Logger.LogAttrs(ctx, level, msg, attrs...)
}
