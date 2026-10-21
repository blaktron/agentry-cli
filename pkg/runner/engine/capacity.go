package engine

// A model provider that answers that the model is at capacity refuses the
// moment, not the run: nothing in the package or the person's setup can
// prevent it, and it clears on its own. So the engine waits it out and
// sends the request again, the same way for every harness:
//
//   - a session's turn (an interactive session, a persistent agent, a run
//     driven as one turn) is sent again on the same session;
//   - an autonomous harness that is one process is started again.
//
// Either only when the turn or the run failed before its first tool call:
// one whose tools already acted could make them act twice.
//
// Every wait is announced, at most capacityTries of them for one turn or
// one run, and none that would end past the run's timeout. When the tries
// run out the run fails with the provider's words, as any harness failure.

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// capacityWait is how long the engine waits after a capacity answer before
// it sends the request again; a variable so the tests need not wait.
var capacityWait = 60 * time.Second

// capacityTries is the most waits for one turn or one run.
const capacityTries = 10

// canWaitAtCapacity reports whether a wait of capacityWait can end before
// ctx does: a run whose timeout would pass during the wait fails now, with
// the provider's words, rather than later as a timeout.
func canWaitAtCapacity(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= capacityWait {
		return false
	}
	return true
}

// capacityNotice is what the person is told of the nth wait.
func capacityNotice(n int) string {
	return fmt.Sprintf("the model's provider says the model is at capacity; trying again in %s (%d of %d)", capacityWait, n, capacityTries)
}

// noteCapacity tells an autonomous run's stderr of the nth wait.
func noteCapacity(w io.Writer, n int) {
	if w != nil {
		_, _ = fmt.Fprintf(w, "note: %s\n", capacityNotice(n))
	}
}

// promptAtCapacity sends text as a session's turn and sends it again, on
// the same session, after each capacity answer it can wait out under ctx
// that again allows: again(n) is asked before the nth wait, after the failed
// attempt's events are recorded, and announces the wait when it answers
// true. It answers false once the turn has called a tool, since sending the
// turn again could make its tools act twice.
func promptAtCapacity(ctx context.Context, drv adapter.SessionDriver, text string, again func(n int) bool) (*adapter.Turn, error) {
	res, err := drv.Prompt(ctx, text)
	for n := 1; err != nil && n <= capacityTries && adapter.AtCapacity(err.Error()) && canWaitAtCapacity(ctx) && again(n); n++ {
		if sleepCtx(ctx, capacityWait) != nil {
			break
		}
		res, err = drv.Prompt(ctx, text)
	}
	return res, err
}

// harnessContext is ctx bounded by the run's timeout. The timeout runs from
// the harness's first start, so a harness started again after a capacity
// answer has what is left of it, not a timeout of its own.
func (rd *runDirs) harnessContext(ctx context.Context, p Params) (context.Context, context.CancelFunc) {
	if p.Budgets.TimeoutSeconds <= 0 {
		return context.WithCancel(ctx)
	}
	if rd.harnessEnds.IsZero() {
		rd.harnessEnds = time.Now().Add(time.Duration(p.Budgets.TimeoutSeconds) * time.Second)
	}
	return context.WithDeadline(ctx, rd.harnessEnds)
}

// harnessReceipt opens one of the harness's receipts: made new for its
// first start, so a reused output folder's earlier run never stands for
// this one, and added to for a start after a capacity answer, so the
// receipts keep every attempt.
func (rd *runDirs) harnessReceipt(path string) (*os.File, error) {
	if rd.harnessStarts == 0 {
		return os.Create(path) //nolint:gosec // our receipts dir
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644) //nolint:gosec // our receipts dir
}

// retryAtCapacity starts an autonomous harness with run and starts it again
// after each capacity answer it can wait out, while it has called no tool.
// Each start's collector begins from the usage of the starts before it, so
// the run's usage, its live meter and its cost budget count every attempt.
func retryAtCapacity(ctx context.Context, p Params, rd *runDirs, run func() agentOutcome) agentOutcome {
	out := run()
	for n := 1; n <= capacityTries && out.atCapacity && out.usage.Turns == 0 && rd.canRestartAtCapacity(ctx); n++ {
		noteCapacity(p.Stderr, n)
		if sleepCtx(ctx, capacityWait) != nil {
			break
		}
		rd.harnessUsage = out.usage
		out = run()
	}
	return out
}

// canRestartAtCapacity is canWaitAtCapacity for a harness started again:
// ctx carries no timeout of its own, the run's harness deadline does.
func (rd *runDirs) canRestartAtCapacity(ctx context.Context) bool {
	if !canWaitAtCapacity(ctx) {
		return false
	}
	return rd.harnessEnds.IsZero() || time.Until(rd.harnessEnds) > capacityWait
}

// exitedAtCapacity reports whether an autonomous harness that exited
// failing did so on a capacity answer: the error it reported says the model
// is at capacity. A run driven as one session turn has already waited in
// its session (oneTurn) and is never started again.
func exitedAtCapacity(p Params, reported string) bool {
	return p.Plan.Harness.OneTurn == nil && adapter.AtCapacity(reported)
}

// withBase adds the usage of the harness's earlier starts.
func withBase(u, base receipt.Usage) receipt.Usage {
	u.InputTokens += base.InputTokens
	u.OutputTokens += base.OutputTokens
	u.TotalTokens += base.TotalTokens
	u.CostUSD += base.CostUSD
	u.Turns += base.Turns
	return u
}
