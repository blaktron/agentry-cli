package schedule

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// labelPrefix names every LaunchAgent this package writes.
const labelPrefix = "net.secureagentry.schedule."

// Launchd is the macOS alarm: a LaunchAgent per schedule in the person's
// own GUI session (gui/<uid>), so the login keychain and Docker Desktop
// are there, and it rings only while they are signed in. Its
// StartCalendarInterval keeps local time, so its rings are LocalRingsFor's
// superset. RunAtLoad rings once at every sign-in, which is the catch-up
// for an occurrence missed while the Mac was off; one missed while it slept
// rings at the wake, which launchd coalesces into one.
type Launchd struct {
	// Dir is ~/Library/LaunchAgents.
	Dir string
	// UID is the person's user id, the GUI domain's name.
	UID int
	// Loc is this computer's zone, the one launchd's calendar keeps.
	Loc *time.Location
	// Launchctl runs launchctl with args and returns its output; replaced
	// in tests.
	Launchctl func(args ...string) ([]byte, error)
	// Now is the clock the rings are compiled from; replaced in tests.
	Now func() time.Time
	// Wait is how long Probe waits for its report.
	Wait time.Duration
}

// NewLaunchd is the alarm for the person running this process.
func NewLaunchd() (*Launchd, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Launchd{
		Dir: filepath.Join(home, "Library", "LaunchAgents"),
		UID: os.Getuid(),
		Loc: time.Local,
		Launchctl: func(args ...string) ([]byte, error) {
			return exec.Command("/bin/launchctl", args...).CombinedOutput()
		},
		Now:  time.Now,
		Wait: 2 * time.Minute,
	}, nil
}

// Name implements Alarm.
func (*Launchd) Name() string { return "LaunchAgent" }

func launchdLabel(id string) string { return labelPrefix + id }

func (l *Launchd) plistPath(id string) string {
	return filepath.Join(l.Dir, launchdLabel(id)+".plist")
}

func (l *Launchd) domain() string { return "gui/" + strconv.Itoa(l.UID) }

func (l *Launchd) target(id string) string { return l.domain() + "/" + launchdLabel(id) }

// Install implements Alarm. An unchanged agent that is loaded is left
// alone, and a changed one is not reloaded while it runs: bootout would
// stop the run mid-way. sync, which the desktop runs at every start,
// writes it once the run is over.
func (l *Launchd) Install(sc Schedule, e Entry) error {
	if !ValidID(sc.ID) {
		return fmt.Errorf("%q is not a schedule id", sc.ID)
	}
	b, err := LaunchAgentPlist(sc, e, l.Loc, l.Now())
	if err != nil {
		return err
	}
	path := l.plistPath(sc.ID)
	old, rerr := os.ReadFile(path)
	state, loaded := l.state(sc.ID)
	if rerr == nil && bytes.Equal(old, b) && loaded {
		return nil
	}
	if loaded && state == "running" {
		return errors.New("it is running now: `agentry schedule sync` rewrites it once the run is over")
	}
	if err := os.MkdirAll(l.Dir, 0o755); err != nil {
		return err
	}
	if err := writeAtomic(path, b, 0o644); err != nil {
		return err
	}
	if loaded {
		if out, err := l.Launchctl("bootout", l.target(sc.ID)); err != nil {
			return fmt.Errorf("launchctl bootout %s: %v: %s", l.target(sc.ID), err, strings.TrimSpace(string(out)))
		}
	}
	return l.bootstrap(path)
}

