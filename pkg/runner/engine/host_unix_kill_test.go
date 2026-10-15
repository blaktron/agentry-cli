//go:build !windows

package engine

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestConfigProcessEscalatesToTheWholeGroup: exec's own escalation when
// WaitDelay expires is os.Process.Kill on the leader's single pid, so a child
// that ignores SIGTERM survives the run holding whatever it was given — the
// model key, in the harness's case. The group is SIGTERM'd first and
// SIGKILL'd after the grace, which reaches that child.
//
// The child has to hold the leader's stdout open, because that is what makes
// cmd.Wait block past the leader's own exit and gives the escalation its
// window: with Stdout an *os.File the descriptor is handed straight to the
// child, Wait returns the moment the leader dies, and there is nothing left to
// escalate.
func TestConfigProcessEscalatesToTheWholeGroup(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skipf("needs /proc to tell a dead child from a live one: %v", err)
	}
	// Short, so the test does not spend ten seconds on it. A parameter to
	// configProcess rather than a package variable, because a write from here
	// would race with the escalation goroutine's read under -race.
	const grace = 400 * time.Millisecond

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "harness.sh")
	// The leader answers SIGTERM and dies. The child ignores it, loops, and
	// keeps the inherited stdout open.
	body := "#!/bin/sh\n( trap '' TERM; while :; do sleep 0.05; done ) &\necho $! > " + pidFile + "\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // a throwaway test script
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", script)
	cmd.Stdout = io.Discard // not an *os.File: Wait blocks on the pipe the child holds
	cmd.Stderr = io.Discard
	waited := configProcess(cmd, grace)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// The child's pid, from the leader's $! — $$ inside a subshell is the
	// parent's pid in both dash and bash, so it cannot be used here.
	var child int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, aerr := strconv.Atoi(strings.TrimSpace(string(b))); aerr == nil && pid > 0 {
				child = pid
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if child == 0 {
		t.Fatal("the child never recorded its pid")
	}
	if !processAlive(child) {
		t.Fatalf("child %d is not running before the kill", child)
	}

	cancel() // what a budget or a timeout does
	start := time.Now()
	werr := cmd.Wait()
	waited()
	elapsed := time.Since(start)
	t.Logf("Wait returned after %s: %v", elapsed, werr)

	gone := false
	for i := 0; i < 150 && !gone; i++ {
		gone = !processAlive(child)
		if !gone {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !gone {
		_ = syscall.Kill(child, syscall.SIGKILL) // do not leave it behind
		t.Fatalf("child %d ignored SIGTERM and survived: the escalation did not reach the process group", child)
	}
	// Ours, not exec's: WaitDelay is grace + 2s, so a Wait that returned
	// inside the grace plus a margin was ended by the group SIGKILL.
	if elapsed > grace+time.Second {
		t.Errorf("Wait took %s, past the %s grace — exec's single-pid escalation ended it, not the group's", elapsed, grace)
	}
}

// TestProcStartTimeReadsTheStartField pins the field arithmetic the group
// kill's safety rests on: /proc/<pid>/stat's comm field is parenthesised and
// may contain spaces and parentheses itself, so the start time is counted from
// the last ')' — and a mis-counted field reads something that is not a tick
// count at all, which would silently disable the pid-reuse guard.
func TestProcStartTimeReadsTheStartField(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skipf("needs /proc: %v", err)
	}
	start, ok := procStartTime(os.Getpid())
	if !ok {
		t.Fatal("procStartTime could not read this process's own start time")
	}
	uptime, err := os.ReadFile("/proc/uptime")
	if err != nil {
		t.Skipf("needs /proc/uptime: %v", err)
	}
	secs, _, found := strings.Cut(string(uptime), " ")
	if !found {
		t.Fatalf("/proc/uptime is not two fields: %q", uptime)
	}
	up, err := strconv.ParseFloat(strings.TrimSpace(secs), 64)
	if err != nil {
		t.Fatalf("/proc/uptime: %v", err)
	}
	// The field is ticks since boot at the kernel's 100 Hz, so it cannot
	// exceed the uptime; a field counted one place off reads a pid, a ppid or
	// a flag word instead, and lands outside that bound.
	if limit := uint64(up*100) + 100; start == 0 || start > limit {
		t.Fatalf("start time %d ticks is not a plausible tick count (uptime %.0fs, limit %d) — the field is mis-counted", start, up, limit)
	}
}

// processAlive reports whether pid is still a live process. A zombie counts as
// dead: it was killed and is only waiting to be reaped, and its parent here is
// not this test, so nothing of ours will reap it.
func processAlive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	// The comm field is parenthesised and may contain spaces, so the state is
	// the first field after the last ')'.
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 >= len(s) {
		return false
	}
	return s[i+2] != 'Z'
}
