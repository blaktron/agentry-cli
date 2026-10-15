package schedule

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Golden: the LaunchAgent holds the binary, the schedule id, paths and the
// calendar, and nothing else.
func TestLaunchAgentPlistGolden(t *testing.T) {
	unixAlarm(t)
	sc := sample(t)
	sc.ID = "abcdefghij"
	sc.When = Recurrence{Kind: Weekly, Days: []string{"tue", "thu"}, Time: "22:00", Zone: "UTC", Start: sc.Created}
	b, err := LaunchAgentPlist(sc, goldenEntry(), time.UTC, sc.Created)
	if err != nil {
		t.Fatal(err)
	}
	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by agentry schedule: Morning digest, every tue, thu at 22:00 (UTC). The schedule itself is abcdefghij in the runner home. -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>net.secureagentry.schedule.abcdefghij</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/bin/agentry</string>
		<string>schedule</string>
		<string>fire</string>
		<string>abcdefghij</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/usr/local/bin:/usr/bin</string>
		<key>AGENTRY_DOCKER</key>
		<string>/usr/bin/docker</string>
	</dict>
	<key>StartCalendarInterval</key>
	<array>
		<dict>
			<key>Weekday</key>
			<integer>2</integer>
			<key>Hour</key>
			<integer>22</integer>
			<key>Minute</key>
			<integer>0</integer>
		</dict>
		<dict>
			<key>Weekday</key>
			<integer>4</integer>
			<key>Hour</key>
			<integer>22</integer>
			<key>Minute</key>
			<integer>0</integer>
		</dict>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>ProcessType</key>
	<string>Background</string>
</dict>
</plist>
`
	if string(b) != want {
		t.Fatalf("plist:\n%s\nwant:\n%s", b, want)
	}
}

// "Once" is carried by the month and day; a monthly day past the 28th
// rings on every day the month's last can be; every day needs no day.
func TestLaunchAgentPlistCalendars(t *testing.T) {
	unixAlarm(t)
	sc := sample(t)
	sc.ID = "abcdefghij"
	cases := []struct {
		r    Recurrence
		want []string
	}{
		{Recurrence{Kind: Once, Date: "2026-10-20", Time: "08:05"},
			[]string{"<key>Month</key>\n\t\t\t<integer>10</integer>\n\t\t\t<key>Day</key>\n\t\t\t<integer>20</integer>\n\t\t\t<key>Hour</key>\n\t\t\t<integer>8</integer>\n\t\t\t<key>Minute</key>\n\t\t\t<integer>5</integer>"}},
		{Recurrence{Kind: Monthly, MonthDay: 30, Time: "09:00"},
			[]string{"<key>Day</key>\n\t\t\t<integer>28</integer>", "<key>Day</key>\n\t\t\t<integer>31</integer>"}},
		{Recurrence{Kind: Daily, Time: "07:00"},
			[]string{"<dict>\n\t\t\t<key>Hour</key>\n\t\t\t<integer>7</integer>\n\t\t\t<key>Minute</key>\n\t\t\t<integer>0</integer>\n\t\t</dict>"}},
	}
	for _, c := range cases {
		c.r.Zone, c.r.Start = "UTC", sc.Created
		sc.When = c.r
		b, err := LaunchAgentPlist(sc, goldenEntry(), time.UTC, sc.Created)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range c.want {
			if !strings.Contains(string(b), w) {
				t.Errorf("%s: plist lacks %q:\n%s", c.r.Describe(), w, b)
			}
		}
	}
}

func TestLaunchAgentPlistEscapes(t *testing.T) {
	unixAlarm(t)
	sc := sample(t)
	sc.ID = "abcdefghij"
	sc.Name = "a --> <b>&\n</dict>"
	e := goldenEntry()
	e.Binary = "/Applications/My <App>.app/agentry"
	b, err := LaunchAgentPlist(sc, e, time.UTC, sc.Created)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Count(s, "-->") != 1 || strings.Count(s, "</dict>") != 3 || !strings.Contains(s, "My &lt;App&gt;.app") {
		t.Fatalf("a name or path broke out:\n%s", s)
	}
	e.Binary = "agentry"
	if _, err := LaunchAgentPlist(sc, e, time.UTC, sc.Created); err == nil {
		t.Fatal("a relative binary was accepted")
	}
}

// fakeLaunchctl keeps which labels are loaded and running.
type fakeLaunchctl struct {
	loaded map[string]string // target → state
	calls  []string
	refuse bool
	onBoot func(path string)
}

func (f *fakeLaunchctl) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case "print":
		if st, ok := f.loaded[args[1]]; ok {
			return []byte("gui/501/x = {\n\tstate = " + st + "\n}\n"), nil
		}
		return []byte("Could not find service"), errors.New("exit status 113")
	case "bootstrap":
		if f.refuse {
			return []byte("Bootstrap failed: 125: Domain does not support specified action"), errors.New("exit status 125")
		}
		label := strings.TrimSuffix(filepath.Base(args[2]), ".plist")
		f.loaded[args[1]+"/"+label] = "not running"
		if f.onBoot != nil {
			f.onBoot(args[2])
		}
	case "bootout":
		delete(f.loaded, args[1])
	}
	return nil, nil
}

func newFakeLaunchd(t *testing.T) (*Launchd, *fakeLaunchctl) {
	f := &fakeLaunchctl{loaded: map[string]string{}}
	return &Launchd{Dir: filepath.Join(t.TempDir(), "LaunchAgents"), UID: 501, Loc: time.UTC, Launchctl: f.run,
		Now: func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) }, Wait: 2 * time.Second}, f
}

func TestLaunchdInstallRemove(t *testing.T) {
	unixAlarm(t)
	l, f := newFakeLaunchd(t)
	sc := sample(t)
	sc.ID = "abcdefghij"
	if err := l.Install(sc, goldenEntry()); err != nil {
		t.Fatal(err)
	}
	target := "gui/501/net.secureagentry.schedule.abcdefghij"
	if _, ok := f.loaded[target]; !ok {
		t.Fatalf("not bootstrapped: %v", f.calls)
	}
	// An unchanged, loaded agent is left alone: no bootout, no RunAtLoad.
	f.calls = nil
	if err := l.Install(sc, goldenEntry()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.calls, []string{"print " + target}) {
		t.Fatalf("an unchanged agent was reloaded: %v", f.calls)
	}
	// A changed one is not reloaded while it runs.
	f.loaded[target] = "running"
	sc.When.Time = "10:00"
	if err := l.Install(sc, goldenEntry()); err == nil || !strings.Contains(err.Error(), "running now") {
		t.Fatalf("reloaded under a running fire: %v", err)
	}
	f.loaded[target] = "not running"
	f.calls = nil
	if err := l.Install(sc, goldenEntry()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.Join(f.calls, ";"), "print "+target+";bootout "+target+";bootstrap gui/501 ") {
		t.Fatalf("change calls %v", f.calls)
	}
	// Someone else's agent in the same folder is never listed.
	_ = os.WriteFile(filepath.Join(l.Dir, "com.example.backup.plist"), []byte("x"), 0o644)
	ids, err := l.Installed()
	if err != nil || !reflect.DeepEqual(ids, []string{"abcdefghij"}) {
		t.Fatalf("Installed = %v %v", ids, err)
	}
	if err := l.Remove("abcdefghij"); err != nil {
		t.Fatal(err)
	}
	if ids, _ := l.Installed(); len(ids) != 0 {
		t.Fatalf("still installed: %v", ids)
	}
	if _, ok := f.loaded[target]; ok {
		t.Fatal("still loaded")
	}
	if err := l.Remove("../../x"); err == nil {
		t.Fatal("Remove took a path")
	}
}

// A fire that ends its own schedule removes the file but does not boot
// out the agent it is running in.
func TestLaunchdRemoveFromItsOwnFire(t *testing.T) {
	unixAlarm(t)
	l, f := newFakeLaunchd(t)
	sc := sample(t)
	sc.ID = "abcdefghij"
	if err := l.Install(sc, goldenEntry()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XPC_SERVICE_NAME", "net.secureagentry.schedule.abcdefghij")
	f.calls = nil
	if err := l.Remove(sc.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("booted out its own agent: %v", f.calls)
	}
	if ids, _ := l.Installed(); len(ids) != 0 {
		t.Fatalf("the file is still there: %v", ids)
	}
}

// A pause or a removal made while a run is going does not boot the agent
// out: launchd would stop the run before it is recorded.
func TestLaunchdRemoveSparesARunningAgent(t *testing.T) {
	unixAlarm(t)
	l, f := newFakeLaunchd(t)
	sc := sample(t)
	sc.ID = "abcdefghij"
	if err := l.Install(sc, goldenEntry()); err != nil {
		t.Fatal(err)
	}
	target := "gui/501/net.secureagentry.schedule.abcdefghij"
	f.loaded[target] = "running"
	if err := l.Remove(sc.ID); err != nil {
		t.Fatal(err)
	}
	if f.loaded[target] != "running" {
		t.Fatalf("a running agent was booted out: %v", f.calls)
	}
	if ids, _ := l.Installed(); len(ids) != 0 {
		t.Fatalf("the file is still there: %v", ids)
	}
}

func TestLaunchdInstallWithoutAGUISession(t *testing.T) {
	unixAlarm(t)
	l, f := newFakeLaunchd(t)
	f.refuse = true
	sc := sample(t)
	sc.ID = "abcdefghij"
	if err := l.Install(sc, goldenEntry()); err == nil || !strings.Contains(err.Error(), "signed in to this Mac's desktop") {
		t.Fatalf("Install = %v", err)
	}
}

// The probe is a one-shot agent whose report comes back through a file,
// and is booted out afterwards.
func TestLaunchdProbe(t *testing.T) {
	unixAlarm(t)
	l, f := newFakeLaunchd(t)
	var args []string
	f.onBoot = func(path string) {
		b, _ := os.ReadFile(path)
		s := string(b)
		i := strings.Index(s, "<key>ProgramArguments</key>")
		j := strings.Index(s, "</array>")
		for _, line := range strings.Split(s[i:j], "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "<string>"); ok {
				args = append(args, strings.TrimSuffix(v, "</string>"))
			}
		}
		_ = os.WriteFile(args[len(args)-1], []byte("gap: docker CLI not found\n"), 0o600)
	}
	e := goldenEntry()
	e.Args = []string{"schedule", "probe"}
	out, err := l.Probe(e)
	if err == nil || out != "gap: docker CLI not found" {
		t.Fatalf("Probe = %q %v", out, err)
	}
	if len(args) != 5 || args[3] != "--report" {
		t.Fatalf("probe args %v", args)
	}
	if len(f.loaded) != 0 {
		t.Fatalf("the probe agent was left loaded: %v", f.loaded)
	}
	if _, err := os.Stat(filepath.Dir(args[4])); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the probe's folder was left: %v", err)
	}
	f.onBoot = func(path string) {}
	l.Wait = 300 * time.Millisecond
	if _, err := l.Probe(e); err == nil || !strings.Contains(err.Error(), "did not run agentry") {
		t.Fatalf("a probe that never ran: %v", err)
	}
}
