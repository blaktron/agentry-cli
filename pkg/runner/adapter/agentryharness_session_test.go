package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/runnerimage/harness/proto"
)

// fakeHarness plays the runner's own harness in its session mode: it reads
// the driver's commands and writes events, as a script says.
type fakeHarness struct {
	t   *testing.T
	in  *bufio.Scanner
	out io.WriteCloser
	seq int64

	mu   sync.Mutex
	seen []proto.Command
}

// startFakeHarness returns the driver's transport and runs script as the
// harness on the other end of it.
func startFakeHarness(t *testing.T, script func(h *fakeHarness)) (io.ReadWriteCloser, *fakeHarness) {
	t.Helper()
	toHarnessR, toHarnessW := io.Pipe()
	fromHarnessR, fromHarnessW := io.Pipe()
	h := &fakeHarness{t: t, in: bufio.NewScanner(toHarnessR), out: fromHarnessW}
	h.in.Buffer(make([]byte, 0, 64<<10), 16<<20)
	go func() {
		defer func() { _ = fromHarnessW.Close() }()
		script(h)
		// Whatever the driver still writes is read, so it never blocks.
		for h.in.Scan() {
		}
	}()
	return pipeRW{Reader: fromHarnessR, WriteCloser: toHarnessW}, h
}

// write writes one event.
func (h *fakeHarness) write(ev proto.Event) {
	h.seq++
	ev.V, ev.Seq = proto.Version, h.seq
	line, _ := json.Marshal(ev)
	_, _ = h.out.Write(append(line, '\n'))
}

// raw writes one line as it is.
func (h *fakeHarness) raw(line string) { _, _ = io.WriteString(h.out, line+"\n") }

// read is the driver's next command; the zero Command when its side closed.
func (h *fakeHarness) read() proto.Command {
	if !h.in.Scan() {
		return proto.Command{}
	}
	var cmd proto.Command
	if err := json.Unmarshal(h.in.Bytes(), &cmd); err != nil {
		h.t.Errorf("the driver wrote a line that is no command: %s", h.in.Text())
	}
	h.mu.Lock()
	h.seen = append(h.seen, cmd)
	h.mu.Unlock()
	return cmd
}

// expect reads the next command and holds it to typ.
func (h *fakeHarness) expect(typ string) proto.Command {
	cmd := h.read()
	if cmd.Type != typ {
		h.t.Errorf("the driver wrote %+v, want a %s", cmd, typ)
	}
	return cmd
}

// opened answers the session's open with a ready event.
func (h *fakeHarness) opened(continued bool) proto.Command {
	cmd := h.expect(proto.CmdOpen)
	h.write(proto.Event{Type: proto.TypeReady, Conversation: cmd.Conversation, Tools: []string{"fs_list"}, Continued: continued})
	return cmd
}

// drive starts a driver on rw and collects its events until it closes.
func driveHarnessSession(t *testing.T, rw io.ReadWriteCloser, opts SessionOptions) (SessionDriver, func() []Event) {
	t.Helper()
	ad, err := Get(AgentryHarnessAdapterName)
	if err != nil {
		t.Fatal(err)
	}
	drv, err := ad.Session(context.Background(), rw, opts)
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	collected := make(chan struct{})
	go func() {
		for ev := range drv.Events() {
			events = append(events, ev)
		}
		close(collected)
	}()
	return drv, func() []Event {
		if err := drv.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
		<-collected
		return events
	}
}

func harnessTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestAgentryHarnessSessionDrivesOneTurnAndAnswersPermissions(t *testing.T) {
	rw, _ := startFakeHarness(t, func(h *fakeHarness) {
		open := h.opened(false)
		if open.Conversation == "" {
			t.Error("the open names no conversation")
		}
		if p := h.expect(proto.CmdPrompt); p.Text != "list the files" {
			t.Errorf("prompt: %+v", p)
		}
		h.write(proto.Event{Type: proto.TypeToolCall, CallID: "c1", Tool: "fs_list", Arguments: json.RawMessage(`{}`)})
		// A gateway tool is permitted.
		h.write(proto.Event{Type: proto.TypePermission, CallID: "c1", Tool: "fs_list"})
		if a := h.expect(proto.CmdPermit); a.CallID != "c1" {
			t.Errorf("the answer for fs_list: %+v", a)
		}
		h.write(proto.Event{Type: proto.TypeToolResult, CallID: "c1", Tool: "fs_list", Text: "notes.txt\t25"})
		// A tool the gateway does not serve is denied, and the harness
		// answers the call with the denial: the turn goes on.
		h.write(proto.Event{Type: proto.TypeToolCall, CallID: "c2", Tool: "shell_exec", Arguments: json.RawMessage(`{}`)})
		h.write(proto.Event{Type: proto.TypePermission, CallID: "c2", Tool: "shell_exec"})
		if a := h.expect(proto.CmdDeny); a.CallID != "c2" {
			t.Errorf("the answer for shell_exec: %+v", a)
		}
		h.write(proto.Event{Type: proto.TypeToolResult, CallID: "c2", Tool: "shell_exec", Failed: true, Text: "Not run: this call was not permitted."})
		h.write(proto.Event{Type: proto.TypeText, Text: "Hello, "})
		h.write(proto.Event{Type: proto.TypeText, Text: "world."})
		h.write(proto.Event{Type: proto.TypeUsage, Usage: &proto.Usage{Input: 12, CacheRead: 100, Output: 3, ContextTokens: 112, ContextLimit: 200000}})
		// The turn's end is the runner's to accept.
		h.write(proto.Event{Type: proto.TypeTurnEnding})
		h.expect(proto.CmdAccept)
		h.write(proto.Event{Type: proto.TypeTurnEnd, Reason: proto.EndAnswered, Text: "Hello, world.", Conversation: open.Conversation})
	})
	drv, closeAndCollect := driveHarnessSession(t, rw, SessionOptions{GatewayTools: []string{"fs_list", "write_report"}})
	ctx := harnessTestContext(t)
	if err := drv.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	c, ok := drv.(Continuing)
	if !ok || c.ConversationID() == "" {
		t.Fatalf("the session names no conversation: %v", drv)
	}
	if continued, why := c.Continued(); continued || why != "" {
		t.Errorf("a new conversation: continued=%v why=%q", continued, why)
	}
	turn, err := drv.Prompt(ctx, "list the files")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if turn.StopReason != "end_turn" || turn.Text != "Hello, world." {
		t.Errorf("turn = %+v", turn)
	}
	if turn.Usage == nil || turn.Usage.InputTokens != 112 || turn.Usage.OutputTokens != 3 || turn.Usage.TotalTokens != 115 {
		t.Errorf("usage = %+v", turn.Usage)
	}
	events := closeAndCollect()

	kinds := map[EventKind]int{}
	var granted, denied, text string
	for _, ev := range events {
		kinds[ev.Kind]++
		switch ev.Kind {
		case EventPermissionGranted:
			granted = ev.Text
		case EventPermissionDenied:
			denied = ev.Text
		case EventAssistantText:
			text += ev.Text
		case EventUsage:
			if ev.InputTokens != 112 || ev.OutputTokens != 3 || ev.ContextTokens != 112 || ev.ContextLimit != 200000 {
				t.Errorf("usage event: %+v", ev)
			}
		}
		if len(ev.Raw) == 0 {
			t.Errorf("event %s dropped its raw line", ev.Kind)
		}
	}
	if kinds[EventToolCall] != 2 || kinds[EventToolResult] != 2 || kinds[EventUsage] != 1 || kinds[EventError] != 0 {
		t.Errorf("event kinds: %v", kinds)
	}
	if granted != "fs_list" || denied != "shell_exec" || text != "Hello, world." {
		t.Errorf("granted %q, denied %q, text %q", granted, denied, text)
	}
}

