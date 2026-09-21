// The claude adapter (plan runner-m-r4-adapters §4.1, agentry-cli#80):
// the person's own installation of Claude Code, driven as a host process
// rather than an image the runner builds or pins (decision D2). Headless
// print mode with the stream-json event protocol is the autonomous run;
// the same protocol in both directions is the interactive session.
//
// The invariants of §3 are kept by construction, not by trust:
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
//     run-scoped directory. What the model sees is the BriefAgent's
//     instruction and nothing else, which is also what makes a run
//     reproducible on someone else's machine.
//   - Operator-owned model credentials. --bare deliberately reads no
//     OAuth credential and no keychain entry, so the model endpoint is
//     the operator policy's (ANTHROPIC_BASE_URL plus the key by name),
//     never the person's own login. That matches the runner's rule that
//     the model key is always the operator's (annex Q-R2), and it is why
//     harness.claude.baseUrl / .model / .apiKeyEnv exist in the policy.
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

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry/pkg/manifest"
)

func init() { Register(&claudeAdapter{}) }

const (
	// ClaudeAdapterName is the registry key and --adapter value.
	ClaudeAdapterName = "claude"
	// claudeExecutable is looked up on PATH unless the operator policy
	// sets harness.claude.path (plan §5).
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
	// claudeTransport names the session transport in the plan. Claude
	// Code speaks its own newline-delimited stream-json in both
	// directions; it is not ACP, so acpSession (docker-agent's driver)
	// does not apply — see DR-18.
	claudeTransport = "claude-stream-json"
	// claudeProtocolVersion tracks the shape of that protocol as this
	// adapter reads it.
	claudeProtocolVersion = 1
)

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
// both read on a Linux workstation; the Windows spellings follow the
// installer documentation (~\.local\bin\claude.exe) and npm's global shim,
// and are not verified here.
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

