package schedule

import (
	"errors"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func windowsEntry() Entry {
	return Entry{
		Binary: `C:\Program Files\Agentry\agentry.exe`,
		Args:   []string{"schedule", "fire", "abcdefghij"},
		Env:    []string{`PATH=C:\Windows\system32;C:\Program Files\Docker\Docker\resources\bin`, `AGENTRY_DOCKER=C:\Program Files\Docker\Docker\resources\bin\docker.exe`},
	}
}

const testSID = "S-1-5-21-1111111111-2222222222-3333333333-1001"

// Golden: the task holds the binary, the schedule id, paths, the person's
// SID and the calendar, and nothing else.
func TestTaskXMLGolden(t *testing.T) {
	sc := sample(t)
	sc.ID = "abcdefghij"
	sc.When = Recurrence{Kind: Weekdays, Time: "09:00", Zone: "UTC", Start: sc.Created}
	doc, err := TaskXML(sc, windowsEntry(), testSID, `C:\Windows\System32\conhost.exe`, time.UTC, sc.Created)
	if err != nil {
		t.Fatal(err)
	}
	want := `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Author>agentry schedule</Author>
    <Description>Written by agentry schedule: Morning digest, weekdays at 09:00 (UTC). The schedule itself is abcdefghij in the runner home.</Description>
  </RegistrationInfo>
  <Triggers>
    <CalendarTrigger>
      <StartBoundary>2026-10-08T09:00:00</StartBoundary>
      <ScheduleByWeek>
        <DaysOfWeek>
          <Monday />
          <Tuesday />
          <Wednesday />
          <Thursday />
          <Friday />
        </DaysOfWeek>
        <WeeksInterval>1</WeeksInterval>
      </ScheduleByWeek>
    </CalendarTrigger>
    <LogonTrigger>
      <UserId>S-1-5-21-1111111111-2222222222-3333333333-1001</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>S-1-5-21-1111111111-2222222222-3333333333-1001</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT72H</ExecutionTimeLimit>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>C:\Windows\System32\conhost.exe</Command>
      <Arguments>--headless "C:\Program Files\Agentry\agentry.exe" schedule fire abcdefghij --env "PATH=C:\Windows\system32;C:\Program Files\Docker\Docker\resources\bin" --env "AGENTRY_DOCKER=C:\Program Files\Docker\Docker\resources\bin\docker.exe"</Arguments>
    </Exec>
  </Actions>
</Task>
`
	if doc != want {
		t.Fatalf("task:\n%s\nwant:\n%s", doc, want)
	}
}

// Every day, a month's last day, and a one-off ring for the time the
// spring change skips.
func TestTaskXMLCalendars(t *testing.T) {
	sc := sample(t)
	sc.ID = "abcdefghij"
	ny := mustLoc(t, "America/New_York")
	cases := []struct {
		r    Recurrence
		want []string
	}{
		{Recurrence{Kind: Daily, Time: "02:30", Zone: "America/New_York"}, []string{
			"<ScheduleByDay>\n        <DaysInterval>1</DaysInterval>", "<StartBoundary>2026-10-08T02:30:00</StartBoundary>",
			"<TimeTrigger>\n      <StartBoundary>2027-03-14T03:00:00</StartBoundary>"}},
		{Recurrence{Kind: Monthly, MonthDay: 31, Time: "09:00", Zone: "America/New_York"}, []string{
			"<Day>28</Day>\n          <Day>29</Day>\n          <Day>30</Day>\n          <Day>31</Day>", "<January />", "<December />"}},
		{Recurrence{Kind: Once, Date: "2026-10-20", Time: "08:00", Zone: "America/New_York"}, []string{
			"<TimeTrigger>\n      <StartBoundary>2026-10-20T08:00:00</StartBoundary>"}},
	}
	for _, c := range cases {
		c.r.Start = sc.Created
		sc.When = c.r
		doc, err := TaskXML(sc, windowsEntry(), testSID, `C:\Windows\System32\conhost.exe`, ny, sc.Created)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range c.want {
			if !strings.Contains(doc, w) {
				t.Errorf("%s: task lacks %q:\n%s", c.r.Describe(), w, doc)
			}
		}
		if c.r.Kind == Once && strings.Contains(doc, "CalendarTrigger") {
			t.Errorf("a once schedule repeats:\n%s", doc)
		}
	}
}

func TestTaskXMLEscapes(t *testing.T) {
	sc := sample(t)
	sc.ID = "abcdefghij"
	sc.Name = "a <b>&\n</Description><Exec>"
	e := windowsEntry()
	e.Args = append(e.Args, `he said "hi" \`)
	doc, err := TaskXML(sc, e, testSID, `C:\Windows\System32\conhost.exe`, time.UTC, sc.Created)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(doc, "<Exec>") != 1 || strings.Count(doc, "</Description>") != 1 {
		t.Fatalf("a name broke out:\n%s", doc)
	}
	if !strings.Contains(doc, `"he said \"hi\" \\"`) {
		t.Fatalf("argument not quoted:\n%s", doc)
	}
	e.Binary = "agentry.exe"
	if _, err := TaskXML(sc, e, testSID, `C:\Windows\System32\conhost.exe`, time.UTC, sc.Created); err == nil {
		t.Fatal("a relative binary was accepted")
	}
	if _, err := TaskXML(sc, windowsEntry(), "", `C:\Windows\System32\conhost.exe`, time.UTC, sc.Created); err == nil {
		t.Fatal("a task with no user was accepted")
	}
}

// argvW splits a command line the way CommandLineToArgvW does, for the
// arguments after the program.
func argvW(s string) []string {
	var out []string
	var cur strings.Builder
	in, have := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			n := 0
			for i < len(s) && s[i] == '\\' {
				n++
				i++
			}
			if i < len(s) && s[i] == '"' {
				cur.WriteString(strings.Repeat(`\`, n/2))
				if n%2 == 1 {
					cur.WriteByte('"')
				} else {
					in = !in
				}
				have = true
			} else {
				cur.WriteString(strings.Repeat(`\`, n))
				i--
				have = true
			}
		case c == '"':
			in, have = !in, true
		case (c == ' ' || c == '\t') && !in:
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}

func TestWindowsArgRoundTrips(t *testing.T) {
	words := []string{"plain", "", "two words", `C:\Program Files\x\`, `say "hi"`, `back\\"slash`, `trail\\`, "tab\there", `PATH=C:\a b;C:\c\`}
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = windowsArg(w)
	}
	if got := argvW(strings.Join(quoted, " ")); !reflect.DeepEqual(got, words) {
		t.Fatalf("round trip:\n got %q\nwant %q", got, words)
	}
}

func TestUTF16LE(t *testing.T) {
	b := utf16LE("<é>")
	if b[0] != 0xFF || b[1] != 0xFE {
		t.Fatalf("no byte-order mark: % x", b[:2])
	}
	u := make([]uint16, (len(b)-2)/2)
	for i := range u {
		u[i] = uint16(b[2+2*i]) | uint16(b[3+2*i])<<8
	}
	if s := string(utf16.Decode(u)); s != "<é>" {
		t.Fatalf("decoded %q", s)
	}
}

// fakeSchtasks keeps the tasks schtasks would hold.
type fakeSchtasks struct {
	tasks      map[string]string // name → XML file content
	calls      []string
	refuseRoot bool // the \Agentry\ folder cannot be made
	onRun      func(name string)
}

func (f *fakeSchtasks) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case "/Create":
		name := args[2]
		if f.refuseRoot && strings.HasPrefix(name, taskFolder) {
			return []byte("ERROR: Access is denied."), errors.New("exit status 1")
		}
		b, err := os.ReadFile(args[4])
		if err != nil {
			return nil, err
		}
		f.tasks[name] = string(b)
	case "/Delete":
		if _, ok := f.tasks[args[2]]; !ok {
			return []byte("ERROR: The system cannot find the file specified."), errors.New("exit status 1")
		}
		delete(f.tasks, args[2])
	case "/Query":
		var b strings.Builder
		b.WriteString("\"\\Microsoft\\Windows\\Defrag\\ScheduledDefrag\",\"N/A\",\"Ready\"\r\n")
		for n := range f.tasks {
			b.WriteString("\"" + n + "\",\"10/9/2026 9:00:00 AM\",\"Ready\"\r\n")
		}
		b.WriteString("\"\\agentry-schedule-notanid\",\"N/A\",\"Ready\"\r\n")
		return []byte(b.String()), nil
	case "/Run":
		if f.onRun != nil {
			f.onRun(args[2])
		}
	}
	return nil, nil
}

