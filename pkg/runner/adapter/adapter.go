// Package adapter defines the harness adapter interface and its registry.
// The harness ecosystem moves fast; the runner's value is the
// contract-to-enforcement compiler, not the model loop, so harnesses sit
// behind the smallest interface that lets the engine stay harness-agnostic:
// compile definition files + describe the agent container, then normalize
// the event stream.
package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry/pkg/manifest"
)

// Input is everything an adapter may use to compile harness files. Note
// what is absent: secret values, host paths, run ids — adapters compile
// deterministic plan content only.
type Input struct {
	// Mode is the run mode the plan is compiled for (autonomous or
	// interactive); it selects the harness argv and the Session block and
	// changes nothing else.
	Mode manifest.Mode
	// Instruction is the fully rendered system instruction: the System
	// Prompt, then the package's instruction body.
	Instruction string
	// Summary is the manifest one-liner (harness description fields).
	Summary string
	// Model is the operator's model policy.
	Model policy.Model
	// HarnessImage is the pinned harness container image.
	HarnessImage string
	// GatewayURL is the MCP endpoint on the run network.
	GatewayURL string
	// SchemaJSON is the optional output.schema.json content.
	SchemaJSON []byte
	// Harness is the operator policy's block for THIS adapter
	// (harness.<name>), the configuration an adapter that drives the
	// person's own installation of a harness needs: which executable, and —
	// because such a harness speaks its own protocol to its own endpoint —
	// which model and credential variable it is given. Zero for a
	// containerised harness.
	Harness policy.HarnessPolicy
	// UnchangedTools are tools beyond the input tools whose results reach
	// the model unchanged, past a harness's own scrub of tool output: the
	// host link's, where the release declares a target host. Empty for
	// every other plan.
	UnchangedTools []string
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
	// installation instead of Agent.
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
// contracts, so adapters map what they recognize and pass the rest through
// as Other; receipts keep every raw line regardless.
const (
	// EventToolCall counts against --max-turns (exit 16). Text and Tool
	// both name the tool; CallID is the harness's id for the call.
	EventToolCall EventKind = "tool_call"
	// EventToolResult is a tool's answer to a call: CallID names the call
	// it answers, Tool the tool where the harness says it, Failed whether
	// the tool reported an error, and Text the text it returned. It is an
	// observation, never a turn: it counts against no budget and is never
	// the run's result.
	EventToolResult EventKind = "tool_result"
	// EventAssistantText carries assistant output text; the engine keeps
	// the last one as the result.
	EventAssistantText EventKind = "assistant_text"
	// EventUsage carries token/cost accounting.
	EventUsage EventKind = "usage"
	// EventError carries a harness-reported error.
	EventError EventKind = "error"
	// EventPermissionGranted records an in-harness permission request the
	// runner allowed: the tool is one the gateway serves, so the policy
	// already bounds it (interactive sessions).
	EventPermissionGranted EventKind = "permission_granted"
	// EventPermissionDenied records an in-harness permission request the
	// runner refused: a tool outside the gateway's catalog, which should
	// never occur (interactive sessions).
	EventPermissionDenied EventKind = "permission_denied"
	// EventPlanLimit says the person's plan refused the run's model calls
	// for its usage limit (invariant I9): the engine ends the run with the
	// plan-limit outcome, never retries it and never reports it as success.
	// Text is the vendor's own words, ResetsAt when it said when the limit
	// lifts (zero otherwise: no reset time is invented).
	EventPlanLimit EventKind = "plan_limit"
	// EventOrgPolicy says a policy of the person's organization or
	// enterprise at the vendor refused the run: the engine ends the run
	// with that policy named, never retries it and never reports it as
	// success. Policy is which, a word of receipt's closed list
	// (receipt.OrgPolicy*); Text the vendor's words.
	EventOrgPolicy EventKind = "org_policy"
	// EventOutOfCredits says the model's provider refused the run's calls
	// because the account that pays has no credits left: an HTTP 402, or
	// the provider's own words for it. The engine ends the run with the
	// out-of-credits outcome and the provider's reason, never retried,
	// never a success. Text is the provider's words.
	EventOutOfCredits EventKind = "out_of_credits"
	// EventReasoning marks a chunk of the model's reasoning stream. It
	// carries no text: reasoning is never the result. The engine notes only
	// that the model's last output was reasoning, to name why a
	// schema-checked run has no answer.
	EventReasoning EventKind = "reasoning"
	// EventOther is an unrecognized-but-valid harness event.
	EventOther EventKind = "other"
)

// Event is one normalized harness event.
type Event struct {
	Kind EventKind
	// Text is the assistant text (EventAssistantText), the error message
	// (EventError), the tool's name (EventToolCall) or the text it
	// returned (EventToolResult).
	Text string
	// Tool is the tool's name on EventToolCall, and on EventToolResult
	// when the harness repeats it with the answer; a harness that does
	// not leaves the pairing to CallID.
	Tool string
	// CallID is the harness's id for a tool call, on EventToolCall and on
	// the EventToolResult that answers it; "" when the harness gives none.
	CallID string
	// Failed marks an EventToolResult whose tool reported an error.
	Failed bool
	// Redacted marks an EventToolResult whose content held an embedded
	// resource, which Text leaves out: the tool server answers with one
	// when the content is the person's own input. The receipts keep it.
	Redacted bool
	// Delta marks Text as one streamed chunk of the current assistant
	// message rather than a whole message: the engine concatenates deltas
	// until a tool call or usage event ends the message.
	Delta bool
	// InputTokens/OutputTokens/CostUSD are set on EventUsage.
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	// TotalTokens is set on EventUsage by a harness that reports context
	// tokens without an input/output split: docker-agent's ACP
	// `usage_update` carries a cumulative `used`, and the driver emits the
	// delta since the previous update. Input and output stay zero then —
	// unknown, not guessed.
	TotalTokens int64
	// ResetsAt is set on EventPlanLimit when the harness said when the
	// plan's limit lifts.
	ResetsAt time.Time
	// Policy is set on EventOrgPolicy: which policy refused the run.
	Policy string
	// Raw is the original line for receipts.
	Raw json.RawMessage
}

// ToolExcerptMax is the most bytes of a tool's output a live run's driver
// is handed (engine.Params.OnEvent). The receipts keep the whole raw line;
// the live event is for following a run as it happens, where a tool that
// returns a whole file must not flood the reader.
const ToolExcerptMax = 2048

// toolExcerptCut marks an excerpt that was cut at ToolExcerptMax.
const toolExcerptCut = "\n[\u2026 cut]"

// ToolExcerpt is the first ToolExcerptMax bytes of a tool's output, cut at
// a character boundary and marked as cut when it is longer, with
// surrounding whitespace trimmed. Cut only text that is already masked:
// a secret the cut splits no longer matches the value it would be masked
// by.
func ToolExcerpt(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= ToolExcerptMax {
		return s
	}
	cut := ToolExcerptMax
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRightFunc(s[:cut], unicode.IsSpace) + toolExcerptCut
}

// textBlock is a content block that may carry text, in the shape MCP and
// the harnesses that follow it share.
type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// joinText is the text of a list of content blocks, one block per line,
// and whether one of them was an embedded resource, whose text is left
// out. Images and other blocks carry no text and are left to the receipts.
func joinText(blocks []textBlock) (text string, redacted bool) {
	var parts []string
	for _, b := range blocks {
		switch {
		case b.Type == "text" && b.Text != "":
			parts = append(parts, b.Text)
		case b.Type == "resource":
			redacted = true
		}
	}
	return strings.Join(parts, "\n"), redacted
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
	// harness container's attached stdio. Adapters without an interactive
	// transport return ErrNotSupported.
	Session(ctx context.Context, rw io.ReadWriteCloser, opts SessionOptions) (SessionDriver, error)
}

// SessionOptions is what the engine tells a driver about the run it will
// drive.
type SessionOptions struct {
	// GatewayTools are the tool names the run's gateway serves — the run's
	// own grant: the plan's `tools`, bounded by policy at compile, plus the
	// knowledge and voice tools a run-time attachment adds, plus every tool
	// the gateway lists to the session's probe, which is how a declared MCP
	// server's tools are known. A harness permission request for one of
	// them is granted; anything else is denied (under ACP the client is the
	// permission authority for every tool call, and a denial does not reach
	// the model as a refused tool result — docker-agent fails the whole
	// turn).
	GatewayTools []string
	// ModelSignIn says the run is paid by a model sign-in: a 429 from the
	// model is then the plan's limit (EventPlanLimit), not a rate limit to
	// wait out (invariant I9).
	ModelSignIn bool
	// Resumable asks for a conversation the driver can carry over to a new
	// harness process (spec.SessionSpec.RestartOnRenewal): the engine may
	// end the harness between turns and hand the driver a new one (Resumer).
	Resumable bool
	// SignInToken is the run's model sign-in for a harness that asks its
	// driver for a token (spec.ModelSignInCallback): the token to hand it,
	// renewed first when refresh is set or the one held is near its end.
	// Nil when the run hands its sign-in no such way.
	SignInToken func(ctx context.Context, refresh bool) (SignInToken, error)
	// GatewayToken is the run's bearer for its gateway, for a driver that
	// tells its harness the gateway's MCP server over the session itself
	// rather than through a plan file that names the variable.
	GatewayToken string
	// HarnessFile reads one of the plan's harness files as the run
	// rendered it, for a driver that hands its harness the instruction
	// over the session rather than by path.
	HarnessFile func(name string) ([]byte, error)
	// ContinueID names a conversation the harness stored in an earlier
	// process, on the session store the run mounts again: a driver that can
	// continue one (Continuing) loads it at Start instead of opening a new
	// one. Empty opens a new conversation.
	ContinueID string
}

// Continuing is a SessionDriver that can continue a conversation an earlier
// harness process stored (SessionOptions.ContinueID). After Start it names
// the conversation it is in, and says whether that is the one it was asked
// to continue and, when it is not, why: a harness that could not load it
// opens a new conversation rather than failing the start.
type Continuing interface {
	// ConversationID is the harness's own id for the conversation the
	// session is in, set by Start.
	ConversationID() string
	// Continued reports whether Start continued ContinueID's conversation,
	// and why not when it did not ("" when none was asked for).
	Continued() (bool, string)
}

// SignInToken is a model sign-in's access token as a driver hands it to
// its harness, and when it expires. Never a refresh token (I3).
type SignInToken struct {
	AccessToken string
	ExpiresAt   time.Time
}

// Resumer is a SessionDriver that carries its conversation over to a new
// harness process: after the engine has ended the old one between turns
// (its stdin closed, the process gone), Resume performs the handshake on rw
// and resumes the conversation, on the same event stream (the restart token
// route).
type Resumer interface {
	Resume(ctx context.Context, rw io.ReadWriteCloser) error
}

// ModelSignInAdapter is implemented by an adapter that can spend a model
// sign-in: it names the kinds it can, and for those its Compile returns
// Result.ModelSignIn. The compiler refuses a policy naming a sign-in kind
// for any other adapter by policy, before the adapter compiles, so no
// adapter renders a key for a sign-in.
type ModelSignInAdapter interface {
	ModelSignIns() []string
}

// SpendsModelSignIn reports whether a can spend a model sign-in of kind.
func SpendsModelSignIn(a Adapter, kind string) bool {
	s, ok := a.(ModelSignInAdapter)
	return ok && slices.Contains(s.ModelSignIns(), kind)
}

// HostHarness is implemented by adapters that drive the person's own
// installation of a harness instead of a container, so `agentry doctor
// --adapter <name>` can report what is actually installed before a
// scheduled run finds out.
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
	// MCPCommand is the one line that registers the agentry-mcp server with
	// this host, verbatim from agentry-mcp's README ("Install"; the README
	// also shows an AGENTRY_BIN variant these strings leave out); the path
	// is the person's own build of the server. A client shows it; nothing
	// runs it.
	MCPCommand() string
	// WellKnownPaths are the places the harness's executable is usually
	// installed on goos ("linux", "darwin", "windows"), tried in order
	// after PATH: a GUI launched from the dock or the Start Menu carries
	// the login session's PATH, not the shell's, and the desktop apps that
	// bundle a harness put it nowhere on PATH at all. Each is a path
	// template, and this is the whole rule (harnessprobe applies it and
	// defines nothing of its own):
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

// SessionDriver drives one interactive harness session. Turns go in through
// Prompt; everything the harness says comes out of Events as the same
// normalized kinds an autonomous run produces, so receipts have one shape.
// A permission request from the harness is answered by the driver — granted
// for a gateway tool (EventPermissionGranted), denied for anything else
// (EventPermissionDenied) — and surfaced either way, so the transcript
// shows every approval. Every event a turn produces is on Events before its
// Prompt returns: the engine records them ahead of the turn's end on that
// promise, so none is lost to an End that follows the turn.
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

// registry maps adapter names to constructors.
var registry = map[string]Adapter{}

// planned names error with a "planned" message instead of "unknown".
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
		return nil, fmt.Errorf("adapter %q is planned but not implemented; available: %v", name, Names())
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

// DefaultName is the default adapter.
const DefaultName = "docker-agent"

// DefaultFor is the adapter a run uses when none is named, for a model
// sign-in docker-agent cannot spend: the Codex image for a ChatGPT plan,
// the Claude Code image for a Claude subscription, the Copilot CLI image
// for a Copilot plan; else DefaultName. docker-agent stays the default for
// every key.
func DefaultFor(pol *policy.Policy) string {
	if pol == nil {
		return DefaultName
	}
	switch policy.KeyKind(pol.Harness[DefaultName], pol.Model) {
	case policy.KeyKindChatGPTPlan:
		return CodexImageAdapterName
	case policy.KeyKindClaudeSignIn:
		return ClaudeImageAdapterName
	case policy.KeyKindCopilotSignIn:
		return CopilotImageAdapterName
	}
	return DefaultName
}

// keyringAccount is the OS keychain account a run falls back to for the
// model key when the named variable is unset (policy.KeySource says
// keyring): the model provider's; "" when the environment is the only
// source.
func keyringAccount(in Input) string {
	if policy.KeySource(in.Harness, in.Model) != modelkey.SourceKeyring {
		return ""
	}
	return modelkey.Account(in.Model.Provider)
}

// anthropicKeyExpectation is the credential kind the plan is rendered for,
// for the engine to hold the resolved value against. Only an
// Anthropic-shaped credential has two kinds: the claude adapter attaches it
// always (its credential is Anthropic's shape whatever endpoint the policy
// names), the docker-agent adapter when its client is the anthropic one.
func anthropicKeyExpectation(in Input) *spec.CredentialKind {
	return &spec.CredentialKind{Provider: policy.ProviderAnthropic, Kind: policy.KeyKind(in.Harness, in.Model)}
}

// OrgPolicyRefusal is a driver's Start refused by a policy of the person's
// organization at the vendor: the engine ends the run with that outcome, as
// it does for an EventOrgPolicy mid-run.
type OrgPolicyRefusal struct {
	// Policy is which, a word of receipt's closed list; Text the vendor's
	// words.
	Policy, Text string
}

func (r *OrgPolicyRefusal) Error() string { return "refused by an organization's policy: " + r.Text }

// Refusal is an adapter's policy-shaped refusal at compile time: the
// compiler reports it as a refusal by policy (exit 11), not a compile
// failure, because the policy is what has to change.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }
