package persistent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/applink"
)

// TestTheMeetingsEntryIsOffUntilAgreedAndNeedsTheHeartbeat: the Assistant
// alone has the entry, off by default, saying it records calls of meetings
// on the person's calendar; switching it on without the heartbeat is
// refused, and with it asks the person first; it restarts nothing and is
// never pending.
func TestTheMeetingsEntryIsOffUntilAgreedAndNeedsTheHeartbeat(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	for _, name := range []string{"main", "coder"} {
		a, err := Lookup(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := BuildRoster(a, Config{}, nil).Entry(EntryMeetings); ok {
			t.Errorf("the %s agent has a meetings entry", name)
		}
	}
	a := assistantAgent(t)
	e, ok := BuildRoster(a, Config{}, nil).Entry(EntryMeetings)
	if !ok || e.On || e.State != StateOff || e.Kind != KindMeetings || e.GrantedBy != GrantedByBuiltin || e.Fixed {
		t.Fatalf("the meetings entry: %+v", e)
	}
	for _, want := range []string{"record and transcribe the calls of meetings it finds on your calendar", "that you asked it to record", "where you can cancel it", "heartbeat"} {
		if !strings.Contains(e.Line, want) {
			t.Errorf("the sentence %q lacks %q", e.Line, want)
		}
	}

	c := Config{Enabled: true}
	_, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: EntryMeetings, On: true}, Consent: true})
	if err == nil || !strings.Contains(err.Error(), "heartbeat") || !strings.Contains(err.Error(), "agentry agent grant assistant heartbeat on") {
		t.Fatalf("switched on without the heartbeat: %v", err)
	}
	if c.Switches[EntryMeetings] {
		t.Fatal("a refused switch was kept")
	}
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: EntryHeartbeat, On: true}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	_, err = ApplyChange(home, a, &c, Change{Switch: &Switch{ID: EntryMeetings, On: true}})
	var ce *ConsentError
	if !errors.As(err, &ce) || len(ce.Entries) != 1 || ce.Entries[0].ID != EntryMeetings || ce.Entries[0].Line != e.Line {
		t.Fatalf("switched on without agreement: %v", err)
	}
	if meetingsOn(a, c) {
		t.Fatal("on before the person agreed")
	}
	restart, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: EntryMeetings, On: true}, Consent: true})
	if err != nil || restart {
		t.Fatalf("switching it on: restart %v, %v", restart, err)
	}
	if !meetingsOn(a, c) || c.Consented[EntryMeetings] != e.Line {
		t.Fatalf("on and agreed: %+v", c)
	}
	applied := &Applied{}
	if got, _ := BuildRoster(a, c, applied).Entry(EntryMeetings); got.State != StateOn {
		t.Errorf("a running segment's meetings entry reads %s, want on and never pending", got.State)
	}
	// A config.yaml switched by hand, with no agreement, lets it do nothing.
	hand := Config{Enabled: true, Switches: map[string]bool{EntryHeartbeat: true, EntryMeetings: true}, Consented: map[string]string{EntryHeartbeat: heartbeatLine}}
	if meetingsOn(a, hand) {
		t.Error("a hand-switched meetings entry counts as on")
	}
}

