//go:build !windows

package procx

import (
	"os/exec"
	"syscall"
)

// detach starts the child in a session of its own, so a terminal's hangup
// or its Ctrl+C does not reach it.
func detach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}
