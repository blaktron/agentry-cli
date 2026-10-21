package engine

// An autonomous run driven as one turn of its harness's session protocol
// (spec.HarnessSpec.OneTurn): the codex adapters on a ChatGPT plan run
// app-server for both modes, because OpenAI's plan usage serves
// app-server's requests (its originator is the app's name) and not exec's.
// The engine opens the session over the harness's stdio, sends the task as
// its one user turn and ends the session; the driver's events go through
// the same collector as a stdout stream's, so the budgets, the result, the
// plan's limit and the receipts are the autonomous run's.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
)

// oneTurnHandshake bounds the session's handshake, as a session's does.
const oneTurnHandshake = 120 * time.Second

// oneTurnExitWait is how long the harness has to exit once the session is
// ended (stdin closed) before kill stops it.
var oneTurnExitWait = 30 * time.Second

// oneTurn drives the task as the one turn of the plan's session protocol
// over rw (the harness's stdout and stdin), recording every event in c. It
// returns the turn's error, which the caller classifies after c's own
// outcomes (a plan's limit, a budget). opts are the driver's
// (sessionOptions), the gateway tools it may approve among them. kill
// stops the harness when it does not exit after the session ends; a panic
// while recording is c's error and kills it too.
func oneTurn(ctx context.Context, p Params, rw io.ReadWriteCloser, task []byte, opts adapter.SessionOptions, c *eventCollector, kill func(error)) error {
	drv, err := p.Adapter.Session(ctx, rw, opts)
	if err != nil {
		_ = rw.Close()
		return fmt.Errorf("one-turn session: %w", err)
	}
	pumped := make(chan struct{})
	// synced answers the tool calls recorded once every event the driver
	// had queued when it was asked is recorded (a capacity wait's check).
	synced := make(chan chan int)
	go func() {
		defer close(pumped)
		defer func() {
			if r := recover(); r != nil {
				c.panicErr = fmt.Errorf("event stream panicked: %v", r)
				kill(c.panicErr)
				// Drain, so the driver never blocks on a channel nobody reads.
				for range drv.Events() {
				}
			}
		}()
		record := func(ev adapter.Event) {
			if len(ev.Raw) > 0 {
				c.recordRaw(ev.Raw)
			}
			c.record(ev)
		}
		events := drv.Events()
		for open := true; open; {
			select {
			case ev, ok := <-events:
				if open = ok; ok {
					record(ev)
				}
			case ack := <-synced:
				for drained := false; !drained && open; {
					select {
					case ev, ok := <-events:
						if open = ok; ok {
							record(ev)
						}
					default:
						drained = true
					}
				}
				ack <- c.turns
			}
		}
		c.flushLiveText()
	}()
	toolCalls := func() (int, bool) {
		ack := make(chan int, 1)
		select {
		case synced <- ack:
			return <-ack, true
		case <-pumped:
			return 0, false
		}
	}
	hs, cancel := context.WithTimeout(ctx, oneTurnHandshake)
	err = drv.Start(hs)
	cancel()
	if err == nil {
		_, err = promptAtCapacity(ctx, drv, string(task), func(n int) bool {
			if calls, ok := toolCalls(); !ok || calls > 0 {
				return false
			}
			noteCapacity(p.Stderr, n)
			return true
		})
	}
	// Ending the session closes the harness's stdin, on which it exits; one
	// that does not is stopped.
	stop := time.AfterFunc(oneTurnExitWait, func() { kill(fmt.Errorf("the harness did not exit within %s of its session ending", oneTurnExitWait)) })
	_ = drv.Close()
	stop.Stop()
	<-pumped
	return err
}

// sessionOptions is what a session's driver is told of the run: the
// gateway tools it may approve, whether a model sign-in pays (and, for a
// harness that asks its driver for its token, the token source), the
// gateway's bearer and the plan's harness files as rendered, for a driver
// that hands its harness the gateway and the instruction over the session.
func sessionOptions(p Params, rd *runDirs, token string, tools []string) adapter.SessionOptions {
	return adapter.SessionOptions{
		GatewayTools: tools,
		ModelSignIn:  p.Plan.Harness.ModelSignIn != nil,
		SignInToken:  rd.signInTokenFunc(),
		GatewayToken: token,
		HarnessFile:  rd.harnessFile,
	}
}

// harnessFile reads one of the plan's harness files as the run rendered it
// (renderPlanFiles), by its name under the plan's harness folder.
func (rd *runDirs) harnessFile(name string) ([]byte, error) {
	if !filepath.IsLocal(name) {
		return nil, fmt.Errorf("no harness file %q in the plan", name)
	}
	return os.ReadFile(filepath.Join(rd.cfg, "harness", filepath.FromSlash(name))) //nolint:gosec // the run's own rendered plan
}
