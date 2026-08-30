// Package adapter defines the harness adapter interface (RUNNER-PLAN §R10)
// and its registry. The harness ecosystem moves fast (annex §4); the
// runner's leverage is the contract-to-enforcement compiler, not the model
// loop, so harnesses sit behind the smallest interface that lets the
// engine stay harness-agnostic: compile definition files + describe the
// agent container, then normalize the event stream.
package adapter

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// Input is everything an adapter may use to compile harness files. Note
// what is absent: secret values, host paths, run ids — adapters compile
// deterministic plan content only (§R4).
type Input struct {
	// Instruction is the fully rendered system instruction (§R2: prompt
	// body + runner contract postscript).
	Instruction string
	// Summary is the manifest one-liner (harness description fields).
	Summary string
	// Model is the operator's model policy.
	Model policy.Model
	// HarnessImage is the pinned harness container image.
	HarnessImage string
	// GatewayURL is the MCP endpoint on the run network.
	GatewayURL string
	// SchemaJSON is the optional output.schema.json content (§R2).
	SchemaJSON []byte
}

// Result is what an adapter compiles.
type Result struct {
	// Files are harness-native definition files, written by the compiler
	// under plan/harness/ and mounted read-only at spec.PlanMount.
	Files map[string][]byte
	// Agent is the agent container to run.
	Agent spec.ContainerSpec
	// Model is the provider/model string for receipts.
	Model string
	// ResultKind is "json" when SchemaJSON was wired, else "text".
	ResultKind string
}

// EventKind classifies normalized harness events.
type EventKind string

// Event kinds. Parsed best-effort: harness NDJSON schemas are not stable
// contracts (annex risk register), so adapters map what they recognize and
// pass the rest through as Other; receipts keep every raw line regardless.
const (
	// EventToolCall counts against --max-turns (§R8 exit 16).
	EventToolCall EventKind = "tool_call"
	// EventAssistantText carries assistant output text; the engine keeps
	// the last one as the result.
	EventAssistantText EventKind = "assistant_text"
	// EventUsage carries token/cost accounting.
	EventUsage EventKind = "usage"
	// EventError carries a harness-reported error.
	EventError EventKind = "error"
	// EventOther is an unrecognized-but-valid harness event.
	EventOther EventKind = "other"
)

// Event is one normalized harness event.
type Event struct {
	Kind EventKind
	// Text is the assistant text (EventAssistantText) or error message
	// (EventError).
	Text string
	// InputTokens/OutputTokens/CostUSD are set on EventUsage.
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	// Raw is the original line for receipts.
	Raw json.RawMessage
}

// Adapter compiles a run plan's harness half and interprets its output.
type Adapter interface {
	// Name is the registry key (--adapter flag value).
	Name() string
	// Compile renders harness definition files and the agent container.
	Compile(in Input) (Result, error)
	// Events parses one harness stdout line into normalized events. A
	// non-JSON or unrecognized line yields a single EventOther/EventAssistantText
	// per the adapter's judgment; never an error — parsing must not kill
	// a run the harness thinks is fine.
	Events(line []byte) []Event
}

// registry maps adapter names to constructors. Slots for planned adapters
// (M-R4) are declared with nil so --list-adapters can show them honestly.
var registry = map[string]Adapter{}

// planned names error with an M-R4 pointer instead of "unknown".
var planned = map[string]bool{"goose": true, "claude": true, "codex": true}

// Register installs an adapter; duplicate names are a programmer error.
func Register(a Adapter) {
	if _, dup := registry[a.Name()]; dup {
		panic("adapter registered twice: " + a.Name())
	}
	registry[a.Name()] = a
}

// Get resolves an adapter name.
func Get(name string) (Adapter, error) {
	if a, ok := registry[name]; ok {
		return a, nil
	}
	if planned[name] {
		return nil, fmt.Errorf("adapter %q is planned but not implemented in v1 (RUNNER-PLAN M-R4); available: %v", name, Names())
	}
	return nil, fmt.Errorf("unknown adapter %q; available: %v", name, Names())
}

// Names lists registered adapters, sorted.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// DefaultName is the v1 default adapter (annex §7, Q-R4).
const DefaultName = "docker-agent"
