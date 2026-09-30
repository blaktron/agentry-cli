package store

import (
	"os"
	"testing"
	"time"
)

// TestAHeldWorkingCopyIsKeptFreshForOtherProcesses (agentry-cli#507):
// another process on the same store (the desktop beside the CLI) knows a
// live working copy only by its modification time, so the process that
// holds one touches it as it goes; one given back is touched no more.
func TestAHeldWorkingCopyIsKeptFreshForOtherProcesses(t *testing.T) {
	prev := scratchTouchEvery
	t.Cleanup(func() { scratchTouchEvery = prev })
	scratchTouchEvery = 5 * time.Millisecond

	stale := func(dir string) bool {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		return time.Since(fi.ModTime()) > staleAfter
	}
	age := func(dir string) {
		old := time.Now().Add(-2 * staleAfter)
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
	}
	scratch, done, err := New(t.TempDir(), Options{}).Scratch()
	if err != nil {
		t.Fatal(err)
	}
	age(scratch)
	for deadline := time.Now().Add(5 * time.Second); stale(scratch); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a held working copy was never touched: another process's sweep would remove it after an hour")
		}
	}
	done()
	done() // giving it back twice is harmless
	if _, err := os.Stat(scratch); err == nil {
		t.Fatal("a scratch given back is still there")
	}
	// A directory of that name afterwards is nobody's: it is not touched.
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	age(scratch)
	time.Sleep(20 * scratchTouchEvery)
	if !stale(scratch) {
		t.Fatal("a scratch given back is still being touched")
	}
}
