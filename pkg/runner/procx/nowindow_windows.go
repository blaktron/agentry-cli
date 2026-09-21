//go:build windows

package procx

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// noWindow adds CREATE_NO_WINDOW to the process creation flags, keeping
// whatever else the caller put in SysProcAttr (a CmdLine, for one).
func noWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
