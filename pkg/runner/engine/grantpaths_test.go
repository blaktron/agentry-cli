package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// TestMakeGrantedPaths: each path a paths grant binds exists in the
// run's workspace before the gateway starts the tools server, a path
// the release ships is left as it is, and a link the release ships
// cannot carry a new folder outside the workspace.
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
	// Paths of a grant that is not a paths grant are not made.
	stray := t.TempDir()
	if err := makeGrantedPaths(stray, spec.CapSummary{
		FSReadMode: "workspace", FSReadPaths: []string{"stray-read"},
		FSWriteMode: "none", FSWritePaths: []string{"stray-write"},
	}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(stray); len(entries) != 0 {
		t.Errorf("paths of a grant that is not a paths grant made %d entries", len(entries))
	}

	// A workspace that is not there is the cli class, naming the step.
	var ce *ClassifiedError
	gone := filepath.Join(t.TempDir(), "gone")
	if err := makeGrantedPaths(gone, spec.CapSummary{FSWriteMode: "paths", FSWritePaths: []string{"out"}}); !errors.As(err, &ce) || ce.Code != ExitCLI || !strings.Contains(err.Error(), "open the run's workspace") {
		t.Fatalf("a workspace that is gone: %v", err)
	}

	// A path that runs through a file cannot be examined, and is not made.
	err := makeGrantedPaths(ws, spec.CapSummary{FSWriteMode: "paths", FSWritePaths: []string{"notes.md/sub"}})
	if !errors.As(err, &ce) || ce.Code != ExitCLI || !strings.HasPrefix(err.Error(), "the granted path notes.md/sub: ") {
		t.Fatalf("a path through a file: %v", err)
	}
	if b, rerr := os.ReadFile(filepath.Join(ws, "notes.md")); rerr != nil || string(b) != "shipped\n" {
		t.Fatalf("the file in the way changed: %q %v", b, rerr)
	}

	// Through a run, the folder is made in the run's copy of the workspace and
	// never in the install it was copied from.
	p, f := fixtureEdited(t, nil, func(m *manifest.Manifest) {
		m.Capabilities.Filesystem.Write = manifest.FSAccess{Mode: manifest.FSModePaths, Paths: []string{"out"}}
	})
	happyFake(f)
	p.KeepRunDir = true
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fi, statErr := os.Stat(filepath.Join(p.RunRoot, "runs", p.RunID, "workspace", "out")); statErr != nil || !fi.IsDir() {
		t.Errorf("the run's workspace has no out folder: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(p.WorkspaceSrc, "out")); statErr == nil {
		t.Error("the installed workspace was given the folder")
	}

	if runtime.GOOS == "windows" {
		return // a symlink needs a privilege the test cannot assume
	}
	outside := t.TempDir()
	linked := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(linked, "out")); err != nil {
		t.Fatal(err)
	}
	err = makeGrantedPaths(linked, spec.CapSummary{FSWriteMode: "paths", FSWritePaths: []string{"out/sub"}})
	if err == nil {
		t.Fatal("a shipped link let a granted folder be made outside the workspace")
	}
	if !errors.As(err, &ce) || ce.Code != ExitCLI || !strings.HasPrefix(err.Error(), "the granted path out/sub: ") {
		t.Fatalf("a link out of the workspace: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outside, "sub")); statErr == nil {
		t.Fatal("a folder was made outside the workspace")
	}
}
