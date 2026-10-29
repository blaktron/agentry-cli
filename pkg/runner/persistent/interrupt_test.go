package persistent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// TestInterruptOverTheOwnerSocket: a client's interrupt stops the turn in
// flight, the turn's Send says it was stopped, the agent goes on, and an
// interrupt with no turn in flight says there was none to stop.
func TestInterruptOverTheOwnerSocket(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	hold := make(chan struct{})
	defer close(hold)
	st := &starter{hold: hold}
	sv := startUpkeeper(t, home, st, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events, unsubscribe := sv.Subscribe()
	defer unsubscribe()
	sent := make(chan error, 1)
	go func() { sent <- sv.Send(ctx, "write me an essay") }()
	waitFor(t, "the turn in flight", func() bool { return sv.Status().Busy })
	stopped, err := RequestInterrupt(ctx, home, "assistant")
	if err != nil || !stopped {
		t.Fatalf("interrupt over the socket = %v, %v", stopped, err)
	}
	if err := <-sent; !errors.Is(err, session.ErrInterrupted) {
		t.Fatalf("the stopped turn's Send = %v", err)
	}
	var closed *session.Event
	for closed == nil {
		select {
		case ev := <-events:
			if ev.Kind == session.KindAssistantTurn {
				closed = &ev
			}
		case <-ctx.Done():
			t.Fatal("the stopped turn never closed")
		}
	}
	if closed.StopReason != session.StopInterrupted {
		t.Fatalf("the turn closed with %+v", closed)
	}
	if sv.Status().Busy {
		t.Fatal("the agent is still busy after the stop")
	}
	stopped, err = RequestInterrupt(ctx, home, "assistant")
	if err != nil || stopped {
		t.Fatalf("an interrupt with no turn = %v, %v", stopped, err)
	}
}
