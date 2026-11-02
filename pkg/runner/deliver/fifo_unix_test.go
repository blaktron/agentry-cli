//go:build !windows

package deliver

import (
	"syscall"
	"testing"
)

// mkfifo makes a named pipe at path and reports that it did.
func mkfifo(t *testing.T, path string) bool {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Fatal(err)
	}
	return true
}
