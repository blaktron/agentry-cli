package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// checkLocal runs `schedule add --local dir --check --json` with the
// harness's policy and returns what it printed.
func checkLocal(t *testing.T, h *signedOutHarness, dir string, extra ...string) scheduleCheckJSON {
	t.Helper()
	args := append([]string{"schedule", "add", "--local", dir, "--daily", "09:00", "--zone", "UTC",
		"--out", t.TempDir(), "--policy", h.policy, "--check", "--json"}, extra...)
	stdout, stderr, err := runCLI(t, strings.NewReader(""), args...)
	if err != nil {
		t.Fatalf("check: %v (%s)", err, stderr)
	}
	var res scheduleCheckJSON
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("check output %q: %v", stdout, err)
	}
	return res
}

// assertNothingKept: a check keeps no schedule, writes no alarm, seals
// nothing and leaves linger alone.
func assertNothingKept(t *testing.T, h *signedOutHarness) {
	t.Helper()
	if list, _, _ := h.store.List(); len(list) != 0 {
		t.Errorf("a check kept %d schedule(s)", len(list))
	}
	if len(h.alarm.installed) != 0 || len(h.sealed) != 0 || len(h.loginctl) != 0 || h.lingerOn() {
		t.Errorf("a check changed something: alarms %d, sealed %d, loginctl %v, linger %v",
			len(h.alarm.installed), len(h.sealed), h.loginctl, h.lingerOn())
	}
}

func TestScheduleCheckReadyKeepsNothing(t *testing.T) {
	h := newSignedOutHarness(t)
	res := checkLocal(t, h, repoPath(t, "testdata/runner/pure-text"), "--allow-unsigned", "--signed-out")
	if !res.Ready || len(res.Gaps) != 0 {
		t.Fatalf("not ready: %q", res.Gaps)
	}
	// --allow-unsigned lifts the directory's refusal, so a run meets none
	// (agentry-cli#856: the preview lists it, the check does not).
	if len(res.Refusals) != 0 {
		t.Errorf("refusals = %+v", res.Refusals)
	}
	if !strings.HasPrefix(res.Ref, "fixtures/pure-text") || res.Version != "1.0.0" || res.Digest == "" || res.Next == nil || res.Describe == "" {
		t.Errorf("check = %+v", res)
	}
	if !res.SignedOut.Available || !res.Installed {
		t.Errorf("signed out %+v, installed %v", res.SignedOut, res.Installed)
	}
	assertNothingKept(t, h)
}

// A registry release not laid down on this computer is no gap: add
// installs it. The machine's signed-out refusals still are.
func TestScheduleCheckNotInstalledIsNoGap(t *testing.T) {
	newSignedOutHarness(t)
	sc := schedule.Schedule{Ref: "acme/digest", Version: "1.0.0", Digest: "sha256:aa"}
	if installedHere(sc) {
		t.Fatal("an empty runner home holds acme/digest")
	}
	home, _ := runnerHome()
	if err := os.MkdirAll(installdirReleaseDir(home, sc), 0o755); err != nil {
		t.Fatal(err)
	}
	if !installedHere(sc) {
		t.Fatal("a laid-down release reads as not installed")
	}
	if !installedHere(schedule.Schedule{Local: t.TempDir()}) {
		t.Fatal("a local directory reads as not installed")
	}
}

