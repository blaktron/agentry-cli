// Package ical reads an iCalendar (RFC 5545) feed, the ICS document a
// calendar service publishes at a subscription address, and expands its
// events into the occurrences that fall in a window: recurrences (RRULE,
// RDATE, EXDATE), the instances a feed overrides (RECURRENCE-ID), all-day
// events and the time zones events are written in.
//
// It reads; it never writes a calendar. It is part of the runner image's
// embedded build context, so it is stdlib-only.
package ical

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	_ "time/tzdata" // the tools image may carry no zoneinfo; feeds name IANA zones
)

// Limits on what one feed may hold, so a hostile or broken feed cannot
// make the reader work without end.
const (
	// MaxLineBytes is the longest unfolded content line read.
	MaxLineBytes = 256 << 10
	// MaxEvents is the most VEVENTs one feed may hold.
	MaxEvents = 50000
)

// ErrNotCalendar is returned for a document that is not an iCalendar
// object: no BEGIN:VCALENDAR.
var ErrNotCalendar = errors.New("not an iCalendar feed: no BEGIN:VCALENDAR")

// Calendar is one parsed feed.
type Calendar struct {
	// Name is X-WR-CALNAME, when the feed gives one.
	Name string
	// TimeZone is X-WR-TIMEZONE, when the feed gives one: the zone its
	// floating times are meant in.
	TimeZone string
	// Events are the feed's VEVENTs: the series' masters and the
	// instances they override, in feed order.
	Events []Event

	zones map[string]*vtimezone
	// freeBusy says the feed holds VFREEBUSY blocks, read as events titled
	// Busy.
	freeBusy bool
}

// Event is one VEVENT.
type Event struct {
	UID         string
	Summary     string
	Location    string
	Description string
	URL         string
	Organizer   string
	// Status is TENTATIVE, CONFIRMED or CANCELLED (upper case), or empty.
	Status string
	// Transparent is TRANSP:TRANSPARENT: the event does not block time.
	Transparent bool

	Start    dateTime
	End      *dateTime
	Duration time.Duration
	hasDur   bool

	RRule   string
	RDates  []dateTime
	ExDates []dateTime
	// RecurrenceID, when set, makes this event an override of the
	// instance of UID's series that would have started then.
	RecurrenceID *dateTime
}

// dateTime is a DATE or DATE-TIME value as written: its wall clock, and
// whether it is UTC, floating or in a named zone.
type dateTime struct {
	Y, Mo, D, H, Mi, S int
	// DateOnly is VALUE=DATE (an all-day value).
	DateOnly bool
	// UTC is a DATE-TIME ending in Z.
	UTC bool
	// TZID names the zone of a DATE-TIME that is neither UTC nor floating.
	TZID string
}

// Parse reads an iCalendar document. It reads at most maxBytes (zero means
// unbounded; the caller bounds the fetch itself as well).
func Parse(r io.Reader, maxBytes int64) (*Calendar, error) {
	if maxBytes > 0 {
		r = io.LimitReader(r, maxBytes+1)
	}
	lr := &countingReader{r: r}
	lines, err := unfold(lr)
	if err != nil {
		return nil, err
	}
	if maxBytes > 0 && lr.n > maxBytes {
		return nil, fmt.Errorf("the feed is larger than %d bytes", maxBytes)
	}
	return parseLines(lines)
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// unfold joins RFC 5545's folded lines: a line that starts with a space or
// a tab continues the one before it, less that first character.
func unfold(r io.Reader) ([]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLineBytes)
	var lines []string
	for sc.Scan() {
		l := strings.TrimRight(sc.Text(), "\r")
		if l == "" {
			continue
		}
		if (l[0] == ' ' || l[0] == '\t') && len(lines) > 0 {
			last := len(lines) - 1
			if len(lines[last])+len(l) > MaxLineBytes {
				return nil, fmt.Errorf("a content line is longer than %d bytes", MaxLineBytes)
			}
			lines[last] += l[1:]
			continue
		}
		lines = append(lines, l)
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, fmt.Errorf("a content line is longer than %d bytes", MaxLineBytes)
		}
		return nil, err
	}
	return lines, nil
}

