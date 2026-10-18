//go:build linux

package toolsrv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A process a declared command leaves running in the background is ended
// with the call: it writes nothing once run_command has returned.
func TestRunCommandEndsWhatItLeftRunning(t *testing.T) {
	spec := baseSpec(t)
	spec.FSWrite = Access{Enabled: true}
	spec.Exec = Exec{Commands: []string{"sh"}}
	s := newTestServer(t, spec)
	s.confine = confinement{}
	ticks := filepath.Join(spec.WorkspaceDir, "ticks")
	got, isErr := s.toolRunCommand(rawArgs(t, map[string]any{"command": "sh", "args": []string{"-c",
		`(while :; do echo tick >> "$1"; sleep 0.05; done) >/dev/null 2>&1 & echo started`, "sh", ticks}}))
	if isErr || !strings.Contains(got, "started") {
		t.Fatalf("run_command = %q (isError %v)", got, isErr)
	}
	size := func() int64 {
		fi, err := os.Stat(ticks)
		if err != nil {
			return 0
		}
		return fi.Size()
	}
	time.Sleep(100 * time.Millisecond)
	before := size()
	time.Sleep(500 * time.Millisecond)
	if after := size(); after != before {
		t.Fatalf("the background loop outlived the call: %d bytes, then %d", before, after)
	}
}

// A name the check passed, whose directory a background process then swaps
// for a link out of the workspace, is not written through the link: the
// write goes through a beneath.Dir on the workspace.
func TestAWorkspaceWriteRefusesALinkSwappedInAfterTheCheck(t *testing.T) {
	spec := baseSpec(t)
	spec.FSWrite = Access{Enabled: true}
	s := newTestServer(t, spec)
	outside := t.TempDir()
	dir := filepath.Join(spec.WorkspaceDir, "notes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := s.workspacePath("notes/today.md")
	if err != nil {
		t.Fatal(err)
	}
	// The swap, between the check and the write.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	if err := s.writeWorkspace(rel, "hello"); err == nil {
		t.Fatal("the write went through a link swapped in after the check")
	}
	if _, err := os.Stat(filepath.Join(outside, "today.md")); !os.IsNotExist(err) {
		t.Fatalf("a file was written outside the workspace: %v", err)
	}
}
