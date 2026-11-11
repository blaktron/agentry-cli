package llm

import (
	"regexp"
	"strconv"
	"strings"
)

// overflowWindows find the window a server states when it refuses a request
// as too long. Each has one group, the window in tokens.
var overflowWindows = []*regexp.Regexp{
	// OpenAI, vLLM: "This model's maximum context length is 32768 tokens."
	regexp.MustCompile(`(?i)maximum context length is ([\d,]+) tokens`),
	// llama.cpp: "... exceeds the available context size (2048 tokens)".
	regexp.MustCompile(`(?i)available context size \(([\d,]+) tokens\)`),
	// llama.cpp's error body ("n_ctx":2048) and its log wording, which LM
	// Studio passes on ("n_keep: 5746 >= n_ctx: 4096").
	regexp.MustCompile(`"?n_ctx"?\s*:\s*([\d,]+)`),
	// Anthropic: "prompt is too long: 213462 tokens > 200000 maximum".
	regexp.MustCompile(`(?i)tokens\s*>\s*([\d,]+) maximum`),
	// Gemini: "... exceeds the maximum number of tokens allowed (1048576)".
	regexp.MustCompile(`(?i)maximum number of tokens allowed \(([\d,]+)\)`),
}

// The least and the most a stated window is believed, so a stray number in a
// server's message cannot pose as one.
const (
	minStatedWindow = 256
	maxStatedWindow = 100_000_000
)

// StatedWindow is the window, in tokens, a refusal's words state, or zero.
func StatedWindow(message string) int64 {
	for _, re := range overflowWindows {
		m := re.FindStringSubmatch(message)
		if m == nil {
			continue
		}
		n, err := strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
		if err == nil && n >= minStatedWindow && n <= maxStatedWindow {
			return n
		}
	}
	return 0
}

// withWindow sets e's window from its message when e is the refusal of a
// conversation as too long.
func withWindow(e *Error) *Error {
	if e.Class == Overflow && e.Window == 0 {
		e.Window = StatedWindow(e.Message)
	}
	return e
}
