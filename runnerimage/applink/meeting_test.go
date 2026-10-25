package applink

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestTheMeetingToolsAreTheAssistantsAlone: Agentry Helper's set has none
// of them; each takes its own fields and nothing else, the times and the
// title required to arm.
func TestTheMeetingToolsAreTheAssistantsAlone(t *testing.T) {
	for _, name := range Tools(SetMain) {
		if IsMeetingTool(name) {
			t.Errorf("Agentry Helper's set has %s", name)
		}
	}
	takes := map[string][]string{
		MeetingArm:    {"app", "attendees", "end", "start", "title"},
		MeetingList:   nil,
		MeetingCancel: {"window"},
	}
	required := map[string][]string{MeetingArm: {"start", "end", "title"}, MeetingCancel: {"window"}}
	for _, tool := range meetingTools {
		if !IsMeetingTool(tool.Name) || !slices.Contains(Tools(SetAssistant), tool.Name) {
			t.Errorf("%s is not one of the Assistant's meeting tools", tool.Name)
		}
		props := tool.Schema["properties"].(map[string]any)
		if got := slices.Sorted(maps.Keys(props)); !slices.Equal(got, takes[tool.Name]) || tool.Schema["additionalProperties"] != false {
			t.Errorf("%s takes %v", tool.Name, got)
		}
		req, _ := tool.Schema["required"].([]string)
		if !slices.Equal(req, required[tool.Name]) {
			t.Errorf("%s requires %v", tool.Name, req)
		}
	}
	arm := meetingTools[0]
	for _, want := range []string{"5 minutes before", "15 minutes after", "more than an hour from now is refused", "Nothing is recorded if no call is detected", "can cancel it"} {
		if !strings.Contains(arm.Description, want) {
			t.Errorf("meeting_arm's description lacks %q", want)
		}
	}
}

// TestParseMeetingArmHoldsItsRules: a window opening within the hour is
// armed, from 5 minutes before the start to 15 after the end; one opening
// later, one over, one ending before it starts, one too long, one with an
// unknown argument, no title, a control character or an overlong text is
// refused.
func TestParseMeetingArmHoldsItsRules(t *testing.T) {
	now := time.Date(2026, 10, 26, 13, 30, 0, 0, time.UTC)
	a, err := ParseMeetingArm(json.RawMessage(`{"start":"2026-10-26T10:00:00-04:00","end":"2026-10-26T11:00:00-04:00","title":"  Budget\treview ","attendees":["Sam Li","",  "sam@example.com"],"app":"zoom"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Opens().Equal(time.Date(2026, 10, 26, 13, 55, 0, 0, time.UTC)) || !a.Closes().Equal(time.Date(2026, 10, 26, 15, 15, 0, 0, time.UTC)) {
		t.Errorf("the window %s to %s", a.Opens(), a.Closes())
	}
	if a.Title != "Budget review" || !slices.Equal(a.Attendees, []string{"Sam Li", "sam@example.com"}) || a.App != "zoom" {
		t.Errorf("%+v", a)
	}
	// The window opening exactly an hour from now is armed; a minute later is not.
	if _, err := ParseMeetingArm(json.RawMessage(`{"start":"2026-10-26T14:35:00Z","end":"2026-10-26T15:00:00Z","title":"x"}`), now); err != nil {
		t.Errorf("a window opening in an hour: %v", err)
	}
	// A meeting under way, its window still open, is armed.
	if _, err := ParseMeetingArm(json.RawMessage(`{"start":"2026-10-26T13:00:00Z","end":"2026-10-26T13:20:00Z","title":"x"}`), now); err != nil {
		t.Errorf("a window still open: %v", err)
	}
	long := strings.Repeat("a", MaxMeetingTitle+1)
	for name, raw := range map[string]string{
		"opens in more than an hour": `{"start":"2026-10-26T14:36:00Z","end":"2026-10-26T15:00:00Z","title":"x"}`,
		"is over":                    `{"start":"2026-10-26T12:00:00Z","end":"2026-10-26T13:14:00Z","title":"x"}`,
		"ends before it starts":      `{"start":"2026-10-26T14:00:00Z","end":"2026-10-26T13:59:00Z","title":"x"}`,
		"lasts over 12 hours":        `{"start":"2026-10-26T14:00:00Z","end":"2026-10-27T02:01:00Z","title":"x"}`,
		"has no offset":              `{"start":"2026-10-26T14:00:00","end":"2026-10-26T15:00:00Z","title":"x"}`,
		"takes an unknown argument":  `{"start":"2026-10-26T14:00:00Z","end":"2026-10-26T15:00:00Z","title":"x","silent":true}`,
		"has no title":               `{"start":"2026-10-26T14:00:00Z","end":"2026-10-26T15:00:00Z","title":"  "}`,
		"has a control character":    `{"start":"2026-10-26T14:00:00Z","end":"2026-10-26T15:00:00Z","title":"x\u001b[2J"}`,
		"has an overlong title":      `{"start":"2026-10-26T14:00:00Z","end":"2026-10-26T15:00:00Z","title":"` + long + `"}`,
		"is empty":                   ``,
	} {
		if _, err := ParseMeetingArm(json.RawMessage(raw), now); err == nil {
			t.Errorf("a meeting that %s was armed", name)
		}
	}
	many := `{"start":"2026-10-26T14:00:00Z","end":"2026-10-26T15:00:00Z","title":"x","attendees":[` + strings.TrimSuffix(strings.Repeat(`"a",`, MaxMeetingAttendees+1), ",") + `]}`
	if _, err := ParseMeetingArm(json.RawMessage(many), now); err == nil {
		t.Error("too many attendees were armed")
	}
}

// TestParseMeetingCancelTakesAWindowAlone: the window's id, and nothing else.
func TestParseMeetingCancelTakesAWindowAlone(t *testing.T) {
	if id, err := ParseMeetingCancel(json.RawMessage(`{"window":" w1 "}`)); err != nil || id != "w1" {
		t.Errorf("%q, %v", id, err)
	}
	for _, raw := range []string{``, `{}`, `{"window":""}`, `{"window":"w1","all":true}`} {
		if _, err := ParseMeetingCancel(json.RawMessage(raw)); err == nil {
			t.Errorf("%s was taken", raw)
		}
	}
}