func TestAgentryHarnessSessionKeepsWhatItDoesNotKnowAsAnObservation(t *testing.T) {
	rw, _ := startFakeHarness(t, func(h *fakeHarness) {
		h.opened(false)
		h.expect(proto.CmdPrompt)
		h.raw("a line that is not JSON")
		h.raw(`{"v":9,"seq":3,"type":"telemetry","text":"something a later harness says"}`)
		h.raw(`{"jsonrpc":"2.0","id":7,"method":"fs/read_text_file","params":{"path":"/etc/passwd"}}`)
		h.write(proto.Event{Type: proto.TypeText, Text: "Done."})
		h.write(proto.Event{Type: proto.TypeTurnEnd, Reason: proto.EndAnswered, Text: "Done."})
	})
	drv, closeAndCollect := driveHarnessSession(t, rw, SessionOptions{})
	ctx := harnessTestContext(t)
	if err := drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	turn, err := drv.Prompt(ctx, "go")
	if err != nil || turn.Text != "Done." {
		t.Fatalf("turn %+v, err %v", turn, err)
	}
	others := 0
	for _, ev := range closeAndCollect() {
		switch ev.Kind {
		case EventOther:
			others++
		case EventError:
			t.Errorf("an unknown line became an error: %+v", ev)
		}
	}
	// The ready, the three unknown lines and the turn's end.
	if others != 5 {
		t.Errorf("%d observations, want 5", others)
	}
}

func TestAgentryHarnessSessionInterruptStopsThePromptInFlight(t *testing.T) {
	rw, _ := startFakeHarness(t, func(h *fakeHarness) {
		h.opened(false)
		h.expect(proto.CmdPrompt)
		h.write(proto.Event{Type: proto.TypeText, Text: "The report so far"})
		h.expect(proto.CmdInterrupt)
		h.write(proto.Event{Type: proto.TypeTurnEnd, Reason: proto.EndInterrupted, Text: "The report so far"})
	})
	drv, closeAndCollect := driveHarnessSession(t, rw, SessionOptions{})
	defer closeAndCollect()
	ctx := harnessTestContext(t)
	if err := drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	it, ok := drv.(Interrupter)
	if !ok {
		t.Fatal("the driver cannot stop a turn")
	}
	// With no turn in flight it does nothing: the harness would otherwise
	// read an interrupt before its prompt.
	if err := it.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var turn *Turn
	var perr error
	go func() {
		defer close(done)
		turn, perr = drv.Prompt(ctx, "write the report")
	}()
	// Asked again until the turn is the driver's, as the engine asks.
	for stopped := false; !stopped; {
		if err := it.Interrupt(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
			stopped = true
		case <-time.After(20 * time.Millisecond):
		}
	}
	if perr != nil || !turn.Stopped() || turn.Text != "The report so far" {
		t.Errorf("turn %+v, err %v", turn, perr)
	}
}

func TestAgentryHarnessSessionTurnUsageSumsItsRequests(t *testing.T) {
	rw, _ := startFakeHarness(t, func(h *fakeHarness) {
		h.opened(false)
		for range 2 {
			h.expect(proto.CmdPrompt)
			h.write(proto.Event{Type: proto.TypeUsage, Usage: &proto.Usage{Input: 100, Output: 10, ContextTokens: 100, CostUSD: 0.01}})
			// The conversation's size after a summary is no request.
			h.write(proto.Event{Type: proto.TypeUsage, Usage: &proto.Usage{ContextTokens: 40, Compacted: true, CostUSD: 0.01}})
			h.write(proto.Event{Type: proto.TypeUsage, Usage: &proto.Usage{Input: 5, CacheRead: 40, CacheWrite: 15, Output: 7, ContextTokens: 60, CostUSD: 0.02}})
			h.write(proto.Event{Type: proto.TypeTurnEnd, Reason: proto.EndAnswered, Text: "ok"})
		}
		h.expect(proto.CmdPrompt)
		h.write(proto.Event{Type: proto.TypeTurnEnd, Reason: proto.EndAnswered, Text: "ok"})
	})
	drv, closeAndCollect := driveHarnessSession(t, rw, SessionOptions{})
	ctx := harnessTestContext(t)
	if err := drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		turn, err := drv.Prompt(ctx, "go")
		if err != nil {
			t.Fatal(err)
		}
		// Each turn counts its own requests, not the session's.
		if turn.Usage == nil || turn.Usage.InputTokens != 160 || turn.Usage.OutputTokens != 17 {
			t.Errorf("usage = %+v", turn.Usage)
		}
	}
	// A turn with no request reports no usage rather than zero tokens.
	if turn, err := drv.Prompt(ctx, "go"); err != nil || turn.Usage != nil {
		t.Errorf("turn %+v, err %v", turn, err)
	}
	var compacted int
	var cost float64
	for _, ev := range closeAndCollect() {
		if ev.Kind != EventUsage {
			continue
		}
		cost = max(cost, ev.CostUSD)
		if ev.ContextCompacted {
			compacted++
			if ev.ContextTokens != 40 || ev.InputTokens != 0 {
				t.Errorf("the size after a summary: %+v", ev)
			}
		}
	}
	if compacted != 2 || cost != 0.02 {
		t.Errorf("compacted %d, cost %v", compacted, cost)
	}
}

