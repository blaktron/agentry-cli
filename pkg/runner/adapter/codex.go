// The codex adapter (plan runner-m-r4-adapters §4.2, agentry-cli#81): the
// person's own installation of the OpenAI Codex CLI, driven as a host
// process from a run-scoped CODEX_HOME (decision D2). `codex exec --json`
// is the autonomous run; codex's own app-server protocol over stdio is
// the interactive session (DR-18's reasoning applies: the vendor's ACP
// package would be a second install).
//
// The §3 invariants, by construction:
//
//   - Gateway-only tools. The generated config.toml declares one MCP
//     server, the run's gateway, and switches every built-in tool off:
//     the shell (features.shell_tool), the image viewer
//     (features.view_image), web search, apps, the browser, computer use,
//     image generation, sub-agents, hooks, memories. What is left to the
//     model is the gateway's catalog plus codex's own three MCP resource
//     readers, which read from that same gateway. --strict-config makes a
//     renamed key a loud failure rather than a silently re-enabled tool.
//   - Secrets by name. The gateway token reaches the config as the NAME
//     of the variable codex reads it from (bearer_token_env_var); the model
//     key likewise (env_key). No plan file holds a value.
//   - No ambient context. CODEX_HOME points inside the run, so none of the
//     person's config, rules, skills or sessions is read and nothing of
//     this run lands in theirs; --ignore-rules keeps their execpolicy out;
//     --ephemeral persists no session.
//   - Operator-owned model access. The model endpoint is a provider block
//     built from the policy: codex speaks the OpenAI Responses API and
//     nothing else in this release, and the policy's baseUrl, key
//     variable and model name are what it is given.
//
// Written against codex-cli 0.153.4 (codexProbeVersion); the flags, keys
// and event shapes were established by running it, and the conformance
// fixture is a recording of a real run.
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

func init() { Register(&codexAdapter{}) }

const (
	// CodexAdapterName is the registry key and --adapter value.
	CodexAdapterName = "codex"
	codexExecutable  = "codex"
	// codexProbeVersion is the release this adapter was written against;
	// `agentry doctor --adapter codex` warns on another, never refuses.
	codexProbeVersion = "0.153.4"
	// codexInstructionFile is the plan file the config names as the
	// model instructions (replacing codex's built-in prompt); codexHomeDir
	// is the run-scoped CODEX_HOME, and its config.toml is a plan file too.
	codexInstructionFile = "instruction.md"
	codexHomeDir         = ".codex-home"
	codexConfigFile      = codexHomeDir + "/config.toml"
	// codexGatewayServer is the MCP server id in the config, and the
	// serverName an approval elicitation carries.
	codexGatewayServer = "gateway"
	// codexProviderID names the model provider block built from the policy.
	codexProviderID = "agentry"
	// codexDefaultBaseURL is where the provider block points when the
	// policy names no endpoint.
	codexDefaultBaseURL = "https://api.openai.com/v1"
	// codexTransport names the session transport in the plan.
	codexTransport       = "codex-app-server"
	codexProtocolVersion = 2
)

// codexAdapter implements Adapter for the installed Codex CLI.
type codexAdapter struct{}

func (c *codexAdapter) Name() string { return CodexAdapterName }

// Executable, ProbeVersion and VersionArgv implement HostHarness.
func (c *codexAdapter) Executable() string    { return codexExecutable }
func (c *codexAdapter) ProbeVersion() string  { return codexProbeVersion }
func (c *codexAdapter) VersionArgv() []string { return []string{"--version"} }
func (c *codexAdapter) InstallURL() string    { return "https://github.com/openai/codex" }
func (c *codexAdapter) DisplayName() string   { return "Codex" }

// WellKnownPaths implements HostHarness. The standalone installer's
// ~/.local/bin/codex links to ~/.codex/packages/standalone/current/bin/
// codex (read on a Linux workstation); the Codex app on macOS carries its
// CLI at Contents/Resources/bin/codex (openai/codex#19041). The Windows
// spellings are the standalone layout with .exe and npm's global shim,
// not verified here.
func (c *codexAdapter) WellKnownPaths(goos string) []string {
	switch goos {
	case "windows":
		return []string{`~/.local/bin/codex.exe`, `~/.codex/packages/standalone/current/bin/codex.exe`, `${APPDATA}/npm/codex.cmd`}
	case "darwin":
		return []string{"~/.local/bin/codex", "~/.codex/packages/standalone/current/bin/codex", "/Applications/Codex.app/Contents/Resources/bin/codex"}
	default:
		return []string{"~/.local/bin/codex", "~/.codex/packages/standalone/current/bin/codex"}
	}
}

func (c *codexAdapter) MCPCommand() string {
	return "codex mcp add agentry -- node /path/to/agentry-mcp/dist/index.js"
}

