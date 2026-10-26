// The claude adapter: the person's own installation of Claude Code, driven
// as a host process rather than an image the runner builds or pins.
// Headless print mode with the stream-json event protocol is the autonomous
// run; the same protocol in both directions is the interactive session.
//
// The runner's invariants are kept by construction, not by trust:
//
//   - Gateway-only tools. The single MCP server in the generated
//     mcp.json is the run's gateway; --strict-mcp-config makes that file
//     the ONLY source of MCP servers, --tools "" removes every built-in
//     tool (Bash, the file tools, WebFetch), and --permission-mode
//     dontAsk denies anything the --allowedTools prefix does not name.
//   - Secrets by name. The gateway token reaches the config as
//     ${MCP_GATEWAY_TOKEN}, expanded by Claude Code from the process
//     environment; the plan file holds the variable's name, never a value.
//   - No ambient context. --bare skips the person's hooks, plugins,
//     skills, auto memory and CLAUDE.md; --setting-sources "" loads no
//     user, project or local settings; CLAUDE_CONFIG_DIR points at a
//     run-scoped directory. What the model sees is the QuickAgent's
//     instruction and nothing else, which is also what makes a run
//     reproducible on someone else's machine. For a subscription token
//     the run has no --bare (see Compile): the config dir and the
//     setting sources still close hooks, plugins, skills, memory and the
//     person's login, the engine refuses the run when an ancestor of the
//     harness directory holds a CLAUDE.md, and managed settings stay the
//     administrator's — a weaker construction for that kind, recorded
//     as such.
//   - Operator-owned model credentials. The model endpoint is the
//     operator policy's (ANTHROPIC_BASE_URL plus the key by name), never
//     the person's own login: with an API key, --bare reads no OAuth
//     credential and no keychain entry; with a subscription token (policy
//     apiKeyKind), which Claude Code reads only from
//     CLAUDE_CODE_OAUTH_TOKEN and only without --bare, the run-scoped
//     CLAUDE_CONFIG_DIR is what holds no login of the person's. That
//     matches the runner's rule that the model key is always the
//     operator's, and it is why harness.claude.baseUrl /
//     .model / .apiKeyEnv / .apiKeyKind exist in the policy.
//
// Written against Claude Code 2.1.263 (claudeProbeVersion); the flags,
// the mcp.json shape and the event shapes below were established by
// running that release, and the conformance fixtures are a recording of
// a real run.
package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry/pkg/manifest"
)

func init() { Register(&claudeAdapter{}) }

const (
	// ClaudeAdapterName is the registry key and --adapter value.
	ClaudeAdapterName = "claude"
	// claudeExecutable is looked up on PATH unless the operator policy sets
	// harness.claude.path.
	claudeExecutable = "claude"
	// claudeProbeVersion is the Claude Code release this adapter was
	// written against; `agentry doctor --adapter claude` warns when the
	// installed one differs, and never refuses over it.
	claudeProbeVersion = "2.1.263"
	// claudeInstructionFile and claudeMCPFile are plan files, written
	// into the run's harness directory, which is also the harness
	// process's working directory — so the argv names them relative and
	// the plan stays free of host paths.
	claudeInstructionFile = "instruction.md"
	claudeMCPFile         = "mcp.json"
	// claudeConfigDirName is the run-scoped CLAUDE_CONFIG_DIR: Claude
	// Code creates it, and keeping it inside the run means no state of
	// this run lands in the person's own configuration.
	claudeConfigDirName = ".claude-home"
	// claudeGatewayServer is the MCP server name in mcp.json; Claude Code
	// derives its tool names as mcp__<server>__<tool>, so the same string
	// is the --allowedTools prefix that admits the gateway's catalog and
	// nothing else.
	claudeGatewayServer = "gateway"
	// claudeTransport names the session transport in the plan. Claude Code
	// speaks its own newline-delimited stream-json in both directions; it
	// is not ACP, so acpSession (docker-agent's driver) does not apply.
	claudeTransport = "claude-stream-json"
	// claudeProtocolVersion tracks the shape of that protocol as this
	// adapter reads it.
	claudeProtocolVersion = 1
)

// claudeAncestorContext is what Claude Code reads from the working
// directory's ancestors when not in bare mode (the memory files and the
// rules directory), for the engine to refuse a subscription run over.
var claudeAncestorContext = []string{"CLAUDE.md", "CLAUDE.local.md", ".claude/rules"}

// claudeAdapter implements Adapter for the installed Claude Code CLI.
type claudeAdapter struct{}

