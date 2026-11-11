package engine

// A harness that keeps its conversation in the run's session store is also
// started again on it when it ends early after its first tool call without
// failing the run:
//
//   - its last turn ended with no reply, or with reasoning only (an empty
//     final answer, which would otherwise end the run with no result);
//   - it exited failing on its loop detector, the same call repeating (a
//     harness build whose detector ends the run instead of messaging the
//     model).
//
// Each start carries a message that says why, so the model goes on from
// where it is instead of beginning again. Together with the finish check's
// starts they are at most finishChecks per run (rd.restarts), each recorded
// in the receipt with its reason. Before the run's first tool call nothing
// here applies: such a start is the capacity case, a fresh one.

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// emptyAnswerMessage is the user message a harness is started again with
// after an empty or reasoning-only final answer.
const emptyAnswerMessage = "Your last turn ended without a reply. Continue the task from where you are, or give your final answer now."

// loopDetectedMessage is the user message a harness is started again with
// after it exited on its loop detector.
const loopDetectedMessage = "The same call kept repeating; take a different step toward the goal, or finish with what you have, or end as blocked if it cannot be reached."

// restartWhat says, for the harness's stderr receipt and the run's stderr,
// what a restart reason means.
func restartWhat(reason string) string {
	switch reason {
	case receipt.RestartLoopDetected:
		return "a repeated call ended it"
	case receipt.RestartReasoningOnly:
		return "an answer of reasoning only"
	}
	return "an empty answer"
}

// exitedOnLoop reports whether an autonomous harness that exited failing did
// so on its loop detector: the error it reported on its event stream, or the
// last lines of its stderr, name it. A run driven as one session turn is
// never started again.
func exitedOnLoop(p Params, reported, stderrPath string) bool {
	if p.Plan.Harness.OneTurn != nil {
		return false
	}
	return namesLoop(reported) || namesLoop(lastLines(stderrPath, capacityStderrLines))
}

func namesLoop(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "loop_detected") || strings.Contains(s, "loop detected")
}

// earlyEndAsk is restartHarness's test for a start after an early end: the
// reason and message, or "" when out is none, the run's restarts are spent,
// the time left cannot hold one, or the agent ended blocked.
func (rd *runDirs) earlyEndAsk(ctx context.Context, p Params, out agentOutcome) (reason, message string) {
	switch {
	case out.emptyAnswer != "" && out.err == nil:
		reason, message = out.emptyAnswer, emptyAnswerMessage
	case out.loopExit:
		reason, message = receipt.RestartLoopDetected, loopDetectedMessage
	default:
		return "", ""
	}
	h := p.Plan.Harness
	if h.Resume == nil || h.Host != nil || h.OneTurn != nil || out.usage.Turns == 0 ||
		len(rd.restarts) >= finishChecks || !rd.canRestartAfter(ctx, finishCheckMargin) ||
		(rd.callsLedger != "" && blockedOf(rd.callsLedger) != nil) {
		return "", ""
	}
	return reason, message
}

// noteEarlyEnd tells an autonomous run's stderr of the nth restart after an
// early end.
func noteEarlyEnd(w io.Writer, reason string, n int) {
	if w != nil {
		_, _ = fmt.Fprintf(w, "note: the harness ended early (%s); starting it again on its conversation (%d of %d)\n", restartWhat(reason), n, finishChecks)
	}
}
