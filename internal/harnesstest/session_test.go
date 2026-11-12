package harnesstest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/internal/scriptedmodel"
	"github.com/blaktron/agentry-cli/runnerimage/harness"
	"github.com/blaktron/agentry-cli/runnerimage/harness/convo"
	"github.com/blaktron/agentry-cli/runnerimage/harness/proto"
)

// What a session needs of the harness: each test starts the serve mode,
// writes the runner's commands to it as a driver would, and reads its events
// as they come.

// session is a running serve mode.
type session struct {
	t     *testing.T
	stdin *io.PipeWriter
	// events are the events not yet read by the test; all is every event
	// read so far.
	events chan proto.Event
	all    []proto.Event
	exit   chan int

	mu     sync.Mutex
	stderr bytes.Buffer
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// serve starts the harness's session mode on the session folder dir.
func serve(t *testing.T, model *scriptedmodel.Server, g *gateway, dir string, configEdit func(map[string]any)) *session {
	t.Helper()
	cfg := map[string]any{
		"version": proto.Version,
		"model": map[string]any{
			"api": "anthropic", "name": "claude-test", "baseUrl": model.URL(),
			"keyEnv": "MODEL_KEY", "maxOutputTokens": 1024, "contextWindow": 200000,
		},
		"system":     "You are a careful agent.",
		"gateway":    map[string]any{"url": g.srv.URL + "/mcp", "tokenEnv": "MCP_GATEWAY_TOKEN"},
		"sessionDir": dir,
	}
	if configEdit != nil {
		configEdit(cfg)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "harness.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(name string) string {
		return map[string]string{"MODEL_KEY": modelKey, "MCP_GATEWAY_TOKEN": gatewayToken}[name]
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &session{t: t, stdin: inW, events: make(chan proto.Event, 1024), exit: make(chan int, 1)}
	go func() {
		code := harness.Serve(context.Background(), []string{"--config", path}, inR, outW, lockedWriter{&s.mu, &s.stderr}, env)
		_ = outW.Close()
		s.exit <- code
	}()
	go func() {
		defer close(s.events)
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
		for n := int64(1); sc.Scan(); n++ {
			var ev proto.Event
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				t.Errorf("stdout line is not an event: %q", sc.Text())
				return
			}
			if ev.V != proto.Version || ev.Seq != n {
				t.Errorf("event %d has v %d seq %d", n, ev.V, ev.Seq)
			}
			s.events <- ev
		}
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		select {
		case <-s.exit:
		case <-time.After(10 * time.Second):
			t.Error("the session did not end when its stdin did")
		}
	})
	return s
}

// send writes one command.
func (s *session) send(cmd proto.Command) {
	s.t.Helper()
	line, err := json.Marshal(cmd)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.stdin.Write(append(line, '\n')); err != nil {
		s.t.Fatalf("writing %s: %v", cmd.Type, err)
	}
}

// until reads events up to and including the next of type typ, and returns
// it with the ones read on the way.
func (s *session) until(typ string) (proto.Event, []proto.Event) {
	s.t.Helper()
	var passed []proto.Event
	timeout := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				s.mu.Lock()
				defer s.mu.Unlock()
				s.t.Fatalf("the session ended before a %s event; it wrote: %s\nstderr: %s", typ, types(s.all), s.stderr.String())
			}
			s.all = append(s.all, ev)
			passed = append(passed, ev)
			if ev.Type == typ {
				return ev, passed
			}
		case <-timeout:
			s.t.Fatalf("no %s event within 15s; the session wrote: %s", typ, types(s.all))
		}
	}
}

// open opens a conversation and returns its ready event.
func (s *session) open(id string) proto.Event {
	s.t.Helper()
	s.send(proto.Command{Type: proto.CmdOpen, Conversation: id})
	ev, _ := s.until(proto.TypeReady)
	return ev
}

// turn sends a prompt and answers what the harness asks, permitting every
// call and accepting the turn's end, until the turn ends. It returns the
// turn's events.
func (s *session) turn(text string) []proto.Event {
	s.t.Helper()
	s.send(proto.Command{Type: proto.CmdPrompt, Text: text})
	return s.answerUntilEnd()
}

