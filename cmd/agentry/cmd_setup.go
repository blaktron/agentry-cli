// `agentry setup` (RUNNER-PLAN-2 §P1): prepare a machine for QuickAgent
// runs before any agent is involved — configure the operator's preferred
// LLM provider into the policy, then stand up the docker-mcp-gateway plane
// (pull pinned images, build the runner image) right away instead of
// lazily at first agent use. Idempotent; `agentry run` still self-heals,
// so setup is a convenience, not a gate.
package main

import (
	"bufio"
	"fmt"
	"github.com/blaktron/agentry-cli/runnerimage"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
)

// setupOpts carries the setup command's flags.
type setupOpts struct {
	provider     string
	model        string
	apiKeyEnv    string
	apiKeySource string
	apiKeyKind   string
	baseURL      string
	copilotPlan  string
	oauthApp     bool
	connect      bool
	policyPath   string
	yes          bool
	skipImages   bool
}

// newSetupCmd builds the setup command.
func newSetupCmd() *cobra.Command {
	var opts setupOpts
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Prepare this machine for QuickAgent runs (LLM provider + docker MCP gateway)",
		Long: "One-time (and re-runnable) machine setup for the QuickAgent runner\n" +
			"(RUNNER-PLAN-2 §P1):\n\n" +
			"  1. Configure your preferred LLM provider for agent runs into the operator\n" +
			"     policy — interactively on a terminal, or from --provider/--model/\n" +
			"     --api-key-env/--base-url with --yes. The key itself is never stored;\n" +
			"     only the env var NAME that will hold it.\n" +
			"  2. Configure the docker-mcp-gateway plane right away: verify the docker\n" +
			"     engine, pull the pinned gateway and harness images, and build the local\n" +
			"     runner image from the embedded context.\n\n" +
			"Re-running updates the model block and re-verifies images; everything else\n" +
			"in the policy is preserved. Note: setup rewrites the policy file as\n" +
			"generated YAML, so hand-written comments in it are not kept.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSetup(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.provider, "provider", "", "LLM provider: anthropic | openai | google | openrouter | poe | huggingface (a key, or --connect your account) | copilot (your Copilot plan, Sign in with GitHub) | custom (or any docker-agent provider name)")
	f.StringVar(&opts.model, "model", "", "model id (default: the provider's table default)")
	f.StringVar(&opts.apiKeyEnv, "api-key-env", "", "env var NAME holding your api key (never the value)")
	f.StringVar(&opts.apiKeySource, "api-key-source", "", "where the key comes from when the variable is unset: env (the default) | keyring (the OS keychain entry provider:<provider>; an interactive setup offers to store it)")
	f.StringVar(&opts.apiKeyKind, "api-key-kind", "", "what pays for the model: api (an API key, the default) | subscription (an Anthropic `claude setup-token` token — Claude Code only; derived from the key when this setup stores it) | chatgpt-plan (provider openai: your ChatGPT plan, signed in with ChatGPT; interactive setup asks) | claude-signin (provider anthropic: Use your Claude subscription (runs Claude Code): Claude Code signs you in itself; interactive setup asks) | copilot-signin (provider copilot, its only one: Sign in with GitHub)")
	f.StringVar(&opts.baseURL, "base-url", "", "custom endpoint base URL (derives the egress-proxy allowlist)")
	f.BoolVar(&opts.oauthApp, "oauth-app", false, "provider copilot: sign in through Agentry's OAuth App rather than its GitHub App (the fallback, plan D26)")
	f.BoolVar(&opts.connect, "connect", false, "provider openrouter, poe or huggingface: "+setup.ConnectLabel+" (the vendor makes a key for Agentry in your browser, kept in the OS keychain) rather than a key you paste; interactive setup asks")
	f.StringVar(&opts.copilotPlan, "copilot-plan", "", "provider copilot: your Copilot plan, which names the Copilot API host runs reach: individual (Free, Pro, Pro+, Max; the default) | business | enterprise")
	f.StringVar(&opts.policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	f.BoolVar(&opts.yes, "yes", false, "non-interactive: fill gaps from the provider table instead of asking")
	f.BoolVar(&opts.skipImages, "skip-images", false, "configure the policy only; skip the docker image pulls/build")
	return cmd
}

// setupOutput is the `setup --json` shape (agentry-cli#102): the policy as
// configured — the model provider and the NAME of its key variable, never a
// value — and whether the images were prepared.
type setupOutput struct {
	PolicyPath string `json:"policyPath"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	// APIKeyEnv is the environment variable NAME the runner reads the key
	// from; the value is never printed.
	APIKeyEnv string `json:"apiKeyEnv"`
	// APIKeySource is "keyring" when the OS keychain is the fallback for
	// an unset variable; "" when the environment is the only source.
	APIKeySource string `json:"apiKeySource,omitempty"`
	// APIKeyKind is "subscription" for an Anthropic subscription token
	// (agentry-cli#171); absent for an API key.
	APIKeyKind string `json:"apiKeyKind,omitempty"`
	BaseURL    string `json:"baseUrl,omitempty"`
	// Images is "ready" when the gateway, harness and runner images were
	// pulled or built, "skipped" under --skip-images.
	Images string `json:"images"`
}

// runSetup executes both §P1 halves.
func runSetup(cmd *cobra.Command, opts setupOpts) error {
	home, err := runnerHome()
	if err != nil {
		return err
	}
	policyPath := opts.policyPath
	if policyPath == "" {
		if policyPath, err = policy.DefaultPath(); err != nil {
			return err
		}
	}
	pol, _, err := policy.EnsureDefault(policyPath)
	if err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}

	interactive := !opts.yes && stdinIsTTY(cmd.InOrStdin())
	// One buffered reader for the questions and a ChatGPT sign-in's pasted
	// redirect: two would each keep what they read ahead of the other.
	in := bufio.NewReader(cmd.InOrStdin())
	prompter := setup.Prompter(&setup.TTYPrompter{In: in, Err: cmd.ErrOrStderr()})
	if err := setup.ConfigureModel(prompter, pol, setup.ModelOpts{
		Provider:       opts.provider,
		Model:          opts.model,
		APIKeyEnv:      opts.apiKeyEnv,
		APIKeySource:   opts.apiKeySource,
		APIKeyKind:     opts.apiKeyKind,
		BaseURL:        opts.baseURL,
		ChatGPT:        chatgptOffer(cmd, in, home),
		Claude:         claudeOffer(cmd, in, home),
		Copilot:        copilotOffer(cmd, home, opts.oauthApp, pol, !opts.skipImages),
		Connect:        connectOffer(cmd, in, home),
		ConnectAccount: opts.connect,
		CopilotPlan:    opts.copilotPlan,
		Yes:            !interactive,
	}); err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}
	if err := policy.Save(policyPath, pol); err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "policy written: %s\n", policyPath)
	// A schedule that runs signed out holds its own sealed copy of the
	// keys: a changed key or provider is sealed again now (D18).
	resealSignedOut(cmd.ErrOrStderr())
	result := setupOutput{PolicyPath: policyPath, Provider: pol.Model.Provider, Model: pol.Model.Name,
		APIKeyEnv: pol.Model.APIKeyEnv, APIKeySource: pol.Model.APIKeySource, APIKeyKind: pol.Model.APIKeyKind, BaseURL: pol.Model.BaseURL, Images: "skipped"}

	if opts.skipImages {
		if flags.jsonOut {
			return emitJSON(cmd.OutOrStdout(), result)
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "setup complete (policy only; images skipped)")
		return nil
	}
	docker := dockerx.New()
	imageCtx := homeImageDir(home)
	// The harness image prepared is the default adapter's for this model
	// setup, and no other (plan model-sign-ins.md D7): docker-agent for a
	// key, the Codex image for a ChatGPT plan, the Claude Code image (built
	// here) for a Claude subscription, the Copilot CLI image (built here)
	// for a Copilot plan.
	runsOn := adapter.DefaultFor(pol)
	if err := gateway.EnsureInstalled(cmd.Context(), docker, pol,
		pol.RunnerImageTag(runnerimage.ContentVersion()), imageCtx, cmd.ErrOrStderr(), runsOn == adapter.DefaultName); err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	if runsOn != adapter.DefaultName {
		if h, ok := pol.Images.HarnessImageFor(runsOn); ok {
			if err := gateway.EnsureHarnessImage(cmd.Context(), docker, runsOn, h, cmd.ErrOrStderr()); err != nil {
				return engine.Classify(engine.ExitGateway, err)
			}
		}
	}
	result.Images = "ready"
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), result)
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(),
		"setup complete — gateway, harness, and runner images ready; QuickAgents can run")
	return nil
}
