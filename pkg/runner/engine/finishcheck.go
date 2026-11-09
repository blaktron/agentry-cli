package engine

// The finish check. A harness that answers without a tool call ends an
// autonomous run, whatever is left of its task. When the run's checklist
// still has steps pending or in progress at that ending, the engine starts
// the harness again on its stored conversation with one message naming
// them, at most finishChecks times; the ending after the last ask stands.
// The steps are read from the tools server's copy of the list in the run's
// receipts (ChecklistCopyFile), never from the model's answer.
//
// The check is made only where a start on the stored conversation is a
// resume (spec.HarnessSpec.Resume, a container harness, not one driven as a
// session turn), and only after an ending with no error: the harness ended
// on a text answer, so no tool call is in flight and none can act twice.
// A run whose agent called end_blocked, or whose runner found it blocked,
// is not asked. Its starts count in the run's turns, cost and time like a
// resume's, and none is begun with less than finishCheckMargin of the
// harness's time left.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// ChecklistCopyFile is the tools server's copy of the run's checklist under
// the run's receipts.
const ChecklistCopyFile = "checklist.md"

// finishChecks is the most times one run is asked about its open steps.
const finishChecks = 2

// finishCheckMargin is the least time a finish check needs: one start of the
// harness and one answer. A variable so the tests need not wait.
var finishCheckMargin = 2 * time.Minute

// maxChecklistCopy bounds the copy the engine reads.
const maxChecklistCopy = 256 << 10

// The run's endings as its checklist left them, for the receipt.
const (
	endingAllDone     = "all steps done"
	endingNoChecklist = "no checklist"
)

// finishCheckMessage is the user message a finish check starts the harness
// with: the open steps, and, on the last ask, that the answer must name any
// step it leaves.
func finishCheckMessage(open []toolsrv.Step, last bool) string {
	var b strings.Builder
	b.WriteString("Your checklist has open steps:\n")
	for _, st := range open {
		fmt.Fprintf(&b, "- %s (%s)\n", st.Text, strings.ReplaceAll(st.Status, "_", " "))
	}
	b.WriteString("Continue them, or drop each one with its reason and finish.")
	if last {
		b.WriteString(" This is the last time you are asked: name in your answer every step you leave open, and why.")
	}
	return b.String()
}

// grantChecklistCopy stamps the copy's container path into the run's
// rendered tools spec when the engine made the copy's file for it, as
// grantFetchLedger stamps its ledger.
func grantChecklistCopy(gatewayDir string, rd *runDirs) error {
	if rd.checklistCopy == "" {
		return nil
	}
	specPath := filepath.Join(gatewayDir, "toolspec.json")
	b, err := os.ReadFile(specPath) //nolint:gosec // our own rendered config
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var ts toolsrv.Spec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ts); err != nil {
		return fmt.Errorf("the rendered tools spec does not decode as the tools server reads it: %w", err)
	}
	if ts.Checklist == "" {
		return nil
	}
	ts.ChecklistCopy = path.Join(spec.OutMount, "receipts", ChecklistCopyFile)
	out, err := json.MarshalIndent(&ts, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(specPath, append(out, '\n'), 0o644) //nolint:gosec // read by the tools container
}

// keepsChecklist reports whether p's run keeps a checklist the runner reads:
// a release's one-shot run whose plan grants the checklist tool.
func keepsChecklist(p Params) bool {
	return p.EndBlocked && slices.Contains(p.Plan.Tools, toolsrv.ToolChecklist)
}

// readChecklistCopy reads the steps the tools server last copied. ok is
// false when there is no copy: the agent never saved or read a list. A copy
// that is not a regular file is read as none.
func readChecklistCopy(file string) (steps []toolsrv.Step, ok bool) {
	if file == "" {
		return nil, false
	}
	fi, err := os.Lstat(file)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return nil, false
	}
	f, err := os.Open(file) //nolint:gosec // the run's own receipt
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxChecklistCopy))
	if err != nil {
		return nil, false
	}
	return toolsrv.ParseChecklist(string(b)), true
}

// finishCheckable reports whether out, a start of an autonomous harness, may
// be followed by a finish check: it ended with no error, on a harness whose
// next start resumes its stored conversation, and the run is not blocked.
func finishCheckable(p Params, rd *runDirs, out agentOutcome) bool {
	if out.err != nil || rd.checklistCopy == "" {
		return false
	}
	h := p.Plan.Harness
	if h.Resume == nil || h.Host != nil || h.OneTurn != nil {
		return false
	}
	return blockedOf(rd.callsLedger) == nil
}

// openStepsAtFinish are the open steps of a run that may be asked, or nil.
func openStepsAtFinish(p Params, rd *runDirs, out agentOutcome) []toolsrv.Step {
	if !finishCheckable(p, rd, out) {
		return nil
	}
	steps, _ := readChecklistCopy(rd.checklistCopy)
	return toolsrv.OpenSteps(steps)
}

// noteFinishCheck tells an autonomous run's stderr of the nth finish check.
func noteFinishCheck(w io.Writer, open, n int) {
	if w != nil {
		_, _ = fmt.Fprintf(w, "note: the agent finished with %d checklist step(s) open; starting the harness again on its conversation to ask about them (%d of %d)\n", open, n, finishChecks)
	}
}

// checklistRecord is the envelope's record of the run's checklist at its
// end and the finish checks made, or nil for a run that keeps none.
func checklistRecord(rd *runDirs) *receipt.Checklist {
	if rd.checklistCopy == "" {
		return nil
	}
	rec := &receipt.Checklist{FinishChecks: rd.finishChecks}
	steps, ok := readChecklistCopy(rd.checklistCopy)
	if !ok {
		rec.Ending = endingNoChecklist
		return rec
	}
	rec.Steps = len(steps)
	open := toolsrv.OpenSteps(steps)
	if len(open) == 0 {
		rec.Ending = endingAllDone
		return rec
	}
	for _, st := range open {
		rec.Open = append(rec.Open, rd.redact.String(st.Text))
	}
	rec.Ending = fmt.Sprintf("finished with %d open step(s): %s", len(open), strings.Join(rec.Open, "; "))
	return rec
}

// makeChecklistCopy makes the copy's file, new and empty, for the tools
// server to write: whatever the output folder held under the name is
// removed first, so a link a person left there is not followed.
func makeChecklistCopy(rd *runDirs) error {
	rd.checklistCopy = filepath.Join(rd.receiptsDir, ChecklistCopyFile)
	_ = os.Remove(rd.checklistCopy)
	f, err := os.OpenFile(rd.checklistCopy, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644) //nolint:gosec // written by the tools container
	if err != nil {
		return fmt.Errorf("create the checklist's copy: %w", err)
	}
	return f.Close()
}

// finishCheckDue is restartHarness's test for a finish check: the open steps
// to ask about, when another ask is allowed and time remains for it.
func (rd *runDirs) finishCheckDue(ctx context.Context, p Params, out agentOutcome) []toolsrv.Step {
	if len(rd.finishChecks) >= finishChecks || !rd.canRestartAfter(ctx, finishCheckMargin) {
		return nil
	}
	return openStepsAtFinish(p, rd, out)
}