func (s *session) answerUntilEnd() []proto.Event {
	s.t.Helper()
	var out []proto.Event
	timeout := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				s.t.Fatalf("the session ended inside a turn; it wrote: %s", types(s.all))
			}
			s.all = append(s.all, ev)
			out = append(out, ev)
			switch ev.Type {
			case proto.TypePermission:
				s.send(proto.Command{Type: proto.CmdPermit, CallID: ev.CallID})
			case proto.TypeTurnEnding:
				s.send(proto.Command{Type: proto.CmdAccept})
			case proto.TypeTurnEnd:
				return out
			}
		case <-timeout:
			s.t.Fatalf("the turn did not end within 15s; the session wrote: %s", types(s.all))
		}
	}
}

// end closes the session's stdin and returns the exit code.
func (s *session) end() int {
	s.t.Helper()
	_ = s.stdin.Close()
	select {
	case code := <-s.exit:
		s.exit <- code
		for ev := range s.events {
			s.all = append(s.all, ev)
		}
		return code
	case <-time.After(10 * time.Second):
		s.t.Fatal("the session did not end when its stdin did")
		return -1
	}
}

func TestASessionRunsEachPromptAsATurnOfOneConversation(t *testing.T) {
	model := scriptedmodel.New(t,
		asks("c1", "fs_read", `{"path":"a.txt"}`).WithUsage(scriptedmodel.Usage{Input: 100, Output: 10}),
		scriptedmodel.Text("It holds a greeting.").WithUsage(scriptedmodel.Usage{Input: 20, CacheRead: 110, Output: 6}),
		scriptedmodel.Text("Five words.").WithUsage(scriptedmodel.Usage{Input: 10, CacheRead: 136, Output: 3}),
	)
	g := newGateway(t, "fs_read")
	g.call = func(context.Context, string, json.RawMessage) (string, string) {
		return textResult("hello from the file"), ""
	}
	dir := t.TempDir()
	s := serve(t, model, g, dir, nil)

	if ready := s.open("chat-1"); ready.Conversation != "chat-1" || ready.Continued || strings.Join(ready.Tools, ",") != "fs_read" {
		t.Fatalf("ready: %+v", ready)
	}
	first := s.turn("Read a.txt and say what it holds.")
	// The call is announced, then asked about, then run.
	if got, want := types(first), "usage tool_call permission tool_result text usage turn_ending turn_end"; got != want {
		t.Fatalf("the first turn's events: %s\nwant:                    %s", got, want)
	}
	if p := first[2]; p.CallID != "c1" || p.Tool != "fs_read" {
		t.Errorf("permission: %+v", p)
	}
	if end := last(t, first, proto.TypeTurnEnd); end.Reason != proto.EndAnswered || end.Text != "It holds a greeting." || end.Conversation != "chat-1" {
		t.Errorf("the first turn's end: %+v", end)
	}
	// Every request reports its tokens and the conversation's size.
	if u := last(t, first, proto.TypeUsage).Usage; u == nil || u.Input != 20 || u.CacheRead != 110 || u.Output != 6 || u.ContextTokens != 130 || u.ContextLimit != 200000 {
		t.Errorf("the second request's usage: %+v", u)
	}

	second := s.turn("How many words is that?")
	if got, want := types(second), "text usage turn_ending turn_end"; got != want {
		t.Fatalf("the second turn's events: %s", got)
	}
	if end := last(t, second, proto.TypeTurnEnd); end.Text != "Five words." {
		t.Errorf("the second turn's end: %+v", end)
	}
	reqs := model.Requests()
	if got := roles(reqs[2]); got != "user assistant user assistant user" {
		t.Fatalf("the second turn's request: %s", got)
	}
	if m := lastMessage(t, reqs[2]); m.Text != "How many words is that?" {
		t.Errorf("the second prompt: %q", m.Text)
	}
	for i := 1; i < len(reqs); i++ {
		if err := scriptedmodel.PrefixOf(reqs[i-1], reqs[i]); err != nil {
			t.Errorf("request %d does not continue request %d: %v", i+1, i, err)
		}
	}
	if code := s.end(); code != harness.ExitAnswered {
		t.Errorf("exit %d", code)
	}
}

