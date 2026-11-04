//go:build !linux

package toolsrv

import "testing"

// endShimGroups has nothing to end off Linux, where a declared command is
// not given a process group of its own.
func endShimGroups(*testing.T, string) {}
