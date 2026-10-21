package adapter

import "testing"

// TestHarnessTitle: a harness is named as a person knows it, its image the
// same as its host install; the default harness is not named.
func TestHarnessTitle(t *testing.T) {
	for name, want := range map[string]string{
		ClaudeAdapterName: "Claude Code", ClaudeImageAdapterName: "Claude Code",
		CodexAdapterName: "Codex", CodexImageAdapterName: "Codex",
		CopilotImageAdapterName: "Copilot CLI", DefaultName: "", "unknown": "",
	} {
		if got := HarnessTitle(name); got != want {
			t.Errorf("HarnessTitle(%q) = %q, want %q", name, got, want)
		}
	}
}
