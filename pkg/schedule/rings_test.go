package schedule

import (
	"reflect"
	"slices"
	"testing"
	"time"
)

// Every occurrence in the window has a ring at its own local minute, in
// the schedule's own zone and in another whose daylight-saving changes
// fall on other days (the superset fire narrows, D4).
func TestLocalRingsCoverEveryOccurrence(t *testing.T) {
	start := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	schedules := []Recurrence{
		{Kind: Daily, Time: "09:00"},
		{Kind: Daily, Time: "02:30"},
		{Kind: Weekdays, Time: "08:15"},
		{Kind: Weekly, Days: []string{"tue", "sun"}, Time: "22:00"},
		{Kind: Weekly, Days: []string{"sun"}, Time: "02:30"},
		{Kind: Monthly, MonthDay: 31, Time: "23:30"},
		{Kind: Monthly, MonthDay: 1, Time: "00:30"},
		{Kind: Monthly, MonthDay: 15, Time: "12:00"},
	}
	for _, zone := range []string{"America/New_York", "Europe/London", "Australia/Sydney"} {
		for _, local := range []string{"America/New_York", "Europe/London", "Asia/Kolkata"} {
			loc := mustLoc(t, local)
			for _, r := range schedules {
				r.Zone, r.Start = zone, start
				rs, err := LocalRingsFor(r, loc, start)
				if err != nil {
					t.Fatal(err)
				}
				occ := r.Between(start, start.AddDate(oneOffYears, 0, 0), 0)
				if len(occ) == 0 {
					t.Fatalf("%s in %s: no occurrences", r.Describe(), local)
				}
				for _, o := range occ {
					if !rs.RingsAt(o.In(loc)) {
						t.Fatalf("%s on a %s computer: no ring at %s (%v)", r.Describe(), local, o.In(loc), rs)
					}
				}
			}
		}
	}
}

func TestLocalRingsShapes(t *testing.T) {
	start := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	london := mustLoc(t, "Europe/London")
	cases := []struct {
		r    Recurrence
		want LocalRings
	}{
		{Recurrence{Kind: Daily, Time: "09:00"}, LocalRings{Repeat: []LocalRing{{Hour: 9}}}},
		{Recurrence{Kind: Weekdays, Time: "09:00"}, LocalRings{Repeat: []LocalRing{{Hour: 9,
			Weekdays: []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}}}}},
		{Recurrence{Kind: Weekly, Days: []string{"tue", "thu"}, Time: "22:00"}, LocalRings{Repeat: []LocalRing{{Hour: 22,
			Weekdays: []time.Weekday{time.Tuesday, time.Thursday}}}}},
		{Recurrence{Kind: Monthly, MonthDay: 5, Time: "09:30"}, LocalRings{Repeat: []LocalRing{{Hour: 9, Minute: 30, MonthDays: []int{5}}}}},
		{Recurrence{Kind: Monthly, MonthDay: 31, Time: "09:00"}, LocalRings{Repeat: []LocalRing{{Hour: 9, MonthDays: []int{28, 29, 30, 31}}}}},
		{Recurrence{Kind: Once, Date: "2026-10-20", Time: "08:00"}, LocalRings{Once: []time.Time{time.Date(2026, 10, 20, 8, 0, 0, 0, london)}}},
	}
	for _, c := range cases {
		c.r.Zone, c.r.Start = "Europe/London", start
		got, err := LocalRingsFor(c.r, london, start)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Repeat, c.want.Repeat) || len(got.Once) != len(c.want.Once) {
			t.Errorf("%s: %+v, want %+v", c.r.Describe(), got, c.want)
			continue
		}
		for i := range got.Once {
			if !got.Once[i].Equal(c.want.Once[i]) {
				t.Errorf("%s: once %v, want %v", c.r.Describe(), got.Once, c.want.Once)
			}
		}
	}
}

// The spring change skips 02:30 in New York: that day's occurrence (at
// 03:00, the first valid minute after) is a one-off ring of its own, not a
// weekly ring at 03:00 all year, for the next three years.
func TestLocalRingsSkippedTimeIsAOneOff(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	start := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	r := Recurrence{Kind: Daily, Time: "02:30", Zone: "America/New_York", Start: start}
	got, err := LocalRingsFor(r, ny, start)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Repeat, []LocalRing{{Hour: 2, Minute: 30}}) {
		t.Fatalf("repeat %+v", got.Repeat)
	}
	// Three years of them, as the systemd timer carries (DR-83).
	want := []time.Time{time.Date(2027, 3, 14, 3, 0, 0, 0, ny), time.Date(2028, 3, 12, 3, 0, 0, 0, ny), time.Date(2029, 3, 11, 3, 0, 0, 0, ny)}
	if !slices.EqualFunc(got.Once, want, time.Time.Equal) {
		t.Fatalf("once %v, want %v", got.Once, want)
	}
}

// A schedule in a zone whose changes fall on other days than this
// computer's rings at both local times its occurrences take.
func TestLocalRingsAcrossZones(t *testing.T) {
	london := mustLoc(t, "Europe/London")
	start := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	r := Recurrence{Kind: Daily, Time: "09:00", Zone: "America/New_York", Start: start}
	got, err := LocalRingsFor(r, london, start)
	if err != nil {
		t.Fatal(err)
	}
	// 14:00 in London most of the year; 13:00 while one side has changed
	// and the other has not (two weeks in March, one in autumn).
	want := []LocalRing{{Hour: 13}, {Hour: 14}}
	if !reflect.DeepEqual(got.Repeat, want) || len(got.Once) != 0 {
		t.Fatalf("rings %+v, want %+v", got, want)
	}
}

// A once schedule rings at its occurrence even when the computer only
// comes back after it: fire then decides (D5).
func TestLocalRingsOncePast(t *testing.T) {
	start := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	r := Recurrence{Kind: Once, Date: "2026-10-09", Time: "08:00", Zone: "UTC", Start: start}
	got, err := LocalRingsFor(r, time.UTC, start.AddDate(0, 0, 5))
	if err != nil || len(got.Once) != 1 || !got.Once[0].Equal(time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("rings %+v %v", got, err)
	}
}
