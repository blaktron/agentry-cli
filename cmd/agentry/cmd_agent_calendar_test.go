package main

import (
	"context"
	"errors"
	"fmt"
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

// TestAgentConfigureCalendarFeedSignIn: an address that asks for a sign-in
// names --calendar-sign-in and keeps nothing; with it, the browser's
// sign-in is kept beside the address and the feed recorded signed in;
// given again with an empty address, the feed keeps its address and takes
// the new sign-in; given again without the flag, the sign-in goes; a
// closed window keeps nothing; --no-calendar-feed removes the sign-in too.
func TestAgentConfigureCalendarFeedSignIn(t *testing.T) {
	home := agentHome(t)
	keys := memSecrets{}
	old := agentSecrets
	agentSecrets = keys
	t.Cleanup(func() { agentSecrets = old })
	readable, signIn := persistent.CalendarFeedReadable, signInCalendarFeed
	t.Cleanup(func() { persistent.CalendarFeedReadable, signInCalendarFeed = readable, signIn })
	persistent.CalendarFeedReadable = func(context.Context, string) (string, int, error) {
		return "", 0, fmt.Errorf("%w at accounts.google.com", persistent.ErrCalendarFeedSignIn)
	}
	const session = `[{"n":"SID","v":"browser-session-0123456789","d":".google.com","p":"/"}]`
	var opened []string
	closed := false
	signInCalendarFeed = func(_ context.Context, address string, progress func(string)) (string, string, int, error) {
		opened = append(opened, address)
		progress("Sign in in the browser window that opened.")
		if closed {
			return "", "", 0, errors.New("the browser window was closed before the sign-in was done")
		}
		return session, "Team", 7, nil
	}
	const address = "https://calendar.google.com/calendar/ical/team%40example.com/public/basic.ics"
	account := persistent.CalendarKeychainAccount("assistant", "Work")
	part := persistent.CalendarSessionAccount("assistant", "Work", 1)

	_, _, err := runCLI(t, strings.NewReader(address+"\n"), "agent", "configure", "assistant", "--calendar-feed", "Work", "--yes")
	if err == nil || exitCode(err) != engine.ExitUsage || !strings.Contains(err.Error(), `--calendar-feed "Work" --calendar-sign-in`) || strings.Contains(err.Error(), "team%40") {
		t.Fatalf("a feed that asks for a sign-in: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("kept %v", keys)
	}
	out, errOut, err := runCLI(t, strings.NewReader(address+"\n"), "agent", "configure", "assistant", "--calendar-feed", "Work", "--calendar-sign-in", "--yes")
	if err != nil || !strings.Contains(out, `Signed in and read the calendar "Team"`) || !strings.Contains(errOut, "browser window") || strings.Contains(out+errOut, "browser-session") {
		t.Fatalf("--calendar-sign-in: %v\n%s\n%s", err, out, errOut)
	}
	if keys[account] != address || keys[part] != session || len(opened) != 1 || opened[0] != address {
		t.Fatalf("keys %v, opened %v", keys, opened)
	}
	cfg, _ := persistent.LoadConfig(home, "assistant")
	if len(cfg.CalendarFeeds) != 1 || !cfg.CalendarFeeds[0].SignedIn {
		t.Fatalf("config %+v", cfg.CalendarFeeds)
	}
	// Signed in again with an empty address: the feed's own is opened.
	if _, _, err := runCLI(t, strings.NewReader("\n"), "agent", "configure", "assistant", "--calendar-feed", "work", "--calendar-sign-in", "--yes"); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 2 || opened[1] != address || keys[account] != address {
		t.Fatalf("signed in again: opened %v, keys %v", opened, keys)
	}
	// A closed window keeps what was there.
	closed = true
	if _, _, err := runCLI(t, strings.NewReader("\n"), "agent", "configure", "assistant", "--calendar-feed", "Work", "--calendar-sign-in", "--yes"); err == nil || exitCode(err) != engine.ExitUsage {
		t.Fatalf("a closed window: %v", err)
	}
	if keys[part] != session {
		t.Fatalf("a closed window changed the sign-in: %v", keys)
	}
	// Set again without a sign-in, to an address that reads: the sign-in
	// goes.
	persistent.CalendarFeedReadable = func(context.Context, string) (string, int, error) { return "", 3, nil }
	if _, _, err := runCLI(t, strings.NewReader(address+"\n"), "agent", "configure", "assistant", "--calendar-feed", "Work", "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, has := keys[part]; has {
		t.Fatal("a feed set without a sign-in kept the old one")
	}
	if cfg, _ := persistent.LoadConfig(home, "assistant"); cfg.CalendarFeeds[0].SignedIn {
		t.Fatal("the feed is still recorded signed in")
	}
	closed = false
	if _, _, err := runCLI(t, strings.NewReader(address+"\n"), "agent", "configure", "assistant", "--calendar-feed", "Work", "--calendar-sign-in", "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, nil, "agent", "configure", "assistant", "--no-calendar-feed", "Work"); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("--no-calendar-feed left %v", keys)
	}
	for _, args := range [][]string{
		{"--calendar-sign-in"},
		{"--no-calendar-feed", "Work", "--calendar-sign-in"},
	} {
		if _, _, err := runCLI(t, nil, append([]string{"agent", "configure", "assistant"}, args...)...); err == nil || exitCode(err) != engine.ExitUsage {
			t.Errorf("%v: %v", args, err)
		}
	}
	// A new feed needs its address, sign-in or not.
	if _, _, err := runCLI(t, strings.NewReader("\n"), "agent", "configure", "assistant", "--calendar-feed", "Home", "--calendar-sign-in", "--yes"); err == nil || exitCode(err) != engine.ExitUsage {
		t.Errorf("a new feed with no address: %v", err)
	}
}
