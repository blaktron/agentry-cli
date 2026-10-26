package persistent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// The address of a test feed: a secret, which no file may hold.
const feedAddress = "https://calendar.google.com/calendar/ical/person%40example.com/private-0f1e2d3c4b5a69788796a5b4c3d2e1f0/basic.ics"

func workFeed() CalendarFeed {
	return CalendarFeed{Name: "Work", Host: "calendar.google.com", Set: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)}
}

func TestACalendarFeedIsAnEntryWhoseAddressStaysInTheKeychain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	a := assistantAgent(t)
	c := Config{Enabled: true}
	f := workFeed()
	var ce *ConsentError
	if _, err := ApplyChange(home, a, &c, Change{SetCalendarFeed: &f}); !errors.As(err, &ce) || len(ce.Entries) != 1 ||
		!strings.HasPrefix(ce.Entries[0].Line, "Reads your calendar feed Work from Google Calendar (calendar.google.com), read only") ||
		!strings.Contains(ce.Entries[0].Line, "the agent is never shown it") {
		t.Fatalf("a feed unagreed: %v", err)
	}
	if len(c.CalendarFeeds) != 0 {
		t.Fatal("a refused change was kept")
	}
	if _, err := ApplyChange(home, a, &c, Change{SetCalendarFeed: &f, Consent: true}); err != nil {
		t.Fatal(err)
	}
	e, ok := BuildRoster(a, c, nil).Entry(CalendarEntryID("Work"))
	if !ok || e.Kind != KindCalendar || !e.On || e.GrantedBy != GrantedByYou || e.Name != "Work (calendar feed)" {
		t.Fatalf("the entry %+v", e)
	}
	if got := CalendarFeeds(a, c); len(got) != 1 || got[0] != (CalendarFeedState{Name: "Work", Host: "calendar.google.com", Service: "Google Calendar", On: true, Set: f.Set, Entry: "calendar:Work"}) {
		t.Errorf("CalendarFeeds = %+v", got)
	}
	if err := SaveConfig(home, a.Name, c); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(Dir(home, a.Name), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "calendar.google.com") || strings.Contains(string(b), "private-") || readVersion(t, home, a.Name) != 10 {
		t.Fatalf("config.yaml:\n%s", b)
	}
	// Set again under another case, it keeps its name.
	again := f
	again.Name, again.Set = "work", f.Set.Add(time.Hour)
	if _, err := ApplyChange(home, a, &c, Change{SetCalendarFeed: &again}); err != nil || len(c.CalendarFeeds) != 1 || c.CalendarFeeds[0].Name != "Work" || !c.CalendarFeeds[0].Set.Equal(again.Set) {
		t.Fatalf("set again: %v %+v", err, c.CalendarFeeds)
	}
	if _, err := ApplyChange(home, a, &c, Change{RemoveCalendarFeed: "WORK"}); err != nil || len(c.CalendarFeeds) != 0 || c.Consented[CalendarEntryID("Work")] != "" {
		t.Fatalf("removed: %v %+v", err, c)
	}
}

