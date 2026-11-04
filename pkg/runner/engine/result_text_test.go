package engine

import (
	"io"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
)

// The result is the assistant's LAST MESSAGE in full, assembled from the
// streamed deltas docker-agent emits, not the last delta; reasoning chunks
// and text before a tool call do not leak into it (the sequence below is
// the shape of a real run's receipt).
func TestCollectorAssemblesStreamedFinalMessage(t *testing.T) {
	ad, err := adapter.Get("docker-agent")
	if err != nil {
		t.Fatal(err)
	}
	c := &eventCollector{adapter: ad, events: io.Discard}
	c.consume(strings.NewReader(strings.Join([]string{
		`{"type":"stream_started"}`,
		`{"type":"agent_choice_reasoning","content":"Let me look"}`,
		`{"type":"agent_choice","content":"Looking"}`,
		`{"type":"agent_choice","content":" first."}`,
		`{"type":"partial_tool_call","tool_call":{"id":"c1","function":{"name":"list_inputs","arguments":"{"}}}`,
		`{"type":"tool_call","tool_call":{"id":"c1","function":{"name":"list_inputs","arguments":"{}"}}}`,
		`{"type":"tool_call_response","tool_call_id":"c1","response":"tickets.csv"}`,
		`{"type":"agent_choice_reasoning","content":"Now the answer"}`,
		`{"type":"agent_choice","content":"{"}`,
		`{"type":"agent_choice","content":"\n  \"tickets\": []"}`,
		`{"type":"agent_choice","content":"\n}"}`,
		`{"type":"message_added"}`,
		`{"type":"token_usage","usage":{"input_tokens":10,"output_tokens":5,"cost":0}}`,
		// Tokens add up across requests; the cost is the running total, so
		// the highest one stands.
		`{"type":"token_usage","usage":{"input_tokens":7,"output_tokens":3,"cost":0.05}}`,
		`{"type":"token_usage","usage":{"input_tokens":1,"output_tokens":1,"cost":0.03}}`,
		`{"type":"stream_stopped","reason":"normal"}`,
	}, "\n") + "\n"))
	if want := "{\n  \"tickets\": []\n}"; c.lastText != want {
		t.Fatalf("lastText = %q, want %q", c.lastText, want)
	}
	if c.turns != 1 {
		t.Fatalf("turns = %d, want 1", c.turns)
	}
	if c.answerInReasoning() {
		t.Fatal("answerInReasoning, though the answer came after the reasoning")
	}
	if c.inTok != 18 || c.outTok != 9 || c.costUSD != 0.05 {
		t.Fatalf("usage = %d/%d at $%v", c.inTok, c.outTok, c.costUSD)
	}

	// A whole-message shape replaces whatever was streamed before it.
	c.consume(strings.NewReader(`{"type":"assistant_message","message":{"content":"whole"}}` + "\n"))
	if c.lastText != "whole" {
		t.Fatalf("whole message: %q", c.lastText)
	}

	// A whole message ends the one in progress: chunks after it start afresh.
	c.consume(strings.NewReader(strings.Join([]string{
		`{"type":"agent_choice","content":"partial "}`,
		`{"type":"assistant_message","message":{"content":"whole"}}`,
		`{"type":"agent_choice","content":" more"}`,
	}, "\n") + "\n"))
	if c.lastText != " more" {
		t.Fatalf("chunks after a whole message: %q", c.lastText)
	}

	// A usage report ends the message in progress: the chunks after it are a
	// message of their own.
	c.consume(strings.NewReader(strings.Join([]string{
		`{"type":"agent_choice","content":"first message"}`,
		`{"type":"token_usage","usage":{"input_tokens":1,"output_tokens":1,"cost":0}}`,
		`{"type":"agent_choice","content":"second "}`,
		`{"type":"agent_choice","content":"message"}`,
	}, "\n") + "\n"))
	if c.lastText != "second message" {
		t.Fatalf("after a usage report: %q", c.lastText)
	}

	// A run paid by a person's plan counts tokens and never dollars.
	paid := &eventCollector{adapter: ad, events: io.Discard, signIn: true}
	paid.consume(strings.NewReader(`{"type":"token_usage","usage":{"input_tokens":4,"output_tokens":2,"cost":0.05}}` + "\n"))
	if u := paid.usage(); u.CostUSD != 0 || u.InputTokens != 4 || u.OutputTokens != 2 {
		t.Fatalf("a plan-paid run's usage = %+v", u)
	}
}

