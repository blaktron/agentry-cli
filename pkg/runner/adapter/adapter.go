// Package adapter defines the harness adapter interface (RUNNER-PLAN §R10)
// and its registry. The harness ecosystem moves fast (annex §4); the
// runner's leverage is the contract-to-enforcement compiler, not the model
// loop, so harnesses sit behind the smallest interface that lets the
// engine stay harness-agnostic: compile definition files + describe the
// agent container, then normalize the event stream.
package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry/pkg/manifest"
)

// Input is everything an adapter may use to compile harness files. Note
// what is absent: secret values, host paths, run ids — adapters compile
// deterministic plan content only (§R4).
type Input struct {
	// Mode is the run mode the plan is compiled for (autonomous or
	// interactive); it selects the harness argv and the Session block and
	// changes nothing else (plan §6.10 line 1).
	Mode manifest.Mode
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
	// Harness is the operator policy's block for THIS adapter
	// (harness.<name>), the configuration an adapter that drives the
	// person's own installation of a harness needs: which executable,
	// and — because such a harness speaks its own protocol to its own
	// endpoint — which model and credential variable it is given (plan
	// runner-m-r4-adapters §5). Zero for a containerised harness.
	Harness policy.HarnessPolicy
}

// Result is what an adapter compiles.
type Result struct {
	// Files are harness-native definition files, written by the compiler
	// under plan/harness/ and mounted read-only at spec.PlanMount.
	Files map[string][]byte
	// Session is the interactive drive (Input.Mode interactive); nil for
	// an autonomous plan.
	Session *spec.SessionSpec
	// Agent is the agent container to run.
	Agent spec.ContainerSpec
	// Host runs the harness on this machine from the person's own
	// installation instead of Agent (plan runner-m-r4-adapters §5).
	Host *spec.HostProcessSpec
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
	// EventPermissionGranted records an in-harness permission request the
	// runner allowed: the tool is one the gateway serves, so the policy
	// already bounds it (interactive sessions; DR-15, agentry-cli#61).
	EventPermissionGranted EventKind = "permission_granted"
	// EventPermissionDenied records an in-harness permission request the
	// runner refused: a tool outside the gateway's catalog, which should
	// never occur (interactive sessions; plan §6.10 line 4).
	EventPermissionDenied EventKind = "permission_denied"
	// EventOther is an unrecognized-but-valid harness event.
	EventOther EventKind = "other"
)

// Event is one normalized harness event.
type Event struct {
	Kind EventKind
	// Text is the assistant text (EventAssistantText) or error message
	// (EventError).
	Text string
	// Delta marks Text as one streamed chunk of the current assistant
	// message rather than a whole message: the engine concatenates deltas
	// until a tool call or usage event ends the message (agentry-cli#53).
	Delta bool
	// InputTokens/OutputTokens/CostUSD are set on EventUsage.
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	// TotalTokens is set on EventUsage by a harness that reports context
	// tokens without an input/output split: docker-agent's ACP
	// `usage_update` carries a cumulative `used`, and the driver emits the
	// delta since the previous update (agentry-cli#71). Input and output
	// stay zero then — unknown, not guessed.
	TotalTokens int64
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
	// Session returns the driver for an interactive session over the
	// harness container's attached stdio (plan §6.9). Adapters without an
	// interactive transport return ErrNotSupported.
	Session(ctx context.Context, rw io.ReadWriteCloser, opts SessionOptions) (SessionDriver, error)
}

// SessionOptions is what the engine tells a driver about the run it will
// drive.
type SessionOptions struct {
	// GatewayTools are the tool names the run's gateway serves — the plan's
	// `tools`, bounded by policy at compile. A harness permission request
	// for one of them is granted; anything else is denied (DR-15,
	// agentry-cli#61: under ACP the client is the permission authority for
	// every tool call, and a denial does not reach the model as a refused
	// tool result — docker-agent fails the whole turn).
	GatewayTools []string
}

// HostHarness is implemented by adapters that drive the person's own
// installation of a harness instead of a container (plan
// runner-m-r4-adapters §5), so `agentry doctor --adapter <name>` can
// report what is actually installed before a scheduled run finds out.
type HostHarness interface {
	// Executable is the program name resolved on PATH (the operator
	// policy's harness.<name>.path overrides the lookup).
	Executable() string
	// ProbeVersion is the harness release this adapter was written
	// against. A different one is a warning, never a refusal: the
	// adapters read event shapes best-effort by design.
	ProbeVersion() string
	// VersionArgv prints the harness's version.
	VersionArgv() []string
	// InstallURL is where a person gets the harness when it is not on
	// this machine — the vendor's own page, for a client to show.
	InstallURL() string
}

// ErrNotSupported is returned by Session for adapters without an
// interactive transport.
var ErrNotSupported = errors.New("adapter does not support interactive sessions")

// SessionDriver drives one interactive harness session (DR-15). Turns go
// in through Prompt; everything the harness says comes out of Events as
// the same normalized kinds an autonomous run produces, so receipts have
// one shape. A permission request from the harness is answered by the
// driver — granted for a gateway tool (EventPermissionGranted), denied for
// anything else (EventPermissionDenied) — and surfaced either way, so the
// transcript shows every approval.
type SessionDriver interface {
	// Start performs the protocol handshake and opens the session.
	Start(ctx context.Context) error
	// Prompt sends one user turn and returns when the harness ends it.
	Prompt(ctx context.Context, text string) (*Turn, error)
	// Events streams the session's events; closed after Close.
	Events() <-chan Event
	// Close ends the protocol; the harness exits on stdin EOF.
	Close() error
}

// Turn is the harness's answer to one Prompt.
type Turn struct {
	// StopReason is the protocol's reason (end_turn, cancelled, ...).
	StopReason string
	// Text is the assistant text of the turn.
	Text string
	// Usage is the turn's token usage when the harness reports it.
	Usage *TurnUsage
}

// TurnUsage is the token usage of one turn.
type TurnUsage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

// registry maps adapter names to constructors. Slots for planned adapters
// (M-R4) are declared with nil so --list-adapters can show them honestly.
var registry = map[string]Adapter{}

// planned names error with an M-R4 pointer instead of "unknown".
var planned = map[string]bool{"goose": true}

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
