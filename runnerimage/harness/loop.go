package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/blaktron/agentry-cli/runnerimage/harness/proto"
)

// loopWatch remembers the newest calls of a turn, to see the model making
// the same call, or the same short sequence of calls, over and over.
type loopWatch struct {
	calls []watched
}

type watched struct{ name, sig string }

// watchKeep is how many calls loopWatch keeps at least.
const watchKeep = 256

// add notes one call.
func (w *loopWatch) add(name, arguments string) {
	w.calls = append(w.calls, watched{name: name, sig: name + "\x00" + canonicalJSON(arguments)})
	// No check looks further back than the longest sequence at its
	// threshold; a generous bound keeps the memory small on a long turn.
	if len(w.calls) > 2*watchKeep {
		w.calls = append(w.calls[:0], w.calls[len(w.calls)-watchKeep:]...)
	}
}

// reset forgets every call, so the next streak is counted afresh.
func (w *loopWatch) reset() { w.calls = w.calls[:0] }

// repeating reports the streak the newest calls end, if they end one: the
// calls of one round of it, in order, and how many rounds in a row. A round
// is one call (the same call again and again) or, when cfg looks for
// sequences, two to cfg.MaxPeriod calls that are not all the same call.
func (w *loopWatch) repeating(cfg *proto.Loop) (round []string, times int) {
	if cfg == nil {
		return nil, 0
	}
	for period := 1; period <= max(cfg.MaxPeriod, 1); period++ {
		need := cfg.Repeats
		if period > 1 {
			need = cfg.CycleRepeats
		}
		n := period * need
		if need < 2 || len(w.calls) < n {
			continue
		}
		tail := w.calls[len(w.calls)-n:]
		same := true
		for i := period; i < n && same; i++ {
			same = tail[i].sig == tail[i-period].sig
		}
		if !same {
			continue
		}
		if period > 1 {
			// A sequence of one call repeated is the first case, with its
			// own threshold.
			distinct := false
			for i := 1; i < period && !distinct; i++ {
				distinct = tail[i].sig != tail[0].sig
			}
			if !distinct {
				continue
			}
		}
		for _, c := range tail[:period] {
			round = append(round, c.name)
		}
		return round, need
	}
	return nil, 0
}

// canonicalJSON is raw with its members in one order and no spacing, so the
// same arguments written two ways are the same; raw itself when it is not
// JSON.
func canonicalJSON(raw string) string {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return strings.TrimSpace(raw)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return strings.TrimSpace(raw)
	}
	return strings.TrimSpace(out.String())
}

// runtimeNote opens every message the harness writes to the model in its own
// name, so the model does not take it for the person's.
const runtimeNote = "Note from the agent runtime, not the user: "

// nudgeText is the message the model gets after a streak. It names the tools
// and the count and never the arguments: they are the model's own words, and
// repeating them here would give them the weight of an instruction.
func nudgeText(round []string, times, earlier int) string {
	var b strings.Builder
	b.WriteString(runtimeNote)
	if len(round) == 1 {
		fmt.Fprintf(&b, "you have made the same call to %s, with the same arguments, %d times in a row. ", round[0], times)
	} else {
		fmt.Fprintf(&b, "you have made the same %d calls (%s), with the same arguments, %d times in a row. ", len(round), joinNames(round), times)
	}
	b.WriteString("Repeating it will not get a different result. " +
		"Reach the same goal another way: use a different tool or different arguments, " +
		"or check your checklist or plan for what is left. " +
		"If the goal cannot be reached, say why, then move on to the next step or end the run: " +
		"finish with what is done, or, if you have a tool for ending a run as blocked (such as end_blocked), call it.")
	switch {
	case earlier == 1:
		b.WriteString(" You were told this once before in this conversation.")
	case earlier > 1:
		fmt.Fprintf(&b, " You were told this %d times before in this conversation.", earlier)
	}
	return b.String()
}

// joinNames lists names as prose: "a", "a and b", "a, b and c".
func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return "a tool"
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
