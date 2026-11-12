package remote

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/session/protocol"
)

// frame is one line of the runhost's session protocol as the device takes it.
func frame(t *testing.T, ev protocol.Event) runhost.Frame {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return runhost.Frame{Result: raw}
}

// sentCommand is the next command the session wrote for the runhost.
func sentCommand(t *testing.T, s *Session) protocol.Command {
	t.Helper()
	select {
	case line := <-s.cmds:
		var in runhost.SessionInput
		if err := json.Unmarshal(line, &in); err != nil || in.Command == nil {
			t.Fatalf("the session wrote %s (%v)", line, err)
		}
		var c protocol.Command
		if err := json.Unmarshal(*in.Command, &c); err != nil {
			t.Fatal(err)
		}
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("the session wrote no command")
		return protocol.Command{}
	}
}

// TestAMessageDuringATurnOnARunhost: a steer goes to the runhost's session as
// its own command and is settled by that session's steer event, or by the
// refusal it answers with. A runhost whose CLI does not know the command is
// one whose harness cannot take the message.
func TestAMessageDuringATurnOnARunhost(t *testing.T) {
	ctx := context.Background()
	steer := func(s *Session) chan error {
		out := make(chan error, 1)
		go func() { out <- s.Steer(ctx, "and the date") }()
		if c := sentCommand(t, s); c.Steer == nil || c.Steer.Text != "and the date" {
			t.Fatalf("the command sent: %+v", c)
		}
		return out
	}
	var _ session.Steerer = (*Session)(nil)

	// Taken: the session's own steer event answers it. The runner's notice
	// of an added file is a steer event too, and answers nothing.
	s := NewSession(nil)
	out := steer(s)
	s.Frame(frame(t, protocol.Event{Event: session.Event{Kind: session.KindSteer, Text: "A new input file is available", Synthetic: true}}))
	select {
	case err := <-out:
		t.Fatalf("the runner's own notice settled the person's message: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	s.Frame(frame(t, protocol.Event{Event: session.Event{Kind: session.KindSteer, Text: "and the date"}}))
	if err := <-out; err != nil {
		t.Errorf("a message the runhost's session took: %v", err)
	}

	for _, tc := range []struct {
		refused protocol.Refusal
		is      error
		says    string
	}{
		{protocol.RefusedNoTurn, session.ErrNoTurn, ""},
		{protocol.RefusedCannotSteer, session.ErrCannotSteer, "takes one only between turns"},
		{protocol.RefusedUnknown, session.ErrCannotSteer, "the runhost's CLI is older than the command"},
		{protocol.RefusedSteer, nil, "the runhost's session refused it (steer_refused): the limit is reached"},
	} {
		s := NewSession(nil)
		// A turn is in flight, as it is whenever a message is sent into one.
		turn := make(chan error, 1)
		go func() { turn <- s.Send(ctx, "first") }()
		sentCommand(t, s)
		out := steer(s)
		text := "the limit is reached"
		if tc.refused == protocol.RefusedCannotSteer {
			text = "the docker-agent harness takes one only between turns"
		}
		s.Frame(frame(t, protocol.Event{Event: session.Event{Kind: session.KindError, Text: text}, Refused: tc.refused}))
		err := <-out
		if err == nil || (tc.is != nil && !errors.Is(err, tc.is)) || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: %v", tc.refused, err)
		}
		// The refusal was the message's, and the turn is still in flight.
		select {
		case err := <-turn:
			t.Errorf("%s: the turn was settled with %v", tc.refused, err)
		case <-time.After(20 * time.Millisecond):
		}
		s.Frame(frame(t, protocol.Event{Event: session.Event{Kind: session.KindAssistantTurn, Text: "done"}}))
		if err := <-turn; err != nil {
			t.Errorf("%s: the turn after it: %v", tc.refused, err)
		}
	}

	// A command the runhost does not know, with no message in flight, is the
	// turn's to hear.
	s = NewSession(nil)
	turn := make(chan error, 1)
	go func() { turn <- s.Send(ctx, "first") }()
	sentCommand(t, s)
	s.Frame(frame(t, protocol.Event{Event: session.Event{Kind: session.KindError, Text: "unknown command"}, Refused: protocol.RefusedUnknown}))
	if err := <-turn; err == nil || errors.Is(err, session.ErrCannotSteer) {
		t.Errorf("an unknown command with no message in flight: %v", err)
	}

	// The session's end answers a message still waiting.
	s = NewSession(nil)
	out = steer(s)
	s.Frame(frame(t, protocol.Event{Event: session.Event{Kind: session.KindSessionEnd, Reason: session.EndQuit}}))
	if err := <-out; !errors.Is(err, session.ErrEnded) {
		t.Errorf("a message when the session ended: %v", err)
	}
	if err := s.Steer(ctx, "after the end"); !errors.Is(err, session.ErrEnded) {
		t.Errorf("a message after the end: %v", err)
	}
}
