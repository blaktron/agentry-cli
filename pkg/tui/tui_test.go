package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
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
		"Task:   \n\nRemind.":   "A task ran on its own",
		"Task:  Spaced out  \n": `Task "Spaced out" ran on its own`,
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

// ready is a model that has been given a window of w by h.
func ready(opts Options, w, h int) model {
	m := newModel(newFakeSession(), opts)
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return next.(model)
}

// press types text into the input and presses Enter, and returns the model
// and the command it asked for.
func press(m model, text string) (model, tea.Cmd) {
	m.input.SetValue(text)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return next.(model), cmd
}

// quits says whether cmd quits the program.
func quits(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// last is the transcript's last n lines, joined.
func last(m model, n int) string {
	return strings.Join(m.lines[len(m.lines)-n:], "\n")
}

// askingSession is a session that asks the person questions.
type askingSession struct {
	*fakeSession
	answered []string
	err      error
}

func (a *askingSession) Answer(_ context.Context, id string, yes bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.answered = append(a.answered, fmt.Sprintf("%s:%v", id, yes))
	return a.err
}

// TestEscAndTheQuitCommandsLeaveTheChatAndOnlyFinishAsksForAResult: Esc, Ctrl-C,
// /quit and /exit leave without a result; /finish leaves asking for one.
func TestEscAndTheQuitCommandsLeaveTheChatAndOnlyFinishAsksForAResult(t *testing.T) {
	m := ready(testOpts(), 100, 30)
	for name, leave := range map[string]func() (tea.Model, tea.Cmd){
		"Esc":    func() (tea.Model, tea.Cmd) { return m.Update(tea.KeyMsg{Type: tea.KeyEsc}) },
		"Ctrl-C": func() (tea.Model, tea.Cmd) { return m.Update(tea.KeyMsg{Type: tea.KeyCtrlC}) },
		"/quit": func() (tea.Model, tea.Cmd) {
			m.input.SetValue("/quit")
			return m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		},
		"/exit": func() (tea.Model, tea.Cmd) {
			m.input.SetValue("/exit")
			return m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		},
	} {
		next, cmd := leave()
		got := next.(model)
		if !quits(cmd) || !got.quitting || got.outcome.Finish {
			t.Errorf("%s: quit=%v quitting=%v finish=%v", name, quits(cmd), got.quitting, got.outcome.Finish)
		}
		if got.View() != "" {
			t.Errorf("%s: the chat is still drawn after it was left: %q", name, got.View())
		}
	}
	got, cmd := press(m, "/finish")
	if !quits(cmd) || !got.quitting || !got.outcome.Finish {
		t.Errorf("/finish: quit=%v quitting=%v finish=%v", quits(cmd), got.quitting, got.outcome.Finish)
	}
}

// TestTheInputLineSendsOneTurnAtATime: Enter sends what was typed as a turn
// and clears the line, a blank line sends nothing, and while the agent
// answers another message is held back with a word on the status line.
func TestTheInputLineSendsOneTurnAtATime(t *testing.T) {
	fs := newFakeSession()
	m := newModel(fs, testOpts())
	m, cmd := press(m, "   ")
	if cmd != nil || m.busy || len(m.lines) != 2 {
		t.Fatalf("a blank line: cmd=%v busy=%v lines=%d", cmd != nil, m.busy, len(m.lines))
	}
	m, cmd = press(m, "hello")
	if cmd == nil || !m.busy || m.input.Value() != "" {
		t.Fatalf("a message: cmd=%v busy=%v input=%q", cmd != nil, m.busy, m.input.Value())
	}
	if msg := cmd(); msg != (turnDoneMsg{}) {
		t.Fatalf("the turn finished with %v", msg)
	}
	if fs.sent[0] != "hello" || len(fs.sent) != 1 {
		t.Fatalf("sent = %q", fs.sent)
	}
	status := m.statusLine()
	m, cmd = press(m, "while you answer")
	if cmd != nil || len(fs.sent) != 1 || m.input.Value() != "" {
		t.Fatalf("a message while busy: cmd=%v sent=%q input=%q", cmd != nil, fs.sent, m.input.Value())
	}
	if got := m.statusLine(); got == status || !strings.HasSuffix(got, " · thinking… · the agent is still answering — wait for its turn to end ") {
		t.Fatalf("status = %q", got)
	}

	// The turn's end frees the line and clears the word.
	next, _ := m.Update(turnDoneMsg{})
	m = next.(model)
	if m.busy {
		t.Fatal("the chat is busy after the turn ended")
	}
	m, cmd = press(m, "again")
	if cmd == nil || !m.busy || strings.Contains(m.statusLine(), "still answering") {
		t.Fatalf("the next message: cmd=%v busy=%v status=%q", cmd != nil, m.busy, m.statusLine())
	}

	// A failed turn is shown, one the session's end cut off is not.
	m = ready(testOpts(), 100, 30)
	for _, tc := range []struct {
		name string
		msg  tea.Msg
		want string
	}{
		{"a failed turn", turnDoneMsg{err: errors.New("boom")}, "error: boom"},
		{"a turn the end of the session cut off", turnDoneMsg{err: session.ErrEnded}, ""},
		{"a file that was not added", inputDoneMsg{path: "a.txt", err: errors.New("no such file")}, "could not add a.txt: no such file"},
		{"a file that was added", inputDoneMsg{path: "a.txt"}, ""},
		{"an answer that was not taken", answerDoneMsg{err: errors.New("too late")}, "the answer was not taken: too late"},
		{"an answer that was taken", answerDoneMsg{}, ""},
	} {
		before := len(m.lines)
		next, _ := m.Update(tc.msg)
		got := next.(model)
		switch {
		case tc.want == "" && len(got.lines) != before:
			t.Errorf("%s: said %q", tc.name, last(got, len(got.lines)-before))
		case tc.want != "" && (len(got.lines) != before+1 || got.lines[before] != tc.want):
			t.Errorf("%s: lines %q, want %q", tc.name, got.lines[before:], tc.want)
		}
	}
}

// TestTheSlashCommandsOfTheChat: /caps lists the enforced contract, /help
// says what each command does, /add hands the session its path, an unknown
// command is said so and never sent to the agent, and /yes and /no answer
// the question the runner asked last.
func TestTheSlashCommandsOfTheChat(t *testing.T) {
	fs := newFakeSession()
	m := ready(testOpts(), 100, 30)
	m.s = fs

	m, cmd := press(m, "/caps")
	if cmd != nil || last(m, 3) != "Enforced capabilities:\n  network.egress: none\n  filesystem.read: workspace" {
		t.Errorf("/caps: %q", last(m, 3))
	}
	m, cmd = press(m, "/help")
	if cmd != nil || last(m, 1) != "/add <path> — give the agent a file (read-only under /inputs); /yes, /no — answer what Agentry asks you for the agent; /finish — ask for the final result and end; /quit — end without a result; /caps — the enforced contract" {
		t.Errorf("/help: %q", last(m, 1))
	}
	m, cmd = press(m, "/add")
	if cmd != nil || last(m, 1) != "usage: /add <path>" {
		t.Errorf("/add: %q", last(m, 1))
	}
	m, cmd = press(m, "/add   notes and more.txt  ")
	if cmd == nil {
		t.Fatal("/add with a path asked for nothing")
	}
	if msg := cmd(); msg != (inputDoneMsg{path: "notes and more.txt"}) || len(fs.added) != 1 || fs.added[0] != "notes and more.txt" {
		t.Errorf("/add: %v, added %q", msg, fs.added)
	}
	m, cmd = press(m, "/nope with words")
	if cmd != nil || last(m, 1) != "unknown command /nope (try /help)" || m.busy || len(fs.sent) != 0 {
		t.Errorf("an unknown command: cmd=%v %q busy=%v sent=%q", cmd != nil, last(m, 1), m.busy, fs.sent)
	}

	// Nothing to answer.
	for _, c := range []string{"/yes", "/no"} {
		m, cmd = press(m, c)
		if cmd != nil || last(m, 1) != "no question is waiting for your answer" {
			t.Errorf("%s with no question: %q", c, last(m, 1))
		}
	}
	// A session that asks nothing is not asked to take an answer.
	m.apply(session.Event{Kind: session.KindQuestion, Question: "q-0", Text: "Allow?"})
	m, cmd = press(m, "/yes")
	if cmd != nil || last(m, 1) != "no question is waiting for your answer" {
		t.Errorf("/yes on a session that asks nothing: %q", last(m, 1))
	}

	asking := &askingSession{fakeSession: fs}
	m.s = asking
	m.apply(session.Event{Kind: session.KindQuestion, Question: "q-7", Text: "Install the tool?"})
	if last(m, 2) != "Agentry asks you: Install the tool?\nAnswer with /yes or /no." || m.question != "q-7" {
		t.Errorf("a question: %q (%q)", last(m, 2), m.question)
	}
	m, cmd = press(m, "/yes")
	if m.question != "" || cmd == nil {
		t.Fatalf("/yes: question=%q cmd=%v", m.question, cmd != nil)
	}
	if msg := cmd(); msg != (answerDoneMsg{}) || len(asking.answered) != 1 || asking.answered[0] != "q-7:true" {
		t.Errorf("/yes: %v, answered %q", msg, asking.answered)
	}
	m, cmd = press(m, "/yes") // answered: nothing is waiting
	if cmd != nil || last(m, 1) != "no question is waiting for your answer" {
		t.Errorf("a second /yes: %q", last(m, 1))
	}
	m.apply(session.Event{Kind: session.KindQuestion, Question: "q-8", Text: "Go on?"})
	m, cmd = press(m, "/no")
	if msg := cmd(); msg != (answerDoneMsg{}) || len(asking.answered) != 2 || asking.answered[1] != "q-8:false" {
		t.Errorf("/no: %v, answered %q", msg, asking.answered)
	}
}

// TestTheTranscriptShowsWhatTheSessionDoes: each event of the session is drawn
// as its line, what streams is shown as the agent's line when the turn ends or
// the person speaks, and only the person's own turns are counted.
func TestTheTranscriptShowsWhatTheSessionDoes(t *testing.T) {
	m := ready(testOpts(), 100, 30)
	// Styles that say their name, since a test's terminal draws no faint.
	m.styles.dim = lipgloss.NewStyle().SetString("faint|")
	apply := func(ev session.Event) string {
		before := len(m.lines)
		m.apply(ev)
		return strings.Join(m.lines[before:], "\n")
	}
	for _, tc := range []struct {
		name string
		ev   session.Event
		want string
	}{
		{"the person's turn", session.Event{Kind: session.KindUserTurn, Text: "hi"}, "You: hi"},
		{"a tool call", session.Event{Kind: session.KindToolCall, Text: "fs_list"}, "  · tool: fs_list"},
		{"a permission granted", session.Event{Kind: session.KindPermissionGranted, Text: "fs_read"}, "  · permission granted: fs_read"},
		{"a permission denied", session.Event{Kind: session.KindPermissionDenied, Text: "shell"}, "  · permission request denied: shell"},
		{"an input added", session.Event{Kind: session.KindInputAdded, Path: "/inputs/a.txt"}, "  · input added: /inputs/a.txt"},
		{"a notice", session.Event{Kind: session.KindNotice, Text: "restarting"}, "Runner: restarting"},
		{"an error", session.Event{Kind: session.KindError, Text: "rate limited"}, "harness error: rate limited"},
		{"a usage event", session.Event{Kind: session.KindUsage, CostUSD: 0.5}, ""},
		{"a lower cost", session.Event{Kind: session.KindUsage, CostUSD: 0.2}, ""},
		{"an answer", session.Event{Kind: session.KindAssistantTurn, Text: "  padded\n"}, "Agent: padded"},
		{"a turn the runner composed", session.Event{Kind: session.KindUserTurn, Synthetic: true, Task: "t1", Text: "a file was added"}, "Runner: a file was added"},
		{"a tool call in it", session.Event{Kind: session.KindToolCall, Text: "fs_read"}, "  · tool: fs_read"},
		{"its answer", session.Event{Kind: session.KindAssistantTurn, Text: "Read it."}, "Agent: Read it."},
	} {
		if got := apply(tc.ev); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	if m.userTurns != 1 || m.toolCalls != 2 || m.costUSD != 0.5 {
		t.Errorf("counted turns=%d tools=%d cost=%v, want 1, 2, 0.5", m.userTurns, m.toolCalls, m.costUSD)
	}
	// A turn the runner composed is not drawn faint, as a task's or an upkeep's is.
	for _, l := range m.lines[len(m.lines)-4:] {
		if strings.Contains(l, "faint|") {
			t.Errorf("the runner's input is drawn as its own turn: %q", l)
		}
	}

	// What streams is the agent's line once the turn ends, or the person
	// speaks, or the session ends, and not twice.
	for name, ev := range map[string]session.Event{
		"the turn ends":     {Kind: session.KindAssistantTurn, Text: "partial words"},
		"the person speaks": {Kind: session.KindUserTurn, Text: "next"},
		"the session ends":  {Kind: session.KindSessionEnd, Reason: session.EndQuit},
	} {
		n := m
		n.streaming.Reset()
		n.apply(session.Event{Kind: session.KindAssistantText, Text: "partial "})
		n.apply(session.Event{Kind: session.KindAssistantText, Text: "words"})
		if n.streaming.String() != "partial words" {
			t.Fatalf("%s: streaming %q", name, n.streaming.String())
		}
		before := len(n.lines)
		n.apply(ev)
		got := n.lines[before:]
		if got[0] != "Agent: partial words" || n.streaming.Len() != 0 {
			t.Errorf("%s: lines %q streaming %q", name, got, n.streaming.String())
		}
		if name != "the turn ends" && len(got) != 2 {
			t.Errorf("%s: lines %q", name, got)
		}
		if name == "the turn ends" && len(got) != 1 {
			t.Errorf("%s: lines %q", name, got)
		}
	}

	// The end of the session says why and ends the chat.
	end := newModel(newFakeSession(), testOpts())
	end.apply(session.Event{Kind: session.KindSessionEnd, Reason: session.EndWall})
	if last(end, 1) != "Session ended: wall_timeout" || !end.ended || end.outcome.Reason != session.EndWall || end.outcome.Finish {
		t.Errorf("the end: %q ended=%v outcome=%+v", last(end, 1), end.ended, end.outcome)
	}

	// An upkeep's reply is not streamed, and a reply of several lines is cut
	// to its first.
	up := newModel(newFakeSession(), testOpts())
	up.apply(session.Event{Kind: session.KindUserTurn, Upkeep: true, Text: "Upkeep: tidy (memory-consolidate)."})
	up.apply(session.Event{Kind: session.KindAssistantText, Text: "tidying"})
	if up.streaming.Len() != 0 {
		t.Errorf("an upkeep's reply was streamed: %q", up.streaming.String())
	}
	before := len(up.lines)
	up.apply(session.Event{Kind: session.KindAssistantTurn, Text: "done\nand the details"})
	up.apply(session.Event{Kind: session.KindAssistantTurn, Text: "just one line"})
	if got := strings.Join(up.lines[before:], "|"); !strings.HasSuffix(strings.Split(got, "|")[0], "Agent: done …") || !strings.HasSuffix(strings.Split(got, "|")[1], "Agent: just one line") {
		t.Errorf("an upkeep's replies: %q", got)
	}
}

// TestTheChatOpensWithTheAgentsOpeningLineAndTheCommands: the manifest's
// opening line is shown when the session starts without a task, and the
// commands are listed under it.
func TestTheChatOpensWithTheAgentsOpeningLineAndTheCommands(t *testing.T) {
	commands := "Commands: /add <path>  /finish  /quit  /caps  /help"
	for name, tc := range map[string]struct {
		opts Options
		want []string
	}{
		"an opening line":            {Options{Opening: "What should I look at?"}, []string{"Agent: What should I look at?", commands}},
		"an opening line and a task": {Options{Opening: "What should I look at?", HasTask: true}, []string{commands}},
		"a blank opening line":       {Options{Opening: "  \n"}, []string{commands}},
		"no opening line":            {Options{}, []string{commands}},
	} {
		m := newModel(newFakeSession(), tc.opts)
		if strings.Join(m.lines, "\n") != strings.Join(tc.want, "\n") {
			t.Errorf("%s: %q, want %q", name, m.lines, tc.want)
		}
	}
}

// TestTheStatusLineShowsTheRunsBudgets: the status line names the run and
// counts the turns, the tools and the cost against the caps the run has, and
// says when the agent is answering, when the line is held, and how long the
// session may sit idle.
func TestTheStatusLineShowsTheRunsBudgets(t *testing.T) {
	opts := testOpts()
	opts.MaxCostUSD = 2.5
	m := newModel(newFakeSession(), opts)
	m.userTurns, m.toolCalls, m.costUSD = 3, 7, 0.13
	m.lastTurn = time.Now()
	if got, want := m.statusLine(), " demo/chatty@1.0.0 · interactive · turns 3/10 · tools 7/60 · $0.13/$2.50 · idle ends in 15m0s "; got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
	m.lastTurn = time.Now().Add(-time.Hour)
	if got, want := m.statusLine(), " demo/chatty@1.0.0 · interactive · turns 3/10 · tools 7/60 · $0.13/$2.50 · idle ends in 0s "; got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
	m.opts.PlanRun = true
	m.busy = true
	m.notice = "hold on"
	if got, want := m.statusLine(), " demo/chatty@1.0.0 · interactive · turns 3/10 · tools 7/60 · paid by your plan · idle ends in 0s · thinking… · hold on "; got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
	bare := newModel(newFakeSession(), Options{Ref: "a/b@1"})
	bare.userTurns, bare.toolCalls, bare.costUSD = 1, 2, 0.5
	if got, want := bare.statusLine(), " a/b@1 · interactive · turns 1 · tools 2 · $0.50 "; got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
}

// TestTheChatIsDrawnToTheWindowAndStopsWhenItEnds: the transcript has the
// window less the input, the status and a spacer, and never fewer than three
// lines; before it has a size the view says it is starting, and once the chat
// is left or the session is over nothing is drawn. A session event keeps the
// chat reading the next, the end of the session or of its events ends it.
func TestTheChatIsDrawnToTheWindowAndStopsWhenItEnds(t *testing.T) {
	fresh := newModel(newFakeSession(), testOpts())
	if got := fresh.View(); got != "starting…" {
		t.Errorf("before a size: %q", got)
	}
	m := ready(testOpts(), 100, 30)
	if m.vp.Width != 100 || m.vp.Height != 27 || m.input.Width != 96 {
		t.Errorf("a 100 by 30 window: viewport %d by %d, input %d", m.vp.Width, m.vp.Height, m.input.Width)
	}
	if v := m.View(); !strings.Contains(v, "> ") || !strings.Contains(v, " demo/chatty@1.0.0 · interactive") {
		t.Errorf("view: %q", v)
	}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	if got := next.(model); got.vp.Width != 80 || got.vp.Height != 17 || got.input.Width != 76 {
		t.Errorf("a resize to 80 by 20: viewport %d by %d, input %d", got.vp.Width, got.vp.Height, got.input.Width)
	}
	if small := ready(testOpts(), 60, 4); small.vp.Height != 3 {
		t.Errorf("a window 4 lines high: viewport %d", small.vp.Height)
	}

	// A session event is drawn and the next is waited for; the end of the
	// session quits.
	next, cmd := m.Update(sessionEventMsg(session.Event{Kind: session.KindNotice, Text: "hi"}))
	if cmd == nil || quits(nil) || next.(model).ended || last(next.(model), 1) != "Runner: hi" {
		t.Errorf("an event: cmd=%v ended=%v %q", cmd != nil, next.(model).ended, last(next.(model), 1))
	}
	next, cmd = m.Update(sessionEventMsg(session.Event{Kind: session.KindSessionEnd, Reason: session.EndQuit}))
	if !quits(cmd) || !next.(model).ended || next.(model).View() != "" {
		t.Errorf("the end of the session: quit=%v ended=%v view=%q", quits(cmd), next.(model).ended, next.(model).View())
	}
	next, cmd = m.Update(eventsClosedMsg{})
	if !quits(cmd) || !next.(model).ended || next.(model).outcome.Finish {
		t.Errorf("the end of the events: quit=%v ended=%v", quits(cmd), next.(model).ended)
	}
}

// TestRunReturnsHowTheChatWasLeft: Run shows the chat until the person leaves
// it or the session ends, and returns the outcome; a context that ends it is
// no error.
func TestRunReturnsHowTheChatWasLeft(t *testing.T) {
	t.Run("the session ends", func(t *testing.T) {
		fs := newFakeSession()
		fs.events <- session.Event{Kind: session.KindSessionEnd, Reason: session.EndIdle}
		in, _ := io.Pipe()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out, err := Run(ctx, fs, testOpts(), in, io.Discard)
		if err != nil || out.Reason != session.EndIdle || out.Finish {
			t.Errorf("Run = %+v, %v", out, err)
		}
	})
	t.Run("the person finishes", func(t *testing.T) {
		in, w := io.Pipe()
		go func() { _, _ = io.WriteString(w, "/finish\r") }()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out, err := Run(ctx, newFakeSession(), testOpts(), in, io.Discard)
		if err != nil || !out.Finish || out.Reason != "" {
			t.Errorf("Run = %+v, %v", out, err)
		}
	})
	t.Run("the program cannot read its input", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("a closed file is read differently on Windows")
		}
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
		_ = r.Close()
		out, err := Run(context.Background(), newFakeSession(), testOpts(), r, io.Discard)
		if err == nil || out != (Outcome{}) {
			t.Errorf("Run = %+v, %v", out, err)
		}
	})
	t.Run("the context ends", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		in, _ := io.Pipe()
		out, err := Run(ctx, newFakeSession(), testOpts(), in, io.Discard)
		if err != nil || out != (Outcome{}) {
			t.Errorf("Run = %+v, %v", out, err)
		}
	})
}
