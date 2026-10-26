package ical

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// maxPeriods bounds how many periods (days, weeks, months or years) one
// rule is walked through, so a rule that never yields cannot spin.
const maxPeriods = 200000

// MaxWork bounds what one expansion of a feed may do, counted in periods
// walked and candidate instances made across all its series, so a feed
// whose rules are hostile or enormous costs a bounded time and memory; what
// is past it is left out, with a note.
const MaxWork = 4_000_000

// rrule is a parsed RRULE.
type rrule struct {
	freq       string
	interval   int
	count      int
	until      *dateTime
	byDay      []weekdayNum
	byMonthDay []int
	byMonth    []int
	byYearDay  []int
	bySetPos   []int
	byHour     []int
	byMinute   []int
	wkst       time.Weekday
}

// weekdayNum is one BYDAY entry: a weekday, and its ordinal within the
// month or year (0 for every one).
type weekdayNum struct {
	n  int
	wd time.Weekday
}

var weekdays = map[string]time.Weekday{
	"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday,
	"TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday,
}

func parseRRule(s string) (*rrule, error) {
	r := &rrule{interval: 1, wkst: time.Monday}
	for _, part := range strings.Split(strings.TrimSpace(s), ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		k, v = strings.ToUpper(strings.TrimSpace(k)), strings.ToUpper(strings.TrimSpace(v))
		var err error
		switch k {
		case "FREQ":
			r.freq = v
		case "INTERVAL":
			r.interval, err = strconv.Atoi(v)
			if err == nil && r.interval < 1 {
				err = fmt.Errorf("INTERVAL %d", r.interval)
			}
		case "COUNT":
			r.count, err = strconv.Atoi(v)
			if err == nil && r.count < 1 {
				err = fmt.Errorf("COUNT %d", r.count)
			}
		case "UNTIL":
			var dt dateTime
			dt, err = parseDateTimeValue(v, nil)
			r.until = &dt
		case "BYDAY":
			for _, d := range strings.Split(v, ",") {
				d = strings.TrimSpace(d)
				if len(d) < 2 {
					return nil, fmt.Errorf("BYDAY %q", d)
				}
				wd, ok := weekdays[d[len(d)-2:]]
				if !ok {
					return nil, fmt.Errorf("BYDAY %q", d)
				}
				n := 0
				if num := d[:len(d)-2]; num != "" {
					n, err = strconv.Atoi(strings.TrimPrefix(num, "+"))
					if err != nil || n == 0 || n > 53 || n < -53 {
						return nil, fmt.Errorf("BYDAY %q", d)
					}
				}
				r.byDay = append(r.byDay, weekdayNum{n: n, wd: wd})
			}
		case "BYMONTHDAY":
			r.byMonthDay, err = intList(v, -31, 31)
		case "BYMONTH":
			r.byMonth, err = intList(v, 1, 12)
		case "BYYEARDAY":
			r.byYearDay, err = intList(v, -366, 366)
		case "BYSETPOS":
			r.bySetPos, err = intList(v, -366, 366)
		case "BYHOUR":
			r.byHour, err = intList(v, 0, 23)
		case "BYMINUTE":
			r.byMinute, err = intList(v, 0, 59)
		case "BYWEEKNO", "BYSECOND":
			return nil, fmt.Errorf("RRULE %s is not supported", k)
		case "WKST":
			wd, ok := weekdays[v]
			if !ok {
				err = fmt.Errorf("WKST %q", v)
			}
			r.wkst = wd
		}
		if err != nil {
			return nil, fmt.Errorf("RRULE %s: %v", k, err)
		}
	}
	switch r.freq {
	case "DAILY", "WEEKLY", "MONTHLY", "YEARLY":
	case "":
		return nil, fmt.Errorf("RRULE has no FREQ")
	default:
		return nil, fmt.Errorf("RRULE FREQ=%s is not supported", r.freq)
	}
	return r, nil
}

func intList(v string, lo, hi int) ([]int, error) {
	var out []int
	for _, s := range strings.Split(v, ",") {
		n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(s), "+"))
		if err != nil || n < lo || n > hi || n == 0 && lo < 0 {
			return nil, fmt.Errorf("%q", s)
		}
		out = append(out, n)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// civil is a calendar date.
type civil struct{ y, m, d int }

func (c civil) weekday() time.Weekday {
	return time.Date(c.y, time.Month(c.m), c.d, 12, 0, 0, 0, time.UTC).Weekday()
}

func (c civil) addDays(n int) civil {
	t := time.Date(c.y, time.Month(c.m), c.d+n, 12, 0, 0, 0, time.UTC)
	return civil{t.Year(), int(t.Month()), t.Day()}
}

func (c civil) less(o civil) bool {
	if c.y != o.y {
		return c.y < o.y
	}
	if c.m != o.m {
		return c.m < o.m
	}
	return c.d < o.d
}

func daysIn(y, m int) int {
	return time.Date(y, time.Month(m)+1, 0, 12, 0, 0, 0, time.UTC).Day()
}