func TestAgentryHarnessSessionAPromptErrorCarriesTheHarnessesWords(t *testing.T) {
	rw, _ := startFakeHarness(t, func(h *fakeHarness) {
		h.opened(false)
		h.expect(proto.CmdPrompt)
		h.write(proto.Event{Type: proto.TypeError, Class: proto.ClassAuth, Status: 401, Text: "invalid x-api-key"})
		h.write(proto.Event{Type: proto.TypeTurnEnd, Reason: proto.EndError})
		// The session is there for the next turn, which the server is too
		// busy for: words the engine waits out.
		h.expect(proto.CmdPrompt)
		h.write(proto.Event{Type: proto.TypeError, Class: proto.ClassCapacity, Status: 529, Text: "Overloaded"})
		h.write(proto.Event{Type: proto.TypeTurnEnd, Reason: proto.EndError})
		// And one the account cannot pay for.
		h.expect(proto.CmdPrompt)
		h.write(proto.Event{Type: proto.TypeError, Class: proto.ClassCredits, Status: 402, Text: "Insufficient credits"})
		h.write(proto.Event{Type: proto.TypeTurnEnd, Reason: proto.EndError})
	})
	drv, closeAndCollect := driveHarnessSession(t, rw, SessionOptions{})
	ctx := harnessTestContext(t)
	if err := drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := drv.Prompt(ctx, "go"); err == nil || err.Error() != "HTTP 401: invalid x-api-key" {
		t.Errorf("the refused key: %v", err)
	}
	if _, err := drv.Prompt(ctx, "go"); err == nil || !AtCapacity(err.Error()) {
		t.Errorf("the busy server: %v", err)
	}
	if _, err := drv.Prompt(ctx, "go"); err == nil || !strings.Contains(err.Error(), "Insufficient credits") {
		t.Errorf("the unpaid account: %v", err)
	}
	var errs, credits []string
	for _, ev := range closeAndCollect() {
		switch ev.Kind {
		case EventError:
			errs = append(errs, ev.Text)
		case EventOutOfCredits:
			credits = append(credits, ev.Text)
		}
	}
	// The engine says a failure once: the turn's error restates the event.
	if len(errs) != 2 || errs[0] != "HTTP 401: invalid x-api-key" {
		t.Errorf("error events: %q", errs)
	}
	if len(credits) != 1 || !strings.Contains(credits[0], "Insufficient credits") {
		t.Errorf("out-of-credits events: %q", credits)
	}
}

func TestAgentryHarnessSessionEventsAreCompleteBeforeThePromptReturns(t *testing.T) {
	rw, _ := startFakeHarness(t, func(h *fakeHarness) {
		h.opened(false)
		h.expect(proto.CmdPrompt)
		for range 50 {
			h.write(proto.Event{Type: proto.TypeToolCall, CallID: "c", Tool: "fs_list", Arguments: json.RawMessage(`{}`)})
		}
		h.write(proto.Event{Type: proto.TypeUsage, Usage: &proto.Usage{Input: 1, Output: 1}})
		h.write(proto.Event{Type: proto.TypeTurnEnd, Reason: proto.EndAnswered, Text: "ok"})
	})
	ad, _ := Get(AgentryHarnessAdapterName)
	drv, err := ad.Session(context.Background(), rw, SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = drv.Close() }()
	ctx := harnessTestContext(t)
	if err := drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := drv.Prompt(ctx, "go"); err != nil {
		t.Fatal(err)
	}
	// Nobody read the events while the turn ran: they are all queued now.
	calls, usage, end := 0, 0, false
	for drained := false; !drained; {
		select {
		case ev := <-drv.Events():
			switch {
			case ev.Kind == EventToolCall:
				calls++
			case ev.Kind == EventUsage:
				usage++
			case strings.Contains(string(ev.Raw), `"turn_end"`):
				end = true
			}
		default:
			drained = true
		}
	}
	if calls != 50 || usage != 1 || !end {
		t.Errorf("queued when Prompt returned: %d tool calls, %d usage, turn end %v", calls, usage, end)
	}
}

