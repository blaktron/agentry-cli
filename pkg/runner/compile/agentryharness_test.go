package compile

import (
	"errors"
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

func TestCreditsCompileForTheHarnessesThatCanSpendThem(t *testing.T) {
	m := parseManifest(t, pureTextYAML)
	ws := workspaceFor(t, m, nil)
	compileOn := func(name string) (*spec.Plan, error) {
		pol := testPolicy()
		pol.Model = policy.Model{Provider: policy.ProviderAgentry, Name: "agentry", BaseURL: "https://registry.example" + policy.AgentryInferencePath}
		return Compile(Inputs{
			Manifest: m, Digest: "sha256:0123456789abcdef", Source: "local", WorkspaceDir: ws, AdapterName: name,
			Policy: pol, PolicySHA: "fixedpolicysha", RunnerImage: "agentry-runner:test",
		}, t.TempDir())
	}
	// Both container harnesses reach the registry's endpoint, each with the
	// run's credits session as its key and never a key of the person's.
	for _, name := range []string{adapter.DefaultName, adapter.AgentryHarnessAdapterName} {
		plan, err := compileOn(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		session := 0
		for _, v := range plan.Harness.Agent.Env {
			if v.FromInference {
				session++
			}
			if v.FromHostEnv != "" && strings.Contains(v.FromHostEnv, "API_KEY") {
				t.Errorf("%s: the plan reads %s from the host", name, v.FromHostEnv)
			}
		}
		if session != 1 {
			t.Errorf("%s: %d variables carry the credits session, want 1: %+v", name, session, plan.Harness.Agent.Env)
		}
	}
	// A harness with no such client is refused by name, and told which can.
	_, err := compileOn(adapter.ClaudeAdapterName)
	var refusal *adapter.Refusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "runs on the docker-agent and agentry-harness harnesses only; claude cannot spend them") {
		t.Errorf("credits on a harness that cannot spend them: %v", err)
	}
}