func daysInYear(y int) int {
	return time.Date(y, 12, 31, 12, 0, 0, 0, time.UTC).YearDay()
}

// starts walks the rule from dtstart (a wall clock in loc), calling emit
// with each instance's start in order until emit returns false, the rule's
// COUNT or UNTIL ends it, or an instance starts at or after stop. DTSTART is
// always the first instance and counts toward COUNT, whether or not the rule
// would have produced it, as calendar services read a series. A rule with
// no COUNT skips the periods that end before from, which no instance there
// can reach. Each period walked and each candidate made spends one of
// *work; it reports whether the walk gave up, at maxPeriods or with *work
// spent, before reaching stop.
func (r *rrule) starts(dtstart time.Time, loc *time.Location, until *time.Time, from, stop time.Time, work *int, emit func(time.Time) bool) (exhausted bool) {
	first := civil{dtstart.Year(), int(dtstart.Month()), dtstart.Day()}
	h0, mi0, s0 := dtstart.Clock()
	hours := r.byHour
	if len(hours) == 0 {
		hours = []int{h0}
	}
	minutes := r.byMinute
	if len(minutes) == 0 {
		minutes = []int{mi0}
	}
	emitted := 0
	if !dtstart.Before(stop) {
		return false
	}
	if !emit(dtstart) {
		return false
	}
	emitted++
	if r.count == 1 {
		return false
	}
	p0 := 0
	if r.count == 0 {
		p0 = r.periodsBefore(first, civil{from.Year(), int(from.Month()), from.Day()})
	}
	for p := p0; p < p0+maxPeriods; p++ {
		if periodStart(r, first, p).y > stop.Year()+1 {
			return false
		}
		days := r.periodDays(first, p)
		*work -= 1 + len(days)*len(hours)*len(minutes)
		if *work < 0 {
			return true
		}
		var set []time.Time
		for _, d := range days {
			for _, h := range hours {
				for _, mi := range minutes {
					set = append(set, time.Date(d.y, time.Month(d.m), d.d, h, mi, s0, 0, loc))
				}
			}
		}
		slices.SortFunc(set, func(a, b time.Time) int { return a.Compare(b) })
		set = slices.CompactFunc(set, func(a, b time.Time) bool { return a.Equal(b) })
		if len(r.bySetPos) > 0 {
			set = setPos(set, r.bySetPos)
		}
		for _, t := range set {
			if !t.After(dtstart) {
				continue
			}
			if until != nil && t.After(*until) {
				return false
			}
			if !t.Before(stop) {
				return false
			}
			if !emit(t) {
				return false
			}
			emitted++
			if r.count > 0 && emitted >= r.count {
				return false
			}
		}
	}
	return true
}

// periodsBefore is how many whole periods of the rule, from first's, end
// before day, less one to be sure: the walk may start there.
func (r *rrule) periodsBefore(first, day civil) int {
	if !first.less(day) {
		return 0
	}
	days := int(time.Date(day.y, time.Month(day.m), day.d, 12, 0, 0, 0, time.UTC).Sub(time.Date(first.y, time.Month(first.m), first.d, 12, 0, 0, 0, time.UTC)).Hours() / 24)
	var n int
	switch r.freq {
	case "DAILY":
		n = days / r.interval
	case "WEEKLY":
		n = days / 7 / r.interval
	case "MONTHLY":
		n = ((day.y-first.y)*12 + day.m - first.m) / r.interval
	default:
		n = (day.y - first.y) / r.interval
	}
	if n -= 2; n < 0 {
		return 0
	}
	return n
}

func setPos(set []time.Time, pos []int) []time.Time {
	var out []time.Time
	for _, p := range pos {
		i := p - 1
		if p < 0 {
			i = len(set) + p
		}
		if i >= 0 && i < len(set) {
			out = append(out, set[i])
		}
	}
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	return slices.CompactFunc(out, func(a, b time.Time) bool { return a.Equal(b) })
}

// periodStart is the first day of the p-th period of the rule.
func periodStart(r *rrule, first civil, p int) civil {
	switch r.freq {
	case "DAILY":
		return first.addDays(p * r.interval)
	case "WEEKLY":
		back := (int(first.weekday()) - int(r.wkst) + 7) % 7
		return first.addDays(-back + 7*p*r.interval)
	case "MONTHLY":
		t := time.Date(first.y, time.Month(first.m)+time.Month(p*r.interval), 1, 12, 0, 0, 0, time.UTC)
		return civil{t.Year(), int(t.Month()), 1}
	default: // YEARLY
		return civil{first.y + p*r.interval, 1, 1}
	}
}

