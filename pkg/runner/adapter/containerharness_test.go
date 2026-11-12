package adapter

import (
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// A run that names no adapter takes the container harness the policy
// chooses, and a model paid by a model sign-in takes its vendor's whatever
// the policy chooses.
func TestTheDefaultAdapterFollowsThePolicysContainerHarness(t *testing.T) {
	if AgentryHarnessAdapterName != policy.AgentryHarnessAdapter || DefaultName != policy.DockerAgentAdapter {
		t.Fatalf("the policy names the harnesses %q and %q", policy.DockerAgentAdapter, policy.AgentryHarnessAdapter)
	}
	for _, name := range policy.ContainerHarnesses() {
		if _, err := Get(name); err != nil {
			t.Errorf("%s is not a registered adapter: %v", name, err)
		}
		if !SpendsCredits(name) {
			t.Errorf("%s cannot spend credits", name)
		}
	}
	pol := policy.Default()
	if got := DefaultFor(pol); got != DefaultName {
		t.Fatalf("a policy that never chose: %q", got)
	}
	pol.ContainerHarness = AgentryHarnessAdapterName
	if got := DefaultFor(pol); got != AgentryHarnessAdapterName {
		t.Fatalf("containerHarness agentry-harness: %q", got)
	}
	for kind, want := range map[string]string{
		policy.KeyKindChatGPTPlan:   CodexImageAdapterName,
		policy.KeyKindClaudeSignIn:  ClaudeImageAdapterName,
		policy.KeyKindCopilotSignIn: CopilotImageAdapterName,
	} {
		pol.Model.APIKeyKind = kind
		if got := DefaultFor(pol); got != want {
			t.Errorf("a model paid by %s runs on %q, want %q", kind, got, want)
		}
	}
}

// A provider the runner's own harness does not reach is refused with the
// way back to the default harness.
func TestTheRunnersOwnHarnessRefusalNamesTheWayBack(t *testing.T) {
	if err := AgentryHarnessReaches(policy.Default().Model); err != nil {
		t.Fatalf("the default model: %v", err)
	}
	err := AgentryHarnessReaches(policy.Model{Provider: "amazon-bedrock", Name: "some-model"})
	if err == nil || !strings.Contains(err.Error(), "--adapter docker-agent") || !strings.Contains(err.Error(), "agentry harness use docker-agent") {
		t.Fatalf("an unreached provider: %v", err)
	}
}
