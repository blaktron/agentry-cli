package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

const feedURL = "https://calendar.google.com/calendar/ical/person%40example.com/private-31415926535897932384/basic.ics"

// withFeed is an Assistant's calendar grant with one feed, whose address is
// the segment's.
func withFeed(p *Params) {
	p.Plan.Capabilities.Calendar = &spec.CalendarGrant{Feeds: []spec.CalendarFeed{{Name: "Work", Host: "calendar.google.com", URLEnv: spec.CalendarFeedEnv(0)}}}
	p.CalendarURLs = map[string]string{spec.CalendarFeedEnv(0): feedURL}
}

// TestAFeedsAddressReachesTheToolsContainerAlone: the address is written
// under the tools server's key from the segment's addresses alone, never
// from the policy source; a feed with no address refuses the segment,
// naming the feed and not an address; its host is a relay host.
func TestAFeedsAddressReachesTheToolsContainerAlone(t *testing.T) {
	p, _ := fixture(t)
	withFeed(&p)
	t.Setenv(spec.CalendarFeedEnv(0), "https://from-the-environment.example/x.ics")
	values, err := secretValues(p)
	if err != nil {
		t.Fatal(err)
	}
	holding := 0
	for _, v := range values {
		if v == feedURL {
			holding++
		}
	}
	if values[compile.SecretKey(spec.CalendarFeedEnv(0))] != feedURL || holding != 1 {
		t.Fatalf("values = %v", values)
	}
	p.CalendarURLs = nil
	if _, err := secretValues(p); err == nil || !strings.Contains(err.Error(), "the calendar feed Work") || strings.Contains(err.Error(), "from-the-environment") {
		t.Fatalf("a feed with no address: %v", err)
	}
	if hosts := relayHosts(p.Plan); !slices.Contains(hosts, "calendar.google.com:443") {
		t.Errorf("relay hosts %v", hosts)
	}
}

// TestAFeedsAddressIsMaskedEverywhere: as a stored secret's value, the
// address is masked in the events, the transcript, every receipt file and
// the result.
func TestAFeedsAddressIsMaskedEverywhere(t *testing.T) {
	p, f, _ := persistentFixture(t, "")
	withFeed(&p)
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	get, done := collect(s)
	if err := s.Send(context.Background(), "the feed is at "+feedURL); err != nil {
		t.Fatal(err)
	}
	if _, err := s.End(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	<-done
	for _, ev := range get() {
		if strings.Contains(ev.Text, "private-3141") {
			t.Errorf("an event holds the address: %+v", ev)
		}
	}
	seen := 0
	err = filepath.Walk(p.OutDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // the segment's own output
		if err != nil {
			return err
		}
		seen++
		if strings.Contains(string(b), "private-3141") {
			return errors.New(path + " holds the address")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 3 {
		t.Fatalf("the segment wrote %d files", seen)
	}
}

// A browser sign-in's cookie, which no output may hold.
const sessionCookie = "signed-in-2718281828459045"

// TestASignInReachesTheToolsContainerAloneAndIsMasked: a signed-in feed's
// sign-in is written under its own key from the segment's values alone, a
// feed whose sign-in is missing refuses the segment by name, and the
// redactor masks the sign-in whole and each of its cookies.
func TestASignInReachesTheToolsContainerAloneAndIsMasked(t *testing.T) {
	p, _ := fixture(t)
	withFeed(&p)
	session := `[{"n":"SID","v":"` + sessionCookie + `","d":".google.com","p":"/"}]`
	p.Plan.Capabilities.Calendar.Feeds[0].SessionEnv = spec.CalendarSessionEnv(0)
	p.CalendarURLs[spec.CalendarSessionEnv(0)] = session
	values, err := secretValues(p)
	if err != nil {
		t.Fatal(err)
	}
	if values[compile.SecretKey(spec.CalendarSessionEnv(0))] != session {
		t.Fatalf("values = %v", values)
	}
	rd := &runDirs{}
	r := rd.newRedactor(p, "", values)
	for _, text := range []string{"cookie " + sessionCookie, "whole " + session} {
		if got := r.String(text); strings.Contains(got, sessionCookie) {
			t.Errorf("masked %q to %q", text, got)
		}
	}
	delete(p.CalendarURLs, spec.CalendarSessionEnv(0))
	if _, err := secretValues(p); err == nil || !strings.Contains(err.Error(), "the sign-in of the calendar feed Work") {
		t.Fatalf("a feed with no sign-in: %v", err)
	}
}
