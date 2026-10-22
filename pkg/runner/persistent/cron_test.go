package persistent

import (
	"testing"
	"time"
)

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// TestCronNext: each shape of field names the times cron names.
func TestCronNext(t *testing.T) {
	toronto := mustZone(t, "America/Toronto")
	from := time.Date(2026, 10, 21, 14, 3, 30, 0, toronto) // a Wednesday
	cases := []struct {
		expr string
		want time.Time
	}{
		{"* * * * *", time.Date(2026, 10, 21, 14, 4, 0, 0, toronto)},
		{"0 8 * * 1-5", time.Date(2026, 10, 22, 8, 0, 0, 0, toronto)},
		{"*/15 * * * *", time.Date(2026, 10, 21, 14, 15, 0, 0, toronto)},
		{"30 9 1 * *", time.Date(2026, 11, 1, 9, 30, 0, 0, toronto)},
		{"0 9 * * 0", time.Date(2026, 10, 25, 9, 0, 0, 0, toronto)},
		{"0 9 * * 7", time.Date(2026, 10, 25, 9, 0, 0, 0, toronto)},
		{"0 12 25 12 *", time.Date(2026, 12, 25, 12, 0, 0, 0, toronto)},
		{"5,35 14 * * *", time.Date(2026, 10, 21, 14, 5, 0, 0, toronto)},
		{"10-12/2 15 * * *", time.Date(2026, 10, 21, 15, 10, 0, 0, toronto)},
		// Either day: the 1st of the month or a Friday.
		{"0 7 1 * 5", time.Date(2026, 10, 23, 7, 0, 0, 0, toronto)},
	}
	for _, c := range cases {
		cr, err := ParseCron(c.expr)
		if err != nil {
			t.Fatalf("%q: %v", c.expr, err)
		}
		got, err := cr.Next(from, toronto)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("%q: next after %s = %s (%v), want %s", c.expr, from, got, err, c.want)
		}
	}
}

// TestCronAcrossSummerTime: a minute the clocks skip does not come, and a
// repeated one comes once.
func TestCronAcrossSummerTime(t *testing.T) {
	toronto := mustZone(t, "America/Toronto")
	cr, _ := ParseCron("30 2 * * *")
	// 8 March 2026: 02:00 becomes 03:00.
	got, _ := cr.Next(time.Date(2026, 3, 7, 12, 0, 0, 0, toronto), toronto)
	if got.Day() != 9 || got.Hour() != 2 || got.Minute() != 30 {
		t.Errorf("a skipped minute: %s, want 9 March 02:30", got)
	}
	cr, _ = ParseCron("30 1 * * *")
	// 1 November 2026: 01:30 comes twice.
	first, _ := cr.Next(time.Date(2026, 11, 1, 0, 0, 0, 0, toronto), toronto)
	second, _ := cr.Next(first, toronto)
	if second.Day() != 2 {
		t.Errorf("a repeated minute came twice: %s then %s", first, second)
	}
}

func TestCronRefusesWhatIsNotAnExpression(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8", "*/0 * * * *", "5-1 * * * *", "a * * * *", "* * * * * *"} {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("%q was taken", expr)
		}
	}
	cr, _ := ParseCron("0 0 31 2 *")
	if _, err := cr.Next(time.Now(), time.UTC); err == nil {
		t.Error("31 February came")
	}
}

func TestCronCloserThanAndCount(t *testing.T) {
	from := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	for expr, want := range map[string]bool{"*/15 * * * *": false, "*/10 * * * *": true, "0,50 * * * *": true, "0 8 * * *": false, "55 23 * * *": false} {
		cr, _ := ParseCron(expr)
		if got, _ := cr.CloserThan(from, time.UTC, 15*time.Minute); got != want {
			t.Errorf("%q closer than 15 minutes: %v, want %v", expr, got, want)
		}
	}
	cr, _ := ParseCron("*/15 * * * *")
	if n := cr.Count(from, from.Add(24*time.Hour), time.UTC, 200); n != 96 {
		t.Errorf("every 15 minutes fires %d times a day, want 96", n)
	}
}
