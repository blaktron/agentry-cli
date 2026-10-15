// Package schedule keeps the schedules of runs on this computer: when a
// QuickAgent runs again, the store that is the truth about it, the record of
// every occurrence, and the operating system's own entries that wake `agentry
// schedule fire` at the right moments.
//
// A schedule's repeat is a small struct, never cron text, so one type
// compiles to every system's trigger and to the times a calendar shows. Times
// are wall-clock in a named zone: "9am" stays 9am across a daylight saving
// change.
package schedule

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Kind is the shape of a repeat.
type Kind string

const (
	// Once runs at one date and time.
	Once Kind = "once"
	// Daily runs every day at a time.
	Daily Kind = "daily"
	// Weekdays runs Monday to Friday at a time.
	Weekdays Kind = "weekdays"
	// Weekly runs on chosen days of the week at a time.
	Weekly Kind = "weekly"
	// Monthly runs on one day of the month at a time; a day the month does
	// not have runs on its last day.
	Monthly Kind = "monthly"
)

// EndKind is how a repeat ends.
type EndKind string

const (
	// EndNever repeats until the schedule is removed.
	EndNever EndKind = "never"
	// EndOn repeats until a date, that date's occurrence included.
	EndOn EndKind = "on"
	// EndAfter repeats for a number of runs: the store counts the
	// occurrences fire runs (State.Runs), so an occurrence missed or one
	// that fell while the schedule was paused uses none of them.
	EndAfter EndKind = "after"
)

// End is when a repeat stops. A Once schedule has none.
type End struct {
	Kind EndKind `yaml:"kind" json:"kind"`
	// On is the last date (YYYY-MM-DD, in the schedule's zone) for EndOn.
	On string `yaml:"on,omitempty" json:"on,omitempty"`
	// After is the number of runs for EndAfter.
	After int `yaml:"after,omitempty" json:"after,omitempty"`
}

// Recurrence is when a schedule is due.
type Recurrence struct {
	Kind Kind `yaml:"kind" json:"kind"`
	// Date is the day of a Once schedule (YYYY-MM-DD).
	Date string `yaml:"date,omitempty" json:"date,omitempty"`
	// Time is the wall-clock time of every occurrence (HH:MM, 24-hour).
	Time string `yaml:"time" json:"time"`
	// Days are the days of a Weekly schedule, lower-case three-letter
	// names (mon … sun), kept sorted Monday first.
	Days []string `yaml:"days,omitempty" json:"days,omitempty"`
	// MonthDay is the day of a Monthly schedule, 1 to 31.
	MonthDay int `yaml:"monthDay,omitempty" json:"monthDay,omitempty"`
	// Zone is the IANA time zone the times are in (Europe/London).
	Zone string `yaml:"zone" json:"zone"`
	// Start is the instant the schedule was made: nothing before it is an
	// occurrence.
	Start time.Time `yaml:"start" json:"start"`
	End   End       `yaml:"end,omitempty" json:"end,omitempty"`
}

var dayNames = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// weekdayOf maps a day name to its time.Weekday.
var weekdayOf = map[string]time.Weekday{
	"mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday,
	"fri": time.Friday, "sat": time.Saturday, "sun": time.Sunday,
}

// ParseDays reads a comma-separated list of day names ("tue,thu",
// "Tuesday, Thursday") into the sorted, de-duplicated form Days keeps.
func ParseDays(s string) ([]string, error) {
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		p := strings.ToLower(strings.TrimSpace(part))
		if p == "" {
			continue
		}
		if len(p) > 3 {
			p = p[:3]
		}
		if _, ok := weekdayOf[p]; !ok {
			return nil, fmt.Errorf("unknown day %q (want mon, tue, wed, thu, fri, sat or sun)", strings.TrimSpace(part))
		}
		seen[p] = true
	}
	out := make([]string, 0, len(seen))
	for _, d := range dayNames {
		if seen[d] {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no days given")
	}
	return out, nil
}

// clock is a parsed HH:MM.
type clock struct{ h, m int }

func parseClock(s string) (clock, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return clock{}, fmt.Errorf("time %q is not HH:MM (24-hour)", s)
	}
	return clock{t.Hour(), t.Minute()}, nil
}

// Validate reports the first thing wrong with r.
func (r Recurrence) Validate() error {
	if _, err := parseClock(r.Time); err != nil {
		return err
	}
	if r.Zone == "" {
		return errors.New("no time zone")
	}
	if _, err := time.LoadLocation(r.Zone); err != nil {
		return fmt.Errorf("time zone %q: %w", r.Zone, err)
	}
	if r.Start.IsZero() {
		return errors.New("no start")
	}
	switch r.Kind {
	case Once:
		if _, err := time.Parse(time.DateOnly, r.Date); err != nil {
			return fmt.Errorf("date %q is not YYYY-MM-DD", r.Date)
		}
		if r.End.Kind != "" && r.End.Kind != EndNever {
			return errors.New("a schedule that runs once has no end")
		}
	case Daily, Weekdays:
	case Weekly:
		if len(r.Days) == 0 {
			return errors.New("a weekly schedule needs at least one day")
		}
		for _, d := range r.Days {
			if _, ok := weekdayOf[d]; !ok {
				return fmt.Errorf("unknown day %q", d)
			}
		}
	case Monthly:
		if r.MonthDay < 1 || r.MonthDay > 31 {
			return fmt.Errorf("day of the month %d is not 1 to 31", r.MonthDay)
		}
	default:
		return fmt.Errorf("unknown repeat %q (want once, daily, weekdays, weekly or monthly)", r.Kind)
	}
	switch r.End.Kind {
	case "", EndNever:
	case EndOn:
		if _, err := time.Parse(time.DateOnly, r.End.On); err != nil {
			return fmt.Errorf("end date %q is not YYYY-MM-DD", r.End.On)
		}
	case EndAfter:
		if r.End.After < 1 {
			return errors.New("an end after N runs needs N of at least 1")
		}
	default:
		return fmt.Errorf("unknown end %q (want never, on or after)", r.End.Kind)
	}
	return nil
}

