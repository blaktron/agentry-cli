package dockerx

import (
	"os/exec"
	"syscall"
)

// dieWithParent asks the kernel to end cmd's process when the thread that
// started it exits, which on Linux follows the CLI's own death.
func dieWithParent(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM
}
