//go:build unix

package opencode

import (
	"os/exec"
	"syscall"
	"time"
)

// waitDelay is how long the process is given to die and release its pipes
// after it was asked to stop, before Go closes them and gives up on it.
const waitDelay = 5 * time.Second

// setupProcessGroup puts the agent and everything it spawns into its own
// process group, and makes cancellation signal the whole group.
//
// This matters because the agent is not one process: it starts MCP servers as
// children. Killing only the agent leaves those children running, holding the
// pipes kibitz is reading, so a job that timed out would keep the worker
// blocked for as long as its orphans live.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// A negative pid signals the group. SIGTERM first so the agent can
		// flush what it has; WaitDelay bounds how long that politeness costs.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}
