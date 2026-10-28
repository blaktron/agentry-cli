package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

func TestToolCallAsText(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       bool
	}{
		{"qwen closers", "The `fetch` tool requires `auth.env`. Let me try that approach again.\n</parameter>\n</function>\n</tool_call>\n", true},
		{"qwen whole call", "Fetching.\n<tool_call>\n<function=fetch>\n<parameter=url>\nhttps://api.osv.dev/v1/query\n</parameter>\n</function>\n</tool_call>", true},
		{"hermes", "<tool_call>\n{\"name\": \"fetch\", \"arguments\": {\"url\": \"https://example.com\"}}\n</tool_call>", true},
		{"cut off", "Next I fetch the advisory.\n<tool_call>\n{\"name\": \"fetch\", \"argu", true},
		{"cut off function", "Next.\n<function=fetch>\n<parameter=url>\nhttps://exa", true},
		{"upper case", "Done.\n</TOOL_CALL>", true},
		{"answer", "# Report\n\nNo reachable vulnerabilities were found.", false},
		{"fenced example", "A Qwen tool call looks like this:\n```\n<tool_call>\n<function=fetch>\n</function>\n</tool_call>\n```", false},
		{"fenced unclosed", "It starts:\n```\n<tool_call>\n```", false},
		{"inline mention", "The model wrote <tool_call> in its reply, which the server parses.", false},
		{"markup then answer", "<tool_call>{}</tool_call>\nThe answer is 4.", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolCallAsText(tc.text); got != tc.want {
				t.Fatalf("toolCallAsText = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestErrToolCallAsTextIsAHarnessFailureWithItsReason(t *testing.T) {
	err := errToolCallAsText()
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitHarness {
		t.Fatalf("class: %v", err)
	}
	if r := receipt.ReasonOf(err); r == nil || r.Code != receipt.ReasonToolCallAsText {
		t.Fatalf("reason: %+v", r)
	}
}

// The shape of a real run on a local model server: the model wrote its next
// fetch in its own markup, the server passed it through as text, and the
// harness ended the run on it. The run fails at its output with the reason,
// and the text stays its result.
func TestRunRefusesAToolCallWrittenAsText(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	leaked := "Let me try that approach again.\n</parameter>\n</function>\n</tool_call>"
	line, _ := json.Marshal(map[string]any{"type": "agent_choice", "message": map[string]any{"content": leaked}})
	f.Foreground["-agent"] = &dockerx.FakeForeground{Stdout: strings.Join([]string{
		`{"type":"tool_call","tool":"fetch"}`,
		string(line),
		`{"type":"stream_stopped","usage":{"input_tokens":100,"output_tokens":40,"cost":0.05}}`,
	}, "\n") + "\n"}
	env, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitHarness || !strings.Contains(err.Error(), "tool call written as text") {
		t.Fatalf("want exit %d naming the tool call, got %v", ExitHarness, err)
	}
	if env == nil || env.Status == "ok" || env.Stage != receipt.StageOutput || env.Reason == nil || env.Reason.Code != receipt.ReasonToolCallAsText {
		t.Fatalf("envelope = %+v", env)
	}
	var result string
	if err := json.Unmarshal(env.Result, &result); err != nil || result != leaked {
		t.Fatalf("result = %s", env.Result)
	}
}