// tomlString quotes a value as a TOML basic string. The values here are
// model ids, URLs and variable names; the escape covers what such values
// could carry.
func tomlString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

// codexConfig renders the run-scoped config.toml. Hand-written rather
// than encoded: the shape is fixed, the byte order matters for the plan
// hash, and the runner takes no TOML dependency for six lines of it.
func codexConfig(model, baseURL, keyEnv string) []byte {
	var b strings.Builder
	b.WriteString("# generated by agentry run — do not edit (RUNNER-PLAN §R10, codex adapter)\n")
	b.WriteString("model_provider = " + tomlString(codexProviderID) + "\n")
	b.WriteString("model = " + tomlString(model) + "\n")
	// The instruction is the BriefAgent's, in place of codex's built-in
	// prompt; the path is rendered per run (a plan carries no host path).
	b.WriteString("model_instructions_file = " + tomlString(spec.PHCfg+"/harness/"+codexInstructionFile) + "\n")
	// Nothing prompts (there is no one to answer in an autonomous run),
	// and the gateway's own tools are auto-approved by the server block
	// below — a global "never" alone REFUSES an MCP tool call that would
	// otherwise ask ("requires approval, but approval policy is never",
	// measured), so the two settings work together.
	b.WriteString("approval_policy = \"never\"\n")
	b.WriteString("sandbox_mode = \"read-only\"\n")
	b.WriteString("web_search = \"disabled\"\n")
	b.WriteString("\n[features]\n")
	for _, f := range []string{
		"shell_tool", "view_image", "apps", "browser_use", "computer_use",
		"image_generation", "multi_agent", "hooks", "memories", "skill_mcp_dependency_install",
	} {
		b.WriteString(f + " = false\n")
	}
	b.WriteString("\n[analytics]\nenabled = false\n")
	b.WriteString("\n[otel]\nexporter = \"none\"\n")
	b.WriteString("\n[model_providers." + codexProviderID + "]\n")
	b.WriteString("name = " + tomlString(codexProviderID) + "\n")
	b.WriteString("base_url = " + tomlString(baseURL) + "\n")
	b.WriteString("env_key = " + tomlString(keyEnv) + "\n")
	b.WriteString("wire_api = \"responses\"\n")
	b.WriteString("\n[mcp_servers." + codexGatewayServer + "]\n")
	b.WriteString("url = " + tomlString(spec.PHHostGatewayURL) + "\n")
	b.WriteString("bearer_token_env_var = " + tomlString(spec.GatewayTokenEnv) + "\n")
	// The gateway is the run's enforcement boundary: its catalog is the
	// whole tool surface and the tools container enforces each declared
	// capability, so every gateway call is meant to run. "approve"
	// auto-approves this server's tools — codex's own rule is that an MCP
	// call runs without a prompt only when its approval mode is Approve,
	// or when the policy is "never" AND the sandbox grants full disk write
	// (mcp_permission_prompt_is_auto_approved); our sandbox is read-only,
	// so the mode is what carries it. "auto" is a different value that does
	// NOT unconditionally approve. An autonomous run has no one to answer a
	// prompt; an interactive session still routes approvals to the runner
	// when the harness sends them (the session driver).
	b.WriteString("default_tools_approval_mode = \"approve\"\n")
	return []byte(b.String())
}

// Compile renders the instruction, the config and the host process.
func (c *codexAdapter) Compile(in Input) (Result, error) {
	if in.Instruction == "" {
		return Result{}, errors.New("codex adapter: empty instruction")
	}
	model := in.Harness.Model
	if model == "" {
		model = in.Model.Name
	}
	if model == "" {
		return Result{}, errors.New("codex adapter: no model (set model.name, or harness.codex.model in the policy)")
	}
	keyEnv := policy.KeyEnv(in.Harness, in.Model)
	if keyEnv == "" {
		return Result{}, errors.New("codex adapter: no model key variable (set model.apiKeyEnv, or harness.codex.apiKeyEnv in the policy)")
	}
	baseURL := in.Harness.BaseURL
	if baseURL == "" {
		baseURL = in.Model.BaseURL
	}
	if baseURL == "" {
		baseURL = codexDefaultBaseURL
	}
	instruction, resultKind := schemaInstruction(in.Instruction, in.SchemaJSON)

	mode := in.Mode
	if mode == "" {
		mode = manifest.ModeAutonomous
	}
	var argv []string
	var session *spec.SessionSpec
	stdinTask := false
	switch mode {
	case manifest.ModeAutonomous:
		// The task rides stdin ("-"); JSONL events on stdout; no session
		// persisted; a config key this release does not know is a
		// refusal, not a silently re-enabled tool.
		argv = []string{"exec", "--json", "--ephemeral", "--skip-git-repo-check",
			"--strict-config", "--ignore-rules", "--color", "never", "-"}
		stdinTask = true
	case manifest.ModeInteractive:
		argv = []string{"app-server", "--strict-config"}
		session = &spec.SessionSpec{Transport: codexTransport, ProtocolVersion: codexProtocolVersion}
	default:
		return Result{}, fmt.Errorf("codex adapter: unknown run mode %q", mode)
	}

	env := []spec.EnvVar{
		{Name: spec.GatewayTokenEnv, FromRunToken: true},
		// The key under the very name the config's env_key reads.
		{Name: keyEnv, FromHostEnv: keyEnv, FromKeyring: keyringAccount(in)},
		{Name: "CODEX_HOME", Value: spec.PHCfg + "/harness/" + codexHomeDir},
		{Name: "HTTPS_PROXY", Value: spec.PHHostProxyURL},
		{Name: "HTTP_PROXY", Value: spec.PHHostProxyURL},
		{Name: "NO_PROXY", Value: "127.0.0.1,localhost"},
	}

	return Result{
		Files: map[string][]byte{
			codexInstructionFile: []byte(instruction),
			codexConfigFile:      codexConfig(model, baseURL, keyEnv),
		},
		Host: &spec.HostProcessSpec{
			Command:     codexExecutable,
			Argv:        argv,
			Env:         env,
			InheritEnv:  []string{"HOME", "PATH"},
			VersionArgv: []string{"--version"},
			StdinTask:   stdinTask,
		},
		Session:    session,
		Model:      "openai/" + model,
		ResultKind: resultKind,
	}, nil
}

