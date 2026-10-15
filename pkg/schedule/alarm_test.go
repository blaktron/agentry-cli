package schedule

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// unixAlarm skips a test of an alarm that writes Unix paths (systemd,
// launchd) where the running system cannot call them absolute: those
// alarms never run on Windows.
func unixAlarm(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a systemd or launchd entry names Unix paths; this alarm never runs on Windows")
	}
}

func TestOnCalendar(t *testing.T) {
	start := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		r    Recurrence
		want string
	}{
		{Recurrence{Kind: Once, Date: "2026-10-20", Time: "08:00"}, "2026-10-20 08:00:00 Europe/London"},
		{Recurrence{Kind: Daily, Time: "09:00"}, "*-*-* 09:00:00 Europe/London"},
		{Recurrence{Kind: Weekdays, Time: "09:00"}, "Mon..Fri *-*-* 09:00:00 Europe/London"},
		{Recurrence{Kind: Weekly, Days: []string{"tue", "thu"}, Time: "22:00"}, "Tue,Thu *-*-* 22:00:00 Europe/London"},
		{Recurrence{Kind: Monthly, MonthDay: 5, Time: "09:00"}, "*-*-05 09:00:00 Europe/London"},
		{Recurrence{Kind: Monthly, MonthDay: 31, Time: "09:00"}, "*-*-28..31 09:00:00 Europe/London"},
	}
	for _, c := range cases {
		c.r.Zone, c.r.Start = "Europe/London", start
		got, err := OnCalendar(c.r, start)
		if err != nil || !reflect.DeepEqual(got, []string{c.want}) {
			t.Errorf("%s: %v %v, want %q", c.r.Kind, got, err, c.want)
		}
	}
}

