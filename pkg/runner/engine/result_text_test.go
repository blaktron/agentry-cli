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
	if c.inTok != 10 || c.outTok != 5 {
		t.Fatalf("usage = %d/%d", c.inTok, c.outTok)
	}

	// A whole-message shape replaces whatever was streamed before it.
	c.consume(strings.NewReader(`{"type":"assistant_message","message":{"content":"whole"}}` + "\n"))
	if c.lastText != "whole" {
		t.Fatalf("whole message: %q", c.lastText)
	}
}
