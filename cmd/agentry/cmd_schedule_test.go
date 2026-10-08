package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// fakeAlarm records what fire and add ask of the system's scheduler.
type fakeAlarm struct {
	installed map[string]schedule.Entry
	removed   []string
	listErr   error // Installed fails with it
}

func (*fakeAlarm) Name() string { return "fake" }
func (a *fakeAlarm) Install(sc schedule.Schedule, e schedule.Entry) error {
	a.installed[sc.ID] = e
	return nil
}

func (a *fakeAlarm) Remove(id string) error {
	delete(a.installed, id)
	a.removed = append(a.removed, id)
	return nil
}

func (*fakeAlarm) Probe(schedule.Entry) (string, error) { return "ok", nil }

func (a *fakeAlarm) Installed() ([]string, error) {
	if a.listErr != nil {
		return nil, a.listErr
	}
	var ids []string
	for id := range a.installed {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

// scheduleHarness points the store at a temp runner home and replaces the
// alarm, the clock and the run with fakes.
type scheduleHarness struct {
	store schedule.Store
	alarm *fakeAlarm
	now   time.Time
	runs  []time.Time
	// result is what each fake run returns.
	result schedule.Outcome
}

func newScheduleHarness(t *testing.T) *scheduleHarness {
	t.Helper()
	t.Setenv("AGENTRY_RUNNER_HOME", t.TempDir())
	h := &scheduleHarness{alarm: &fakeAlarm{installed: map[string]schedule.Entry{}}, result: schedule.Ran}
	store, err := scheduleStore()
	if err != nil {
		t.Fatal(err)
	}
	h.store = store
	savedAlarm, savedNow, savedExec := scheduleAlarm, scheduleNow, scheduleExecute
	t.Cleanup(func() { scheduleAlarm, scheduleNow, scheduleExecute = savedAlarm, savedNow, savedExec })
	scheduleAlarm = func() (schedule.Alarm, error) { return h.alarm, nil }
	scheduleNow = func() time.Time { return h.now }
	scheduleExecute = func(_ *cobra.Command, _ schedule.Store, _ schedule.Schedule, occ time.Time, manual bool) schedule.Fire {
		h.runs = append(h.runs, occ)
		f := schedule.Fire{Occurrence: occ, Manual: manual, Outcome: h.result}
		if h.result == schedule.Failed {
			f.ExitCode, f.Message = 13, "docker is not running"
		}
		return f
	}
	return h
}

func (h *scheduleHarness) add(t *testing.T, r schedule.Recurrence) schedule.Schedule {
	t.Helper()
	sc := schedule.Schedule{Name: "digest", Ref: "acme/digest", Version: "1.0.0", Digest: "sha256:aa",
		Run: schedule.RunSpec{OutDir: t.TempDir(), MaxTurns: -1, MaxCost: -1}, When: r, Created: r.Start}
	if err := h.store.Create(&sc, nil); err != nil {
		t.Fatal(err)
	}
	if err := installAlarm(sc); err != nil {
		t.Fatal(err)
	}
	return sc
}

func fire(t *testing.T, id string) error {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	return scheduleFire(cmd, id)
}

func utc(d, h int) time.Time { return time.Date(2026, 10, d, h, 0, 0, 0, time.UTC) }

func TestScheduleFireRunsWhatIsDueOnce(t *testing.T) {
	h := newScheduleHarness(t)
	sc := h.add(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: utc(8, 12)})

	h.now = utc(9, 9)
	if err := fire(t, sc.ID); err != nil {
		t.Fatal(err)
	}
	// The same occurrence rung again (a duplicate trigger) does not run.
	if err := fire(t, sc.ID); err != nil {
		t.Fatal(err)
	}
	// An early ring finds nothing due.
	h.now = utc(9, 20)
	if err := fire(t, sc.ID); err != nil {
		t.Fatal(err)
	}
	if len(h.runs) != 1 || !h.runs[0].Equal(utc(9, 9)) {
		t.Fatalf("runs = %v, want one at the 9th 09:00", h.runs)
	}
	fires, _ := h.store.Fires(sc.ID, 0)
	if len(fires) != 1 || fires[0].Outcome != schedule.Ran {
		t.Fatalf("fires = %+v", fires)
	}
}

func TestScheduleFireRecordsMissedAndRunsTheLatest(t *testing.T) {
	h := newScheduleHarness(t)
	sc := h.add(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: utc(8, 12)})
	// Off from the 9th to the 12th at 10:00 (Persistent=true rings at boot).
	h.now = utc(12, 10)
	if err := fire(t, sc.ID); err != nil {
		t.Fatal(err)
	}
	if len(h.runs) != 1 || !h.runs[0].Equal(utc(12, 9)) {
		t.Fatalf("runs = %v, want the 12th's only", h.runs)
	}
	fires, _ := h.store.Fires(sc.ID, 0)
	var missed int
	for _, f := range fires {
		if f.Outcome == schedule.Missed {
			missed++
		}
	}
	if missed != 3 {
		t.Fatalf("missed = %d, want 3 (the 9th to the 11th): %+v", missed, fires)
	}
}

