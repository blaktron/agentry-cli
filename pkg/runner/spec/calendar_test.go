package spec

import (
	"strings"
	"testing"
)

func TestCalendarFeedAddress(t *testing.T) {
	for raw, want := range map[string]string{
		"https://calendar.google.com/calendar/ical/a%40b.com/private-x/basic.ics": "calendar.google.com",
		"webcal://p01-caldav.icloud.com/published/2/abc":                          "p01-caldav.icloud.com",
		"WEBCALS://Outlook.Office365.com/owa/calendar/x/y/calendar.ics":           "outlook.office365.com",
		"https://calendar.example.org:443/feed.ics#frag":                          "calendar.example.org",
	} {
		addr, host, err := CalendarFeedAddress(raw)
		if err != nil || host != want || !strings.HasPrefix(addr, "https://"+want+"/") || strings.Contains(addr, "#") {
			t.Errorf("%s: %q %q %v", raw, addr, host, err)
		}
	}
	for raw, want := range map[string]string{
		"":                                    "empty",
		"http://calendar.google.com/x.ics":    "only https",
		"ftp://calendar.google.com/x.ics":     "not an https or webcal",
		"https://user:pw@calendar.example/x":  "user name or password",
		"https://calendar.example:8443/x.ics": "names a port",
		"https://10.0.0.1/x.ics":              "by its name",
		"https://[::1]/x.ics":                 "by its name",
		"https://localhost/x.ics":             "by its name",
		"https://calendar.example/x y.ics":    "a space or a control",
		"https://calendar.example/" + strings.Repeat("a", MaxCalendarFeedURL): "longer than",
	} {
		_, _, err := CalendarFeedAddress(raw)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.60s: err = %v, want %q", raw, err, want)
			continue
		}
		if raw != "" && strings.Contains(err.Error(), raw) {
			t.Errorf("the error repeats the address: %v", err)
		}
	}
}

func TestCheckCalendarFeedName(t *testing.T) {
	for _, ok := range []string{"Work", "Family & friends", "Équipe (Paris)", strings.Repeat("a", 64)} {
		if err := CheckCalendarFeedName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", " Work", "a:b", "a,b", "a\nb", "a‮b", strings.Repeat("a", 65), `a"b`} {
		if err := CheckCalendarFeedName(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestCheckCalendarGrant(t *testing.T) {
	g := &CalendarGrant{Feeds: []CalendarFeed{{Name: "Work", Host: "calendar.google.com", URLEnv: CalendarFeedEnv(0)}}}
	if err := CheckCalendarGrant(g); err != nil {
		t.Fatal(err)
	}
	if CalendarFeedEnv(1) != "AGENTRY_CALENDAR_FEED_2" {
		t.Errorf("CalendarFeedEnv(1) = %s", CalendarFeedEnv(1))
	}
	for name, bad := range map[string]*CalendarGrant{
		"no feeds":     {},
		"a bad env":    {Feeds: []CalendarFeed{{Name: "W", Host: "calendar.google.com", URLEnv: "HOME"}}},
		"an address":   {Feeds: []CalendarFeed{{Name: "W", Host: "192.168.1.1", URLEnv: CalendarFeedEnv(0)}}},
		"named twice":  {Feeds: []CalendarFeed{{Name: "W", Host: "a.example", URLEnv: CalendarFeedEnv(0)}, {Name: "w", Host: "b.example", URLEnv: CalendarFeedEnv(1)}}},
		"an env twice": {Feeds: []CalendarFeed{{Name: "W", Host: "a.example", URLEnv: CalendarFeedEnv(0)}, {Name: "X", Host: "b.example", URLEnv: CalendarFeedEnv(0)}}},
	} {
		if err := CheckCalendarGrant(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if hosts := (&CalendarGrant{Feeds: []CalendarFeed{{Host: "a.example"}, {Host: "b.example"}, {Host: "a.example"}}}).Hosts(); strings.Join(hosts, ",") != "a.example,b.example" {
		t.Errorf("Hosts = %v", hosts)
	}
}
