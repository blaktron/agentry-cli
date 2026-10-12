package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// addLocalThroughPrepare runs `schedule add --local` on the pure-text
// fixture to the end, its prepare step included, against a fake daemon.
func addLocalThroughPrepare(t *testing.T, h *signedOutHarness, extra ...string) (*dockerx.Fake, error) {
	t.Helper()
	return scheduleAddLocal(t, h, repoPath(t, "testdata/runner/pure-text"), extra...)
}

// scheduleAddLocal is addLocalThroughPrepare on the QuickAgent in dir.
func scheduleAddLocal(t *testing.T, h *signedOutHarness, dir string, extra ...string) (*dockerx.Fake, error) {
	t.Helper()
	f := &dockerx.Fake{}
	seedHostLinkImage(t, f, h.policy)
	prev := newDockerClient
	newDockerClient = func() dockerx.Client { return f }
	t.Cleanup(func() { newDockerClient = prev })
	args := append([]string{"schedule", "add", "--local", dir,
		"--once", "2026-10-09 09:00", "--zone", "UTC", "--out", t.TempDir(), "--policy", h.policy, "--yes"}, extra...)
	_, _, err := runCLI(t, strings.NewReader(""), args...)
	return f, err
}

// builtRunnerImage says whether the prepare step ran: it builds the
// runner image the fake daemon does not have.
func builtRunnerImage(f *dockerx.Fake) bool {
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "Build:agentry-runner:") {
			return true
		}
	}
	return false
}

// TestScheduleAddLocalKeepsAllowUnsigned (agentry-cli#856): add --local
// with --allow-unsigned goes through its prepare step and keeps the
// schedule with the flag, and a fire's own run arguments pass the same
// gates.
func TestScheduleAddLocalKeepsAllowUnsigned(t *testing.T) {
	h := newSignedOutHarness(t)
	f, err := addLocalThroughPrepare(t, h, "--allow-unsigned")
	if err != nil {
		t.Fatalf("add --local --allow-unsigned: %v", err)
	}
	if !builtRunnerImage(f) {
		t.Fatalf("the add never reached its prepare step: %q", f.Ops())
	}
	list, _, err := h.store.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("kept %d schedule(s): %v", len(list), err)
	}
	sc := list[0]
	if sc.Ref != "fixtures/pure-text" || sc.Version != "1.0.0" || sc.Name != "fixtures/pure-text" {
		t.Errorf("kept as %q %q named %q, want the ref without its version", sc.Ref, sc.Version, sc.Name)
	}
	if !sc.Run.AllowUnsigned {
		t.Fatal("the schedule dropped --allow-unsigned")
	}
	if len(h.alarm.installed) != 1 {
		t.Fatalf("alarms = %d", len(h.alarm.installed))
	}
	// A fire runs `agentry run` with baseRunArgs: the pinned directory and
	// the flag pass the verify and consent gates up to the prepare step.
	args := baseRunArgs(sc)
	if !slices.Contains(args, "--allow-unsigned") {
		t.Fatalf("a fire is not given --allow-unsigned: %q", args)
	}
	if _, stderr, err := runCLI(t, strings.NewReader(""), append(append([]string{"run"}, args...), "--prepare")...); err != nil {
		t.Fatalf("a fire's run arguments: %v (%s)", err, stderr)
	}
}

// Without --allow-unsigned the add is refused before anything is kept,
// naming the flag.
func TestScheduleAddLocalRefusedWithoutAllowUnsigned(t *testing.T) {
	h := newSignedOutHarness(t)
	f, err := addLocalThroughPrepare(t, h)
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "--allow-unsigned") {
		t.Fatalf("refusal = %v", err)
	}
	if builtRunnerImage(f) {
		t.Fatal("a refused add reached its prepare step")
	}
	list, _, err := h.store.List()
	if err != nil || len(list) != 0 {
		t.Fatalf("a refused add kept %d schedule(s): %v", len(list), err)
	}
}