func TestScheduleFireFailureIsRecordedAndExitsNonZero(t *testing.T) {
	h := newScheduleHarness(t)
	h.result = schedule.Failed
	sc := h.add(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: utc(8, 12)})
	h.now = utc(9, 9)
	err := fire(t, sc.ID)
	if code, ok := exitCodeOf(err); !ok || code != 13 {
		t.Fatalf("fire = %v (code %d), want exit 13", err, code)
	}
	fires, _ := h.store.Fires(sc.ID, 0)
	if len(fires) != 1 || fires[0].Outcome != schedule.Failed || fires[0].ExitCode != 13 {
		t.Fatalf("fires = %+v", fires)
	}
	// A failure is not retried: the next ring of the same occurrence is a no-op.
	_ = fire(t, sc.ID)
	if len(h.runs) != 1 {
		t.Fatalf("retried: %v", h.runs)
	}
}

func TestScheduleFireOnceEndsAndRemovesItsAlarm(t *testing.T) {
	h := newScheduleHarness(t)
	sc := h.add(t, schedule.Recurrence{Kind: schedule.Once, Date: "2026-10-10", Time: "08:00", Zone: "UTC", Start: utc(8, 12)})
	h.now = utc(10, 8)
	if err := fire(t, sc.ID); err != nil {
		t.Fatal(err)
	}
	st, _ := h.store.State(sc.ID)
	if !st.Ended || len(h.runs) != 1 {
		t.Fatalf("state %+v runs %v", st, h.runs)
	}
	if _, ok := h.alarm.installed[sc.ID]; ok {
		t.Fatal("the alarm of an ended schedule is still installed")
	}
}

func TestScheduleFirePausedAndGone(t *testing.T) {
	h := newScheduleHarness(t)
	sc := h.add(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: utc(8, 12)})
	sc.Paused = true
	if err := h.store.Save(sc); err != nil {
		t.Fatal(err)
	}
	h.now = utc(9, 9)
	if err := fire(t, sc.ID); err != nil || len(h.runs) != 0 {
		t.Fatalf("paused: %v %v", err, h.runs)
	}
	// An alarm whose schedule was removed removes itself.
	if err := h.store.Remove(sc.ID); err != nil {
		t.Fatal(err)
	}
	if err := fire(t, sc.ID); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(h.alarm.removed, sc.ID) || len(h.runs) != 0 {
		t.Fatalf("gone: removed %v runs %v", h.alarm.removed, h.runs)
	}
}

// An end after N runs counts runs: a missed occurrence uses none of them.
func TestScheduleEndsAfterNRuns(t *testing.T) {
	h := newScheduleHarness(t)
	sc := h.add(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: utc(8, 12),
		End: schedule.End{Kind: schedule.EndAfter, After: 2}})
	h.now = utc(9, 9)
	_ = fire(t, sc.ID)
	h.now = utc(12, 9) // the 10th and 11th missed: one more run left
	_ = fire(t, sc.ID)
	st, _ := h.store.State(sc.ID)
	if st.Runs != 2 || !st.Ended || len(h.runs) != 2 {
		t.Fatalf("state %+v runs %v", st, h.runs)
	}
	if _, ok := h.alarm.installed[sc.ID]; ok {
		t.Fatal("the alarm outlived the last run")
	}
}

