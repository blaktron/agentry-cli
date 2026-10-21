package storefile

import (
	"path/filepath"
	"testing"
)

// TestTryLockNeverWaits: while one holder has the lock, a try reports it
// held and takes nothing; once it is let go, a try takes it.
func TestTryLockNeverWaits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, held, err := TryLock(path, "x")
	if err != nil || held || unlock == nil {
		t.Fatalf("first try: held %v, err %v", held, err)
	}
	if again, held, err := TryLock(path, "x"); err != nil || !held || again != nil {
		t.Fatalf("second try while held: held %v, err %v", held, err)
	}
	unlock()
	next, held, err := TryLock(path, "x")
	if err != nil || held {
		t.Fatalf("try after the release: held %v, err %v", held, err)
	}
	next()
}
