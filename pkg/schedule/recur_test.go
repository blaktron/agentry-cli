package schedule

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

func at(t *testing.T, zone, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", s, mustLoc(t, zone))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestNextShapes(t *testing.T) {
	const z = "Europe/London"
	start := at(t, z, "2026-10-08 12:00") // a Thursday
	cases := []struct {
		name  string
		r     Recurrence
		after string
		want  string // "" = none
	}{
		{"daily later today", Recurrence{Kind: Daily, Time: "18:00"}, "2026-10-08 12:00", "2026-10-08 18:00"},
		{"daily tomorrow", Recurrence{Kind: Daily, Time: "09:00"}, "2026-10-08 12:00", "2026-10-09 09:00"},
		{"weekdays skip the weekend", Recurrence{Kind: Weekdays, Time: "09:00"}, "2026-10-09 10:00", "2026-10-12 09:00"},
		{"weekly tuesday 10pm", Recurrence{Kind: Weekly, Days: []string{"tue"}, Time: "22:00"}, "2026-10-08 12:00", "2026-10-13 22:00"},
		{"weekly two days", Recurrence{Kind: Weekly, Days: []string{"tue", "fri"}, Time: "07:30"}, "2026-10-08 12:00", "2026-10-09 07:30"},
		{"monthly 15th", Recurrence{Kind: Monthly, MonthDay: 15, Time: "09:00"}, "2026-10-08 12:00", "2026-10-15 09:00"},
		{"monthly 31st in november is the 30th", Recurrence{Kind: Monthly, MonthDay: 31, Time: "09:00"}, "2026-10-31 10:00", "2026-11-30 09:00"},
		{"monthly 30th in february is the 28th", Recurrence{Kind: Monthly, MonthDay: 30, Time: "09:00"}, "2027-01-30 10:00", "2027-02-28 09:00"},
		{"once ahead", Recurrence{Kind: Once, Date: "2026-10-20", Time: "08:00"}, "2026-10-08 12:00", "2026-10-20 08:00"},
		{"once passed", Recurrence{Kind: Once, Date: "2026-10-20", Time: "08:00"}, "2026-10-20 08:00", ""},
		{"end on a date includes it", Recurrence{Kind: Daily, Time: "09:00", End: End{Kind: EndOn, On: "2026-10-10"}}, "2026-10-09 10:00", "2026-10-10 09:00"},
		{"end on a date stops after it", Recurrence{Kind: Daily, Time: "09:00", End: End{Kind: EndOn, On: "2026-10-10"}}, "2026-10-10 10:00", ""},
		{"end after 2: the second", Recurrence{Kind: Daily, Time: "09:00", End: End{Kind: EndAfter, After: 2}}, "2026-10-09 10:00", "2026-10-10 09:00"},
		{"end after 2: none after", Recurrence{Kind: Daily, Time: "09:00", End: End{Kind: EndAfter, After: 2}}, "2026-10-10 10:00", ""},
		{"nothing before the start", Recurrence{Kind: Daily, Time: "09:00"}, "2026-10-01 10:00", "2026-10-09 09:00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := c.r
			r.Zone, r.Start = z, start
			if err := r.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			got, ok := r.Next(at(t, z, c.after))
			if c.want == "" {
				if ok {
					t.Fatalf("Next = %v, want none", got)
				}
				return
			}
			if !ok || !got.Equal(at(t, z, c.want)) {
				t.Fatalf("Next = %v %v, want %s", got.In(mustLoc(t, z)), ok, c.want)
			}
		})
	}
}