// Compile renders the instruction, the gateway-only MCP config and the
// host process that runs them.
func (c *claudeAdapter) Compile(in Input) (Result, error) {
	if in.Instruction == "" {
		return Result{}, errors.New("claude adapter: empty instruction")
	}
	model := in.Harness.Model
	if model == "" {
		model = in.Model.Name
	}
	if model == "" {
		return Result{}, errors.New("claude adapter: no model (set model.name, or harness.claude.model in the policy)")
	}
	keyEnv := policy.KeyEnv(in.Harness, in.Model)
	if keyEnv == "" {
		return Result{}, errors.New("claude adapter: no model key variable (set model.apiKeyEnv, or harness.claude.apiKeyEnv in the policy)")
	}

	instruction, resultKind := schemaInstruction(in.Instruction, in.SchemaJSON)

	cfg, err := json.MarshalIndent(claudeMCPConfig{MCPServers: map[string]claudeMCPServer{
		claudeGatewayServer: {
			Type: "http",
			// The gateway is published on 127.0.0.1 for a host harness;
			// the address is only known per run, so it is a placeholder
			// the engine renders (plan §5).
			URL: spec.PHHostGatewayURL,
			// ${VAR} is Claude Code's own expansion, from the process
			// environment: the token never appears in plan bytes.
			Headers: map[string]string{"Authorization": "Bearer ${" + spec.GatewayTokenEnv + "}"},
		},
	}}, "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("claude adapter: render %s: %w", claudeMCPFile, err)
	}
	cfg = append(cfg, '\n')

	mode := in.Mode
	if mode == "" {
		mode = manifest.ModeAutonomous
	}
	// One argv for both modes but for the input side: an autonomous run
	// takes its task on stdin and ends, a session reads user turns as
	// stream-json and stays up until stdin closes.
	argv := []string{
		"-p", "--bare", "--verbose",
		"--output-format", "stream-json",
		"--system-prompt-file", claudeInstructionFile,
		"--mcp-config", claudeMCPFile,
		"--strict-mcp-config",
		"--setting-sources", "",
		"--tools", "",
		"--allowedTools", "mcp__" + claudeGatewayServer,
		"--permission-mode", "dontAsk",
		"--permission-prompts", "none",
		"--no-session-persistence",
		"--disable-slash-commands",
		"--model", model,
	}
	var session *spec.SessionSpec
	stdinTask := true
	switch mode {
	case manifest.ModeAutonomous:
	case manifest.ModeInteractive:
		argv = append(argv, "--input-format", "stream-json")
		stdinTask = false
		session = &spec.SessionSpec{
			Transport:       claudeTransport,
			ProtocolVersion: claudeProtocolVersion,
		}
	default:
		return Result{}, fmt.Errorf("claude adapter: unknown run mode %q", mode)
	}

	env := []spec.EnvVar{
		{Name: spec.GatewayTokenEnv, FromRunToken: true},
		// The model endpoint is the operator's, by name (§R4). Claude
		// Code sends ANTHROPIC_AUTH_TOKEN as a bearer credential, which
		// is what an Anthropic-compatible endpoint expects.
		{Name: "ANTHROPIC_AUTH_TOKEN", FromHostEnv: keyEnv, FromKeyring: keyringAccount(in)},
	}
	if in.Harness.BaseURL != "" {
		env = append(env, spec.EnvVar{Name: "ANTHROPIC_BASE_URL", Value: in.Harness.BaseURL})
	}
	env = append(env,
		// The proxy is the intended route out. A host process is not
		// inside the run network, so this is a request the harness can
		// ignore; §6 measures whether it does and the register says so.
		spec.EnvVar{Name: "HTTPS_PROXY", Value: spec.PHHostProxyURL},
		spec.EnvVar{Name: "HTTP_PROXY", Value: spec.PHHostProxyURL},
		// The gateway and the proxy are both on 127.0.0.1: MCP traffic
		// must reach the gateway directly rather than through the proxy.
		spec.EnvVar{Name: "NO_PROXY", Value: "127.0.0.1,localhost"},
		spec.EnvVar{Name: "CLAUDE_CONFIG_DIR", Value: spec.PHCfg + "/harness/" + claudeConfigDirName},
		// Telemetry, error reporting, update checks and every other
		// background call off (§3 invariant 7): a sandboxed run should
		// talk to the model and the gateway, and to nothing else.
		spec.EnvVar{Name: "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", Value: "1"},
		spec.EnvVar{Name: "DISABLE_TELEMETRY", Value: "1"},
		spec.EnvVar{Name: "DISABLE_ERROR_REPORTING", Value: "1"},
		spec.EnvVar{Name: "DISABLE_AUTOUPDATER", Value: "1"},
		spec.EnvVar{Name: "DISABLE_BUG_COMMAND", Value: "1"},
	)

	return Result{
		Files: map[string][]byte{
			claudeInstructionFile: []byte(instruction),
			claudeMCPFile:         cfg,
		},
		Host: &spec.HostProcessSpec{
			Command: claudeExecutable,
			Argv:    argv,
			Env:     env,
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
		Model:      "anthropic/" + model,
		ResultKind: resultKind,
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
	} `json:"message"`
	// Result is the final answer text on a result message.
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	StopReason   string  `json:"stop_reason"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	// ToolName appears on the permission-denial system message.
	ToolName string `json:"tool_name"`
}

// claudeBlock is one content block of an assistant message.
type claudeBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"`
}

// Events maps one Claude Code stream-json line to normalized events.
// Best-effort by design (§3 invariant 5): an unrecognized shape becomes
// EventOther and never an error, and the receipts keep every raw line.
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
	case "result":
		return claudeResultEvents(msg, raw)
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
		}
		return []Event{{Kind: EventOther, Text: msg.Subtype, Raw: raw}}
	}
	return []Event{{Kind: EventOther, Text: msg.Type, Raw: raw}}
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
			calls = append(calls, Event{Kind: EventToolCall, Text: b.Name, Raw: raw})
		}
		// thinking / redacted_thinking are the model's reasoning: the
		// receipts keep the raw line, the result must not (agentry-cli#53).
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
// protocol (DR-18). SessionOptions.GatewayTools is not used: unlike ACP,
// this harness is not asked per call — the tool allow-list is compiled
// into the argv and the harness refuses everything else itself.
func (c *claudeAdapter) Session(_ context.Context, rw io.ReadWriteCloser, _ SessionOptions) (SessionDriver, error) {
	if rw == nil {
		return nil, errors.New("claude session: nil transport")
	}
	s := &claudeSession{
		rw:       rw,
		ad:       c,
		events:   make(chan Event, 256),
		readDone: make(chan struct{}),
	}
	go s.readLoop()
	return s, nil
}
