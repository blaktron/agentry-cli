// The claude-image adapter (plan model-sign-ins.md §4.4, D19–D23;
// agentry-cli#740): Anthropic's Claude Code, unmodified, in an image built
// on this machine from the npm packages this build pins
// (pkg/runner/harnessbuild), run in the agent container as docker-agent
// is. It is the default whenever the policy's model is a Claude
// subscription through Claude Code (apiKeyKind claude-signin), and runs on
// nothing else: an Anthropic key stays docker-agent's, or the installed
// claude's.
//
// What it shares with the host claude adapter: the gateway-only MCP
// config, the argv and the stream-json driver, so the §3 invariants of
// claude.go hold the same way (--strict-mcp-config with the gateway alone,
// --tools "", --setting-sources ""). What the container adds:
//
//   - Claude Code's own sign-in, kept in its config directory: a docker
//     volume of Agentry's own, which only Claude Code reads and writes,
//     mounted read-write at harnessbuild.ClaudeConfigDir by the compiler
//     (D20, I3, I4). The runner never reads it.
//   - No --bare (bare mode reads no OAuth credential) and no key variable
//     at all, so nothing outranks the sign-in: the container's environment
//     is the plan's alone, without ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN
//     or ANTHROPIC_BASE_URL, and --setting-sources "" loads no settings
//     that could name an apiKeyHelper.
//   - The run network's egress proxy, which allows Anthropic's API and the
//     host Claude Code refreshes its sign-in at, nothing else (I2), and
//     claude.ai's connectors off.
//   - The image's own user on a read-only root, the working directory an
//     empty tmpfs home: the ancestors whose CLAUDE.md a run without --bare
//     reads are the image's, so DR-26's refusal is not needed here.
package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

func init() { Register(&claudeImageAdapter{}) }

const (
	// ClaudeImageAdapterName is the registry key and --adapter value.
	ClaudeImageAdapterName = "claude-image"
	// claudeImageBinary is Claude Code in the image, where `npm install -g`
	// links it.
	claudeImageBinary = "/usr/local/bin/claude"
	// claudeImageHome is the image user's home, the working directory.
	claudeImageHome = "/home/node"
	// ClaudeImageConfigDir is where the image keeps Claude Code's config
	// directory, the sign-in's volume (harnessbuild.ClaudeConfigDir, which
	// the build makes for the image's user).
	ClaudeImageConfigDir = "/claude"
)

// claudeImageAdapter implements Adapter for the Claude Code image. It is
// not a HostHarness: nothing of it is installed on the machine.
type claudeImageAdapter struct{}

func (c *claudeImageAdapter) Name() string { return ClaudeImageAdapterName }

// ModelSignIns implements ModelSignInAdapter.
func (c *claudeImageAdapter) ModelSignIns() []string { return []string{policy.KeyKindClaudeSignIn} }

// Compile renders the instruction, the MCP config and the agent container.
func (c *claudeImageAdapter) Compile(in Input) (Result, error) {
	if in.Instruction == "" {
		return Result{}, errors.New("claude-image adapter: empty instruction")
	}
	if kind := policy.KeyKind(in.Harness, in.Model); kind != policy.KeyKindClaudeSignIn {
		return Result{}, &Refusal{Reason: fmt.Sprintf("the claude-image adapter runs on your Claude subscription (policy apiKeyKind %s; agentry setup → anthropic → claude), and the policy pays with %q: for an Anthropic key use the default adapter or --adapter claude",
			policy.KeyKindClaudeSignIn, kind)}
	}
	if in.Harness.BaseURL != "" {
		return Result{}, &Refusal{Reason: "a Claude subscription through Claude Code reaches Anthropic itself, and harness.claude-image.baseUrl names " + in.Harness.BaseURL + ": remove it"}
	}
	model, err := claudeModel(ClaudeImageAdapterName, in)
	if err != nil {
		return Result{}, err
	}
	instruction, resultKind := schemaInstruction(in.Instruction, in.SchemaJSON)
	cfg, err := claudeMCPJSON(in.GatewayURL)
	if err != nil {
		return Result{}, fmt.Errorf("claude-image adapter: %w", err)
	}
	argv, session, stdinTask, err := claudeArgv(in.Mode,
		spec.PlanMount+"/"+claudeInstructionFile, spec.PlanMount+"/"+claudeMCPFile, model, false)
	if err != nil {
		return Result{}, fmt.Errorf("claude-image adapter: %w", err)
	}
	env := []spec.EnvVar{
		{Name: spec.GatewayTokenEnv, FromRunToken: true},
		{Name: "HOME", Value: claudeImageHome},
		{Name: "CLAUDE_CONFIG_DIR", Value: ClaudeImageConfigDir},
		// The model and the sign-in's refresh go through the run's egress
		// proxy; MCP traffic stays direct on the run network.
		{Name: "HTTPS_PROXY", Value: spec.ProxyURL()},
		{Name: "HTTP_PROXY", Value: spec.ProxyURL()},
		{Name: "NO_PROXY", Value: spec.GatewayAlias + ",localhost,127.0.0.1"},
		// A claude.ai sign-in would otherwise fetch the account's own
		// connectors as MCP servers.
		{Name: "ENABLE_CLAUDEAI_MCP_SERVERS", Value: "false"},
	}
	env = append(env, claudeQuietEnv()...)
	return Result{
		Files: map[string][]byte{
			claudeInstructionFile: []byte(instruction),
			claudeMCPFile:         cfg,
		},
		Agent: spec.ContainerSpec{
			Image:        in.HarnessImage,
			Entrypoint:   claudeImageBinary,
			Argv:         argv,
			Env:          env,
			Mounts:       []spec.Mount{{Source: spec.PHCfg + "/harness", Target: spec.PlanMount, ReadOnly: true}},
			StdinTask:    stdinTask,
			ReadOnlyRoot: true,
			Tmpfs:        []string{"/tmp:mode=1777", claudeImageHome + ":mode=1777"},
		},
		Session:    session,
		Model:      "anthropic/" + model,
		ResultKind: resultKind,
		ModelSignIn: &spec.ModelSignInSpec{
			Kind: policy.KeyKindClaudeSignIn, Provider: policy.ProviderAnthropic,
			Delivery: spec.ModelSignInConfigDir, Path: ClaudeImageConfigDir,
		},
	}, nil
}

// Events implements Adapter: Claude Code's stream-json, as the host
// adapter reads it.
func (c *claudeImageAdapter) Events(line []byte) []Event { return (&claudeAdapter{}).Events(line) }

// Session implements Adapter: the stream-json driver the host adapter uses.
func (c *claudeImageAdapter) Session(ctx context.Context, rw io.ReadWriteCloser, opts SessionOptions) (SessionDriver, error) {
	return (&claudeAdapter{}).Session(ctx, rw, opts)
}
