package persistent

// The switch that lets an agent set up meeting recordings. With it on, the
// agent's meeting tools (runnerimage/applink) reach its client, Agentry
// Desktop, which arms a window around a meeting the agent found on the
// person's calendar and records a call it detects inside it. The switch is
// read at each call, like the heartbeat, which it needs: an agent arms a
// meeting within the hour before it starts, from a task its heartbeat fires.

import (
	"slices"

	"github.com/blaktron/agentry-cli/runnerimage/applink"
)

// EntryMeetings is the roster's switch for an agent's meeting tools.
const EntryMeetings = "meetings"

// KindMeetings is the kind of the meetings entry.
const KindMeetings EntryKind = "meetings"

// The meetings entry's name and consent sentence.
const (
	meetingsName = "Can set up meeting recordings"
	meetingsLine = "Has Agentry Desktop record and transcribe the calls of meetings it finds on your calendar that you asked it to record: each armed recording shows in Agentry, where you can cancel it, and it needs its heartbeat and Meeting notes on"
)

// The meeting tools' answers while the agent may not use them: the switch
// off, the switch and the heartbeat off, or the heartbeat alone off.
const (
	meetingsOff        = "The person has not let you set up meeting recordings: they switch on \"" + meetingsName + "\" in Agentry. Tell them so if they asked you to record a meeting, and do not ask again unless they bring it up."
	meetingsOffNoBeats = "The person has not let you set up meeting recordings, and your heartbeat is off: they switch on \"Its heartbeat and tasks\", then \"" + meetingsName + "\", in Agentry. Tell them so if they asked you to record a meeting, and do not ask again unless they bring it up."
	meetingsNoBeats    = "Your heartbeat is off, so you cannot set up meeting recordings: the person switches on \"Its heartbeat and tasks\" in Agentry. Tell them so if they asked you to record a meeting."
)

// readAtUse says the roster entry id is read when it is used, not given to
// a segment: switching it takes effect at once and restarts nothing.
func readAtUse(id string) bool { return id == EntryHeartbeat || id == EntryMeetings }

// meetingsOn says the agent may use its meeting tools: its roster has the
// meetings entry on and the person agreed to it as its sentence reads now.
// A config.yaml switched some other way lets it do nothing.
func meetingsOn(a Agent, c Config) bool {
	if !a.Builtin.ArmsMeetings() {
		return false
	}
	r := BuildRoster(a, c, nil)
	e, ok := r.Entry(EntryMeetings)
	if !ok || !e.On {
		return false
	}
	return !slices.ContainsFunc(Unconsented(r, c), func(u RosterEntry) bool { return u.ID == EntryMeetings })
}

// refuseMeeting answers a meeting tool's call the supervisor does not hand
// on: the switch or the heartbeat off, or a meeting_arm or meeting_cancel
// whose arguments break their rules. The application holds the arguments
// to the same rules again.
func (s *Supervisor) refuseMeeting(req applink.Request) (applink.Answer, bool) {
	cfg, err := LoadConfig(s.opts.Home, s.opts.Agent.Name)
	if err != nil {
		return applink.Answer{Text: "your configuration could not be read: " + err.Error(), IsError: true}, true
	}
	on, beats := meetingsOn(s.opts.Agent, cfg), heartbeatOn(s.opts.Agent, cfg)
	switch {
	case !on && !beats:
		return applink.Answer{Text: meetingsOffNoBeats, IsError: true}, true
	case !on:
		return applink.Answer{Text: meetingsOff, IsError: true}, true
	case !beats:
		return applink.Answer{Text: meetingsNoBeats, IsError: true}, true
	}
	switch req.Tool {
	case applink.MeetingArm:
		if _, err := applink.ParseMeetingArm(req.Arguments, s.now()); err != nil {
			return applink.Answer{Text: "not armed: " + err.Error(), IsError: true}, true
		}
	case applink.MeetingCancel:
		if _, err := applink.ParseMeetingCancel(req.Arguments); err != nil {
			return applink.Answer{Text: err.Error(), IsError: true}, true
		}
	}
	return applink.Answer{}, false
}
