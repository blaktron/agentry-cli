package tui

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// fakeSession answers every turn with a tool call and an echo, and records
// what the chat asked of it.
type fakeSession struct {
	mu     sync.Mutex
	events chan session.Event
	sent   []string
	added  []string
	ended  bool
	reason session.EndReason
}

func newFakeSession() *fakeSession { return &fakeSession{events: make(chan session.Event, 64)} }

func (f *fakeSession) Events() <-chan session.Event { return f.events }
func (f *fakeSession) Send(_ context.Context, text string) error {
	f.mu.Lock()
	f.sent = append(f.sent, text)
	f.mu.Unlock()
	f.events <- session.Event{Kind: session.KindUserTurn, Text: text}
	f.events <- session.Event{Kind: session.KindToolCall, Text: "fs_list"}
	f.events <- session.Event{Kind: session.KindAssistantText, Text: "echo: "}
	f.events <- session.Event{Kind: session.KindAssistantText, Text: text}
	f.events <- session.Event{Kind: session.KindAssistantTurn, Text: "echo: " + text, StopReason: "end_turn"}
	return nil
}
func (f *fakeSession) AddInput(_ context.Context, p string) error {
	f.mu.Lock()
	f.added = append(f.added, p)
	f.mu.Unlock()
	f.events <- session.Event{Kind: session.KindInputAdded, Path: "/inputs/" + p}
	return nil
}
func (f *fakeSession) End(context.Context, bool) (*receipt.Envelope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended = true
	return &receipt.Envelope{Status: "ok"}, nil
}
func (f *fakeSession) Ended() (bool, session.EndReason) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ended, f.reason
}

func testOpts() Options {
	return Options{Ref: "demo/chatty@1.0.0", Model: "anthropic/x", Caps: []string{"network.egress: none", "filesystem.read: workspace"},
		Budgets: spec.SessionBudgets{IdleTimeoutSeconds: 900, MaxUserTurns: 10}, MaxTurns: 60, Opening: "What should I look at?"}
}

func waitFor(t *testing.T, tm *teatest.TestModel, want string) {
	t.Helper()
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return bytes.Contains(b, []byte(want)) },
		teatest.WithCheckInterval(20*time.Millisecond), teatest.WithDuration(5*time.Second))
}

func TestChatTurnsCommandsAndFinish(t *testing.T) {
	fs := newFakeSession()
	tm := teatest.NewTestModel(t, newModel(fs, testOpts()), teatest.WithInitialTermSize(100, 30))
	waitFor(t, tm, "What should I look at?")

	tm.Type("hello agent")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "echo: hello agent") // the last line of the turn; earlier lines are checked on the final model

	tm.Type("/caps")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "network.egress: none")

	tm.Type("/add notes.txt")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "input added: /inputs/notes.txt")

	tm.Type("/finish")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	final := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(model)
	if !final.outcome.Finish {
		t.Error("/finish did not set Finish")
	}
	transcript := strings.Join(final.lines, "\n")
	for _, want := range []string{"You: hello agent", "· tool: fs_list", "Agent: echo: hello agent", "network.egress: none", "input added: /inputs/notes.txt"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("transcript lacks %q:\n%s", want, transcript)
		}
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.sent) != 1 || fs.sent[0] != "hello agent" {
		t.Errorf("sent = %v", fs.sent)
	}
	if len(fs.added) != 1 || fs.added[0] != "notes.txt" {
		t.Errorf("added = %v", fs.added)
	}
	if fs.ended {
		t.Error("the chat must not call End itself")
	}
}

func TestChatEndsWhenTheSessionEnds(t *testing.T) {
	fs := newFakeSession()
	tm := teatest.NewTestModel(t, newModel(fs, testOpts()), teatest.WithInitialTermSize(100, 30))
	waitFor(t, tm, "Commands:")
	fs.events <- session.Event{Kind: session.KindSessionEnd, Reason: session.EndIdle}
	final := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(model)
	if final.outcome.Finish || final.outcome.Reason != session.EndIdle || !final.ended {
		t.Errorf("outcome = %+v ended=%v", final.outcome, final.ended)
	}
}

func TestChatQuitWithoutFinish(t *testing.T) {
	fs := newFakeSession()
	tm := teatest.NewTestModel(t, newModel(fs, testOpts()), teatest.WithInitialTermSize(100, 30))
	waitFor(t, tm, "Commands:")
	tm.Type("/quit")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	final := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(model)
	if final.outcome.Finish {
		t.Error("/quit must not finish")
	}
	if !strings.Contains(final.statusLine(), "demo/chatty@1.0.0") {
		t.Errorf("status = %q", final.statusLine())
	}
}