func TestACalendarFeedIsRefusedWhereItCannotBeGiven(t *testing.T) {
	home := shortHome(t)
	for name, tc := range map[string]struct {
		agent Agent
		feed  CalendarFeed
		want  string
	}{
		"the Coding agent":       {coderAgent(t), workFeed(), "the Assistant alone reads them"},
		"a name with a colon":    {assistantAgent(t), CalendarFeed{Name: "a:b", Host: "calendar.google.com"}, "may not hold"},
		"an empty name":          {assistantAgent(t), CalendarFeed{Name: "", Host: "calendar.google.com"}, "1 to 64 characters"},
		"a host with a port":     {assistantAgent(t), CalendarFeed{Name: "W", Host: "calendar.google.com:8443"}, "not a host name"},
		"a host in capitals":     {assistantAgent(t), CalendarFeed{Name: "W", Host: "Calendar.Google.com"}, "not a host name"},
		"a host that is a path":  {assistantAgent(t), CalendarFeed{Name: "W", Host: "evil.example/x"}, "not a host name"},
		"a host that is an addr": {assistantAgent(t), CalendarFeed{Name: "W", Host: "10.0.0.1"}, "not a host name"},
	} {
		t.Run(name, func(t *testing.T) {
			c := Config{Enabled: true}
			f := tc.feed
			if _, err := ApplyChange(home, tc.agent, &c, Change{SetCalendarFeed: &f, Consent: true}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRosterGivesTheFeedToTheToolsServerAlone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	a := assistantAgent(t)
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "on", false: "off"}[on], func(t *testing.T) {
			f := workFeed()
			keys := fakeSecrets{CalendarKeychainAccount("assistant", "Work"): "webcal://calendar.google.com/calendar/ical/person%40example.com/private-0f1e2d3c4b5a69788796a5b4c3d2e1f0/basic.ics"}
			c := Config{Enabled: true}
			for _, ch := range []Change{{SetCalendarFeed: &f}, {Switch: &Switch{ID: CalendarEntryID("Work"), On: on}}} {
				ch.Consent = true
				if _, err := ApplyChange(home, a, &c, ch); err != nil {
					t.Fatal(err)
				}
			}
			gr, applied, done, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, keys)
			if err != nil {
				t.Fatal(err)
			}
			defer done()
			given := len(gr.CalendarFeeds) == 1 && gr.CalendarFeeds[0] == (localrun.CalendarFeed{Name: "Work", Host: "calendar.google.com", URL: feedAddress})
			if given != on {
				t.Fatalf("feeds %+v", gr.CalendarFeeds)
			}
			if slices.Contains(applied.On, CalendarEntryID("Work")) != on {
				t.Errorf("applied %v", applied.On)
			}
			sp, err := localrun.Segment{Builtin: a.Builtin, Home: home, Policy: policy.Default(), PolicySHA: "s", Build: "test", Grants: gr}.Plan(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			cat, err := os.ReadFile(filepath.Join(sp.Dir, "gateway", "catalog.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(cat), "calendar.google.com:443") != on || strings.Contains(string(cat), "agentry-tools.agentry_calendar_feed_1") != on {
				t.Errorf("the tools server's feed, on %v:\n%s", on, cat)
			}
			if g := sp.Plan.Capabilities.Calendar; (g != nil && len(g.Feeds) == 1 && g.Feeds[0].URLEnv == "AGENTRY_CALENDAR_FEED_1") != on {
				t.Errorf("the plan's calendar grant: %+v", g)
			}
			err = filepath.Walk(sp.Dir, func(path string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return err
				}
				b, err := os.ReadFile(path) //nolint:gosec // the plan's own files
				if err != nil {
					return err
				}
				if strings.Contains(string(b), "private-0f1e2d3c") {
					return errors.New(path + " holds the address")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAFeedWhoseAddressIsMissingOrMovedRefusesTheSegment(t *testing.T) {
	home := shortHome(t)
	a := assistantAgent(t)
	c := Config{Enabled: true}
	f := workFeed()
	if _, err := ApplyChange(home, a, &c, Change{SetCalendarFeed: &f, Consent: true}); err != nil {
		t.Fatal(err)
	}
	r := BuildRoster(a, c, nil)
	for name, keys := range map[string]Secrets{
		"none kept":          fakeSecrets{},
		"on another host":    fakeSecrets{CalendarKeychainAccount("assistant", "Work"): "https://collector.example/private-0123456789/basic.ics"},
		"plain http":         fakeSecrets{CalendarKeychainAccount("assistant", "Work"): "http://calendar.google.com/calendar/ical/x/basic.ics"},
		"no keychain at all": nil,
	} {
		_, _, _, err := Apply(context.Background(), home, a, c, r, nil, nil, keys)
		if err == nil {
			t.Errorf("%s: a feed was given", name)
			continue
		}
		if keys != nil && !strings.Contains(err.Error(), `agentry agent configure assistant --calendar-feed "Work"`) {
			t.Errorf("%s: %v", name, err)
		}
		if strings.Contains(err.Error(), "private-0123") || strings.Contains(err.Error(), "collector.example") {
			t.Errorf("%s: the error repeats the address: %v", name, err)
		}
	}
}

func TestSettingAFeedAgainRestartsTheSegment(t *testing.T) {
	home := shortHome(t)
	a := assistantAgent(t)
	c := Config{Enabled: true}
	f := workFeed()
	if restart, err := ApplyChange(home, a, &c, Change{SetCalendarFeed: &f, Consent: true}); err != nil || !restart {
		t.Fatalf("adding: restart %v, %v", restart, err)
	}
	again := f
	again.Set = f.Set.Add(time.Minute)
	if restart, err := ApplyChange(home, a, &c, Change{SetCalendarFeed: &again}); err != nil || !restart {
		t.Fatalf("its address set again: restart %v, %v", restart, err)
	}
	if restart, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: CalendarEntryID("Work"), On: false}}); err != nil || !restart {
		t.Fatalf("switched off: restart %v, %v", restart, err)
	}
}

func TestTheResetsKeepOrRemoveTheFeeds(t *testing.T) {
	for _, tc := range []struct {
		name               string
		reset              func(context.Context, string, string, dockerx.Client) error
		listed, on, inKeys bool
	}{
		{"history", ResetHistory, true, true, true},
		{"agent", ResetAgent, true, false, true},
		{"all", Reset, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := shortHome(t)
			a := assistantAgent(t)
			keys := fakeSecrets{CalendarKeychainAccount("assistant", "Work"): feedAddress, "provider:openai": "kept-0123456789"}
			old := resetSecrets
			resetSecrets = keys
			t.Cleanup(func() { resetSecrets = old })
			c := Config{Enabled: true}
			f := workFeed()
			if _, err := ApplyChange(home, a, &c, Change{SetCalendarFeed: &f, Consent: true}); err != nil {
				t.Fatal(err)
			}
			if err := SaveConfig(home, a.Name, c); err != nil {
				t.Fatal(err)
			}
			if err := tc.reset(context.Background(), home, a.Name, &dockerx.Fake{}); err != nil {
				t.Fatal(err)
			}
			after, err := LoadConfig(home, a.Name)
			if err != nil {
				t.Fatal(err)
			}
			e, _ := BuildRoster(a, after, nil).Entry(CalendarEntryID("Work"))
			if (len(after.CalendarFeeds) == 1) != tc.listed || e.On != tc.on {
				t.Errorf("after the reset: feeds %+v, on %v", after.CalendarFeeds, e.On)
			}
			if _, kept := keys[CalendarKeychainAccount("assistant", "Work")]; kept != tc.inKeys {
				t.Errorf("the keychain after the reset: %v", keys)
			}
			if _, kept := keys["provider:openai"]; !kept {
				t.Error("a reset removed a key that is not the agent's")
			}
		})
	}
}

func TestOnlyTheAssistantHasCalendarEntries(t *testing.T) {
	c := Config{CalendarFeeds: []CalendarFeed{workFeed()}}
	if _, ok := BuildRoster(coderAgent(t), c, nil).Entry(CalendarEntryID("Work")); ok {
		t.Error("the Coding agent's roster has a calendar feed")
	}
	if CalendarFeeds(coderAgent(t), c) != nil {
		t.Error("the Coding agent lists calendar feeds")
	}
}
