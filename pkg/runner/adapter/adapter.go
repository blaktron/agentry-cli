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
	"slices"
	"sort"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
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
	// Instruction is the fully rendered system instruction: the System
	// Prompt, then the package's instruction body (agentry-cli#520).
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
	// OneTurn, on an autonomous plan, is the session protocol the run is
	// driven by as one turn (spec.HarnessSpec.OneTurn); the adapter's
	// Session is its driver. Nil when the harness prints its events.
	OneTurn *spec.SessionSpec
	// ModelSignIn is how the harness reads the model sign-in the policy
	// names (policy.IsModelSignIn of its key kind): its kind, its delivery
	// and the container path the harness reads it at. The compiler adds
	// the mount itself (invariant I4) and refuses the plan when the policy
	// names a sign-in and the adapter returned none. Nil for a key.
	ModelSignIn *spec.ModelSignInSpec
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
	// EventPlanLimit says the person's plan refused the run's model calls
	// for its usage limit (plan model-sign-ins.md D12, invariant I9): the
	// engine ends the run with the plan-limit outcome, never retries it
	// and never reports it as success. Text is the vendor's own words,
	// ResetsAt when it said when the limit lifts (zero otherwise: no reset
	// time is invented).
	EventPlanLimit EventKind = "plan_limit"
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
	// ResetsAt is set on EventPlanLimit when the harness said when the
	// plan's limit lifts.
	ResetsAt time.Time
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
	// GatewayTools are the tool names the run's gateway serves — the run's
	// own grant: the plan's `tools`, bounded by policy at compile, plus the
	// knowledge and voice tools a run-time attachment adds
	// (agentry-cli#576), plus every tool the gateway lists to the session's
	// probe, which is how a declared MCP server's tools are known
	// (agentry-cli#578). A harness permission request
	// for one of them is granted; anything else is denied (DR-15,
	// agentry-cli#61: under ACP the client is the permission authority for
	// every tool call, and a denial does not reach the model as a refused
	// tool result — docker-agent fails the whole turn).
	GatewayTools []string
	// ModelSignIn says the run is paid by a model sign-in: a 429 from the
	// model is then the plan's limit (EventPlanLimit), not a rate limit to
	// wait out (invariant I9).
	ModelSignIn bool
	// Resumable asks for a conversation the driver can carry over to a new
	// harness process (spec.SessionSpec.RestartOnRenewal): the engine may
	// end the harness between turns and hand the driver a new one (Resumer).
	Resumable bool
}

// Resumer is a SessionDriver that carries its conversation over to a new
// harness process: after the engine has ended the old one between turns
// (its stdin closed, the process gone), Resume performs the handshake on
// rw and resumes the conversation, on the same event stream (the restart
// token route, plan model-sign-ins.md D10).
type Resumer interface {
	Resume(ctx context.Context, rw io.ReadWriteCloser) error
}

// ModelSignInAdapter is implemented by an adapter that can spend a model
// sign-in (plan model-sign-ins.md §4.2): it names the kinds it can, and
// for those its Compile returns Result.ModelSignIn. The compiler refuses a
// policy naming a sign-in kind for any other adapter by policy, before
// the adapter compiles, so no adapter renders a key for a sign-in.
type ModelSignInAdapter interface {
	ModelSignIns() []string
}

// SpendsModelSignIn reports whether a can spend a model sign-in of kind.
func SpendsModelSignIn(a Adapter, kind string) bool {
	s, ok := a.(ModelSignInAdapter)
	return ok && slices.Contains(s.ModelSignIns(), kind)
}

// HostHarness is implemented by adapters that drive the person's own
// installation of a harness instead of a container (plan
// runner-m-r4-adapters §5), so `agentry doctor --adapter <name>` can
// report what is actually installed before a scheduled run finds out.
type HostHarness interface {
	// Executable is the program name looked up on PATH, then in
	// WellKnownPaths; the operator policy's harness.<name>.path
	// overrides both.
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
	// DisplayName is the product's name for a person ("Claude Code"),
	// where Name is the adapter's ("claude").
	DisplayName() string
	// MCPCommand is the one line that registers the agentry-mcp server
	// with this host, verbatim from agentry-mcp's README ("Install"; the
	// README also shows an AGENTRY_BIN variant these strings leave out);
	// the path is the person's own build of the server. A client shows
	// it; nothing runs it (agentry-cli#151).
	MCPCommand() string
	// WellKnownPaths are the places the harness's executable is usually
	// installed on goos ("linux", "darwin", "windows"), tried in order
	// after PATH (agentry-cli#167): a GUI launched from the dock or the
	// Start Menu carries the login session's PATH, not the shell's, and
	// the desktop apps that bundle a harness put it nowhere on PATH at
	// all. Each is a path template, and this is the whole rule
	// (harnessprobe applies it and defines nothing of its own):
	//
	//   - "~" as the whole first element is the home directory ("~user"
	//     is left alone); it is substituted after the variables, so a
	//     "$" in the home path is a character, not an expansion;
	//   - "${VAR}", braces required, is an environment variable; a bare
	//     "$VAR" is literal; a template whose variable is unset or empty
	//     is skipped, never resolved against an empty prefix;
	//   - a final "*" is a directory of version-named builds, of which
	//     the newest by dotted version is taken (a prerelease suffix does
	//     not demote a build; on a tie between "2.1.275" and
	//     "2.1.275.exe" the executable wins).
	//
	// Only the executable's existence is read from them — a regular file,
	// through a link; nothing is run but the version flag.
	WellKnownPaths(goos string) []string
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
// transcript shows every approval. Every event a turn produces is on
// Events before its Prompt returns: the engine records them ahead of the
// turn's end on that promise, so none is lost to an End that follows the
// turn (agentry-cli#204).
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

// DefaultFor is the adapter a run uses when none is named: the Codex image
// when the policy's model is paid by a ChatGPT plan, which docker-agent
// cannot spend (plan model-sign-ins.md D8, D15; agentry-cli#739), else
// DefaultName. docker-agent stays the default for every key (D16).
func DefaultFor(pol *policy.Policy) string {
	if pol != nil && policy.KeyKind(pol.Harness[DefaultName], pol.Model) == policy.KeyKindChatGPTPlan {
		return CodexImageAdapterName
	}
	return DefaultName
}

// keyringAccount is the OS keychain account a run falls back to for the
// model key when the named variable is unset (policy.KeySource says
// keyring; agentry-cli#148): the model provider's; "" when the environment
// is the only source.
func keyringAccount(in Input) string {
	if policy.KeySource(in.Harness, in.Model) != modelkey.SourceKeyring {
		return ""
	}
	return modelkey.Account(in.Model.Provider)
}

// anthropicKeyExpectation is the credential kind the plan is rendered
// for, for the engine to hold the resolved value against
// (agentry-cli#171). Only an Anthropic-shaped credential has two kinds:
// the claude adapter attaches it always (its credential is Anthropic's
// shape whatever endpoint the policy names), the docker-agent adapter
// when its client is the anthropic one.
func anthropicKeyExpectation(in Input) *spec.CredentialKind {
	return &spec.CredentialKind{Provider: policy.ProviderAnthropic, Kind: policy.KeyKind(in.Harness, in.Model)}
}

// Refusal is an adapter's policy-shaped refusal at compile time: the
// compiler reports it as a refusal by policy (exit 11), not a compile
// failure, because the policy is what has to change.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }
