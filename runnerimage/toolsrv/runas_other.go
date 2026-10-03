//go:build !linux

package toolsrv

import "os/exec"

// runAs is never reached off Linux: chooseConfinement confines nothing
// there (the debugging path).
func runAs(*exec.Cmd, uint32) {}
