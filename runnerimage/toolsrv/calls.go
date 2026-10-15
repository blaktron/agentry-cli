package toolsrv

// The calls ledger and end_blocked. A one-shot run of a release records every
// tool call's outcome, and the agent can end the run as blocked, so that a
// run whose agent could not do its task is not recorded as a success. The
// engine reads the ledger after the agent ends: an end_blocked call, every
// input call failing, every write_report failing, or no ledger at all
// (the tools could not write the run's output folder) ends the run with
// the blocked exit class.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
)

// ToolEndBlocked ends a one-shot run as blocked; granted only with the
// calls ledger.
const ToolEndBlocked = "end_blocked"

// MaxBlockedReason bounds the sentence end_blocked keeps, in bytes.
const MaxBlockedReason = 500

// Calls is the calls ledger's grant. The engine stamps Ledger into a
// release's one-shot run alone, never into the compiled plan (a session's
// plan is the same bytes), and end_blocked exists only then.
type Calls struct {
	// Ledger is the container path the ledger is appended to, under OutDir:
	// a start line when the server starts, then one line a tool call.
	Ledger string `json:"ledger,omitempty"`
}

// CallEntry is one line of the calls ledger. It names the tool and whether
// the call answered without an error, never its arguments: an input's name
// is the person's. Fault says a failed call failed on the run's own
// machinery (failed), which nothing the agent asks can bring about; only
// such a failure is the runner's evidence, so an agent cannot earn a
// refunded run by asking for a file that is not there. Blocked is the
// agent's own sentence from end_blocked.
type CallEntry struct {
	Start   bool   `json:"start,omitempty"`
	Tool    string `json:"tool,omitempty"`
	OK      bool   `json:"ok,omitempty"`
	Fault   bool   `json:"fault,omitempty"`
	Blocked string `json:"blocked,omitempty"`
}

const endBlockedDescription = "End this run as blocked. Call it once, when the task cannot be done with what this run has (the inputs could not be read, or a file or a capability the task needs is missing), with one sentence saying why. The run is then recorded as blocked, not as done. Write what you could before calling it, and stop after."

func endBlockedSchema() map[string]any {
	return objSchema(map[string]any{
		"reason": strProp("one sentence: what the task needed that this run did not have"),
	}, "reason")
}

func validateCalls(spec *Spec) error {
	l := spec.Calls.Ledger
	if l == "" {
		return nil
	}
	if !filepath.IsAbs(l) {
		return fmt.Errorf("calls.ledger %q must be an absolute container path", l)
	}
	if rel, err := filepath.Rel(spec.OutDir, l); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("calls.ledger %q must be under outDir %q", l, spec.OutDir)
	}
	return nil
}

// toolEndBlocked keeps the agent's sentence in the ledger, one line and
// bounded. If the ledger cannot take it, the agent is told to say so in its
// answer instead; the engine then finds the ledger short of what it needs.
func (s *server) toolEndBlocked(args json.RawMessage) (string, bool) {
	var a struct {
		Reason string `json:"reason"`
	}
	if err := unmarshalArgs(args, &a); err != nil {
		return "end_blocked: " + err.Error(), true
	}
	reason := oneLine(a.Reason, MaxBlockedReason)
	if reason == "" {
		return "end_blocked: reason is required: one sentence saying why the task cannot be done", true
	}
	if err := s.appendCall(CallEntry{Tool: ToolEndBlocked, OK: true, Blocked: reason}); err != nil {
		s.logf("calls ledger: %v", err)
		return "end_blocked: the run could not record it; say in your answer that the task could not be done, and why, then stop", true
	}
	return "Recorded: this run ends as blocked. Stop now; your last message is still the run's answer.", false
}

// recordCall appends one tool call's outcome; end_blocked records its own.
func (s *server) recordCall(tool string, ok bool) {
	fault := s.callFault && !ok
	s.callFault = false
	if tool == ToolEndBlocked {
		return
	}
	if err := s.appendCall(CallEntry{Tool: tool, OK: ok, Fault: fault}); err != nil {
		s.logf("calls ledger: %v", err)
	}
}

// failed is a tool's answer to an error from the filesystem, which marks
// the call a fault when the error is the run's machinery failing: a
// channel it may not open (an SELinux label), an I/O
// error, a read-only or full disk. A name that does not exist, a path
// that escapes its root or an argument out of range is the agent's own
// and no fault.
func (s *server) failed(tool string, err error) (string, bool) {
	if isFault(err) {
		s.callFault = true
	}
	return tool + ": " + err.Error(), true
}

func isFault(err error) bool {
	for _, target := range []error{fs.ErrPermission, syscall.EIO, syscall.EROFS, syscall.ENOSPC} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// startCalls writes the ledger's start line, so that a ledger the engine
// cannot find says the tools could not write the output folder at all.
func (s *server) startCalls() {
	if err := s.appendCall(CallEntry{Start: true}); err != nil {
		s.logf("calls ledger: %v", err)
	}
}

// appendCall writes one line, opening the ledger for each: calls are
// handled one at a time, and a file held open across the run would outlive
// a teardown that removed it.
func (s *server) appendCall(e CallEntry) error {
	l := s.spec.Calls.Ledger
	if l == "" {
		return nil
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(l, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // the spec's own ledger path
	if err != nil {
		return err
	}
	s.own(l)
	_, werr := f.Write(append(b, '\n'))
	return errors.Join(werr, f.Close())
}

// oneLine folds s's whitespace to single spaces and cuts it to max bytes on
// a character boundary.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(strings.ToValidUTF8(s, "�")), " ")
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
