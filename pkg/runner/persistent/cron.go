package persistent

// A task's time is a five-field cron expression (minute, hour, day of the
// month, month, day of the week) read in an IANA zone. Each field is "*", a
// number, a range "a-b", a list "a,b", or any of those stepped "/n". As in
// cron, a task with both a day of the month and a day of the week restricted
// runs on a day that matches either. Day of the week 0 and 7 are Sunday.

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed five-field expression.
type Cron struct {
	minute, hour, dom, month, dow uint64
	// domAll and dowAll say the field was "*" (or "*/1"): cron's either-day
	// rule applies only when both are restricted.
	domAll, dowAll bool
}

// cronField is one field's bounds and name, for its errors.
type cronField struct {
	name     string
	min, max int
}

var cronFields = [5]cronField{
	{"minute", 0, 59}, {"hour", 0, 23}, {"day of the month", 1, 31}, {"month", 1, 12}, {"day of the week", 0, 7},
}

// ParseCron reads a five-field expression.
func ParseCron(expr string) (Cron, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Cron{}, fmt.Errorf("a task's time is five cron fields (minute hour day-of-month month day-of-week), such as \"0 8 * * 1-5\"; %q has %d", expr, len(fields))
	}
	var bits [5]uint64
	var all [5]bool
	for i, f := range fields {
		b, a, err := parseCronField(f, cronFields[i])
		if err != nil {
			return Cron{}, err
		}
		bits[i], all[i] = b, a
	}
	// Sunday is 0 and 7.
	if bits[4]&(1<<7) != 0 {
		bits[4] = bits[4]&^(1<<7) | 1
	}
	return Cron{minute: bits[0], hour: bits[1], dom: bits[2], month: bits[3], dow: bits[4], domAll: all[2], dowAll: all[4]}, nil
}

func parseCronField(f string, fd cronField) (uint64, bool, error) {
	var bits uint64
	all := false
	for _, part := range strings.Split(f, ",") {
		rng, step := part, 1
		if i := strings.IndexByte(part, '/'); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n < 1 {
				return 0, false, fmt.Errorf("the %s field %q has a step that is not a whole number above 0", fd.name, f)
			}
			rng, step = part[:i], n
		}
		lo, hi := fd.min, fd.max
		switch {
		case rng == "*":
			if step == 1 {
				all = true
			}
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err1, err2 error
			lo, err1 = strconv.Atoi(a)
			hi, err2 = strconv.Atoi(b)
			if err1 != nil || err2 != nil || lo > hi {
				return 0, false, fmt.Errorf("the %s field %q has a range that is not low-high", fd.name, f)
			}
		default:
			n, err := strconv.Atoi(rng)
			if err != nil {
				return 0, false, fmt.Errorf("the %s field %q is not a number, a range, a list or *", fd.name, f)
			}
			lo, hi = n, n
			if strings.Contains(part, "/") {
				hi = fd.max
			}
		}
		if lo < fd.min || hi > fd.max {
			return 0, false, fmt.Errorf("the %s field %q is outside %d-%d", fd.name, f, fd.min, fd.max)
		}
		for v := lo; v <= hi; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, all, nil
}

// dayMatches says the task runs on t's day.
func (c Cron) dayMatches(t time.Time) bool {
	dom := c.dom&(1<<uint(t.Day())) != 0
	dow := c.dow&(1<<uint(t.Weekday())) != 0
	if c.domAll || c.dowAll {
		return dom && dow
	}
	return dom || dow
}

// errNoNext says an expression names no time that comes (31 February).
var errNoNext = errors.New("this time never comes")

// cronHorizon is how far ahead Next looks: past it, an expression is taken
// to name no time (a 29 February on a weekday comes within it).
const cronHorizon = 8 * 366 * 24 * time.Hour

// Next is the first time after after, to the minute, that the expression
// names in loc; errNoNext when none comes within cronHorizon. A wall-clock
// minute a change to summer time skips does not come; one a change back to
// winter time repeats comes once, the first time.
func (c Cron) Next(after time.Time, loc *time.Location) (time.Time, error) {
	// The walk is over wall-clock minutes, held as UTC so that no zone
	// change skips or repeats one; each match is then read in loc.
	a := after.In(loc)
	w := time.Date(a.Year(), a.Month(), a.Day(), a.Hour(), a.Minute(), 0, 0, time.UTC).Add(time.Minute)
	end := w.Add(cronHorizon)
	for w.Before(end) {
		if c.month&(1<<uint(w.Month())) == 0 {
			w = time.Date(w.Year(), w.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if !c.dayMatches(w) {
			w = time.Date(w.Year(), w.Month(), w.Day()+1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if c.hour&(1<<uint(w.Hour())) == 0 {
			w = time.Date(w.Year(), w.Month(), w.Day(), w.Hour()+1, 0, 0, 0, time.UTC)
			continue
		}
		if c.minute&(1<<uint(w.Minute())) == 0 {
			w = w.Add(time.Minute)
			continue
		}
		t := time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), 0, 0, loc)
		// A minute the change to summer time skips reads back as another
		// wall time, and does not come; a repeated one is read once.
		if t.Hour() != w.Hour() || t.Minute() != w.Minute() || !t.After(after) {
			w = w.Add(time.Minute)
			continue
		}
		return t, nil
	}
	return time.Time{}, errNoNext
}

// CloserThan says two of the expression's times in loc, over the year after
// from, are less than d apart: it fires more often than every d.
func (c Cron) CloserThan(from time.Time, loc *time.Location, d time.Duration) (bool, error) {
	prev, err := c.Next(from, loc)
	if err != nil {
		return false, err
	}
	end := from.AddDate(1, 0, 1)
	for prev.Before(end) {
		next, err := c.Next(prev, loc)
		if err != nil {
			return false, nil
		}
		if next.Sub(prev) < d {
			return true, nil
		}
		prev = next
	}
	return false, nil
}

// Count is how many times the expression names in loc after from and up to
// to, counting at most limit+1.
func (c Cron) Count(from, to time.Time, loc *time.Location, limit int) int {
	n := 0
	for t := from; n <= limit; n++ {
		next, err := c.Next(t, loc)
		if err != nil || next.After(to) {
			return n
		}
		t = next
	}
	return n
}
