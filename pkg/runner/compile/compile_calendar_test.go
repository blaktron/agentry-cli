package compile

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// assistantInputs compiles the pure-text release as the Assistant, with its
// folders and the calendar grant g.
func assistantInputs(t *testing.T, g *spec.CalendarGrant) Inputs {
	t.Helper()
	in := persistentInputs(t, &spec.AgentFiles{Share: true})
	y := strings.Replace(strings.Replace(pureTextYAML, "modes: [autonomous]", "modes: [persistent]", 1), "name: pure-text\nnamespace: demo", "name: assistant\nnamespace: agentry", 1)
	in.Manifest = parseManifest(t, y)
	in.Calendar = g
	return in
}

func twoFeeds() *spec.CalendarGrant {
	return &spec.CalendarGrant{TimeZone: "Europe/London", Feeds: []spec.CalendarFeed{
		{Name: "Family", Host: "p01-caldav.icloud.com", URLEnv: spec.CalendarFeedEnv(0)},
		{Name: "Work", Host: "calendar.google.com", URLEnv: spec.CalendarFeedEnv(1)},
	}}
}

// TestTheCalendarFeedsCompileTheirContainer: the tools container reaches the
// feeds' hosts on 443 alone, holds each address as a secret of its own, and
// grants the calendar tools; the plan, the receipt's lines and the System
// Prompt name the feeds and hosts, never an address.
func TestTheCalendarFeedsCompileTheirContainer(t *testing.T) {
	planDir := t.TempDir()
	plan, err := Compile(assistantInputs(t, twoFeeds()), planDir)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := os.ReadFile(filepath.Join(planDir, "gateway", "catalog.yaml")) //nolint:gosec // test plan dir
	if err != nil {
		t.Fatal(err)
	}
	c := string(cat)
	if !strings.Contains(c, "p01-caldav.icloud.com:443") || !strings.Contains(c, "calendar.google.com:443") || strings.Count(c, ":443") != 2 || strings.Contains(c, "disableNetwork") {
		t.Errorf("the tools container's network:\n%s", c)
	}
	for _, env := range []string{spec.CalendarFeedEnv(0), spec.CalendarFeedEnv(1)} {
		if !strings.Contains(c, "env: "+env) || !strings.Contains(c, SecretKey(env)) {
			t.Errorf("the tools container is not given %s:\n%s", env, c)
		}
	}
	b, err := os.ReadFile(filepath.Join(planDir, "gateway", "toolspec.json")) //nolint:gosec // test plan dir
	if err != nil {
		t.Fatal(err)
	}
	var ts toolsrv.Spec
	if err := json.Unmarshal(b, &ts); err != nil {
		t.Fatal(err)
	}
	if ts.Calendar.TimeZone != "Europe/London" || len(ts.Calendar.Feeds) != 2 || ts.Calendar.Feeds[1] != (toolsrv.CalendarFeed{Name: "Work", Host: "calendar.google.com", URLEnv: "AGENTRY_CALENDAR_FEED_2"}) {
		t.Errorf("tools spec calendar: %+v", ts.Calendar)
	}
	for _, tool := range []string{toolsrv.ToolCalendarFeeds, toolsrv.ToolCalendarEvents, toolsrv.ToolCalendarFreeTime} {
		if !slices.Contains(plan.Tools, tool) {
			t.Errorf("%s is not granted: %v", tool, plan.Tools)
		}
	}
	if slices.Contains(plan.Tools, toolsrv.ToolFetch) {
		t.Errorf("fetch is granted beside the feeds: %v", plan.Tools)
	}
	if line := plan.Enforcement["persistent.calendar"]; !strings.Contains(line, "Family (p01-caldav.icloud.com), Work (calendar.google.com)") || !strings.Contains(line, "never given its address") {
		t.Errorf("persistent.calendar: %q", line)
	}
	if line := plan.Enforcement["network"]; !strings.Contains(line, "p01-caldav.icloud.com:443, calendar.google.com:443 alone") {
		t.Errorf("network: %q", line)
	}
	if len(plan.Secrets) != 0 {
		t.Errorf("the feeds' addresses are not declared secrets: %v", plan.Secrets)
	}
	instr := renderedInstruction(t, planDir, plan.Harness.Adapter)
	if !strings.Contains(instr, "**Calendar feeds.** You read the person's calendars from the feeds Family, Work, read only, in their time zone, Europe/London") ||
		!strings.Contains(instr, "the calendar tools fetch the person's calendar feeds") {
		t.Errorf("the System Prompt does not describe the feeds:\n%s", instr)
	}
}