// A refusal not lifted refuses the schedule; an --allow-* flag given for
// nothing is not kept, so it cannot accept a refusal nobody has seen yet;
// an interactive-only release is refused (invariant 2).
func TestAdmitPreview(t *testing.T) {
	r := schedule.RunSpec{AllowUnsigned: true, AllowUnscanned: true, AllowVulnerableMCP: true}
	p := &runPlanOutput{Ref: "acme/x@1.0.0", Modes: []string{"autonomous"},
		Refusals: []runPlanRefusal{{Kind: refusalAttestation, Message: "unscanned", Lift: "--allow-unscanned"}}}
	got, err := admitPreview(p, r)
	if err != nil || !got.AllowUnscanned || got.AllowUnsigned || got.AllowVulnerableMCP {
		t.Fatalf("admitPreview = %+v %v, want only --allow-unscanned kept", got, err)
	}
	if _, err := admitPreview(p, schedule.RunSpec{}); err == nil {
		t.Fatal("an unlifted refusal was admitted")
	}
	p.Refusals = []runPlanRefusal{{Kind: refusalInstalledChanged, Message: "changed"}}
	if _, err := admitPreview(p, r); err == nil {
		t.Fatal("a refusal no flag lifts was admitted")
	}
	p = &runPlanOutput{Ref: "acme/chat@1.0.0", Modes: []string{"interactive"}}
	if _, err := admitPreview(p, r); err == nil {
		t.Fatal("an interactive-only release was admitted")
	}
}

func TestScheduleSync(t *testing.T) {
	h := newScheduleHarness(t)
	live := h.add(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: utc(8, 12)})
	paused := h.add(t, schedule.Recurrence{Kind: schedule.Daily, Time: "10:00", Zone: "UTC", Start: utc(8, 12)})
	paused.Paused = true
	_ = h.store.Save(paused)
	h.alarm.installed["zzzzzzzzzz"] = schedule.Entry{} // an orphan
	delete(h.alarm.installed, live.ID)                 // a lost alarm
	written, removed, err := scheduleSync()
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := h.alarm.Installed()
	if written != 1 || removed != 2 || !slices.Equal(ids, []string{live.ID}) {
		t.Fatalf("sync wrote %d removed %d, now %v", written, removed, ids)
	}
}

// A fire runs the pinned digest, autonomous, with only the consents given
// at add, and never a blanket --yes (invariant 1).
func TestBaseRunArgsPinsAndNeverSaysYes(t *testing.T) {
	sc := schedule.Schedule{Ref: "acme/digest", Version: "1.2.0", Digest: "sha256:abc",
		Run: schedule.RunSpec{MaxTurns: -1, MaxCost: -1, Intake: []string{"topic=go"}, Files: []string{"/data/in.csv"}}}
	args := baseRunArgs(sc)
	joined := strings.Join(args, " ")
	for _, want := range []string{"acme/digest@1.2.0", "--pin sha256:abc", "--mode autonomous", "--intake topic=go", "--file /data/in.csv"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q lack %q", joined, want)
		}
	}
	for _, never := range []string{"--yes", "--allow-unsigned", "--allow-unscanned", "--allow-vulnerable-mcp", "--max-turns", "--max-cost"} {
		if strings.Contains(joined, never) {
			t.Errorf("args %q carry %q", joined, never)
		}
	}
	sc.Run.AllowUnscanned = true
	if !slices.Contains(baseRunArgs(sc), "--allow-unscanned") {
		t.Error("a consent given at add is not passed")
	}
	// A local directory is pinned too: an edited one is another release.
	local := schedule.Schedule{Local: "/src/agent", Digest: "sha256:def", Run: schedule.RunSpec{MaxTurns: -1, MaxCost: -1}}
	if j := strings.Join(baseRunArgs(local), " "); !strings.Contains(j, "--local /src/agent --pin sha256:def") || strings.Contains(j, "--yes") {
		t.Errorf("local args %q", j)
	}
}

