package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/harness/proto"
	"github.com/blaktron/agentry/pkg/manifest"
)

// gatedHarness plays the runner's own harness as the engine drives it: it
// reads the runner's commands from its stdin, writes the harness's events,
// and asks before it ends a turn. It plays the tools server's half too, the
// copy of the checklist and the calls ledger in the run's receipts.
type gatedHarness struct {
	// script is one start of the harness.
	script   func(h *gatedIO) int
	receipts string

	mu       sync.Mutex
	starts   int
	commands []proto.Command
}

// gatedIO is one start's stdin and stdout.
type gatedIO struct {
	h   *gatedHarness
	in  *bufio.Reader
	out io.Writer
	seq int64
}

func (g *gatedHarness) handler(_ context.Context, _ dockerx.RunSpec, stdin io.Reader, stdout, _ io.Writer) (int, error) {
	g.mu.Lock()
	g.starts++
	g.mu.Unlock()
	// The tools server's first line: it could write the run's receipts.
	appendLines(filepath.Join(g.receipts, CallsLedgerFile), []string{startLine})
	return g.script(&gatedIO{h: g, in: bufio.NewReader(stdin), out: stdout}), nil
}

func (g *gatedHarness) snapshot() (int, []proto.Command) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.starts, append([]proto.Command(nil), g.commands...)
}

// read is the runner's next command, false once its stdin has ended.
func (io *gatedIO) read() (proto.Command, bool) {
	line, err := io.in.ReadBytes('\n')
	if err != nil {
		return proto.Command{}, false
	}
	var c proto.Command
	if json.Unmarshal(line, &c) != nil {
		return proto.Command{}, false
	}
	io.h.mu.Lock()
	io.h.commands = append(io.h.commands, c)
	io.h.mu.Unlock()
	return c, true
}

func (io *gatedIO) emit(ev proto.Event) {
	io.seq++
	ev.V, ev.Seq = proto.Version, io.seq
	line, _ := json.Marshal(ev)
	_, _ = io.out.Write(append(line, '\n'))
}

// tool is one tool call and its result.
func (io *gatedIO) tool(name string) {
	io.emit(proto.Event{Type: proto.TypeToolCall, CallID: name, Tool: name, Arguments: json.RawMessage(`{}`)})
	io.emit(proto.Event{Type: proto.TypeToolResult, CallID: name, Tool: name, Text: "ok"})
	appendLines(filepath.Join(io.h.receipts, CallsLedgerFile), []string{`{"tool":"` + name + `","ok":true}`})
}

// answer is the model's reply with no call: its text, when it has any, and
// the request's token count.
func (io *gatedIO) answer(text string) {
	if text != "" {
		io.emit(proto.Event{Type: proto.TypeText, Text: text})
	}
	io.emit(proto.Event{Type: proto.TypeUsage, Usage: &proto.Usage{Input: 100, Output: 10, ContextTokens: 100}})
}

// ending asks whether the turn ends, and returns the message to go on with,
// or "" when it does.
func (io *gatedIO) ending() string {
	io.emit(proto.Event{Type: proto.TypeTurnEnding})
	if c, ok := io.read(); ok && c.Type == proto.CmdContinue {
		return c.Text
	}
	return ""
}

func (io *gatedIO) end(text string) int {
	io.emit(proto.Event{Type: proto.TypeTurnEnd, Reason: proto.EndAnswered, Text: text})
	return 0
}

// checklist writes the tools server's copy of a list of three steps, the
// first done of them done.
func (io *gatedIO) checklist(done int) {
	var b strings.Builder
	b.WriteString("# Checklist\n\n")
	for i, s := range []string{"take the inventory", "query each package", "write the report"} {
		box := "- [ ] "
		if i < done {
			box = "- [x] "
		}
		b.WriteString(box + s + "\n")
	}
	_ = os.WriteFile(filepath.Join(io.h.receipts, ChecklistCopyFile), []byte(b.String()), 0o644)
}

