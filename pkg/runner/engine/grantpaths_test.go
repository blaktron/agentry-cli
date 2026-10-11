package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// TestMakeGrantedPaths (agentry-cli#855): each path a paths grant binds
// exists in the run's workspace before the gateway starts the tools
// server, a path the release ships is left as it is, and a link the
// release ships cannot carry a new folder outside the workspace.
func TestMakeGrantedPaths(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "notes.md"), []byte("shipped\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	caps := spec.CapSummary{
		FSReadMode: "paths", FSReadPaths: []string{"docs", "notes.md"},
		FSWriteMode: "paths", FSWritePaths: []string{"out", "reports/weekly"},
	}
	if err := makeGrantedPaths(ws, caps); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"docs", "out", "reports/weekly"} {
		if fi, err := os.Stat(filepath.Join(ws, filepath.FromSlash(p))); err != nil || !fi.IsDir() {
			t.Errorf("%s: want a folder, got %v", p, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(ws, "notes.md")); err != nil || string(b) != "shipped\n" {
		t.Errorf("the shipped file changed: %q %v", b, err)
	}

	// Whole-workspace grants bind the workspace itself: nothing to make.
	empty := t.TempDir()
	if err := makeGrantedPaths(empty, spec.CapSummary{FSReadMode: "workspace", FSWriteMode: "workspace"}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Errorf("whole-workspace grants made %d entries", len(entries))
	}

	if runtime.GOOS == "windows" {
		return // a symlink needs a privilege the test cannot assume
	}
	outside := t.TempDir()
	linked := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(linked, "out")); err != nil {
		t.Fatal(err)
	}
	err := makeGrantedPaths(linked, spec.CapSummary{FSWriteMode: "paths", FSWritePaths: []string{"out/sub"}})
	if err == nil {
		t.Fatal("a shipped link let a granted folder be made outside the workspace")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "sub")); statErr == nil {
		t.Fatal("a folder was made outside the workspace")
	}
}