// The alarm's entry names paths, never a credential (invariant 7).
func TestFireEntryCarriesNoCredential(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-should-never-appear")
	t.Setenv("GITHUB_TOKEN", "ghp_should_never_appear")
	t.Setenv("AGENTRY_TOKEN", "agt_should_never_appear")
	t.Setenv("DOCKER_HOST", "tcp://admin:never@docker.internal:2376")
	e, err := fireEntry("abcdefghij")
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range e.Env {
		if strings.Contains(kv, "never") {
			t.Fatalf("entry env carries a credential: %q", kv)
		}
		k, _, _ := strings.Cut(kv, "=")
		if !slices.Contains([]string{"PATH", "AGENTRY_DOCKER", "AGENTRY_RUNNER_HOME", "AGENTRY_CONFIG_DIR", "DOCKER_HOST", "DOCKER_CONTEXT"}, k) {
			t.Fatalf("entry env carries %s", k)
		}
	}
	if !slices.Equal(e.Args, []string{"schedule", "fire", "abcdefghij"}) {
		t.Fatalf("args %v", e.Args)
	}
	// The whole chain: what each system's entry is written from carries
	// none of it either.
	sc := schedule.Schedule{ID: "abcdefghij", Name: "digest", When: schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: utc(8, 12)}}
	service, timer, err := schedule.SystemdUnits(sc, e)
	if err != nil {
		t.Fatal(err)
	}
	plist, err := schedule.LaunchAgentPlist(sc, e, time.UTC, utc(8, 12))
	if err != nil {
		t.Fatal(err)
	}
	task, err := schedule.TaskXML(sc, e, "S-1-5-21-1-2-3-1001", `C:\Windows\System32\conhost.exe`, time.UTC, utc(8, 12))
	if err != nil {
		t.Fatal(err)
	}
	for name, doc := range map[string]string{"service": service, "timer": timer, "plist": string(plist), "task": task} {
		if strings.Contains(doc, "never") || strings.Contains(doc, "admin:") {
			t.Fatalf("the %s carries a credential:\n%s", name, doc)
		}
	}
}