func (c *claudeAdapter) Name() string { return ClaudeAdapterName }

// Executable, ProbeVersion and VersionArgv implement HostHarness so
// `agentry doctor --adapter claude` can report the installation.
func (c *claudeAdapter) Executable() string    { return claudeExecutable }
func (c *claudeAdapter) ProbeVersion() string  { return claudeProbeVersion }
func (c *claudeAdapter) VersionArgv() []string { return []string{"--version"} }
func (c *claudeAdapter) InstallURL() string    { return "https://github.com/anthropics/claude-code" }
func (c *claudeAdapter) DisplayName() string   { return "Claude Code" }
func (c *claudeAdapter) MCPCommand() string {
	return "claude mcp add agentry -- node /path/to/agentry-mcp/dist/index.js"
}

// WellKnownPaths implements HostHarness. The native installer's
// ~/.local/bin/claude (a link to ~/.local/share/claude/versions/<ver>) and
// the Claude desktop app's own Claude Code build, ~/.claude/remote/ccd-cli/
// <ver> (its Code tab's runtime, answering --version like the CLI), were
// both observed on Linux; the Windows spellings follow the installer
// documentation (~\.local\bin\claude.exe) and npm's global shim, and are
// not verified here.
func (c *claudeAdapter) WellKnownPaths(goos string) []string {
	switch goos {
	case "windows":
		return []string{`~/.local/bin/claude.exe`, `~/.claude/remote/ccd-cli/*`, `${APPDATA}/npm/claude.cmd`}
	default:
		return []string{"~/.local/bin/claude", "~/.claude/remote/ccd-cli/*"}
	}
}

// claudeMCPServer is one entry of Claude Code's MCP config file. Structs,
// not map[string]any, so the rendered bytes are deterministic.
type claudeMCPServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

type claudeMCPConfig struct {
	MCPServers map[string]claudeMCPServer `json:"mcpServers"`
}

// ModelSignIns implements ModelSignInAdapter: the installed Claude Code
// spends a Claude subscription through its own sign-in, kept in a directory
// Agentry owns.
func (c *claudeAdapter) ModelSignIns() []string { return []string{policy.KeyKindClaudeSignIn} }

// claudeMCPJSON is the gateway-only MCP config, the gateway at url.
func claudeMCPJSON(url string) ([]byte, error) {
	cfg, err := json.MarshalIndent(claudeMCPConfig{MCPServers: map[string]claudeMCPServer{
		claudeGatewayServer: {
			Type: "http",
			URL:  url,
			// ${VAR} is Claude Code's own expansion, from the process
			// environment: the token never appears in plan bytes.
			Headers: map[string]string{"Authorization": "Bearer ${" + spec.GatewayTokenEnv + "}"},
		},
	}}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render %s: %w", claudeMCPFile, err)
	}
	return append(cfg, '\n'), nil
}

// claudeArgv is the one argv of both modes but for the input side: an
// autonomous run takes its task on stdin and ends, a session reads user
// turns as stream-json and stays up until stdin closes. bare is --bare,
// which reads no OAuth credential, so a run on a subscription token or on
// Claude Code's own sign-in runs without it.
// persist leaves session persistence on, for a persistent agent's segment,
// whose conversation the next segment continues: Claude Code then writes
// it under its config directory's projects folder.
func claudeArgv(mode manifest.Mode, instructionPath, mcpPath, model string, bare, persist bool) (argv []string, session *spec.SessionSpec, stdinTask bool, err error) {
	argv = []string{"-p"}
	if bare {
		argv = append(argv, "--bare")
	}
	argv = append(argv,
		"--verbose",
		"--output-format", "stream-json",
		"--system-prompt-file", instructionPath,
		"--mcp-config", mcpPath,
		"--strict-mcp-config",
		"--setting-sources", "",
		"--tools", "",
		"--allowedTools", "mcp__"+claudeGatewayServer,
		"--permission-mode", "dontAsk",
		"--permission-prompts", "none",
	)
	if !persist {
		argv = append(argv, "--no-session-persistence")
	}
	argv = append(argv,
		"--disable-slash-commands",
		"--model", model,
	)
	switch mode {
	case "", manifest.ModeAutonomous:
		return argv, nil, true, nil
	case manifest.ModeInteractive:
		return append(argv, "--input-format", "stream-json"), &spec.SessionSpec{
			Transport:       claudeTransport,
			ProtocolVersion: claudeProtocolVersion,
		}, false, nil
	}
	return nil, nil, false, fmt.Errorf("unknown run mode %q", mode)
}