func TestScheduleCheckListsEveryGap(t *testing.T) {
	h := newSignedOutHarness(t)
	scheduleGOOS = "darwin"

	// An interactive-only copy of the fixture.
	dir := t.TempDir()
	b, err := os.ReadFile(repoPath(t, "testdata/runner/pure-text/agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), bytes.Replace(b, []byte("modes: [autonomous]"), []byte("modes: [interactive]"), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("Say hello.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// No --allow-unsigned, and --signed-out where it is not offered.
	res := checkLocal(t, h, dir, "--signed-out")
	gaps := strings.Join(res.Gaps, "\n")
	for _, want := range []string{
		"runs interactive only",
		"a run would refuse: ",
		"Docker Desktop stops when you sign out",
	} {
		if !strings.Contains(gaps, want) {
			t.Errorf("gaps lack %q:\n%s", want, gaps)
		}
	}
	if res.Ready || res.SignedOut.Available || !strings.Contains(res.SignedOut.Reason, "Docker Desktop stops") {
		t.Errorf("ready %v, signed out %+v", res.Ready, res.SignedOut)
	}
	if len(res.Refusals) == 0 || res.Refusals[0].Lift != "--allow-unsigned" {
		t.Errorf("refusals = %+v", res.Refusals)
	}
	assertNothingKept(t, h)
}

// An output folder that cannot be written is a gap, not a failure to check.
func TestScheduleCheckOutputFolderIsAGap(t *testing.T) {
	h := newSignedOutHarness(t)
	missing := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(missing, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runCLI(t, strings.NewReader(""), "schedule", "add", "--local", repoPath(t, "testdata/runner/pure-text"),
		"--daily", "09:00", "--out", missing, "--policy", h.policy, "--allow-unsigned", "--check", "--json")
	if err != nil {
		t.Fatalf("check: %v (%s)", err, stderr)
	}
	var res scheduleCheckJSON
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatal(err)
	}
	if res.Ready || !strings.Contains(strings.Join(res.Gaps, "\n"), "--out "+missing) {
		t.Fatalf("gaps = %q", res.Gaps)
	}
	assertNothingKept(t, h)
}

func TestScheduleCheckSignedOut(t *testing.T) {
	h := newSignedOutHarness(t)
	read := func() signedOutAvailability {
		t.Helper()
		stdout, _, err := runCLI(t, nil, "schedule", "check-signed-out", "--json")
		if err != nil {
			t.Fatal(err)
		}
		var a signedOutAvailability
		if err := json.Unmarshal([]byte(stdout), &a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	if a := read(); !a.Available || a.Reason != "" {
		t.Fatalf("availability = %+v", a)
	}
	h.dockerOS = "Docker Desktop"
	if a := read(); a.Available || !strings.Contains(a.Reason, "needs Docker Engine") {
		t.Fatalf("Docker Desktop: %+v", a)
	}
	h.dockerOS, h.systemdV = "Fedora Linux 44", 255
	if a := read(); a.Available || !strings.Contains(a.Reason, "systemd 256") {
		t.Fatalf("systemd 255: %+v", a)
	}
	scheduleGOOS = "windows"
	if a := read(); a.Available || !strings.Contains(a.Reason, "Docker Desktop stops when you sign out") {
		t.Fatalf("windows: %+v", a)
	}
	if gaps := signedOutMachineGaps(context.Background()); len(gaps) != 1 {
		t.Fatalf("windows gaps = %q", gaps)
	}
}

func TestScheduleResealCommand(t *testing.T) {
	h := newSignedOutHarness(t)
	a := h.addLocal(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: h.now})
	b := h.addLocal(t, schedule.Recurrence{Kind: schedule.Daily, Time: "10:00", Zone: "UTC", Start: h.now})
	for _, id := range []string{a.ID, b.ID} {
		if _, _, err := runCLI(t, strings.NewReader(""), "schedule", "edit", id, "--signed-out", "--yes"); err != nil {
			t.Fatal(err)
		}
	}
	h.sealed = nil
	stdout, _, err := runCLI(t, nil, "schedule", "reseal", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rep resealReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Sealed) != 2 || len(rep.Failed) != 0 || len(h.sealed) != 2 {
		t.Fatalf("report %+v, sealed %d", rep, len(h.sealed))
	}

	// A keychain that no longer holds the key: both lose their sealed
	// keys, the report names them, and the command fails.
	scheduleCollectKeys = func([]string, []string) (map[string]string, error) {
		return nil, errors.New("the OS keychain has no entry provider:openai")
	}
	stdout, _, err = runCLI(t, nil, "schedule", "reseal", "--json")
	wantClassified(t, err, engine.ExitPolicy)
	rep = resealReport{}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Failed) != 2 || rep.Failed[0].Name != "pure" || !strings.Contains(rep.Failed[0].Error, "no entry") {
		t.Fatalf("report %+v", rep)
	}
	if credentialKept(h.store, a.ID) || credentialKept(h.store, b.ID) {
		t.Fatal("a failed reseal kept the old keys")
	}
}

func installdirReleaseDir(home string, sc schedule.Schedule) string {
	ns, name, _ := strings.Cut(sc.Ref, "/")
	return installdir.ReleaseDir(home, ns, name, sc.Version, sc.Digest)
}
