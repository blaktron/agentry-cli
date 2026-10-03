//go:build linux

package toolsrv

import (
	"os/exec"
	"syscall"
)

// runAs starts cmd as uid and gid id, with no supplementary groups.
func runAs(cmd *exec.Cmd, id uint32) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: id, Gid: id, Groups: []uint32{}}}
}
