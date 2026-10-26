package toolsrv

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The feed's address, which no answer may hold.
const testFeedURL = "https://calendar.google.com/calendar/ical/person%40example.com/private-0123456789abcdef0123456789abcdef/basic.ics"

const testFeedBody = "BEGIN:VCALENDAR\r\n" +
	"X-WR-CALNAME:Person\r\n" +
	"BEGIN:VEVENT\r\nUID:1\r\nSUMMARY:Dentist\r\nLOCATION:High Street\r\n" +
	"DESCRIPTION:Subscribe at " + testFeedURL + "\r\n" +
	"DTSTART;TZID=Europe/London:20261029T100000\r\nDTEND;TZID=Europe/London:20261029T110000\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:2\r\nSUMMARY:Stand-up\r\n" +
	"DTSTART;TZID=Europe/London:20261026T093000\r\nDURATION:PT15M\r\nRRULE:FREQ=DAILY;BYDAY=MO,TU,WE,TH,FR\r\nEND:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// feedTransport answers every request with handler's answer and counts
// them.
type feedTransport struct {
	handler func(*http.Request) (*http.Response, error)
	calls   int
}

func (f *feedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.calls++
	return f.handler(r)
}

func answer(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func calendarServer(t *testing.T, url string, handler func(*http.Request) (*http.Response, error)) (*server, *feedTransport) {
	t.Helper()
	t.Setenv("AGENTRY_CALENDAR_FEED_1", url)
	ft := &feedTransport{handler: handler}
	old, oldNow := calendarTransport, calendarNow
	calendarTransport = func() http.RoundTripper { return ft }
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	calendarNow = func() time.Time { return now }
	t.Cleanup(func() { calendarTransport, calendarNow = old, oldNow })
	spec := baseSpec(t)
	spec.Calendar = Calendar{TimeZone: "Europe/London", Feeds: []CalendarFeed{{Name: "Work", Host: "calendar.google.com", URLEnv: "AGENTRY_CALENDAR_FEED_1"}}}
	return newTestServer(t, spec), ft
}

func call(t *testing.T, s *server, tool string, args map[string]any) (string, bool) {
	t.Helper()
	d, ok := s.tools[tool]
	if !ok {
		t.Fatalf("%s is not granted", tool)
	}
	return s.callTool(d, rawArgs(t, args))
}

func TestTheCalendarToolsExistOnlyWithAFeed(t *testing.T) {
	spec := baseSpec(t)
	if strings.Contains(strings.Join(spec.GrantedTools(), ","), "calendar") {
		t.Errorf("tools with no feed: %v", spec.GrantedTools())
	}
	spec.Calendar = Calendar{Feeds: []CalendarFeed{{Name: "Work", Host: "calendar.google.com", URLEnv: "AGENTRY_CALENDAR_FEED_1"}}}
	got := strings.Join(spec.GrantedTools(), ",")
	if got != "calendar_feeds,calendar_events,calendar_free_time,write_report" {
		t.Errorf("tools with a feed: %s", got)
	}
}

func TestCalendarEventsAnswersWithoutTheAddress(t *testing.T) {
	s, ft := calendarServer(t, testFeedURL, func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != testFeedURL {
			t.Errorf("fetched %s", r.URL)
		}
		return answer(200, testFeedBody), nil
	})
	text, isErr := call(t, s, ToolCalendarEvents, map[string]any{"from": "2026-10-29", "to": "2026-10-30"})
	if isErr {
		t.Fatalf("calendar_events failed: %s", text)
	}
	for _, want := range []string{"Feed Work: fetched Tue 27 Oct 08:00 (just now)", "Times are in Europe/London.", "Thu 29 Oct 2026",
		"- 09:30–09:45 Stand-up [repeats]", "- 10:00–11:00 Dentist", "Where: High Street", "[redacted:AGENTRY_CALENDAR_FEED_1]"} {
		if !strings.Contains(text, want) {
			t.Errorf("answer lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "private-0123456789abcdef") {
		t.Errorf("the answer holds the address:\n%s", text)
	}
	// A second call within the cache's time is answered from it.
	if _, isErr := call(t, s, ToolCalendarEvents, map[string]any{"query": "dentist", "from": "2026-10-29", "to": "2026-10-30"}); isErr || ft.calls != 1 {
		t.Errorf("second call: error %v, fetches %d", isErr, ft.calls)
	}
	feeds, _ := call(t, s, ToolCalendarFeeds, map[string]any{})
	if !strings.Contains(feeds, `- Work (Google Calendar): last fetched Tue 27 Oct 08:00 (just now), the calendar "Person"`) || strings.Contains(feeds, "private-") {
		t.Errorf("calendar_feeds:\n%s", feeds)
	}
	calendarNow = func() time.Time { return time.Date(2026, 10, 27, 8, 6, 0, 0, time.UTC) }
	if _, isErr := call(t, s, ToolCalendarEvents, map[string]any{}); isErr || ft.calls != 2 {
		t.Errorf("after the cache: error %v, fetches %d", isErr, ft.calls)
	}
}

func TestCalendarFreeTime(t *testing.T) {
	s, _ := calendarServer(t, testFeedURL, func(*http.Request) (*http.Response, error) { return answer(200, testFeedBody), nil })
	text, isErr := call(t, s, ToolCalendarFreeTime, map[string]any{"from": "2026-10-29", "to": "2026-10-30", "min_minutes": 60})
	if isErr {
		t.Fatalf("calendar_free_time failed: %s", text)
	}
	if !strings.Contains(text, "- 11:00–17:00 (6h00)") || strings.Contains(text, "09:00–09:30") || strings.Contains(text, "09:45–10:00") {
		t.Errorf("free time:\n%s", text)
	}
	text, _ = call(t, s, ToolCalendarFreeTime, map[string]any{"from": "2026-10-31", "to": "2026-11-02"})
	if !strings.Contains(text, " none.") {
		t.Errorf("a weekend's free time on weekdays only:\n%s", text)
	}
}

func TestAFeedReachesItsOwnHostAlone(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		handler   func(*http.Request) (*http.Response, error)
		want      string
	}{
		{"plain http", "http://calendar.google.com/x.ics", nil, "not an https address"},
		{"another host", "https://evil.example/x.ics", nil, "not an https address on calendar.google.com"},
		{"a redirect away", testFeedURL, func(r *http.Request) (*http.Response, error) {
			resp := answer(302, "")
			resp.Header.Set("Location", "https://collector.example/steal")
			resp.Request = r
			return resp, nil
		}, "redirected to collector.example"},
		{"a reset address", testFeedURL, func(*http.Request) (*http.Response, error) { return answer(404, "gone"), nil }, "may have been reset"},
		{"not a calendar", testFeedURL, func(*http.Request) (*http.Response, error) { return answer(200, "<html>sign in</html>"), nil }, "could not be read as a calendar"},
		{"a transport error naming the address", testFeedURL, func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("proxy refused " + r.URL.String())
		}, "the fetch failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ft := &feedTransport{handler: tc.handler}
			if tc.handler == nil {
				ft.handler = func(*http.Request) (*http.Response, error) {
					t.Error("a request was made")
					return answer(200, testFeedBody), nil
				}
			}
			s, _ := calendarServer(t, tc.url, ft.handler)
			text, isErr := call(t, s, ToolCalendarEvents, map[string]any{})
			if !isErr || !strings.Contains(text, tc.want) {
				t.Errorf("error %v, answer:\n%s", isErr, text)
			}
			if strings.Contains(text, "private-0123") || strings.Contains(text, "/x.ics") {
				t.Errorf("the answer holds the address:\n%s", text)
			}
		})
	}
}

