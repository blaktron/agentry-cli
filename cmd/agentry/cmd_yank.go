// `agentry yank` (§11): author yank of a published version. Yanked versions
// leave range resolution but stay downloadable by exact pin (§5.5); the
// version string is burned forever.
package main

import (
	"fmt"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/apiclient"
	"github.com/blaktron/agentry/pkg/manifest"
)

// newYankCmd builds the yank command.
func newYankCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "yank <ns/name@version>",
		Short: "Yank a published version (excluded from ranges; exact pins warn)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runYank(cmd, args[0], reason)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why the version is being yanked (recorded and shown to installers)")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

// runYank executes the yank.
func runYank(cmd *cobra.Command, ref, reason string) error {
	parsed, err := manifest.ParseRef(ref)
	if err != nil {
		return err
	}
	if !exactVersionRe.MatchString(parsed.Range) {
		return fmt.Errorf("yank requires an exact version, e.g. %s/%s@1.2.3", parsed.Namespace, parsed.Name)
	}

	reg := registryURL()
	token := credentialFor(reg)
	if token == "" {
		return fmt.Errorf("no credential for %s — run `agentry login` or set %s", reg, envToken)
	}
	client, err := newAPIClientChecked(cmd, reg, token)
	if err != nil {
		return err
	}
	resp, err := client.YankVersionWithResponse(cmd.Context(), parsed.Namespace, parsed.Name, parsed.Range,
		apiclient.YankVersionJSONRequestBody{Reason: reason})
	if err != nil {
		return fmt.Errorf("yank %s: %w", ref, err)
	}
	if resp.StatusCode() != http.StatusNoContent {
		return apiProblem("yank "+ref, resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.Body)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "yanked %s (reason: %s)\n", ref, reason)
	return nil
}