// TestTheMeetingToolsAreAnsweredOnlyWhileTheSwitchIsOn: off, each meeting
// tool is refused by the supervisor and never reaches the application; on,
// a meeting_arm within the hour reaches it, and one whose window opens more
// than an hour from now, one that is over, or one with an argument it does
// not take is refused without reaching it. With the heartbeat switched off
// afterwards, the tools are refused again.
func TestTheMeetingToolsAreAnsweredOnlyWhileTheSwitchIsOn(t *testing.T) {
	home := shortHome(t)
	enableHeartbeat(t, home, true)
	clk := &clock{t: time.Date(2026, 10, 26, 13, 30, 0, 0, time.UTC)}
	var asked []applink.Request
	sv, err := Start(context.Background(), Options{
		Home: home, Agent: assistantAgent(t), Client: ClientDesktop, Policy: policy.Default(), Docker: &dockerx.Fake{},
		StartSegment: (&starter{resumed: true}).start, Now: clk.now, Tick: time.Hour,
		AppLink: func(_ context.Context, req applink.Request) applink.Answer {
			asked = append(asked, req)
			return applink.Answer{Text: "armed w1"}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = sv.Stop(context.Background()) })

	soon := `{"start":"2026-10-26T14:00:00Z","end":"2026-10-26T15:00:00Z","title":"Budget review","attendees":["Sam Li"],"app":"zoom"}`
	for _, tool := range []string{applink.MeetingArm, applink.MeetingList, applink.MeetingCancel} {
		args := map[string]string{applink.MeetingArm: soon, applink.MeetingList: `{}`, applink.MeetingCancel: `{"window":"w1"}`}[tool]
		if got := callTask(t, sv, tool, args); !got.IsError || got.Text != meetingsOff {
			t.Errorf("%s with the switch off: %+v", tool, got)
		}
	}
	if len(asked) != 0 {
		t.Fatalf("the application was asked while the switch was off: %v", asked)
	}
	if _, err := sv.Change(Change{Switch: &Switch{ID: EntryHeartbeat, On: false}}); err != nil {
		t.Fatal(err)
	}
	if got := callTask(t, sv, applink.MeetingList, `{}`); !got.IsError || got.Text != meetingsOffNoBeats {
		t.Errorf("meeting_list with the switch and the heartbeat off: %+v", got)
	}
	if _, err := sv.Change(Change{Switch: &Switch{ID: EntryHeartbeat, On: true}, Consent: true}); err != nil {
		t.Fatal(err)
	}

	if _, err := sv.Change(Change{Switch: &Switch{ID: EntryMeetings, On: true}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	if got := callTask(t, sv, applink.MeetingArm, soon); got.IsError || got.Text != "armed w1" || len(asked) != 1 {
		t.Fatalf("meeting_arm within the hour: %+v, asked %d", got, len(asked))
	}
	for name, args := range map[string]string{
		"opens in more than an hour": `{"start":"2026-10-26T14:40:00Z","end":"2026-10-26T15:00:00Z","title":"Later"}`,
		"is over":                    `{"start":"2026-10-26T12:00:00Z","end":"2026-10-26T13:10:00Z","title":"Earlier"}`,
		"takes an unknown argument":  `{"start":"2026-10-26T14:00:00Z","end":"2026-10-26T15:00:00Z","title":"x","record":"always"}`,
		"has no title":               `{"start":"2026-10-26T14:00:00Z","end":"2026-10-26T15:00:00Z"}`,
	} {
		if got := callTask(t, sv, applink.MeetingArm, args); !got.IsError || !strings.HasPrefix(got.Text, "not armed: ") {
			t.Errorf("a meeting_arm that %s: %+v", name, got)
		}
	}
	if got := callTask(t, sv, applink.MeetingCancel, `{}`); !got.IsError {
		t.Errorf("meeting_cancel with no window: %+v", got)
	}
	if got := callTask(t, sv, applink.MeetingList, `{}`); got.IsError || len(asked) != 2 {
		t.Errorf("meeting_list: %+v, asked %d", got, len(asked))
	}

	if _, err := sv.Change(Change{Switch: &Switch{ID: EntryHeartbeat, On: false}}); err != nil {
		t.Fatal(err)
	}
	if got := callTask(t, sv, applink.MeetingArm, soon); !got.IsError || got.Text != meetingsNoBeats || len(asked) != 2 {
		t.Errorf("meeting_arm with the heartbeat off: %+v", got)
	}
}

// TestTheMeetingToolsNeedAgentryDesktop: run from the CLI, with no
// application to answer them, a meeting tool that passes the supervisor's
// checks is answered as Agentry Desktop's.
func TestTheMeetingToolsNeedAgentryDesktop(t *testing.T) {
	home := shortHome(t)
	enableHeartbeat(t, home, true)
	c, err := LoadConfig(home, "assistant")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyChange(home, assistantAgent(t), &c, Change{Switch: &Switch{ID: EntryMeetings, On: true}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(home, "assistant", c); err != nil {
		t.Fatal(err)
	}
	sv := startAssistant(t, home, &starter{resumed: true})
	defer func() { _, _ = sv.Stop(context.Background()) }()
	if got := callTask(t, sv, applink.MeetingList, `{}`); !got.IsError || !strings.Contains(got.Text, "Agentry Desktop") {
		t.Errorf("meeting_list with no application: %+v", got)
	}
}