// claudeQuietEnv turns off telemetry, error reporting, update checks and
// every other background call: a sandboxed run should talk to the model and
// the gateway, and to nothing else.
func claudeQuietEnv() []spec.EnvVar {
	return []spec.EnvVar{
		{Name: "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", Value: "1"},
		{Name: "DISABLE_TELEMETRY", Value: "1"},
		{Name: "DISABLE_ERROR_REPORTING", Value: "1"},
		{Name: "DISABLE_AUTOUPDATER", Value: "1"},
		{Name: "DISABLE_BUG_COMMAND", Value: "1"},
	}
}

// claudeModel is the run's model: the harness block's, else the policy's.
func claudeModel(adapterName string, in Input) (string, error) {
	model := in.Harness.Model
	if model == "" {
		model = in.Model.Name
	}
	if model == "" {
		return "", fmt.Errorf("%s adapter: no model (set model.name, or harness.%s.model in the policy)", adapterName, adapterName)
	}
	return model, nil
}

// Compile renders the instruction, the gateway-only MCP config and the
// host process that runs them.
func (c *claudeAdapter) Compile(in Input) (Result, error) {
	if in.Instruction == "" {
		return Result{}, errors.New("claude adapter: empty instruction")
	}
	model, err := claudeModel(ClaudeAdapterName, in)
	if err != nil {
		return Result{}, err
	}
	kind := policy.KeyKind(in.Harness, in.Model)
	signIn := kind == policy.KeyKindClaudeSignIn
	keyEnv := policy.KeyEnv(in.Harness, in.Model)
	if keyEnv == "" && !signIn {
		return Result{}, errors.New("claude adapter: no model key variable (set model.apiKeyEnv, or harness.claude.apiKeyEnv in the policy)")
	}
	if signIn && in.Harness.BaseURL != "" {
		// Claude Code's own sign-in talks to Anthropic itself; pointing it
		// at another endpoint would hand that endpoint the person's login.
		return Result{}, &Refusal{Reason: "a Claude subscription through Claude Code reaches Anthropic itself, and harness.claude.baseUrl names " + in.Harness.BaseURL + ": remove it, or pay with an API key"}
	}

	instruction, resultKind := schemaInstruction(in.Instruction, in.SchemaJSON)

	// The gateway is published on 127.0.0.1 for a host harness; the address
	// is only known per run, so it is a placeholder the engine renders.
	cfg, err := claudeMCPJSON(spec.PHHostGatewayURL)
	if err != nil {
		return Result{}, fmt.Errorf("claude adapter: %w", err)
	}

	// A subscription OAuth token is read from CLAUDE_CODE_OAUTH_TOKEN, and
	// bare mode does not read that variable (Anthropic's own documentation:
	// "Bare mode does not read CLAUDE_CODE_OAUTH_TOKEN"), so that kind runs
	// without --bare; so does Claude Code's own sign-in, which bare mode
	// does not read either. Most of what --bare withheld is withheld by the
	// rest: --setting-sources "" loads no user, project or local settings,
	// and the config directory holds no hooks, plugins or memory of the
	// person's (run-scoped for a token; for a sign-in, Agentry's own, never
	// the person's ~/.claude). Two things stay open and are said so:
	// CLAUDE.md files in the ancestors of the harness directory, which the
	// engine refuses such a run over (engine: ancestorFile), and managed
	// settings, which are the machine administrator's and no flag closes.
	subscription := kind == policy.KeyKindSubscription
	argv, session, stdinTask, err := claudeArgv(in.Mode, claudeInstructionFile, claudeMCPFile, model, !subscription && !signIn, false)
	if err != nil {
		return Result{}, fmt.Errorf("claude adapter: %w", err)
	}

	// Without --bare, Claude Code reads CLAUDE.md, CLAUDE.local.md and
	// .claude/rules from the working directory and every ancestor up to
	// the root — the run directory sits under the person's home, so a
	// ~/CLAUDE.md would enter such a run. The engine refuses the run when
	// one is there (RefuseAncestorFiles), which is the invariant measured
	// rather than assumed.
	var ancestorContext []string
	if subscription || signIn {
		ancestorContext = claudeAncestorContext
	}
	env := []spec.EnvVar{{Name: spec.GatewayTokenEnv, FromRunToken: true}}
	configDir := spec.PHCfg + "/harness/" + claudeConfigDirName
	var msi *spec.ModelSignInSpec
	if signIn {
		// The person's sign-in lives in Agentry's own directory for the
		// installed Claude Code, which only Claude Code reads and writes:
		// the engine names it here and never reads it. No key variable, so
		// nothing outranks the sign-in (Claude Code reads
		// ANTHROPIC_AUTH_TOKEN, ANTHROPIC_API_KEY and apiKeyHelper first),
		// and none of the person's is inherited (InheritEnv below).
		configDir = spec.PHModelSignIn
		msi = &spec.ModelSignInSpec{Kind: kind, Provider: policy.ProviderAnthropic, Delivery: spec.ModelSignInConfigDir, Path: spec.PHModelSignIn}
	} else {
		// The model credential is the operator's, by name: an API key as
		// ANTHROPIC_AUTH_TOKEN, the bearer an Anthropic-compatible endpoint
		// expects; a subscription token as CLAUDE_CODE_OAUTH_TOKEN, the one
		// variable Claude Code reads such a token from. The plan records
		// which kind it was rendered for, and the engine refuses a value of
		// the other kind.
		credentialVar := "ANTHROPIC_AUTH_TOKEN"
		if subscription {
			credentialVar = "CLAUDE_CODE_OAUTH_TOKEN"
		}
		env = append(env, spec.EnvVar{Name: credentialVar, FromHostEnv: keyEnv, FromKeyring: keyringAccount(in), ExpectedKind: anthropicKeyExpectation(in)})
		if in.Harness.BaseURL != "" {
			env = append(env, spec.EnvVar{Name: "ANTHROPIC_BASE_URL", Value: in.Harness.BaseURL})
		}
	}
	env = append(env,
		// The proxy is the intended route out. A host process is not inside
		// the run network, so this is a request the harness can ignore.
		spec.EnvVar{Name: "HTTPS_PROXY", Value: spec.PHHostProxyURL},
		spec.EnvVar{Name: "HTTP_PROXY", Value: spec.PHHostProxyURL},
		// The gateway and the proxy are both on 127.0.0.1: MCP traffic
		// must reach the gateway directly rather than through the proxy.
		spec.EnvVar{Name: "NO_PROXY", Value: "127.0.0.1,localhost"},
		spec.EnvVar{Name: "CLAUDE_CONFIG_DIR", Value: configDir},
	)
	if signIn {
		// A claude.ai sign-in would otherwise fetch the account's own
		// connectors as MCP servers.
		env = append(env, spec.EnvVar{Name: "ENABLE_CLAUDEAI_MCP_SERVERS", Value: "false"})
	}
	env = append(env, claudeQuietEnv()...)

	return Result{
		Files: map[string][]byte{
			claudeInstructionFile: []byte(instruction),
			claudeMCPFile:         cfg,
		},
		Host: &spec.HostProcessSpec{
			Command:             claudeExecutable,
			RefuseAncestorFiles: ancestorContext,
			Argv:                argv,
			Env:                 env,
			// HOME and PATH only: the binary needs them to run. The
			// person's model credentials are deliberately NOT inherited
			// — the operator policy's are the ones a run may spend.
			InheritEnv:  []string{"HOME", "PATH"},
			VersionArgv: []string{"--version"},
			StdinTask:   stdinTask,
		},
		Session: session,
		// Claude Code speaks the Anthropic Messages API whatever endpoint
		// it is pointed at; the policy's baseUrl says which one that was.
		Model:       "anthropic/" + model,
		ResultKind:  resultKind,
		ModelSignIn: msi,
	}, nil
}