// Next is the first occurrence strictly after after, or false when the
// calendar has none left. An EndAfter count is not the calendar's but the
// runs': Remaining applies it. r must be valid.
func (r Recurrence) Next(after time.Time) (time.Time, bool) {
	loc, err := time.LoadLocation(r.Zone)
	if err != nil {
		return time.Time{}, false
	}
	c, err := parseClock(r.Time)
	if err != nil {
		return time.Time{}, false
	}
	var endDay time.Time // the last day's start, for EndOn
	if r.End.Kind == EndOn {
		d, err := time.ParseInLocation(time.DateOnly, r.End.On, loc)
		if err != nil {
			return time.Time{}, false
		}
		endDay = dayStart(d)
	}
	from := after
	if from.Before(r.Start) {
		from = r.Start
	}
	if r.Kind == Once {
		day, err := time.ParseInLocation(time.DateOnly, r.Date, loc)
		if err != nil {
			return time.Time{}, false
		}
		t := wallTime(day, c, loc)
		if t.After(after) && !t.Before(r.Start) {
			return t, true
		}
		return time.Time{}, false
	}
	day := dayStart(from.In(loc))
	// Ten years of days bounds the walk; a valid repeat is due far sooner.
	for i := 0; i < 3660; i++ {
		if !endDay.IsZero() && day.After(endDay) {
			return time.Time{}, false
		}
		if r.dueOn(day) {
			t := wallTime(day, c, loc)
			if !t.Before(r.Start) && t.After(after) {
				return t, true
			}
		}
		day = dayStart(day.AddDate(0, 0, 1))
	}
	return time.Time{}, false
}

// Remaining is how many runs an EndAfter schedule has left after runs of
// them, and -1 for any other end.
func (r Recurrence) Remaining(runs int) int {
	if r.End.Kind != EndAfter {
		return -1
	}
	return max(r.End.After-runs, 0)
}

// Between is every occurrence in (from, to], at most limit of them (0: no
// limit). A calendar asks it for a month; fire asks it for what fell due.
func (r Recurrence) Between(from, to time.Time, limit int) []time.Time {
	var out []time.Time
	t := from
	for {
		n, ok := r.Next(t)
		if !ok || n.After(to) {
			return out
		}
		out = append(out, n)
		if limit > 0 && len(out) >= limit {
			return out
		}
		t = n
	}
}

// dueOn reports whether day (a day start in the zone) has an occurrence.
func (r Recurrence) dueOn(day time.Time) bool {
	switch r.Kind {
	case Daily:
		return true
	case Weekdays:
		wd := day.Weekday()
		return wd != time.Saturday && wd != time.Sunday
	case Weekly:
		for _, d := range r.Days {
			if weekdayOf[d] == day.Weekday() {
				return true
			}
		}
		return false
	case Monthly:
		last := daysIn(day.Year(), day.Month(), day.Location())
		want := r.MonthDay
		if want > last {
			want = last
		}
		return day.Day() == want
	}
	return false
}

func daysIn(year int, month time.Month, loc *time.Location) int {
	return time.Date(year, month+1, 0, 12, 0, 0, 0, loc).Day()
}

// dayStart is noon-anchored date arithmetic's other half: the date of t,
// re-made at noon so adding a day never lands on the wrong date across a
// daylight-saving change. Only the date of the result is ever read.
func dayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 12, 0, 0, 0, t.Location())
}

// wallTime is the first instant on day whose wall clock reads c or later.
// A time the spring change skips becomes the first valid minute after it;
// a time the autumn change repeats is its first instance.
func wallTime(day time.Time, c clock, loc *time.Location) time.Time {
	y, m, d := day.Date()
	t := time.Date(y, m, d, c.h, c.m, 0, 0, loc)
	if lt := t.In(loc); lt.Hour() == c.h && lt.Minute() == c.m && lt.Day() == d {
		// Unambiguous, or Go picked one of two instances: take the earlier
		// if an hour before reads the same wall clock.
		if e := t.Add(-time.Hour).In(loc); e.Hour() == c.h && e.Minute() == c.m && e.Day() == d {
			return t.Add(-time.Hour)
		}
		return t
	}
	// In a gap: walk minutes from three hours before until the wall clock
	// on day reaches c.
	want := c.h*60 + c.m
	u := t.Add(-3 * time.Hour).Truncate(time.Minute)
	for i := 0; i < 6*60; i++ {
		lt := u.In(loc)
		if lt.Year() == y && lt.Month() == m && lt.Day() == d && lt.Hour()*60+lt.Minute() >= want {
			return u
		}
		u = u.Add(time.Minute)
	}
	return t
}

// Describe is the repeat in words, for a list and a confirmation.
func (r Recurrence) Describe() string {
	var s string
	switch r.Kind {
	case Once:
		s = "once on " + r.Date + " at " + r.Time
	case Daily:
		s = "every day at " + r.Time
	case Weekdays:
		s = "weekdays at " + r.Time
	case Weekly:
		s = "every " + strings.Join(r.Days, ", ") + " at " + r.Time
	case Monthly:
		s = fmt.Sprintf("every month on day %d at %s", r.MonthDay, r.Time)
	}
	s += " (" + r.Zone + ")"
	switch r.End.Kind {
	case EndOn:
		s += ", until " + r.End.On
	case EndAfter:
		s += fmt.Sprintf(", %d times", r.End.After)
	}
	return s
}