func newFakeTaskScheduler() (*TaskScheduler, *fakeSchtasks) {
	f := &fakeSchtasks{tasks: map[string]string{}}
	return &TaskScheduler{UserSID: testSID, Conhost: `C:\Windows\System32\conhost.exe`, Loc: time.UTC, Schtasks: f.run,
		Now: func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) }, Wait: 2 * time.Second}, f
}

func TestTaskSchedulerInstallRemove(t *testing.T) {
	w, f := newFakeTaskScheduler()
	sc := sample(t)
	sc.ID = "abcdefghij"
	if err := w.Install(sc, windowsEntry()); err != nil {
		t.Fatal(err)
	}
	doc, ok := f.tasks[`\Agentry\agentry-schedule-abcdefghij`]
	if !ok || !strings.HasPrefix(doc, "\xff\xfe") {
		t.Fatalf("tasks %v", f.tasks)
	}
	ids, err := w.Installed()
	if err != nil || !reflect.DeepEqual(ids, []string{"abcdefghij"}) {
		t.Fatalf("Installed = %v %v", ids, err)
	}
	// A rewrite replaces the task in place.
	if err := w.Install(sc, windowsEntry()); err != nil || len(f.tasks) != 1 {
		t.Fatalf("rewrite: %v %v", err, f.tasks)
	}
	if err := w.Remove(sc.ID); err != nil || len(f.tasks) != 0 {
		t.Fatalf("Remove: %v %v", err, f.tasks)
	}
	// A missing task is not an error.
	if err := w.Remove(sc.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.Remove("../../x"); err == nil {
		t.Fatal("Remove took a path")
	}
}

// Where the person may not make the \Agentry\ folder, the task goes in
// the root, and is still found and removed.
func TestTaskSchedulerRootWhenTheFolderIsRefused(t *testing.T) {
	w, f := newFakeTaskScheduler()
	f.refuseRoot = true
	sc := sample(t)
	sc.ID = "abcdefghij"
	if err := w.Install(sc, windowsEntry()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.tasks[`\agentry-schedule-abcdefghij`]; !ok || len(f.tasks) != 1 {
		t.Fatalf("tasks %v", f.tasks)
	}
	if ids, _ := w.Installed(); !reflect.DeepEqual(ids, []string{"abcdefghij"}) {
		t.Fatalf("Installed = %v", ids)
	}
	// Once the folder can be made, the copy in the root is removed.
	f.refuseRoot = false
	if err := w.Install(sc, windowsEntry()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.tasks[`\Agentry\agentry-schedule-abcdefghij`]; !ok || len(f.tasks) != 1 {
		t.Fatalf("tasks %v", f.tasks)
	}
}

func TestTaskSchedulerProbe(t *testing.T) {
	w, f := newFakeTaskScheduler()
	var ran string
	f.onRun = func(name string) {
		ran = name
		doc := f.tasks[name]
		// The report path is the argument after --report, in the UTF-16 XML.
		u := make([]uint16, (len(doc)-2)/2)
		for i := range u {
			u[i] = uint16(doc[2+2*i]) | uint16(doc[3+2*i])<<8
		}
		s := string(utf16.Decode(u))
		i := strings.Index(s, "<Arguments>") + len("<Arguments>")
		args := argvW(s[i:strings.Index(s, "</Arguments>")])
		for k, a := range args {
			if a == "--report" {
				_ = os.WriteFile(args[k+1], []byte("ok\n"), 0o600)
			}
		}
		if strings.Contains(s, "<Triggers>") {
			t.Error("the probe task has a trigger")
		}
	}
	e := windowsEntry()
	e.Args = []string{"schedule", "probe"}
	if !isWindowsAbs(e.Binary) {
		t.Fatal("isWindowsAbs")
	}
	// The probe needs a binary the running system calls absolute.
	if runtime.GOOS != "windows" {
		e.Binary = "/usr/bin/agentry"
	}
	out, err := w.Probe(e)
	if err != nil || out != "ok" {
		t.Fatalf("Probe = %q %v (calls %v)", out, err, f.calls)
	}
	if !strings.HasPrefix(ran, `\Agentry\agentry-schedule-probe-`) || len(f.tasks) != 0 {
		t.Fatalf("ran %q, left %v", ran, f.tasks)
	}
	if ids, _ := w.Installed(); len(ids) != 0 {
		t.Fatalf("a probe task is listed as a schedule: %v", ids)
	}
}

// A listing that cannot be read whole is an error, never a shorter list:
// remove would leave a task it did not see, and list would call it missing.
func TestTaskSchedulerListingThatCannotBeRead(t *testing.T) {
	w, _ := newFakeTaskScheduler()
	w.Schtasks = func(args ...string) ([]byte, error) {
		return []byte("\"\\Agentry\\agentry-schedule-abcdefghij\",\"N/A\",\"Ready\"\r\n\"\\broken,\"x\r\n"), nil
	}
	if ids, err := w.Installed(); err == nil {
		t.Fatalf("Installed = %v, want an error", ids)
	}
	if err := w.Remove("abcdefghij"); err == nil {
		t.Fatal("Remove went on from a half-read listing")
	}
}
