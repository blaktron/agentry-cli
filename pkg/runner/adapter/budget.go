package adapter

import (
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// Budget is the most one tool result may hold for a harness to pass it to
// the model whole: bytes and lines. The compiler stamps it into the tools
// spec, and the tools server pages every result within it.
type Budget struct {
	Bytes int64
	Lines int
}

// resultBudgets is each adapter's measured tool-result limit, less a margin
// for what the harness adds around the text (each was measured from the
// pinned release's own code). Only docker-agent counts lines; every adapter
// keeps its line limit, which costs the others nothing but a smaller page
// of very short lines. A harness bump re-measures its row, as it re-records
// its conformance.
var resultBudgets = map[string]Budget{
	// docker-agent's always-on limit_large_tool_results hook: an MCP result
	// over 50 KiB or 2,000 lines is replaced by its last 50 KiB. Measured
	// on 1.150.0, whose history bounding (bound_tool_messages.go) cuts at
	// the same 50 KiB; 1.159.0 keeps both limits.
	DefaultName: {Bytes: toolsrv.DefaultMaxResultBytes, Lines: toolsrv.DefaultMaxResultLines},
	// Claude Code 2.1.296 saves a tool result over 50,000 characters (UTF-16
	// code units, never more than its UTF-8 bytes) to a file the agent has no
	// tool to open, and gives the model the path; an input tool's resource
	// comes with a "[Resource from … at …]" prefix.
	ClaudeAdapterName:      claudeBudget,
	ClaudeImageAdapterName: claudeBudget,
	// Codex 0.162.0 holds an MCP result to its model's truncation policy,
	// cutting out the middle, and the model's entry is the adapter's own
	// catalog's (codexCatalog), which sets it: codexToolResultBytes, the
	// runner's choice (codex's own fallback is 10,000 bytes). An input
	// tool's resource is sent as its whole JSON, escaped, which can double
	// a page of short lines, so the budget is under half the limit.
	CodexAdapterName:      codexBudget,
	CodexImageAdapterName: codexBudget,
	// Copilot CLI 1.0.95 saves a tool result over 20,480 bytes to a file and
	// gives the model its first 500 characters.
	CopilotImageAdapterName: {Bytes: 19 << 10, Lines: toolsrv.DefaultMaxResultLines},
}

var (
	claudeBudget = Budget{Bytes: 46 << 10, Lines: toolsrv.DefaultMaxResultLines}
	codexBudget  = Budget{Bytes: 20 << 10, Lines: toolsrv.DefaultMaxResultLines}
)

// ResultBudget is the result budget of the adapter named name; an adapter
// with no measured row gets the tightest measured.
func ResultBudget(name string) Budget {
	if b, ok := resultBudgets[name]; ok {
		return b
	}
	return tightestBudget()
}

// tightestBudget is the smallest measured budget, for an adapter with no
// row of its own.
func tightestBudget() Budget {
	var t Budget
	for _, b := range resultBudgets {
		if t.Bytes == 0 || b.Bytes < t.Bytes {
			t = b
		}
	}
	return t
}

// The page a window holds. A sixteenth of the window keeps a turn's few
// pages a small part of it, so a run on a small window is compacted before
// a page can overflow it.
const (
	// bytesPerToken is the estimate docker-agent's own caps count by.
	bytesPerToken = 4
	windowPages   = 16
	// minPageBytes is the smallest page: below it a file of short lines
	// takes more turns than the window it saves.
	minPageBytes = 16 << 10
	// unknownWindowPageBytes is the page on an endpoint of the person's
	// own whose window is not recorded, where nothing else bounds the
	// conversation: two thirds of docker-agent's page.
	unknownWindowPageBytes = 32 << 10
)

// PageBudget is the result budget of a run on model m under the adapter
// named name: the adapter's own (ResultBudget), held to a sixteenth of the
// model's window where the policy records one, in whole KiB and never under
// minPageBytes; on an endpoint of the person's own with no recorded window,
// to unknownWindowPageBytes. A hosted model with no recorded window keeps
// the adapter's budget: its harness knows its window, and every hosted
// window is large enough to hold that page.
func PageBudget(name string, m policy.Model) Budget {
	b := ResultBudget(name)
	page := b.Bytes
	switch {
	case m.ContextWindow > 0:
		page = max(m.ContextWindow*bytesPerToken/windowPages&^1023, minPageBytes)
	case m.OwnEndpoint():
		page = unknownWindowPageBytes
	}
	b.Bytes = min(b.Bytes, page)
	return b
}
