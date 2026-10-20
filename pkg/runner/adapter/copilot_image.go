// The copilot-image adapter: GitHub's Copilot CLI, unmodified, in an image
// built on this machine from GitHub's release download
// (pkg/runner/harnessbuild), run in the agent container as docker-agent is
// and driven through its JSON-RPC server mode (`--headless --stdio`) by the
// runner's own transport (copilot_session.go). It is the default whenever
// the policy's model is a Copilot plan (apiKeyKind copilot-signin), and
// runs on nothing else. There is no host adapter.
//
// How each adapter invariant holds:
//
//  1. Gateway-only tools: --available-tools mcp:* on the argv and
//     availableTools ["mcp:*"] in the session, Copilot CLI's allowlist,
//     which it classifies by source when a tool registers, so every
//     built-in (shell, files, web, sub-agents, ask_user, tool search) is
//     unloaded and a built-in a later release adds is too. The built-in
//     MCP servers are off (--disable-builtin-mcps) and the gateway is the
//     session's one MCP server.
//  2. Secrets by name: the plan holds no secret. The person's GitHub token
//     reaches Copilot CLI through the session's token callback, over its
//     stdio alone (spec.ModelSignInCallback), and the gateway's bearer in
//     the session's MCP server headers, sent by the driver: neither is in
//     the container's environment, a file or the plan.
//  3. One route out: the run network's egress proxy, which allows GitHub's
//     API and the plan's Copilot API host alone (policy.CopilotProxyAllow,
//     I2); telemetry and experiment hosts are denied.
//  4. Deterministic plan content: the instruction and the session's
//     configuration are plan files (instruction.md, session.json), which
//     the driver sends as they are, adding only the session's ids and the
//     bearer.
//  5. Best-effort events: the session's events are read for the dozen
//     kinds the runner needs; the rest are kept as they came.
//  6. The same envelope: tokens from assistant.usage; a plan run reports
//     no dollars. A quota or rate-limit error is the plan's limit
//     (I9); an organization's policy refusing the run is its own outcome.
//  7. Telemetry off: enableSessionTelemetry false, and the proxy denies
//     GitHub's telemetry hosts besides. GitHub's System Prompt is replaced
//     by the run's (systemMessage replace); no custom instructions,
//     skills, memory, hooks, plugins or remote agents.
//
// The container: the image's own user on a read-only root, the home an
// empty tmpfs (COPILOT_HOME there), Copilot CLI's unpacked package read
// from the image (it would otherwise unpack into the home, which docker
// mounts noexec), no keychain, no login shell, no auto-update, no auto
// login, no sandbox of its own (it would want namespaces the run's
// capabilities do not give it). The plan's folder is not mounted at all:
// the driver hands the harness what it needs over the session.
package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/blaktron/agentry-cli/pkg/runner/harnessbuild"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry/pkg/manifest"
)

func init() { Register(&copilotImageAdapter{}) }

const (
	// CopilotImageAdapterName is the registry key and --adapter value.
	CopilotImageAdapterName = harnessbuild.CopilotImageAdapter
	// copilotInstructionFile and copilotSessionFile are the plan's harness
	// files the driver reads (SessionOptions.HarnessFile).
	copilotInstructionFile = "instruction.md"
	copilotSessionFile     = "session.json"
	// copilotGatewayServer is the gateway's name in the session.
	copilotGatewayServer = "gateway"
	// copilotTransport names the drive in the plan.
	copilotTransport = "copilot-jsonrpc-stdio"
	// agentryClientName is the app's name to Copilot CLI (its clientName and
	// connect's editorName), as it is to Codex.
	agentryClientName = codexClientName
	// copilotProtocolVersion is the SDK protocol the driver speaks
	// (copilot-sdk's sdk-protocol-version.json, 3 in go/v1.0.19; Copilot
	// CLI 1.0.95 answers connect with 3).
	copilotProtocolVersion = 3
	// copilotImageUser is the image's own user, who owns a persistent
	// agent's session store.
	copilotImageUser = "copilot"
)

// copilotArgv is Copilot CLI's server mode, with everything it would do
// beyond answering the session switched off.
var copilotArgv = []string{
	"--headless", "--stdio",
	"--no-auto-update", "--no-auto-login", "--no-remote",
	"--disable-builtin-mcps", "--no-custom-instructions", "--no-sandbox",
	"--available-tools", copilotAvailableTools,
}

// copilotAvailableTools is the allowlist: the session's MCP servers' tools,
// which are the gateway's alone.
const copilotAvailableTools = "mcp:*"

