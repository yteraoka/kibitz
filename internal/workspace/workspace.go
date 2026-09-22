// Package workspace prepares a disposable checkout of a pull request for the
// agent to read. One job gets one directory, which is removed afterwards.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yteraoka/kibitz/internal/forge"
)

// Local refs the fetch writes to. Naming them keeps the checkout independent
// of whatever branch names the pull request happens to use.
const (
	headRef = "refs/kibitz/head"
	baseRef = "refs/kibitz/base"
)

// Config configures preparation.
type Config struct {
	// Root is the directory job workspaces are created under.
	Root string
	// Depth bounds how much history is fetched. Zero means a full clone, which
	// is rarely what a review needs.
	Depth int
	// Timeout bounds each git invocation.
	Timeout time.Duration
}

// Spec describes what to fetch.
type Spec struct {
	CloneURL string
	// HeadRef is the ref that points at the pull request's head, from
	// [forge.HeadRef]. Fetching by ref rather than by branch is what makes a
	// pull request from a fork reachable at all.
	HeadRef string
	// BaseBranch is the branch the pull request targets. It is fetched too, so
	// the agent can compare against it.
	BaseBranch string
	Credential forge.CloneCredential
}

// Workspace is a prepared checkout.
type Workspace struct {
	// Dir is the working directory handed to the agent.
	Dir string
	// HeadSHA is the commit that was checked out. The review is reported
	// against this commit, not against whatever the event claimed, because a
	// push may have landed in between.
	HeadSHA string
	// BaseSHA is the tip of the target branch that was fetched.
	BaseSHA string

	logger *slog.Logger
}

// Prepare creates a workspace and fetches the pull request into it.
func Prepare(ctx context.Context, cfg Config, spec Spec, logger *slog.Logger) (ws *Workspace, err error) {
	if spec.CloneURL == "" {
		return nil, errors.New("workspace: clone url is empty")
	}
	if spec.HeadRef == "" {
		return nil, errors.New("workspace: head ref is empty")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Minute
	}
	if cfg.Root == "" {
		cfg.Root = os.TempDir()
	}
	if err := os.MkdirAll(cfg.Root, 0o700); err != nil {
		return nil, fmt.Errorf("workspace: creating root: %w", err)
	}

	dir, err := os.MkdirTemp(cfg.Root, "job-")
	if err != nil {
		return nil, fmt.Errorf("workspace: creating directory: %w", err)
	}
	// Anything that fails from here on leaves no directory behind: a worker
	// that fails often would otherwise fill its disk with half-clones.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()

	w := &Workspace{Dir: dir, logger: logger}
	runner := gitRunner{dir: dir, timeout: cfg.Timeout, credential: spec.Credential}

	if err := runner.run(ctx, "init", "--quiet", "--initial-branch=main"); err != nil {
		return nil, err
	}
	if err := runner.run(ctx, "remote", "add", "origin", spec.CloneURL); err != nil {
		return nil, err
	}

	fetch := []string{"fetch", "--quiet", "--no-tags"}
	if cfg.Depth > 0 {
		fetch = append(fetch, "--depth="+strconv.Itoa(cfg.Depth))
	}
	fetch = append(fetch, "origin", spec.HeadRef+":"+headRef)
	if err := runner.run(ctx, fetch...); err != nil {
		return nil, err
	}
	if err := runner.run(ctx, "checkout", "--quiet", headRef); err != nil {
		return nil, err
	}

	if w.HeadSHA, err = runner.output(ctx, "rev-parse", "HEAD"); err != nil {
		return nil, err
	}

	// The base branch is best effort: a review of the diff still works without
	// it, and it is missing whenever the branch was deleted mid-flight.
	if spec.BaseBranch != "" {
		baseFetch := []string{"fetch", "--quiet", "--no-tags"}
		if cfg.Depth > 0 {
			baseFetch = append(baseFetch, "--depth="+strconv.Itoa(cfg.Depth))
		}
		baseFetch = append(baseFetch, "origin", "refs/heads/"+spec.BaseBranch+":"+baseRef)
		if err := runner.run(ctx, baseFetch...); err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "could not fetch the base branch",
				slog.String("branch", spec.BaseBranch),
				slog.String("error", err.Error()),
			)
		} else if sha, err := runner.output(ctx, "rev-parse", baseRef); err == nil {
			w.BaseSHA = sha
		}
	}

	return w, nil
}

// Close removes the workspace.
func (w *Workspace) Close() error {
	if w == nil || w.Dir == "" {
		return nil
	}
	if err := os.RemoveAll(w.Dir); err != nil {
		return fmt.Errorf("workspace: removing %s: %w", w.Dir, err)
	}
	return nil
}

// Size reports the checkout's size in bytes, which is how a job notices that a
// repository is too large before handing it to the agent.
func (w *Workspace) Size() (int64, error) {
	var total int64
	err := filepath.WalkDir(w.Dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			// A file that vanished mid-walk is not a reason to fail the job.
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// gitRunner invokes git with the credential supplied out of band.
type gitRunner struct {
	dir        string
	timeout    time.Duration
	credential forge.CloneCredential
}

func (g gitRunner) command(ctx context.Context, args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)

	// gosec's G204 fires on the variable arguments. The binary is the literal
	// "git", every caller in this package passes a fixed subcommand first, and
	// the arguments are handed to exec rather than to a shell.
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: fixed binary, no shell
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(),
		// Never stop for credentials: a prompt in a worker is a hung job.
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GCM_INTERACTIVE=never",
	)
	if g.credential.AuthHeader != "" {
		// The credential travels in the environment rather than in the command
		// line, so it does not show up in the process list, and in a header
		// rather than in the remote URL, so git cannot echo it back in an
		// error message.
		key, value, _ := strings.Cut(g.credential.AuthHeader, ": ")
		cmd.Env = append(cmd.Env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0="+key+": "+value,
		)
	}
	return cmd, cancel
}

func (g gitRunner) run(ctx context.Context, args ...string) error {
	cmd, cancel := g.command(ctx, args...)
	defer cancel()

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (g gitRunner) output(ctx context.Context, args ...string) (string, error) {
	cmd, cancel := g.command(ctx, args...)
	defer cancel()

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}
