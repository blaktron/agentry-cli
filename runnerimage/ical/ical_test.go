package ical

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func parse(t *testing.T, body string) *Calendar {
	t.Helper()
	c, err := Parse(strings.NewReader(strings.ReplaceAll(body, "\n", "\r\n")), 0)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return c
}

func starts(ex Expansion) []string {
	var out []string
	for _, o := range ex.Occurrences {
		out = append(out, o.Start.Format("2006-01-02 15:04 MST")+" "+o.Summary)
	}
	return out
}

func wantStarts(t *testing.T, ex Expansion, want ...string) {
	t.Helper()
	got := starts(ex)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("occurrences:\n got %q\nwant %q", got, want)
	}
}

func TestAWeeklySeriesWithAnExceptionAndAnOverride(t *testing.T) {
	london := mustLoc(t, "Europe/London")
	c := parse(t, `BEGIN:VCALENDAR
VERSION:2.0
X-WR-CALNAME:Work
BEGIN:VEVENT
UID:standup@example
SUMMARY:Stand-up
DTSTART;TZID=Europe/London:20261005T093000
DTEND;TZID=Europe/London:20261005T094500
RRULE:FREQ=WEEKLY;BYDAY=MO,WE
EXDATE;TZID=Europe/London:20261014T093000
END:VEVENT
BEGIN:VEVENT
UID:standup@example
RECURRENCE-ID;TZID=Europe/London:20261019T093000
SUMMARY:Stand-up (moved)
DTSTART;TZID=Europe/London:20261019T110000
DTEND;TZID=Europe/London:20261019T111500
END:VEVENT
END:VCALENDAR`)
	if c.Name != "Work" {
		t.Errorf("Name = %q", c.Name)
	}
	ex := c.Expand(time.Date(2026, 10, 12, 0, 0, 0, 0, london), time.Date(2026, 10, 22, 0, 0, 0, 0, london), london)
	wantStarts(t, ex,
		"2026-10-12 09:30 BST Stand-up",
		"2026-10-19 11:00 BST Stand-up (moved)",
		"2026-10-21 09:30 BST Stand-up",
	)
	for _, o := range ex.Occurrences {
		if !o.Recurring || o.End.Sub(o.Start) != 15*time.Minute {
			t.Errorf("%v: recurring %v, length %v", o.Summary, o.Recurring, o.End.Sub(o.Start))
		}
	}
}

func TestASeriesKeepsItsWallClockAcrossADaylightSavingChange(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	london := mustLoc(t, "Europe/London")
	c := parse(t, `BEGIN:VCALENDAR
BEGIN:VEVENT
UID:a
SUMMARY:Sync
DTSTART;TZID=America/New_York:20261026T090000
DURATION:PT1H
RRULE:FREQ=DAILY;COUNT=10
END:VEVENT
END:VCALENDAR`)
	// New York leaves daylight saving on 1 November 2026: 09:00 there is
	// 13:00 UTC before and 14:00 UTC after.
	ex := c.Expand(time.Date(2026, 10, 31, 0, 0, 0, 0, ny), time.Date(2026, 11, 3, 0, 0, 0, 0, ny), london)
	var utc []string
	for _, o := range ex.Occurrences {
		utc = append(utc, o.Start.UTC().Format("01-02 15:04"))
	}
	if got, want := strings.Join(utc, ","), "10-31 13:00,11-01 14:00,11-02 14:00"; got != want {
		t.Errorf("UTC starts = %s, want %s", got, want)
	}
}