func TestAgentryHarnessSessionContinuesAStoredConversation(t *testing.T) {
	const stored = "0c5d1f6e-58a1-4f0c-9a53-6f6a2f1f4a11"
	t.Run("loaded", func(t *testing.T) {
		rw, h := startFakeHarness(t, func(h *fakeHarness) { h.opened(true) })
		drv, closeAndCollect := driveHarnessSession(t, rw, SessionOptions{ContinueID: stored})
		if err := drv.Start(harnessTestContext(t)); err != nil {
			t.Fatal(err)
		}
		c := drv.(Continuing)
		if continued, why := c.Continued(); !continued || why != "" || c.ConversationID() != stored {
			t.Errorf("continued=%v why=%q id=%q", continued, why, c.ConversationID())
		}
		closeAndCollect()
		if h.seen[0].Conversation != stored {
			t.Errorf("the open: %+v", h.seen[0])
		}
	})
	t.Run("nothing stored under the id", func(t *testing.T) {
		rw, _ := startFakeHarness(t, func(h *fakeHarness) { h.opened(false) })
		drv, closeAndCollect := driveHarnessSession(t, rw, SessionOptions{ContinueID: stored})
		defer closeAndCollect()
		if err := drv.Start(harnessTestContext(t)); err != nil {
			t.Fatal(err)
		}
		c := drv.(Continuing)
		if continued, why := c.Continued(); continued || why == "" || c.ConversationID() != stored {
			t.Errorf("continued=%v why=%q id=%q", continued, why, c.ConversationID())
		}
	})
	t.Run("a record the harness cannot read", func(t *testing.T) {
		rw, h := startFakeHarness(t, func(h *fakeHarness) {
			h.expect(proto.CmdOpen)
			h.write(proto.Event{Type: proto.TypeError, Class: proto.ClassHarness, Text: "conversation " + stored + ": the file is format 9, this harness reads format 1"})
			h.opened(false)
		})
		drv, closeAndCollect := driveHarnessSession(t, rw, SessionOptions{ContinueID: stored, ConversationID: "chosen-by-the-engine"})
		if err := drv.Start(harnessTestContext(t)); err != nil {
			t.Fatal(err)
		}
		c := drv.(Continuing)
		continued, why := c.Continued()
		if continued || !strings.Contains(why, "format 9") || c.ConversationID() != "chosen-by-the-engine" {
			t.Errorf("continued=%v why=%q id=%q", continued, why, c.ConversationID())
		}
		// A conversation that gave way to a new one is not the session's
		// failure.
		for _, ev := range closeAndCollect() {
			if ev.Kind == EventError {
				t.Errorf("the refused open became the session's error: %+v", ev)
			}
		}
		if len(h.seen) != 2 || h.seen[1].Conversation != "chosen-by-the-engine" {
			t.Errorf("the opens: %+v", h.seen)
		}
	})
	t.Run("a harness that cannot start", func(t *testing.T) {
		rw, _ := startFakeHarness(t, func(h *fakeHarness) {
			for range 2 {
				h.expect(proto.CmdOpen)
				h.write(proto.Event{Type: proto.TypeError, Class: proto.ClassGateway, Text: "401 Unauthorized"})
			}
		})
		drv, closeAndCollect := driveHarnessSession(t, rw, SessionOptions{ContinueID: stored})
		defer closeAndCollect()
		if err := drv.Start(harnessTestContext(t)); err == nil || !strings.Contains(err.Error(), "the tool gateway failed (401 Unauthorized)") {
			t.Errorf("start: %v", err)
		}
	})
}

