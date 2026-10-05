package engine

// A blocked run (agentry-cli#629, plan quickagents-that-run.md M9, D5): a
// one-shot run of a release whose agent could not do its task ends with
// the blocked exit class, not ok. The agent says so with end_blocked; the
// runner also says so on its own evidence, read from the tools server's
// calls ledger once the agent has ended. The evidence is a fault of the
// run's machinery, not of the agent, which a hosted run refunds (D5); an
// agent's own end_blocked is settled like a failed run.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// CallsLedgerFile is the calls ledger under the run's receipts.
const CallsLedgerFile = "calls.ndjson"

// grantCalls stamps the calls ledger into the run's rendered tools spec,
// which grants end_blocked, and rebuilds the tools server's allowlist line
// from it, as grantWeb does.
func grantCalls(gatewayDir string) error {
	specPath := filepath.Join(gatewayDir, "toolspec.json")
	b, err := os.ReadFile(specPath) //nolint:gosec // our own rendered config
	if err != nil {
		return err
	}
	var ts toolsrv.Spec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ts); err != nil {
		return fmt.Errorf("the rendered tools spec does not decode as the tools server reads it: %w", err)
	}
	ts.Calls.Ledger = path.Join(spec.OutMount, "receipts", CallsLedgerFile)
	out, err := json.MarshalIndent(&ts, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(specPath, append(out, '\n'), 0o644); err != nil { //nolint:gosec // read by the tools container
		return err
	}
	return setAllowlist(filepath.Join(gatewayDir, "tools.yaml"), ts.GrantedTools())
}

// ReadCallsLedger reads a run's calls ledger (receipts/calls.ndjson). A
// missing ledger is none; a line that does not decode is skipped, as no
// evidence either way.
func ReadCallsLedger(file string) ([]toolsrv.CallEntry, error) {
	f, err := os.Open(file) //nolint:gosec // a run's own receipt
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []toolsrv.CallEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e toolsrv.CallEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// inputTools are the calls that read the run's inputs.
var inputTools = map[string]bool{
	toolsrv.ToolListInputs:   true,
	toolsrv.ToolReadInput:    true,
	toolsrv.ToolSearchInputs: true,
}

// callTally is what blockedOf counts of one kind of call.
type callTally struct{ calls, ok, faults int }

func (t *callTally) add(e toolsrv.CallEntry) {
	t.calls++
	if e.OK {
		t.ok++
	} else if e.Fault {
		t.faults++
	}
}

// failedOnTheRun says every call failed and the run's machinery failed at
// least one: a channel the tools could not open or write. The agent's own
// mistakes (a name that is not there, a report too large) are not the
// runner's evidence, or asking for a missing file would earn a run that
// D5 refunds.
func (t callTally) failedOnTheRun() bool { return t.calls > 0 && t.ok == 0 && t.faults > 0 }

// blockedOf reads the calls ledger of a run whose agent ended without a
// failure of its own, and answers the blocked class when the run did not
// do its task; nil otherwise. The runner's evidence comes first, since a
// tool that failed the agent is why an agent says it is blocked:
//   - no start line: the tools could not write the ledger the engine made
//     for them, so no input read or report could have reached the run;
//   - every input call failed, and on the run's machinery;
//   - every write_report failed, and on the run's machinery;
//   - and then the agent's own end_blocked, with its sentence.
func blockedOf(ledger string) error {
	entries, err := ReadCallsLedger(ledger)
	if err != nil {
		return nil // a ledger the engine cannot read is no evidence either way
	}
	started := false
	var inputs, reports callTally
	agentSaid := ""
	for _, e := range entries {
		switch {
		case e.Start:
			started = true
		case e.Tool == toolsrv.ToolEndBlocked:
			if agentSaid == "" {
				agentSaid = e.Blocked
			}
		case inputTools[e.Tool]:
			inputs.add(e)
		case e.Tool == toolsrv.ToolWriteReport:
			reports.add(e)
		}
	}
	runnerBlocked := func(detail, why string) error {
		return Classify(ExitBlocked, receipt.WithReason(errors.New("blocked: "+why), receipt.ReasonRunnerBlocked, detail))
	}
	switch {
	case !started:
		return runnerBlocked(receipt.BlockedToolsUnrecorded, "the runner's tools could not write the run's output folder, so no input could be read and no report written")
	case inputs.failedOnTheRun():
		return runnerBlocked(receipt.BlockedInputsUnread, fmt.Sprintf("every one of the agent's %d call(s) to read the inputs failed, the run's own channel among them", inputs.calls))
	case reports.failedOnTheRun():
		return runnerBlocked(receipt.BlockedReportUnwritten, fmt.Sprintf("every one of the agent's %d call(s) to write_report failed, the run's own output folder among them", reports.calls))
	case agentSaid != "":
		return Classify(ExitBlocked, receipt.WithReason(errors.New("blocked: the agent could not do the task: "+agentSaid), receipt.ReasonAgentBlocked, ""))
	}
	return nil
}
