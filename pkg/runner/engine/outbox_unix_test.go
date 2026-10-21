//go:build !windows

package engine

import (
	"syscall"
	"testing"
)

// makePipe makes a named pipe at path.
func makePipe(t *testing.T, path string) bool {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return true
}
