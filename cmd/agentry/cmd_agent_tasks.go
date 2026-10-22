// agentry agent tasks: the tasks the Assistant registered for its heartbeat
// to fire, and the person's pause, resume and remove (pkg/runner/persistent).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

const agentTasksLong = "Lists the tasks a persistent agent that keeps time (the Assistant) registered\n" +
	"for itself: an instruction it acts on, in a turn of its own, at the times a\n" +
	"cron expression names. Its heartbeat fires them while the agent runs, once\n" +
	"its roster's heartbeat entry is on (`agentry agent grant assistant heartbeat\n" +
	"on`); a task that came due while it was stopped is named to it at its next\n" +
	"turn and not run. The agent adds and changes its tasks when you ask it to in\n" +
	"chat; here you can pause, resume or remove one.\n\n" +
	"  agentry agent tasks assistant\n" +
	"  agentry agent tasks assistant pause t2\n" +
	"  agentry agent tasks assistant remove t2"

func newAgentTasksCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tasks <name> [pause|resume|remove <task>]",
		Short: "List a persistent agent's tasks, or pause, resume or remove one",
		Long:  agentTasksLong,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 && len(args) != 3 {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("agentry agent tasks <name> [pause|resume|remove <task>]"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := cliAgent(args[0])
			if err != nil {
				return err
			}
			if !a.Builtin.KeepsTime() {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent keeps no tasks", a.Name))
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			var tasks []persistent.Task
			if len(args) == 3 {
				op := persistent.TaskOp{Op: args[1], ID: args[2]}
				if tasks, _, err = persistent.ChangeTaskOf(ctx, home, a, persistent.ClientCLI, op); err != nil {
					if errors.Is(err, persistent.ErrNoTask) {
						return engine.Classify(engine.ExitUsage, err)
					}
					return err
				}
			} else if tasks, _, err = persistent.TasksOf(ctx, home, a); err != nil {
				return err
			}
			if tasks == nil {
				tasks = []persistent.Task{}
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), struct {
					Agent string            `json:"agent"`
					Tasks []persistent.Task `json:"tasks"`
				}{a.Name, tasks})
			}
			cfg, err := persistent.LoadConfig(home, a.Name)
			if err != nil {
				return err
			}
			e, _ := persistent.BuildRoster(a, cfg, nil).Entry(persistent.EntryHeartbeat)
			printTasks(cmd.OutOrStdout(), a, tasks, e.On)
			return nil
		},
	}
}

// printTasks prints the tasks, one a block.
func printTasks(w io.Writer, a persistent.Agent, ts []persistent.Task, heartbeat bool) {
	if !heartbeat {
		_, _ = fmt.Fprintf(w, "The %s agent's heartbeat is off, so none of its tasks fires: agentry agent grant %s heartbeat on\n", a.Name, a.Name)
	}
	if len(ts) == 0 {
		_, _ = fmt.Fprintf(w, "The %s agent has no tasks. It registers them when you ask it to do something at a time.\n", a.Name)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TASK\tNAME\tWHEN\tZONE\tSTATE\tNEXT")
	for _, t := range ts {
		next := "-"
		if !t.Next.IsZero() {
			next = t.Next.Local().Format("Mon 2 Jan 15:04")
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, safeTerminalText(t.Name, 80), t.Cron, t.Zone, t.State, next)
	}
	_ = tw.Flush()
	for _, t := range ts {
		_, _ = fmt.Fprintf(w, "\n%s: %s\n", t.ID, safeTerminalText(strings.ReplaceAll(t.Instruction, "\n", " "), 400))
		if !t.LastFire.IsZero() {
			_, _ = fmt.Fprintf(w, "  last fired %s: %s\n", t.LastFire.Local().Format("Mon 2 Jan 15:04"), safeTerminalText(t.LastResult, 300))
		}
	}
}