// gatedFixture is a release's one-shot run on the runner's own harness,
// whose agent container is g.
func gatedFixture(t *testing.T, g *gatedHarness) (Params, *dockerx.Fake) {
	t.Helper()
	shortFinishMargin(t)
	shortResumeWaits(t)
	shortCapacityWait(t)
	p, f := fixture(t)
	m, err := manifest.Parse([]byte(engineManifest))
	if err != nil {
		t.Fatal(err)
	}
	p.PlanDir = filepath.Join(t.TempDir(), "plan")
	p.Plan, err = compile.Compile(compile.Inputs{
		Manifest: m, Digest: "sha256:feedface", Source: "local",
		WorkspaceDir: p.WorkspaceSrc, Policy: p.Policy, PolicySHA: "sha", RunnerImage: "agentry-runner:test",
		AdapterName: adapter.AgentryHarnessAdapterName,
	}, p.PlanDir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Adapter, err = adapter.Get(adapter.AgentryHarnessAdapterName); err != nil {
		t.Fatal(err)
	}
	happyFake(f)
	p.EndBlocked = true
	p.Task = strings.NewReader("work the three steps")
	g.receipts = filepath.Join(p.OutDir, "receipts")
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: g.handler}
	return p, f
}

// A harness that asks before it ends a turn is told of its open steps on
// its running process: it is started once, the message is the finish
// check's, the ask is recorded as one, and the answer after it is the run's.
func TestAHarnessThatAsksIsToldOfItsOpenStepsWithoutBeingStartedAgain(t *testing.T) {
	g := &gatedHarness{script: func(h *gatedIO) int {
		if c, ok := h.read(); !ok || c.Type != proto.CmdPrompt {
			return 2
		}
		h.tool("read_input")
		h.checklist(1)
		h.answer("One step is done.")
		if h.ending() == "" {
			return h.end("One step is done.")
		}
		h.tool("fs_read")
		h.tool("write_report")
		h.checklist(3)
		h.answer("All three steps are done.")
		if h.ending() != "" {
			return 2
		}
		return h.end("All three steps are done.")
	}}
	p, f := gatedFixture(t, g)
	var stderr bytes.Buffer
	p.Stderr = &stderr
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr.String())
	}
	starts, commands := g.snapshot()
	if starts != 1 {
		t.Fatalf("the harness was started %d times, want once", starts)
	}
	want := "Your checklist has open steps:\n- query each package (pending)\n- write the report (pending)\nContinue them, or drop each one with its reason and finish."
	if len(commands) != 3 || commands[0] != (proto.Command{Type: proto.CmdPrompt, Text: "work the three steps"}) ||
		commands[1] != (proto.Command{Type: proto.CmdContinue, Text: want}) || commands[2] != (proto.Command{Type: proto.CmdAccept}) {
		t.Fatalf("the harness was sent %+v", commands)
	}
	if string(env.Result) != `"All three steps are done."` {
		t.Fatalf("result %s", env.Result)
	}
	if env.Usage.Turns != 3 || env.Usage.InputTokens != 200 {
		t.Fatalf("usage %+v", env.Usage)
	}
	c := env.Checklist
	if c == nil || c.Ending != endingAllDone || len(c.FinishChecks) != 1 || c.FinishChecks[0].Open != 2 || c.FinishChecks[0].Message != want {
		t.Fatalf("checklist record %+v", c)
	}
	if len(env.Restarts) != 1 || env.Restarts[0].Reason != receipt.RestartFinishCheck || env.Restarts[0].Message != want {
		t.Fatalf("restarts %+v", env.Restarts)
	}
	if !strings.Contains(stderr.String(), "note: the agent finished with 2 checklist step(s) open; it was told to go on (1 of 2)") {
		t.Fatalf("the ask was not announced: %q", stderr.String())
	}
}

