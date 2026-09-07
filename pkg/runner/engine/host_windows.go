//go:build windows

package engine

import (
	"os/exec"
	"time"
)

// configProcess: Windows has no process groups to signal; the context
// kill terminates the harness process itself.
func configProcess(cmd *exec.Cmd) {
	cmd.WaitDelay = 10 * time.Second
}
