package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Verify runs every command in dir and reports what happened.
//
// It is the whole of what the runner does, and it is deliberately small. There
// is no shell, no environment kibitz assembled, and no interpretation of the
// output: a command's exit status is the answer, and the text is kept only so
// that a person can read why.
//
// The timeout bounds the run rather than each command, because the thing worth
// bounding is how long a verification may take. A step the deadline lands on
// is marked, and the ones after it do not run.
func Verify(ctx context.Context, dir string, req *Request) *Result {
	ctx, cancel := context.WithTimeout(ctx, req.timeout())
	defer cancel()

	result := &Result{Steps: make([]Step, 0, len(req.Commands))}
	for _, argv := range req.Commands {
		if len(argv) == 0 {
			continue
		}
		step := runOne(ctx, dir, argv)
		result.Steps = append(result.Steps, step)
		if !step.Passed() {
			// The first failure is the answer. Running the tests after a
			// build that did not compile produces noise, not information.
			break
		}
	}
	return result
}

func runOne(ctx context.Context, dir string, argv []string) Step {
	started := time.Now()
	step := Step{Command: argv, ExitCode: -1}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv comes from the repository's own settings, and there is no shell
	cmd.Dir = dir
	// The environment is built rather than inherited. The runner holds no
	// credentials worth withholding, but a build that depends on something
	// kibitz happened to have set would pass here and fail in CI.
	cmd.Env = buildEnv()

	var out boundedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	step.Duration = time.Since(started)
	step.Output = out.String()
	step.Truncated = out.truncated

	switch {
	case err == nil:
		step.ExitCode = 0
	case ctx.Err() != nil:
		step.TimedOut = true
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			step.ExitCode = exitErr.ExitCode()
		} else {
			// Could not be started at all: a command the image does not have.
			step.Output = strings.TrimSpace(step.Output + "\n" + err.Error())
		}
	}
	return step
}

// buildEnv is the environment a build gets. PATH, HOME and the language
// toolchains' cache directories, and nothing else.
func buildEnv() []string {
	keep := []string{
		"PATH", "HOME", "LANG", "LC_ALL", "TZ",
		"GOPATH", "GOMODCACHE", "GOCACHE", "GOFLAGS", "GOPROXY", "GOTOOLCHAIN",
		"npm_config_cache", "PNPM_HOME", "CARGO_HOME", "RUSTUP_HOME",
		"PIP_CACHE_DIR", "UV_CACHE_DIR", "MAVEN_OPTS", "GRADLE_USER_HOME",
	}

	env := make([]string, 0, len(keep)+1)
	for _, name := range keep {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	// Tests that behave differently under CI usually behave better: less
	// interactivity, no colour, no prompts.
	env = append(env, "CI=true")
	return env
}

// boundedBuffer keeps the last MaxOutputBytes of what it is given.
//
// The tail rather than the head: a failing test says what failed at the end,
// and a build that printed ten thousand lines of progress said nothing in the
// first hundred.
type boundedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if _, err := b.buf.Write(p); err != nil {
		return 0, err
	}
	if b.buf.Len() > MaxOutputBytes {
		b.truncated = true
		// Drop from the front, keeping the limit.
		excess := b.buf.Len() - MaxOutputBytes
		b.buf.Next(excess)
	}
	return n, nil
}

func (b *boundedBuffer) String() string { return strings.TrimSpace(b.buf.String()) }
