package ical

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Occurrence is one instance of an event that falls in a window.
type Occurrence struct {
	UID         string
	Summary     string
	Location    string
	Description string
	URL         string
	Organizer   string
	// Status is TENTATIVE or CONFIRMED, or empty; a cancelled instance is
	// never an occurrence.
	Status string
	Start  time.Time
	End    time.Time
	// AllDay is an event of whole days: Start is midnight of its first
	// day and End midnight after its last, in the zone it was read in.
	AllDay bool
	// Busy says the instance blocks time: it is not TRANSP:TRANSPARENT.
	Busy bool
	// Recurring says the instance is one of a series.
	Recurring bool
}

// Expansion is what Expand found.
type Expansion struct {
	Occurrences []Occurrence
	// Notes say what could not be read exactly: a time zone held at a
	// fixed offset or not recognised, a rule that could not be followed.
	Notes []string
}

// Expand returns every occurrence that overlaps [from, to), sorted by
// start. All-day events and floating times are read in loc, unless the
// feed names its own zone for floating times.
func (c *Calendar) Expand(from, to time.Time, loc *time.Location) Expansion {
	var ex Expansion
	notes := map[string]bool{}
	note := func(s string) {
		if !notes[s] {
			notes[s] = true
			ex.Notes = append(ex.Notes, s)
		}
	}
	floating := loc
	if c.TimeZone != "" {
		if l, ok := ianaLocation(c.TimeZone); ok {
			floating = l
		} else if name, ok := windowsZones[c.TimeZone]; ok {
			if l, err := time.LoadLocation(name); err == nil {
				floating = l
			}
		}
	}
	work := MaxWork
	r := resolver{cal: c, loc: loc, floating: floating, note: note, work: &work}

	overrides := map[string][]Event{}
	for _, e := range c.Events {
		if e.RecurrenceID != nil {
			overrides[e.UID] = append(overrides[e.UID], e)
		}
	}
	masters := map[string]bool{}
	for _, e := range c.Events {
		if e.RecurrenceID != nil {
			continue
		}
		masters[e.UID] = true
		ex.Occurrences = append(ex.Occurrences, r.expandMaster(e, overrides[e.UID], from, to)...)
	}
	for uid, ovs := range overrides {
		for _, o := range ovs {
			if o.Status == "CANCELLED" {
				continue
			}
			if occ, ok := r.single(o, from, to); ok {
				occ.Recurring = masters[uid]
				ex.Occurrences = append(ex.Occurrences, occ)
			}
		}
	}
	slices.SortStableFunc(ex.Occurrences, func(a, b Occurrence) int {
		if n := a.Start.Compare(b.Start); n != 0 {
			return n
		}
		return strings.Compare(a.Summary, b.Summary)
	})
	return ex
}

type resolver struct {
	cal      *Calendar
	loc      *time.Location
	floating *time.Location
	note     func(string)
	// work is what the expansion may still do (MaxWork).
	work *int
}

// at is a value's instant: UTC, in its TZID's zone, or floating.
func (r resolver) at(dt dateTime) time.Time {
	switch {
	case dt.DateOnly:
		return time.Date(dt.Y, time.Month(dt.Mo), dt.D, 0, 0, 0, 0, r.loc)
	case dt.UTC:
		return time.Date(dt.Y, time.Month(dt.Mo), dt.D, dt.H, dt.Mi, dt.S, 0, time.UTC)
	case dt.TZID != "":
		return time.Date(dt.Y, time.Month(dt.Mo), dt.D, dt.H, dt.Mi, dt.S, 0, r.zone(dt.TZID))
	default:
		return time.Date(dt.Y, time.Month(dt.Mo), dt.D, dt.H, dt.Mi, dt.S, 0, r.floating)
	}
}

