// `agentry setup` (RUNNER-PLAN-2 §P1): prepare a machine for BriefAgent
// runs before any agent is involved — configure the operator's preferred
// LLM provider into the policy, then stand up the docker-mcp-gateway plane
// (pull pinned images, build the runner image) right away instead of
// lazily at first agent use. Idempotent; `agentry run` still self-heals,
// so setup is a convenience, not a gate.
package main

import (
	"fmt"
	"github.com/blaktron/agentry-cli/runnerimage"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
)

// setupOpts carries the setup command's flags.
type setupOpts struct {
	provider   string
	model      string
	apiKeyEnv  string
	baseURL    string
	policyPath string
	yes        bool
	skipImages bool
}

// newSetupCmd builds the setup command.
func newSetupCmd() *cobra.Command {
	var opts setupOpts
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Prepare this machine for BriefAgent runs (LLM provider + docker MCP gateway)",
		Long: "One-time (and re-runnable) machine setup for the BriefAgent runner\n" +
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
	f.StringVar(&opts.provider, "provider", "", "LLM provider: anthropic | openai | google | custom (or any docker-agent provider name)")
	f.StringVar(&opts.model, "model", "", "model id (default: the provider's table default)")
	f.StringVar(&opts.apiKeyEnv, "api-key-env", "", "env var NAME holding your api key (never the value)")
	f.StringVar(&opts.baseURL, "base-url", "", "custom endpoint base URL (derives the egress-proxy allowlist)")
	f.StringVar(&opts.policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	f.BoolVar(&opts.yes, "yes", false, "non-interactive: fill gaps from the provider table instead of asking")
	f.BoolVar(&opts.skipImages, "skip-images", false, "configure the policy only; skip the docker image pulls/build")
	return cmd
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
	prompter := setup.Prompter(&setup.TTYPrompter{In: cmd.InOrStdin(), Err: cmd.ErrOrStderr()})
	if err := setup.ConfigureModel(prompter, pol, setup.ModelOpts{
		Provider:  opts.provider,
		Model:     opts.model,
		APIKeyEnv: opts.apiKeyEnv,
		BaseURL:   opts.baseURL,
		Yes:       !interactive,
	}); err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}
	if err := policy.Save(policyPath, pol); err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "policy written: %s\n", policyPath)

	if opts.skipImages {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "setup complete (policy only; images skipped)")
		return nil
	}
	docker := dockerx.New()
	imageCtx := homeImageDir(home)
	if err := gateway.EnsureInstalled(cmd.Context(), docker, pol,
		pol.RunnerImageTag(runnerimage.ContentVersion()), imageCtx, cmd.ErrOrStderr(), true); err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(),
		"setup complete — gateway, harness, and runner images ready; BriefAgents can run")
	return nil
}
