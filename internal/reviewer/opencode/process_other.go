//go:build !unix

package opencode

import (
	"os/exec"
	"time"
)

// setupProcessGroup falls back to the default behaviour elsewhere: the worker
// runs on Linux, so process-group handling is only implemented there.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.WaitDelay = 5 * time.Second
}
