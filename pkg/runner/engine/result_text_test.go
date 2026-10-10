package engine

import (
	"io"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
)

// The result is the assistant's LAST MESSAGE in full, assembled from the
// streamed deltas docker-agent emits, not the last delta; reasoning chunks
// and text before a tool call do not leak into it (agentry-cli#53 — the
// sequence below is the shape of the first dev-host run's receipt).
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
	if c.inTok != 10 || c.outTok != 5 {
		t.Fatalf("usage = %d/%d", c.inTok, c.outTok)
	}

	// A whole-message shape replaces whatever was streamed before it.
	c.consume(strings.NewReader(`{"type":"assistant_message","message":{"content":"whole"}}` + "\n"))
	if c.lastText != "whole" {
		t.Fatalf("whole message: %q", c.lastText)
	}
}

// agentry-cli#825: two shapes of production's runtime-smoke on 2026-10-10.
// Run d31be0ca called its tools, then streamed the answer as reasoning and
// sent no message; run fecd9e18 reasoned, called nothing, and docker-agent
// warned that it "produced only reasoning and no reply". Both end on
// reasoning, both count their tool calls as turns, and neither keeps any of
// the reasoning as text.
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
	c.consume(strings.NewReader(strings.Join([]string{
		`{"type":"tool_call","tool_call":{"id":"c1","function":{"name":"list_inputs","arguments":"{}"}}}`,
		`{"type":"agent_choice_reasoning","content":"{\"answer\":1}"}`,
	}, "\n") + "\n"))
	if !c.answerInReasoning() {
		t.Fatal("reasoning after a tool call, with the earlier text, read as answered")
	}
}
