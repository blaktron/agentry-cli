package toolsrv

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"os"
	"strings"
)

// The re-read stub: a read tool asked again, within one turn, for the same
// page of a file whose bytes have not changed answers that it is unchanged
// rather than serving the page again. Inside a turn nothing leaves the
// history, so the earlier answer is still there for the agent to read; a
// new turn or a compaction (TurnFile) starts a fresh log, and fresh: true
// always serves the page.

// rereadTools are the read tools the stub answers for.
var rereadTools = map[string]bool{
	ToolFSRead:        true,
	ToolReadInput:     true,
	ToolKnowledgeRead: true,
	ToolScratchRead:   true,
}

// RereadStub is a re-read's answer.
const RereadStub = "unchanged since your earlier call in this turn; call again with fresh: true for the text"

// freshProp is the argument that always serves the page.
func freshProp() map[string]any {
	return boolProp("true to be served the page again even if this turn already read it unchanged (an identical re-read is otherwise answered \"unchanged\")")
}

// rereadLog is what a run's read tools served in the current turn.
type rereadLog struct {
	turn  string
	pages map[string][sha256.Size]byte
}

// reread answers a read tool's call whose answer is text: the stub, and
// true, when the same call served the same bytes earlier in this turn; text
// otherwise. The call is keyed by the tool and its arguments, fresh left
// out. A turn count that cannot be read serves the page and records
// nothing, so a stub never stands for a read of another turn.
func (s *server) reread(tool string, args json.RawMessage, text string) (string, bool) {
	if !rereadTools[tool] {
		return text, false
	}
	turn := ""
	if s.spec.TurnFile != "" {
		b, err := os.ReadFile(s.spec.TurnFile)
		if err != nil {
			s.reads = rereadLog{}
			return text, false
		}
		turn = strings.TrimSpace(string(b))
	}
	if s.reads.pages == nil || turn != s.reads.turn {
		s.reads = rereadLog{turn: turn, pages: map[string][sha256.Size]byte{}}
	}
	var a map[string]any
	if len(bytes.TrimSpace(args)) > 0 && json.Unmarshal(args, &a) != nil {
		return text, false
	}
	fresh, _ := a["fresh"].(bool)
	delete(a, "fresh")
	k, err := json.Marshal(a)
	if err != nil {
		return text, false
	}
	key := tool + " " + string(k)
	sum := sha256.Sum256([]byte(text))
	if prev, ok := s.reads.pages[key]; ok && prev == sum && !fresh {
		return RereadStub, true
	}
	s.reads.pages[key] = sum
	return text, false
}
