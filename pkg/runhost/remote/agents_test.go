package remote

import "testing"

// An agent's name or an installed agent's reference has a runner home key;
// anything that could name a path does not.
func TestAnAgentsSocketKey(t *testing.T) {
	for in, want := range map[string]string{"assistant": "assistant", "coder": "coder", "acme/helper": "acme.helper"} {
		if got, ok := AgentSocketKey(in); !ok || got != want {
			t.Errorf("%s: %q %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"../x", "a/b/c", "Assistant", "", "acme/../x"} {
		if _, ok := AgentSocketKey(bad); ok {
			t.Errorf("%q was taken", bad)
		}
	}
}

// A device drives only the agent subcommands a runhost runs, without the
// flags that read a file, a sign-in or a credential from the device.
func TestCheckAgentArgs(t *testing.T) {
	if err := CheckAgentArgs("start", []string{"assistant", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if err := CheckAgentArgs("install", []string{"acme/helper"}); err == nil {
		t.Error("install was taken")
	}
	if err := CheckAgentArgs("configure", []string{"coder", "--no-dev-host"}); err == nil {
		t.Error("a negated refused flag was taken")
	}
}
