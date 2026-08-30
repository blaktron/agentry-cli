// `agentry runner-tools` (hidden): serve the companion tools server on
// stdio, exactly as the runner image's `tools` mode does inside the tools
// container. It exists for two reasons: debugging a tool spec without a
// docker round-trip, and letting acceptance scripts exercise the MCP
// surface on machines with no engine (RUNNER-DEVIATIONS DR-7).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

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
			return toolsrv.Serve(cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), &s)
		},
	}
	cmd.Flags().StringVar(&specPath, "spec", "", "toolspec.json (the compiler writes one under plan/gateway/)")
	return cmd
}
