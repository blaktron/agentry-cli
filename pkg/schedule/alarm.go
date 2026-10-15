package schedule

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Alarm is an operating system's own scheduler, which wakes `agentry
// schedule fire <id>` when a schedule may be due. It is only an alarm: a
// fire re-reads the store and decides, so an alarm that rings early, late,
// twice or for a removed schedule does no harm, and one may ring on a
// superset of the occurrences when the system cannot say exactly what the
// schedule means.
type Alarm interface {
	// Name is the system's scheduler, for messages ("systemd user timer").
	Name() string
	// Install writes, or rewrites, the entry for sc.
	Install(sc Schedule, e Entry) error
	// Remove deletes the entry for id; a missing one is not an error.
	Remove(id string) error
	// Installed lists the schedule ids that have an entry.
	Installed() ([]string, error)
	// Probe runs e once now in the context the alarm will run it in (the
	// same user manager, groups and environment) and returns its output,
	// so a schedule is refused when made rather than failing at its first
	// fire: a shell can reach what the scheduler cannot.
	Probe(e Entry) (string, error)
}

// Entry is what every alarm runs: Binary with Args, under Env. Env holds
// paths and names only, never a credential.
type Entry struct {
	// Binary is the absolute path of the agentry that wrote the entry.
	Binary string
	// Args are the arguments after it (schedule fire <id>).
	Args []string
	// Env is KEY=VALUE pairs: PATH and AGENTRY_DOCKER as resolved when the
	// entry was written, so the system's own minimal PATH cannot hide
	// docker (the launchd and Windows first-launch PATH lessons).
	Env []string
	// Credential is a signed-out schedule's encrypted credential, which
	// the unit loads with LoadCredentialEncrypted= (Linux only; the other
	// alarms refuse an entry that names one).
	Credential string
}

// ErrUnsupported is a system this build has no alarm for yet.
var ErrUnsupported = errors.New("scheduling runs on this computer is not available on this system yet")

// ForThisSystem is the alarm for the running system.
func ForThisSystem() (Alarm, error) {
	switch runtime.GOOS {
	case "linux":
		return NewSystemd()
	case "darwin":
		return NewLaunchd()
	case "windows":
		return NewTaskScheduler()
	}
	return nil, fmt.Errorf("%s: %w", runtime.GOOS, ErrUnsupported)
}

// unitPrefix names every entry this package writes, so Installed and a
// sync can tell its own from the person's.
const unitPrefix = "agentry-schedule-"

// Systemd is the Linux alarm: a systemd user timer and service per
// schedule, in the person's own manager (no root, and the session's bus so
// the keychain is reachable while they are signed in). Cron is not used:
// a cron job has no session bus.
type Systemd struct {
	// Dir is the user unit folder (~/.config/systemd/user).
	Dir string
	// Systemctl runs `systemctl --user` with args; replaced in tests.
	Systemctl func(args ...string) error
	// SystemdRun runs `systemd-run --user` with args and returns its
	// output; replaced in tests.
	SystemdRun func(args ...string) ([]byte, error)
	// Running reports whether a unit is starting, running or stopping;
	// nil is never. Replaced in tests.
	Running func(unit string) bool
}

// NewSystemd is the alarm for the person running this process.
func NewSystemd() (*Systemd, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		base = filepath.Join(home, ".config")
	}
	return &Systemd{
		Dir: filepath.Join(base, "systemd", "user"),
		Systemctl: func(args ...string) error {
			out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("systemctl --user %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		SystemdRun: func(args ...string) ([]byte, error) {
			return exec.Command("systemd-run", append([]string{"--user"}, args...)...).CombinedOutput()
		},
		Running: func(unit string) bool {
			out, err := exec.Command("systemctl", "--user", "show", "-p", "ActiveState", "--value", unit).Output()
			switch strings.TrimSpace(string(out)) {
			case "activating", "active", "deactivating", "reloading", "refreshing":
				return err == nil
			}
			return false
		},
	}, nil
}

// Name implements Alarm.
func (*Systemd) Name() string { return "systemd user timer" }

func unitName(id string) string { return unitPrefix + id }

// Install implements Alarm.
func (s *Systemd) Install(sc Schedule, e Entry) error {
	if !ValidID(sc.ID) {
		return fmt.Errorf("%q is not a schedule id", sc.ID)
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	service, timer, err := SystemdUnits(sc, e)
	if err != nil {
		return err
	}
	n := unitName(sc.ID)
	if err := writeAtomic(filepath.Join(s.Dir, n+".service"), []byte(service), 0o644); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(s.Dir, n+".timer"), []byte(timer), 0o644); err != nil {
		return err
	}
	// A file a fire left while its service ran goes once it has stopped.
	_ = s.removeIdleServices()
	if err := s.Systemctl("daemon-reload"); err != nil {
		return err
	}
	// restart, not start: a rewritten timer takes its new calendar now.
	if err := s.Systemctl("enable", n+".timer"); err != nil {
		return err
	}
	return s.Systemctl("restart", n+".timer")
}

