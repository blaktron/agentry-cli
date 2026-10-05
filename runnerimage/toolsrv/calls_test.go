package toolsrv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// callsSpec is a spec with the calls ledger and the inputs channel.
func callsSpec(t *testing.T) *Spec {
	t.Helper()
	spec := baseSpec(t)
	spec.InputsDir = t.TempDir()
	spec.Calls.Ledger = filepath.Join(spec.OutDir, "receipts", "calls.ndjson")
	return spec
}

func readCalls(t *testing.T, file string) []CallEntry {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var out []CallEntry
	for _, ln := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e CallEntry
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatalf("ledger line %q: %v", ln, err)
		}
		out = append(out, e)
	}
	return out
}

func callLine(id int, tool string, args map[string]any) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	return string(b)
}

// end_blocked exists only beside the calls ledger, which the engine stamps
// into a release's one-shot run alone (agentry-cli#629).
func TestEndBlockedExistsOnlyWithTheLedger(t *testing.T) {
	spec := baseSpec(t)
	if slices.Contains(spec.GrantedTools(), ToolEndBlocked) {
		t.Fatal("end_blocked granted without the ledger")
	}
	spec = callsSpec(t)
	got := toolNames(t, runServe(t, spec, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)[0])
	if !slices.Contains(got, ToolEndBlocked) || got[len(got)-1] != ToolEndBlocked {
		t.Fatalf("tools/list %v", got)
	}
}

// The ledger has its start line, then one line a call: the tool and
// whether it answered without an error, never its arguments. end_blocked
// keeps the agent's sentence, folded to one line and bounded.
func TestTheLedgerRecordsEachCallsOutcome(t *testing.T) {
	spec := callsSpec(t)
	if err := os.WriteFile(filepath.Join(spec.InputsDir, "secret-plans.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	long := "The archive\nis   encrypted. " + strings.Repeat("é", MaxBlockedReason)
	resps := runServe(t, spec,
		callLine(1, ToolReadInput, map[string]any{"path": "secret-plans.txt"}),
		callLine(2, ToolReadInput, map[string]any{"path": "missing.txt"}),
		callLine(3, ToolWriteReport, map[string]any{"path": "r.md", "content": "x"}),
		callLine(4, ToolEndBlocked, map[string]any{"reason": "  "}),
		callLine(5, ToolEndBlocked, map[string]any{"reason": long}),
	)
	if len(resps) != 5 {
		t.Fatalf("%d responses", len(resps))
	}
	if r := resultOf(t, resps[3]); r["isError"] != true {
		t.Fatalf("an empty reason was taken: %v", r)
	}
	if r := resultOf(t, resps[4]); r["isError"] != false {
		t.Fatalf("end_blocked: %v", r)
	}
	got := readCalls(t, spec.Calls.Ledger)
	// A name that is not there is the agent's mistake, never a fault.
	if len(got) != 5 || !got[0].Start ||
		got[1] != (CallEntry{Tool: ToolReadInput, OK: true}) ||
		got[2] != (CallEntry{Tool: ToolReadInput}) ||
		got[3] != (CallEntry{Tool: ToolWriteReport, OK: true}) {
		t.Fatalf("ledger %+v", got)
	}
	b, _ := os.ReadFile(spec.Calls.Ledger)
	if strings.Contains(string(b), "secret-plans") || strings.Contains(string(b), "missing.txt") {
		t.Fatalf("the ledger names an input:\n%s", b)
	}
	blocked := got[4]
	if blocked.Tool != ToolEndBlocked || !blocked.OK || len(blocked.Blocked) > MaxBlockedReason ||
		!strings.HasPrefix(blocked.Blocked, "The archive is encrypted. é") || strings.ContainsRune(blocked.Blocked, '�') {
		t.Fatalf("blocked entry %+v", blocked)
	}
}

// A ledger outside the output folder is a spec the compiler never writes.
func TestTheLedgerStaysUnderTheOutputFolder(t *testing.T) {
	spec := callsSpec(t)
	spec.Calls.Ledger = filepath.Join(spec.WorkspaceDir, "calls.ndjson")
	if err := validateSpec(spec); err == nil {
		t.Fatal("a ledger outside outDir was accepted")
	}
	spec.Calls.Ledger = "receipts/calls.ndjson"
	if err := validateSpec(spec); err == nil {
		t.Fatal("a relative ledger was accepted")
	}
}

// When the ledger cannot be written, end_blocked tells the agent to say so
// in its answer instead (the engine then finds no start line).
func TestEndBlockedWithoutALedgerSaysSo(t *testing.T) {
	spec := callsSpec(t)
	if err := os.WriteFile(filepath.Join(spec.OutDir, "receipts"), []byte("not a folder"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, spec)
	text, isErr := s.toolEndBlocked(rawArgs(t, map[string]any{"reason": "no inputs"}))
	if !isErr || !strings.Contains(text, "say in your answer") {
		t.Fatalf("%v %q", isErr, text)
	}
}

// A fault is the run's machinery failing, which no argument brings about:
// a channel the tools may not open, an I/O error, a read-only or full disk.
func TestAFaultIsTheRunsMachinery(t *testing.T) {
	s := newTestServer(t, callsSpec(t))
	for _, tc := range []struct {
		err   error
		fault bool
	}{
		{&os.PathError{Op: "open", Path: "/inputs-text/inputs.json", Err: syscall.EACCES}, true},
		{&os.PathError{Op: "write", Path: "/out/reports/r.md", Err: syscall.ENOSPC}, true},
		{&os.PathError{Op: "open", Path: "/out/reports/r.md", Err: syscall.EROFS}, true},
		{&os.PathError{Op: "read", Path: "/inputs/a", Err: syscall.EIO}, true},
		{&os.PathError{Op: "stat", Path: "/inputs/nope", Err: syscall.ENOENT}, false},
		{&os.PathError{Op: "mkdir", Path: "/out/reports/a", Err: syscall.ENOTDIR}, false},
	} {
		s.callFault = false
		if text, isErr := s.failed("read_input", tc.err); !isErr || !strings.HasPrefix(text, "read_input: ") || s.callFault != tc.fault {
			t.Errorf("%v: fault %v, want %v (%q)", tc.err, s.callFault, tc.fault, text)
		}
	}
}

// Through the server: an input the tools may not read is a fault in the
// ledger; one that is not there is not.
func TestAnUnreadableChannelIsRecordedAsAFault(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	spec := callsSpec(t)
	locked := filepath.Join(spec.InputsDir, "locked.txt")
	if err := os.WriteFile(locked, []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	runServe(t, spec,
		callLine(1, ToolReadInput, map[string]any{"path": "locked.txt"}),
		callLine(2, ToolReadInput, map[string]any{"path": "missing.txt"}),
	)
	got := readCalls(t, spec.Calls.Ledger)
	if len(got) != 3 || got[1] != (CallEntry{Tool: ToolReadInput, Fault: true}) || got[2] != (CallEntry{Tool: ToolReadInput}) {
		t.Fatalf("ledger %+v", got)
	}
}
