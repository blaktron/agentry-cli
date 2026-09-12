package engine

import (
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// The exit-class names the envelope carries (receipt.ExitClassName) and the
// codes this package defines are one table in two places; this holds them
// together (agentry-cli#102).
func TestExitClassNamesMatchCodes(t *testing.T) {
	want := map[int]string{
		ExitOK: "ok", ExitCLI: "cli", ExitUsage: "usage", ExitVerify: "verify", ExitPolicy: "policy",
		ExitCompile: "compile", ExitGateway: "gateway", ExitHarness: "harness", ExitTimeout: "timeout",
		ExitBudget: "budget", ExitOutputSchema: "outputSchema",
	}
	for code, name := range want {
		if got := receipt.ExitClassName(code); got != name {
			t.Errorf("code %d: %q, want %q", code, got, name)
		}
	}
	if got := receipt.ExitClassName(99); got != "exit-99" {
		t.Errorf("unknown code: %q", got)
	}
}
