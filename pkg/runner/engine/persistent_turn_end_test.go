package engine

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// startSegment starts the persistent fixture's segment with harness h.
func startSegment(t *testing.T, p Params, f *dockerx.Fake, h *fakeHarness) (session.Session, func() []session.Event, <-chan struct{}) {
	t.Helper()
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	get, done := collect(s)
	return s, get, done
}

func eventsOfKind(evs []session.Event, kind session.Kind) []session.Event {
	var out []session.Event
	for _, ev := range evs {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// Interrupt stops the turn in flight: the harness is told to, the turn
// closes with what the agent had said and the stop reason "interrupted",
// its Send says it was stopped, and the next turn is taken as usual.
func TestAnInterruptStopsTheTurnAndTheSegmentGoesOn(t *testing.T) {
	p, f, _ := persistentFixture(t, "")
	h := &fakeHarness{loadSession: true, holdForCancel: true}
	s, get, done := startSegment(t, p, f, h)
	it, ok := s.(session.Interrupter)
	if !ok {
		t.Fatal("a segment cannot be interrupted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := it.Interrupt(ctx); !errors.Is(err, session.ErrNoTurn) {
		t.Fatalf("Interrupt with no turn = %v", err)
	}
	sent := make(chan error, 1)
	go func() { sent <- s.Send(ctx, "write me an essay") }()
	for len(eventsOfKind(get(), session.KindAssistantText)) == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("the turn never started")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := it.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if err := <-sent; !errors.Is(err, session.ErrInterrupted) {
		t.Fatalf("Send of a stopped turn = %v", err)
	}
	h.mu.Lock()
	cancels := h.cancels
	h.holdForCancel = false
	h.mu.Unlock()
	if cancels != 1 {
		t.Fatalf("session/cancel sent %d times", cancels)
	}
	if err := s.Send(ctx, "again"); err != nil {
		t.Fatalf("the turn after a stop: %v", err)
	}
	if ended, why := s.Ended(); ended {
		t.Fatalf("the segment ended (%s)", why)
	}
	_, _ = s.End(ctx, false)
	<-done
	turns := eventsOfKind(get(), session.KindAssistantTurn)
	if len(turns) != 2 || turns[0].StopReason != session.StopInterrupted || turns[0].Text != "Half an answer" || turns[1].StopReason == session.StopInterrupted {
		t.Fatalf("assistant turns %+v", turns)
	}
	if errs := eventsOfKind(get(), session.KindError); len(errs) != 0 {
		t.Fatalf("a stopped turn is not an error: %+v", errs)
	}
}

// limitAdapter is the fixture's adapter whose driver meets the plan's
// limit at every turn, as codex does on a ChatGPT plan: the limit, then the
// turn's failure restating it.
type limitAdapter struct{ adapter.Adapter }

func (a limitAdapter) Session(ctx context.Context, rw io.ReadWriteCloser, o adapter.SessionOptions) (adapter.SessionDriver, error) {
	d, err := a.Adapter.Session(ctx, rw, o)
	if err != nil {
		return nil, err
	}
	out := make(chan adapter.Event, 64)
	go func() {
		for ev := range d.Events() {
			out <- ev
		}
		close(out)
	}()
	return &limitDriver{SessionDriver: d, out: out}, nil
}

type limitDriver struct {
	adapter.SessionDriver
	out chan adapter.Event
}

func (d *limitDriver) Events() <-chan adapter.Event { return d.out }

func (d *limitDriver) Prompt(context.Context, string) (*adapter.Turn, error) {
	const said = "The ChatGPT user has reached their usage limit."
	d.out <- adapter.Event{Kind: adapter.EventPlanLimit, Text: said}
	return nil, errors.New("codex session: turn failed: stream disconnected before completion: " + said)
}

// A persistent agent outlives its plan's limit: each turn that meets it
// fails with one error naming the limit, and the segment goes on, so the
// next turn tries again.
func TestAPersistentAgentOutlivesItsPlansLimit(t *testing.T) {
	p, f, h := persistentFixture(t, "")
	p.Adapter = limitAdapter{p.Adapter}
	s, get, done := startSegment(t, p, f, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		if err := s.Send(ctx, "hello"); err == nil {
			t.Fatalf("turn %d met the limit and succeeded", i+1)
		}
		if ended, why := s.Ended(); ended {
			t.Fatalf("turn %d ended the segment (%s)", i+1, why)
		}
	}
	env, err := s.End(ctx, false)
	<-done
	if err != nil || env.Reason != nil {
		t.Fatalf("end = %v (%+v): a segment that outlived the limit is not ended by it", err, env.Reason)
	}
	errs := eventsOfKind(get(), session.KindError)
	if len(errs) != 2 {
		t.Fatalf("errors %+v, want one for each turn", errs)
	}
	for _, e := range errs {
		if e.Text != "plan limit reached: The ChatGPT user has reached their usage limit." {
			t.Errorf("error %q", e.Text)
		}
	}
}
