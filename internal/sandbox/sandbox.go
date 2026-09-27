// Package sandbox runs a repository's own build and tests somewhere that
// holds none of kibitz's credentials.
//
// Implement mode writes code, and code has to be built and tested before it
// is worth anybody's review. Doing that in the worker would mean running the
// repository's test suite in a process that holds the GitHub App's private
// key, the GitLab and Azure DevOps tokens, and credentials for Firestore,
// Pub/Sub and Vertex AI. A test file is a place anybody who can open a pull
// request may put code.
//
// So the build runs somewhere else: a Cloud Run job whose service account has
// no roles at all beyond reading one bucket. A hostile test there can reach
// the metadata server and mint a token, and the token is good for nothing.
// The host is already separated — Cloud Run runs containers under gVisor —
// which is the half of the problem this package does not have to solve.
//
// What crosses the boundary is a tar of the workspace and a list of commands,
// and what comes back is whether each one passed. Nothing else: the runner
// never learns which repository it is building, who asked, or what the change
// was for.
//
// See docs/security.md and ADR-0019.
package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Limits on what one verification may be.
const (
	// MaxOutputBytes is how much of one command's output is kept. A test suite
	// that prints a megabyte of progress has already said what went wrong in
	// the last few lines, and the whole thing has to fit in a pull request
	// comment.
	MaxOutputBytes = 16 << 10
	// MaxCommands bounds the list. A repository that needs twenty commands to
	// know whether it builds is telling kibitz something, and it is not "run
	// all of them".
	MaxCommands = 10
	// DefaultTimeout bounds one whole verification, not one command.
	DefaultTimeout = 15 * time.Minute
)

// Request is what the runner is given.
//
// It carries no repository name, no issue, and no actor. The runner has no use
// for them and no business knowing them: everything it needs is the tree and
// the commands.
type Request struct {
	// Commands are run in order, from the root of the extracted tree. Each is
	// an argv, already split: the runner starts processes directly and never
	// through a shell.
	Commands [][]string `json:"commands"`
	// Timeout bounds the whole run. Zero means [DefaultTimeout].
	Timeout time.Duration `json:"timeout,omitempty"`
}

// Step is one command's outcome.
type Step struct {
	Command []string `json:"command"`
	// ExitCode is the process's status, or -1 when it could not be started or
	// was killed by the timeout.
	ExitCode int `json:"exit_code"`
	// Output is stdout and stderr interleaved, tail-truncated to
	// [MaxOutputBytes].
	Output string `json:"output"`
	// Truncated says output was cut, so that a reader does not mistake the
	// tail for the whole thing.
	Truncated bool          `json:"truncated,omitempty"`
	Duration  time.Duration `json:"duration"`
	// TimedOut marks the step the timeout landed on.
	TimedOut bool `json:"timed_out,omitempty"`
}

// Passed reports whether this step succeeded.
func (s Step) Passed() bool { return s.ExitCode == 0 && !s.TimedOut }

// Result is what comes back.
type Result struct {
	Steps []Step `json:"steps"`
	// Failure is set when the run could not be carried out at all — a tar
	// that would not extract, a request that named no commands. It is
	// different from a command that failed, which is an answer.
	Failure string `json:"failure,omitempty"`
}

// Passed reports whether every command succeeded and the run itself did not
// fail. A result with no steps has not passed: an empty verification is not a
// verification.
func (r *Result) Passed() bool {
	if r == nil || r.Failure != "" || len(r.Steps) == 0 {
		return false
	}
	for _, step := range r.Steps {
		if !step.Passed() {
			return false
		}
	}
	return true
}

// FirstFailure returns the step that failed, for the report.
func (r *Result) FirstFailure() (Step, bool) {
	if r == nil {
		return Step{}, false
	}
	for _, step := range r.Steps {
		if !step.Passed() {
			return step, true
		}
	}
	return Step{}, false
}

// ParseCommands turns the strings a repository configured into argv.
//
// There is no shell, so there is nothing for a shell metacharacter to mean.
// Rather than accept one and ignore it — which would make "go build && go
// test" silently run "go build" with three odd arguments — the ones that only
// make sense to a shell are refused, and the repository is told to write two
// commands instead.
func ParseCommands(commands []string) ([][]string, error) {
	if len(commands) > MaxCommands {
		return nil, fmt.Errorf("sandbox: %d commands is more than the %d allowed", len(commands), MaxCommands)
	}

	out := make([][]string, 0, len(commands))
	for _, command := range commands {
		command = strings.TrimSpace(command)
		if command == "" {
			continue
		}
		if bad := shellOnly(command); bad != "" {
			return nil, fmt.Errorf("sandbox: %q contains %q, which needs a shell; "+
				"commands are run directly, so write them as separate entries", command, bad)
		}
		out = append(out, strings.Fields(command))
	}
	if len(out) == 0 {
		return nil, errors.New("sandbox: no commands to run")
	}
	return out, nil
}

// shellOnly returns the first character that would only mean something to a
// shell, or "".
func shellOnly(command string) string {
	for _, token := range []string{"&&", "||", ";", "|", ">", "<", "`", "$(", "$", "\n"} {
		if strings.Contains(command, token) {
			return token
		}
	}
	return ""
}

// Marshal and Unmarshal keep the wire format in one place: the worker writes
// it and a different binary reads it, and the two are deployed separately.
func (r Request) Marshal() ([]byte, error) { return json.Marshal(r) }

// UnmarshalRequest reads a request.
func UnmarshalRequest(data []byte) (*Request, error) {
	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("sandbox: the request is not valid JSON: %w", err)
	}
	if len(req.Commands) == 0 {
		return nil, errors.New("sandbox: the request names no commands")
	}
	if len(req.Commands) > MaxCommands {
		return nil, fmt.Errorf("sandbox: the request names %d commands, more than the %d allowed",
			len(req.Commands), MaxCommands)
	}
	return &req, nil
}

func (r Result) Marshal() ([]byte, error) { return json.Marshal(r) }

// UnmarshalResult reads a result.
func UnmarshalResult(data []byte) (*Result, error) {
	var res Result
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("sandbox: the result is not valid JSON: %w", err)
	}
	return &res, nil
}

func (r Request) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return DefaultTimeout
}
