package schedule

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// taskFolder is the Task Scheduler folder every schedule's task is in.
// Where the person may not make a folder, the task goes in the root,
// under the same name.
const taskFolder = `\Agentry\`

// TaskScheduler is the Windows alarm: a task per schedule that runs as the
// person, only while they are logged on (an interactive token, so
// Credential Manager and Docker Desktop are theirs), and never wakes the
// computer. Its calendar keeps local time, so its triggers are
// LocalRingsFor's superset. A logon trigger and StartWhenAvailable are the
// catch-up for an occurrence missed while the computer was off, asleep or
// signed out; fire decides whether it still runs.
//
// A task cannot set its program's environment, so the entry's Env travels
// as `--env KEY=VALUE` arguments, which fire and probe apply first; and it
// runs through `conhost --headless`, so no console window opens on the
// person's screen at every ring.
type TaskScheduler struct {
	// UserSID is the person's security identifier, the task's principal.
	UserSID string
	// Conhost is the absolute path of conhost.exe.
	Conhost string
	// Loc is this computer's zone, the one the triggers keep.
	Loc *time.Location
	// Schtasks runs schtasks.exe with args and returns its output;
	// replaced in tests.
	Schtasks func(args ...string) ([]byte, error)
	// Now is the clock the triggers are compiled from; replaced in tests.
	Now func() time.Time
	// Wait is how long Probe waits for its report.
	Wait time.Duration
}

// NewTaskScheduler is the alarm for the person running this process.
func NewTaskScheduler() (*TaskScheduler, error) {
	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("who is running agentry: %w", err)
	}
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	sys := filepath.Join(root, "System32")
	return &TaskScheduler{
		UserSID: u.Uid,
		Conhost: filepath.Join(sys, "conhost.exe"),
		Loc:     time.Local,
		Schtasks: func(args ...string) ([]byte, error) {
			return exec.Command(filepath.Join(sys, "schtasks.exe"), args...).CombinedOutput()
		},
		Now:  time.Now,
		Wait: 2 * time.Minute,
	}, nil
}

// Name implements Alarm.
func (*TaskScheduler) Name() string { return "Task Scheduler task" }

// taskNames are where a schedule's task may be: the folder first.
func taskNames(id string) []string { return taskPlaces(unitPrefix + id) }

// taskPlaces are the two places a task named leaf may be: the folder
// first, the root where the folder cannot be made.
func taskPlaces(leaf string) []string { return []string{taskFolder + leaf, `\` + leaf} }

// Install implements Alarm: the task in \Agentry\, or in the root where
// the folder cannot be made, and none left in the other place.
func (ts *TaskScheduler) Install(sc Schedule, e Entry) error {
	if !ValidID(sc.ID) {
		return fmt.Errorf("%q is not a schedule id", sc.ID)
	}
	doc, err := TaskXML(sc, e, ts.UserSID, ts.Conhost, ts.Loc, ts.Now())
	if err != nil {
		return err
	}
	names := taskNames(sc.ID)
	at, err := ts.create(names, doc)
	if err != nil {
		return err
	}
	// The other place may hold an older copy (the folder became writable,
	// or stopped being so): one task per schedule.
	have, _ := ts.installedNames()
	for _, n := range names {
		if n != at && have[n] {
			_, _ = ts.Schtasks("/Delete", "/TN", n, "/F")
		}
	}
	return nil
}

// create registers doc under the first of names that takes it, and says
// which.
func (ts *TaskScheduler) create(names []string, doc string) (string, error) {
	f, err := os.CreateTemp("", "agentry-schedule-*.xml")
	if err != nil {
		return "", err
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()
	_, werr := f.Write(utf16LE(doc))
	if cerr := f.Close(); werr != nil || cerr != nil {
		return "", errors.Join(werr, cerr)
	}
	var errs []error
	for _, n := range names {
		out, err := ts.Schtasks("/Create", "/TN", n, "/XML", path, "/F")
		if err == nil {
			return n, nil
		}
		errs = append(errs, fmt.Errorf("schtasks /Create %s: %v: %s", n, err, strings.TrimSpace(string(out))))
	}
	return "", errors.Join(errs...)
}

// Remove implements Alarm.
func (ts *TaskScheduler) Remove(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("%q is not a schedule id", id)
	}
	have, err := ts.installedNames()
	if err != nil {
		return err
	}
	var errs []error
	for _, n := range taskNames(id) {
		if !have[n] {
			continue
		}
		if out, err := ts.Schtasks("/Delete", "/TN", n, "/F"); err != nil {
			errs = append(errs, fmt.Errorf("schtasks /Delete %s: %v: %s", n, err, strings.TrimSpace(string(out))))
		}
	}
	return errors.Join(errs...)
}

// taskNamePattern is a schedule's task as schtasks lists it.
var taskNamePattern = regexp.MustCompile(`^(?:` + regexp.QuoteMeta(strings.TrimSuffix(taskFolder, `\`)) + `)?\\` +
	regexp.QuoteMeta(unitPrefix) + `(` + idExpr + `)$`)

// Installed implements Alarm.
func (ts *TaskScheduler) Installed() ([]string, error) {
	have, err := ts.installedNames()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var ids []string
	for n := range have {
		if m := taskNamePattern.FindStringSubmatch(n); m != nil && !seen[m[1]] {
			seen[m[1]] = true
			ids = append(ids, m[1])
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// installedNames is every task schtasks lists whose name is a schedule's.
// The listing's first column is the task's full name in every language.
func (ts *TaskScheduler) installedNames() (map[string]bool, error) {
	out, err := ts.Schtasks("/Query", "/FO", "CSV", "/NH")
	if err != nil {
		return nil, fmt.Errorf("schtasks /Query: %v: %s", err, strings.TrimSpace(string(out)))
	}
	r := csv.NewReader(bytes.NewReader(out))
	r.FieldsPerRecord = -1
	have := map[string]bool{}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A listing read half way would call a task missing that is
			// there: remove would leave it, and list would say "no alarm".
			return nil, fmt.Errorf("read schtasks' list of tasks: %w", err)
		}
		if len(rec) > 0 && taskNamePattern.MatchString(rec[0]) {
			have[rec[0]] = true
		}
	}
	return have, nil
}

// Probe implements Alarm: a task with no trigger, started once, in the
// person's logged-on session, removed afterwards.
func (ts *TaskScheduler) Probe(e Entry) (string, error) {
	return probeWithReport(e, ts.Wait, func(e Entry, _ string) (func(), error) {
		doc, err := taskDoc("Agentry schedule probe, removed when it has run.", e, ts.UserSID, ts.Conhost, nil)
		if err != nil {
			return nil, err
		}
		at, err := ts.create(taskPlaces(unitPrefix+"probe-"+strconv.FormatInt(ts.Now().UnixNano(), 36)), doc)
		if err != nil {
			return nil, err
		}
		cleanup := func() { _, _ = ts.Schtasks("/Delete", "/TN", at, "/F") }
		if out, err := ts.Schtasks("/Run", "/TN", at); err != nil {
			cleanup()
			return nil, fmt.Errorf("schtasks /Run %s: %v: %s", at, err, strings.TrimSpace(string(out)))
		}
		return cleanup, nil
	})
}

// TaskXML is sc's task, as Task Scheduler's XML (before its UTF-16
// encoding).
func TaskXML(sc Schedule, e Entry, userSID, conhost string, loc *time.Location, now time.Time) (string, error) {
	rings, err := LocalRingsFor(sc.When, loc, now)
	if err != nil {
		return "", err
	}
	// A repeating trigger's start is a day already begun; only its time
	// of day and its days are read.
	day := now.In(loc)
	var t strings.Builder
	boundary := func(h, m int) string {
		return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, loc).Format("2006-01-02T15:04:05")
	}
	for _, r := range rings.Repeat {
		fmt.Fprintf(&t, "    <CalendarTrigger>\n      <StartBoundary>%s</StartBoundary>\n", boundary(r.Hour, r.Minute))
		switch {
		case len(r.MonthDays) > 0:
			t.WriteString("      <ScheduleByMonth>\n        <DaysOfMonth>\n")
			for _, d := range r.MonthDays {
				fmt.Fprintf(&t, "          <Day>%d</Day>\n", d)
			}
			t.WriteString("        </DaysOfMonth>\n        <Months>\n")
			for m := time.January; m <= time.December; m++ {
				fmt.Fprintf(&t, "          <%s />\n", m)
			}
			t.WriteString("        </Months>\n      </ScheduleByMonth>\n")
		case len(r.Weekdays) > 0:
			t.WriteString("      <ScheduleByWeek>\n        <DaysOfWeek>\n")
			for _, d := range r.Weekdays {
				fmt.Fprintf(&t, "          <%s />\n", d)
			}
			t.WriteString("        </DaysOfWeek>\n        <WeeksInterval>1</WeeksInterval>\n      </ScheduleByWeek>\n")
		default:
			t.WriteString("      <ScheduleByDay>\n        <DaysInterval>1</DaysInterval>\n      </ScheduleByDay>\n")
		}
		t.WriteString("    </CalendarTrigger>\n")
	}
	for _, at := range rings.Once {
		fmt.Fprintf(&t, "    <TimeTrigger>\n      <StartBoundary>%s</StartBoundary>\n    </TimeTrigger>\n",
			at.In(loc).Format("2006-01-02T15:04:05"))
	}
	// At sign-in: the catch-up for what fell due while signed out or off.
	fmt.Fprintf(&t, "    <LogonTrigger>\n      <UserId>%s</UserId>\n    </LogonTrigger>\n", xmlText(userSID))
	desc := fmt.Sprintf("Written by agentry schedule: %s, %s. The schedule itself is %s in the runner home.",
		singleLine(sc.Name), singleLine(sc.When.Describe()), sc.ID)
	return taskDoc(desc, e, userSID, conhost, []byte(t.String()))
}

// taskDoc is a task document around its triggers.
func taskDoc(desc string, e Entry, userSID, conhost string, triggers []byte) (string, error) {
	if e.Credential != "" {
		return "", ErrSignedOutUnsupported
	}
	if !filepath.IsAbs(e.Binary) && !isWindowsAbs(e.Binary) {
		return "", fmt.Errorf("the agentry binary %q is not an absolute path", e.Binary)
	}
	if userSID == "" {
		return "", errors.New("no user to run the task as")
	}
	words := []string{"--headless", e.Binary}
	words = append(words, e.Args...)
	for _, kv := range e.Env {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" {
			return "", fmt.Errorf("environment entry %q is not KEY=VALUE", kv)
		}
		words = append(words, "--env", kv)
	}
	args := make([]string, len(words))
	for i, w := range words {
		if strings.ContainsAny(w, "\x00\r\n") {
			return "", fmt.Errorf("argument %q holds a line break or a NUL, which a task's command line cannot carry", w)
		}
		args[i] = windowsArg(w)
	}
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-16\"?>\n")
	b.WriteString("<Task version=\"1.2\" xmlns=\"http://schemas.microsoft.com/windows/2004/02/mit/task\">\n")
	fmt.Fprintf(&b, "  <RegistrationInfo>\n    <Author>agentry schedule</Author>\n    <Description>%s</Description>\n  </RegistrationInfo>\n", xmlText(desc))
	if len(triggers) > 0 {
		b.WriteString("  <Triggers>\n")
		b.Write(triggers)
		b.WriteString("  </Triggers>\n")
	}
	fmt.Fprintf(&b, `  <Principals>
    <Principal id="Author">
      <UserId>%s</UserId>
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
      <Command>%s</Command>
      <Arguments>%s</Arguments>
    </Exec>
  </Actions>
</Task>
`, xmlText(userSID), xmlText(conhost), xmlText(strings.Join(args, " ")))
	return b.String(), nil
}

// windowsArg quotes one argument the way CommandLineToArgvW and the Go
// runtime read it back: in double quotes when it has a space, a tab or a
// quote, with each run of backslashes before a quote, or before the
// closing quote, doubled.
func windowsArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes+1))
			slashes = 0
		default:
			slashes = 0
		}
		b.WriteByte(c)
	}
	b.WriteString(strings.Repeat(`\`, slashes))
	b.WriteByte('"')
	return b.String()
}

// isWindowsAbs reports a drive-absolute Windows path (C:\…), which
// filepath.IsAbs on another system does not know.
func isWindowsAbs(p string) bool {
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z'))
}

// utf16LE is s as UTF-16 little-endian with its byte-order mark, the
// encoding Task Scheduler writes and reads its XML in.
func utf16LE(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2+2*len(u))
	b[0], b[1] = 0xFF, 0xFE
	for i, c := range u {
		b[2+2*i], b[3+2*i] = byte(c), byte(c>>8)
	}
	return b
}
