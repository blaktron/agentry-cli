// agentry agent compact: the conversation compacted now, by the running
// harness's own summary where it can, else by an upkeep turn and a new
// conversation that opens with the agent's handoff (pkg/runner/persistent).
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

const agentCompactLong = "Compacts a running persistent agent's conversation now, whatever its size.\n" +
	"On a harness that can summarise its own conversation (the Agentry harness,\n" +
	"Codex, Copilot CLI), the harness replaces the conversation with a summary of\n" +
	"it in one request that calls no tool, and an agent that keeps a memory, the\n" +
	"Assistant, then saves what the summary holds to it. On any other harness the\n" +
	"agent is sent an upkeep turn: it writes a short handoff, and an agent that\n" +
	"keeps a memory first saves what the conversation produced to it; then a new\n" +
	"conversation starts that opens with the handoff. Either way the agent goes on\n" +
	"with a small context. Compacting uses your model plan and can take a few\n" +
	"minutes, up to half an hour on a model server of your own; a message sent\n" +
	"meanwhile waits for it. If it fails, the conversation goes on as it was.\n\n" +
	"Every agent also compacts on its own: when you have sent nothing for two\n" +
	"hours and its conversation holds 100,000 tokens or more, and when a turn has\n" +
	"filled three quarters of the model's context window. At most 12 upkeep turns\n" +
	"run in a day, this command's included. Each compaction is a line of the segment's\n" +
	"receipts/upkeep.ndjson.\n\n" +
	"  agentry agent compact assistant\n" +
	"  agentry agent compact coder"

func newAgentCompactCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "compact <name>",
		Short: "Compact a running agent's conversation into a handoff and start a fresh one",
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
			home, err := runnerHome()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), persistent.CompactWait)
			defer cancel()
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Compacting the %s agent's conversation, which can take a few minutes\n", a.Name)
			out, err := persistent.RequestCompact(ctx, home, a.Name)
			if errors.Is(err, persistent.ErrNotRunning) {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("the %s agent is not running: start it first (agentry agent start %s)", a.Name, a.Name))
			}
			if err != nil {
				return err
			}
			if flags.jsonOut {
				if err := emitJSON(cmd.OutOrStdout(), struct {
					Agent     string               `json:"agent"`
					Compacted persistent.Compacted `json:"compacted"`
				}{a.Name, out}); err != nil {
					return err
				}
				if !out.Done() {
					return engine.Classify(engine.ExitCLI, errors.New("the compaction did not finish: "+out.Reason))
				}
				return nil
			}
			if !out.Done() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Nothing was compacted: %s. The conversation goes on as it was.\n", out.Reason)
				return engine.Classify(engine.ExitCLI, errors.New("the compaction did not finish"))
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), compactedLine(out, a.KeepsMemory()))
			return nil
		},
	}
}

// compactedLine says what a compaction that finished did; memory says the
// agent keeps one.
func compactedLine(out persistent.Compacted, memory bool) string {
	facts := "the memory's fact lines could not be counted"
	if out.FactsBefore >= 0 && out.FactsAfter >= 0 {
		facts = fmt.Sprintf("the memory went from %d to %d fact lines", out.FactsBefore, out.FactsAfter)
	}
	switch {
	case out.Swap == persistent.SwapInPlace && !memory:
		return "Compacted: the running harness replaced the conversation with a summary of it."
	case out.Swap == persistent.SwapInPlace && out.SaveFailed != "":
		return "Compacted: the running harness replaced the conversation with a summary of it, but saving the summary to memory did not finish: " +
			out.SaveFailed + ". The summary keeps what it was to save."
	case out.Swap == persistent.SwapInPlace:
		return fmt.Sprintf("Compacted: the running harness replaced the conversation with a summary of it, and %s.", facts)
	case !memory:
		return fmt.Sprintf("Compacted: a fresh conversation opens with a %d-byte handoff.", out.HandoffBytes)
	}
	return fmt.Sprintf("Compacted: %s, and a fresh conversation opens with a %d-byte handoff.", facts, out.HandoffBytes)
}
