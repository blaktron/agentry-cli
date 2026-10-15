// `agentry runner-tools` (hidden): serve the companion tools server on
// stdio, exactly as the runner image's `tools` mode does inside the tools
// container. It exists for two reasons: debugging a tool spec without a
// docker round-trip, and letting acceptance scripts exercise the MCP
// surface on machines with no engine.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/runnerimage/confine"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// runnerConfineUse is the hidden command that is runner-tools' confine
// trampoline, as the runner image's `confine` role is the tools
// container's.
const runnerConfineUse = "runner-confine"

// newRunnerToolsCmd builds the hidden runner-tools command.
func newRunnerToolsCmd() *cobra.Command {
	var specPath string
	cmd := &cobra.Command{
		Use:    "runner-tools --spec toolspec.json",
		Short:  "Serve the companion MCP tools server on stdio (debugging)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if specPath == "" {
				return fmt.Errorf("--spec is required")
			}
			b, err := os.ReadFile(specPath) //nolint:gosec // operator-named spec file
			if err != nil {
				return err
			}
			var s toolsrv.Spec
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&s); err != nil {
				return fmt.Errorf("parse %s: %w", specPath, err)
			}
			toolsrv.ConfineRole = runnerConfineUse
			return toolsrv.Serve(cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), &s)
		},
	}
	cmd.Flags().StringVar(&specPath, "spec", "", "toolspec.json (the compiler writes one under plan/gateway/)")
	return cmd
}

// newRunnerConfineCmd builds the hidden runner-confine command: confine's
// trampoline for runner-tools' run_command. It never returns.
func newRunnerConfineCmd() *cobra.Command {
	return &cobra.Command{
		Use:                runnerConfineUse + " --write DIR [--write DIR …] -- PROGRAM [ARG …]",
		Short:              "Run a declared command able to write only beneath the named directories (runner-tools)",
		Hidden:             true,
		DisableFlagParsing: true,
		Run: func(_ *cobra.Command, args []string) {
			confine.Trampoline(args)
		},
	}
}
