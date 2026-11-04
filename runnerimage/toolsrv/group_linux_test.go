//go:build linux

package toolsrv

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A process a declared command leaves running in the background is ended
// with the call: it writes nothing once run_command has returned.
//
// sleep is declared so the loop finds it and idles between ticks, and the
// loop stops by itself after about twenty seconds, so a loop this process
// cannot end (it dies mid-call) neither spins nor stays.
func TestRunCommandEndsWhatItLeftRunning(t *testing.T) {
	spec := baseSpec(t)
	spec.FSWrite = Access{Enabled: true}
	spec.Exec = Exec{Commands: []string{"sh", "sleep"}}
	s := newTestServer(t, spec)
	s.confine = confinement{}
	ticks := filepath.Join(spec.WorkspaceDir, "ticks")
	got, isErr := s.toolRunCommand(rawArgs(t, map[string]any{"command": "sh", "args": []string{"-c",
		`(i=0; while [ $i -lt 400 ]; do echo tick >> "$1"; sleep 0.05; i=$((i+1)); done) >/dev/null 2>&1 & echo started`, "sh", ticks}}))
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

// endShimGroups kills the process group of every process still running from
// one of dir's shims. Such a process is alive when its group is read, so the
// group is one a declared command started, never a group a reused pid leads;
// the test's own group is never signalled, only the process.
func endShimGroups(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	prefix := []byte(dir + string(os.PathSeparator))
	own := syscall.Getpgrp()
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || !bytes.HasPrefix(cmdline, prefix) {
			continue
		}
		pgid, err := syscall.Getpgid(pid)
		switch {
		case err != nil:
		case pgid <= 1 || pgid == own:
			_ = syscall.Kill(pid, syscall.SIGKILL)
		default:
			if syscall.Kill(-pgid, syscall.SIGKILL) == nil {
				t.Logf("ended process group %d, left running by a declared command", pgid)
			}
		}
	}
}
