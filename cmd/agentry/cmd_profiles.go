// `agentry profiles` (RUNNER-PLAN-2 §P2): list and remove the MCP profiles
// guided setup created — the persistent keys/identities per QuickAgent
// signature. Secret VALUES are never printed by any subcommand.
package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/profile"
)

// newProfilesCmd builds the profiles command tree.
func newProfilesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profiles",
		Short: "Manage QuickAgent MCP profiles (stored keys and identities per release)",
	}
	cmd.AddCommand(newProfilesListCmd(), newProfilesRemoveCmd())
	return cmd
}

func newProfilesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List stored MCP profiles",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := runnerHome()
			if err != nil {
				return err
			}
			entries, err := profile.List(home)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(entries) == 0 {
				_, _ = fmt.Fprintln(out, "no MCP profiles stored (guided setup creates one on a QuickAgent's first run)")
				return nil
			}
			for _, e := range entries {
				if e.Profile == nil {
					_, _ = fmt.Fprintf(out, "UNREADABLE  %s\n", e.Dir)
					continue
				}
				keep := "keep"
				if !e.Profile.Keep {
					keep = "remove-on-completion"
				}
				secrets := "none"
				if names := e.Profile.SecretNames(); len(names) > 0 {
					secrets = strings.Join(names, ", ")
				}
				last := e.Profile.LastUsedAt
				if last == "" {
					last = "never used"
				}
				_, _ = fmt.Fprintf(out, "%s\n  digest %s · %s · secrets: %s · last used: %s\n",
					e.Profile.Ref, shortDigestSuffixOf(e.Dir), keep, secrets, last)
			}
			return nil
		},
	}
}

func newProfilesRemoveCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "remove [ns/name[@version]]",
		Short: "Remove stored MCP profiles for a QuickAgent (or --all)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if all == (len(args) == 1) {
				return errors.New("pass exactly one of ns/name[@version] or --all")
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			entries, err := profile.List(home)
			if err != nil {
				return err
			}
			removed := 0
			for _, e := range entries {
				if !all && !e.Profile.MatchesRef(args[0]) {
					continue
				}
				if err := profile.Remove(e.Dir); err != nil {
					return err
				}
				name := e.Dir
				if e.Profile != nil {
					name = e.Profile.Ref
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed profile %s\n", name)
				removed++
			}
			if removed == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "nothing matched")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "remove every stored profile")
	return cmd
}

// shortDigestSuffixOf recovers the digest suffix from a profile dir name
// (…/<version>-<digest12>) for display.
func shortDigestSuffixOf(dir string) string {
	base := filepath.Base(dir)
	if i := strings.LastIndex(base, "-"); i >= 0 && i+1 < len(base) {
		return base[i+1:]
	}
	return base
}
