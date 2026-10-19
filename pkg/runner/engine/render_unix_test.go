//go:build unix

package engine

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestARequestThatIsAPipeNeverHoldsTheEngine: the hand-off folder is the
// tools server's to write too; a request it swaps for a named pipe with no
// writer is refused at once, never waited on.
func TestARequestThatIsAPipeNeverHoldsTheEngine(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "r1.json"), 0o644); err != nil {
		t.Skipf("no fifo: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	done := make(chan error, 1)
	go func() {
		_, err := readRootFile(root, "r1.json", 4096)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a pipe was read as a request")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the engine waited on a pipe")
	}
}
