package adapter

import (
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// TestKeyringAccountFollowsThePolicy (agentry-cli#148): the model's source,
// overridden by the harness's; env or unset means no keychain fallback.
func TestKeyringAccountFollowsThePolicy(t *testing.T) {
	in := Input{Model: policy.Default().Model}
	if got := keyringAccount(in); got != "" {
		t.Fatalf("default: %q", got)
	}
	in.Model.APIKeySource = "keyring"
	if got := keyringAccount(in); got != "provider:anthropic" {
		t.Fatalf("model keyring: %q", got)
	}
	in.Harness = policy.HarnessPolicy{APIKeySource: "env"}
	if got := keyringAccount(in); got != "" {
		t.Fatalf("harness env overrides: %q", got)
	}
	in.Model.APIKeySource = ""
	in.Harness = policy.HarnessPolicy{APIKeySource: "keyring"}
	if got := keyringAccount(in); got != "provider:anthropic" {
		t.Fatalf("harness keyring: %q", got)
	}
}

// TestDockerAgentEmitsTheKeyringAccount: the plan's env var names the
// account beside the variable, never a value.
func TestDockerAgentEmitsTheKeyringAccount(t *testing.T) {
	ad, err := Get(DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	in := testInput()
	in.Model.APIKeySource = "keyring"
	res, err := ad.Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range res.Agent.Env {
		if v.Name == in.Model.APIKeyEnv {
			found = true
			if v.FromHostEnv != in.Model.APIKeyEnv || v.FromKeyring != "provider:anthropic" || v.Value != "" {
				t.Fatalf("env var = %+v", v)
			}
		}
	}
	if !found {
		t.Fatalf("no key variable in %+v", res.Agent.Env)
	}
}
