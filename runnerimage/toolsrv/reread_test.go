package toolsrv

import (
	"os"
	"path/filepath"
	"testing"
)

// rereadServer is a server with each read tool's fixture written: notes.txt
// in the workspace, the scratch folder and the inputs, and notes.md in a
// knowledge pack; its turn count is in a file of the test's own.
func rereadServer(t *testing.T) (*server, *Spec) {
	t.Helper()
	spec := decodedSpec(t)
	spec.FSRead = Access{Enabled: true}
	spec.ScratchDir = t.TempDir()
	spec.KnowledgeDir = t.TempDir()
	spec.Knowledge = []KnowledgePack{{Name: "aws-docs", Digest: "sha256:aaaa"}}
	writePack(t, filepath.Join(spec.KnowledgeDir, "aws-docs"))
	text := []byte("the first version of the notes\n")
	mustWrite(t, filepath.Join(spec.WorkspaceDir, "notes.txt"), text)
	mustWrite(t, filepath.Join(spec.ScratchDir, "notes.txt"), text)
	mustWrite(t, filepath.Join(spec.InputsDir, "notes.txt"), text)
	mustWrite(t, filepath.Join(spec.KnowledgeDir, "aws-docs", "wiki", "notes.md"), text)
	writeIndex(t, spec, []InputEntry{{Path: "notes.txt", Type: "text", Bytes: int64(len(text)), TextBytes: int64(len(text))}})
	spec.TurnFile = filepath.Join(t.TempDir(), "turn")
	mustWrite(t, spec.TurnFile, []byte("1\n"))
	return newTestServer(t, spec), spec
}

var rereadCases = []struct{ tool, path string }{
	{ToolFSRead, "notes.txt"},
	{ToolScratchRead, "notes.txt"},
	{ToolReadInput, "notes.txt"},
	{ToolKnowledgeRead, "notes.md"},
}

// TestAnIdenticalRereadInATurnIsAnsweredUnchanged: the same page of the same
// bytes, asked again in the same turn, is the stub; fresh serves it again.
func TestAnIdenticalRereadInATurnIsAnsweredUnchanged(t *testing.T) {
	s, _ := rereadServer(t)
	for _, tc := range rereadCases {
		t.Run(tc.tool, func(t *testing.T) {
			args := map[string]any{"path": tc.path}
			first, isErr := s.callTool(s.tools[tc.tool], rawArgs(t, args))
			if isErr || first == RereadStub {
				t.Fatalf("first read = %q (error %v)", first, isErr)
			}
			if again, _ := s.callTool(s.tools[tc.tool], rawArgs(t, args)); again != RereadStub {
				t.Fatalf("an identical re-read served %q", again)
			}
			if fresh, _ := s.callTool(s.tools[tc.tool], rawArgs(t, map[string]any{"path": tc.path, "fresh": true})); fresh != first {
				t.Fatalf("fresh served %q, want the page", fresh)
			}
		})
	}
}

// TestAStubNeverStandsForChangedBytesOrAnotherPage: a file that changed, or
// another offset, is served whole.
func TestAStubNeverStandsForChangedBytesOrAnotherPage(t *testing.T) {
	s, spec := rereadServer(t)
	read := func(args map[string]any) string {
		got, _ := s.callTool(s.tools[ToolFSRead], rawArgs(t, args))
		return got
	}
	first := read(map[string]any{"path": "notes.txt"})
	if got := read(map[string]any{"path": "notes.txt", "offset": 4}); got == RereadStub {
		t.Fatal("another offset was answered unchanged")
	}
	mustWrite(t, filepath.Join(spec.WorkspaceDir, "notes.txt"), []byte("the second version of the notes\n"))
	if got := read(map[string]any{"path": "notes.txt"}); got == RereadStub || got == first {
		t.Fatalf("a changed file was served %q", got)
	}
}

// TestAStubNeverCrossesATurn: once the runner moves the turn count on (a new
// turn, a compaction), the same read is served again.
func TestAStubNeverCrossesATurn(t *testing.T) {
	s, spec := rereadServer(t)
	args := map[string]any{"path": "notes.txt"}
	first, _ := s.callTool(s.tools[ToolFSRead], rawArgs(t, args))
	if err := os.WriteFile(spec.TurnFile, []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.callTool(s.tools[ToolFSRead], rawArgs(t, args)); got != first {
		t.Fatalf("the next turn's read = %q, want the page", got)
	}
	if got, _ := s.callTool(s.tools[ToolFSRead], rawArgs(t, args)); got != RereadStub {
		t.Fatalf("the new turn's re-read = %q, want the stub", got)
	}
}

// TestTheRereadStubFitsEveryBudgetAndIsOffered: the stub is within every
// harness's result budget, and the four read tools, alone, take fresh.
func TestTheRereadStubFitsEveryBudgetAndIsOffered(t *testing.T) {
	for _, lim := range testBudgets {
		if b := lim.budget(); int64(len(RereadStub)) > b.bytes {
			t.Errorf("the stub is over the %v budget", b)
		}
	}
	s, _ := rereadServer(t)
	for name, d := range s.tools {
		props, _ := d.schema["properties"].(map[string]any)
		_, has := props["fresh"]
		if has != rereadTools[name] {
			t.Errorf("%s offers fresh: %v", name, has)
		}
	}
}

// TestAnUnreadableTurnCountServesEveryRead: with the turn count gone, the
// tools server cannot tell turns apart, so it serves every read whole.
func TestAnUnreadableTurnCountServesEveryRead(t *testing.T) {
	s, spec := rereadServer(t)
	args := map[string]any{"path": "notes.txt"}
	first, _ := s.callTool(s.tools[ToolFSRead], rawArgs(t, args))
	if err := os.Remove(spec.TurnFile); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if got, _ := s.callTool(s.tools[ToolFSRead], rawArgs(t, args)); got != first {
			t.Fatalf("a read with no turn count = %q, want the page", got)
		}
	}
}
