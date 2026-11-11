package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// endStart scripts one start of endHarness: how many tool calls it makes,
// what its last turn is, and how it exits.
type endStart struct {
	tools int
	// end is the last turn: "answer", "empty", "reasoning" or "warning"
	// (an empty turn the harness reports with its own warning).
	end  string
	exit int
	// errText is the harness's stderr on a failing exit.
	errText string
}

// endHarness plays a docker-agent that makes the scripted tool calls, each
// recorded under the conversation it is started with, and ends as the
// script says. A start given the task begins its conversation again; a start
// given anything else continues it.
type endHarness struct {
	script []endStart

	mu       sync.Mutex
	done     map[string]int
	starts   []harnessStart
	executed map[string]int
	cost     map[string]float64
}

func (h *endHarness) handler(_ context.Context, r dockerx.RunSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	msg, _ := io.ReadAll(stdin)
	conversation := ""
	for i, a := range r.Argv {
		if a == "--session" && i+1 < len(r.Argv) {
			conversation = r.Argv[i+1]
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done == nil {
		h.done, h.executed, h.cost = map[string]int{}, map[string]int{}, map[string]float64{}
	}
	n := len(h.starts)
	h.starts = append(h.starts, harnessStart{conversation: conversation, message: string(msg)})
	sc := endStart{end: "answer"}
	if n < len(h.script) {
		sc = h.script[n]
	}
	for i := 0; i < sc.tools; i++ {
		h.done[conversation]++
		h.executed[fmt.Sprintf("%s/%d", conversation, h.done[conversation])]++
		_, _ = fmt.Fprintf(stdout, `{"type":"tool_call","tool":"read_input","step":%d}`+"\n", h.done[conversation])
	}
	h.cost[conversation] += 0.01
	_, _ = fmt.Fprintf(stdout, `{"type":"stream_stopped","usage":{"input_tokens":100,"output_tokens":10,"cost":%g}}`+"\n", h.cost[conversation])
	switch sc.end {
	case "answer":
		_, _ = fmt.Fprintf(stdout, `{"type":"agent_choice","message":{"content":"answer %d"}}`+"\n", n)
	case "reasoning":
		_, _ = fmt.Fprintln(stdout, `{"type":"agent_choice_reasoning","content":"thinking about it"}`)
	case "warning":
		_, _ = fmt.Fprintln(stdout, `{"type":"warning","message":"the model stopped without a final reply after using tools"}`)
	}
	if sc.errText != "" {
		_, _ = fmt.Fprintln(stderr, sc.errText)
	}
	return sc.exit, nil
}

func (h *endHarness) snapshot() []harnessStart {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]harnessStart(nil), h.starts...)
}

func shortFinishMargin(t *testing.T) {
	t.Helper()
	was := finishCheckMargin
	finishCheckMargin = time.Millisecond
	t.Cleanup(func() { finishCheckMargin = was })
}

func endFixture(t *testing.T, h *endHarness) (Params, *dockerx.Fake) {
	t.Helper()
	shortResumeWaits(t)
	shortCapacityWait(t)
	shortFinishMargin(t)
	p, f := fixture(t)
	happyFake(f)
	p.Task = strings.NewReader("do the work")
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	return p, f
}

// Each early end starts the harness once more on its conversation with the
// message for that end, records the reason, sums the usage, and runs no
// tool call twice.
func TestAnEarlyEndAfterAToolCallStartsTheHarnessAgain(t *testing.T) {
	for name, tc := range map[string]struct {
		first   endStart
		reason  string
		message string
		note    string
	}{
		"empty answer":    {endStart{tools: 1, end: "empty"}, receipt.RestartEmptyAnswer, emptyAnswerMessage, "an empty answer"},
		"reasoning only":  {endStart{tools: 1, end: "reasoning"}, receipt.RestartReasoningOnly, emptyAnswerMessage, "an answer of reasoning only"},
		"harness warning": {endStart{tools: 1, end: "warning"}, receipt.RestartEmptyAnswer, emptyAnswerMessage, "an empty answer"},
		"loop exit":       {endStart{tools: 1, end: "empty", exit: 1, errText: "Error: loop_detected: the same call repeated"}, receipt.RestartLoopDetected, loopDetectedMessage, "a repeated call ended it"},
	} {
		t.Run(name, func(t *testing.T) {
			h := &endHarness{script: []endStart{tc.first, {tools: 1, end: "answer"}}}
			p, f := endFixture(t, h)
			var stderr bytes.Buffer
			p.Stderr = &stderr
			env, err := Run(context.Background(), f, p)
			if err != nil {
				t.Fatalf("run failed: %v\n%s", err, stderr.String())
			}
			starts := h.snapshot()
			if len(starts) != 2 {
				t.Fatalf("the harness started %d times, want 2: %+v", len(starts), starts)
			}
			if starts[0].message != "do the work" && !strings.Contains(starts[0].message, "do the work") {
				t.Fatalf("first start's message %q", starts[0].message)
			}
			if starts[1].conversation != starts[0].conversation || starts[1].message != tc.message {
				t.Fatalf("second start %+v, want conversation %q and message %q", starts[1], starts[0].conversation, tc.message)
			}
			for step, n := range h.executed {
				if n != 1 {
					t.Fatalf("tool call %s ran %d times", step, n)
				}
			}
			if len(env.Restarts) != 1 || env.Restarts[0].Reason != tc.reason || env.Restarts[0].Message != tc.message {
				t.Fatalf("restarts %+v, want one for %s", env.Restarts, tc.reason)
			}
			if env.Usage.Turns != 2 || env.Usage.InputTokens != 200 {
				t.Fatalf("usage %+v, want both starts' sum", env.Usage)
			}
			if env.Usage.CostUSD < 0.0199 || env.Usage.CostUSD > 0.0201 {
				t.Fatalf("cost %v, want 0.02", env.Usage.CostUSD)
			}
			if !strings.Contains(stderr.String(), "starting it again on its conversation (1 of 2)") {
				t.Fatalf("the restart was not announced: %q", stderr.String())
			}
			agentStderr, _ := os.ReadFile(filepath.Join(p.OutDir, "receipts", "agent-stderr.log"))
			if !strings.Contains(string(agentStderr), "after "+tc.note+" (1 of 2): ") {
				t.Fatalf("the harness's receipt does not name the restart:\n%s", agentStderr)
			}
		})
	}
}