// TestATaskTurnIsOneLineNotThePersonsMessage: a turn a persistent agent's
// heartbeat sent is drawn as one line naming its task, without its
// instruction, and is not counted as the person's turn.
func TestATaskTurnIsOneLineNotThePersonsMessage(t *testing.T) {
	fs := newFakeSession()
	tm := teatest.NewTestModel(t, newModel(fs, testOpts()), teatest.WithInitialTermSize(100, 30))
	waitFor(t, tm, "Commands:")
	fs.events <- session.Event{Kind: session.KindUserTurn, Task: "t1", Text: "Task: Stretch\n\nRemind the person to stand up."}
	fs.events <- session.Event{Kind: session.KindAssistantTurn, Text: "Time to stand up."}
	waitFor(t, tm, "Agent: Time to stand up.")
	tm.Type("/quit")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	final := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(model)
	transcript := strings.Join(final.lines, "\n")
	if !strings.Contains(transcript, `Task "Stretch" ran on its own`) {
		t.Errorf("no task line:\n%s", transcript)
	}
	for _, not := range []string{"You:", "Remind the person"} {
		if strings.Contains(transcript, not) {
			t.Errorf("the task's turn shows %q:\n%s", not, transcript)
		}
	}
	if final.userTurns != 0 {
		t.Errorf("the task's turn counted as the person's: %d", final.userTurns)
	}
}

func TestTaskLine(t *testing.T) {
	for in, want := range map[string]string{
		"Task: Morning brief\n\nSummarise the share.":  `Task "Morning brief" ran on its own`,
		"(a file was added)\nTask: Check\n\nCheck in.": `Task "Check" ran on its own`,
		"no name here":          "A task ran on its own",
		"Task: \"quoted\"\n\nx": `Task "\"quoted\"" ran on its own`,
	} {
		if got := TaskLine(in); got != want {
			t.Errorf("TaskLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAnUpkeepTurnIsOneLineNotThePersonsMessage: a persistent agent's
// upkeep turn is drawn as one runner line, its answer shortened to a faint
// first line, and is not counted as the person's turn.
func TestAnUpkeepTurnIsOneLineNotThePersonsMessage(t *testing.T) {
	fs := newFakeSession()
	tm := teatest.NewTestModel(t, newModel(fs, testOpts()), teatest.WithInitialTermSize(100, 30))
	waitFor(t, tm, "Commands:")
	fs.events <- session.Event{Kind: session.KindUserTurn, Upkeep: true, Text: "Upkeep: compact this conversation (memory-compact)."}
	fs.events <- session.Event{Kind: session.KindToolCall, Text: "memory_write"}
	fs.events <- session.Event{Kind: session.KindAssistantText, Text: "owed: the brief\nctx: secret details"}
	fs.events <- session.Event{Kind: session.KindAssistantTurn, Text: "owed: the brief\nctx: secret details"}
	waitFor(t, tm, "Agent: owed: the brief …")
	tm.Type("hello")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "echo: hello")
	tm.Type("/quit")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	final := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(model)
	transcript := strings.Join(final.lines, "\n")
	for _, want := range []string{"Compacting: saving this conversation to memory first", "· tool: memory_write", "Agent: owed: the brief …", "You: hello", "Agent: echo: hello"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("transcript lacks %q:\n%s", want, transcript)
		}
	}
	for _, not := range []string{"You: Upkeep", "memory-compact", "secret details"} {
		if strings.Contains(transcript, not) {
			t.Errorf("the upkeep turn shows %q:\n%s", not, transcript)
		}
	}
	if final.userTurns != 1 {
		t.Errorf("userTurns = %d, want 1 (the person's alone)", final.userTurns)
	}
}

// TestTheAgentsOwnTurnIsFaint: the tool lines and the answer of a task or
// upkeep turn are drawn in the faint style; the person's turn's are not.
func TestTheAgentsOwnTurnIsFaint(t *testing.T) {
	m := newModel(newFakeSession(), testOpts())
	// Styles that say their name, since a test's terminal draws no faint.
	m.styles.dim = lipgloss.NewStyle().SetString("faint|")
	m.styles.tool = lipgloss.NewStyle().SetString("tool|")
	m.apply(session.Event{Kind: session.KindUserTurn, Task: "t1", Text: "Task: Stretch\n\nStand up."})
	m.apply(session.Event{Kind: session.KindToolCall, Text: "fs_list"})
	m.apply(session.Event{Kind: session.KindAssistantTurn, Text: "Time to stand up."})
	m.apply(session.Event{Kind: session.KindUserTurn, Text: "thanks"})
	m.apply(session.Event{Kind: session.KindToolCall, Text: "fs_read"})
	m.apply(session.Event{Kind: session.KindAssistantTurn, Text: "You're welcome."})
	want := []string{
		"faint|   · tool: fs_list",
		"faint| Agent: Time to stand up.",
		"You: thanks",
		"tool|   · tool: fs_read",
		"Agent: You're welcome.",
	}
	got := m.lines[len(m.lines)-len(want):]
	for i, w := range want {
		if !strings.HasSuffix(got[i], w) || (!strings.HasPrefix(w, "faint|") && strings.Contains(got[i], "faint|")) {
			t.Errorf("line %d = %q, want %q", i, got[i], w)
		}
	}
}

func TestUpkeepLine(t *testing.T) {
	for in, want := range map[string]string{
		"Upkeep: compact this conversation (memory-compact).":   "Compacting: saving this conversation to memory first",
		"Upkeep: compact this conversation.":                    "Compacting: writing a handoff",
		"Upkeep: consolidate your memory (memory-consolidate).": "Tidying its memory",
		"anything else": "Compacting: writing a handoff",
	} {
		if got := UpkeepLine(in); got != want {
			t.Errorf("UpkeepLine(%q) = %q, want %q", in, got, want)
		}
	}
}