func TestCalendarWindows(t *testing.T) {
	s, _ := calendarServer(t, testFeedURL, func(*http.Request) (*http.Response, error) { return answer(200, testFeedBody), nil })
	for args, want := range map[string]map[string]any{
		"to must be after from":         {"from": "2026-10-29", "to": "2026-10-28"},
		"longer than 366 days":          {"from": "2026-01-01", "to": "2027-06-01"},
		"is not a date":                 {"from": "next Tuesday"},
		"not an IANA zone":              {"time_zone": "Mars/Olympus"},
		`no calendar feed is named "x"`: {"feed": "x"},
	} {
		text, isErr := call(t, s, ToolCalendarEvents, want)
		if !isErr || !strings.Contains(text, args) {
			t.Errorf("%v: error %v, answer %s", want, isErr, text)
		}
	}
}

func TestValidateSpecCalendar(t *testing.T) {
	feed := CalendarFeed{Name: "Work", Host: "calendar.google.com", URLEnv: "AGENTRY_CALENDAR_FEED_1"}
	for _, tc := range []struct {
		cal  Calendar
		want string
	}{
		{Calendar{Feeds: []CalendarFeed{feed}, TimeZone: "Europe/Oslo"}, ""},
		{Calendar{Feeds: []CalendarFeed{{Name: "", Host: feed.Host, URLEnv: feed.URLEnv}}}, "name"},
		{Calendar{Feeds: []CalendarFeed{{Name: "a\nb", Host: feed.Host, URLEnv: feed.URLEnv}}}, "printable"},
		{Calendar{Feeds: []CalendarFeed{{Name: "W", Host: "calendar.google.com:8443", URLEnv: feed.URLEnv}}}, "not a host name"},
		{Calendar{Feeds: []CalendarFeed{{Name: "W", Host: feed.Host, URLEnv: "PATH"}}}, "not a calendar feed variable"},
		{Calendar{Feeds: []CalendarFeed{feed, {Name: "work", Host: feed.Host, URLEnv: "AGENTRY_CALENDAR_FEED_2"}}}, "named twice"},
		{Calendar{Feeds: []CalendarFeed{feed}, TimeZone: "Local"}, "not an IANA zone"},
	} {
		spec := baseSpec(t)
		spec.Calendar = tc.cal
		err := validateSpec(spec)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%+v: err = %v, want %q", tc.cal, err, tc.want)
		}
	}
}

func TestACalendarAddressIsHeldOutOfWrittenFiles(t *testing.T) {
	s, _ := calendarServer(t, testFeedURL, func(*http.Request) (*http.Response, error) { return answer(200, testFeedBody), nil })
	if got := s.holdsRunSecret("see " + testFeedURL); got != "AGENTRY_CALENDAR_FEED_1" {
		t.Errorf("holdsRunSecret = %q", got)
	}
}
