package persistent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/runnerimage/applink"
)

// writeTranscript writes entries as the pump does.
func writeTranscript(t *testing.T, home string, entries ...Entry) {
	t.Helper()
	for _, e := range entries {
		if err := appendEntry(home, "assistant", e); err != nil {
			t.Fatal(err)
		}
	}
}

func turnAt(segment int, kind session.Kind, at time.Time, text string) Entry {
	return Entry{Segment: segment, Run: "r", Event: session.Event{Kind: kind, Time: at, Text: text}}
}

func recall(t *testing.T, sv *Supervisor, args map[string]any) applink.Answer {
	t.Helper()
	raw, _ := json.Marshal(args)
	return sv.answerAppLink(context.Background(), applink.Request{Tool: applink.Recall, Arguments: raw})
}

// TestRecallIsRedacted: recall answers from the transcript alone, which the
// engine wrote redacted, and masks a stored secret's value again, in case it
// was stored after the turn that said it; Agentry's upkeep turn and the
// handoff it was answered with, a notice turn and a tool call are never
// returned.
func TestRecallIsRedacted(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	if err := (func() error {
		c, err := LoadConfig(home, "assistant")
		if err != nil {
			return err
		}
		c.Secrets = []StoredSecret{{Name: "CHALET_CODE", Set: time.Now()}}
		return SaveConfig(home, "assistant", c)
	})(); err != nil {
		t.Fatal(err)
	}
	keys := fakeSecrets{SecretAccount("assistant", "CHALET_CODE"): "door-code-8841-chalet"}
	day := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	upkeep := turnAt(1, session.KindUserTurn, day.Add(3*time.Minute), "Upkeep: compact this conversation (memory-compact).")
	upkeep.Upkeep = true
	notice := turnAt(1, session.KindUserTurn, day.Add(5*time.Minute), "a file was added: chalet-map.pdf")
	notice.Synthetic = true
	writeTranscript(t, home,
		turnAt(1, session.KindUserTurn, day, "the chalet's door code is door-code-8841-chalet"),
		turnAt(1, session.KindToolCall, day.Add(time.Minute), "fs_write chalet"),
		turnAt(1, session.KindAssistantTurn, day.Add(2*time.Minute), "Noted the chalet's code."),
		upkeep,
		turnAt(1, session.KindAssistantTurn, day.Add(4*time.Minute), "owed: book the chalet\npages: chalet"),
		notice,
		turnAt(1, session.KindAssistantTurn, day.Add(6*time.Minute), "I see the chalet map."),
	)
	sv := &Supervisor{opts: Options{Home: home, Agent: assistantAgent(t), Secrets: keys}}
	a := recall(t, sv, map[string]any{"query": "chalet"})
	if a.IsError {
		t.Fatalf("recall: %s", a.Text)
	}
	if strings.Contains(a.Text, "door-code-8841-chalet") || !strings.Contains(a.Text, "the chalet's door code is [redacted]") {
		t.Errorf("a stored secret's value reached the answer: %s", a.Text)
	}
	for _, never := range []string{"owed:", "Upkeep:", "fs_write", "chalet-map.pdf", "I see the chalet map"} {
		if strings.Contains(a.Text, never) {
			t.Errorf("recall returned %q: %s", never, a.Text)
		}
	}
	if !strings.Contains(a.Text, "2 turn(s)") || !strings.Contains(a.Text, "the person, segment 1 turn 1") || !strings.Contains(a.Text, "you, segment 1 turn 1") {
		t.Errorf("recall's answer: %s", a.Text)
	}
}