// property is one content line: NAME;PARAM=VALUE...:VALUE.
type property struct {
	Name   string
	Params map[string]string
	Value  string
}

// parseProperty splits a content line at its first colon outside a quoted
// parameter value.
func parseProperty(line string) (property, bool) {
	inQuote := false
	colon := -1
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inQuote = !inQuote
		case ':':
			if !inQuote {
				colon = i
			}
		}
		if colon >= 0 {
			break
		}
	}
	if colon < 0 {
		return property{}, false
	}
	head, value := line[:colon], line[colon+1:]
	parts := splitOutsideQuotes(head, ';')
	p := property{Name: strings.ToUpper(strings.TrimSpace(parts[0])), Value: value}
	for _, kv := range parts[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if p.Params == nil {
			p.Params = map[string]string{}
		}
		p.Params[strings.ToUpper(strings.TrimSpace(k))] = strings.Trim(v, `"`)
	}
	return p, p.Name != ""
}

func splitOutsideQuotes(s string, sep byte) []string {
	var out []string
	inQuote := false
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case sep:
			if !inQuote {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// unescapeText undoes TEXT escaping: \n, \N, \, \; and \\.
func unescapeText(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n', 'N':
			b.WriteByte('\n')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func parseLines(lines []string) (*Calendar, error) {
	cal := &Calendar{zones: map[string]*vtimezone{}}
	var stack []string
	var ev *Event
	var tz *vtimezone
	var obs *observance
	sawCalendar := false
	for _, line := range lines {
		p, ok := parseProperty(line)
		if !ok {
			continue
		}
		switch p.Name {
		case "BEGIN":
			comp := strings.ToUpper(strings.TrimSpace(p.Value))
			stack = append(stack, comp)
			switch {
			case comp == "VCALENDAR":
				sawCalendar = true
			case comp == "VEVENT" && len(stack) == 2:
				if len(cal.Events) >= MaxEvents {
					return nil, fmt.Errorf("the feed holds more than %d events", MaxEvents)
				}
				ev = &Event{}
			case comp == "VTIMEZONE" && len(stack) == 2:
				tz = &vtimezone{}
			case comp == "VFREEBUSY" && len(stack) == 2:
				cal.freeBusy = true
			case (comp == "STANDARD" || comp == "DAYLIGHT") && tz != nil:
				obs = &observance{daylight: comp == "DAYLIGHT"}
			}
			continue
		case "END":
			comp := strings.ToUpper(strings.TrimSpace(p.Value))
			if len(stack) == 0 || stack[len(stack)-1] != comp {
				return nil, fmt.Errorf("END:%s does not close BEGIN:%s", comp, last(stack))
			}
			stack = stack[:len(stack)-1]
			switch {
			case comp == "VEVENT" && ev != nil && len(stack) == 1:
				if ev.Start.Y != 0 {
					cal.Events = append(cal.Events, *ev)
				}
				ev = nil
			case comp == "VTIMEZONE" && tz != nil:
				if tz.id != "" {
					cal.zones[tz.id] = tz
				}
				tz = nil
			case (comp == "STANDARD" || comp == "DAYLIGHT") && obs != nil && tz != nil:
				tz.observances = append(tz.observances, *obs)
				obs = nil
			}
			continue
		}
		if !sawCalendar {
			continue
		}
		switch {
		case ev != nil && len(stack) == 2:
			setEventProperty(ev, p)
		case len(stack) == 2 && stack[1] == "VFREEBUSY" && p.Name == "FREEBUSY":
			if err := cal.addFreeBusy(p); err != nil {
				return nil, err
			}
		case obs != nil:
			setObservanceProperty(obs, p)
		case tz != nil:
			switch p.Name {
			case "TZID":
				tz.id = p.Value
			case "X-LIC-LOCATION":
				tz.location = p.Value
			}
		case len(stack) == 1:
			switch p.Name {
			case "X-WR-CALNAME":
				cal.Name = unescapeText(p.Value)
			case "X-WR-TIMEZONE":
				cal.TimeZone = strings.TrimSpace(p.Value)
			}
		}
	}
	if !sawCalendar {
		return nil, ErrNotCalendar
	}
	return cal, nil
}

func last(s []string) string {
	if len(s) == 0 {
		return "nothing"
	}
	return s[len(s)-1]
}

func setEventProperty(ev *Event, p property) {
	switch p.Name {
	case "UID":
		ev.UID = p.Value
	case "SUMMARY":
		ev.Summary = unescapeText(p.Value)
	case "LOCATION":
		ev.Location = unescapeText(p.Value)
	case "DESCRIPTION":
		ev.Description = unescapeText(p.Value)
	case "URL":
		ev.URL = p.Value
	case "ORGANIZER":
		if cn := p.Params["CN"]; cn != "" {
			ev.Organizer = cn
		} else {
			ev.Organizer = strings.TrimPrefix(strings.TrimPrefix(p.Value, "mailto:"), "MAILTO:")
		}
	case "STATUS":
		ev.Status = strings.ToUpper(strings.TrimSpace(p.Value))
	case "TRANSP":
		ev.Transparent = strings.EqualFold(strings.TrimSpace(p.Value), "TRANSPARENT")
	case "DTSTART":
		if dt, err := parseDateTime(p); err == nil {
			ev.Start = dt
		}
	case "DTEND":
		if dt, err := parseDateTime(p); err == nil {
			ev.End = &dt
		}
	case "DURATION":
		if d, err := parseDuration(p.Value); err == nil {
			ev.Duration, ev.hasDur = d, true
		}
	case "RRULE":
		ev.RRule = p.Value
	case "RDATE":
		if strings.EqualFold(p.Params["VALUE"], "PERIOD") {
			for _, v := range strings.Split(p.Value, ",") {
				start, _, _ := strings.Cut(v, "/")
				if dt, err := parseDateTimeValue(start, p.Params); err == nil {
					ev.RDates = append(ev.RDates, dt)
				}
			}
			return
		}
		ev.RDates = append(ev.RDates, parseDateTimeList(p)...)
	case "EXDATE":
		ev.ExDates = append(ev.ExDates, parseDateTimeList(p)...)
	case "RECURRENCE-ID":
		if dt, err := parseDateTime(p); err == nil {
			ev.RecurrenceID = &dt
		}
	}
}

func parseDateTimeList(p property) []dateTime {
	var out []dateTime
	for _, v := range strings.Split(p.Value, ",") {
		if dt, err := parseDateTimeValue(strings.TrimSpace(v), p.Params); err == nil {
			out = append(out, dt)
		}
	}
	return out
}

func parseDateTime(p property) (dateTime, error) {
	return parseDateTimeValue(strings.TrimSpace(p.Value), p.Params)
}

// parseDateTimeValue reads YYYYMMDD or YYYYMMDDTHHMMSS[Z]. A DATE value is
// taken as one whether or not VALUE=DATE says so, as feeds often omit it.
func parseDateTimeValue(v string, params map[string]string) (dateTime, error) {
	var dt dateTime
	digits := func(s string) (int, bool) {
		n := 0
		for i := 0; i < len(s); i++ {
			if s[i] < '0' || s[i] > '9' {
				return 0, false
			}
			n = n*10 + int(s[i]-'0')
		}
		return n, len(s) > 0
	}
	if len(v) < 8 {
		return dt, fmt.Errorf("bad date %q", v)
	}
	var ok1, ok2, ok3 bool
	dt.Y, ok1 = digits(v[0:4])
	dt.Mo, ok2 = digits(v[4:6])
	dt.D, ok3 = digits(v[6:8])
	if !ok1 || !ok2 || !ok3 || dt.Mo < 1 || dt.Mo > 12 || dt.D < 1 || dt.D > 31 {
		return dt, fmt.Errorf("bad date %q", v)
	}
	if len(v) == 8 {
		dt.DateOnly = true
		return dt, nil
	}
	if len(v) < 15 || v[8] != 'T' {
		return dt, fmt.Errorf("bad date-time %q", v)
	}
	dt.H, ok1 = digits(v[9:11])
	dt.Mi, ok2 = digits(v[11:13])
	dt.S, ok3 = digits(v[13:15])
	if !ok1 || !ok2 || !ok3 || dt.H > 24 || dt.Mi > 59 || dt.S > 60 {
		return dt, fmt.Errorf("bad date-time %q", v)
	}
	switch rest := v[15:]; rest {
	case "Z", "z":
		dt.UTC = true
	case "":
		dt.TZID = strings.TrimSpace(params["TZID"])
	default:
		return dt, fmt.Errorf("bad date-time %q", v)
	}
	return dt, nil
}

// parseDuration reads an RFC 5545 duration: [+-]P[nW] or [+-]P[nD][T[nH][nM][nS]].
func parseDuration(v string) (time.Duration, error) {
	s := strings.TrimSpace(strings.ToUpper(v))
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	if !strings.HasPrefix(s, "P") || len(s) < 3 {
		return 0, fmt.Errorf("bad duration %q", v)
	}
	s = s[1:]
	var d time.Duration
	inTime := false
	n := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			if n < 0 {
				n = 0
			}
			n = n*10 + int(c-'0')
			if n > 1_000_000 {
				return 0, fmt.Errorf("bad duration %q", v)
			}
			continue
		case c == 'T':
			inTime = true
			continue
		}
		if n < 0 {
			return 0, fmt.Errorf("bad duration %q", v)
		}
		switch {
		case c == 'W' && !inTime:
			d += time.Duration(n) * 7 * 24 * time.Hour
		case c == 'D' && !inTime:
			d += time.Duration(n) * 24 * time.Hour
		case c == 'H' && inTime:
			d += time.Duration(n) * time.Hour
		case c == 'M' && inTime:
			d += time.Duration(n) * time.Minute
		case c == 'S' && inTime:
			d += time.Duration(n) * time.Second
		default:
			return 0, fmt.Errorf("bad duration %q", v)
		}
		n = -1
	}
	if n >= 0 {
		return 0, fmt.Errorf("bad duration %q", v)
	}
	if neg {
		d = -d
	}
	return d, nil
}

// addFreeBusy reads a FREEBUSY property's periods (start/end or
// start/duration, in UTC) as events titled Busy; a FREE period is not one.
func (c *Calendar) addFreeBusy(p property) error {
	if t := strings.ToUpper(p.Params["FBTYPE"]); t == "FREE" {
		return nil
	}
	for _, period := range strings.Split(p.Value, ",") {
		a, b, ok := strings.Cut(strings.TrimSpace(period), "/")
		if !ok {
			continue
		}
		start, err := parseDateTimeValue(a, nil)
		if err != nil || start.DateOnly {
			continue
		}
		ev := Event{UID: fmt.Sprintf("freebusy-%d", len(c.Events)+1), Summary: "Busy", Start: start}
		if strings.HasPrefix(strings.ToUpper(b), "P") || strings.HasPrefix(b, "+P") {
			d, err := parseDuration(b)
			if err != nil {
				continue
			}
			ev.Duration, ev.hasDur = d, true
		} else {
			end, err := parseDateTimeValue(b, nil)
			if err != nil {
				continue
			}
			ev.End = &end
		}
		if len(c.Events) >= MaxEvents {
			return fmt.Errorf("the feed holds more than %d events", MaxEvents)
		}
		c.Events = append(c.Events, ev)
	}
	return nil
}
