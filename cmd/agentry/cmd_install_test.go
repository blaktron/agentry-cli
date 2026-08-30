package main

import (
	"testing"
)

// TestResolveInstallPathsManagedFlag: only a directory the CLI chose is
// cleared on reinstall.
func TestResolveInstallPathsManagedFlag(t *testing.T) {
	for _, target := range validTargets {
		withDir, err := resolveInstallPaths(target, "/user/chosen", "helper")
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if withDir.managed {
			t.Errorf("%s: a --dir destination is marked managed and would be cleared", target)
		}
		def, err := resolveInstallPaths(target, "", "helper")
		if err != nil {
			t.Fatalf("%s default: %v", target, err)
		}
		if !def.managed {
			t.Errorf("%s: the default destination is not managed and stale files would linger", target)
		}
	}
	if _, err := resolveInstallPaths("nope", "", "helper"); err == nil {
		t.Error("an unknown target resolved to a path")
	}
}
