package engine

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeWorkDir(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	for _, f := range []string{"docs/report.pdf", "decoded/report.txt", "apply/wiki/page.md"} {
		path := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("in the clear\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestSweepWorkDirsRemovesWhatADeadBuildLeftAndNeverALiveOnes: a work
// directory whose owner holds its lock is left whole; the same directory
// once the owner is gone, which leaves the lock file behind with nothing
// holding it, is removed with everything in it.
func TestSweepWorkDirsRemovesWhatADeadBuildLeftAndNeverALiveOnes(t *testing.T) {
	root := t.TempDir()
	dir := writeWorkDir(t, root, "knowledge-build-1")
	release, err := HoldWorkDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if got := SweepWorkDirs(root, "knowledge-build-", time.Hour, &out); len(got) != 0 || !pathExists(filepath.Join(dir, "docs", "report.pdf")) || out.Len() != 0 {
		t.Fatalf("the sweep touched a live build's directory: %v %q", got, out.String())
	}
	if _, err := HoldWorkDir(dir); err == nil {
		t.Error("a second process took the lock of a live build's directory")
	}
	// The owner is gone: its lock file stays, held by nothing.
	release()
	got := SweepWorkDirs(root, "knowledge-build-", time.Hour, &out)
	if len(got) != 1 || got[0] != "knowledge-build-1" || pathExists(dir) {
		t.Fatalf("the sweep left a dead build's directory: %v", got)
	}
	if !strings.Contains(out.String(), "knowledge-build-1") {
		t.Errorf("the sweep did not say what it removed: %q", out.String())
	}
}

// TestSweepWorkDirsJudgesOnlyWhatIsProvenDead: a directory with no lock
// file is a starting build's until it is older than the grace, a directory
// of another name or a link in a directory's place is never touched.
func TestSweepWorkDirsJudgesOnlyWhatIsProvenDead(t *testing.T) {
	root := t.TempDir()
	fresh := writeWorkDir(t, root, "knowledge-build-fresh")
	stale := writeWorkDir(t, root, "knowledge-build-stale")
	other := writeWorkDir(t, root, "something-else")
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	outside := writeWorkDir(t, t.TempDir(), "outside")
	if err := os.Symlink(outside, filepath.Join(root, "knowledge-build-link")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	SweepWorkDirs(root, "knowledge-build-", time.Hour, nil)
	if !pathExists(fresh) || pathExists(stale) || !pathExists(other) || !pathExists(filepath.Join(outside, "docs", "report.pdf")) {
		t.Errorf("fresh %v, stale %v, other %v, outside %v", pathExists(fresh), pathExists(stale), pathExists(other), pathExists(outside))
	}
	if got := SweepWorkDirs(filepath.Join(root, "absent"), "knowledge-build-", time.Hour, nil); got != nil {
		t.Errorf("a missing root swept %v", got)
	}
}