func TestCheckFireInputs(t *testing.T) {
	keyring := func(present bool) func(string) (bool, error) {
		return func(string) (bool, error) { return present, nil }
	}
	env := func(kv ...string) func(string) string {
		return func(k string) string {
			for i := 0; i+1 < len(kv); i += 2 {
				if kv[i] == k {
					return kv[i+1]
				}
			}
			return ""
		}
	}
	keyringPol := func() *policy.Policy {
		p := &policy.Policy{}
		p.Model.Provider, p.Model.APIKeyEnv, p.Model.APIKeySource = "anthropic", "ANTHROPIC_API_KEY", "keyring"
		p.SecretSource.Mode = "env"
		return p
	}
	m := &manifest.Manifest{}
	m.Capabilities.Secrets = []string{"NEWS_KEY", "TOPIC_TOKEN"}
	m.Capabilities.OptionalSecrets = []string{"TOPIC_TOKEN"}

	cases := []struct {
		name    string
		pol     *policy.Policy
		prof    map[string]string
		intake  []string
		m       *manifest.Manifest
		key     bool
		getenv  func(string) string
		wantGap []string // substrings, one per expected gap; nil = no gaps
	}{
		{"all kept", keyringPol(), map[string]string{"NEWS_KEY": "x"}, nil, m, true, env(), nil},
		{"no key in the keychain", keyringPol(), map[string]string{"NEWS_KEY": "x"}, nil, m, false, env(), []string{"no model key is kept"}},
		{"key only in this shell", keyringPol(), map[string]string{"NEWS_KEY": "x"}, nil, m, false, env("ANTHROPIC_API_KEY", "k"), []string{"only in $ANTHROPIC_API_KEY"}},
		{"env-only key source", func() *policy.Policy { p := keyringPol(); p.Model.APIKeySource = ""; return p }(), map[string]string{"NEWS_KEY": "x"}, nil, m, true, env("ANTHROPIC_API_KEY", "k"), []string{"comes from $ANTHROPIC_API_KEY"}},
		{"secret only in this shell", keyringPol(), nil, nil, m, true, env("NEWS_KEY", "v"), []string{"NEWS_KEY is only in this shell"}},
		{"secret missing", keyringPol(), nil, nil, m, true, env(), []string{"NEWS_KEY is not kept"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gaps := checkFireInputs(c.m, c.pol, c.prof, c.intake, keyring(c.key), c.getenv)
			if len(gaps) != len(c.wantGap) {
				t.Fatalf("gaps = %q, want %d like %q", gaps, len(c.wantGap), c.wantGap)
			}
			for i, w := range c.wantGap {
				if !strings.Contains(gaps[i], w) {
					t.Errorf("gap %q lacks %q", gaps[i], w)
				}
			}
		})
	}

	// Intake: a secret answer is never kept by a schedule; a required
	// question with no answer is a gap.
	qm := &manifest.Manifest{}
	qm.Capabilities.Secrets = []string{"PASSWORD"}
	qm.Intake = []manifest.IntakeQuestion{
		{ID: "topic", Prompt: "Topic", Required: true},
		{ID: "password", Prompt: "Password", Type: manifest.IntakeSecret},
	}
	gaps := checkFireInputs(qm, keyringPol(), map[string]string{"PASSWORD": "p"}, []string{"password=hunter2"}, keyring(true), env())
	joined := strings.Join(gaps, "\n")
	if !strings.Contains(joined, "a schedule keeps no secret answer") || !strings.Contains(joined, `"topic" is required`) {
		t.Fatalf("intake gaps = %q", gaps)
	}
	if gaps := checkFireInputs(qm, keyringPol(), map[string]string{"PASSWORD": "p"}, []string{"topic=go"}, keyring(true), env()); len(gaps) != 0 {
		t.Fatalf("answered intake: gaps %q", gaps)
	}
}

func TestWhenFlags(t *testing.T) {
	start := utc(8, 12)
	ok := []whenFlags{
		{daily: "09:00", zone: "UTC"},
		{weekly: "tue", at: "22:00", zone: "UTC"},
		{monthly: 31, at: "09:00", zone: "UTC", times: 3},
		{once: "2026-10-20 08:00", zone: "UTC"},
		{weekdays: "07:30", zone: "Europe/London", until: "2026-12-24"},
	}
	for _, w := range ok {
		if _, err := w.recurrence(start); err != nil {
			t.Errorf("%+v: %v", w, err)
		}
	}
	bad := []whenFlags{
		{},                                      // nothing
		{daily: "09:00", weekdays: "09:00"},     // two
		{weekly: "tue"},                         // no --at
		{daily: "09:00", at: "10:00"},           // --at with daily
		{once: "2026-10-01 08:00", zone: "UTC"}, // in the past
		{once: "2026-10-20", zone: "UTC"},       // no time
		{daily: "09:00", until: "2026-12-01", times: 2},
	}
	for _, w := range bad {
		if _, err := w.recurrence(start); err == nil {
			t.Errorf("%+v: accepted", w)
		}
	}
}

