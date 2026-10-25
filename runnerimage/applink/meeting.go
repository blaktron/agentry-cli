package applink

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// The Assistant's meeting tools: it arms Agentry Desktop to record a call
// during a meeting the person asked it to record, lists what is armed and
// cancels a window. The application answers them; an armed window records
// only a call the application detects inside it, so a meeting nobody joins
// records nothing.
const (
	MeetingArm    = "meeting_arm"
	MeetingList   = "meeting_list"
	MeetingCancel = "meeting_cancel"
)

// An armed window opens ArmLead before the meeting starts and closes
// ArmTail after it ends. A window that would open more than ArmAhead from
// now is refused: arming is done close to the meeting, so a calendar read
// hours earlier never holds a recording open for a meeting that moved.
const (
	ArmLead  = 5 * time.Minute
	ArmTail  = 15 * time.Minute
	ArmAhead = time.Hour
)

// The bounds of meeting_arm's arguments.
const (
	MaxMeetingLength    = 12 * time.Hour
	MaxMeetingTitle     = 200
	MaxMeetingAttendee  = 200
	MaxMeetingAttendees = 100
	MaxMeetingAppHint   = 64
	MaxMeetingWindowID  = 64
)

var meetingTools = []Tool{
	{MeetingArm, "Arm Agentry Desktop to record a meeting the person asked you to record: a window from 5 minutes before its start to 15 minutes after its end, in which a call Agentry detects is recorded and transcribed on this computer. Nothing is recorded if no call is detected in the window. A window opening more than an hour from now is refused, so arm a meeting within the hour before it starts. The person sees every armed window in Agentry and can cancel it. Returns the window's id.",
		obj(map[string]any{
			"start":     str("when the meeting starts, as an RFC 3339 time with its offset, such as 2026-10-26T14:00:00-04:00"),
			"end":       str("when the meeting ends, the same way"),
			"title":     str("the meeting's title, as the calendar has it"),
			"attendees": strs("the attendees' names or addresses, as the calendar lists them"),
			"app":       str("the meeting app the calendar names, if any: zoom, teams, meet, webex or another"),
		}, "start", "end", "title")},
	{MeetingList, "List the armed meeting windows: each one's id, title, when it opens and closes, and whether it has recorded.", obj(map[string]any{})},
	{MeetingCancel, "Cancel an armed meeting window, when the meeting was cancelled or the person no longer wants it recorded.",
		obj(map[string]any{"window": str("the window's id, from meeting_arm or meeting_list")}, "window")},
}

// IsMeetingTool says tool is one of the meeting tools.
func IsMeetingTool(tool string) bool {
	return tool == MeetingArm || tool == MeetingList || tool == MeetingCancel
}

// MeetingArmArgs are meeting_arm's arguments.
type MeetingArmArgs struct {
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Title     string    `json:"title"`
	Attendees []string  `json:"attendees,omitempty"`
	App       string    `json:"app,omitempty"`
}

// Opens is when the armed window opens.
func (a MeetingArmArgs) Opens() time.Time { return a.Start.Add(-ArmLead) }

// Closes is when the armed window closes.
func (a MeetingArmArgs) Closes() time.Time { return a.End.Add(ArmTail) }

// ParseMeetingArm reads meeting_arm's arguments and holds them to its
// rules at now: two times with their offsets, the end after the start and
// still to come, the window opening within ArmAhead, a title, and each text
// within its bound. An argument it does not know refuses the call.
func ParseMeetingArm(raw json.RawMessage, now time.Time) (MeetingArmArgs, error) {
	var in struct {
		Start     string   `json:"start"`
		End       string   `json:"end"`
		Title     string   `json:"title"`
		Attendees []string `json:"attendees"`
		App       string   `json:"app"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if len(bytes.TrimSpace(raw)) == 0 || dec.Decode(&in) != nil {
		return MeetingArmArgs{}, errors.New("meeting_arm takes start, end, title, attendees and app, and nothing else")
	}
	var a MeetingArmArgs
	var err error
	if a.Start, err = time.Parse(time.RFC3339, strings.TrimSpace(in.Start)); err != nil {
		return MeetingArmArgs{}, fmt.Errorf("start is not an RFC 3339 time with its offset (such as 2026-10-26T14:00:00-04:00): %q", in.Start)
	}
	if a.End, err = time.Parse(time.RFC3339, strings.TrimSpace(in.End)); err != nil {
		return MeetingArmArgs{}, fmt.Errorf("end is not an RFC 3339 time with its offset (such as 2026-10-26T15:00:00-04:00): %q", in.End)
	}
	switch {
	case !a.End.After(a.Start):
		return MeetingArmArgs{}, errors.New("the meeting ends before it starts")
	case a.End.Sub(a.Start) > MaxMeetingLength:
		return MeetingArmArgs{}, fmt.Errorf("a meeting armed is at most %d hours long", int(MaxMeetingLength.Hours()))
	case !a.Closes().After(now):
		return MeetingArmArgs{}, errors.New("that meeting is over")
	case a.Opens().After(now.Add(ArmAhead)):
		return MeetingArmArgs{}, fmt.Errorf("the window would open at %s, more than an hour from now (%s): arm it within the hour before it starts, with a task timed for then",
			a.Opens().Format(time.RFC3339), now.In(a.Start.Location()).Format(time.RFC3339))
	}
	if a.Title, err = meetingText("title", in.Title, MaxMeetingTitle); err != nil {
		return MeetingArmArgs{}, err
	}
	if a.Title == "" {
		return MeetingArmArgs{}, errors.New("meeting_arm needs the meeting's title")
	}
	if len(in.Attendees) > MaxMeetingAttendees {
		return MeetingArmArgs{}, fmt.Errorf("at most %d attendees", MaxMeetingAttendees)
	}
	for _, at := range in.Attendees {
		v, err := meetingText("an attendee", at, MaxMeetingAttendee)
		if err != nil {
			return MeetingArmArgs{}, err
		}
		if v != "" {
			a.Attendees = append(a.Attendees, v)
		}
	}
	if a.App, err = meetingText("app", in.App, MaxMeetingAppHint); err != nil {
		return MeetingArmArgs{}, err
	}
	return a, nil
}

// ParseMeetingCancel reads meeting_cancel's argument, the window's id.
func ParseMeetingCancel(raw json.RawMessage) (string, error) {
	var in struct {
		Window string `json:"window"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if len(bytes.TrimSpace(raw)) == 0 || dec.Decode(&in) != nil {
		return "", errors.New("meeting_cancel takes the window's id, and nothing else")
	}
	id, err := meetingText("window", in.Window, MaxMeetingWindowID)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", errors.New("meeting_cancel needs the window's id, from meeting_arm or meeting_list")
	}
	return id, nil
}

// meetingText is one line of text, its spaces collapsed, of at most max
// characters, with no control character.
func meetingText(what, s string, max int) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%s is not UTF-8 text", what)
	}
	for _, r := range s {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' || r == 0x7f {
			return "", fmt.Errorf("%s holds a control character", what)
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > max {
		return "", fmt.Errorf("%s is longer than %d characters", what, max)
	}
	return s, nil
}