func TestADeniedCallIsItsResultAndTheTurnGoesOn(t *testing.T) {
	model := scriptedmodel.New(t,
		scriptedmodel.ToolCalls(
			scriptedmodel.Call{ID: "c1", Name: "fs_write", Arguments: `{"path":"a.txt"}`},
			scriptedmodel.Call{ID: "c2", Name: "fs_read", Arguments: `{"path":"a.txt"}`},
		),
		scriptedmodel.Text("I could not write the file, so I only read it."),
	)
	g := newGateway(t, "fs_read", "fs_write")
	s := serve(t, model, g, t.TempDir(), nil)
	s.open("chat-1")
	s.send(proto.Command{Type: proto.CmdPrompt, Text: "Write a.txt, then read it."})

	ask, _ := s.until(proto.TypePermission)
	if ask.CallID != "c1" || ask.Tool != "fs_write" {
		t.Fatalf("the first permission: %+v", ask)
	}
	// An answer for a call that is not the one asked about is not taken.
	s.send(proto.Command{Type: proto.CmdPermit, CallID: "c2"})
	if w, _ := s.until(proto.TypeWarning); w.Code != proto.WarnCommandRefused || w.Reason != proto.CmdPermit {
		t.Fatalf("the answer for another call: %+v", w)
	}
	s.send(proto.Command{Type: proto.CmdDeny, CallID: "c1"})
	denied, _ := s.until(proto.TypeToolResult)
	if denied.CallID != "c1" || !denied.Failed || !strings.Contains(denied.Text, "not permitted") {
		t.Fatalf("the denied call's result: %+v", denied)
	}
	events := s.answerUntilEnd()
	if end := last(t, events, proto.TypeTurnEnd); end.Reason != proto.EndAnswered {
		t.Fatalf("the turn's end: %+v", end)
	}
	if got := g.called(); len(got) != 1 || !strings.HasPrefix(got[0], "fs_read ") {
		t.Errorf("the gateway was sent: %v", got)
	}
	results := lastMessage(t, model.Requests()[1]).Results
	if len(results) != 2 || !results[0].IsError || !strings.Contains(results[0].Text, "not permitted") || results[1].IsError {
		t.Errorf("the results the model read: %+v", results)
	}
}

