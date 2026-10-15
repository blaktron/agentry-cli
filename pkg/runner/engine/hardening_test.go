package engine

import (
	"strings"
	"testing"
)

// TestTheReceiptNamesWhatTheFilterGivesTheGatewaysContainers: where the
// run's docker filter runs, the containers line names every capability
// dropped and each class's own; without it (Docker Desktop) it says the
// gateway leaves them as they are.
func TestTheReceiptNamesWhatTheFilterGivesTheGatewaysContainers(t *testing.T) {
	filtered := launchedLine(true)
	for _, want := range []string{"every capability dropped", "pids bound", "CHOWN, DAC_OVERRIDE, FOWNER, KILL, SETGID, SETUID",
		"a declared MCP server DAC_READ_SEARCH", "DAC_OVERRIDE too where it writes", "a relay none"} {
		if !strings.Contains(filtered, want) {
			t.Errorf("filtered line lacks %q: %s", want, filtered)
		}
	}
	if plain := launchedLine(false); !strings.Contains(plain, "their capabilities and root as it leaves them") || strings.Contains(plain, "dropped") {
		t.Errorf("unfiltered line: %s", plain)
	}
}
