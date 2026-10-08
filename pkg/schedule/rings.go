package schedule

import (
	"slices"
	"sort"
	"time"
)

// LocalRings is when a scheduler that keeps only this computer's local
// time (launchd, Task Scheduler) is to ring for a schedule. Neither can
// name the schedule's own zone, or the plan's rules for a month's last day
// and the daylight-saving changes, so the rings are compiled from the
// schedule's own occurrences: a superset, never fewer, which fire narrows
// (D4). A ring that comes late still runs its occurrence, since fire runs
// the latest one due when no later one has fallen due (D5).
type LocalRings struct {
	// Repeat rings at a wall time on some days, every week or month.
	Repeat []LocalRing
	// Once rings at these instants: an occurrence whose local wall time
	// is its own (the spring change, a zone whose offset to this
	// computer's moves for a few days), or a schedule that runs once.
	Once []time.Time
}

// LocalRing rings at Hour:Minute local time on Weekdays (none: every day),
// or, when MonthDays is set, on those days of every month.
type LocalRing struct {
	Hour, Minute int
	Weekdays     []time.Weekday
	MonthDays    []int
}

// ringWindow is how far ahead the repeating rings are learnt from: a
// year and a bit holds every daylight-saving pattern.
const ringWindow = 400 * 24 * time.Hour

// oneOffYears is how far ahead an occurrence no repeating ring covers
// gets a one-off ring of its own, as systemd's skipped-time rings do; every
// add, edit and sync writes the rings afresh.
const oneOffYears = 3

// LocalRingsFor compiles r into rings in loc from now on.
func LocalRingsFor(r Recurrence, loc *time.Location, now time.Time) (LocalRings, error) {
	if err := r.Validate(); err != nil {
		return LocalRings{}, err
	}
	var out LocalRings
	if r.Kind == Once {
		// The one occurrence, even when it is past: a fire then decides
		// whether it still runs (D5).
		if t, ok := r.Next(r.Start.Add(-time.Nanosecond)); ok {
			out.Once = []time.Time{t.In(loc)}
		}
		return out, nil
	}
	type group struct {
		hour, minute int
		weekdays     map[time.Weekday]bool
		monthDays    map[int]bool
		at           []time.Time
	}
	groups := map[int]*group{}
	for _, t := range r.Between(now, now.Add(ringWindow), 0) {
		lt := t.In(loc)
		key := lt.Hour()*60 + lt.Minute()
		g := groups[key]
		if g == nil {
			g = &group{hour: lt.Hour(), minute: lt.Minute(), weekdays: map[time.Weekday]bool{}, monthDays: map[int]bool{}}
			groups[key] = g
		}
		g.weekdays[lt.Weekday()] = true
		g.monthDays[lt.Day()] = true
		g.at = append(g.at, lt)
	}
	keys := make([]int, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		g := groups[k]
		if len(g.at) == 1 {
			// A wall time of its own (the spring change): a one-off,
			// below, not a ring every week all year.
			continue
		}
		ring := LocalRing{Hour: g.hour, Minute: g.minute}
		if r.Kind == Monthly {
			if r.MonthDay > 28 {
				// The month's last day moves (and a leap February may not
				// fall in the window): ring on every day it can be.
				for d := 28; d <= 31; d++ {
					g.monthDays[d] = true
				}
			}
			for d := range g.monthDays {
				ring.MonthDays = append(ring.MonthDays, d)
			}
			slices.Sort(ring.MonthDays)
		} else if len(g.weekdays) < 7 {
			for d := range g.weekdays {
				ring.Weekdays = append(ring.Weekdays, d)
			}
			slices.Sort(ring.Weekdays)
		}
		out.Repeat = append(out.Repeat, ring)
	}
	for _, t := range r.Between(now, now.AddDate(oneOffYears, 0, 0), 0) {
		if lt := t.In(loc); !out.RingsAt(lt) {
			out.Once = append(out.Once, lt)
		}
	}
	return out, nil
}

// RingsAt reports whether the rings ring at t's own local minute, so an
// occurrence at t waits for no later ring.
func (rs LocalRings) RingsAt(t time.Time) bool {
	for _, o := range rs.Once {
		if o.Equal(t) {
			return true
		}
	}
	for _, r := range rs.Repeat {
		if t.Hour() != r.Hour || t.Minute() != r.Minute {
			continue
		}
		switch {
		case len(r.MonthDays) > 0:
			if slices.Contains(r.MonthDays, t.Day()) {
				return true
			}
		case len(r.Weekdays) > 0:
			if slices.Contains(r.Weekdays, t.Weekday()) {
				return true
			}
		default:
			return true
		}
	}
	return false
}
