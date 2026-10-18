//go:build unix

package destination

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestFromFileDoesNotWaitOnAFIFO: a file that is a FIFO with no writer is
// refused at once, not opened.
func TestFromFileDoesNotWaitOnAFIFO(t *testing.T) {
	p := filepath.Join(t.TempDir(), "destinations.json")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skip("no FIFO here:", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := FromFile(p, envOf(nil))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO was read")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("FromFile waits on a FIFO")
	}
}
