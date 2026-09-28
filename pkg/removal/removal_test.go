package removal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func withOp(t *testing.T, tries int, f func(string, bool) error) {
	t.Helper()
	oldOp, oldTries, oldDelay := op, Tries, Delay
	op, Tries, Delay = f, tries, time.Millisecond
	t.Cleanup(func() { op, Tries, Delay = oldOp, oldTries, oldDelay })
}

func TestRemoveRetriesAHeldFileUntilItGoes(t *testing.T) {
	calls := 0
	withOp(t, 20, func(string, bool) error {
		calls++
		if calls < 4 {
			return errors.New("The process cannot access the file because it is being used by another process.")
		}
		return nil
	})
	if err := Remove("x", false); err != nil || calls != 4 {
		t.Fatalf("err = %v after %d calls, want nil after 4", err, calls)
	}
}

func TestRemoveGivesUpAfterItsTries(t *testing.T) {
	calls := 0
	held := errors.New("in use")
	withOp(t, 3, func(string, bool) error { calls++; return held })
	if err := Remove("x", true); !errors.Is(err, held) || calls != 3 {
		t.Fatalf("err = %v after %d calls, want the last error after 3", err, calls)
	}
}

func TestRemoveRealFilesAndGone(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "secrets.env")
	tree := filepath.Join(dir, "knowledge")
	if err := os.WriteFile(f, []byte("K=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tree, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Remove(f, false); err != nil || !Gone(f) {
		t.Fatalf("file: %v, gone %v", err, Gone(f))
	}
	if err := Remove(tree, true); err != nil || !Gone(tree) {
		t.Fatalf("tree: %v, gone %v", err, Gone(tree))
	}
	if err := Remove(f, false); err != nil {
		t.Fatalf("already gone: %v", err)
	}
	if triesFor("windows") <= 1 || triesFor("linux") != 1 {
		t.Error("only Windows waits")
	}
}
