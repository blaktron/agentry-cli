package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// fakeAlarm records what fire and add ask of the system's scheduler.
type fakeAlarm struct {
	installed map[string]schedule.Entry
	removed   []string
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

func (a *fakeAlarm) Installed() ([]string, error) {
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
}

// The alarm's entry names paths, never a credential (invariant 7).
func TestFireEntryCarriesNoCredential(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-should-never-appear")
	t.Setenv("GITHUB_TOKEN", "ghp_should_never_appear")
	t.Setenv("AGENTRY_TOKEN", "agt_should_never_appear")
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
