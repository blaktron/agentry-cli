// `agentry uninstall` removes this machine's RUNNER state for a
// BriefAgent: the verified install cache (workspace + compiled plans)
// and, BY DEFAULT, its MCP profile — stored keys, identities, and the
// installed MCP-server store (RUNNER-PLAN-3: remove on uninstall is the
// default; keeping credentials is the explicit choice). --keep-mcp
// retains the profile directory, which is signature-locked: only a
// reinstall of the exact same release digest can ever use it again — not
// another BriefAgent, not another user, not a newer version of this one.
//
// Embedded installs (claude-skill / agents-md exports, and the mcp exports
// of older clients) are separate
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

// uninstallOutput is the `uninstall --json` and `uninstall --plan --json`
// shape (agentry-cli#102): what was — or, with --plan, would be — removed.
type uninstallOutput struct {
	Ref string `json:"ref"`
	// Plan is true when nothing was touched.
	Plan bool `json:"plan"`
	// Installs are the runner install cache directories.
	Installs []string `json:"installs"`
	// Profiles are the MCP profiles (keys, identities, servers) removed —
	// or, under --keep-mcp, kept: then they are listed under KeptProfiles.
	Profiles     []string `json:"profiles"`
	KeptProfiles []string `json:"keptProfiles,omitempty"`
	// ConsentRecord is true when the runner's consent record in the shared
	// install state was (or would be) removed.
	ConsentRecord bool `json:"consentRecord"`
}

func (o uninstallOutput) empty() bool {
	return len(o.Installs) == 0 && len(o.Profiles) == 0 && len(o.KeptProfiles) == 0 && !o.ConsentRecord
}

func newUninstallCmd() *cobra.Command {
	var keepMCP, plan bool
	cmd := &cobra.Command{
		Use:   "uninstall <ns/name[@version]>",
		Short: "Remove a BriefAgent's runner install (and, by default, its MCP profile and servers)",
		Long: "Remove this machine's runner state for a BriefAgent: the verified install\n" +
			"cache and, by default, its MCP profile — stored keys and installed MCP\n" +
			"servers included (remove is the default). --keep-mcp retains the profile,\n" +
			"which stays locked to the exact release digest it was created for.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUninstall(cmd, args[0], keepMCP, plan)
		},
	}
	cmd.Flags().BoolVar(&keepMCP, "keep-mcp", false,
		"keep the MCP profile (keys, identities, installed servers) for a same-digest reinstall")
	cmd.Flags().BoolVar(&plan, "plan", false, "preview only: name what would be removed; remove nothing")
	return cmd
}

func runUninstall(cmd *cobra.Command, sel string, keepMCP, plan bool) error {
	ref, err := manifest.ParseRef(sel)
	if err != nil {
		return err
	}
	home, err := runnerHome()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if flags.jsonOut {
		out = cmd.ErrOrStderr() // §R8: the result on stdout, the narration on stderr
	}
	result := uninstallOutput{Ref: sel, Plan: plan, Installs: []string{}, Profiles: []string{}}
	verb := "removed"
	if plan {
		verb = "would remove"
	}

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
		if !plan {
			if err := os.RemoveAll(full); err != nil {
				return fmt.Errorf("remove install %s: %w", full, err)
			}
		}
		_, _ = fmt.Fprintf(out, "%s install %s\n", verb, full)
		result.Installs = append(result.Installs, full)
	}
	if !plan {
		// Prune empty parents so the tree does not accumulate husks.
		_ = os.Remove(agentDir)
		_ = os.Remove(filepath.Dir(agentDir))
	}

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
			result.KeptProfiles = append(result.KeptProfiles, e.Dir)
			continue
		}
		if !plan {
			if err := profile.Remove(e.Dir); err != nil {
				return err
			}
		}
		_, _ = fmt.Fprintf(out, "%s MCP profile %s (keys, identities, and installed MCP servers)\n", verb, e.Profile.Ref)
		result.Profiles = append(result.Profiles, e.Dir)
	}

	// 3. The runner's consent record in the shared install state, so a
	// future install asks about capabilities from scratch.
	if state, serr := installed.Load(); serr == nil {
		key := installed.Key{
			Registry: "runner", Namespace: ref.Namespace, Name: ref.Name,
			Target: "runner", Dir: agentDir,
		}
		if plan {
			result.ConsentRecord = state.Find(key) != nil
		} else if state.Remove(key) {
			result.ConsentRecord = true
			if err := installed.Save(state); err != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
			}
		}
	}

	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), result)
	}
	if result.empty() {
		_, _ = fmt.Fprintf(out, "nothing installed for %s on this machine\n", sel)
	}
	return nil
}
