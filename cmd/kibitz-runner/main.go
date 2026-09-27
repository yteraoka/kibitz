// Command kibitz-runner builds and tests a tree that somebody else's code
// wrote, somewhere kibitz's credentials are not.
//
// It is the other half of implement mode. The worker writes a workspace and a
// list of commands to one prefix of one bucket; this binary reads them, runs
// the commands, and writes back what happened. It never learns which
// repository it is building, who asked for it, or why — there is nothing it
// could do with any of that, and a process running a stranger's test suite
// should be told as little as possible.
//
// Its service account holds no roles beyond reading and writing that prefix.
// A test that reaches the metadata server gets a token that is good for
// nothing, which is the property that makes running the test safe at all.
//
// See docs/security.md and ADR-0019.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yteraoka/kibitz/internal/sandbox"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// A job that is being shut down should still write its result: a
	// verification whose answer was lost looks exactly like one that hung.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, logger); err != nil {
		logger.LogAttrs(ctx, slog.LevelError, "the verification could not be carried out",
			slog.String("error", err.Error()),
		)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger) error {
	location := os.Getenv("KIBITZ_RUNNER_LOCATION")
	if location == "" {
		return errors.New("KIBITZ_RUNNER_LOCATION is not set; it names the prefix holding this run's objects")
	}

	store, err := sandbox.OpenStore(ctx, location)
	if err != nil {
		return err
	}

	workDir, err := os.MkdirTemp("", "kibitz-verify-")
	if err != nil {
		return fmt.Errorf("creating the work directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	// Anything that goes wrong from here is reported as a result rather than
	// as an exit status. The worker is waiting for a result; a job that failed
	// without writing one leaves it waiting for the timeout.
	result := verify(ctx, logger, store, workDir)

	data, err := result.Marshal()
	if err != nil {
		return fmt.Errorf("encoding the result: %w", err)
	}
	if err := store.Put(context.WithoutCancel(ctx), sandbox.ResultObject, bytesReader(data)); err != nil {
		return fmt.Errorf("writing the result: %w", err)
	}

	logger.LogAttrs(ctx, slog.LevelInfo, "verification finished",
		slog.Bool("passed", result.Passed()),
		slog.Int("steps", len(result.Steps)),
		slog.String("failure", result.Failure),
	)
	// The job exits 0 whether the tests passed or not: whether the code is
	// good is the result's business, and a failed execution would make the
	// worker think the runner itself broke.
	return nil
}

// verify does the work, turning any problem into a result the worker can read.
func verify(ctx context.Context, logger *slog.Logger, store sandbox.Store, workDir string) *sandbox.Result {
	requestBody, err := readObject(ctx, store, sandbox.RequestObject)
	if err != nil {
		return &sandbox.Result{Failure: err.Error()}
	}
	request, err := sandbox.UnmarshalRequest(requestBody)
	if err != nil {
		return &sandbox.Result{Failure: err.Error()}
	}

	archive, err := store.Get(ctx, sandbox.WorkspaceObject)
	if err != nil {
		return &sandbox.Result{Failure: err.Error()}
	}
	defer func() { _ = archive.Close() }()

	if err := sandbox.Unpack(archive, workDir); err != nil {
		return &sandbox.Result{Failure: err.Error()}
	}

	logger.LogAttrs(ctx, slog.LevelInfo, "running the repository's own checks",
		slog.Int("commands", len(request.Commands)),
		slog.Duration("timeout", timeoutOf(request)),
	)
	return sandbox.Verify(ctx, workDir, request)
}

func bytesReader(data []byte) io.Reader { return bytes.NewReader(data) }

// readAtMost reads up to limit bytes and reports anything beyond it as an
// error rather than silently truncating a document that would then fail to
// parse for a reason nobody could see.
func readAtMost(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("larger than the %d byte limit", limit)
	}
	return data, nil
}

func timeoutOf(req *sandbox.Request) time.Duration {
	if req.Timeout > 0 {
		return req.Timeout
	}
	return sandbox.DefaultTimeout
}

// readObject reads one object whole. The three of them are a JSON document, a
// tar, and a JSON document; only the first is read into memory.
func readObject(ctx context.Context, store sandbox.Store, name string) ([]byte, error) {
	r, err := store.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()

	// Bounded: the request is a short document, and a reader that keeps going
	// is not one.
	const maxRequestBytes = 64 << 10
	data, err := readAtMost(r, maxRequestBytes)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	return data, nil
}
