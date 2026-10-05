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
func grantCalls(gatewayDir string, on bool) error {
	if !on {
		return nil
	}
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

// ReadCallsLedger reads a run's calls ledger (receipts/calls.ndjson): its
// entries, how many lines did not decode, and whether it exists.
func ReadCallsLedger(file string) (entries []toolsrv.CallEntry, bad int, found bool, err error) {
	f, err := os.Open(file) //nolint:gosec // a run's own receipt
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var e toolsrv.CallEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			bad++
			continue
		}
		entries = append(entries, e)
	}
	return entries, bad, true, sc.Err()
}

// inputTools are the calls that read the run's inputs.
var inputTools = map[string]bool{
	toolsrv.ToolListInputs:   true,
	toolsrv.ToolReadInput:    true,
	toolsrv.ToolSearchInputs: true,
}

// blockedOf reads the calls ledger of a run whose agent ended without a
// failure of its own, and answers the blocked class when the run did not
// do its task; nil otherwise. The runner's evidence comes first, since a
// tool that failed the agent is why an agent says it is blocked:
//   - no ledger, or none the tools started: they could not write the run's
//     output folder, so no input read or report could have reached it;
//   - the agent called the input tools, and every call failed;
//   - the agent called write_report, and every call failed;
//   - and then the agent's own end_blocked, with its sentence.
func blockedOf(receiptsDir string) error {
	entries, _, found, err := ReadCallsLedger(filepath.Join(receiptsDir, CallsLedgerFile))
	if err != nil {
		return nil // a ledger the engine cannot read is no evidence either way
	}
	started := false
	var inputCalls, inputOK, reportCalls, reportOK int
	agent := ""
	for _, e := range entries {
		switch {
		case e.Start:
			started = true
		case e.Tool == toolsrv.ToolEndBlocked:
			if agent == "" {
				agent = e.Blocked
			}
		case inputTools[e.Tool]:
			inputCalls++
			if e.OK {
				inputOK++
			}
		case e.Tool == toolsrv.ToolWriteReport:
			reportCalls++
			if e.OK {
				reportOK++
			}
		}
	}
	runner := func(detail, why string) error {
		return Classify(ExitBlocked, receipt.WithReason(errors.New("blocked: "+why), receipt.ReasonRunnerBlocked, detail))
	}
	switch {
	case !found || !started:
		return runner(receipt.BlockedToolsUnrecorded, "the runner's tools could not write the run's output folder, so no input could be read and no report written")
	case inputCalls > 0 && inputOK == 0:
		return runner(receipt.BlockedInputsUnread, fmt.Sprintf("every one of the agent's %d call(s) to read the inputs failed", inputCalls))
	case reportCalls > 0 && reportOK == 0:
		return runner(receipt.BlockedReportUnwritten, fmt.Sprintf("every one of the agent's %d call(s) to write_report failed", reportCalls))
	case agent != "":
		return Classify(ExitBlocked, receipt.WithReason(errors.New("blocked: the agent could not do the task: "+agent), receipt.ReasonAgentBlocked, ""))
	}
	return nil
}