// The DST table (plan §4): 9am stays 9am, a skipped time is the first
// valid minute after it, a repeated time fires once at its first instance.
func TestNextAcrossDaylightSaving(t *testing.T) {
	cases := []struct {
		name, zone, time, after string
		wantUTC                 string
	}{
		// London: BST ends 2026-10-25 02:00 → 01:00; 9am is 08:00Z before, 09:00Z after.
		{"london 9am before the change", "Europe/London", "09:00", "2026-10-24 10:00", "2026-10-25T09:00:00Z"},
		{"london 9am the day before", "Europe/London", "09:00", "2026-10-23 10:00", "2026-10-24T08:00:00Z"},
		// 01:30 London happens twice on 2026-10-25: the first is 00:30Z.
		{"london repeated 01:30 fires first", "Europe/London", "01:30", "2026-10-24 12:00", "2026-10-25T00:30:00Z"},
		{"london repeated 01:30 only once", "Europe/London", "01:30", "2026-10-25 01:45", "2026-10-26T01:30:00Z"},
		// New York: EDT starts 2027-03-14 02:00 → 03:00; 02:30 does not exist.
		{"new york skipped 02:30 is 03:00", "America/New_York", "02:30", "2027-03-13 12:00", "2027-03-14T07:00:00Z"},
		{"new york 02:30 the next day", "America/New_York", "02:30", "2027-03-14 04:00", "2027-03-15T06:30:00Z"},
		// New York: EDT ends 2026-11-01 02:00 → 01:00; 01:15 twice, first is 05:15Z.
		{"new york repeated 01:15 fires first", "America/New_York", "01:15", "2026-10-31 12:00", "2026-11-01T05:15:00Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Recurrence{Kind: Daily, Time: c.time, Zone: c.zone, Start: at(t, c.zone, "2026-10-01 00:00")}
			got, ok := r.Next(at(t, c.zone, c.after))
			want, _ := time.Parse(time.RFC3339, c.wantUTC)
			if !ok || !got.Equal(want) {
				t.Fatalf("Next = %v, want %v", got.UTC(), want)
			}
		})
	}
}

func TestBetween(t *testing.T) {
	const z = "UTC"
	r := Recurrence{Kind: Weekdays, Time: "09:00", Zone: z, Start: at(t, z, "2026-10-01 00:00")}
	got := r.Between(at(t, z, "2026-10-09 00:00"), at(t, z, "2026-10-14 00:00"), 0)
	want := []string{"2026-10-09 09:00", "2026-10-12 09:00", "2026-10-13 09:00"}
	if len(got) != len(want) {
		t.Fatalf("Between = %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(at(t, z, want[i])) {
			t.Fatalf("Between[%d] = %v, want %s", i, got[i], want[i])
		}
	}
	if n := len(r.Between(at(t, z, "2026-10-01 00:00"), at(t, z, "2026-12-01 00:00"), 5)); n != 5 {
		t.Fatalf("limit: got %d", n)
	}
}

func TestValidateRefuses(t *testing.T) {
	start := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	bad := []Recurrence{
		{Kind: Daily, Time: "9am", Zone: "UTC", Start: start},
		{Kind: Daily, Time: "09:00", Zone: "Mars/Olympus", Start: start},
		{Kind: Daily, Time: "09:00", Zone: "", Start: start},
		{Kind: Weekly, Time: "09:00", Zone: "UTC", Start: start},
		{Kind: Monthly, MonthDay: 32, Time: "09:00", Zone: "UTC", Start: start},
		{Kind: Once, Date: "tomorrow", Time: "09:00", Zone: "UTC", Start: start},
		{Kind: Once, Date: "2026-10-09", Time: "09:00", Zone: "UTC", Start: start, End: End{Kind: EndAfter, After: 2}},
		{Kind: Daily, Time: "09:00", Zone: "UTC", Start: start, End: End{Kind: EndAfter}},
		{Kind: "hourly", Time: "09:00", Zone: "UTC", Start: start},
		{Kind: Daily, Time: "09:00", Zone: "UTC"},
	}
	for i, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("case %d (%+v): validated", i, r)
		}
	}
}

func TestParseDays(t *testing.T) {
	got, err := ParseDays("Thursday, tue,TUE")
	if err != nil || len(got) != 2 || got[0] != "tue" || got[1] != "thu" {
		t.Fatalf("ParseDays = %v, %v", got, err)
	}
	if _, err := ParseDays("funday"); err == nil {
		t.Fatal("funday parsed")
	}
}