func TestAnInterruptEndsTheTurnWithEveryOpenCallSettled(t *testing.T) {
	model := scriptedmodel.New(t,
		scriptedmodel.ToolCalls(
			scriptedmodel.Call{ID: "c1", Name: "fs_read", Arguments: `{"path":"a.txt"}`},
			scriptedmodel.Call{ID: "c2", Name: "run_command", Arguments: `{"command":"make deploy"}`},
			scriptedmodel.Call{ID: "c3", Name: "fs_read", Arguments: `{"path":"b.txt"}`},
		),
		scriptedmodel.Text("The deploy was stopped; I did not repeat it."),
	)
	g := newGateway(t, "fs_read", "run_command")
	started := make(chan struct{})
	g.call = func(ctx context.Context, name string, _ json.RawMessage) (string, string) {
		if name == "run_command" {
			close(started)
			<-ctx.Done()
		}
		return textResult("ok"), ""
	}
	dir := t.TempDir()
	s := serve(t, model, g, dir, nil)
	s.open("chat-1")
	s.send(proto.Command{Type: proto.CmdPrompt, Text: "Read a.txt, deploy, read b.txt."})
	for range 2 {
		ask, _ := s.until(proto.TypePermission)
		s.send(proto.Command{Type: proto.CmdPermit, CallID: ask.CallID})
	}
	<-started
	s.send(proto.Command{Type: proto.CmdInterrupt})
	end, passed := s.until(proto.TypeTurnEnd)
	if end.Reason != proto.EndInterrupted {
		t.Fatalf("the turn's end: %+v", end)
	}
	// The call under way is answered where the runner can see it; the turn
	// ends with no error.
	var stopped []proto.Event
	for _, ev := range passed {
		if ev.Type == proto.TypeError {
			t.Errorf("a stopped turn reported an error: %+v", ev)
		}
		if ev.Type == proto.TypeToolResult && ev.CallID == "c2" {
			stopped = append(stopped, ev)
		}
	}
	if len(stopped) != 1 || !stopped[0].Failed || !strings.Contains(stopped[0].Text, "stopped before this call returned") {
		t.Errorf("the result of the call under way: %+v", stopped)
	}
	// The record holds a result for each of the reply's calls.
	answered := map[string]string{}
	var notes []string
	for _, e := range entries(t, dir, "chat-1") {
		if e.Kind == convo.KindResult {
			answered[e.CallID] = e.Result.Text
		}
		if e.Kind == convo.KindUser {
			notes = append(notes, e.Note)
		}
	}
	if len(answered) != 3 || answered["c1"] != "ok" || !strings.Contains(answered["c3"], "Not run") {
		t.Errorf("the record's results: %v", answered)
	}
	if strings.Join(notes, ",") != ","+convo.NoteStopped {
		t.Errorf("the record's user entries: %q", notes)
	}

	// The session goes on: the next turn's request is a whole conversation,
	// and the stopped call is not sent again.
	next := s.turn("What happened?")
	if end := last(t, next, proto.TypeTurnEnd); end.Reason != proto.EndAnswered {
		t.Fatalf("the next turn's end: %+v", end)
	}
	req := model.Requests()[1]
	if got := roles(req); got != "user assistant user" {
		t.Fatalf("the next turn's request: %s", got)
	}
	m := lastMessage(t, req)
	if len(m.Results) != 3 || m.Results[0].IsError || !m.Results[1].IsError || !m.Results[2].IsError {
		t.Errorf("the results the model read: %+v", m.Results)
	}
	if !strings.HasPrefix(m.Text, runtimeNote) || !strings.HasSuffix(m.Text, "What happened?") {
		t.Errorf("the message after the stopped turn: %q", m.Text)
	}
	if got := g.called(); len(got) != 2 {
		t.Errorf("the gateway was sent %d calls, want the 2 made before the stop: %v", len(got), got)
	}
}

func TestATurnStoppedInTheMiddleOfAnAnswerKeepsWhatWasWritten(t *testing.T) {
	model := scriptedmodel.New(t,
		scriptedmodel.Stall("The report so far: "),
		scriptedmodel.Text("Carrying on."),
	)
	g := newGateway(t, "fs_read")
	s := serve(t, model, g, t.TempDir(), nil)
	s.open("chat-1")
	s.send(proto.Command{Type: proto.CmdPrompt, Text: "Write the report."})
	s.until(proto.TypeText)
	s.send(proto.Command{Type: proto.CmdInterrupt})
	end, _ := s.until(proto.TypeTurnEnd)
	if end.Reason != proto.EndInterrupted || end.Text == "" || !strings.HasPrefix("The report so far: ", end.Text) {
		t.Fatalf("the turn's end: %+v", end)
	}
	// An interrupt with no turn under way does nothing.
	s.send(proto.Command{Type: proto.CmdInterrupt})

	s.turn("Go on.")
	req := model.Requests()[1]
	if got := roles(req); got != "user assistant user" {
		t.Fatalf("the next turn's request: %s", got)
	}
	if said := req.Messages[1].Text; said != end.Text {
		t.Errorf("the model's own words were sent back as %q, the turn ended with %q", said, end.Text)
	}
}

