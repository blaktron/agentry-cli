package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// heldOpen says whether this process still holds path open. On Linux it
// reads the process's descriptors; on Windows an open handle makes the
// file impossible to rename, which is the harm itself (agentry-cli#459).
// Elsewhere there is no cheap way to ask, and the test is skipped.
func heldOpen(t *testing.T, path string) bool {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		want, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		fds, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		for _, fd := range fds {
			if target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name())); err == nil && target == want {
				return true
			}
		}
		return false
	case "windows":
		probe := path + ".probe"
		if err := os.Rename(path, probe); err != nil {
			return true
		}
		if err := os.Rename(probe, path); err != nil {
			t.Fatal(err)
		}
		return false
	default:
		t.Skipf("no way to ask whether a file is open on %s", runtime.GOOS)
		return false
	}
}
