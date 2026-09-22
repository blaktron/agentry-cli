//go:build windows

package engine

import (
	"os/exec"
	"time"
)

// configProcess: Windows has no process groups to signal; the context
// kill terminates the harness process itself. The returned function is the
// unix build's "the process has been waited on" call-off, which has nothing
// to call off here.
func configProcess(cmd *exec.Cmd, grace time.Duration) func() {
	cmd.WaitDelay = grace
	return func() {}
}
