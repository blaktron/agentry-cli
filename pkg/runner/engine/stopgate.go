package engine

// The stop gate. A harness that asks before it ends a turn
// (adapter.Gated) is answered on its running process where any other
// harness is started again on its conversation: when the model answers with
// no reply, or with reasoning only, after a tool call, and when it answers
// with steps of its checklist still open, the engine tells it to go on with
// the same message a restart would carry (restart.go, finishcheck.go). The
// rules are the restarts' own: at most finishChecks asks a run, none with
// less than finishCheckMargin of the harness's time left, none of a run
// that ended blocked. Each ask is recorded in the receipt as a restart is.
//
// The harness's stdin is an OS pipe, not one of the process's own: the
// engine client hands a file straight to the container's process, so a
// harness that exits while the pipe is still open ends its run, where a
// pipe copied by a goroutine would be waited on for as long as it stayed
// open.

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// stopGate is the engine's end of a gated harness's stdin.
type stopGate struct {
	adapter adapter.Gated
	r, w    *os.File
	// prompt is done once the task has been written, or could not be.
	prompt chan struct{}
	once   sync.Once
}

// openStopGate opens the harness's stdin and writes the task to it as the
// prompt, on a goroutine of its own: a task larger than the pipe holds is
// taken only once the harness reads.
func openStopGate(a adapter.Gated, task []byte) (*stopGate, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("open the harness's stdin: %w", err)
	}
	g := &stopGate{adapter: a, r: r, w: w, prompt: make(chan struct{})}
	go func() {
		defer close(g.prompt)
		_, _ = w.Write(a.PromptCommand(task))
	}()
	return g, nil
}

// stdin is what the harness reads.
func (g *stopGate) stdin() io.Reader { return g.r }

// answer tells the harness to go on with message, or, when message is "",
// that its turn ends. A harness that has gone is not told; its exit is the
// run's outcome.
func (g *stopGate) answer(message string) {
	<-g.prompt
	_, _ = g.w.Write(g.adapter.EndingAnswer(message))
}

// close ends the harness's stdin once its process has ended. A nil gate is
// a run that has none.
func (g *stopGate) close() {
	if g == nil {
		return
	}
	g.once.Do(func() {
		// The reading end first: a prompt still being written to a
		// harness that never read it then fails instead of waiting.
		_ = g.r.Close()
		_ = g.w.Close()
	})
}

// endingAnswer is the engine's answer to a harness about to end its turn on
// the model's answer: the message to go on with, or "" to let it end. c is
// the running start's collector, read and reset on the goroutine that
// records its events.
func (rd *runDirs) endingAnswer(ctx context.Context, p Params, c *eventCollector) string {
	if len(rd.restarts) >= finishChecks || !rd.canRestartAfter(ctx, finishCheckMargin) ||
		(rd.callsLedger != "" && blockedOf(rd.callsLedger) != nil) {
		return ""
	}
	c.flushLiveText()
	reason, message, what := c.emptyEnd(), emptyAnswerMessage, ""
	if reason != "" {
		what = "ended its turn with " + restartWhat(reason)
	} else {
		var open []toolsrv.Step
		if rd.checklistCopy != "" {
			steps, _ := readChecklistCopy(rd.checklistCopy)
			open = toolsrv.OpenSteps(steps)
		}
		if len(open) == 0 {
			return ""
		}
		reason = receipt.RestartFinishCheck
		message = finishCheckMessage(open, len(rd.restarts)+1 == finishChecks)
		what = fmt.Sprintf("finished with %d checklist step(s) open", len(open))
		rd.finishChecks = append(rd.finishChecks, receipt.FinishCheck{Open: len(open), Message: rd.redact.String(message)})
	}
	rd.restarts = append(rd.restarts, receipt.Restart{Reason: reason, Message: rd.redact.String(message)})
	noteAsked(p.Stderr, what, len(rd.restarts))
	// The turn goes on: what the model wrote so far is not its answer.
	c.discardText()
	return message
}

// noteAsked tells an autonomous run's stderr that its harness was told to
// go on, for the nth time.
func noteAsked(w io.Writer, what string, n int) {
	if w != nil {
		_, _ = fmt.Fprintf(w, "note: the agent %s; it was told to go on (%d of %d)\n", what, n, finishChecks)
	}
}