// systemd never rings at a skipped wall time, so the calendar carries a
// one-off ring at the instant the schedule's rule runs it.
func TestOnCalendarRingsSkippedTimes(t *testing.T) {
	r := Recurrence{Kind: Daily, Time: "02:30", Zone: "America/New_York", Start: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
	got, err := OnCalendar(r, r.Start)
	if err != nil {
		t.Fatal(err)
	}
	// 2027-03-14, 2028-03-12 and 2029-03-11 skip 02:00–03:00: each runs at 03:00 EDT = 07:00Z.
	want := []string{"*-*-* 02:30:00 America/New_York", "2027-03-14 07:00:00 UTC", "2028-03-12 07:00:00 UTC", "2029-03-11 07:00:00 UTC"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("OnCalendar = %q, want %q", got, want)
	}
	// A time no change skips gets no extra ring.
	r.Time = "09:00"
	if got, _ := OnCalendar(r, r.Start); len(got) != 1 {
		t.Fatalf("09:00: %q", got)
	}
}

// A superset calendar (the 28th to the 31st) rings on days that are not
// occurrences; fire's Due narrows it to the month's own day.
func TestSupersetRingsAreNarrowedByDue(t *testing.T) {
	r := Recurrence{Kind: Monthly, MonthDay: 30, Time: "09:00", Zone: "UTC", Start: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
	feb28 := time.Date(2027, 2, 28, 9, 0, 0, 0, time.UTC)
	if run, _ := Due(r, State{Last: time.Date(2027, 1, 30, 9, 0, 0, 0, time.UTC)}, feb28); !run.Equal(feb28) {
		t.Fatalf("february's last day: %v", run)
	}
	mar28 := time.Date(2027, 3, 28, 9, 0, 0, 0, time.UTC)
	if run, _ := Due(r, State{Last: feb28}, mar28); !run.IsZero() {
		t.Fatalf("march 28th is not the 30th: %v", run)
	}
}

func goldenEntry() Entry {
	return Entry{
		Binary: "/usr/bin/agentry",
		Args:   []string{"schedule", "fire", "abcdefghij"},
		Env:    []string{"PATH=/usr/local/bin:/usr/bin", "AGENTRY_DOCKER=/usr/bin/docker"},
	}
}

// Golden: the generated units hold the binary, the schedule id, paths and
// the calendar, and nothing else.
func TestSystemdUnitsGolden(t *testing.T) {
	unixAlarm(t)
	sc := sample(t)
	sc.ID = "abcdefghij"
	service, timer, err := SystemdUnits(sc, goldenEntry())
	if err != nil {
		t.Fatal(err)
	}
	wantService := `# Written by agentry schedule; the schedule itself is abcdefghij in the runner home.
[Unit]
Description=Agentry schedule: Morning digest

[Service]
Type=oneshot
Environment="PATH=/usr/local/bin:/usr/bin"
Environment="AGENTRY_DOCKER=/usr/bin/docker"
ExecStart="/usr/bin/agentry" "schedule" "fire" "abcdefghij"
`
	wantTimer := `# Written by agentry schedule: every day at 09:00 (UTC).
[Unit]
Description=Agentry schedule: Morning digest

[Timer]
OnCalendar=*-*-* 09:00:00 UTC
Persistent=true
AccuracySec=1min
Unit=agentry-schedule-abcdefghij.service

[Install]
WantedBy=timers.target
`
	if service != wantService {
		t.Errorf("service:\n%s\nwant:\n%s", service, wantService)
	}
	if timer != wantTimer {
		t.Errorf("timer:\n%s\nwant:\n%s", timer, wantTimer)
	}
}

func TestSystemdUnitsEscape(t *testing.T) {
	unixAlarm(t)
	sc := sample(t)
	sc.ID = "abcdefghij"
	sc.Name = "100% of it\n[Service]\nExecStart=/bin/sh"
	e := goldenEntry()
	e.Binary = "/opt/my $app/agentry"
	service, timer, err := SystemdUnits(sc, e)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{service, timer} {
		if strings.Count(u, "\n[Service]\n") > 1 || strings.Contains(u, "\nExecStart=/bin/sh") {
			t.Fatalf("a name broke out of its line:\n%s", u)
		}
		if !strings.Contains(u, "100%% of it") {
			t.Fatalf("percent not escaped:\n%s", u)
		}
	}
	if !strings.Contains(service, `ExecStart="/opt/my $$app/agentry"`) {
		t.Fatalf("binary not quoted:\n%s", service)
	}
	e.Binary = "agentry"
	if _, _, err := SystemdUnits(sc, e); err == nil {
		t.Fatal("a relative binary was accepted")
	}
	e = goldenEntry()
	e.Env = []string{"PATH=/bin\nExecStart=/bin/sh"}
	if _, _, err := SystemdUnits(sc, e); err == nil {
		t.Fatal("a multi-line environment entry was accepted")
	}
}

func TestSystemdProbe(t *testing.T) {
	unixAlarm(t)
	var got []string
	s := &Systemd{SystemdRun: func(args ...string) ([]byte, error) {
		got = args
		return []byte("ok\n"), nil
	}}
	e := goldenEntry()
	e.Args = []string{"schedule", "probe"}
	out, err := s.Probe(e)
	want := []string{"--wait", "--pipe", "--collect", "--quiet", "--setenv=PATH=/usr/local/bin:/usr/bin",
		"--setenv=AGENTRY_DOCKER=/usr/bin/docker", "--", "/usr/bin/agentry", "schedule", "probe"}
	if err != nil || out != "ok" || !reflect.DeepEqual(got, want) {
		t.Fatalf("Probe = %q %v, args %q", out, err, got)
	}
}

func TestSystemdInstallRemove(t *testing.T) {
	unixAlarm(t)
	var calls []string
	s := &Systemd{Dir: filepath.Join(t.TempDir(), "user"), Systemctl: func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}}
	sc := sample(t)
	sc.ID = "abcdefghij"
	if err := s.Install(sc, goldenEntry()); err != nil {
		t.Fatal(err)
	}
	want := []string{"daemon-reload", "enable agentry-schedule-abcdefghij.timer", "restart agentry-schedule-abcdefghij.timer"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("install calls %v, want %v", calls, want)
	}
	// Someone else's unit in the same folder is never listed.
	_ = os.WriteFile(filepath.Join(s.Dir, "backup.timer"), []byte("x"), 0o644)
	ids, err := s.Installed()
	if err != nil || !reflect.DeepEqual(ids, []string{"abcdefghij"}) {
		t.Fatalf("Installed = %v %v", ids, err)
	}
	calls = nil
	if err := s.Remove("abcdefghij"); err != nil {
		t.Fatal(err)
	}
	want = []string{"disable --now agentry-schedule-abcdefghij.timer", "daemon-reload"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("remove calls %v, want %v", calls, want)
	}
	if ids, _ := s.Installed(); len(ids) != 0 {
		t.Fatalf("still installed: %v", ids)
	}
	if err := s.Remove("../../x"); err == nil {
		t.Fatal("Remove took a path")
	}
}

// A fire that ends its own schedule removes its alarm from inside the
// service that runs it: the timer goes, the running service keeps its
// file (a reload without it would stop the run on the manager's default
// start timeout), and the next Remove sweeps it once it has stopped.
func TestSystemdRemoveKeepsARunningServiceFile(t *testing.T) {
	unixAlarm(t)
	running := map[string]bool{}
	s := &Systemd{Dir: filepath.Join(t.TempDir(), "user"), Systemctl: func(...string) error { return nil },
		Running: func(unit string) bool { return running[unit] }}
	for _, id := range []string{"abcdefghij", "klmnopqrst"} {
		sc := sample(t)
		sc.ID = id
		if err := s.Install(sc, goldenEntry()); err != nil {
			t.Fatal(err)
		}
	}
	svc := filepath.Join(s.Dir, "agentry-schedule-abcdefghij.service")
	running["agentry-schedule-abcdefghij.service"] = true
	if err := s.Remove("abcdefghij"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(svc); err != nil {
		t.Fatal("the running service's file was removed under it")
	}
	if ids, _ := s.Installed(); !reflect.DeepEqual(ids, []string{"klmnopqrst"}) {
		t.Fatalf("Installed = %v: a timerless service is no alarm", ids)
	}
	running["agentry-schedule-abcdefghij.service"] = false
	sc := sample(t)
	sc.ID = "klmnopqrst"
	if err := s.Install(sc, goldenEntry()); err != nil { // a sync
		t.Fatal(err)
	}
	if _, err := os.Stat(svc); !os.IsNotExist(err) {
		t.Fatal("the stopped service's file was not swept")
	}
	if err := s.Remove("klmnopqrst"); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(s.Dir); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}
