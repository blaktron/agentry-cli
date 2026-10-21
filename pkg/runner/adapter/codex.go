// The codex adapter: the person's own installation of the OpenAI Codex CLI,
// driven as a host process from a run-scoped CODEX_HOME. On a key `codex
// exec --json` is the autonomous run; codex's own app-server protocol over
// stdio is the interactive session (the vendor's ACP package would be a
// second install), and both modes of a run paid by a ChatGPT plan
// (codex_image.go says why).
//
// The runner's invariants, by construction:
//
//   - Gateway-only tools. The generated config.toml declares one MCP
//     server, the run's gateway, and switches every built-in tool off:
//     the shell (features.shell_tool), the image viewer
//     (features.view_image), web search, apps, the browser, computer use,
//     image generation, sub-agents, hooks, memories, code mode, goals,
//     the question to the person (request_user_input). What is
//     left to the model is the gateway's catalog plus codex's own three MCP
//     resource readers, which read from that same gateway. --strict-config
//     makes a renamed key a loud failure rather than a silently re-enabled
//     tool. Some tools are not feature keys but come from the model's
//     catalog entry: codex's bundled catalog gives every real OpenAI model
//     apply_patch and deferred tool_search, and most of them code mode.
//     So the config points codex at a one-model catalog
//     the adapter renders (codexCatalog), which replaces the bundled one
//     for the process, and keeps the gateway's tools out of the deferred
//     surface, so none hides behind tool_search.
//   - Secrets by name. The gateway token reaches the config as the NAME
//     of the variable codex reads it from (bearer_token_env_var); the model
//     key likewise (env_key). No plan file holds a value.
//   - No ambient context. CODEX_HOME points inside the run, so none of the
//     person's config, rules, skills or sessions is read and nothing of
//     this run lands in theirs; --ignore-rules keeps their execpolicy out;
//     --ephemeral (an ephemeral thread, under app-server) persists no
//     session, but on the restart token route, whose thread is kept in the
//     run's own CODEX_HOME so it can be resumed. Codex's own bundled skills are
//     switched off too ([skills.bundled], [skills].include_instructions), so
//     the prompt lists none, and so are codex's permissions message and
//     environment context, which describe its own sandbox, not the run's
//     grant, and carry the host's working directory and timezone.
//   - Operator-owned model access. The model endpoint is a provider block
//     built from the policy: codex speaks the OpenAI Responses API and
//     nothing else in this release, and the policy's baseUrl, key
//     variable and model name are what it is given.
//
// Written against codex-cli 0.153.4 (codexProbeVersion); the flags, keys
// and event shapes were established by running it, and the conformance
// fixture is a recording of a real run. The plan-usage provider block, the
// auth command and the error shapes a ChatGPT plan meets were measured on
// 0.162.0, the Codex image's release.
package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"unicode"

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
	// codexCatalogFile is the rendered model catalog (codexCatalog); the
	// config names it as model_catalog_json.
	codexCatalogFile = codexHomeDir + "/models.json"
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
// codex (read on Linux); the Codex app on macOS carries its CLI at
// Contents/Resources/bin/codex (openai/codex#19041). The Windows spellings
// are the standalone layout with .exe and npm's global shim, not verified
// here.
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
	return `"` + TOMLEscape(s) + `"`
}

