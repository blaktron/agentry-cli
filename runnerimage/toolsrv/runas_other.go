//go:build !linux

package toolsrv

import "os/exec"

// runAs is never reached off Linux: chooseConfinement confines nothing
// there (the debugging path).
func runAs(*exec.Cmd, uint32) {}

// runAsIDs is never reached off Linux, as runAs is not.
func runAsIDs(*exec.Cmd, uint32, uint32) {}