// codexExecEvent is the part of one `codex exec --json` line this adapter
// reads. Everything else stays verbatim in the receipts.
type codexExecEvent struct {
	Type string `json:"type"`
	Item struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Message string `json:"message"`
		Tool    string `json:"tool"`
		Status  string `json:"status"`
	} `json:"item"`
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

// Events maps one exec JSONL line to normalized events. Best-effort (§3
// invariant 5): the unknown becomes EventOther, never an error.
func (c *codexAdapter) Events(line []byte) []Event {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return nil
	}
	raw := rawCopy([]byte(trimmed))
	var ev codexExecEvent
	if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
		// stdout is strictly JSONL under --json; anything else is noise,
		// never the answer.
		return []Event{{Kind: EventOther, Text: trimmed, Raw: raw}}
	}
	switch ev.Type {
	case "item.started":
		if ev.Item.Type == "mcp_tool_call" {
			// One call counted when it begins, so --max-turns stops the
			// run before the next call executes.
			return []Event{{Kind: EventToolCall, Text: ev.Item.Tool, Raw: raw}}
		}
	case "item.completed":
		switch ev.Item.Type {
		case "agent_message":
			if ev.Item.Text != "" {
				return []Event{{Kind: EventAssistantText, Text: ev.Item.Text, Raw: raw}}
			}
		case "error":
			// codex reports its warnings as error items too (a model with
			// no metadata, for one); the engine only surfaces the last of
			// these if the process then exits non-zero.
			return []Event{{Kind: EventError, Text: ev.Item.Message, Raw: raw}}
		case "reasoning":
			// The model's reasoning stays in the receipts, out of the result.
			return []Event{{Kind: EventOther, Raw: raw}}
		}
	case "turn.completed":
		return []Event{{Kind: EventUsage, InputTokens: ev.Usage.InputTokens, OutputTokens: ev.Usage.OutputTokens, Raw: raw}}
	case "turn.failed", "error":
		msg := ev.Error.Message
		if msg == "" {
			msg = ev.Message
		}
		if msg == "" {
			msg = ev.Type
		}
		return []Event{{Kind: EventError, Text: msg, Raw: raw}}
	}
	return []Event{{Kind: EventOther, Text: ev.Type, Raw: raw}}
}

// Session drives an interactive run over codex's app-server protocol.
// The runner is the approval authority: an MCP approval for a gateway
// tool is accepted, anything else is declined (DR-15).
func (c *codexAdapter) Session(_ context.Context, rw io.ReadWriteCloser, opts SessionOptions) (SessionDriver, error) {
	if rw == nil {
		return nil, errors.New("codex session: nil transport")
	}
	allowed := make(map[string]bool, len(opts.GatewayTools))
	for _, t := range opts.GatewayTools {
		allowed[t] = true
	}
	s := &codexSession{
		rw:       rw,
		pending:  map[int]chan rpcMessage{},
		events:   make(chan Event, 256),
		readDone: make(chan struct{}),
		permit: func(server, tool string) bool {
			if server != codexGatewayServer {
				return false
			}
			// No tool name in the request means we cannot check the
			// catalog, but the server is ours and the catalog is all it
			// serves; a named tool must be in the plan's grant.
			return tool == "" || allowed[tool]
		},
	}
	go s.readLoop()
	return s, nil
}
