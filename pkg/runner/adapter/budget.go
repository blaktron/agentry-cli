package adapter

import "github.com/blaktron/agentry-cli/runnerimage/toolsrv"

// Budget is the most one tool result may hold for a harness to pass it to
// the model whole: bytes and lines (plan input-sources.md §4, D1;
// agentry-cli#831). The compiler stamps it into the tools spec, and the tools
// server pages every result within it.
type Budget struct {
	Bytes int64
	Lines int
}

// resultBudgets is each adapter's measured tool-result limit, less a margin
// for what the harness adds around the text (RUNNER-DEVIATIONS DR-89 records
// how each was measured, from the pinned release's own code). Only
// docker-agent counts lines; every adapter keeps its line limit, which costs
// the others nothing but a smaller page of very short lines. A harness bump
// re-measures its row, as it re-records its conformance.
var resultBudgets = map[string]Budget{
	// docker-agent's always-on limit_large_tool_results hook: an MCP result
	// over 50 KiB or 2,000 lines is replaced by its last 50 KiB. Measured on
	// 1.128.0 and re-measured on 1.150.0 (agentry-cli#836), whose history
	// bounding (bound_tool_messages.go) cuts at the same 50 KiB.
	DefaultName: {Bytes: toolsrv.DefaultMaxResultBytes, Lines: toolsrv.DefaultMaxResultLines},
	// Claude Code 2.1.296 saves a tool result over 50,000 characters (UTF-16
	// code units, never more than its UTF-8 bytes) to a file the agent has no
	// tool to open, and gives the model the path; an input tool's resource
	// comes with a "[Resource from … at …]" prefix.
	ClaudeAdapterName:      claudeBudget,
	ClaudeImageAdapterName: claudeBudget,
	// Codex 0.162.0 keeps 12,000 bytes of a tool result for a model its
	// catalog does not list (48,000 for one it does; a person's own model is
	// often not listed), cutting out the middle, and sends an input tool's
	// resource as its whole JSON, escaped, which can double a page of short
	// lines.
	CodexAdapterName:      codexBudget,
	CodexImageAdapterName: codexBudget,
	// Copilot CLI 1.0.95 saves a tool result over 20,480 bytes to a file and
	// gives the model its first 500 characters.
	CopilotImageAdapterName: {Bytes: 19 << 10, Lines: toolsrv.DefaultMaxResultLines},
}

var (
	claudeBudget = Budget{Bytes: 46 << 10, Lines: toolsrv.DefaultMaxResultLines}
	codexBudget  = Budget{Bytes: 5 << 10, Lines: toolsrv.DefaultMaxResultLines}
)

// ResultBudget is the result budget of the adapter named name; an adapter
// with no measured row gets the tightest measured, Codex's.
func ResultBudget(name string) Budget {
	if b, ok := resultBudgets[name]; ok {
		return b
	}
	return codexBudget
}
