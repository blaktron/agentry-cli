package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

// TestAgentConfigureCalendarFeed: --calendar-feed reads the address from
// stdin, reads the feed once and, once it is a calendar, keeps the address
// in the keychain (webcal as https) and the feed on the roster, never
// printing the address; one that is not https, or does not read, is not
// kept; --no-calendar-feed removes it; the flags together, with another
// change, or for the Coding agent are refused as usage.
func TestAgentConfigureCalendarFeed(t *testing.T) {
	home := agentHome(t)
	keys := memSecrets{}
	old := agentSecrets
	agentSecrets = keys
	t.Cleanup(func() { agentSecrets = old })
	readable := persistent.CalendarFeedReadable
	t.Cleanup(func() { persistent.CalendarFeedReadable = readable })
	var fetched []string
	unreadable := false
	persistent.CalendarFeedReadable = func(_ context.Context, address string) (string, int, error) {
		fetched = append(fetched, address)
		if unreadable {
			return "", 0, errors.Join(persistent.ErrCalendarFeedUnreadable, errors.New("calendar.google.com answered HTTP 404"))
		}
		return "person@example.com", 42, nil
	}
	const given = "webcal://calendar.google.com/calendar/ical/person%40example.com/private-0f1e2d3c4b5a6978/basic.ics"
	const kept = "https://calendar.google.com/calendar/ical/person%40example.com/private-0f1e2d3c4b5a6978/basic.ics"
	account := persistent.CalendarKeychainAccount("assistant", "Work")

	if _, _, err := runCLI(t, strings.NewReader("http://calendar.google.com/x.ics\n"), "agent", "configure", "assistant", "--calendar-feed", "Work", "--yes"); err == nil ||
		exitCode(err) != engine.ExitUsage || !strings.Contains(err.Error(), "only https is read") || strings.Contains(err.Error(), "/x.ics") {
		t.Fatalf("an http address: %v", err)
	}
	unreadable = true
	if _, _, err := runCLI(t, strings.NewReader(given+"\n"), "agent", "configure", "assistant", "--calendar-feed", "Work", "--yes"); err == nil ||
		exitCode(err) != engine.ExitUsage || !strings.Contains(err.Error(), "404") {
		t.Fatalf("an unreadable feed: %v", err)
	}
	if _, has := keys[account]; has {
		t.Fatal("an unreadable feed was kept")
	}
	unreadable = false
	// Unagreed, nothing is kept; the agreement says what the agent reads.
	_, asked, err := runCLI(t, strings.NewReader(given+"\n"), "agent", "configure", "assistant", "--calendar-feed", "Work")
	if err == nil || !strings.Contains(err.Error(), "--yes") || !strings.Contains(asked, "Reads your calendar feed Work from Google Calendar") {
		t.Fatalf("an unagreed feed: %v\n%s", err, asked)
	}
	if _, has := keys[account]; has {
		t.Fatal("an unagreed feed was kept")
	}
	out, errOut, err := runCLI(t, strings.NewReader(given+"\n"), "agent", "configure", "assistant", "--calendar-feed", "Work", "--yes")
	if err != nil || !strings.Contains(out, `Read the calendar "person@example.com" from Google Calendar (42 events)`) || strings.Contains(out+errOut, "private-0f1e") {
		t.Fatalf("--calendar-feed: %v\n%s\n%s", err, out, errOut)
	}
	if keys[account] != kept || fetched[len(fetched)-1] != kept {
		t.Fatalf("the keychain holds %q; fetched %v", keys[account], fetched)
	}
	cfg, err := persistent.LoadConfig(home, "assistant")
	if err != nil || len(cfg.CalendarFeeds) != 1 || cfg.CalendarFeeds[0].Host != "calendar.google.com" {
		t.Fatalf("config: %v %+v", err, cfg)
	}
	out, _, err = runCLI(t, nil, "agent", "show", "assistant")
	if err != nil || !strings.Contains(out, "Work\tcalendar:Work (Google Calendar, calendar.google.com): on") || strings.Contains(out, "private-") {
		t.Fatalf("show: %v\n%s", err, out)
	}
	out, _, err = runCLI(t, nil, "agent", "show", "assistant", "--json")
	if err != nil || !strings.Contains(out, `"calendarFeeds"`) || strings.Contains(out, "private-") {
		t.Fatalf("show --json: %v\n%s", err, out)
	}
	// Given again under another case, the feed keeps its name.
	if _, _, err := runCLI(t, strings.NewReader(given+"\n"), "agent", "configure", "assistant", "--calendar-feed", "work", "--yes"); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := persistent.LoadConfig(home, "assistant"); len(cfg.CalendarFeeds) != 1 || cfg.CalendarFeeds[0].Name != "Work" {
		t.Fatalf("set again: %+v", cfg.CalendarFeeds)
	}
	if _, _, err := runCLI(t, nil, "agent", "configure", "assistant", "--no-calendar-feed", "WORK"); err != nil {
		t.Fatal(err)
	}
	if _, has := keys[account]; has {
		t.Fatal("--no-calendar-feed kept the address")
	}
	if cfg, _ := persistent.LoadConfig(home, "assistant"); len(cfg.CalendarFeeds) != 0 {
		t.Fatalf("--no-calendar-feed kept the feed: %+v", cfg.CalendarFeeds)
	}
	for _, args := range [][]string{
		{"assistant", "--calendar-feed", "Work", "--no-calendar-feed", "Work"},
		{"assistant", "--calendar-feed", "Work", "--name", "Pat"},
		{"assistant", "--no-calendar-feed", "Work"},
		{"assistant", "--calendar-feed", "a:b"},
		{"coder", "--calendar-feed", "Work"},
	} {
		if _, _, err := runCLI(t, strings.NewReader(given+"\n"), append([]string{"agent", "configure"}, args...)...); err == nil || exitCode(err) != engine.ExitUsage {
			t.Errorf("%v: %v", args, err)
		}
	}
}
