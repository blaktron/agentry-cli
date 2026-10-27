package persistent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/browsersignin"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// A browser sign-in's cookie, which no file may hold.
const sessionCookie = "signed-in-cookie-5a4b3c2d1e0f"

func testSession(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal([]map[string]string{{"n": "SID", "v": sessionCookie, "d": ".google.com", "p": "/"}, {"n": "OSID", "v": "osid-0123456789", "d": "calendar.google.com"}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestACalendarSessionIsKeptInPartsAWindowsCredentialHolds(t *testing.T) {
	keys := fakeSecrets{}
	long := `[{"n":"A","v":"` + strings.Repeat("x", 4500) + `"}]`
	if err := KeepCalendarSession(keys, "assistant", "Work", long); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		v, ok := keys[CalendarSessionAccount("assistant", "Work", i)]
		if !ok || len(v) > 2560 {
			t.Errorf("part %d: present %v, %d bytes", i, ok, len(v))
		}
	}
	if got, err := CalendarSession(keys, "assistant", "Work"); err != nil || got != long {
		t.Fatalf("read back %d bytes, %v", len(got), err)
	}
	// A shorter one leaves no part of the longer one behind.
	short := testSession(t)
	if err := KeepCalendarSession(keys, "assistant", "Work", short); err != nil {
		t.Fatal(err)
	}
	if _, left := keys[CalendarSessionAccount("assistant", "Work", 2)]; left {
		t.Error("a part of the longer sign-in was left")
	}
	if got, _ := CalendarSession(keys, "assistant", "Work"); got != short {
		t.Errorf("read back %q", got)
	}
	// One exactly a part long reads back whole.
	exact := strings.Repeat("y", calendarSessionPart)
	if err := KeepCalendarSession(keys, "assistant", "Work", exact); err != nil {
		t.Fatal(err)
	}
	if got, _ := CalendarSession(keys, "assistant", "Work"); got != exact {
		t.Errorf("a part-long sign-in read back %d bytes", len(got))
	}
	if err := KeepCalendarSession(keys, "assistant", "Work", strings.Repeat("z", calendarSessionPart*calendarSessionParts+1)); err == nil {
		t.Error("a sign-in past the bound was kept")
	}
	keys[CalendarKeychainAccount("assistant", "Work")] = feedAddress
	if err := ForgetCalendarFeed(keys, "assistant", "Work"); err != nil || len(keys) != 0 {
		t.Errorf("ForgetCalendarFeed left %v (%v)", keys, err)
	}
}

func TestAFeedThatAsksForASignInSaysSoAndReadsWithIt(t *testing.T) {
	const body = "BEGIN:VCALENDAR\r\nX-WR-CALNAME:Work\r\nBEGIN:VEVENT\r\nUID:1\r\nSUMMARY:A\r\nDTSTART:20261027T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("SID"); err == nil && c.Value == sessionCookie {
			_, _ = io.WriteString(w, body)
			return
		}
		http.Redirect(w, r, "https://accounts.google.com/ServiceLogin?continue="+r.URL.String(), http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	old := calendarCheckTransport
	calendarCheckTransport = srv.Client().Transport
	t.Cleanup(func() { calendarCheckTransport = old })

	_, _, err := CalendarFeedReadable(context.Background(), srv.URL+"/private-feed.ics")
	if !errors.Is(err, ErrCalendarFeedSignIn) || !errors.Is(err, ErrCalendarFeedUnreadable) || !strings.Contains(err.Error(), "accounts.google.com") || strings.Contains(err.Error(), "private-feed") {
		t.Fatalf("no sign-in: %v", err)
	}
	name, n, err := CalendarFeedReadableWith(context.Background(), srv.URL+"/private-feed.ics", testSession(t))
	if err != nil || name != "Work" || n != 1 {
		t.Fatalf("signed in: %q %d %v", name, n, err)
	}
	if _, _, err := CalendarFeedReadableWith(context.Background(), srv.URL+"/x.ics", "not json"); !errors.Is(err, ErrCalendarFeedUnreadable) {
		t.Errorf("an unreadable sign-in: %v", err)
	}

	// The browser's sign-in: the window's cookies are checked against the
	// address until it reads, then kept with their domains.
	oldSignIn := calendarSignIn
	t.Cleanup(func() { calendarSignIn = oldSignIn })
	calendarSignIn = func(ctx context.Context, o browsersignin.Options) ([]browsersignin.Cookie, error) {
		if o.URL != srv.URL+"/private-feed.ics" {
			t.Errorf("opened %s", o.URL)
		}
		for _, try := range [][]browsersignin.Cookie{
			{{Name: "NID", Value: "before-0123456789", Domain: ".google.com", Path: "/"}},
			{{Name: "SID", Value: sessionCookie, Domain: ".google.com", Path: "/"}},
		} {
			if ok, err := o.Ready(ctx, browsersignin.Header(try)); err != nil || ok {
				if ok {
					return try, nil
				}
				return nil, err
			}
		}
		return nil, browsersignin.ErrClosed
	}
	session, name, n, err := SignInCalendarFeed(context.Background(), srv.URL+"/private-feed.ics", nil)
	if err != nil || name != "Work" || n != 1 {
		t.Fatalf("SignInCalendarFeed: %q %d %v", name, n, err)
	}
	if session != `[{"n":"SID","v":"`+sessionCookie+`","d":".google.com","p":"/"}]` {
		t.Errorf("session %s", session)
	}
	// Closed before the calendar read, the window's last error is named,
	// unless it was still the sign-in page.
	calendarSignIn = func(ctx context.Context, o browsersignin.Options) ([]browsersignin.Cookie, error) {
		_, _ = o.Ready(ctx, "NID=x")
		return nil, browsersignin.ErrClosed
	}
	if _, _, _, err := SignInCalendarFeed(context.Background(), srv.URL+"/x.ics", nil); !errors.Is(err, browsersignin.ErrClosed) || strings.Contains(err.Error(), "still could not be read") {
		t.Errorf("closed on the sign-in page: %v", err)
	}
}

func TestRosterGivesASignedInFeedItsSessionAlone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	a := assistantAgent(t)
	f := workFeed()
	f.SignedIn = true
	keys := fakeSecrets{CalendarKeychainAccount("assistant", "Work"): feedAddress}
	if err := KeepCalendarSession(keys, "assistant", "Work", testSession(t)); err != nil {
		t.Fatal(err)
	}
	c := Config{Enabled: true}
	if _, err := ApplyChange(home, a, &c, Change{SetCalendarFeed: &f, Consent: true}); err != nil {
		t.Fatal(err)
	}
	e, _ := BuildRoster(a, c, nil).Entry(CalendarEntryID("Work"))
	if !strings.Contains(e.Line, "signed in as you") || !strings.Contains(e.Line, "nowhere else") {
		t.Errorf("the consent line: %s", e.Line)
	}
	if st := CalendarFeeds(a, c); len(st) != 1 || !st[0].SignedIn {
		t.Errorf("state %+v", st)
	}
	gr, _, done, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, keys)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if len(gr.CalendarFeeds) != 1 || gr.CalendarFeeds[0] != (localrun.CalendarFeed{Name: "Work", Host: "calendar.google.com", URL: feedAddress, Session: testSession(t)}) {
		t.Fatalf("feeds %+v", gr.CalendarFeeds)
	}
	sp, err := localrun.Segment{Builtin: a.Builtin, Home: home, Policy: policy.Default(), PolicySHA: "s", Build: "test", Grants: gr}.Plan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if g := sp.Plan.Capabilities.Calendar; g == nil || g.Feeds[0].SessionEnv != "AGENTRY_CALENDAR_SESSION_1" {
		t.Errorf("the plan's grant: %+v", g)
	}
	cat, err := os.ReadFile(filepath.Join(sp.Dir, "gateway", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cat), "agentry-tools.agentry_calendar_session_1") {
		t.Errorf("the tools server is not given the sign-in:\n%s", cat)
	}
	err = filepath.Walk(sp.Dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // the plan's own files
		if err != nil {
			return err
		}
		if strings.Contains(string(b), sessionCookie) {
			return errors.New(path + " holds the sign-in")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Its sign-in missing, the segment is refused with the way back.
	_ = ForgetCalendarSession(keys, "assistant", "Work")
	if _, _, _, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, keys); err == nil || !strings.Contains(err.Error(), "--calendar-sign-in") {
		t.Errorf("a missing sign-in: %v", err)
	}
	// Saved, the configuration is of the version that knows sign-ins.
	if err := SaveConfig(home, a.Name, c); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadConfig(home, a.Name); err != nil || got.Version != calendarSignInsVersion || !got.CalendarFeeds[0].SignedIn {
		t.Errorf("saved: %v %+v", err, got)
	}
}

func TestResetAllRemovesTheSignInAndResetAgentKeepsIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reset func(context.Context, string, string, dockerx.Client) error
		kept  bool
	}{
		{"agent", ResetAgent, true},
		{"all", Reset, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := shortHome(t)
			a := assistantAgent(t)
			keys := fakeSecrets{CalendarKeychainAccount("assistant", "Work"): feedAddress}
			_ = KeepCalendarSession(keys, "assistant", "Work", testSession(t))
			old := resetSecrets
			resetSecrets = keys
			t.Cleanup(func() { resetSecrets = old })
			c := Config{Enabled: true}
			f := workFeed()
			f.SignedIn = true
			if _, err := ApplyChange(home, a, &c, Change{SetCalendarFeed: &f, Consent: true}); err != nil {
				t.Fatal(err)
			}
			if err := SaveConfig(home, a.Name, c); err != nil {
				t.Fatal(err)
			}
			if err := tc.reset(context.Background(), home, a.Name, &dockerx.Fake{}); err != nil {
				t.Fatal(err)
			}
			if _, kept := keys[CalendarSessionAccount("assistant", "Work", 1)]; kept != tc.kept {
				t.Errorf("the sign-in after the reset: %v", keys)
			}
		})
	}
}
