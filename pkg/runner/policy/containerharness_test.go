package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The container harness is docker-agent until the policy names the
// runner's own, and a name that is neither is refused with both named.
func TestTheContainerHarnessIsOneOfTwo(t *testing.T) {
	p := Default()
	if got := p.RunContainerHarness(); got != DockerAgentAdapter {
		t.Fatalf("a policy that never chose runs on %q", got)
	}
	var none *Policy
	if got := none.RunContainerHarness(); got != DockerAgentAdapter {
		t.Fatalf("no policy runs on %q", got)
	}
	for _, name := range ContainerHarnesses() {
		p.ContainerHarness = name
		if err := p.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if got := p.RunContainerHarness(); got != name {
			t.Errorf("containerHarness %s runs on %q", name, got)
		}
	}
	for _, name := range []string{"claude", "codex-image", "Agentry-Harness", "goose"} {
		p.ContainerHarness = name
		err := p.Validate()
		if err == nil || !strings.Contains(err.Error(), DockerAgentAdapter) || !strings.Contains(err.Error(), AgentryHarnessAdapter) || !strings.Contains(err.Error(), name) {
			t.Errorf("containerHarness %q: %v", name, err)
		}
	}
}

// The choice is kept by a save and a load, in a file with one model block
// and in one with saved models, and a file that names another harness does
// not load.
func TestTheContainerHarnessIsKeptInThePolicyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	p := Default()
	p.ContainerHarness = AgentryHarnessAdapter
	if err := Save(path, p); err != nil {
		t.Fatal(err)
	}
	got, _, err := Load(path)
	if err != nil || got.ContainerHarness != AgentryHarnessAdapter {
		t.Fatalf("one model block: %+v, %v", got, err)
	}
	if err := got.AddProvider("openai", Provider{APIKeyEnv: "OPENAI_API_KEY", ProxyAllow: []string{"api.openai.com:443"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := got.AddModel(SavedModel{Name: "Second", Provider: "openai", Model: "gpt-5.5"}); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, got); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `policyVersion: "2"`) || !strings.Contains(string(raw), "containerHarness: agentry-harness") {
		t.Fatalf("saved models:\n%s", raw)
	}
	got, _, err = Load(path)
	if err != nil || got.RunContainerHarness() != AgentryHarnessAdapter {
		t.Fatalf("saved models: %v", err)
	}
	// A run's view of the policy keeps the choice.
	view, err := got.ForRun(got.DefaultModel())
	if err != nil || view.RunContainerHarness() != AgentryHarnessAdapter {
		t.Fatalf("a run's view: %v", err)
	}

	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), "containerHarness: agentry-harness", "containerHarness: claude", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "docker-agent or agentry-harness") {
		t.Fatalf("a file naming another harness loaded: %v", err)
	}
}

// The file a first run writes names the key, commented out, so the default
// stays the default.
func TestTheDefaultFileNamesTheContainerHarnessKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	p, _, err := EnsureDefault(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# containerHarness: docker-agent") || p.ContainerHarness != "" {
		t.Fatalf("containerHarness %q in:\n%s", p.ContainerHarness, raw)
	}
}