// periodDays are the days of the p-th period the rule's BY parts select.
func (r *rrule) periodDays(first civil, p int) []civil {
	start := periodStart(r, first, p)
	switch r.freq {
	case "DAILY":
		if r.monthOK(start.m) && r.monthDayOK(start) && r.weekdayOK(start) {
			return []civil{start}
		}
		return nil
	case "WEEKLY":
		var out []civil
		for i := 0; i < 7; i++ {
			d := start.addDays(i)
			if !r.monthOK(d.m) {
				continue
			}
			if len(r.byDay) == 0 {
				if d.weekday() == first.weekday() {
					out = append(out, d)
				}
				continue
			}
			if r.weekdayOK(d) {
				out = append(out, d)
			}
		}
		return out
	case "MONTHLY":
		if !r.monthOK(start.m) {
			return nil
		}
		return r.monthDays(start.y, start.m, first.d)
	default: // YEARLY
		return r.yearDays(start.y, first)
	}
}

func (r *rrule) monthOK(m int) bool { return len(r.byMonth) == 0 || slices.Contains(r.byMonth, m) }

func (r *rrule) monthDayOK(d civil) bool {
	if len(r.byMonthDay) == 0 {
		return true
	}
	n := daysIn(d.y, d.m)
	for _, md := range r.byMonthDay {
		if md > 0 && md == d.d || md < 0 && n+md+1 == d.d {
			return true
		}
	}
	return false
}

func (r *rrule) weekdayOK(d civil) bool {
	if len(r.byDay) == 0 {
		return true
	}
	wd := d.weekday()
	for _, b := range r.byDay {
		if b.wd == wd {
			return true
		}
	}
	return false
}

// monthDays are the days of one month the rule selects: BYMONTHDAY and
// BYDAY (its ordinals counted within the month), both when both are set,
// else the day of the month DTSTART falls on.
func (r *rrule) monthDays(y, m, defaultDay int) []civil {
	n := daysIn(y, m)
	var byMD, byD []int
	for _, md := range r.byMonthDay {
		switch {
		case md > 0 && md <= n:
			byMD = append(byMD, md)
		case md < 0 && n+md+1 >= 1:
			byMD = append(byMD, n+md+1)
		}
	}
	if len(r.byDay) > 0 {
		byD = ordinalDays(civil{y, m, 1}, n, r.byDay)
	}
	var days []int
	switch {
	case len(r.byMonthDay) > 0 && len(r.byDay) > 0:
		for _, d := range byMD {
			if slices.Contains(byD, d) {
				days = append(days, d)
			}
		}
	case len(r.byMonthDay) > 0:
		days = byMD
	case len(r.byDay) > 0:
		days = byD
	default:
		if defaultDay <= n {
			days = []int{defaultDay}
		}
	}
	slices.Sort(days)
	days = slices.Compact(days)
	out := make([]civil, 0, len(days))
	for _, d := range days {
		out = append(out, civil{y, m, d})
	}
	return out
}

// ordinalDays are the day numbers, counted from first (day 1 of a span of
// n days), of each BYDAY entry: every such weekday for a plain entry, the
// n-th from the start or the end for an ordinal one.
func ordinalDays(first civil, n int, byDay []weekdayNum) []int {
	var out []int
	for _, b := range byDay {
		var matches []int
		off := (int(b.wd) - int(first.weekday()) + 7) % 7
		for d := 1 + off; d <= n; d += 7 {
			matches = append(matches, d)
		}
		switch {
		case b.n == 0:
			out = append(out, matches...)
		case b.n > 0 && b.n <= len(matches):
			out = append(out, matches[b.n-1])
		case b.n < 0 && -b.n <= len(matches):
			out = append(out, matches[len(matches)+b.n])
		}
	}
	return out
}

// yearDays are the days of one year the rule selects.
func (r *rrule) yearDays(y int, first civil) []civil {
	var out []civil
	switch {
	case len(r.byYearDay) > 0:
		n := daysInYear(y)
		jan1 := civil{y, 1, 1}
		for _, yd := range r.byYearDay {
			i := yd
			if yd < 0 {
				i = n + yd + 1
			}
			if i < 1 || i > n {
				continue
			}
			d := jan1.addDays(i - 1)
			if r.monthOK(d.m) && r.weekdayOK(d) && r.monthDayOK(d) {
				out = append(out, d)
			}
		}
	case len(r.byDay) > 0 && len(r.byMonth) == 0 && len(r.byMonthDay) == 0:
		jan1 := civil{y, 1, 1}
		for _, i := range ordinalDays(jan1, daysInYear(y), r.byDay) {
			out = append(out, jan1.addDays(i-1))
		}
	case len(r.byMonthDay) > 0 || len(r.byDay) > 0:
		months := r.byMonth
		if len(months) == 0 {
			months = []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
		}
		for _, m := range months {
			out = append(out, r.monthDays(y, m, first.d)...)
		}
	default:
		months := r.byMonth
		if len(months) == 0 {
			months = []int{first.m}
		}
		for _, m := range months {
			if first.d <= daysIn(y, m) {
				out = append(out, civil{y, m, first.d})
			}
		}
	}
	slices.SortFunc(out, func(a, b civil) int {
		switch {
		case a.less(b):
			return -1
		case b.less(a):
			return 1
		}
		return 0
	})
	return slices.Compact(out)
}
