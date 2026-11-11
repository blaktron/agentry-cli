package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// Two answers the harness took out at the output limit leave the run going:
// it finishes on its own answer, the receipt counts both by what was cut,
// and the harness is started once.
func TestCutAnswersTheHarnessRecoveredFromFinishTheRun(t *testing.T) {
	h := &endHarness{script: []endStart{{tools: 1, cuts: []string{"answer", "tool call"}, end: "answer"}}}
	p, f := endFixture(t, h)
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if starts := h.snapshot(); len(starts) != 1 || len(env.Restarts) != 0 {
		t.Fatalf("%d starts, restarts %+v", len(starts), env.Restarts)
	}
	if want := (receipt.LengthRecoveries{Answer: 1, ToolCall: 1}); env.Usage.LengthRecoveries != want {
		t.Fatalf("lengthRecoveries %+v, want %+v", env.Usage.LengthRecoveries, want)
	}
	if strings.Contains(string(env.Result), "the same line again") {
		t.Fatalf("the cut text is in the result: %s", env.Result)
	}
}

// A run whose last answer the output limit cut off fails with that reason,
// and the harness is not started again on it: the same conversation would
// meet the same limit.
func TestARunThatEndsOnACutAnswerFailsAndIsNotStartedAgain(t *testing.T) {
	for name, sc := range map[string]endStart{
		"after two recoveries":     {tools: 1, cuts: []string{"answer", "reasoning"}, end: "cut"},
		"on a harness without any": {tools: 1, end: "cut"},
	} {
		t.Run(name, func(t *testing.T) {
			h := &endHarness{script: []endStart{sc, {end: "answer"}}}
			p, f := endFixture(t, h)
			env, err := Run(context.Background(), f, p)
			var ce *ClassifiedError
			if !errors.As(err, &ce) || ce.Code != ExitHarness || !strings.Contains(err.Error(), "cut off at its output limit") {
				t.Fatalf("err %v, want the cut answer", err)
			}
			if r := receipt.ReasonOf(err); r == nil || r.Code != receipt.ReasonAnswerCut {
				t.Fatalf("reason %+v, want answer-cut", r)
			}
			if starts := h.snapshot(); len(starts) != 1 {
				t.Fatalf("the harness started %d times, want 1", len(starts))
			}
			if env != nil && env.Usage.LengthRecoveries.Total() != len(sc.cuts) {
				t.Fatalf("lengthRecoveries %+v, want %d", env.Usage.LengthRecoveries, len(sc.cuts))
			}
		})
	}
}

// A recovery clears the text streamed before it, which was the cut answer,
// and what the model had reasoned or answered since its last tool call; the
// counts carry over a restart of the harness.
func TestCollectorDropsTheCutTextOnARecovery(t *testing.T) {
	ad, err := adapter.Get("docker-agent")
	if err != nil {
		t.Fatal(err)
	}
	c := &eventCollector{adapter: ad, events: io.Discard, base: receipt.Usage{LengthRecoveries: receipt.LengthRecoveries{Reasoning: 1}}}
	c.consume(strings.NewReader(strings.Join([]string{
		`{"type":"tool_call","tool_call":{"id":"c1","function":{"name":"read_file","arguments":"{}"}}}`,
		`{"type":"agent_choice_reasoning","content":"thinking"}`,
		`{"type":"agent_choice","content":"the same line again"}`,
		`{"type":"warning","message":"Length recovery 1: an answer cut off at the output limit (answer) was taken out of the conversation and the agent was told why."}`,
	}, "\n") + "\n"))
	if c.lastText != "" || c.answered || c.reasoned {
		t.Fatalf("after the recovery: lastText %q, answered %v, reasoned %v", c.lastText, c.answered, c.reasoned)
	}
	if got := c.emptyEnd(); got != receipt.RestartEmptyAnswer {
		t.Fatalf("an end right after a recovery reads as %q, want an empty answer", got)
	}
	c.consume(strings.NewReader(`{"type":"agent_choice","content":"short final answer"}` + "\n"))
	if c.lastText != "short final answer" {
		t.Fatalf("lastText %q", c.lastText)
	}
	if want := (receipt.LengthRecoveries{Answer: 1, Reasoning: 1}); c.usage().LengthRecoveries != want {
		t.Fatalf("usage %+v, want %+v", c.usage().LengthRecoveries, want)
	}
	if c.answerCut {
		t.Fatal("a recovery is not a cut end")
	}
	if b, _ := json.Marshal(receipt.Usage{}); strings.Contains(string(b), "lengthRecoveries") {
		t.Fatalf("a run with no recovery records none: %s", b)
	}
	b, _ := json.Marshal(receipt.Usage{LengthRecoveries: receipt.LengthRecoveries{ToolCall: 2}})
	if !strings.Contains(string(b), `"lengthRecoveries":{"toolCall":2}`) {
		t.Fatalf("receipt usage %s", b)
	}
}
