package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

func newHarnessCmd() *cobra.Command {
	var policyPath string
	cmd := &cobra.Command{
		Use:   "harness",
		Short: "Show or choose the container harness runs use: docker-agent or agentry-harness",
		Long: "The harness is the program that drives the model through a run: it sends the conversation,\n" +
			"makes the tool calls the model asks for and reports what happened. Agentry has two that run in\n" +
			"a container beside the run's tools: docker-agent (Docker Agent, the default) and agentry-harness\n" +
			"(the harness built into the runner). Both run the same QuickAgents under the same policy, and\n" +
			"the receipt names the one that ran.\n\n" +
			"This setting is the policy's containerHarness. A run, a session and a persistent agent follow\n" +
			"it when the model is paid by a key, a connected account or your Agentry credits. A model paid\n" +
			"by a model sign-in (a ChatGPT, Claude or Copilot plan) runs on its vendor's harness whatever\n" +
			"this says, and --adapter still names the harness for one run. A persistent agent moved from\n" +
			"one harness to the other starts its next segment from its memory and the last exchange.\n\n" +
			"With no subcommand, shows the harness chosen.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pol, path, err := modelsPolicy(policyPath)
			if err != nil {
				return err
			}
			return sayHarness(cmd, pol, path, "")
		},
	}
	cmd.Flags().StringVar(&policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	cmd.AddCommand(newHarnessUseCmd())
	return cmd
}

func newHarnessUseCmd() *cobra.Command {
	var policyPath string
	cmd := &cobra.Command{
		Use:   "use <" + strings.Join(policy.ContainerHarnesses(), "|") + ">",
		Short: "Choose the container harness runs use from now on",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.ToLower(strings.TrimSpace(args[0]))
			if err := policy.CheckContainerHarness(name); err != nil || name == "" {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("%q is not a container harness: choose %s", args[0], strings.Join(policy.ContainerHarnesses(), " or ")))
			}
			pol, path, err := modelsPolicy(policyPath)
			if err != nil {
				return err
			}
			was := pol.RunContainerHarness()
			// The default is written as no key at all, so a policy that
			// never chose stays as it was.
			pol.ContainerHarness = name
			if name == policy.DockerAgentAdapter {
				pol.ContainerHarness = ""
			}
			if err := policy.Save(path, pol); err != nil {
				return engine.Classify(engine.ExitPolicy, err)
			}
			return sayHarness(cmd, pol, path, was)
		},
	}
	cmd.Flags().StringVar(&policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	return cmd
}

// harnessOut is `agentry harness --json`: the container harness chosen, the
// two it is chosen from, and the harness a run on the default model takes.
type harnessOut struct {
	// Harness is the container harness the policy chooses.
	Harness string `json:"harness"`
	// Default is whether that is the one a policy that never chose has.
	Default bool `json:"default"`
	// Choices are the harnesses `agentry harness use` takes.
	Choices []string `json:"choices"`
	// RunsOn is the harness a run on the default model uses when it names
	// none: Harness, or the vendor's for a model paid by a model sign-in.
	RunsOn     string `json:"runsOn"`
	PolicyPath string `json:"policyPath"`
	// Previous is the harness chosen before a use that changed it.
	Previous string `json:"previous,omitempty"`
}

// harnessLabel is a container harness as a sentence names it.
func harnessLabel(name string) string {
	switch name {
	case policy.DockerAgentAdapter:
		return "docker-agent (Docker Agent)"
	case policy.AgentryHarnessAdapter:
		return "agentry-harness (the harness built into the runner)"
	}
	return name
}

// sayHarness prints the harness pol chooses; was is the one chosen before
// a change, empty for a plain show.
func sayHarness(cmd *cobra.Command, pol *policy.Policy, path, was string) error {
	name := pol.RunContainerHarness()
	runsOn := adapter.DefaultFor(pol)
	out := harnessOut{Harness: name, Default: name == policy.DockerAgentAdapter, Choices: policy.ContainerHarnesses(), RunsOn: runsOn, PolicyPath: path}
	if was != "" && was != name {
		out.Previous = was
	}
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), out)
	}
	w := cmd.OutOrStdout()
	suffix := ""
	if out.Default {
		suffix = ", the default"
	}
	switch was {
	case "":
		_, _ = fmt.Fprintf(w, "harness: %s%s\n", harnessLabel(name), suffix)
	case name:
		_, _ = fmt.Fprintf(w, "harness: %s%s (unchanged)\n", harnessLabel(name), suffix)
	default:
		_, _ = fmt.Fprintf(w, "harness: %s%s (was %s)\n", harnessLabel(name), suffix, was)
	}
	if runsOn != name {
		_, _ = fmt.Fprintf(w, "the default model is paid by %s: runs on it use %s whatever this says\n",
			policy.SignInSentence(policy.KeyKind(pol.Harness[adapter.DefaultName], pol.Model)), runsOn)
	}
	if was != "" && was != name {
		_, _ = fmt.Fprintln(narration(cmd), "runs and sessions use it from their next start, and a persistent agent from its next turn; a run or session under way finishes on the harness it started on")
	}
	return nil
}
