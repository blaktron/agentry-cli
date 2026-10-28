package engine

import (
	"fmt"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// toolCallClosers end a tool call a model wrote in its own markup (Hermes,
// Qwen and their kin): when the provider's server does not parse that markup
// into a call, it arrives as the message's text, and the harness, seeing no
// call, ends the run on it.
var toolCallClosers = []string{"</tool_call>", "</tool_calls>", "</function>", "</function_call>"}

// toolCallOpeners begin one; a call cut off before its closer still ends the
// message inside it.
var toolCallOpeners = []string{"<tool_call>", "<tool_calls>", "<function=", "<function_call>"}

// toolCallAsText reports whether text ends in a tool call written as text:
// closed by one of toolCallClosers, or opened at the start of a line by one
// of toolCallOpeners and never closed. A call shown in a code fence ends in
// the fence, and markup mentioned within a line opens no call.
func toolCallAsText(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	for _, c := range toolCallClosers {
		if strings.HasSuffix(t, c) {
			return true
		}
	}
	for _, o := range toolCallOpeners {
		i := strings.LastIndex(t, o)
		if i < 0 || (i > 0 && t[i-1] != '\n') {
			continue
		}
		closed := false
		for _, c := range toolCallClosers {
			if strings.Contains(t[i:], c) {
				closed = true
				break
			}
		}
		if !closed && !strings.Contains(t[i:], "```") {
			return true
		}
	}
	return false
}

// errToolCallAsText refuses a one-shot run whose final message is a tool
// call written as text: the agent had not finished, and its unparsed call is
// not an answer. The message stays the run's result and in the receipts.
func errToolCallAsText() error {
	return Classify(ExitHarness, receipt.WithReason(
		fmt.Errorf("the model's final message is a tool call written as text, which its provider did not take as a call, so the agent stopped before it finished (raw output preserved in receipts): the model's server must parse its tool calls (its chat template or tool-call setting), or use another model"),
		receipt.ReasonToolCallAsText, ""))
}
