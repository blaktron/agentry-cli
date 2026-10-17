//go:build linux

package toolsrv

import (
	"errors"
	"os/exec"
	"syscall"
)

// inOwnGroup starts cmd in a process group of its own and has a cancelled
// context kill the whole group, so a call ends every process its command
// started, a background one (`sh -c 'loop &'`) included.
func inOwnGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error { return killGroup(cmd) }
}

// killGroup kills every process left in cmd's group. A group already empty
// is not an error.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