func TestAnAllDayEventSpansItsDays(t *testing.T) {
	oslo := mustLoc(t, "Europe/Oslo")
	c := parse(t, `BEGIN:VCALENDAR
BEGIN:VEVENT
UID:h
SUMMARY:Holiday
DTSTART;VALUE=DATE:20261028
DTEND;VALUE=DATE:20261031
TRANSP:TRANSPARENT
END:VEVENT
END:VCALENDAR`)
	ex := c.Expand(time.Date(2026, 10, 30, 0, 0, 0, 0, oslo), time.Date(2026, 10, 31, 0, 0, 0, 0, oslo), oslo)
	if len(ex.Occurrences) != 1 {
		t.Fatalf("occurrences = %d", len(ex.Occurrences))
	}
	o := ex.Occurrences[0]
	if !o.AllDay || o.Busy || o.Start.Format("2006-01-02") != "2026-10-28" || o.End.Format("2006-01-02") != "2026-10-31" {
		t.Errorf("all-day occurrence = %+v", o)
	}
	// A yearly all-day series.
	c = parse(t, `BEGIN:VCALENDAR
BEGIN:VEVENT
UID:b
SUMMARY:Birthday
DTSTART;VALUE=DATE:19900302
RRULE:FREQ=YEARLY
END:VEVENT
END:VCALENDAR`)
	ex = c.Expand(time.Date(2026, 1, 1, 0, 0, 0, 0, oslo), time.Date(2028, 1, 1, 0, 0, 0, 0, oslo), oslo)
	wantStarts(t, ex, "2026-03-02 00:00 CET Birthday", "2027-03-02 00:00 CET Birthday")
}

func TestOutlooksWindowsZoneNamesAndItsOwnZones(t *testing.T) {
	utc := time.UTC
	c := parse(t, `BEGIN:VCALENDAR
BEGIN:VTIMEZONE
TZID:W. Europe Standard Time
BEGIN:STANDARD
DTSTART:16010101T030000
TZOFFSETFROM:+0200
TZOFFSETTO:+0100
RRULE:FREQ=YEARLY;INTERVAL=1;BYDAY=-1SU;BYMONTH=10
END:STANDARD
BEGIN:DAYLIGHT
DTSTART:16010101T020000
TZOFFSETFROM:+0100
TZOFFSETTO:+0200
RRULE:FREQ=YEARLY;INTERVAL=1;BYDAY=-1SU;BYMONTH=3
END:DAYLIGHT
END:VTIMEZONE
BEGIN:VTIMEZONE
TZID:Customized Time Zone
BEGIN:STANDARD
DTSTART:16010101T000000
TZOFFSETFROM:+0530
TZOFFSETTO:+0530
END:STANDARD
END:VTIMEZONE
BEGIN:VEVENT
UID:w
SUMMARY:Review
DTSTART;TZID=W. Europe Standard Time:20261027T100000
DTEND;TZID=W. Europe Standard Time:20261027T110000
END:VEVENT
BEGIN:VEVENT
UID:c
SUMMARY:Call
DTSTART;TZID=Customized Time Zone:20261027T150000
DTEND;TZID=Customized Time Zone:20261027T153000
END:VEVENT
END:VCALENDAR`)
	ex := c.Expand(time.Date(2026, 10, 27, 0, 0, 0, 0, utc), time.Date(2026, 10, 28, 0, 0, 0, 0, utc), utc)
	var got []string
	for _, o := range ex.Occurrences {
		got = append(got, o.Summary+" "+o.Start.UTC().Format("15:04"))
	}
	if strings.Join(got, ",") != "Review 09:00,Call 09:30" {
		t.Errorf("starts in UTC = %v", got)
	}
	if len(ex.Notes) != 1 || !strings.Contains(ex.Notes[0], "Customized Time Zone") {
		t.Errorf("notes = %q", ex.Notes)
	}
}

func TestMonthlyRules(t *testing.T) {
	utc := time.UTC
	c := parse(t, `BEGIN:VCALENDAR
BEGIN:VEVENT
UID:1
SUMMARY:Second Tuesday
DTSTART:20260113T170000Z
DTEND:20260113T180000Z
RRULE:FREQ=MONTHLY;BYDAY=2TU
END:VEVENT
BEGIN:VEVENT
UID:2
SUMMARY:Last weekday
DTSTART:20260130T160000Z
DTEND:20260130T163000Z
RRULE:FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1;UNTIL=20261231T235959Z
END:VEVENT
BEGIN:VEVENT
UID:3
SUMMARY:On the 31st
DTSTART:20260131T080000Z
RRULE:FREQ=MONTHLY;INTERVAL=1
END:VEVENT
END:VCALENDAR`)
	ex := c.Expand(time.Date(2026, 10, 1, 0, 0, 0, 0, utc), time.Date(2026, 12, 1, 0, 0, 0, 0, utc), utc)
	wantStarts(t, ex,
		"2026-10-13 17:00 UTC Second Tuesday",
		"2026-10-30 16:00 UTC Last weekday",
		"2026-10-31 08:00 UTC On the 31st",
		"2026-11-10 17:00 UTC Second Tuesday",
		"2026-11-30 16:00 UTC Last weekday",
	)
}