func TestAgentryHarnessSessionSteersAndOutlivesAHarnessThatEnds(t *testing.T) {
	prompted := make(chan struct{})
	rw, h := startFakeHarness(t, func(h *fakeHarness) {
		h.opened(false)
		h.expect(proto.CmdPrompt)
		close(prompted)
		if s := h.expect(proto.CmdSteer); s.Text != "in French, please" {
			t.Errorf("steer: %+v", s)
		}
		h.write(proto.Event{Type: proto.TypeSteered, Count: 1})
		h.write(proto.Event{Type: proto.TypeTurnEnd, Reason: proto.EndAnswered, Text: "Bonjour."})
		// A prompt the harness does not take ends its Prompt, which no
		// turn_end would.
		refused := h.expect(proto.CmdPrompt)
		h.write(proto.Event{Type: proto.TypeWarning, Code: proto.WarnCommandRefused, Reason: refused.Type, Text: `The command "prompt" was not taken: a turn is under way.`})
		// The harness dies in the middle of the next turn.
		h.expect(proto.CmdPrompt)
		h.write(proto.Event{Type: proto.TypeText, Text: "Half an ans"})
		_ = h.out.Close()
	})
	drv, closeAndCollect := driveHarnessSession(t, rw, SessionOptions{})
	defer closeAndCollect()
	ctx := harnessTestContext(t)
	if err := drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	st, ok := drv.(Steerer)
	if !ok {
		t.Fatal("the driver cannot steer a turn")
	}
	done := make(chan *Turn, 1)
	go func() {
		turn, err := drv.Prompt(ctx, "greet me")
		if err != nil {
			t.Errorf("prompt: %v", err)
		}
		done <- turn
	}()
	<-prompted
	// An empty steer is nothing to send.
	if err := st.Steer(ctx, "  "); err != nil {
		t.Fatal(err)
	}
	if err := st.Steer(ctx, "in French, please"); err != nil {
		t.Fatal(err)
	}
	if turn := <-done; turn == nil || turn.Text != "Bonjour." {
		t.Errorf("turn = %+v", turn)
	}
	if _, err := drv.Prompt(ctx, "again"); err == nil || !strings.Contains(err.Error(), "did not take the prompt") {
		t.Errorf("the refused prompt: %v", err)
	}
	if _, err := drv.Prompt(ctx, "once more"); err == nil || !strings.Contains(err.Error(), "ended the session") {
		t.Errorf("the turn the harness died in: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, cmd := range h.seen {
		if cmd.Type == proto.CmdSteer && strings.TrimSpace(cmd.Text) == "" {
			t.Error("an empty steer was sent")
		}
	}
}

func TestAgentryHarnessCompilesASessionOfTheSameHarness(t *testing.T) {
	in := agentryHarnessInput()
	run, err := (&agentryHarnessAdapter{}).Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	in.Mode = "interactive"
	res, err := (&agentryHarnessAdapter{}).Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	// The configuration is the one-shot run's, byte for byte: what a
	// session changes is how the harness is driven.
	if string(res.Files[agentryHarnessConfig]) != string(run.Files[agentryHarnessConfig]) {
		t.Error("a session's configuration differs from a run's")
	}
	if got := strings.Join(res.Agent.Argv, " "); got != "harness serve --config /plan/harness.json" {
		t.Errorf("argv: %s", got)
	}
	if res.Agent.StdinTask || res.Resume != nil || res.OneTurn != nil {
		t.Errorf("a session takes its turns as commands: stdinTask=%v resume=%+v oneTurn=%+v", res.Agent.StdinTask, res.Resume, res.OneTurn)
	}
	s := res.Session
	if s == nil || s.Transport != agentryHarnessTransport || s.ProtocolVersion != proto.Version || s.StoreOwner != res.Agent.User || s.ConversationArgs != nil {
		t.Fatalf("session: %+v", s)
	}
	// The record's volume is mounted once, by the session block.
	if len(s.Mounts) != 1 || s.Mounts[0].Target != "/session" || s.Mounts[0].ReadOnly {
		t.Errorf("session mounts: %+v", s.Mounts)
	}
	for _, m := range res.Agent.Mounts {
		if m.Target == "/session" {
			t.Errorf("the record is mounted by the agent block too: %+v", res.Agent.Mounts)
		}
	}
}
