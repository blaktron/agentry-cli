// `agentry uninstall` removes this machine's RUNNER state for a
// BriefAgent: the verified install cache (workspace + compiled plans)
// and, BY DEFAULT, its MCP profile — stored keys, identities, and the
// installed MCP-server store (RUNNER-PLAN-3: remove on uninstall is the
// default; keeping credentials is the explicit choice). --keep-mcp
// retains the profile directory, which is signature-locked: only a
// reinstall of the exact same release digest can ever use it again — not
// another BriefAgent, not another user, not a newer version of this one.
//
// Embedded installs (claude-skill / mcp / agents-md exports) are separate
// state with their own records; this command does not touch them.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/runner/profile"
)

func newUninstallCmd() *cobra.Command {
	var keepMCP bool
	cmd := &cobra.Command{
		Use:   "uninstall <ns/name[@version]>",
		Short: "Remove a BriefAgent's runner install (and, by default, its MCP profile and servers)",
		Long: "Remove this machine's runner state for a BriefAgent: the verified install\n" +
			"cache and, by default, its MCP profile — stored keys and installed MCP\n" +
			"servers included (remove is the default). --keep-mcp retains the profile,\n" +
			"which stays locked to the exact release digest it was created for.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUninstall(cmd, args[0], keepMCP)
		},
	}
	cmd.Flags().BoolVar(&keepMCP, "keep-mcp", false,
		"keep the MCP profile (keys, identities, installed servers) for a same-digest reinstall")
	return cmd
}

func runUninstall(cmd *cobra.Command, sel string, keepMCP bool) error {
	ref, err := manifest.ParseRef(sel)
	if err != nil {
		return err
	}
	home, err := runnerHome()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	removedAnything := false

	// 1. The install cache: home/agents/<ns>/<name>/<version>-<digest12>.
	agentDir := filepath.Join(home, "agents", ref.Namespace, ref.Name)
	entries, err := os.ReadDir(agentDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if ref.Range != "" && !strings.HasPrefix(e.Name(), ref.Range+"-") {
			continue
		}
		full := filepath.Join(agentDir, e.Name())
		if err := os.RemoveAll(full); err != nil {
			return fmt.Errorf("remove install %s: %w", full, err)
		}
		_, _ = fmt.Fprintf(out, "removed install %s\n", full)
		removedAnything = true
	}
	// Prune empty parents so the tree does not accumulate husks.
	_ = os.Remove(agentDir)
	_ = os.Remove(filepath.Dir(agentDir))

	// 2. The MCP profile(s): keys and the installed server store. Remove
	// is the default (RUNNER-PLAN-3 decision 2); --keep-mcp preserves
	// them for a same-digest reinstall and nothing else.
	profs, err := profile.List(home)
	if err != nil {
		return err
	}
	for _, e := range profs {
		if e.Profile == nil || !e.Profile.MatchesRef(sel) {
			continue
		}
		if keepMCP {
			_, _ = fmt.Fprintf(out, "kept MCP profile %s (%s) — locked to its release digest; a same-digest reinstall reconnects without re-prompting\n",
				e.Profile.Ref, filepath.Base(e.Dir))
			continue
		}
		if err := profile.Remove(e.Dir); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "removed MCP profile %s (keys, identities, and installed MCP servers)\n", e.Profile.Ref)
		removedAnything = true
	}

	// 3. The runner's consent record in the shared install state, so a
	// future install asks about capabilities from scratch.
	if state, serr := installed.Load(); serr == nil {
		key := installed.Key{
			Registry: "runner", Namespace: ref.Namespace, Name: ref.Name,
			Target: "runner", Dir: agentDir,
		}
		if state.Remove(key) {
			if err := installed.Save(state); err != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
			}
		}
	}

	if !removedAnything && !keepMCP {
		_, _ = fmt.Fprintf(out, "nothing installed for %s on this machine\n", sel)
	}
	return nil
}