func TestASteerJoinsTheConversationAtTheNextBoundary(t *testing.T) {
	model := scriptedmodel.New(t,
		asks("c1", "fs_read", `{"path":"a.txt"}`),
		scriptedmodel.Text("Summarised in French, as asked."),
		scriptedmodel.Text("And shorter."),
		scriptedmodel.Text("Done with the next one too."),
	)
	g := newGateway(t, "fs_read")
	started, release := make(chan struct{}), make(chan struct{})
	g.call = func(context.Context, string, json.RawMessage) (string, string) {
		close(started)
		<-release
		return textResult("the file's text"), ""
	}
	s := serve(t, model, g, t.TempDir(), nil)
	s.open("chat-1")
	s.send(proto.Command{Type: proto.CmdPrompt, Text: "Summarise a.txt."})
	ask, _ := s.until(proto.TypePermission)
	s.send(proto.Command{Type: proto.CmdPermit, CallID: ask.CallID})
	<-started
	// While the call runs.
	s.send(proto.Command{Type: proto.CmdSteer, Text: "Write the summary in French."})
	close(release)
	if ev, _ := s.until(proto.TypeSteered); ev.Count != 1 {
		t.Fatalf("steered: %+v", ev)
	}
	// While the runner is asked whether the turn ends: the turn goes on.
	s.until(proto.TypeTurnEnding)
	s.send(proto.Command{Type: proto.CmdSteer, Text: "Make it shorter."})
	s.send(proto.Command{Type: proto.CmdAccept})
	if end := last(t, s.answerUntilEnd(), proto.TypeTurnEnd); end.Reason != proto.EndAnswered || end.Text != "And shorter." {
		t.Fatalf("the turn's end: %+v", end)
	}
	// Between turns: it goes ahead of the next prompt.
	s.send(proto.Command{Type: proto.CmdSteer, Text: "From now on, answer in one line."})
	s.turn("Now b.txt.")

	reqs := model.Requests()
	// After the call's result, in the same message.
	if m := lastMessage(t, reqs[1]); len(m.Results) != 1 || m.Results[0].Text != "the file's text" || m.Text != "Write the summary in French." {
		t.Errorf("the request after the call: %+v", m)
	}
	if m := lastMessage(t, reqs[2]); m.Role != "user" || m.Text != "Make it shorter." {
		t.Errorf("the request after the first answer: %+v", m)
	}
	if m := lastMessage(t, reqs[3]); m.Text != "From now on, answer in one line."+"Now b.txt." {
		t.Errorf("the next turn's request: %+v", m)
	}
	for i := 1; i < len(reqs); i++ {
		if err := scriptedmodel.PrefixOf(reqs[i-1], reqs[i]); err != nil {
			t.Errorf("request %d does not continue request %d: %v", i+1, i, err)
		}
	}
}

func TestASessionLoadsAStoredConversation(t *testing.T) {
	model := scriptedmodel.New(t,
		scriptedmodel.Text("The capital is Paris.").WithUsage(scriptedmodel.Usage{Input: 50, Output: 5}),
		scriptedmodel.Text("About two million people.").WithUsage(scriptedmodel.Usage{Input: 70, Output: 6}),
		scriptedmodel.Text("A new conversation."),
	)
	g := newGateway(t, "fs_read")
	dir := t.TempDir()
	prices := func(c map[string]any) {
		c["model"].(map[string]any)["prices"] = map[string]any{"input": 10.0, "cacheRead": 1.0, "cacheWrite": 12.5, "output": 100.0}
	}

	s := serve(t, model, g, dir, prices)
	s.open("chat-1")
	s.turn("What is the capital of France?")
	if code := s.end(); code != harness.ExitAnswered {
		t.Fatalf("exit %d", code)
	}

	s = serve(t, model, g, dir, prices)
	// A conversation that cannot be opened is said so, and the session
	// waits for one that can.
	s.send(proto.Command{Type: proto.CmdOpen, Conversation: "../chat-1"})
	if e, _ := s.until(proto.TypeError); e.Class != proto.ClassHarness {
		t.Fatalf("the refused open: %+v", e)
	}
	s.send(proto.Command{Type: proto.CmdPrompt, Text: "Anyone there?"})
	if w, _ := s.until(proto.TypeWarning); w.Code != proto.WarnCommandRefused {
		t.Fatalf("a prompt with no conversation open: %+v", w)
	}
	ready := s.open("chat-1")
	if !ready.Continued {
		t.Fatalf("ready: %+v", ready)
	}
	// It says where the conversation stands: its size, the window and what
	// it has cost.
	if u := ready.Usage; u == nil || u.ContextTokens != 55 || u.ContextLimit != 200000 || u.CostUSD < 0.000999 || u.CostUSD > 0.001001 {
		t.Fatalf("the continued conversation's standing: %+v", u)
	}
	events := s.turn("How many people live there?")
	req := model.Requests()[1]
	if got := roles(req); got != "user assistant user" || req.Messages[1].Text != "The capital is Paris." {
		t.Fatalf("the request on the loaded conversation: %s", got)
	}
	// The cost goes on from what the stored requests cost.
	want := (50*10.0 + 5*100.0 + 70*10.0 + 6*100.0) / 1e6
	if u := last(t, events, proto.TypeUsage).Usage; u.CostUSD < want-1e-9 || u.CostUSD > want+1e-9 {
		t.Errorf("cost %v, want %v", u.CostUSD, want)
	}

	// Another conversation, opened between turns in the same process. A
	// message that was waiting for the one left behind is not said in it.
	s.send(proto.Command{Type: proto.CmdSteer, Text: "And its population in 1900?"})
	if ready := s.open("chat-2"); ready.Continued || ready.Conversation != "chat-2" {
		t.Fatalf("ready: %+v", ready)
	}
	s.turn("Hello.")
	if req := model.Requests()[2]; roles(req) != "user" || lastMessage(t, req).Text != "Hello." {
		t.Errorf("the new conversation's request: %s, ending with %q", roles(req), lastMessage(t, req).Text)
	}
}