func TestCountAndUntilEndASeries(t *testing.T) {
	utc := time.UTC
	c := parse(t, `BEGIN:VCALENDAR
BEGIN:VEVENT
UID:1
SUMMARY:Course
DTSTART:20261001T100000Z
RRULE:FREQ=WEEKLY;INTERVAL=2;COUNT=3
END:VEVENT
BEGIN:VEVENT
UID:2
SUMMARY:Trial
DTSTART;VALUE=DATE:20261001
RRULE:FREQ=DAILY;UNTIL=20261003
END:VEVENT
END:VCALENDAR`)
	ex := c.Expand(time.Date(2026, 9, 1, 0, 0, 0, 0, utc), time.Date(2027, 1, 1, 0, 0, 0, 0, utc), utc)
	wantStarts(t, ex,
		"2026-10-01 00:00 UTC Trial",
		"2026-10-01 10:00 UTC Course",
		"2026-10-02 00:00 UTC Trial",
		"2026-10-03 00:00 UTC Trial",
		"2026-10-15 10:00 UTC Course",
		"2026-10-29 10:00 UTC Course",
	)
}

func TestCancelledEventsAndInstancesAreLeftOut(t *testing.T) {
	utc := time.UTC
	c := parse(t, `BEGIN:VCALENDAR
BEGIN:VEVENT
UID:1
SUMMARY:Gone
STATUS:CANCELLED
DTSTART:20261027T100000Z
END:VEVENT
BEGIN:VEVENT
UID:2
SUMMARY:Daily
DTSTART:20261026T120000Z
DTEND:20261026T123000Z
RRULE:FREQ=DAILY;COUNT=3
END:VEVENT
BEGIN:VEVENT
UID:2
RECURRENCE-ID:20261027T120000Z
STATUS:CANCELLED
SUMMARY:Daily
DTSTART:20261027T120000Z
END:VEVENT
END:VCALENDAR`)
	ex := c.Expand(time.Date(2026, 10, 26, 0, 0, 0, 0, utc), time.Date(2026, 10, 30, 0, 0, 0, 0, utc), utc)
	wantStarts(t, ex, "2026-10-26 12:00 UTC Daily", "2026-10-28 12:00 UTC Daily")
}

func TestAFreeBusyOnlyFeed(t *testing.T) {
	c := parse(t, `BEGIN:VCALENDAR
BEGIN:VEVENT
UID:1
SUMMARY:Busy
DTSTART:20261027T100000Z
DTEND:20261027T110000Z
END:VEVENT
BEGIN:VEVENT
UID:2
SUMMARY:Tentative
DTSTART:20261027T130000Z
DTEND:20261027T140000Z
END:VEVENT
END:VCALENDAR`)
	if !c.BusyOnly() {
		t.Error("BusyOnly = false for a free/busy feed")
	}
	c = parse(t, `BEGIN:VCALENDAR
BEGIN:VEVENT
UID:1
SUMMARY:Dentist
DTSTART:20261027T100000Z
END:VEVENT
END:VCALENDAR`)
	if c.BusyOnly() {
		t.Error("BusyOnly = true for a feed with titles")
	}
}

func TestFreeSlots(t *testing.T) {
	utc := time.UTC
	at := func(h, m int) time.Time { return time.Date(2026, 10, 29, h, m, 0, 0, utc) }
	occs := []Occurrence{
		{Start: at(9, 30), End: at(10, 0), Busy: true},
		{Start: at(9, 45), End: at(11, 0), Busy: true},
		{Start: at(12, 0), End: at(12, 20), Busy: true},
		{Start: at(13, 0), End: at(15, 0), Busy: false},
		{Start: at(16, 30), End: at(18, 0), Busy: true},
	}
	got := FreeSlots(occs, []Slot{{at(9, 0), at(17, 0)}}, 30*time.Minute)
	var s []string
	for _, f := range got {
		s = append(s, f.Start.Format("15:04")+"-"+f.End.Format("15:04"))
	}
	if strings.Join(s, ",") != "09:00-09:30,11:00-12:00,12:20-16:30" {
		t.Errorf("free = %v", s)
	}
}