func (r resolver) zone(tzid string) *time.Location {
	l, how := r.cal.location(tzid, r.floating)
	switch how {
	case zoneApprox:
		r.note(fmt.Sprintf("the feed's time zone %q was matched by its offsets; times near a daylight-saving change may be an hour out", tzid))
	case zoneUnknown:
		r.note(fmt.Sprintf("the feed's time zone %q is not recognised; its times were read in %s", tzid, r.floating))
	}
	return l
}

// length is an event's duration, and for an all-day event its days.
func (r resolver) length(e Event) (time.Duration, int) {
	if e.Start.DateOnly {
		days := 1
		if e.End != nil && e.End.DateOnly {
			s := civil{e.Start.Y, e.Start.Mo, e.Start.D}
			end := civil{e.End.Y, e.End.Mo, e.End.D}
			n := 0
			for d := s; d.less(end) && n < 3660; d = d.addDays(1) {
				n++
			}
			if n > 0 {
				days = n
			}
		} else if e.hasDur && e.Duration >= 24*time.Hour {
			days = int(e.Duration / (24 * time.Hour))
		}
		return 0, days
	}
	switch {
	case e.End != nil:
		d := r.at(*e.End).Sub(r.at(e.Start))
		if d < 0 {
			d = 0
		}
		return d, 0
	case e.hasDur && e.Duration > 0:
		return e.Duration, 0
	}
	return 0, 0
}

func (r resolver) occurrence(e Event, start time.Time, dur time.Duration, days int) Occurrence {
	o := Occurrence{
		UID: e.UID, Summary: e.Summary, Location: e.Location, Description: e.Description,
		URL: e.URL, Organizer: e.Organizer, Status: e.Status,
		Start: start, Busy: !e.Transparent,
	}
	if e.Start.DateOnly {
		o.AllDay = true
		o.End = start.AddDate(0, 0, days)
	} else {
		o.End = start.Add(dur)
	}
	return o
}

func overlaps(o Occurrence, from, to time.Time) bool {
	if !o.Start.Before(to) {
		return false
	}
	if o.End.After(o.Start) {
		return o.End.After(from)
	}
	return !o.Start.Before(from)
}

func (r resolver) single(e Event, from, to time.Time) (Occurrence, bool) {
	dur, days := r.length(e)
	o := r.occurrence(e, r.at(e.Start), dur, days)
	return o, overlaps(o, from, to)
}

// key identifies an instance of a series by its original start: the date
// of an all-day instance, else its instant.
func key(t time.Time, allDay bool) string {
	if allDay {
		return t.Format("2006-01-02")
	}
	return t.UTC().Format(time.RFC3339)
}

// civilKey is a value's date, as key writes one.
func civilKey(dt dateTime) string {
	return fmt.Sprintf("%04d-%02d-%02d", dt.Y, dt.Mo, dt.D)
}

