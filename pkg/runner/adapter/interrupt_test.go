package adapter_test

import (
	"context"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter/copilottest"
)

// Interrupt is session.abort of the session in flight, and the turn ends
// with what Copilot had said and the stop reason "cancelled".
func TestACopilotInterruptAbortsTheTurnInFlight(t *testing.T) {
	said := make(chan struct{})
	srv := &copilottest.Server{
		Turn: func(c *copilottest.Conn, _ string) {
			c.Event("assistant.message", map[string]any{"messageId": "a1", "content": "Half an answer"})
			close(said)
		},
		Abort: func(c *copilottest.Conn) {
			c.Event("abort", map[string]any{"reason": "user initiated"})
			c.Event("session.idle", map[string]any{})
		},
	}
	k := &tokens{expires: time.Now().Add(8 * time.Hour)}
	d := drive(t, srv, adapter.SessionOptions{GatewayTools: []string{"write_report"}, ModelSignIn: true, SignInToken: k.get, GatewayToken: "gateway-bearer-0003"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		turn *adapter.Turn
		err  error
	}
	got := make(chan answer, 1)
	go func() {
		tr, err := d.drv.Prompt(ctx, "write me an essay")
		got <- answer{tr, err}
	}()
	select {
	case <-said:
	case <-ctx.Done():
		t.Fatal("the turn never started")
	}
	if err := d.drv.(adapter.Interrupter).Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	a := <-got
	if a.err != nil || !a.turn.Stopped() || a.turn.Text != "Half an answer" {
		t.Fatalf("Prompt after an interrupt: %+v, %v", a.turn, a.err)
	}
	// The next turn is taken as usual.
	srv.Turn = nil
	if tr, err := d.drv.Prompt(ctx, "again"); err != nil || tr.Stopped() {
		t.Fatalf("the turn after: %+v, %v", tr, err)
	}
}