func TestFoldedAndEscapedText(t *testing.T) {
	c := parse(t, "BEGIN:VCALENDAR\nBEGIN:VEVENT\nUID:1\nSUMMARY:Lunch\\, then\n  a walk\nDESCRIPTION:line one\\nline two\\; more\nLOCATION;LANGUAGE=en:\"Café: upstairs\"\nORGANIZER;CN=\"Sam Lee\":mailto:sam@example.com\nDTSTART:20261027T120000Z\nEND:VEVENT\nEND:VCALENDAR")
	e := c.Events[0]
	if e.Summary != "Lunch, then a walk" || e.Description != "line one\nline two; more" || e.Organizer != "Sam Lee" || e.Location != `"Café: upstairs"` {
		t.Errorf("event = %+v", e)
	}
}

func TestMalformedAndOversizedFeeds(t *testing.T) {
	if _, err := Parse(strings.NewReader("<html>Sign in</html>"), 0); !errors.Is(err, ErrNotCalendar) {
		t.Errorf("an HTML page: err = %v", err)
	}
	if _, err := Parse(strings.NewReader("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nEND:VCALENDAR\r\n"), 0); err == nil {
		t.Error("mismatched END: no error")
	}
	big := "BEGIN:VCALENDAR\r\n" + strings.Repeat("X-PAD:"+strings.Repeat("a", 1000)+"\r\n", 200) + "END:VCALENDAR\r\n"
	if _, err := Parse(strings.NewReader(big), 100<<10); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("an oversized feed: err = %v", err)
	}
	if _, err := Parse(strings.NewReader(big), 0); err != nil {
		t.Errorf("the same feed unbounded: %v", err)
	}
	// An event with a bad date is skipped, not fatal.
	c := parse(t, "BEGIN:VCALENDAR\nBEGIN:VEVENT\nUID:1\nDTSTART:2026-10-27\nEND:VEVENT\nEND:VCALENDAR")
	if len(c.Events) != 0 {
		t.Errorf("events = %d", len(c.Events))
	}
}

func TestARuleThatCannotBeFollowedShowsItsFirstInstance(t *testing.T) {
	utc := time.UTC
	c := parse(t, `BEGIN:VCALENDAR
BEGIN:VEVENT
UID:1
SUMMARY:Ping
DTSTART:20261027T100000Z
RRULE:FREQ=MINUTELY;INTERVAL=5
END:VEVENT
END:VCALENDAR`)
	ex := c.Expand(time.Date(2026, 10, 27, 0, 0, 0, 0, utc), time.Date(2026, 10, 28, 0, 0, 0, 0, utc), utc)
	wantStarts(t, ex, "2026-10-27 10:00 UTC Ping")
	if len(ex.Notes) != 1 || !strings.Contains(ex.Notes[0], "MINUTELY") {
		t.Errorf("notes = %q", ex.Notes)
	}
}

func TestARuleThatNeverYieldsEnds(t *testing.T) {
	utc := time.UTC
	c := parse(t, `BEGIN:VCALENDAR
BEGIN:VEVENT
UID:1
SUMMARY:Never
DTSTART:20260101T100000Z
RRULE:FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30
END:VEVENT
END:VCALENDAR`)
	done := make(chan Expansion, 1)
	go func() {
		done <- c.Expand(time.Date(2026, 1, 1, 0, 0, 0, 0, utc), time.Date(2027, 1, 1, 0, 0, 0, 0, utc), utc)
	}()
	select {
	case ex := <-done:
		if len(ex.Occurrences) != 0 {
			t.Errorf("occurrences = %v", starts(ex))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Expand did not return")
	}
}