// bootstrap loads the agent at path into the person's GUI session.
func (l *Launchd) bootstrap(path string) error {
	if out, err := l.Launchctl("bootstrap", l.domain(), path); err != nil {
		return fmt.Errorf("launchctl bootstrap %s (a LaunchAgent needs you signed in to this Mac's desktop): %v: %s",
			l.domain(), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// state is the agent's state as launchd reports it ("running",
// "not running"), and whether it is loaded at all.
func (l *Launchd) state(id string) (string, bool) {
	out, err := l.Launchctl("print", l.target(id))
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if s, ok := strings.CutPrefix(strings.TrimSpace(line), "state = "); ok {
			return s, true
		}
	}
	return "", true
}

// Remove implements Alarm. The file goes first, so an agent stopped half
// way is not loaded again at the next sign-in. A running agent is not
// booted out, since launchd would stop its run before the run is
// recorded: neither a fire that ends its own schedule nor a pause or a
// removal made while a run is going. Such an agent stays loaded until the
// person signs out, ringing a fire that finds the schedule paused, ended
// or gone and runs nothing.
func (l *Launchd) Remove(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("%q is not a schedule id", id)
	}
	if err := os.Remove(l.plistPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if os.Getenv("XPC_SERVICE_NAME") == launchdLabel(id) {
		return nil
	}
	state, loaded := l.state(id)
	if !loaded || state == "running" {
		return nil
	}
	if out, err := l.Launchctl("bootout", l.target(id)); err != nil {
		return fmt.Errorf("launchctl bootout %s: %v: %s", l.target(id), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Installed implements Alarm.
func (l *Launchd) Installed() ([]string, error) {
	entries, err := os.ReadDir(l.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, labelPrefix) || !strings.HasSuffix(name, ".plist") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, labelPrefix), ".plist")
		if ValidID(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// Probe implements Alarm: a one-shot agent in the same GUI session, run
// at load, removed afterwards.
func (l *Launchd) Probe(e Entry) (string, error) {
	return probeWithReport(e, l.Wait, func(e Entry, dir string) (func(), error) {
		label := labelPrefix + "probe." + strconv.FormatInt(l.Now().UnixNano(), 36)
		b, err := plistDoc(label, "Agentry schedule probe, removed when it has run.", e, nil)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, label+".plist")
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return nil, err
		}
		if err := l.bootstrap(path); err != nil {
			return nil, err
		}
		return func() { _, _ = l.Launchctl("bootout", l.domain()+"/"+label) }, nil
	})
}

// LaunchAgentPlist is sc's LaunchAgent, as XML.
func LaunchAgentPlist(sc Schedule, e Entry, loc *time.Location, now time.Time) ([]byte, error) {
	rings, err := LocalRingsFor(sc.When, loc, now)
	if err != nil {
		return nil, err
	}
	cal := []calendarInterval{}
	for _, r := range rings.Repeat {
		switch {
		case len(r.MonthDays) > 0:
			for _, d := range r.MonthDays {
				cal = append(cal, calendarInterval{day: d, weekday: -1, hour: r.Hour, minute: r.Minute})
			}
		case len(r.Weekdays) > 0:
			for _, d := range r.Weekdays {
				cal = append(cal, calendarInterval{weekday: int(d), hour: r.Hour, minute: r.Minute})
			}
		default:
			cal = append(cal, calendarInterval{weekday: -1, hour: r.Hour, minute: r.Minute})
		}
	}
	// launchd has no year: a one-off ring rings on that date every year,
	// and fire's own date check runs only the occurrence that is due.
	for _, t := range rings.Once {
		cal = append(cal, calendarInterval{month: int(t.Month()), day: t.Day(), weekday: -1, hour: t.Hour(), minute: t.Minute()})
	}
	comment := fmt.Sprintf("Written by agentry schedule: %s, %s. The schedule itself is %s in the runner home.",
		singleLine(sc.Name), singleLine(sc.When.Describe()), sc.ID)
	return plistDoc(launchdLabel(sc.ID), comment, e, cal)
}

// calendarInterval is one StartCalendarInterval entry; a zero month or
// day and a negative weekday are left out, so launchd reads them as any.
type calendarInterval struct{ month, day, weekday, hour, minute int }

// plistDoc writes the agent: its label, program and environment, its
// calendar (none for a nil cal), and RunAtLoad.
func plistDoc(label, comment string, e Entry, cal []calendarInterval) ([]byte, error) {
	if !filepath.IsAbs(e.Binary) {
		return nil, fmt.Errorf("the agentry binary %q is not an absolute path", e.Binary)
	}
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
`)
	// A comment ends at "--", and is escaped like text so a name never
	// reads as markup.
	fmt.Fprintf(&b, "<!-- %s -->\n<plist version=\"1.0\">\n<dict>\n", strings.ReplaceAll(xmlText(comment), "--", "- -"))
	key := func(k string) { fmt.Fprintf(&b, "\t<key>%s</key>\n", xmlText(k)) }
	str := func(indent, s string) { fmt.Fprintf(&b, "%s<string>%s</string>\n", indent, xmlText(s)) }
	key("Label")
	str("\t", label)
	key("ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, a := range append([]string{e.Binary}, e.Args...) {
		str("\t\t", a)
	}
	b.WriteString("\t</array>\n")
	if len(e.Env) > 0 {
		key("EnvironmentVariables")
		b.WriteString("\t<dict>\n")
		for _, kv := range e.Env {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return nil, fmt.Errorf("environment entry %q is not KEY=VALUE", kv)
			}
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n", xmlText(k))
			str("\t\t", v)
		}
		b.WriteString("\t</dict>\n")
	}
	if cal != nil {
		key("StartCalendarInterval")
		b.WriteString("\t<array>\n")
		for _, c := range cal {
			b.WriteString("\t\t<dict>\n")
			field := func(k string, v int) {
				fmt.Fprintf(&b, "\t\t\t<key>%s</key>\n\t\t\t<integer>%d</integer>\n", k, v)
			}
			if c.month > 0 {
				field("Month", c.month)
			}
			if c.day > 0 {
				field("Day", c.day)
			}
			if c.weekday >= 0 {
				field("Weekday", c.weekday)
			}
			field("Hour", c.hour)
			field("Minute", c.minute)
			b.WriteString("\t\t</dict>\n")
		}
		b.WriteString("\t</array>\n")
	}
	// At load: the sign-in catch-up, and the probe's one run.
	key("RunAtLoad")
	b.WriteString("\t<true/>\n")
	key("ProcessType")
	str("\t", "Background")
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes(), nil
}