// Two shapes seen in real runs. One called its tools, then streamed the
// answer as reasoning and sent no message; the other reasoned, called
// nothing, and docker-agent warned that it "produced only reasoning and
// no reply". Both end on reasoning, both count their tool calls as
// turns, and neither keeps any of the reasoning as text.
func TestCollectorNotesAnswerInReasoning(t *testing.T) {
	ad, err := adapter.Get("docker-agent")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		lines []string
		turns int
	}{
		{"after its tools", []string{
			`{"type":"stream_started"}`,
			`{"type":"agent_choice_reasoning","content":"List the inputs"}`,
			`{"type":"partial_tool_call","tool_call":{"id":"c1","function":{"name":"list_inputs","arguments":"{"}}}`,
			`{"type":"message_added"}`,
			`{"type":"token_usage","usage":{"input_tokens":7000,"output_tokens":40,"cost":0}}`,
			`{"type":"tool_call","tool_call":{"id":"c1","function":{"name":"list_inputs","arguments":"{}"}}}`,
			`{"type":"hook_started"}`,
			`{"type":"hook_finished"}`,
			`{"type":"tool_call_response","tool_call_id":"c1","response":"expenses.csv"}`,
			`{"type":"tool_call","tool_call":{"id":"c2","function":{"name":"write_report","arguments":"{}"}}}`,
			`{"type":"tool_call_response","tool_call_id":"c2","response":"ok"}`,
			`{"type":"message_added"}`,
			`{"type":"toolset_info","available_tools":13,"loading":false}`,
			`{"type":"agent_choice_reasoning","content":"{\"checks\":{"}`,
			`{"type":"agent_choice_reasoning","content":"\"base\":\"EUR\"}}"}`,
			`{"type":"token_usage","usage":{"input_tokens":10812,"output_tokens":382,"context_length":11194,"context_limit":0,"cost":0}}`,
			`{"type":"stream_stopped","reason":"normal"}`,
		}, 2},
		{"with no tool call", []string{
			`{"type":"stream_started"}`,
			`{"type":"agent_choice_reasoning","content":"I will list the inputs"}`,
			`{"type":"token_usage","usage":{"input_tokens":7157,"output_tokens":11,"cost":0}}`,
			`{"type":"warning","message":"Model openai/qwen3.8-flash-next produced only reasoning and no reply (stop reason: stop). Thinking-mode models can emit reasoning tokens without a final answer; the reasoning is not used as the response."}`,
			`{"type":"stream_stopped","reason":"normal"}`,
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &eventCollector{adapter: ad, events: io.Discard}
			c.consume(strings.NewReader(strings.Join(tc.lines, "\n") + "\n"))
			if !c.answerInReasoning() {
				t.Fatal("answerInReasoning unset after a stream that ended on reasoning")
			}
			if c.lastText != "" {
				t.Fatalf("lastText = %q, want the reasoning kept out", c.lastText)
			}
			if got := c.usage().Turns; got != tc.turns {
				t.Fatalf("turns = %d, want %d", got, tc.turns)
			}
		})
	}
}

// Text, then trailing reasoning, is an answer: a failed check on it reads as
// what it is, not as an answer left in the reasoning. Text before a tool
// call is no answer to the reasoning after it.
func TestCollectorTextThenReasoningIsAnAnswer(t *testing.T) {
	ad, err := adapter.Get("docker-agent")
	if err != nil {
		t.Fatal(err)
	}
	c := &eventCollector{adapter: ad, events: io.Discard}
	c.consume(strings.NewReader(strings.Join([]string{
		`{"type":"agent_choice","content":"{\"answer\":\"x\"}"}`,
		`{"type":"agent_choice_reasoning","content":"done"}`,
	}, "\n") + "\n"))
	if c.answerInReasoning() {
		t.Fatal("text then reasoning read as an answer left in the reasoning")
	}
	// Whitespace alone is no answer.
	w := &eventCollector{adapter: ad, events: io.Discard}
	w.consume(strings.NewReader(strings.Join([]string{
		`{"type":"agent_choice","content":"\n  "}`,
		`{"type":"agent_choice_reasoning","content":"{\"answer\":1}"}`,
	}, "\n") + "\n"))
	if !w.answerInReasoning() {
		t.Fatal("whitespace before the reasoning read as an answer")
	}
	c.consume(strings.NewReader(strings.Join([]string{
		`{"type":"tool_call","tool_call":{"id":"c1","function":{"name":"list_inputs","arguments":"{}"}}}`,
		`{"type":"agent_choice_reasoning","content":"{\"answer\":1}"}`,
	}, "\n") + "\n"))
	if !c.answerInReasoning() {
		t.Fatal("reasoning after a tool call, with the earlier text, read as answered")
	}
}
