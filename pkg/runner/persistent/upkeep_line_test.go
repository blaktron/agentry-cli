package persistent

import (
	"testing"

	"github.com/blaktron/agentry-cli/pkg/tui"
)

// TestTheChatNamesEveryUpkeepTurn: the terminal chat tells the upkeep turns
// apart by their messages, so each message here gets its own line there.
func TestTheChatNamesEveryUpkeepTurn(t *testing.T) {
	for text, want := range map[string]string{
		upkeepText:         "Compacting: saving this conversation to memory first",
		upkeepTextNoMemory: "Compacting: writing a handoff",
		consolidateText:    "Tidying its memory",
	} {
		if got := tui.UpkeepLine(text); got != want {
			t.Errorf("tui.UpkeepLine(%q) = %q, want %q", text, got, want)
		}
	}
}