// Before the first tool call an empty end changes nothing.
func TestAnEmptyEndBeforeTheFirstToolCallIsLeftAlone(t *testing.T) {
	h := &endHarness{script: []endStart{{tools: 0, end: "empty"}}}
	p, f := endFixture(t, h)
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if starts := h.snapshot(); len(starts) != 1 || len(env.Restarts) != 0 {
		t.Fatalf("%d starts, restarts %+v", len(starts), env.Restarts)
	}
}

// The budget is two starts for all reasons together: a third early end
// stands, and a finish check counts against the same two.
func TestEarlyEndRestartsShareTheFinishChecksBudget(t *testing.T) {
	t.Run("empty answers only", func(t *testing.T) {
		h := &endHarness{script: []endStart{{tools: 1, end: "empty"}, {end: "reasoning"}, {end: "empty"}, {end: "answer"}}}
		p, f := endFixture(t, h)
		env, err := Run(context.Background(), f, p)
		if err != nil {
			t.Fatalf("run failed: %v", err)
		}
		if starts := h.snapshot(); len(starts) != 1+finishChecks {
			t.Fatalf("%d starts, want %d", len(starts), 1+finishChecks)
		}
		if len(env.Restarts) != 2 || env.Restarts[0].Reason != receipt.RestartEmptyAnswer || env.Restarts[1].Reason != receipt.RestartReasoningOnly {
			t.Fatalf("restarts %+v", env.Restarts)
		}
	})
	t.Run("an empty answer and a loop exit", func(t *testing.T) {
		h := &endHarness{script: []endStart{{tools: 1, end: "empty"}, {end: "empty", exit: 1, errText: "loop detected"}, {end: "empty", exit: 1, errText: "loop detected"}}}
		p, f := endFixture(t, h)
		env, err := Run(context.Background(), f, p)
		if err == nil {
			t.Fatalf("a third loop exit must stand as the run's failure")
		}
		if starts := h.snapshot(); len(starts) != 3 {
			t.Fatalf("%d starts, want 3", len(starts))
		}
		if len(env.Restarts) != 2 || env.Restarts[1].Reason != receipt.RestartLoopDetected {
			t.Fatalf("restarts %+v", env.Restarts)
		}
	})
}

// A finish check that came first leaves one start for an empty answer, and
// not a second one.
func TestAFinishCheckAndAnEmptyAnswerSplitTheBudget(t *testing.T) {
	shortFinishMargin(t)
	rd := &runDirs{restarts: []receipt.Restart{{Reason: receipt.RestartFinishCheck}}}
	p, _ := fixture(t)
	out := agentOutcome{emptyAnswer: receipt.RestartEmptyAnswer, usage: receipt.Usage{Turns: 1}}
	if reason, msg := rd.earlyEndAsk(context.Background(), p, out); reason != receipt.RestartEmptyAnswer || msg != emptyAnswerMessage {
		t.Fatalf("one left: %q %q", reason, msg)
	}
	rd.restarts = append(rd.restarts, receipt.Restart{Reason: receipt.RestartEmptyAnswer})
	if reason, _ := rd.earlyEndAsk(context.Background(), p, out); reason != "" {
		t.Fatalf("none left, asked for %q", reason)
	}
}

// The turn and cost budgets count every start.
func TestEarlyEndRestartsCountInTheBudgets(t *testing.T) {
	h := &endHarness{script: []endStart{{tools: 2, end: "empty"}, {tools: 2, end: "answer"}}}
	p, f := endFixture(t, h)
	p.Budgets.MaxTurns = 3
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitBudget {
		t.Fatalf("err %v, want the turn budget", err)
	}
	h = &endHarness{script: []endStart{{tools: 1, end: "empty"}, {end: "answer"}}}
	p, f = endFixture(t, h)
	p.Budgets.MaxCostUSD = 0.015
	_, err = Run(context.Background(), f, p)
	if !errors.As(err, &ce) || ce.Code != ExitBudget || !strings.Contains(err.Error(), "cost budget") {
		t.Fatalf("err %v, want the cost budget", err)
	}
}

// An agent that ended blocked is not started again.
func TestABlockedRunIsNotStartedAgainOnAnEmptyAnswer(t *testing.T) {
	shortFinishMargin(t)
	p, _ := fixture(t)
	rd := &runDirs{callsLedger: filepath.Join(t.TempDir(), "calls.ndjson")}
	appendLines(rd.callsLedger, []string{startLine, `{"tool":"end_blocked","ok":true,"blocked":"missing"}`})
	out := agentOutcome{emptyAnswer: receipt.RestartEmptyAnswer, usage: receipt.Usage{Turns: 1}}
	if reason, _ := rd.earlyEndAsk(context.Background(), p, out); reason != "" {
		t.Fatalf("asked for %q after end_blocked", reason)
	}
}