// Remove implements Alarm. The timer goes at once. The service's file stays
// while the service runs: a fire that ends its own schedule removes its
// alarm from inside that service, and a reload that finds a running unit's
// file gone reloads it with the manager's defaults, among them a start
// timeout (45 s on Fedora) that stops a run already longer than that. Such
// a file, inert without its timer, is swept by the next Install or Remove
// once its service has stopped.
func (s *Systemd) Remove(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("%q is not a schedule id", id)
	}
	n := unitName(id)
	timerPath := filepath.Join(s.Dir, n+".timer")
	if _, err := os.Stat(timerPath); err == nil {
		// A timer the manager no longer knows is not worth failing over.
		_ = s.Systemctl("disable", "--now", n+".timer")
	}
	var errs []error
	if err := os.Remove(timerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	errs = append(errs, s.removeIdleServices())
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return s.Systemctl("daemon-reload")
}

// removeIdleServices removes every schedule service file that has no timer
// and whose service is not running.
func (s *Systemd) removeIdleServices() error {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, unitPrefix) || !strings.HasSuffix(name, ".service") {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.Dir, strings.TrimSuffix(name, ".service")+".timer")); err == nil {
			continue
		}
		if s.Running != nil && s.Running(name) {
			continue
		}
		if err := os.Remove(filepath.Join(s.Dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Installed implements Alarm.
func (s *Systemd) Installed() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, unitPrefix) || !strings.HasSuffix(name, ".timer") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, unitPrefix), ".timer")
		if ValidID(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// Probe implements Alarm: a transient unit in the person's own manager,
// waited for, its output piped back.
func (s *Systemd) Probe(e Entry) (string, error) {
	if !filepath.IsAbs(e.Binary) {
		return "", fmt.Errorf("the agentry binary %q is not an absolute path", e.Binary)
	}
	args := []string{"--wait", "--pipe", "--collect", "--quiet"}
	if e.Credential != "" {
		cred, err := loadCredential(e.Credential)
		if err != nil {
			return "", err
		}
		args = append(args, "--property="+cred)
	}
	for _, kv := range e.Env {
		args = append(args, "--setenv="+kv)
	}
	args = append(args, "--", e.Binary)
	args = append(args, e.Args...)
	out, err := s.SystemdRun(args...)
	return strings.TrimSpace(string(out)), err
}

// SystemdUnits is the service and the timer for sc, as text.
func SystemdUnits(sc Schedule, e Entry) (service, timer string, err error) {
	if !filepath.IsAbs(e.Binary) {
		return "", "", fmt.Errorf("the agentry binary %q is not an absolute path", e.Binary)
	}
	cal, err := OnCalendar(sc.When, time.Now())
	if err != nil {
		return "", "", err
	}
	desc := "Agentry schedule: " + oneLine(sc.Name)
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by agentry schedule; the schedule itself is %s in the runner home.\n", sc.ID)
	fmt.Fprintf(&b, "[Unit]\nDescription=%s\n\n[Service]\nType=oneshot\n", desc)
	if e.Credential != "" {
		cred, err := loadCredential(e.Credential)
		if err != nil {
			return "", "", err
		}
		// The keys a signed-out fire reads: systemd decrypts them into
		// $CREDENTIALS_DIRECTORY for this service alone.
		fmt.Fprintf(&b, "%s\n", strings.ReplaceAll(cred, "%", "%%"))
	}
	for _, kv := range e.Env {
		if strings.ContainsAny(kv, "\n\r") || !strings.Contains(kv, "=") {
			return "", "", fmt.Errorf("environment entry %q is not KEY=VALUE on one line", kv)
		}
		fmt.Fprintf(&b, "Environment=%s\n", envQuote(kv))
	}
	words := append([]string{e.Binary}, e.Args...)
	quoted := make([]string, len(words))
	for i, w := range words {
		if strings.ContainsAny(w, "\n\r") {
			return "", "", fmt.Errorf("argument %q spans lines", w)
		}
		quoted[i] = systemdQuote(w)
	}
	fmt.Fprintf(&b, "ExecStart=%s\n", strings.Join(quoted, " "))
	service = b.String()

	b.Reset()
	fmt.Fprintf(&b, "# Written by agentry schedule: %s.\n", oneLine(sc.When.Describe()))
	fmt.Fprintf(&b, "[Unit]\nDescription=%s\n\n[Timer]\n", desc)
	for _, c := range cal {
		fmt.Fprintf(&b, "OnCalendar=%s\n", c)
	}
	// Persistent: an occurrence missed while the computer was off rings
	// at the next boot, and fire decides whether it still runs.
	fmt.Fprintf(&b, "Persistent=true\nAccuracySec=1min\nUnit=%s.service\n\n[Install]\nWantedBy=timers.target\n", unitName(sc.ID))
	return service, b.String(), nil
}

// OnCalendar is r as systemd calendar expressions in r's zone. They ring
// on every occurrence, and for a month day past the 28th on every day
// from the 28th on, which fire narrows to the month's own day (a month
// without the 31st runs on its last day). The end of a repeat is fire's.
//
// systemd does not ring at a wall time the spring change skips (checked
// with systemd-analyze 259: 02:30 New York on 2027-03-14 never elapses),
// where the schedule's rule runs the occurrence at the first valid minute
// after it. Each such occurrence in the three years from now gets a
// one-off ring of its own at that instant, in UTC; every sync and every
// add or edit writes them afresh.
func OnCalendar(r Recurrence, now time.Time) ([]string, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	at := r.Time + ":00 " + r.Zone
	var cal []string
	switch r.Kind {
	case Once:
		cal = []string{r.Date + " " + at}
	case Daily:
		cal = []string{"*-*-* " + at}
	case Weekdays:
		cal = []string{"Mon..Fri *-*-* " + at}
	case Weekly:
		days := make([]string, len(r.Days))
		for i, d := range r.Days {
			days[i] = strings.ToUpper(d[:1]) + d[1:]
		}
		cal = []string{strings.Join(days, ",") + " *-*-* " + at}
	case Monthly:
		if r.MonthDay <= 28 {
			cal = []string{fmt.Sprintf("*-*-%02d %s", r.MonthDay, at)}
		} else {
			cal = []string{"*-*-28..31 " + at}
		}
	default:
		return nil, fmt.Errorf("unknown repeat %q", r.Kind)
	}
	return append(cal, skippedTimeRings(r, now)...), nil
}

// skippedTimeRings are one-off rings, in UTC, for the occurrences in the
// three years from now whose wall time the spring change skips.
func skippedTimeRings(r Recurrence, now time.Time) []string {
	loc, err := time.LoadLocation(r.Zone)
	if err != nil {
		return nil
	}
	var rings []string
	for _, t := range r.Between(now, now.AddDate(3, 0, 0), 0) {
		if t.In(loc).Format("15:04") != r.Time {
			rings = append(rings, t.UTC().Format("2006-01-02 15:04:05")+" UTC")
		}
	}
	return rings
}

// envQuote quotes one Environment= assignment: systemd expands no
// variables there, so only backslash, quote and the specifier sign are
// escaped.
func envQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`)
	return `"` + r.Replace(s) + `"`
}

// systemdQuote quotes one ExecStart word: double quotes, with
// backslash, quote, dollar and percent escaped, so no word is split,
// expanded as a variable or read as a specifier.
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`, `%`, `%%`)
	return `"` + r.Replace(s) + `"`
}

