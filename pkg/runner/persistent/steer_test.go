package persistent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// steeringSegment is a fakeSegment whose harness takes a message during a
// turn: each is recorded with a steer event and joined with a steered one,
// as the engine's session records them.
type steeringSegment struct {
	*fakeSegment
	smu     sync.Mutex
	steered []string
}

func (s *steeringSegment) Steer(_ context.Context, text string) error {
	s.mu.Lock()
	inTurn := s.stop != nil
	s.mu.Unlock()
	if !inTurn {
		return session.ErrNoTurn
	}
	s.smu.Lock()
	s.steered = append(s.steered, text)
	s.smu.Unlock()
	s.events <- session.Event{Kind: session.KindSteer, Text: text}
	s.events <- session.Event{Kind: session.KindSteered}
	return nil
}

func (s *steeringSegment) messages() []string {
	s.smu.Lock()
	defer s.smu.Unlock()
	return slices.Clone(s.steered)
}

// steerer is a chat attached to the agent, as agent chat attaches.
func steerer(t *testing.T, home string) (session.Steerer, session.Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Attach(ctx, home, "assistant", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.End(context.Background(), false) })
	st, ok := s.(session.Steerer)
	if !ok {
		t.Fatal("an attached chat cannot steer")
	}
	return st, s
}

// A chat's message during another chat's turn joins that turn: the steer
// event reaches every chat attached, and the turn's own answer still ends
// it. With no turn in flight the message is a turn.
func TestAChatsMessageJoinsAChatsTurnInFlight(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	hold := make(chan struct{})
	st := &starter{hold: hold, steers: true}
	sv := startUpkeeper(t, home, st, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, firstSess := steerer(t, home)
	second, _ := steerer(t, home)
	if err := second.Steer(ctx, "too early"); !errors.Is(err, session.ErrNoTurn) {
		t.Fatalf("a steer with no turn in flight = %v, want ErrNoTurn", err)
	}
	sent := make(chan error, 1)
	go func() { sent <- firstSess.Send(ctx, "write me an essay") }()
	waitFor(t, "the turn in flight", func() bool { return sv.Status().Busy })

	// The other chat steers the first one's turn.
	if err := second.Steer(ctx, "keep it short"); err != nil {
		t.Fatalf("a steer into a chat's turn = %v", err)
	}
	if got := st.steering.messages(); !slices.Equal(got, []string{"keep it short"}) {
		t.Fatalf("the harness was handed %q", got)
	}
	// The chat that sent the turn sees the steer among its events.
	var sawSteer bool
	for !sawSteer {
		select {
		case ev := <-firstSess.Events():
			sawSteer = ev.Kind == session.KindSteer && ev.Text == "keep it short"
		case <-ctx.Done():
			t.Fatal("the sending chat never saw the steer")
		}
	}
	close(hold)
	if err := <-sent; err != nil {
		t.Fatalf("the steered turn's Send = %v", err)
	}
}

// A message during a turn the agent runs on its own waits: the turn is a
// task's, a messenger's or upkeep's, never a chat's.
func TestAMessageDuringTheAgentsOwnTurnWaits(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	hold := make(chan struct{})
	defer close(hold)
	st := &starter{hold: hold, steers: true}
	sv := startUpkeeper(t, home, st, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A turn the runner sends itself, as a task's is sent.
	sv.turn.Lock()
	go func() {
		defer sv.turn.Unlock()
		_ = sv.sendLocked(ctx, "(A task came due.)\n\n", "check the inbox")
	}()
	waitFor(t, "the task's turn in flight", func() bool { return sv.Status().Busy })
	chat, _ := steerer(t, home)
	err := chat.Steer(ctx, "also water the plants")
	if !errors.Is(err, session.ErrCannotSteer) || !strings.Contains(err.Error(), "agent's own") {
		t.Fatalf("a steer into the agent's own turn = %v, want ErrCannotSteer saying why", err)
	}
	if got := st.steering.messages(); len(got) != 0 {
		t.Fatalf("the task's turn was handed %q", got)
	}
}

// A harness that takes a message only between turns refuses the steer as
// such, and the chat keeps the text for the next turn.
func TestASteerOnAHarnessThatCannotTakeOne(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	hold := make(chan struct{})
	defer close(hold)
	st := &starter{hold: hold}
	sv := startUpkeeper(t, home, st, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	chat, sess := steerer(t, home)
	go func() { _ = sess.Send(ctx, "write me an essay") }()
	waitFor(t, "the turn in flight", func() bool { return sv.Status().Busy })
	err := chat.Steer(ctx, "keep it short")
	if !errors.Is(err, session.ErrCannotSteer) {
		t.Fatalf("a steer on a harness that cannot take one = %v, want ErrCannotSteer", err)
	}
	if n := strings.Count(err.Error(), session.ErrCannotSteer.Error()); n != 1 {
		t.Fatalf("the refusal says why %d times: %q", n, err)
	}
	if err := sv.Steer(ctx, "  "); err == nil {
		t.Fatal("an empty message was taken")
	}
}
