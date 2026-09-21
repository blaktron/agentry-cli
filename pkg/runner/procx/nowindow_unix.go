//go:build !windows

package procx

import "os/exec"

// noWindow: there is no console to withhold outside Windows.
func noWindow(*exec.Cmd) {}