// claudeMessage is the part of one stream-json line this adapter reads.
// Everything else in the line is kept verbatim in the receipts.
type claudeMessage struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Message struct {
		// Content is decoded separately: an assistant message carries a
		// block array, other message types carry other shapes, and a
		// shape we do not know must not fail the line.
		Content json.RawMessage `json:"content"`
		// Usage is an assistant message's own request: what it sent,
		// cached tokens apart, is the conversation's size then.
		Usage struct {
			InputTokens         int64 `json:"input_tokens"`
			CacheReadTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	// ModelUsage is a result message's per-model account, with each
	// model's context window.
	ModelUsage map[string]struct {
		ContextWindow int64 `json:"contextWindow"`
	} `json:"modelUsage"`
	// Result is the final answer text on a result message.
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	StopReason   string  `json:"stop_reason"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	// Errors are a failed result's reasons, such as a conversation
	// --resume could not find.
	Errors []string `json:"errors"`
	// Response is a control_response's: the answer to the driver's
	// initialize request.
	Response struct {
		Subtype   string `json:"subtype"`
		RequestID string `json:"request_id"`
		Error     string `json:"error"`
	} `json:"response"`
	// ToolName appears on the permission-denial system message.
	ToolName string `json:"tool_name"`
	// CompactMetadata is a compact_boundary's: Claude Code compacted the
	// conversation itself, and newer versions say its size after.
	CompactMetadata struct {
		Trigger    string `json:"trigger"`
		PostTokens int64  `json:"post_tokens"`
	} `json:"compact_metadata"`
	// RateLimit is a rate_limit_event's: what Claude Code's sign-in was
	// told of the plan's usage limits (Anthropic's SDKRateLimitEvent).
	RateLimit struct {
		Status string `json:"status"`
		// ResetsAt is in seconds since the epoch: Claude Code rounds
		// Anthropic's anthropic-ratelimit-unified-reset header into it
		// (read from 2.1.296's own code).
		ResetsAt      int64  `json:"resetsAt"`
		RateLimitType string `json:"rateLimitType"`
		ErrorCode     string `json:"errorCode"`
	} `json:"rate_limit_info"`
}

// claudeBlock is one content block of a message: an assistant message's
// text and tool calls, a user message's tool results.
type claudeBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"`
	// ID is a tool call's; ToolUseID names the call a tool result answers.
	ID        string `json:"id"`
	ToolUseID string `json:"tool_use_id"`
	// Content is a tool result's: a string, or a list of content blocks.
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

// Events maps one Claude Code stream-json line to normalized events.
// Best-effort by design: an unrecognized shape becomes EventOther and never
// an error, and the receipts keep every raw line.
func (c *claudeAdapter) Events(line []byte) []Event {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return nil
	}
	raw := rawCopy([]byte(trimmed))
	var msg claudeMessage
	if err := json.Unmarshal([]byte(trimmed), &msg); err != nil {
		// Under --output-format stream-json every stdout line is JSON, so
		// a line that is not is noise — a wrapper's banner, a stray write
		// — and must not be mistaken for the run's answer the way
		// docker-agent's plain-text fallback would.
		return []Event{{Kind: EventOther, Text: trimmed, Raw: raw}}
	}
	switch msg.Type {
	case "assistant":
		return claudeAssistantEvents(msg, raw)
	case "user":
		if results := claudeToolResults(msg, raw); len(results) > 0 {
			return results
		}
	case "result":
		return claudeResultEvents(msg, raw)
	case "rate_limit_event":
		return []Event{claudeRateLimitEvent(msg, raw)}
	case "system":
		switch msg.Subtype {
		case "thinking_tokens":
			// A running token counter emitted many times per turn: the
			// receipts keep the lines, the transcript would drown in them.
			return nil
		case "permission_denied":
			// A tool outside the gateway's catalog, refused by the
			// harness itself under --permission-mode dontAsk. It should
			// not happen — the catalog is the whole tool surface — so it
			// is worth a transcript line when it does.
			return []Event{{Kind: EventPermissionDenied, Text: msg.ToolName, Raw: raw}}
		case "compact_boundary":
			// Claude Code compacted the conversation itself: the size it
			// says after, when it says one, is the conversation's size
			// now, and no usage may follow before the turn ends.
			return []Event{{Kind: EventUsage, ContextTokens: msg.CompactMetadata.PostTokens, ContextCompacted: true, Raw: raw}}
		}
		return []Event{{Kind: EventOther, Text: msg.Subtype, Raw: raw}}
	}
	return []Event{{Kind: EventOther, Text: msg.Type, Raw: raw}}
}

// claudeRateLimitEvent maps a rate_limit_event: rejected, or a plan whose
// included usage is spent (errorCode credits_required), is the plan's limit
// (invariant I9), with Claude Code's own reset time when it gave one;
// allowed and allowed_warning are kept in the receipts and nothing more.
func claudeRateLimitEvent(msg claudeMessage, raw json.RawMessage) Event {
	rl := msg.RateLimit
	if rl.Status != "rejected" && rl.ErrorCode != "credits_required" {
		return Event{Kind: EventOther, Text: "rate_limit_event " + rl.Status, Raw: raw}
	}
	text := "Claude Code reported the plan's usage limit"
	if rl.RateLimitType != "" {
		text += " (" + rl.RateLimitType + ")"
	}
	if rl.ErrorCode == "credits_required" {
		text += ": the plan's included usage is spent, and the run cannot continue until usage credits are bought"
	}
	ev := Event{Kind: EventPlanLimit, Text: text, Raw: raw}
	if rl.ResetsAt > 0 {
		ev.ResetsAt = time.Unix(rl.ResetsAt, 0).UTC()
	}
	return ev
}

// claudeAssistantEvents turns one assistant message into at most one text
// event plus one event per tool call.
func claudeAssistantEvents(msg claudeMessage, raw json.RawMessage) []Event {
	var blocks []claudeBlock
	if len(msg.Message.Content) > 0 {
		_ = json.Unmarshal(msg.Message.Content, &blocks)
	}
	var text strings.Builder
	var calls []Event
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use", "server_tool_use", "mcp_tool_use":
			calls = append(calls, Event{Kind: EventToolCall, Text: b.Name, Tool: b.Name, CallID: b.ID, Raw: raw})
		}
		// thinking / redacted_thinking are the model's reasoning: the
		// receipts keep the raw line, the result must not.
	}
	var events []Event
	// One text event per MESSAGE, not per block. The engine keeps the last
	// assistant text as the run's result, so a final answer split over
	// several blocks would otherwise be truncated to its last piece.
	if text.Len() > 0 {
		events = append(events, Event{Kind: EventAssistantText, Text: text.String(), Raw: raw})
	}
	events = append(events, calls...)
	if len(events) == 0 {
		return []Event{{Kind: EventOther, Raw: raw}}
	}
	return events
}

