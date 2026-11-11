package toolsrv

import (
	"path/filepath"
	"strings"
	"testing"
)

func repeatServer(t *testing.T) *server {
	t.Helper()
	s, _ := rereadServer(t)
	return s
}

// beforeNotice is a result without any repeat notice after it.
func beforeNotice(text string) string {
	before, _, _ := strings.Cut(text, "\n\nNote: ")
	return before
}

func hasNotice(text string) bool { return strings.Contains(text, "\n\nNote: ") }

// TestTheThirdIdenticalCallCarriesTheNoticeAndTheSixthRepeatsIt: the second
// call has none, the notice says how many times and that nothing changed, and
// the tool's own answer comes first, unchanged.
func TestTheThirdIdenticalCallCarriesTheNoticeAndTheSixthRepeatsIt(t *testing.T) {
	s := repeatServer(t)
	args := map[string]any{"path": "notes.txt"}
	var got []string
	for i := 0; i < 7; i++ {
		text, isErr := s.callTool(s.tools[ToolScratchRead], rawArgs(t, args))
		if isErr {
			t.Fatalf("call %d failed: %s", i+1, text)
		}
		got = append(got, text)
	}
	for i, text := range got {
		want := i == 2 || i == 5
		if hasNotice(text) != want {
			t.Fatalf("call %d: notice = %v, want %v (%q)", i+1, hasNotice(text), want, text)
		}
	}
	// The result before the notice is what the call alone would answer: the
	// re-read stub from the second call on.
	if !strings.HasPrefix(got[2], RereadStub+"\n\nNote: ") || !strings.HasPrefix(got[5], RereadStub+"\n\nNote: ") {
		t.Fatalf("the tool's own answer was altered: %q / %q", got[2], got[5])
	}
	if !strings.Contains(got[2], "call 3") || !strings.Contains(got[2], "has not changed") || !strings.Contains(got[2], "read the checklist") {
		t.Fatalf("notice text: %q", got[2])
	}
	if !strings.Contains(got[5], "call 6") {
		t.Fatalf("sixth notice text: %q", got[5])
	}
}

// TestArgumentKeyOrderAndSpacingDoNotMakeACallDistinct: the same arguments
// written differently count as one call.
func TestArgumentKeyOrderAndSpacingDoNotMakeACallDistinct(t *testing.T) {
	s := repeatServer(t)
	forms := []string{
		`{"path":"notes.txt","offset":0}`,
		`{ "offset": 0, "path": "notes.txt" }`,
		`{"offset":0,"path":"notes.txt"}`,
	}
	var last string
	for _, f := range forms {
		last, _ = s.callTool(s.tools[ToolFSRead], []byte(f))
	}
	if !hasNotice(last) {
		t.Fatalf("third call, arguments written three ways: %q", last)
	}
}

// TestAChangedResultIsNotARepeat: a read whose text moved between calls
// starts its tally again.
func TestAChangedResultIsNotARepeat(t *testing.T) {
	s := repeatServer(t)
	path := filepath.Join(s.spec.ScratchDir, "notes.txt")
	args := rawArgs(t, map[string]any{"path": "notes.txt"})
	for i := 0; i < 6; i++ {
		mustWrite(t, path, []byte(strings.Repeat("x", i+1)+"\n"))
		text, _ := s.callTool(s.tools[ToolScratchRead], args)
		if hasNotice(text) {
			t.Fatalf("call %d, text changed each time, carries a notice: %q", i+1, text)
		}
	}
}

// TestTheThirdFailureWithTheSameErrorCarriesTheNotice: different arguments,
// the same error text, still a repeated failure.
func TestTheThirdFailureWithTheSameErrorCarriesTheNotice(t *testing.T) {
	s := repeatServer(t)
	for i := 0; i < 7; i++ {
		args := rawArgs(t, map[string]any{"path": "missing.txt", "n": i})
		text, isErr := s.callTool(s.tools[ToolFSRead], args)
		if !isErr {
			t.Fatalf("call %d should fail: %q", i+1, text)
		}
		want := i == 2 || i == 5
		if hasNotice(text) != want {
			t.Fatalf("failure %d: notice = %v, want %v (%q)", i+1, hasNotice(text), want, text)
		}
		if want && !strings.Contains(text, "failed") {
			t.Fatalf("failure notice text: %q", text)
		}
	}
}

// TestADifferentErrorIsNotTheSameFailure: failures with different error text
// are tallied apart.
func TestADifferentErrorIsNotTheSameFailure(t *testing.T) {
	s := repeatServer(t)
	for i := 0; i < 5; i++ {
		args := rawArgs(t, map[string]any{"path": "missing" + strings.Repeat("x", i) + ".txt"})
		if text, _ := s.callTool(s.tools[ToolFSRead], args); hasNotice(text) {
			t.Fatalf("call %d: %q", i+1, text)
		}
	}
}

// TestTheNoticeBlocksNothing: the call is run every time, and an error stays
// an error with its text first.
func TestTheNoticeBlocksNothing(t *testing.T) {
	s := repeatServer(t)
	args := rawArgs(t, map[string]any{"path": "missing.txt"})
	var first string
	for i := 0; i < 8; i++ {
		text, isErr := s.callTool(s.tools[ToolFSRead], args)
		if !isErr {
			t.Fatalf("call %d: error flag lost", i+1)
		}
		if i == 0 {
			first = text
		}
		if !strings.HasPrefix(text, first) {
			t.Fatalf("call %d: the tool's own error changed: %q", i+1, text)
		}
	}
	// A successful write still runs after its notice.
	mustWrite(t, filepath.Join(s.spec.ScratchDir, "again.txt"), []byte("a\n"))
	rd := rawArgs(t, map[string]any{"path": "again.txt"})
	for i := 0; i < 4; i++ {
		if text, isErr := s.callTool(s.tools[ToolScratchRead], rd); isErr || text == "" {
			t.Fatalf("read %d blocked: %q", i+1, text)
		}
	}
}
