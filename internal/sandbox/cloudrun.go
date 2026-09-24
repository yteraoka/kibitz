package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/option"
	run "google.golang.org/api/run/v2"
)

// CloudRunConfig configures [CloudRun].
type CloudRunConfig struct {
	// Job is the resource to execute: projects/P/locations/L/jobs/J. A project
	// number works where the id does.
	Job string
	// Container names the container the overrides apply to. A job with one
	// container that was declared without a name needs none, which is what
	// kibitz's own terraform produces.
	Container string
	// Poll is how long one wait call blocks for. Zero means
	// [DefaultExecutionPoll].
	Poll   time.Duration
	Logger *slog.Logger
}

// DefaultExecutionPoll is how long one wait on the execution blocks for. It is
// a long poll: the call returns as soon as the operation is done, and this is
// only the ceiling on how long it waits before being asked again.
const DefaultExecutionPoll = 30 * time.Second

// CloudRun starts executions of a Cloud Run job.
//
// It is the half of the arrangement that needs a platform, and it is kept to
// one type for that reason: everything about what a verification is lives in
// [Job], which can be tested, and everything about how an execution is started
// lives here, which cannot.
type CloudRun struct {
	svc       *run.Service
	job       string
	container string
	poll      time.Duration
	logger    *slog.Logger
}

// NewCloudRun builds an executor for one job.
func NewCloudRun(ctx context.Context, cfg CloudRunConfig, opts ...option.ClientOption) (*CloudRun, error) {
	name := strings.Trim(strings.TrimSpace(cfg.Job), "/")
	if name == "" {
		return nil, errors.New("sandbox: no Cloud Run job named")
	}
	// Checked here rather than at the first execution: a job name that is not
	// a resource path is a configuration mistake, and a deployment should
	// learn about it at startup and not on somebody's issue.
	if parts := strings.Split(name, "/"); len(parts) != 6 ||
		parts[0] != "projects" || parts[2] != "locations" || parts[4] != "jobs" {
		return nil, fmt.Errorf("sandbox: %q is not a job resource name (projects/P/locations/L/jobs/J)", cfg.Job)
	}

	svc, err := run.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("sandbox: connecting to Cloud Run: %w", err)
	}
	return &CloudRun{
		svc:       svc,
		job:       name,
		container: strings.TrimSpace(cfg.Container),
		poll:      cfg.Poll,
		logger:    cfg.Logger,
	}, nil
}

// Execute implements [Executor].
func (c *CloudRun) Execute(ctx context.Context, env map[string]string, timeout time.Duration) (bool, error) {
	op, err := c.svc.Projects.Locations.Jobs.Run(c.job, &run.GoogleCloudRunV2RunJobRequest{
		Overrides: &run.GoogleCloudRunV2Overrides{
			ContainerOverrides: []*run.GoogleCloudRunV2ContainerOverride{{
				Name: c.container,
				Env:  envVars(env),
			}},
			// One task. A verification is one tree and one list of commands,
			// and a second copy of it would be the same answer twice.
			TaskCount: 1,
			// The job's own timeout is replaced rather than relied on: the
			// request says how long this verification may take, and the
			// platform has to agree or it will kill the container before the
			// runner can write down what happened.
			Timeout: strconv.Itoa(int(timeout.Seconds())) + "s",
		},
	}).Context(ctx).Do()
	if err != nil {
		return false, err
	}

	return c.wait(ctx, op, timeout)
}

// wait follows the operation until it is done, and reports whether the
// execution itself has finished.
//
// What "done" means for this operation is not something this code asserts. It
// reads the execution out of the operation and looks at its completion time,
// which is the platform's own statement; when there is none to read it says
// so by returning false, and the caller waits for the result instead of
// deciding there is none.
func (c *CloudRun) wait(ctx context.Context, op *run.GoogleLongrunningOperation, timeout time.Duration) (bool, error) {
	// The execution's own timeout, plus room for the platform to start the
	// container and to mark the execution complete afterwards.
	deadline := time.Now().Add(timeout + DefaultGrace)

	for !op.Done {
		if !time.Now().Before(deadline) {
			// Not an error: the execution may well be running, and the result
			// is what settles it.
			c.log(ctx, slog.LevelWarn, "gave up waiting for the verification's execution",
				slog.String("operation", op.Name),
			)
			return false, nil
		}

		waited, err := c.svc.Projects.Locations.Operations.Wait(op.Name,
			&run.GoogleLongrunningWaitOperationRequest{
				Timeout: strconv.Itoa(int(c.pollInterval().Seconds())) + "s",
			}).Context(ctx).Do()
		if err != nil {
			return false, err
		}
		op = waited
	}

	if op.Error != nil {
		return true, fmt.Errorf("the execution failed: %s (code %d)", op.Error.Message, op.Error.Code)
	}

	execution := executionOf(op)
	if execution == nil {
		return false, nil
	}
	c.log(ctx, slog.LevelInfo, "the verification's execution finished",
		slog.String("execution", execution.Name),
		slog.String("logs", execution.LogUri),
		slog.Int64("succeeded", execution.SucceededCount),
		slog.Int64("failed", execution.FailedCount),
		slog.Int64("cancelled", execution.CancelledCount),
	)

	// The runner exits 0 whether the code passed or not, so a failed task is
	// the runner itself having broken — a missing image, an out-of-memory
	// kill. It is reported as an error because it is kibitz's problem and not
	// the repository's.
	if execution.FailedCount > 0 || execution.CancelledCount > 0 {
		return true, fmt.Errorf("the verification's task did not complete (failed %d, cancelled %d); logs: %s",
			execution.FailedCount, execution.CancelledCount, execution.LogUri)
	}
	return execution.CompletionTime != "", nil
}

// executionOf reads the execution out of the operation, from whichever field
// carries it. Neither is guaranteed to be there, and a nil answer is a valid
// one: it means this code does not know, which the caller handles.
func executionOf(op *run.GoogleLongrunningOperation) *run.GoogleCloudRunV2Execution {
	for _, raw := range []json.RawMessage{json.RawMessage(op.Response), json.RawMessage(op.Metadata)} {
		if len(raw) == 0 {
			continue
		}
		var execution run.GoogleCloudRunV2Execution
		if err := json.Unmarshal(raw, &execution); err != nil {
			continue
		}
		if execution.Name != "" {
			return &execution
		}
	}
	return nil
}

// envVars renders the overrides in a fixed order, so that two identical
// requests produce two identical calls.
func envVars(env map[string]string) []*run.GoogleCloudRunV2EnvVar {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]*run.GoogleCloudRunV2EnvVar, 0, len(names))
	for _, name := range names {
		out = append(out, &run.GoogleCloudRunV2EnvVar{Name: name, Value: env[name]})
	}
	return out
}

func (c *CloudRun) pollInterval() time.Duration {
	if c.poll > 0 {
		return c.poll
	}
	return DefaultExecutionPoll
}

func (c *CloudRun) log(ctx context.Context, level slog.Level, msg string, attrs ...slog.Attr) {
	if c.logger == nil {
		return
	}
	c.logger.LogAttrs(ctx, level, msg, attrs...)
}
