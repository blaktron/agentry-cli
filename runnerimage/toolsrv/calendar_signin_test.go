package toolsrv

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	testSessionCookie = "sid-value-0123456789abcdef"
	testSession       = `[{"n":"SID","v":"` + testSessionCookie + `","d":".google.com","p":"/"},{"n":"OSID","v":"osid-value-0123456789","d":"calendar.google.com"}]`
)

// signedInServer is calendarServer with the feed signed in through a
// browser.
func signedInServer(t *testing.T, handler func(*http.Request) (*http.Response, error)) *server {
	t.Helper()
	s, _ := calendarServer(t, testFeedURL, handler)
	t.Setenv("AGENTRY_CALENDAR_SESSION_1", testSession)
	s.spec.Calendar.Feeds[0].SessionEnv = "AGENTRY_CALENDAR_SESSION_1"
	return s
}

func TestASignedInFeedSendsItsCookiesAndKeepsARenewedOne(t *testing.T) {
	var sent []string
	s := signedInServer(t, func(r *http.Request) (*http.Response, error) {
		sent = append(sent, r.Header.Get("Cookie"))
		resp := answer(200, testFeedBody)
		resp.Header.Add("Set-Cookie", "SID=renewed-0123456789; Domain=.google.com; Path=/; Secure")
		resp.Request = r
		return resp, nil
	})
	if text, isErr := call(t, s, ToolCalendarEvents, map[string]any{}); isErr {
		t.Fatal(text)
	}
	if len(sent) != 1 || !strings.Contains(sent[0], "SID="+testSessionCookie) || !strings.Contains(sent[0], "OSID=osid-value-0123456789") {
		t.Fatalf("first fetch sent %q", sent)
	}
	later := calendarNow().Add(CalendarCacheFor + time.Minute)
	calendarNow = func() time.Time { return later }
	if text, isErr := call(t, s, ToolCalendarEvents, map[string]any{}); isErr {
		t.Fatal(text)
	}
	if len(sent) != 2 || !strings.Contains(sent[1], "SID=renewed-0123456789") || strings.Contains(sent[1], testSessionCookie) {
		t.Fatalf("second fetch sent %q", sent[1:])
	}
}

func TestAnEndedSignInSaysToSignInAgain(t *testing.T) {
	signInPage := func(r *http.Request) (*http.Response, error) {
		resp := answer(302, "")
		resp.Header.Set("Location", "https://accounts.google.com/ServiceLogin?continue=https%3A%2F%2Fcalendar.google.com")
		resp.Request = r
		return resp, nil
	}
	s := signedInServer(t, signInPage)
	text, isErr := call(t, s, ToolCalendarEvents, map[string]any{})
	if !isErr || !strings.Contains(text, "sign-in has ended") || !strings.Contains(text, `--calendar-feed "Work" --calendar-sign-in`) {
		t.Errorf("an ended sign-in: %s", text)
	}
	if strings.Contains(text, "private-0123") || strings.Contains(text, testSessionCookie) {
		t.Errorf("the answer holds the address or the sign-in: %s", text)
	}
	// A feed with no sign-in that asks for one says so.
	s2, _ := calendarServer(t, testFeedURL, signInPage)
	if text, isErr := call(t, s2, ToolCalendarEvents, map[string]any{}); !isErr || !strings.Contains(text, "asks for a sign-in") {
		t.Errorf("a feed that asks for a sign-in: %s", text)
	}
}

func TestASignInIsMaskedAndHeldOutOfWrittenFiles(t *testing.T) {
	s := signedInServer(t, func(*http.Request) (*http.Response, error) { return answer(200, testFeedBody), nil })
	for _, text := range []string{"cookie " + testSessionCookie, "whole " + testSession} {
		if got := s.scrubStoredSecrets(text); strings.Contains(got, testSessionCookie) || !strings.Contains(got, "[redacted:AGENTRY_CALENDAR_SESSION_1]") {
			t.Errorf("scrub %q = %q", text, got)
		}
		if got := s.holdsRunSecret(text); got != "AGENTRY_CALENDAR_SESSION_1" {
			t.Errorf("holdsRunSecret(%q) = %q", text, got)
		}
	}
}

func TestParseCalendarSession(t *testing.T) {
	cookies, err := ParseCalendarSession(testSession)
	if err != nil || len(cookies) != 2 || cookies[0].Domain != ".google.com" || cookies[1].Path != "/" || !cookies[1].Secure {
		t.Fatalf("%+v %v", cookies, err)
	}
	for _, bad := range []string{"", "[]", "SID=x", `[{"n":"bad name","v":"x"}]`, `[{"n":"A","v":"semi;colon"}]`} {
		if _, err := ParseCalendarSession(bad); err == nil {
			t.Errorf("%q was read", bad)
		}
	}
	if h, err := CalendarSessionHeader(testSession); err != nil || h != "SID="+testSessionCookie+"; OSID=osid-value-0123456789" {
		t.Errorf("header %q %v", h, err)
	}
}

func TestValidateSpecCalendarSession(t *testing.T) {
	spec := baseSpec(t)
	spec.Calendar = Calendar{Feeds: []CalendarFeed{{Name: "Work", Host: "calendar.google.com", URLEnv: "AGENTRY_CALENDAR_FEED_1", SessionEnv: "AGENTRY_CALENDAR_SESSION_1"}}}
	if err := validateCalendar(spec); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"AGENTRY_CALENDAR_FEED_1", "HOME", "AGENTRY_CALENDAR_SESSION_0"} {
		spec.Calendar.Feeds[0].SessionEnv = env
		if err := validateCalendar(spec); err == nil {
			t.Errorf("sessionEnv %s was taken", env)
		}
	}
}
