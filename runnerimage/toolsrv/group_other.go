//go:build !linux

package toolsrv

import "os/exec"

// inOwnGroup is the default cancellation off Linux, the debugging path,
// which runs no tools container.
func inOwnGroup(*exec.Cmd) {}

// killGroup kills the command itself off Linux.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	_ = cmd.Process.Kill()
	return nil
}
