// agentry agent skill: the skills a persistent agent reads, built in and the
// person's own, added from a folder or a zip on this computer and checked as
// the share is (pkg/runner/persistent, pkg/runner/skill).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

const agentSkillLong = "A skill is a folder of instructions a persistent agent reads when a request\n" +
	"matches it: a SKILL.md (in the Agent Skills format, with a name and a\n" +
	"description) and the files it uses. The agent has them read-only at\n" +
	"skills/; the Coding agent can run a skill's scripts with its programs.\n" +
	"Its built-in skills come with Agentry; you switch one off with\n" +
	"`agentry agent grant <name> skill:<skill> off`.\n\n" +
	"You add your own from a folder or a .zip on this computer, and from nowhere\n" +
	"else: it must hold regular files and folders only (no link), at most 500\n" +
	"files and 32 MiB, and its folder's name must be the skill's. It is checked\n" +
	"as a file in the share is, with ClamAV when the runner policy's malwareCheck\n" +
	"is on, and a finding refuses it. Agentry has not reviewed it, and adding it\n" +
	"asks you first. A running agent takes a change at its next turn.\n\n" +
	"  agentry agent skill list coder\n" +
	"  agentry agent skill add coder ./release-notes\n" +
	"  agentry agent skill add assistant ~/Downloads/meeting-notes.zip\n" +
	"  agentry agent skill remove coder release-notes"

func newAgentSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "List, add and remove a persistent agent's skills",
		Long:  agentSkillLong,
	}
	cmd.AddCommand(newAgentSkillListCmd(), newAgentSkillAddCmd(), newAgentSkillRemoveCmd())
	return cmd
}

func newAgentSkillListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <name>",
		Short: "List a persistent agent's skills and whether each is on",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := cliAgent(args[0])
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			st, err := readAgentState(cmd.Context(), home, a)
			if err != nil {
				return err
			}
			var skills persistent.Roster
			for _, e := range st.Roster {
				if e.Kind == persistent.KindSkill {
					skills = append(skills, e)
				}
			}
			if flags.jsonOut {
				if skills == nil {
					skills = persistent.Roster{}
				}
				return emitJSON(cmd.OutOrStdout(), struct {
					Agent  string            `json:"agent"`
					Skills persistent.Roster `json:"skills"`
				}{a.Name, skills})
			}
			printSkills(cmd.OutOrStdout(), a, skills)
			return nil
		},
	}
}

func newAgentSkillAddCmd() *cobra.Command {
	var yes bool
	var policyPath string
	cmd := &cobra.Command{
		Use:   "add <name> <folder or .zip>",
		Short: "Add a skill of your own to a persistent agent, from a folder or a .zip",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := cliAgent(args[0])
			if err != nil {
				return err
			}
			src, err := filepath.Abs(args[1])
			if err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			if err := skillSource(src); err != nil {
				return err
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
			errOut := cmd.ErrOrStderr()
			check := func(ctx context.Context, dir string) (bool, error) {
				return localrun.CheckFolder(ctx, d, pol, home, dir, errOut)
			}
			s, err := persistent.InstallSkill(cmd.Context(), home, a, src, check)
			if err != nil {
				var exit *engine.ClassifiedError
				if errors.As(err, &exit) {
					return err
				}
				return engine.Classify(engine.ExitUsage, err)
			}
			r, running, err := configureAgent(cmd, home, a, persistent.Change{AddSkill: &s}, yes)
			if err != nil {
				_ = persistent.DiscardSkill(home, a, s.Name)
				return err
			}
			if s.Check == persistent.SkillCheckedClamAV {
				_, _ = fmt.Fprintf(errOut, "ClamAV found nothing in the skill %s's files.\n", s.Name)
			} else {
				_, _ = fmt.Fprintf(errOut, "The skill %s's files were not checked for malware (the malware check is off, or ClamAV could not check a file).\n", s.Name)
			}
			printChanged(cmd.OutOrStdout(), a, r, running)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "agree to what adding the skill gives the agent, without asking")
	cmd.Flags().StringVar(&policyPath, "policy", "", "the runner policy whose malware check checks the skill (default: the runner home's)")
	return cmd
}

func newAgentSkillRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name> <skill>",
		Short: "Remove a skill you added from a persistent agent",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := cliAgent(args[0])
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			r, running, err := configureAgent(cmd, home, a, persistent.Change{RemoveSkill: args[1]}, false)
			if err != nil {
				return err
			}
			if err := persistent.DiscardSkill(home, a, args[1]); err != nil {
				return engine.Classify(engine.ExitCLI, err)
			}
			printChanged(cmd.OutOrStdout(), a, r, running)
			return nil
		},
	}
}

// printSkills prints the skills, one a line.
func printSkills(w io.Writer, a persistent.Agent, skills persistent.Roster) {
	if len(skills) == 0 {
		_, _ = fmt.Fprintf(w, "The %s agent has no skills. Add one: agentry agent skill add %s <folder or .zip>\n", a.Name, a.Name)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SKILL\tSTATE\tBY\tWHAT")
	for _, e := range skills {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Name, e.State, e.GrantedBy, safeTerminalText(e.Line, 300))
	}
	_ = tw.Flush()
}

// skillSource says whether a path names a folder or a file, for the usage
// error a missing one gets.
func skillSource(p string) error {
	if _, err := os.Stat(p); err != nil {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("%s: %w", p, err))
	}
	return nil
}