// TestCalendarFeedsAreTheAssistantsAlone: the grant is refused to anything
// but the persistent Assistant with its folders, beside other egress, and
// for a malformed feed.
func TestCalendarFeedsAreTheAssistantsAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Inputs)
		want string
	}{
		{"another built-in", func(in *Inputs) {
			in.Manifest = parseManifest(t, strings.Replace(pureTextYAML, "modes: [autonomous]", "modes: [persistent]", 1))
		}, "agentry/assistant's alone"},
		{"no folders", func(in *Inputs) { in.AgentFiles = nil }, "come with the agent's folders"},
		{"not from a built-in", func(in *Inputs) { in.Source = "registry" }, "persistent"},
		{"a feed with an address in its host", func(in *Inputs) {
			in.Calendar.Feeds[0].Host = "user:pass@evil.example"
		}, "not a host name"},
		{"a feed named twice", func(in *Inputs) { in.Calendar.Feeds[1].Name = "family" }, "named twice"},
		{"a zone that is not one", func(in *Inputs) { in.Calendar.TimeZone = "Local" }, "not an IANA zone"},
		{"beside the review grant", func(in *Inputs) { in.Review = true }, "never given beside"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := assistantInputs(t, twoFeeds())
			tc.edit(&in)
			_, err := Compile(in, t.TempDir())
			var refusal *PolicyRefusal
			if !errors.As(err, &refusal) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want a refusal with %q", err, tc.want)
			}
		})
	}
}

// TestNoFeedsNoNetwork: the Assistant with no feed keeps a tools container
// with no network.
func TestNoFeedsNoNetwork(t *testing.T) {
	planDir := t.TempDir()
	plan, err := Compile(assistantInputs(t, nil), planDir)
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := os.ReadFile(filepath.Join(planDir, "gateway", "catalog.yaml")) //nolint:gosec // test plan dir
	if !strings.Contains(string(cat), "disableNetwork: true") || slices.Contains(plan.Tools, toolsrv.ToolCalendarEvents) || plan.Enforcement["persistent.calendar"] != "" {
		t.Errorf("with no feed:\n%s\n%v", cat, plan.Tools)
	}
}

// TestASignedInFeedCompilesItsSignInForTheToolsContainer: a feed signed in
// through a browser gives the tools container its sign-in as a secret of
// its own beside the address, the tools spec names the variable alone, and
// the plan's line says where the sign-in goes.
func TestASignedInFeedCompilesItsSignInForTheToolsContainer(t *testing.T) {
	g := twoFeeds()
	g.Feeds[1].SessionEnv = spec.CalendarSessionEnv(1)
	planDir := t.TempDir()
	plan, err := Compile(assistantInputs(t, g), planDir)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := os.ReadFile(filepath.Join(planDir, "gateway", "catalog.yaml")) //nolint:gosec // test plan dir
	if err != nil {
		t.Fatal(err)
	}
	if env := spec.CalendarSessionEnv(1); !strings.Contains(string(cat), "env: "+env) || !strings.Contains(string(cat), SecretKey(env)) {
		t.Errorf("the tools container is not given the sign-in:\n%s", cat)
	}
	if strings.Contains(string(cat), spec.CalendarSessionEnv(0)) {
		t.Errorf("a feed with no sign-in was given one:\n%s", cat)
	}
	b, err := os.ReadFile(filepath.Join(planDir, "gateway", "toolspec.json")) //nolint:gosec // test plan dir
	if err != nil {
		t.Fatal(err)
	}
	var ts toolsrv.Spec
	if err := json.Unmarshal(b, &ts); err != nil {
		t.Fatal(err)
	}
	if ts.Calendar.Feeds[1].SessionEnv != "AGENTRY_CALENDAR_SESSION_2" || ts.Calendar.Feeds[0].SessionEnv != "" {
		t.Errorf("tools spec calendar: %+v", ts.Calendar)
	}
	if line := plan.Enforcement["persistent.calendar"]; !strings.Contains(line, "sign-in's cookies") || !strings.Contains(line, "that feed's host alone") {
		t.Errorf("persistent.calendar: %q", line)
	}
	if line := plan.Enforcement["network"]; !strings.Contains(line, "p01-caldav.icloud.com:443, calendar.google.com:443 alone") {
		t.Errorf("network: %q", line)
	}
	g.Feeds[1].SessionEnv = "AGENTRY_CALENDAR_FEED_9"
	if _, err := Compile(assistantInputs(t, g), t.TempDir()); err == nil {
		t.Error("a sign-in under a feed's variable compiled")
	}
}
