package compile

import (
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// The runner's own harness is the runner image, which is built on the
// machine: the receipt says so, and never calls it a tag that can move.
func TestTheRunnersOwnHarnessIsPinnedByItsSource(t *testing.T) {
	pin := imagePin(adapter.AgentryHarnessAdapterName, "agentry-runner:0.7.60", policy.HarnessImage{Ref: "agentry-runner:0.7.60"})
	if pin == nil || pin.By != policy.PinSource || pin.Value != "0.7.60" {
		t.Fatalf("pin: %+v", pin)
	}
	if got, want := pin.String(), "source 0.7.60 (built on this machine from the CLI's own source)"; got != want {
		t.Errorf("receipt line %q, want %q", got, want)
	}
	// Every other adapter's image is pinned as before.
	if pin := imagePin(adapter.DefaultName, "agentry-runner:0.7.60", policy.HarnessImage{}); pin.By != policy.PinTag {
		t.Errorf("another adapter's tag: %+v", pin)
	}
}

func TestTheRunnersOwnHarnessHasNoResultLimitToName(t *testing.T) {
	name := adapter.AgentryHarnessAdapterName
	line := toolResultsEnforcement(name, policy.Model{}, adapter.ResultBudget(name), spec.CapSummary{})
	if strings.Contains(line, "measured tool-result limit") || !strings.Contains(line, "the runner's own page") {
		t.Errorf("receipt line: %s", line)
	}
	small := adapter.PageBudget(name, policy.Model{ContextWindow: 32000})
	line = toolResultsEnforcement(name, policy.Model{ContextWindow: 32000}, small, spec.CapSummary{})
	if strings.Contains(line, "measured tool-result limit") || !strings.Contains(line, "context window of 32000 tokens") {
		t.Errorf("receipt line on a small window: %s", line)
	}
	other := toolResultsEnforcement(adapter.DefaultName, policy.Model{}, adapter.ResultBudget(adapter.DefaultName), spec.CapSummary{})
	if !strings.Contains(other, "the docker-agent harness's measured tool-result limit less a margin") {
		t.Errorf("another adapter's line changed: %s", other)
	}
}
