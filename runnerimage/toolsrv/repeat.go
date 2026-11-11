package toolsrv

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// The repeat notice: across a whole run, the server counts each tool call by
// the tool and its arguments, and each failing call by the tool and its error
// text. The third identical call, and the third failure with the same error,
// have a notice appended to the tool's result, and the sixth repeats it. The
// notice never blocks a tool: the tool has run and its result comes first,
// unchanged. A harness's loop check sees only consecutive identical batches;
// this sees a repeat whatever came between.

// Notice counts at which a repeat is pointed out.
const (
	repeatFirstNotice  = 3
	repeatSecondNotice = 6
)

// repeatAdvice ends every notice.
const repeatAdvice = "Take a different step toward the goal, read the checklist, or finish."

// callCount is one distinct call's tally: how many times in a row it has
// answered with the same bytes, and a hash of those bytes. An answer that
// changed starts the tally again, so a read of text that moved is not a repeat.
type callCount struct {
	n    int
	hash [sha256.Size]byte
}

// repeatLog is a run's tallies.
type repeatLog struct {
	calls    map[string]*callCount
	failures map[string]int
}

// callKey identifies a call: the tool and its arguments as canonical JSON
// (object keys sorted, insignificant whitespace gone). Arguments that are
// not JSON are keyed by their trimmed bytes.
func callKey(tool string, args json.RawMessage) string {
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 {
		return tool + " {}"
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return tool + " " + string(trimmed)
	}
	canon, err := json.Marshal(v) // map keys marshal sorted
	if err != nil {
		return tool + " " + string(trimmed)
	}
	if string(canon) == "null" {
		return tool + " {}"
	}
	return tool + " " + string(canon)
}

// noteRepeat tallies one finished call and returns the notice that belongs
// after its result, or "" when none does. text is the tool's own answer, and
// failed says it is an error. A repeated call's notice wins over the repeated
// failure's, so a call never carries two.
func (s *server) noteRepeat(tool string, args json.RawMessage, text string, failed bool) string {
	// Reading the checklist again is how a run re-orients, and the notice
	// itself points at it, so the checklist is never counted.
	if tool == ToolChecklist {
		return ""
	}
	if s.repeats.calls == nil {
		s.repeats = repeatLog{calls: map[string]*callCount{}, failures: map[string]int{}}
	}
	sum := sha256.Sum256([]byte(text))
	key := callKey(tool, args)
	c := s.repeats.calls[key]
	if c == nil || c.hash != sum {
		c = &callCount{hash: sum}
		s.repeats.calls[key] = c
	}
	c.n++
	failures := 0
	if failed {
		fk := tool + "\x00" + text
		s.repeats.failures[fk]++
		failures = s.repeats.failures[fk]
	}
	switch {
	case isNoticeCount(c.n):
		return fmt.Sprintf("\n\nNote: this is call %d with these arguments, and the result has not changed. %s", c.n, repeatAdvice)
	case isNoticeCount(failures):
		return fmt.Sprintf("\n\nNote: %s has now failed %d times with this same error. %s", tool, failures, repeatAdvice)
	}
	return ""
}

func isNoticeCount(n int) bool { return n == repeatFirstNotice || n == repeatSecondNotice }
