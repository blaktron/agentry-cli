package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/schedule"
)

const forbidSentence = "Unsafe Mode is forbidden by acme's managed settings (run.unsafeMode)"

// forbidUnsafeMode makes this computer's managed settings forbid Unsafe Mode
// until the returned function lifts it.
func forbidUnsafeMode(t *testing.T) (lift func()) {
	t.Helper()
	saved := scheduleUnsafeModeRefusal
	t.Cleanup(func() { scheduleUnsafeModeRefusal = saved })
	reason := forbidSentence
	scheduleUnsafeModeRefusal = func() string { return reason }
	return func() { reason = "" }
}

func (h *scheduleHarness) addInUnsafeMode(t *testing.T, r schedule.Recurrence) schedule.Schedule {
	t.Helper()
	sc := schedule.Schedule{Name: "triage", Ref: "acme/triage", Version: "1.0.0", Digest: "sha256:bb",
		Run:  schedule.RunSpec{OutDir: t.TempDir(), MaxTurns: -1, MaxCost: -1, AllowVulnerableMCP: true, UnsafeMode: true},
		When: r, Created: r.Start}
	if err := h.store.Create(&sc, nil); err != nil {
		t.Fatal(err)
	}
	if err := installAlarm(sc); err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestAScheduleMadeInUnsafeModeIsHeldWhileManagedSettingsForbidIt(t *testing.T) {
	h := newScheduleHarness(t)
	lift := forbidUnsafeMode(t)
	sc := h.addInUnsafeMode(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: utc(8, 12)})

	h.now = utc(9, 9)
	err := fire(t, sc.ID)
	wantClassified(t, err, 11)
	if !strings.Contains(err.Error(), forbidSentence) || !strings.Contains(err.Error(), "consents were given in Unsafe Mode") {
		t.Errorf("the hold says %q", err)
	}
	if len(h.runs) != 0 {
		t.Fatalf("a held schedule ran: %v", h.runs)
	}
	fires, _ := h.store.Fires(sc.ID, 0)
	if len(fires) != 1 || fires[0].Outcome != schedule.Failed || fires[0].ExitCode != 11 || fires[0].Message != forbidSentence {
		t.Fatalf("the held occurrence is recorded as %+v", fires)
	}
	if st, _ := h.store.State(sc.ID); st.Runs != 0 || !st.Last.Equal(utc(9, 9)) {
		t.Fatalf("state after a hold = %+v, want the occurrence used and no run counted", st)
	}
	// The same occurrence rung again is not run either.
	if err := fire(t, sc.ID); err != nil {
		t.Fatalf("a second ring of a held occurrence: %v", err)
	}

	// Once the organization stops forbidding it, the next occurrence runs.
	lift()
	h.now = utc(10, 9)
	if err := fire(t, sc.ID); err != nil {
		t.Fatal(err)
	}
	if len(h.runs) != 1 || !h.runs[0].Equal(utc(10, 9)) {
		t.Fatalf("after the lock lifts, runs = %v", h.runs)
	}
}

func TestAScheduleNotMadeInUnsafeModeIsNotHeld(t *testing.T) {
	h := newScheduleHarness(t)
	forbidUnsafeMode(t)
	sc := h.add(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: utc(8, 12)})
	sc.Run.AllowVulnerableMCP = true
	if err := h.store.Save(sc); err != nil {
		t.Fatal(err)
	}
	h.now = utc(9, 9)
	if err := fire(t, sc.ID); err != nil {
		t.Fatal(err)
	}
	if len(h.runs) != 1 {
		t.Fatalf("an unmarked schedule was held: runs = %v", h.runs)
	}
}

func TestAHeldScheduleIsNotRunByHandOrEdited(t *testing.T) {
	h := newScheduleHarness(t)
	forbidUnsafeMode(t)
	sc := h.addInUnsafeMode(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: utc(8, 12)})
	h.now = utc(9, 12)

	_, _, err := runCLI(t, nil, "schedule", "run", sc.ID)
	wantClassified(t, err, 11)
	if len(h.runs) != 0 {
		t.Fatalf("run by hand: %v", h.runs)
	}
	_, _, err = runCLI(t, nil, "schedule", "edit", sc.ID, "--daily", "10:00")
	wantClassified(t, err, 11)
	if got, _ := h.store.Get(sc.ID); got.When.Time != "09:00" {
		t.Fatalf("a held schedule was edited: %+v", got.When)
	}
	// Pausing it still works.
	if _, stderr, err := runCLI(t, nil, "schedule", "pause", sc.ID); err != nil {
		t.Fatalf("pause: %v (%s)", err, stderr)
	}
}

func TestListAndShowSayAScheduleIsHeld(t *testing.T) {
	h := newScheduleHarness(t)
	forbidUnsafeMode(t)
	sc := h.addInUnsafeMode(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: utc(8, 12)})
	h.now = utc(9, 12)

	stdout, _, err := runCLI(t, nil, "--json", "schedule", "list")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Schedules []scheduleViewJSON `json:"schedules"`
	}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil || len(list.Schedules) != 1 {
		t.Fatalf("list = %s (%v)", stdout, err)
	}
	if v := list.Schedules[0]; v.Held != forbidSentence || !v.Run.UnsafeMode {
		t.Errorf("list's view = held %q, unsafeMode %v", v.Held, v.Run.UnsafeMode)
	}
	stdout, _, err = runCLI(t, nil, "schedule", "list")
	if err != nil || !strings.Contains(stdout, "(held: Unsafe Mode is forbidden here)") {
		t.Errorf("list says %q (%v)", stdout, err)
	}
	stdout, _, err = runCLI(t, nil, "schedule", "show", sc.ID)
	if err != nil || !strings.Contains(stdout, "consents given in Unsafe Mode: yes") || !strings.Contains(stdout, "held: "+forbidSentence) {
		t.Errorf("show says %q (%v)", stdout, err)
	}
}

func TestAddInUnsafeModeIsRefusedWhileManagedSettingsForbidIt(t *testing.T) {
	newScheduleHarness(t)
	forbidUnsafeMode(t)
	for _, extra := range [][]string{nil, {"--on", "box"}} {
		args := append([]string{"schedule", "add", "acme/triage", "--out", t.TempDir(), "--daily", "09:00",
			"--allow-vulnerable-mcp", "--in-unsafe-mode"}, extra...)
		_, _, err := runCLI(t, nil, args...)
		wantClassified(t, err, 11)
		if err == nil || !strings.Contains(err.Error(), forbidSentence) {
			t.Errorf("add %v: %v", extra, err)
		}
	}
}

func TestTheUnsafeModeMarkRoundTrips(t *testing.T) {
	h := newScheduleHarness(t)
	sc := h.addInUnsafeMode(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: utc(8, 12)})
	got, err := h.store.Get(sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Run.UnsafeMode || !got.Run.AllowVulnerableMCP {
		t.Fatalf("the record lost its mark: %+v", got.Run)
	}
	spec := runSpecFlags{inUnsafeMode: true, allowVulnMCP: true, maxTurns: -1, maxCost: -1}
	made, err := scheduleFromFlags("x", t.TempDir(), spec, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: utc(8, 12)}, utc(8, 12))
	if err != nil {
		t.Fatal(err)
	}
	if !made.Run.UnsafeMode {
		t.Fatalf("--in-unsafe-mode was not recorded: %+v", made.Run)
	}
}