// --pin holds a --local directory to its digest (a schedule's pin of a
// local QuickAgent): the right digest runs, any other is a verify refusal.
func TestRunLocalHonoursPin(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	pkg := repoPath(t, "testdata/runner/pure-text")

	stdout, stderr, err := runCLI(t, strings.NewReader(""), "--json", "run", "--local", pkg, "--allow-unsigned", "--plan", "--policy", policyPath)
	if err != nil {
		t.Fatalf("plan: %v (stderr %q)", err, stderr)
	}
	var p runPlanOutput
	if err := json.Unmarshal([]byte(stdout), &p); err != nil || p.Digest == "" {
		t.Fatalf("plan JSON: %v %q", err, stdout)
	}
	if _, stderr, err := runCLI(t, strings.NewReader(""), "run", "--local", pkg, "--allow-unsigned", "--dry-run", "--policy", policyPath, "--pin", p.Digest); err != nil {
		t.Fatalf("the pinned digest refused: %v (stderr %q)", err, stderr)
	}
	_, _, err = runCLI(t, strings.NewReader(""), "run", "--local", pkg, "--allow-unsigned", "--dry-run", "--policy", policyPath,
		"--pin", "sha256:0000000000000000000000000000000000000000000000000000000000000000")
	wantClassified(t, err, engine.ExitVerify)
}

// D5, the same on every system because fire decides: whatever rings it
// (a timer at boot, a LaunchAgent at sign-in, a logon trigger), an
// occurrence missed while the computer was off, asleep or signed out runs
// once, late, if no later occurrence has fallen due; one whose following
// occurrence is already due is recorded missed and does not run.
func TestScheduleFireMissedOccurrenceRule(t *testing.T) {
	cases := []struct {
		name        string
		r           schedule.Recurrence
		rings       []time.Time // when the system rang fire
		wantRan     []time.Time // the occurrences run, in order
		wantMissed  []time.Time
		wantRunsLog int // ran lines in fires.ndjson
	}{
		{
			name:    "back before the following occurrence: runs late, once",
			r:       schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: utc(8, 12)},
			rings:   []time.Time{utc(9, 17), utc(9, 17).Add(time.Minute)}, // sign-in, and a second trigger at once
			wantRan: []time.Time{utc(9, 9)}, wantRunsLog: 1,
		},
		{
			name:    "back as the following occurrence falls due: that one runs, the earlier is missed",
			r:       schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: utc(8, 12)},
			rings:   []time.Time{utc(10, 9)},
			wantRan: []time.Time{utc(10, 9)}, wantMissed: []time.Time{utc(9, 9)}, wantRunsLog: 1,
		},
		{
			name:    "asleep across two weekly occurrences: the latest runs, the earlier is missed",
			r:       schedule.Recurrence{Kind: schedule.Weekly, Days: []string{"tue", "thu"}, Time: "22:00", Zone: "UTC", Start: utc(9, 12)},
			rings:   []time.Time{utc(16, 7)}, // Fri 16th: Tue 13th and Thu 15th fell while asleep
			wantRan: []time.Time{utc(15, 22)}, wantMissed: []time.Time{utc(13, 22)}, wantRunsLog: 1,
		},
		{
			name:    "a once schedule has no following occurrence: it runs when the computer is back",
			r:       schedule.Recurrence{Kind: schedule.Once, Date: "2026-10-10", Time: "08:00", Zone: "UTC", Start: utc(8, 12)},
			rings:   []time.Time{utc(12, 10)},
			wantRan: []time.Time{utc(10, 8)}, wantRunsLog: 1,
		},
		{
			name:  "a ring before anything is due runs nothing",
			r:     schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: utc(8, 12)},
			rings: []time.Time{utc(8, 13), utc(9, 8)},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newScheduleHarness(t)
			sc := h.add(t, c.r)
			for _, at := range c.rings {
				h.now = at
				if err := fire(t, sc.ID); err != nil {
					t.Fatal(err)
				}
			}
			if !slices.EqualFunc(h.runs, c.wantRan, time.Time.Equal) {
				t.Fatalf("ran %v, want %v", h.runs, c.wantRan)
			}
			fires, _ := h.store.Fires(sc.ID, 0)
			var missed []time.Time
			ran := 0
			for _, f := range fires {
				switch f.Outcome {
				case schedule.Missed:
					missed = append(missed, f.Occurrence)
				case schedule.Ran:
					ran++
				}
			}
			if !slices.EqualFunc(missed, c.wantMissed, time.Time.Equal) || ran != c.wantRunsLog {
				t.Fatalf("fires.ndjson: missed %v ran %d, want missed %v ran %d (%+v)", missed, ran, c.wantMissed, c.wantRunsLog, fires)
			}
		})
	}
}

