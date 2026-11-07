package engine

// A harness that keeps its conversation in the run's session store
// (spec.HarnessSpec.Resume) is resumed after its first tool call when it ends
// on a passing model failure (adapter.TransientModelFailure): started again on
// the same conversation with resumeMessage instead of the task, so the start
// itself never runs a tool again (restartHarness, capacity.go). A tool call
// whose result was never stored reaches the model as one that returned none;
// resumeMessage asks the model to check before it calls such a tool again,
// which is all that stands between a call in flight at the failure and its
// running twice.

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// resumeWaits are the waits before each resume of one run, and so how many
// there can be; a variable so the tests need not wait.
var resumeWaits = []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 120 * time.Second, 120 * time.Second}

// resumeMessage is the user message a resumed harness is started with, in
// place of the task its conversation already holds.
const resumeMessage = "The run stopped on a failure of the model server and has been started again on this conversation. " +
	"Carry on with the task from where you stopped; if you keep notes (" + compile.NotesFile + " in your scratch folder), read them first. " +
	"A tool call that shows no result may or may not have acted: check before you call it again."

// resumable reports whether out, a start that ended failing, is resumed: the
// harness keeps its conversation in the run's store, runs in a container,
// called a tool before it ended (a start that called none is the capacity
// case, a fresh one), and ended on a passing model failure.
func resumable(p Params, out agentOutcome) bool {
	return p.Plan.Harness.Resume != nil && p.Plan.Harness.Host == nil && out.transient != "" && out.usage.Turns > 0
}

// noteResume tells an autonomous run's stderr of the nth resume.
func noteResume(w io.Writer, reason string, wait time.Duration, n int) {
	if w != nil {
		_, _ = fmt.Fprintf(w, "note: %s; starting the harness again on its conversation in %s (%d of %d)\n", reason, wait, n, len(resumeWaits))
	}
}

// conversationID is the conversation the harness's next start keeps.
func conversationID(rd *runDirs) string { return fmt.Sprintf("run-%d", rd.conversation) }

// conversationArgv is argv with the conversation's id in place of
// spec.PHConversation.
func conversationArgv(argv []string, rd *runDirs) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = strings.ReplaceAll(a, spec.PHConversation, conversationID(rd))
	}
	return out
}

// exitedOnPassingFailure names the passing model failure an autonomous
// harness that exited failing ended on, read where exitedAtCapacity reads
// its capacity answer; empty for any other end, and for a run driven as one
// session turn, which is never started again.
func exitedOnPassingFailure(p Params, reported, stderrPath string) string {
	if p.Plan.Harness.OneTurn != nil {
		return ""
	}
	if reason, ok := adapter.TransientModelFailure(reported); ok {
		return reason
	}
	reason, _ := adapter.TransientModelFailure(lastLines(stderrPath, capacityStderrLines))
	return reason
}