// copilotImageAdapter implements Adapter for the Copilot CLI image. It is
// not a HostHarness: nothing of it is installed on the machine.
type copilotImageAdapter struct{}

func (c *copilotImageAdapter) Name() string { return CopilotImageAdapterName }

// ModelSignIns implements ModelSignInAdapter.
func (c *copilotImageAdapter) ModelSignIns() []string { return []string{policy.KeyKindCopilotSignIn} }

// copilotSessionConfig is session.json: session.create's parameters as the
// plan fixes them (copilot-sdk go/types.go createSessionRequest, and the
// defaults of its ModeEmpty, go/mode_empty.go). The driver adds the
// session's id, the token callback's registration, the instruction and the
// gateway's bearer.
type copilotSessionConfig struct {
	ClientName                         string                      `json:"clientName"`
	Model                              string                      `json:"model"`
	AvailableTools                     []string                    `json:"availableTools"`
	MCPServers                         map[string]copilotMCPServer `json:"mcpServers"`
	RequestPermission                  bool                        `json:"requestPermission"`
	EnableSessionTelemetry             bool                        `json:"enableSessionTelemetry"`
	SkipCustomInstructions             bool                        `json:"skipCustomInstructions"`
	CustomAgentsLocalOnly              bool                        `json:"customAgentsLocalOnly"`
	EnableSkills                       bool                        `json:"enableSkills"`
	EnableFileHooks                    bool                        `json:"enableFileHooks"`
	EnableHostGitOperations            bool                        `json:"enableHostGitOperations"`
	EnableSessionStore                 bool                        `json:"enableSessionStore"`
	EnableOnDemandInstructionDiscovery bool                        `json:"enableOnDemandInstructionDiscovery"`
	EnableExperimentalMode             bool                        `json:"enableExperimentalMode"`
	SkipEmbeddingRetrieval             bool                        `json:"skipEmbeddingRetrieval"`
	EmbeddingCacheStorage              string                      `json:"embeddingCacheStorage"`
	MCPOAuthTokenStorage               string                      `json:"mcpOAuthTokenStorage"`
	EnvValueMode                       string                      `json:"envValueMode"`
	Memory                             copilotEnabled              `json:"memory"`
	ToolSearch                         copilotEnabled              `json:"toolSearch"`
	Streaming                          bool                        `json:"streaming"`
}

type copilotEnabled struct {
	Enabled bool `json:"enabled"`
}

// copilotMCPServer is an HTTP MCP server in the session (copilot-sdk
// MCPHTTPServerConfig): its headers are the driver's to add.
type copilotMCPServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Tools   []string          `json:"tools"`
	Headers map[string]string `json:"headers,omitempty"`
}

// copilotModel is the model a run asks Copilot for: harness.copilot-image's,
// else the policy's ("auto" lets Copilot choose, the only choice a Free plan
// has).
func copilotModel(in Input) (string, error) {
	model := in.Harness.Model
	if model == "" {
		model = in.Model.Name
	}
	if model == "" {
		return "", fmt.Errorf("%s adapter: no model (set model.name, auto or a Copilot model id; or harness.%s.model in the policy)", CopilotImageAdapterName, CopilotImageAdapterName)
	}
	return model, nil
}