// It is asked twice at most, the second time as the last, and the answer
// after that stands with its steps open.
func TestAHarnessThatAsksIsAskedTwiceAtMost(t *testing.T) {
	g := &gatedHarness{script: func(h *gatedIO) int {
		h.read()
		h.tool("read_input")
		h.checklist(1)
		for n := 1; ; n++ {
			h.answer("Still one step.")
			if h.ending() == "" {
				return h.end("Still one step.")
			}
			if n > 5 {
				return 2
			}
		}
	}}
	p, f := gatedFixture(t, g)
	p.Stderr = io.Discard
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	starts, commands := g.snapshot()
	if starts != 1 || len(commands) != 4 || commands[1].Type != proto.CmdContinue || commands[2].Type != proto.CmdContinue || commands[3].Type != proto.CmdAccept {
		t.Fatalf("%d starts, commands %+v", starts, commands)
	}
	if strings.Contains(commands[1].Text, "last time") || !strings.HasSuffix(commands[2].Text, "This is the last time you are asked: name in your answer every step you leave open, and why.") {
		t.Fatalf("the two asks: %q / %q", commands[1].Text, commands[2].Text)
	}
	if c := env.Checklist; c == nil || len(c.FinishChecks) != 2 || len(c.Open) != 2 || len(env.Restarts) != 2 {
		t.Fatalf("checklist %+v, restarts %+v", c, env.Restarts)
	}
}

// An answer with no reply, or of reasoning alone, after a tool call is told
// to go on the same way, and what the model wrote before is not the result.
func TestAHarnessThatAsksIsToldToGoOnAfterAnEmptyAnswer(t *testing.T) {
	for name, reasoned := range map[string]bool{receipt.RestartEmptyAnswer: false, receipt.RestartReasoningOnly: true} {
		t.Run(name, func(t *testing.T) {
			g := &gatedHarness{script: func(h *gatedIO) int {
				h.read()
				h.emit(proto.Event{Type: proto.TypeText, Text: "I will read the input first."})
				h.tool("read_input")
				h.checklist(3)
				if reasoned {
					h.emit(proto.Event{Type: proto.TypeReasoning, Text: "The answer would be 42."})
				}
				h.answer("")
				more := h.ending()
				if more == "" {
					return h.end("")
				}
				h.answer("The answer is 42.")
				if h.ending() != "" {
					return 2
				}
				return h.end("The answer is 42.")
			}}
			p, f := gatedFixture(t, g)
			var stderr bytes.Buffer
			p.Stderr = &stderr
			env, err := Run(context.Background(), f, p)
			if err != nil {
				t.Fatalf("run failed: %v\n%s", err, stderr.String())
			}
			starts, commands := g.snapshot()
			if starts != 1 || len(commands) != 3 || commands[1] != (proto.Command{Type: proto.CmdContinue, Text: emptyAnswerMessage}) {
				t.Fatalf("%d starts, commands %+v", starts, commands)
			}
			if string(env.Result) != `"The answer is 42."` {
				t.Fatalf("result %s", env.Result)
			}
			if len(env.Restarts) != 1 || env.Restarts[0].Reason != name || env.Restarts[0].Message != emptyAnswerMessage {
				t.Fatalf("restarts %+v", env.Restarts)
			}
			if !strings.Contains(stderr.String(), "it was told to go on (1 of 2)") {
				t.Fatalf("stderr: %q", stderr.String())
			}
		})
	}
}

// A run that ended blocked, one with every step done, and one with too
// little time left are let end.
func TestAHarnessThatAsksIsLetEnd(t *testing.T) {
	script := func(done int, blocked bool) func(h *gatedIO) int {
		return func(h *gatedIO) int {
			h.read()
			h.tool("read_input")
			h.checklist(done)
			if blocked {
				appendLines(filepath.Join(h.h.receipts, CallsLedgerFile), []string{`{"tool":"end_blocked","ok":true,"blocked":"the input is missing"}`})
			}
			h.answer("That is all.")
			if h.ending() != "" {
				return 2
			}
			return h.end("That is all.")
		}
	}
	cases := map[string]struct {
		done    int
		blocked bool
		margin  time.Duration
	}{
		"every step done":      {done: 3},
		"blocked":              {done: 1, blocked: true},
		"too little time left": {done: 1, margin: time.Hour},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := &gatedHarness{script: script(tc.done, tc.blocked)}
			p, f := gatedFixture(t, g)
			if tc.margin > 0 {
				was := finishCheckMargin
				finishCheckMargin = tc.margin
				t.Cleanup(func() { finishCheckMargin = was })
			}
			p.Stderr = io.Discard
			env, _ := Run(context.Background(), f, p)
			starts, commands := g.snapshot()
			if starts != 1 || len(commands) != 2 || commands[1].Type != proto.CmdAccept {
				t.Fatalf("%d starts, commands %+v", starts, commands)
			}
			if env == nil || len(env.Restarts) != 0 {
				t.Fatalf("envelope %+v", env)
			}
		})
	}
}

