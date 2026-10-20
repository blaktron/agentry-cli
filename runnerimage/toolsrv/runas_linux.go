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

// runAsIDs starts cmd as uid and gid, with no supplementary groups.
func runAsIDs(cmd *exec.Cmd, uid, gid uint32) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: []uint32{}}}
}