// TOMLEscape escapes s for the inside of a TOML basic string. The engine
// uses it too, for the host paths it substitutes into a rendered .toml file
// after the plan was compiled.
func TOMLEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`).Replace(s)
}

// codexRender is what codexConfig renders: the model, where the harness
// finds its instruction, catalog and gateway, and how it reaches the model.
type codexRender struct {
	model           string
	instructionPath string
	catalogPath     string
	gatewayURL      string
	baseURL         string
	// keyEnv is the variable a key is read from; "" for a model sign-in.
	keyEnv string
	// signInRoute is a model sign-in's token route (policy
	// signInTokenRoute): auth-command reads tokenPath with tokenReader
	// whenever codex needs a token; restart reads codexTokenEnv, set when
	// the harness starts.
	signInRoute string
	tokenReader string
	tokenPath   string
}

// codexTokenEnv is the variable a harness on the restart route is started
// with its access token in.
const codexTokenEnv = "CODEX_ACCESS_TOKEN"

// codexConfig renders the run-scoped config.toml. Hand-written rather
// than encoded: the shape is fixed, the byte order matters for the plan
// hash, and the runner takes no TOML dependency for six lines of it.
func codexConfig(r codexRender) []byte {
	var b strings.Builder
	b.WriteString("# generated by agentry run — do not edit (codex adapter)\n")
	b.WriteString("model_provider = " + tomlString(codexProviderID) + "\n")
	b.WriteString("model = " + tomlString(r.model) + "\n")
	// The instruction is the QuickAgent's, in place of codex's built-in
	// prompt; on a host the path is rendered per run (a plan carries no
	// host path).
	b.WriteString("model_instructions_file = " + tomlString(r.instructionPath) + "\n")
	// The model's metadata is the adapter's, never codex's bundled catalog
	// (codexCatalog); like the instruction, the path is rendered per run.
	b.WriteString("model_catalog_json = " + tomlString(r.catalogPath) + "\n")
	// Nothing prompts (there is no one to answer in an autonomous run),
	// and the gateway's own tools are auto-approved by the server block
	// below — a global "never" alone REFUSES an MCP tool call that would
	// otherwise ask ("requires approval, but approval policy is never",
	// measured), so the two settings work together.
	b.WriteString("approval_policy = \"never\"\n")
	b.WriteString("sandbox_mode = \"read-only\"\n")
	// Codex describes that sandbox to the model twice, in a permissions
	// message and in the environment context's permission profile. A model
	// told its sandbox is read-only never calls a granted write tool
	// (measured on 0.153.4 and 0.162.0), so neither is sent: the system
	// prompt says what the run may read and write.
	b.WriteString("include_permissions_instructions = false\n")
	b.WriteString("include_environment_context = false\n")
	b.WriteString("web_search = \"disabled\"\n")
	b.WriteString("\n[features]\n")
	for _, f := range []string{
		"shell_tool", "view_image", "apps", "browser_use", "computer_use",
		"image_generation", "multi_agent", "hooks", "memories", "skill_mcp_dependency_install",
		// Code mode adds a script tool that calls the others; the rendered
		// catalog already says "direct", and these are the second lock.
		"code_mode", "code_mode_only",
		// Goal tools (get/create/update_goal): offered by codex-cli
		// 0.162.0, the key already known to 0.153.4 (measured).
		"goals",
	} {
		b.WriteString(f + " = false\n")
	}
	b.WriteString("\n[analytics]\nenabled = false\n")
	b.WriteString("\n[otel]\nexporter = \"none\"\n")
	// Asking the person a question is a built-in tool too, on unless
	// switched off here (not a feature key; measured on 0.153.4).
	b.WriteString("\n[tools.experimental_request_user_input]\nenabled = false\n")
	// Codex installs its bundled system skills (imagegen, openai-docs, …)
	// into CODEX_HOME and lists them in a <skills_instructions> developer
	// message, though the model has no tool to use them: install none and
	// list none (measured on 0.153.4 and 0.162.0).
	b.WriteString("\n[skills]\ninclude_instructions = false\n")
	b.WriteString("\n[skills.bundled]\nenabled = false\n")
	b.WriteString("\n[model_providers." + codexProviderID + "]\n")
	b.WriteString("name = " + tomlString(codexProviderID) + "\n")
	b.WriteString("base_url = " + tomlString(r.baseURL) + "\n")
	if r.signInRoute == "" {
		b.WriteString("env_key = " + tomlString(r.keyEnv) + "\n")
		b.WriteString("wire_api = \"responses\"\n")
	} else {
		// A ChatGPT plan, as OpenAI documents it for Codex's app-server
		// (developers.openai.com/siwc/token-sharing-open-source/codex-app-server):
		// the Responses API, no OpenAI login of codex's own, no websocket.
		// No request is retried: a 429 is the plan's limit, which ends the
		// run as such and is never asked again (invariant I9).
		b.WriteString("wire_api = \"responses\"\n")
		b.WriteString("requires_openai_auth = false\n")
		b.WriteString("supports_websockets = false\n")
		b.WriteString("request_max_retries = 0\n")
		if r.signInRoute == policy.TokenRouteRestart {
			b.WriteString("env_key = " + tomlString(codexTokenEnv) + "\n")
		} else {
			// The token file is rewritten by the runner before the token
			// expires; codex runs the reader when it needs a token, at most
			// every five minutes, and again on a 401 (codex-rs, measured on
			// 0.162.0: the file's content arrives as the bearer).
			b.WriteString("\n[model_providers." + codexProviderID + ".auth]\n")
			b.WriteString("command = " + tomlString(r.tokenReader) + "\n")
			b.WriteString("args = [" + tomlString(r.tokenPath) + "]\n")
			b.WriteString("timeout_ms = 5000\n")
			b.WriteString("refresh_interval_ms = 300000\n")
		}
	}
	b.WriteString("\n[mcp_servers." + codexGatewayServer + "]\n")
	b.WriteString("url = " + tomlString(r.gatewayURL) + "\n")
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
	// Never deferred: a deferred tool reaches the model only through codex's
	// own tool_search, which is not the gateway's and which ChatGPT plan
	// usage refuses. With "deferred" omitted the tools are direct, and
	// codex registers no tool_search at all (none is deferred).
	b.WriteString("omit_tools_from = [\"deferred\"]\n")
	return []byte(b.String())
}

// codexModelInfo is one entry of a codex model catalog (codex-cli's
// ModelInfo, protocol/src/openai_models.rs). Field order is the bytes'
// order, which the plan hash covers.
type codexModelInfo struct {
	Slug                           string          `json:"slug"`
	DisplayName                    string          `json:"display_name"`
	Description                    *string         `json:"description"`
	SupportedReasoningLevels       []struct{}      `json:"supported_reasoning_levels"`
	ShellType                      string          `json:"shell_type"`
	Visibility                     string          `json:"visibility"`
	SupportedInAPI                 bool            `json:"supported_in_api"`
	Priority                       int             `json:"priority"`
	AvailabilityNux                *struct{}       `json:"availability_nux"`
	Upgrade                        *struct{}       `json:"upgrade"`
	IncludeSkillsUsageInstructions bool            `json:"include_skills_usage_instructions"`
	IncludePluginUsageInstructions bool            `json:"include_plugin_usage_instructions"`
	IncludeAppsUsageInstructions   bool            `json:"include_apps_usage_instructions"`
	DefaultReasoningSummary        string          `json:"default_reasoning_summary"`
	SupportVerbosity               bool            `json:"support_verbosity"`
	DefaultVerbosity               *string         `json:"default_verbosity"`
	ApplyPatchToolType             *string         `json:"apply_patch_tool_type"`
	WebSearchToolType              string          `json:"web_search_tool_type"`
	TruncationPolicy               codexTruncation `json:"truncation_policy"`
	ContextWindow                  int64           `json:"context_window"`
	MaxContextWindow               int64           `json:"max_context_window"`
	EffectiveContextWindowPercent  int64           `json:"effective_context_window_percent"`
	ExperimentalSupportedTools     []string        `json:"experimental_supported_tools"`
	SupportsSearchTool             bool            `json:"supports_search_tool"`
	ToolMode                       string          `json:"tool_mode"`
	// BaseInstructions is codex's legacy field for the model's built-in
	// prompt; a catalog entry must carry it or an instruction template, or
	// the whole catalog is refused. The run's model_instructions_file
	// replaces it, so it only has to be there.
	BaseInstructions string `json:"base_instructions"`
}

// codexToolResultBytes is the most of one tool result codex keeps for the
// model: the run's catalog entry's truncation policy, which codex 0.162.0
// applies to every MCP result (and which `tool_output_token_limit` would
// otherwise override). Codex's own fallback for an unknown model is 10,000
// bytes, which would hold the runner's pages to 5 KiB and a run reading a
// codebase to many times the calls; 48,000 is what codex's own catalog
// keeps for its models, and leaves the runner's 20 KiB pages room to double
// as escaped JSON (codexBudget).
const codexToolResultBytes = 48_000

type codexTruncation struct {
	Mode  string `json:"mode"`
	Limit int64  `json:"limit"`
}

// codexCatalog renders the run's model catalog: the run's model alone, so
// codex's longest-prefix lookup finds it exactly, with the values of codex's
// own fallback metadata for an unknown model (model_info_from_slug in
// codex-cli 0.153.4, the shape every recording of this adapter ran on; a
// real OpenAI model therefore gets the API's default reasoning effort and
// verbosity rather than codex's tuned ones) except the tool fields,
// which are pinned off: no apply_patch, no tool_search, no experimental
// tools, no shell (also off by feature), and direct tools rather than code
// mode; and the truncation policy, which is the runner's
// (codexToolResultBytes). A catalog in the config replaces the bundled one and is
// authoritative: codex fetches no model list from the provider.
func codexCatalog(model string) ([]byte, error) {
	entry := codexModelInfo{
		Slug:                          model,
		DisplayName:                   model,
		SupportedReasoningLevels:      []struct{}{},
		ShellType:                     "disabled",
		Visibility:                    "none",
		SupportedInAPI:                true,
		Priority:                      99,
		DefaultReasoningSummary:       "auto",
		WebSearchToolType:             "text",
		TruncationPolicy:              codexTruncation{Mode: "bytes", Limit: codexToolResultBytes},
		ContextWindow:                 272_000,
		MaxContextWindow:              272_000,
		EffectiveContextWindowPercent: 95,
		ExperimentalSupportedTools:    []string{},
		ToolMode:                      "direct",
		BaseInstructions:              "Follow the instructions you are given.",
	}
	out, err := json.MarshalIndent(struct {
		Models []codexModelInfo `json:"models"`
	}{[]codexModelInfo{entry}}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("codex adapter: model catalog: %w", err)
	}
	return append(out, '\n'), nil
}

// codexCommon is what both codex adapters resolve before they compile:
// the model, the instruction and catalog files, and the result kind.
type codexCommon struct {
	model, instruction, resultKind string
	catalog                        []byte
	mode                           manifest.Mode
}

func codexResolve(name string, in Input) (codexCommon, error) {
	if in.Instruction == "" {
		return codexCommon{}, fmt.Errorf("%s adapter: empty instruction", name)
	}
	model := in.Harness.Model
	if model == "" {
		model = in.Model.Name
	}
	if model == "" {
		return codexCommon{}, fmt.Errorf("%s adapter: no model (set model.name, or harness.%s.model in the policy)", name, name)
	}
	// The model id is written into config.toml and models.json, which the
	// engine renders by substituting its @@…@@ placeholders; an id that
	// carried one, or a control character, would change the file it lands
	// in. No model id does either.
	if strings.Contains(model, "@@") || strings.IndexFunc(model, unicode.IsControl) >= 0 {
		return codexCommon{}, fmt.Errorf("%s adapter: %q is not a model id", name, model)
	}
	instruction, resultKind := schemaInstruction(in.Instruction, in.SchemaJSON)
	catalog, err := codexCatalog(model)
	if err != nil {
		return codexCommon{}, err
	}
	mode := in.Mode
	if mode == "" {
		mode = manifest.ModeAutonomous
	}
	if mode != manifest.ModeAutonomous && mode != manifest.ModeInteractive {
		return codexCommon{}, fmt.Errorf("%s adapter: unknown run mode %q", name, mode)
	}
	return codexCommon{model: model, instruction: instruction, resultKind: resultKind, catalog: catalog, mode: mode}, nil
}

// codexBaseURL is the endpoint a key run's provider block points at.
func codexBaseURL(in Input) string {
	if in.Harness.BaseURL != "" {
		return in.Harness.BaseURL
	}
	if in.Model.BaseURL != "" {
		return in.Model.BaseURL
	}
	return codexDefaultBaseURL
}

// codexSignIn is the model sign-in a codex adapter compiles for, "" for a
// key, or the refusal of one it cannot spend. A ChatGPT plan's endpoint is
// OpenAI's own, so a policy that points the harness elsewhere is refused.
func codexSignIn(name string, in Input) (string, error) {
	kind := policy.KeyKind(in.Harness, in.Model)
	if !policy.IsModelSignIn(kind) {
		return "", nil
	}
	if kind != policy.KeyKindChatGPTPlan {
		return "", &Refusal{Reason: fmt.Sprintf("the %s adapter cannot spend %s: choose its own adapter, or an API key", name, policy.SignInSentence(kind))}
	}
	for _, u := range []string{in.Harness.BaseURL, in.Model.BaseURL} {
		if u != "" && strings.TrimRight(u, "/") != codexDefaultBaseURL {
			return "", &Refusal{Reason: fmt.Sprintf("a run paid by %s goes to %s alone, and the policy names %s: remove model.baseUrl and harness.%s.baseUrl", policy.SignInSentence(kind), codexDefaultBaseURL, u, name)}
		}
	}
	return kind, nil
}

// codexSessionSpec is the app-server drive, for a session or one turn.
func codexSessionSpec() *spec.SessionSpec {
	return &spec.SessionSpec{Transport: codexTransport, ProtocolVersion: codexProtocolVersion}
}

// hostOS is the operating system a host harness runs on; a test sets it.
var hostOS = runtime.GOOS

// ModelSignIns implements ModelSignInAdapter: the person's installed Codex
// runs on a ChatGPT plan when the policy picks this adapter.
func (c *codexAdapter) ModelSignIns() []string { return []string{policy.KeyKindChatGPTPlan} }

// Compile renders the instruction, the config and the host process. On a
// key the autonomous run is `codex exec`; on a ChatGPT plan both modes are
// app-server, whose requests are the ones OpenAI serves for plan usage
// (exec presents another originator), and the token is read from a file in
// the run's own folder.
func (c *codexAdapter) Compile(in Input) (Result, error) {
	cc, err := codexResolve(CodexAdapterName, in)
	if err != nil {
		return Result{}, err
	}
	signIn, err := codexSignIn(CodexAdapterName, in)
	if err != nil {
		return Result{}, err
	}
	r := codexRender{
		model: cc.model, instructionPath: spec.PHCfg + "/harness/" + codexInstructionFile,
		catalogPath: spec.PHCfg + "/harness/" + codexCatalogFile, gatewayURL: spec.PHHostGatewayURL,
	}
	env := []spec.EnvVar{{Name: spec.GatewayTokenEnv, FromRunToken: true}}
	var msi *spec.ModelSignInSpec
	if signIn == "" {
		keyEnv := policy.KeyEnv(in.Harness, in.Model)
		if keyEnv == "" {
			return Result{}, errors.New("codex adapter: no model key variable (set model.apiKeyEnv, or harness.codex.apiKeyEnv in the policy)")
		}
		r.keyEnv, r.baseURL = keyEnv, codexBaseURL(in)
		// The key under the very name the config's env_key reads.
		env = append(env, spec.EnvVar{Name: keyEnv, FromHostEnv: keyEnv, FromKeyring: keyringAccount(in)})
	} else {
		// The token file is read with cat, which a Windows host has not
		// got: there the Codex image runs a ChatGPT plan.
		if hostOS == "windows" {
			return Result{}, &Refusal{Reason: "the installed Codex cannot spend your ChatGPT plan on Windows yet: run the Codex image instead (agentry run --adapter " + CodexImageAdapterName + ")"}
		}
		r.baseURL, r.signInRoute = codexDefaultBaseURL, policy.SignInTokenRoute(in.Harness)
		r.tokenReader, r.tokenPath = "cat", spec.PHModelSignIn+"/"+spec.ModelSignInTokenFile
		if r.signInRoute == policy.TokenRouteRestart {
			env = append(env, spec.EnvVar{Name: codexTokenEnv, FromModelSignIn: true})
		}
		msi = &spec.ModelSignInSpec{Kind: signIn, Provider: policy.SignInProvider(signIn), Delivery: spec.ModelSignInToken, Path: r.tokenPath}
		if r.signInRoute == policy.TokenRouteRestart {
			msi.Env = codexTokenEnv
		}
	}
	env = append(env,
		spec.EnvVar{Name: "CODEX_HOME", Value: spec.PHCfg + "/harness/" + codexHomeDir},
		spec.EnvVar{Name: "HTTPS_PROXY", Value: spec.PHHostProxyURL},
		spec.EnvVar{Name: "HTTP_PROXY", Value: spec.PHHostProxyURL},
		spec.EnvVar{Name: "NO_PROXY", Value: "127.0.0.1,localhost"},
	)

	res := Result{
		Files: map[string][]byte{
			codexInstructionFile: []byte(cc.instruction),
			codexConfigFile:      codexConfig(r),
			codexCatalogFile:     cc.catalog,
		},
		Host: &spec.HostProcessSpec{
			Command:     codexExecutable,
			Env:         env,
			InheritEnv:  []string{"HOME", "PATH"},
			VersionArgv: []string{"--version"},
		},
		Model:       "openai/" + cc.model,
		ResultKind:  cc.resultKind,
		ModelSignIn: msi,
	}
	switch {
	case cc.mode == manifest.ModeInteractive:
		res.Host.Argv = []string{"app-server", "--strict-config"}
		res.Session = codexSessionSpec()
		res.Session.RestartOnRenewal = r.signInRoute == policy.TokenRouteRestart
	case signIn != "":
		res.Host.Argv = []string{"app-server", "--strict-config"}
		res.OneTurn = codexSessionSpec()
	default:
		// The task rides stdin ("-"); JSONL events on stdout; no session
		// persisted; a config key this release does not know is a
		// refusal, not a silently re-enabled tool.
		res.Host.Argv = []string{"exec", "--json", "--ephemeral", "--skip-git-repo-check",
			"--strict-config", "--ignore-rules", "--color", "never", "-"}
		res.Host.StdinTask = true
	}
	return res, nil
}

// codexExecEvent is the part of one `codex exec --json` line this adapter
// reads. Everything else stays verbatim in the receipts.
type codexExecEvent struct {
	Type  string    `json:"type"`
	Item  codexItem `json:"item"`
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

// codexItem is the part of a thread item this adapter reads, from exec's
// JSONL and the app-server's notifications alike: a message's text, an
// error's message, an MCP tool call's tool, status, result and error.
type codexItem struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Text    string `json:"text"`
	Message string `json:"message"`
	Tool    string `json:"tool"`
	Server  string `json:"server"`
	Status  string `json:"status"`
	// Result and Error are a tool call's, read only for one: they stay raw
	// so that another item's field of the same name never fails the
	// whole line's decoding.
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// codexToolResultEvent maps a completed MCP tool call, from exec or the
// app-server: its result's text blocks, or its error's message when it
// failed, and whether the result held an embedded resource (joinText).
func codexToolResultEvent(it codexItem, raw json.RawMessage) Event {
	var res struct {
		Content []textBlock `json:"content"`
	}
	var cerr struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(it.Result, &res)
	_ = json.Unmarshal(it.Error, &cerr)
	text, redacted := joinText(res.Content)
	ev := Event{Kind: EventToolResult, Tool: it.Tool, CallID: it.ID, Failed: it.Status == "failed" || cerr.Message != "", Text: text, Redacted: redacted, Raw: raw}
	if ev.Text == "" {
		ev.Text = cerr.Message
	}
	return ev
}

// Events maps one exec JSONL line to normalized events. Best-effort: the
// unknown becomes EventOther, never an error.
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
			return []Event{{Kind: EventToolCall, Text: ev.Item.Tool, Tool: ev.Item.Tool, CallID: ev.Item.ID, Raw: raw}}
		}
	case "item.completed":
		switch ev.Item.Type {
		case "mcp_tool_call":
			return []Event{codexToolResultEvent(ev.Item, raw)}
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

// Session drives an interactive run over codex's app-server protocol. The
// runner is the approval authority: an MCP approval for a gateway tool is
// accepted, anything else is declined.
func (c *codexAdapter) Session(_ context.Context, rw io.ReadWriteCloser, opts SessionOptions) (SessionDriver, error) {
	if rw == nil {
		return nil, errors.New("codex session: nil transport")
	}
	allowed := make(map[string]bool, len(opts.GatewayTools))
	for _, t := range opts.GatewayTools {
		allowed[t] = true
	}
	s := &codexSession{
		rw:         rw,
		signIn:     opts.ModelSignIn,
		resumable:  opts.Resumable || opts.Persistent,
		continueID: opts.ContinueID,
		pending:    map[int]chan rpcMessage{},
		events:     make(chan Event, 256),
		readDone:   make(chan struct{}),
		permit: func(server, tool string) bool {
			if server != codexGatewayServer {
				return false
			}
			// No tool name in the request means we cannot check the
			// catalog, but the server is ours and the catalog is all it
			// serves; a named tool must be one the gateway serves
			// (SessionOptions.GatewayTools).
			return tool == "" || allowed[tool]
		},
	}
	go s.readLoop(rw, s.readDone)
	return s, nil
}