func TestACommandThatCannotBeTakenIsRefusedAndTheSessionGoesOn(t *testing.T) {
	model := scriptedmodel.New(t,
		scriptedmodel.Status(402, "Insufficient credits."),
		scriptedmodel.Text("Hello."),
	)
	g := newGateway(t, "fs_read")
	s := serve(t, model, g, t.TempDir(), nil)
	refused := func(cmd proto.Command) {
		t.Helper()
		s.send(cmd)
		if w, _ := s.until(proto.TypeWarning); w.Code != proto.WarnCommandRefused || w.Reason != cmd.Type {
			t.Fatalf("%s: %+v", cmd.Type, w)
		}
	}
	refused(proto.Command{Type: proto.CmdPrompt, Text: "Hello?"})
	s.open("chat-1")
	refused(proto.Command{Type: proto.CmdAccept})
	refused(proto.Command{Type: proto.CmdDeny, CallID: "c9"})
	refused(proto.Command{Type: "dance"})
	refused(proto.Command{Type: proto.CmdPrompt, Text: "  "})

	// A turn that fails says why with its class, and the session is there
	// for the next one.
	events := s.turn("Hello?")
	if e := last(t, events, proto.TypeError); e.Class != proto.ClassCredits || e.Status != 402 {
		t.Errorf("the failed turn's error: %+v", e)
	}
	if end := last(t, events, proto.TypeTurnEnd); end.Reason != proto.EndError {
		t.Errorf("the failed turn's end: %+v", end)
	}
	// The same prompt sent again after the failure is in the conversation
	// once.
	if end := last(t, s.turn("Hello?"), proto.TypeTurnEnd); end.Reason != proto.EndAnswered || end.Text != "Hello." {
		t.Errorf("the next turn's end: %+v", end)
	}
	if req := model.Requests()[1]; roles(req) != "user" || lastMessage(t, req).Text != "Hello?" {
		t.Errorf("the request after the failed turn: %s, ending with %q", roles(req), lastMessage(t, req).Text)
	}
	if code := s.end(); code != harness.ExitAnswered {
		t.Errorf("exit %d", code)
	}
}