// list and show mark a schedule whose OS entry is missing; sync writes it
// again.
func TestScheduleListMarksAMissingAlarmAndSyncRepairsIt(t *testing.T) {
	h := newScheduleHarness(t)
	h.now = utc(8, 13)
	sc := h.add(t, schedule.Recurrence{Kind: schedule.Daily, Time: "09:00", Zone: "UTC", Start: utc(8, 12)})
	delete(h.alarm.installed, sc.ID) // removed by hand, or lost

	list := func() (text string, views []scheduleViewJSON) {
		t.Helper()
		var out bytes.Buffer
		cmd := newScheduleListCmd()
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.RunE(cmd, nil); err != nil {
			t.Fatal(err)
		}
		saved := flags.jsonOut
		flags.jsonOut = true
		defer func() { flags.jsonOut = saved }()
		var js bytes.Buffer
		cmd.SetOut(&js)
		if err := cmd.RunE(cmd, nil); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(js.Bytes(), &views); err != nil {
			t.Fatal(err)
		}
		return out.String(), views
	}
	text, views := list()
	if !strings.Contains(text, "no alarm: run `agentry schedule sync`") || len(views) != 1 || views[0].Alarm || views[0].AlarmUnknown {
		t.Fatalf("a missing alarm is not marked:\n%s\n%+v", text, views)
	}
	var show bytes.Buffer
	sh := newScheduleShowCmd()
	sh.SetOut(&show)
	if err := sh.RunE(sh, []string{sc.ID}); err != nil || !strings.Contains(show.String(), "no alarm") {
		t.Fatalf("show: %v\n%s", err, show.String())
	}
	if _, _, err := scheduleSync(); err != nil {
		t.Fatal(err)
	}
	text, views = list()
	if strings.Contains(text, "no alarm") || !views[0].Alarm {
		t.Fatalf("sync did not repair it:\n%s", text)
	}
	// A scheduler that cannot be asked is said so, not called missing.
	h.alarm.listErr = errors.New("schtasks /Query: exit status 1")
	text, views = list()
	if strings.Contains(text, "no alarm") || !strings.Contains(text, "could not be asked") || !views[0].AlarmUnknown {
		t.Fatalf("an unanswered scheduler:\n%s\n%+v", text, views)
	}
}

// What a Task Scheduler entry carries as --env is set before fire or the
// probe reads anything; the probe's result also lands in --report.
func TestScheduleProbeTakesEnvAndReport(t *testing.T) {
	report := filepath.Join(t.TempDir(), "report.txt")
	t.Setenv("AGENTRY_RUNNER_HOME", t.TempDir())
	// --env sets the process's own environment: register both so the
	// test's cleanup restores them.
	t.Setenv("AGENTRY_DOCKER", "")
	t.Setenv("PATH", os.Getenv("PATH"))
	dir := t.TempDir()
	cmd := newScheduleProbeCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"--env", "AGENTRY_DOCKER=" + filepath.Join(dir, "no-docker-here"), "--env", "PATH=" + dir,
		"--policy", filepath.Join(dir, "policy.yaml"), "--report", report})
	err := cmd.Execute()
	if code, ok := exitCodeOf(err); !ok || code != engine.ExitPolicy {
		t.Fatalf("probe = %v, want the policy exit", err)
	}
	if got := os.Getenv("AGENTRY_DOCKER"); got != filepath.Join(dir, "no-docker-here") {
		t.Fatalf("--env not applied: %q", got)
	}
	b, rerr := os.ReadFile(report)
	if rerr != nil || !strings.HasPrefix(string(b), "gap: ") || strings.HasSuffix(strings.TrimSpace(string(b)), "ok") {
		t.Fatalf("report %q %v", b, rerr)
	}
	if err := applyAlarmEnv([]string{"NOEQUALS"}); err == nil {
		t.Fatal("an --env without = was accepted")
	}
}