func (r resolver) expandMaster(e Event, ovs []Event, from, to time.Time) []Occurrence {
	if e.Status == "CANCELLED" {
		return nil
	}
	if e.RRule == "" && len(e.RDates) == 0 {
		if o, ok := r.single(e, from, to); ok {
			return []Occurrence{o}
		}
		return nil
	}
	dur, days := r.length(e)
	start := r.at(e.Start)
	allDay := e.Start.DateOnly

	// The walk stops at to; instances that begin before from are kept
	// when they still overlap it.
	stop := to

	// An exception names an instance by its start; one written as a date on
	// a series of timed instances names every instance that day, in the
	// series' own zone.
	excluded, excludedDays := map[string]bool{}, map[string]bool{}
	for _, x := range e.ExDates {
		if x.DateOnly && !allDay {
			excludedDays[civilKey(x)] = true
			continue
		}
		excluded[key(r.at(x), allDay)] = true
	}
	for _, o := range ovs {
		if o.RecurrenceID.DateOnly && !allDay {
			excludedDays[civilKey(*o.RecurrenceID)] = true
			continue
		}
		excluded[key(r.at(*o.RecurrenceID), allDay)] = true
	}

	var starts []time.Time
	if e.RRule != "" {
		rule, err := parseRRule(e.RRule)
		if err != nil {
			r.note(fmt.Sprintf("%q repeats by a rule that could not be followed (%v); only its first instance is shown", e.Summary, err))
			starts = append(starts, start)
		} else {
			var until *time.Time
			if rule.until != nil {
				u := r.at(*rule.until)
				if rule.until.DateOnly {
					u = time.Date(rule.until.Y, time.Month(rule.until.Mo), rule.until.D, 23, 59, 59, 0, start.Location())
				}
				until = &u
			}
			// An instance that starts before from overlaps it only when it
			// is no longer than the series' instances.
			span := dur
			if allDay {
				span = time.Duration(days+1) * 24 * time.Hour
			}
			if rule.starts(start, start.Location(), until, from.Add(-span), stop, r.work, func(t time.Time) bool {
				starts = append(starts, t)
				return true
			}) {
				if *r.work < 0 {
					r.note("the feed's repeating events are too many to work out in full: some instances are left out")
				} else {
					r.note(fmt.Sprintf("%q repeats by a rule whose instances could not all be worked out: some are left out", e.Summary))
				}
			}
		}
	} else {
		starts = append(starts, start)
	}
	for _, rd := range e.RDates {
		t := r.at(rd)
		if allDay && !rd.DateOnly {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, r.loc)
		}
		starts = append(starts, t)
	}

	var out []Occurrence
	seen := map[string]bool{}
	for _, t := range starts {
		k := key(t, allDay)
		if excluded[k] || seen[k] || excludedDays[t.Format("2006-01-02")] {
			continue
		}
		seen[k] = true
		o := r.occurrence(e, t, dur, days)
		o.Recurring = true
		if overlaps(o, from, to) {
			out = append(out, o)
		}
	}
	return out
}

// busyOnlySummaries are the titles a feed published as free/busy only
// gives every event.
var busyOnlySummaries = map[string]bool{
	"": true, "busy": true, "free": true, "tentative": true, "away": true,
	"out of office": true, "working elsewhere": true, "busy (tentative)": true,
}

// BusyOnly says the feed was published as free/busy only: VFREEBUSY
// blocks, or events none of which has a title of its own, a location or a
// description.
func (c *Calendar) BusyOnly() bool {
	if len(c.Events) == 0 {
		return c.freeBusy
	}
	for _, e := range c.Events {
		if !busyOnlySummaries[strings.ToLower(strings.TrimSpace(e.Summary))] || e.Location != "" || e.Description != "" {
			return false
		}
	}
	return true
}

// Slot is a span of time.
type Slot struct{ Start, End time.Time }

// FreeSlots are the spans within windows, each at least min long, that no
// busy occurrence covers.
func FreeSlots(occs []Occurrence, windows []Slot, min time.Duration) []Slot {
	var busy []Slot
	for _, o := range occs {
		if o.Busy && o.End.After(o.Start) {
			busy = append(busy, Slot{o.Start, o.End})
		}
	}
	slices.SortFunc(busy, func(a, b Slot) int { return a.Start.Compare(b.Start) })
	var merged []Slot
	for _, b := range busy {
		if n := len(merged); n > 0 && !b.Start.After(merged[n-1].End) {
			if b.End.After(merged[n-1].End) {
				merged[n-1].End = b.End
			}
			continue
		}
		merged = append(merged, b)
	}
	var out []Slot
	for _, w := range windows {
		cur := w.Start
		for _, b := range merged {
			if !b.End.After(cur) {
				continue
			}
			if !b.Start.Before(w.End) {
				break
			}
			if b.Start.After(cur) && b.Start.Sub(cur) >= min {
				out = append(out, Slot{cur, b.Start})
			}
			if b.End.After(cur) {
				cur = b.End
			}
		}
		if w.End.After(cur) && w.End.Sub(cur) >= min {
			out = append(out, Slot{cur, w.End})
		}
	}
	return out
}