func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r < 0x20 {
			return ' '
		}
		return r
	}, s)
	return strings.ReplaceAll(s, "%", "%%")
}

// probeWithReport runs a probe through an alarm that pipes nothing back
// (launchd, Task Scheduler): start registers and starts e, given a
// folder of its own; the probe writes its result to a report file there,
// which this waits for; start's cleanup and the folder are removed after.
func probeWithReport(e Entry, wait time.Duration, start func(e Entry, dir string) (cleanup func(), err error)) (string, error) {
	if !filepath.IsAbs(e.Binary) {
		return "", fmt.Errorf("the agentry binary %q is not an absolute path", e.Binary)
	}
	dir, err := os.MkdirTemp("", "agentry-schedule-probe-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	report := filepath.Join(dir, "report.txt")
	e.Args = append(append([]string{}, e.Args...), "--report", report)
	cleanup, err := start(e, dir)
	if err != nil {
		return "", err
	}
	defer cleanup()
	return waitReport(report, wait)
}

// waitReport waits for the probe's report file and returns it. The probe
// writes it in one rename when it is done, ending in "ok" or its gaps; an
// exit with gaps is an error, as a failed systemd-run is.
func waitReport(path string, wait time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	for {
		b, err := os.ReadFile(path)
		if err == nil {
			out := strings.TrimSpace(string(b))
			lines := strings.Split(out, "\n")
			if lines[len(lines)-1] == "ok" {
				return out, nil
			}
			return out, errors.New("the probe found gaps")
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("the scheduler did not run agentry within %s", wait)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// xmlTextEscaper escapes what an XML text node cannot hold as it is;
// quotes need no escape there, and stay readable in a golden file.
var xmlTextEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\r", "&#xD;", "\n", "&#xA;")

// xmlText escapes s for an XML text node, and replaces a character XML
// cannot carry at all with U+FFFD.
func xmlText(s string) string {
	s = strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\t' && r != '\n' && r != '\r') || r == 0xFFFE || r == 0xFFFF {
			return '\uFFFD'
		}
		return r
	}, s)
	return xmlTextEscaper.Replace(s)
}

// singleLine is s with its control characters made spaces, for a
// description or a comment that must stay on one line.
func singleLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 {
			return ' '
		}
		return r
	}, s)
}