// Compile renders the instruction, the session's configuration and the
// agent container.
func (c *copilotImageAdapter) Compile(in Input) (Result, error) {
	if in.Instruction == "" {
		return Result{}, errors.New(CopilotImageAdapterName + " adapter: empty instruction")
	}
	if kind := policy.KeyKind(in.Harness, in.Model); kind != policy.KeyKindCopilotSignIn {
		return Result{}, &Refusal{Reason: fmt.Sprintf("the copilot-image adapter runs on your Copilot plan (policy apiKeyKind %s; agentry setup → copilot), and the policy pays with %q",
			policy.KeyKindCopilotSignIn, kind)}
	}
	if in.Harness.BaseURL != "" || in.Model.BaseURL != "" {
		return Result{}, &Refusal{Reason: "a Copilot plan is served by GitHub's own endpoints, which Copilot CLI finds itself, and the policy names a base URL: remove it"}
	}
	if in.GatewayURL == "" {
		return Result{}, errors.New(CopilotImageAdapterName + " adapter: no gateway URL")
	}
	model, err := copilotModel(in)
	if err != nil {
		return Result{}, err
	}
	instruction, resultKind := schemaInstruction(in.Instruction, in.SchemaJSON)
	cfg, err := json.MarshalIndent(copilotSessionConfig{
		ClientName:     agentryClientName,
		Model:          model,
		AvailableTools: []string{copilotAvailableTools},
		MCPServers: map[string]copilotMCPServer{
			copilotGatewayServer: {Type: "http", URL: in.GatewayURL, Tools: []string{"*"}},
		},
		RequestPermission:      true,
		SkipCustomInstructions: true,
		CustomAgentsLocalOnly:  true,
		SkipEmbeddingRetrieval: true,
		EmbeddingCacheStorage:  "in-memory",
		MCPOAuthTokenStorage:   "in-memory",
		EnvValueMode:           "direct",
	}, "", "  ")
	if err != nil {
		return Result{}, err
	}
	res := Result{
		Files: map[string][]byte{
			copilotInstructionFile: []byte(instruction),
			copilotSessionFile:     append(cfg, '\n'),
		},
		Agent:      copilotContainer(in.HarnessImage, copilotHome),
		Model:      "copilot/" + model,
		ResultKind: resultKind,
		ModelSignIn: &spec.ModelSignInSpec{
			Kind: policy.KeyKindCopilotSignIn, Provider: policy.ProviderCopilot, Delivery: spec.ModelSignInCallback,
		},
	}
	sess := &spec.SessionSpec{Transport: copilotTransport, ProtocolVersion: copilotProtocolVersion}
	switch in.Mode {
	case "", manifest.ModeAutonomous:
		res.OneTurn = sess
	case manifest.ModeInteractive:
		res.Session = sess
		if in.Persistent {
			// A persistent agent's conversation outlives its container:
			// COPILOT_HOME, where Copilot CLI keeps its sessions, is the
			// agent's session store, a volume given to the image's user,
			// rather than the tmpfs home.
			res.Agent = copilotContainer(in.HarnessImage, spec.SessionMount)
			sess.Mounts = []spec.Mount{{Source: spec.PHSession, Target: spec.SessionMount}}
			sess.StoreOwner = copilotImageUser
		}
	default:
		return Result{}, fmt.Errorf("copilot-image adapter: unknown run mode %q", in.Mode)
	}
	return res, nil
}

// copilotHome is COPILOT_HOME in the tmpfs home, for every run but a
// persistent agent's.
const copilotHome = harnessbuild.CopilotHome + "/.copilot"

// copilotContainer is Copilot CLI's container in image, keeping its state in
// home: a run's, and a model list's (CopilotListContainer).
func copilotContainer(image, home string) spec.ContainerSpec {
	env := []spec.EnvVar{
		{Name: "HOME", Value: harnessbuild.CopilotHome},
		{Name: "COPILOT_HOME", Value: home},
		{Name: "COPILOT_PKG_CACHE_HOME", Value: harnessbuild.CopilotPkgCache},
		// No keychain to reach, no login shell to copy an environment from,
		// no update to fetch.
		{Name: "COPILOT_DISABLE_KEYTAR", Value: "1"},
		{Name: "COPILOT_DISABLE_LOGIN_SHELL_ENV", Value: "1"},
		{Name: "COPILOT_AUTO_UPDATE", Value: "false"},
		{Name: "NO_COLOR", Value: "1"},
		// GitHub's API and the model go through the run's egress proxy; MCP
		// traffic stays direct on the run network.
		{Name: "HTTPS_PROXY", Value: spec.ProxyURL()},
		{Name: "HTTP_PROXY", Value: spec.ProxyURL()},
		{Name: "NO_PROXY", Value: spec.GatewayAlias + ",localhost,127.0.0.1"},
	}
	return spec.ContainerSpec{
		Image:        image,
		Entrypoint:   harnessbuild.CopilotBinary,
		Argv:         append([]string(nil), copilotArgv...),
		Env:          env,
		ReadOnlyRoot: true,
		Tmpfs:        []string{"/tmp:mode=1777", harnessbuild.CopilotHome + ":mode=1777"},
	}
}

// Events implements Adapter. Both modes are driven through the session
// driver, which normalizes the stream itself; a line read here is kept as
// it is.
func (c *copilotImageAdapter) Events(line []byte) []Event {
	return []Event{{Kind: EventOther, Raw: rawCopy(line)}}
}

// Session implements Adapter: the JSON-RPC driver of copilot_session.go.
func (c *copilotImageAdapter) Session(_ context.Context, rw io.ReadWriteCloser, opts SessionOptions) (SessionDriver, error) {
	return newCopilotSession(rw, opts)
}