// claudeToolResults is one event per tool result a user message carries:
// Claude Code hands each tool's answer back to the model that way. The
// result names its call, not its tool; the call's event names the tool.
func claudeToolResults(msg claudeMessage, raw json.RawMessage) []Event {
	var blocks []claudeBlock
	if len(msg.Message.Content) > 0 {
		_ = json.Unmarshal(msg.Message.Content, &blocks)
	}
	var events []Event
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		text, redacted := claudeContent(b.Content)
		events = append(events, Event{Kind: EventToolResult, CallID: b.ToolUseID, Failed: b.IsError, Text: text, Redacted: redacted, Raw: raw})
	}
	return events
}

// claudeContent is the text of a tool result's content: the string
// itself, or the text blocks of a list joined, and whether the list held an
// embedded resource (joinText).
func claudeContent(c json.RawMessage) (text string, redacted bool) {
	var s string
	if json.Unmarshal(c, &s) == nil {
		return s, false
	}
	var blocks []textBlock
	if json.Unmarshal(c, &blocks) != nil {
		return "", false
	}
	return joinText(blocks)
}

// claudeResultEvents turns the terminating result message into the run's
// answer and its accounting.
func claudeResultEvents(msg claudeMessage, raw json.RawMessage) []Event {
	var events []Event
	if msg.IsError || (msg.Subtype != "" && msg.Subtype != "success") {
		detail := msg.Result
		if detail == "" {
			detail = msg.Subtype
		}
		events = append(events, Event{Kind: EventError, Text: detail, Raw: raw})
	}
	// The result message repeats the final answer. Emitting it (before the
	// usage event, which ends the message) makes the engine's result the
	// harness's own final text rather than whatever the last assistant
	// message happened to be. The session driver drops this copy, because
	// there the assistant message already put it in the transcript.
	if msg.Result != "" {
		events = append(events, Event{Kind: EventAssistantText, Text: msg.Result, Raw: raw})
	}
	events = append(events, Event{
		Kind:         EventUsage,
		InputTokens:  msg.Usage.InputTokens,
		OutputTokens: msg.Usage.OutputTokens,
		CostUSD:      msg.TotalCostUSD,
		Raw:          raw,
	})
	return events
}

// Session drives an interactive run over Claude Code's own stream-json
// protocol. SessionOptions.GatewayTools is not used: unlike ACP, this
// harness is not asked per call — the tool allow-list is compiled into the
// argv and the harness refuses everything else itself.
func (c *claudeAdapter) Session(_ context.Context, rw io.ReadWriteCloser, opts SessionOptions) (SessionDriver, error) {
	if rw == nil {
		return nil, errors.New("claude session: nil transport")
	}
	s := &claudeSession{
		rw:             rw,
		ad:             c,
		events:         make(chan Event, 256),
		readDone:       make(chan struct{}),
		continueID:     opts.ContinueID,
		conversationID: opts.ConversationID,
	}
	if s.continueID != "" || s.conversationID != "" {
		// Made before the reader starts: Claude Code reports a conversation
		// it cannot load as soon as it starts, before any input.
		s.starting = make(chan claudeStart, 1)
	}
	go s.readLoop()
	return s, nil
}
