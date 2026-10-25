// agentry agent compact: memory upkeep now, then a new conversation that
// opens with the agent's handoff (pkg/runner/persistent).
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

const agentCompactLong = "Compacts a running persistent agent's conversation now, whatever its size.\n" +
	"The agent is sent an upkeep turn: it saves what the conversation produced to\n" +
	"its memory and writes a short handoff. Then a new conversation starts that\n" +
	"opens with the handoff, so the agent goes on with a small context and its\n" +
	"memory. An upkeep turn uses your model plan and can take a few minutes; a\n" +
	"message sent meanwhile waits for it. If the upkeep turn fails, nothing is\n" +
	"swapped and the conversation goes on as it was.\n\n" +
	"The Assistant also compacts on its own: when you have sent nothing for two\n" +
	"hours and its conversation holds 100,000 tokens or more, and when a turn has\n" +
	"filled three quarters of the model's context window. At most 12 upkeep turns\n" +
	"run in a day. Each compaction is a line of the segment's\n" +
	"receipts/upkeep.ndjson.\n\n" +
	"  agentry agent compact assistant"

func newAgentCompactCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "compact <name>",
		Short: "Save a running agent's conversation to its memory and start a fresh one",
		Long:  agentCompactLong,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("agentry agent compact <name>"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := cliAgent(args[0])
			if err != nil {
				return err
			}
			if !a.Builtin.KeepsMemory() {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent keeps no memory to compact into", a.Name))
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
			defer cancel()
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Compacting the %s agent's conversation: an upkeep turn first, which can take a few minutes\n", a.Name)
			out, err := persistent.RequestCompact(ctx, home, a.Name)
			if errors.Is(err, persistent.ErrNotRunning) {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent is not running: start it first (agentry agent start %s)", a.Name, a.Name))
			}
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), struct {
					Agent     string               `json:"agent"`
					Compacted persistent.Compacted `json:"compacted"`
				}{a.Name, out})
			}
			if !out.Swapped {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Nothing was compacted: %s. The conversation goes on as it was.\n", out.Reason)
				return engine.Classify(engine.ExitCLI, errors.New("the compaction did not finish"))
			}
			facts := "the memory's fact lines could not be counted"
			if out.FactsBefore >= 0 && out.FactsAfter >= 0 {
				facts = fmt.Sprintf("the memory went from %d to %d fact lines", out.FactsBefore, out.FactsAfter)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Compacted: %s, and a fresh conversation opens with a %d-byte handoff.\n", facts, out.HandoffBytes)
			return nil
		},
	}
}