// TestRecallSearchesEveryConversation: every word of the query must be in a
// turn, in any case; turns of every segment are searched, a task's turns
// with them, newest first, within the dates and the limit; a long turn is
// cut around the match, and the answer stays within its bound.
func TestRecallSearchesEveryConversation(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	d1 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.Local)
	d2 := time.Date(2026, 10, 9, 10, 0, 0, 0, time.Local)
	task := turnAt(2, session.KindUserTurn, d2.Add(time.Hour), "Task: car check\n\nLook for the Volvo dealer's reply.")
	task.Task = "t1"
	long := strings.Repeat("filler ", 300) + "the Volvo dealer offered 68,000 CAD" + strings.Repeat(" more", 300)
	writeTranscript(t, home,
		turnAt(1, session.KindUserTurn, d1, "Ask the Volvo dealer for a quote."),
		turnAt(1, session.KindAssistantTurn, d1.Add(time.Minute), "I'll ask the dealer."),
		turnAt(2, session.KindUserTurn, d2, "what did the dealer say?"),
		turnAt(2, session.KindAssistantTurn, d2.Add(time.Minute), long),
		task,
		turnAt(2, session.KindAssistantTurn, d2.Add(time.Hour+time.Minute), "No reply from the volvo dealer yet."),
	)
	sv := &Supervisor{opts: Options{Home: home, Agent: assistantAgent(t), Secrets: fakeSecrets{}}}

	a := recall(t, sv, map[string]any{"query": "VOLVO dealer"})
	if a.IsError || !strings.Contains(a.Text, "4 turn(s)") {
		t.Fatalf("recall: %s", a.Text)
	}
	newest := strings.Index(a.Text, "No reply from the volvo dealer")
	oldest := strings.Index(a.Text, "Ask the Volvo dealer")
	if newest < 0 || oldest < 0 || newest > oldest {
		t.Errorf("not newest first: %s", a.Text)
	}
	if !strings.Contains(a.Text, "task t1, segment 2 turn 2") || !strings.Contains(a.Text, "you (to the task), segment 2 turn 2") {
		t.Errorf("a task's turns are not named as such: %s", a.Text)
	}
	if !strings.Contains(a.Text, "offered 68,000 CAD") || strings.Contains(a.Text, strings.Repeat("filler ", 120)) {
		t.Errorf("a long turn is not cut around its match: %s", a.Text)
	}

	a = recall(t, sv, map[string]any{"query": "dealer", "from": "2026-10-05", "to": "2026-10-09", "limit": 2})
	if a.IsError || strings.Contains(a.Text, "Ask the Volvo dealer") || !strings.Contains(a.Text, "(2 more not shown") {
		t.Errorf("dates and limit: %s", a.Text)
	}
	if a := recall(t, sv, map[string]any{"query": "volvo submarine"}); a.IsError || !strings.Contains(a.Text, "Nothing in your earlier conversations") {
		t.Errorf("a query with a word no turn has: %s", a.Text)
	}

	var many []Entry
	for i := range 40 {
		many = append(many, turnAt(3, session.KindUserTurn, d2.Add(time.Duration(i)*time.Minute), strings.Repeat("budget ", 200)))
	}
	writeTranscript(t, home, many...)
	if a := recall(t, sv, map[string]any{"query": "budget", "limit": 20}); len(a.Text) > recallMaxAnswer || !strings.Contains(a.Text, "more not shown") {
		t.Errorf("the answer is %d bytes, bound %d: %.200s", len(a.Text), recallMaxAnswer, a.Text)
	}
}

// TestRecallRefusesWhatItDoesNotTake: a query is required, the limit is
// bounded, dates are dates, and no other argument is taken.
func TestRecallRefusesWhatItDoesNotTake(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	sv := &Supervisor{opts: Options{Home: home, Agent: assistantAgent(t), Secrets: fakeSecrets{}}}
	for _, args := range []map[string]any{
		{},
		{"query": "  "},
		{"query": "car", "limit": 21},
		{"query": "car", "from": "last week"},
		{"query": "car", "from": "2026-10-09", "to": "2026-10-01"},
		{"query": "car", "path": "/etc/passwd"},
	} {
		if a := recall(t, sv, args); !a.IsError {
			t.Errorf("recall(%v) = %q, want a refusal", args, a.Text)
		}
	}
	if a := recall(t, sv, map[string]any{"query": "car"}); a.IsError || !strings.Contains(a.Text, "Nothing") {
		t.Errorf("recall with no transcript yet: %+v", a)
	}
}

// TestTheMemoryFrameOpensANewTopic: an agent that keeps a memory is reminded
// to read it at a new conversation's first turn and at the person's first
// turn after half an hour with none, not at the turns between.
func TestTheMemoryFrameOpensANewTopic(t *testing.T) {
	home := shortHome(t)
	enableHeartbeat(t, home, false)
	c := &clock{t: time.Date(2026, 10, 21, 14, 3, 0, 0, time.UTC)}
	st := &starter{}
	sv := startTicking(t, home, st, c)
	send := func(text string) {
		t.Helper()
		if err := sv.Send(context.Background(), text); err != nil {
			t.Fatal(err)
		}
	}
	framed := func(i int) bool {
		t.Helper()
		sent := st.lastSegment().rawTurns()
		return strings.Contains(sent[i], memoryFrame)
	}
	send("hello")
	send("and the car?")
	c.set(c.now().Add(29 * time.Minute))
	send("one more thing")
	c.set(c.now().Add(31 * time.Minute))
	send("back again")
	if !framed(0) || framed(1) || framed(2) || !framed(3) {
		t.Errorf("the frame was sent at %v %v %v %v, want the first and the last", framed(0), framed(1), framed(2), framed(3))
	}

	for _, a := range Agents() {
		if a.Name != "assistant" && a.Builtin.KeepsMemory() {
			t.Errorf("%s keeps a memory and would be framed", a.Name)
		}
	}
}

// TestACompactedConversationOpensWithTheMemoryFrame: the new conversation a
// compaction starts opens with the handoff and the memory frame, once.
func TestACompactedConversationOpensWithTheMemoryFrame(t *testing.T) {
	for _, renew := range []bool{false, true} {
		home := shortHome(t)
		enable(t, home)
		st := &starter{answer: aHandoff, renew: renew}
		sv := startUpkeeper(t, home, st, nil)
		if err := sv.Send(context.Background(), "hello"); err != nil {
			t.Fatal(err)
		}
		if _, err := sv.Compact(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{"what is open?", "and then?"} {
			if err := sv.Send(context.Background(), text); err != nil {
				t.Fatal(err)
			}
		}
		raw := st.last.rawTurns()
		first, second := raw[len(raw)-2], raw[len(raw)-1]
		if !strings.Contains(first, "Agentry compacted your conversation") || strings.Count(first, memoryFrame) != 1 || strings.Contains(second, memoryFrame) {
			t.Errorf("in-segment %v: the turns after the compaction = %q", renew, raw[len(raw)-2:])
		}
	}
}