func TestALineThatIsNoCommandEndsTheSession(t *testing.T) {
	model := scriptedmodel.New(t)
	g := newGateway(t, "fs_read")
	s := serve(t, model, g, t.TempDir(), nil)
	s.open("chat-1")
	if _, err := io.WriteString(s.stdin, "{\"type\":\"prompt\",\"words\":\"x\"}\n"); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.until(proto.TypeError); e.Class != proto.ClassHarness {
		t.Errorf("error: %+v", e)
	}
	select {
	case code := <-s.exit:
		s.exit <- code
		if code != harness.ExitFailed {
			t.Errorf("exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the session did not end")
	}
}

func TestTheSessionModeNeedsItsConfiguration(t *testing.T) {
	var stderr bytes.Buffer
	if code := harness.Serve(context.Background(), nil, strings.NewReader(""), io.Discard, &stderr, func(string) string { return "" }); code != harness.ExitUsage || !strings.Contains(stderr.String(), "usage: harness serve") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestACompactionTheRunnerAsksFor(t *testing.T) {
	const summary = "The person asked what a.txt holds; it holds a greeting, and that was said."
	model := scriptedmodel.New(t,
		asks("c1", "fs_read", `{"path":"a.txt"}`).WithUsage(scriptedmodel.Usage{Input: 400, Output: 20}),
		scriptedmodel.Text("It holds a greeting.").WithUsage(scriptedmodel.Usage{Input: 450, Output: 10}),
		scriptedmodel.Text(summary).WithUsage(scriptedmodel.Usage{Input: 500, Output: 30}),
		scriptedmodel.Text("Yes, a greeting."),
	)
	g := newGateway(t, "fs_read")
	dir := t.TempDir()
	s := serve(t, model, g, dir, set("compaction", map[string]any{"threshold": 0.8, "checkpoint": "Keep what the person asked."}))
	// Nothing is open, and then nothing has been said: there is nothing to
	// summarise, and the model is asked nothing.
	s.send(proto.Command{Type: proto.CmdCompact})
	if w, _ := s.until(proto.TypeWarning); w.Code != proto.WarnCommandRefused {
		t.Fatalf("a compact with no conversation open: %+v", w)
	}
	s.open("chat-1")
	s.send(proto.Command{Type: proto.CmdCompact})
	if w, _ := s.until(proto.TypeWarning); w.Code != proto.WarnCompactionFailed || w.Reason != proto.CompactionRequest {
		t.Fatalf("a compact of an empty conversation: %+v", w)
	}
	if n := len(model.Requests()); n != 0 {
		t.Fatalf("%d requests were made for it", n)
	}

	s.turn("What does a.txt hold?")
	s.send(proto.Command{Type: proto.CmdCompact})
	done, passed := s.until(proto.TypeWarning)
	if done.Code != proto.WarnCompaction || done.Reason != proto.CompactionRequest {
		t.Fatalf("the answer to the compact: %+v", done)
	}
	// The summary is no answer of the agent's, and the conversation's size
	// after it is said before the answer.
	var after *proto.Usage
	for _, ev := range passed {
		if ev.Type == proto.TypeText {
			t.Errorf("the summary was written as answer text: %+v", ev)
		}
		if ev.Type == proto.TypeUsage && ev.Usage.Compacted {
			after = ev.Usage
		}
	}
	if after == nil || after.ContextTokens <= 0 || after.ContextTokens >= 480 {
		t.Errorf("the size after the summary: %+v", after)
	}
	// The next command may come the moment the answer is read.
	events := s.turn("So it is a greeting?")
	if end := last(t, events, proto.TypeTurnEnd); end.Reason != proto.EndAnswered {
		t.Fatalf("the turn after the summary: %+v", end)
	}
	reqs := model.Requests()
	// The summary request is the conversation as it stood with the
	// instruction last; the next turn's request opens with the summary.
	if err := scriptedmodel.PrefixOf(reqs[1], reqs[2]); err != nil {
		t.Errorf("the summary request does not continue the conversation: %v", err)
	}
	if got := roles(reqs[3]); got != "user" || !strings.Contains(reqs[3].Messages[0].Text, summary) || !strings.HasSuffix(reqs[3].Messages[0].Text, "So it is a greeting?") {
		t.Errorf("the request after the summary: %s: %q", got, reqs[3].Messages[0].Text)
	}
	var reasons []string
	for _, e := range entries(t, dir, "chat-1") {
		if e.Kind == convo.KindCompaction {
			reasons = append(reasons, e.Reason)
		}
	}
	if strings.Join(reasons, ",") != proto.CompactionRequest {
		t.Errorf("the record's compactions: %q", reasons)
	}
}

func TestAnInterruptGivesUpACompactionTheRunnerAskedFor(t *testing.T) {
	model := scriptedmodel.New(t,
		scriptedmodel.Text("It holds a greeting."),
		scriptedmodel.Stall("The person asked"),
		scriptedmodel.Text("Still a greeting."),
	)
	g := newGateway(t, "fs_read")
	dir := t.TempDir()
	s := serve(t, model, g, dir, set("compaction", map[string]any{"threshold": 0.8}))
	s.open("chat-1")
	s.turn("What does a.txt hold?")
	s.send(proto.Command{Type: proto.CmdCompact})
	// A prompt meanwhile is not taken: the summary has the turn's place.
	s.send(proto.Command{Type: proto.CmdPrompt, Text: "Hello?"})
	if w, _ := s.until(proto.TypeWarning); w.Code != proto.WarnCommandRefused || w.Reason != proto.CmdPrompt {
		t.Fatalf("a prompt during the summary: %+v", w)
	}
	// Once the summary request is with the model, it is given up.
	for deadline := time.Now().Add(10 * time.Second); len(model.Requests()) < 2; {
		if time.Now().After(deadline) {
			t.Fatal("the summary request never reached the model")
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.send(proto.Command{Type: proto.CmdInterrupt})
	if w, _ := s.until(proto.TypeWarning); w.Code != proto.WarnCompactionFailed || w.Reason != proto.CompactionRequest {
		t.Fatalf("the answer to the compact: %+v", w)
	}
	// The conversation is as it was.
	s.turn("And now?")
	if got := roles(model.Requests()[2]); got != "user assistant user" {
		t.Errorf("the request after the summary was given up: %s", got)
	}
	for _, e := range entries(t, dir, "chat-1") {
		if e.Kind == convo.KindCompaction {
			t.Errorf("the record holds a compaction: %+v", e)
		}
	}
}

func TestAConversationCarriedOnUnderAnotherFrameLeavesItsOldReasoningBehind(t *testing.T) {
	script := func() *scriptedmodel.Server {
		return scriptedmodel.New(t,
			asks("c1", "fs_read", `{"path":"a.txt"}`).WithReasoning("I should read the file first."),
			scriptedmodel.Text("It holds a greeting.").WithReasoning("The file is short."),
		)
	}
	replayed := func(r scriptedmodel.Request) int {
		n := 0
		for _, m := range r.Messages {
			if m.Reasoning != "" {
				n++
			}
		}
		return n
	}
	for name, tc := range map[string]struct {
		edit func(map[string]any)
		tool string
		want int // replies of the first process sent back with their reasoning
	}{
		"the same frame":        {nil, "", 2},
		"another system prompt": {set("system", "You are a careful agent. Today is Tuesday."), "", 0},
		"another model":         {func(c map[string]any) { c["model"].(map[string]any)["name"] = "claude-next" }, "", 0},
		"another tool":          {nil, "fs_write", 0},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			first := script()
			s := serve(t, first, newGateway(t, "fs_read"), dir, nil)
			s.open("chat-1")
			s.turn("What does a.txt hold?")
			// Inside one process every reply goes back whole.
			if got := replayed(first.Requests()[1]); got != 1 {
				t.Fatalf("the first process replayed %d replies' reasoning, want 1", got)
			}
			s.end()

			tools := []string{"fs_read"}
			if tc.tool != "" {
				tools = append(tools, tc.tool)
			}
			second := scriptedmodel.New(t,
				scriptedmodel.Text("Yes.").WithReasoning("Easy."),
				scriptedmodel.Text("Yes, still."),
			)
			s = serve(t, second, newGateway(t, tools...), dir, tc.edit)
			if ready := s.open("chat-1"); !ready.Continued {
				t.Fatalf("ready: %+v", ready)
			}
			s.turn("A greeting, then?")
			s.turn("Sure?")
			reqs := second.Requests()
			if got := replayed(reqs[0]); got != tc.want {
				t.Errorf("the first request replayed %d replies' reasoning, want %d", got, tc.want)
			}
			// A reply made under the frame the process has goes back whole.
			if got := replayed(reqs[1]); got != tc.want+1 {
				t.Errorf("the second request replayed %d replies' reasoning, want %d", got, tc.want+1)
			}
			if err := scriptedmodel.PrefixOf(reqs[0], reqs[1]); err != nil {
				t.Errorf("the second request does not continue the first: %v", err)
			}
			frames := 0
			for _, e := range entries(t, dir, "chat-1") {
				if e.Kind == convo.KindFrame {
					frames++
				}
			}
			if want := map[bool]int{true: 1, false: 2}[tc.want == 2]; frames != want {
				t.Errorf("%d frame entries in the record, want %d", frames, want)
			}
		})
	}
}