// A harness that exits without reading its stdin, or without asking, ends
// its run: the engine does not wait on a stdin nobody reads.
func TestAHarnessThatExitsWithItsStdinUnreadEndsTheRun(t *testing.T) {
	g := &gatedHarness{script: func(h *gatedIO) int {
		h.emit(proto.Event{Type: proto.TypeError, Class: proto.ClassGateway, Text: "tool gateway answered HTTP 401: no bearer"})
		return 1
	}}
	p, f := gatedFixture(t, g)
	// Larger than a pipe holds, so the prompt is still being written when
	// the harness goes.
	p.Task = strings.NewReader(strings.Repeat("a long task ", 200_000))
	p.Stderr = io.Discard
	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), f, p)
		done <- err
	}()
	select {
	case err := <-done:
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != ExitHarness || !strings.Contains(err.Error(), "the tool gateway failed") {
			t.Fatalf("the run's error: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the run waited on a harness that had gone")
	}
}

// What the harness did to carry a long run is counted in the receipt, and
// text streamed by a request that failed and was sent again is no part of
// the answer.
func TestALongRunsEventsAreCountedInTheReceipt(t *testing.T) {
	g := &gatedHarness{script: func(h *gatedIO) int {
		h.read()
		h.emit(proto.Event{Type: proto.TypeWarning, Code: proto.WarnColdClear, Count: 4, Reason: "cold cache"})
		h.tool("read_input")
		h.checklist(3)
		h.emit(proto.Event{Type: proto.TypeWarning, Code: proto.WarnLoopNudge, Tool: "read_input", Count: 5, Text: "5 identical rounds of calls (read_input) in a row"})
		h.emit(proto.Event{Type: proto.TypeWarning, Code: proto.WarnLengthRecovery, Reason: proto.CutToolCall, Count: 1})
		h.emit(proto.Event{Type: proto.TypeWarning, Code: proto.WarnCompaction, Reason: proto.CompactionThreshold})
		h.emit(proto.Event{Type: proto.TypeWarning, Code: proto.WarnCompaction, Reason: proto.CompactionOverflow})
		h.emit(proto.Event{Type: proto.TypeWarning, Code: proto.WarnCompaction, Reason: proto.CompactionOverflow})
		h.emit(proto.Event{Type: proto.TypeUsage, Usage: &proto.Usage{ContextTokens: 900, Compacted: true}})
		h.emit(proto.Event{Type: proto.TypeText, Text: "Half of an ans"})
		h.emit(proto.Event{Type: proto.TypeWarning, Code: proto.WarnRetry, Class: proto.ClassTransport, Count: 1})
		h.answer("The whole answer.")
		h.ending()
		return h.end("The whole answer.")
	}}
	p, f := gatedFixture(t, g)
	p.Stderr = io.Discard
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	u := env.Usage
	if u.LoopNudges != 1 || u.LengthRecoveries != (receipt.LengthRecoveries{ToolCall: 1}) || u.ResultClears != 1 || u.Compactions != (receipt.Compactions{Threshold: 1, Overflow: 2}) {
		t.Fatalf("usage %+v", u)
	}
	// The compaction's report of the conversation's size is no request.
	if u.InputTokens != 100 || u.OutputTokens != 10 || u.Turns != 1 {
		t.Fatalf("usage %+v", u)
	}
	if string(env.Result) != `"The whole answer."` {
		t.Fatalf("result %s", env.Result)
	}
}
