//go:build !windows

package engine

import (
	"os/exec"
	"syscall"
	"time"
)

// configProcess puts the harness in its own process group so a budget or
// timeout kill reaches every child it spawned, gives it SIGTERM first and
// SIGKILL after WaitDelay.
func configProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second
}
