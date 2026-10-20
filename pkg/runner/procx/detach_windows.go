//go:build windows

package procx

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detach starts the child in a process group of its own with no console, so
// the terminal's Ctrl+C and its closing do not reach it.
func detach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS
}
