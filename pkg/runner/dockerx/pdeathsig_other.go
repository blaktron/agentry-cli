//go:build !linux

package dockerx

import "os/exec"

// dieWithParent is Linux's parent-death signal; other systems have none,
// and the run's sweep ends a service its run left.
func dieWithParent(*exec.Cmd) {}
