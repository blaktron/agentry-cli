// agentry agent memory: a persistent agent's memory, the wiki it keeps on a
// volume of its own (pkg/runner/persistent).
package main

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/runnerimage"
)

func newAgentMemoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Export a persistent agent's memory",
		Long: "The Assistant keeps a memory: pages it writes in memory/ on a volume of\n" +
			"its own, on what you tell it and what you share with it, each fact with\n" +
			"its source, with an index and a log. It is kept until you reset the agent.\n\n" +
			"  agentry agent memory export assistant ~/Documents/assistant-memory",
	}
	cmd.AddCommand(newAgentMemoryExportCmd())
	return cmd
}

func newAgentMemoryExportCmd() *cobra.Command {
	var policyPath string
	cmd := &cobra.Command{
		Use:   "export <name> <folder>",
		Short: "Copy a persistent agent's memory into a new folder",
		Long: "Export copies the agent's memory, its pages, index and log, into a new\n" +
			"folder that it makes, beneath a folder that exists: a copy of its files and\n" +
			"folders as they are at that moment, never a link to the volume. A link in\n" +
			"the memory is left out. The agent may be running.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := cliAgent(args[0])
			if err != nil {
				return err
			}
			if !a.Builtin.KeepsMemory() {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent keeps no memory", a.Name))
			}
			dir, err := filepath.Abs(args[1])
			if err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			pol, _, _, err := agentPolicy(policyPath)
			if err != nil {
				return err
			}
			d := newDockerClient()
			if err := d.Available(cmd.Context()); err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			runnerTag := pol.RunnerImageTag(runnerimage.ContentVersion())
			if err := gateway.EnsureRunnerImage(cmd.Context(), d, pol, runnerTag, homeImageDir(home), cmd.ErrOrStderr()); err != nil {
				return engine.Classify(engine.ExitGateway, err)
			}
			out, err := persistent.ExportMemory(cmd.Context(), d, home, a, runnerTag, pol.Docker.Runtime, pol.Docker.Limits, dir)
			if err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), struct {
					Agent  string `json:"agent"`
					Folder string `json:"folder"`
					persistent.Exported
				}{a.Name, dir, out})
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Exported the %s agent's memory into %s: %d file(s), %d byte(s).\n", a.Name, dir, out.Files, out.Bytes)
			if out.Skipped > 0 {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Left out %d entr(ies) that were neither a file nor a folder.\n", out.Skipped)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&policyPath, "policy", "", "operator policy file whose runner image and container limits the export uses (default ~/.agentry/runner/policy.yaml, created on first use)")
	return cmd
}
