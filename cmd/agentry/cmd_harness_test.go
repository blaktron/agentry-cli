package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage"
)

// The container harness is read and changed from the terminal, kept in the
// policy file, and a name that is neither harness is refused with both.
func TestTheContainerHarnessFromTheTerminal(t *testing.T) {
	_, path := modelsFixture(t)
	if out := cli(t, "harness"); !strings.Contains(out, "harness: docker-agent (Docker Agent), the default") {
		t.Fatalf("a policy that never chose: %q", out)
	}
	out, errOut, err := runCLI(t, strings.NewReader(""), "harness", "use", "agentry-harness")
	if err != nil || !strings.Contains(out, "harness: agentry-harness (the harness built into the runner) (was docker-agent)") || !strings.Contains(errOut, "a persistent agent from its next turn") ||
		!strings.Contains(errOut, "released before this setting existed refuses a policy that holds this choice") {
		t.Fatalf("use agentry-harness: %q / %q, %v", out, errOut, err)
	}
	text, pol := readPolicy(t, path)
	if pol.ContainerHarness != policy.AgentryHarnessAdapter || !strings.Contains(text, "containerHarness: agentry-harness") {
		t.Fatalf("the policy after use:\n%s", text)
	}
	// The adapter list marks the harness a run now takes.
	if list := cli(t, "run", "--list-adapters"); !strings.Contains(list, "agentry-harness (default)") || strings.Contains(list, "docker-agent (default)") {
		t.Errorf("the adapter list after use: %q", list)
	}
	var shown harnessOut
	if err := json.Unmarshal([]byte(cli(t, "--json", "harness")), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.Harness != policy.AgentryHarnessAdapter || shown.Default || shown.RunsOn != policy.AgentryHarnessAdapter || shown.PolicyPath != path ||
		strings.Join(shown.Choices, ",") != "docker-agent,agentry-harness" || shown.Previous != "" {
		t.Fatalf("harness --json: %+v", shown)
	}

	// Everything else the policy holds is as it was, and a second model
	// saved afterwards keeps the choice.
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	cli(t, "providers", "add", "openai")
	cli(t, "models", "add", "GPT", "--provider", "openai", "--model", "gpt-5.5")
	if _, pol = readPolicy(t, path); pol.RunContainerHarness() != policy.AgentryHarnessAdapter || pol.DefaultModel().Name != "anthropic" {
		t.Fatalf("after a second saved model: %q, default %+v", pol.ContainerHarness, pol.DefaultModel())
	}

	for _, bad := range []string{"claude", "codex-image", "nope"} {
		_, _, err := runCLI(t, strings.NewReader(""), "harness", "use", bad)
		var ce *engine.ClassifiedError
		if !errors.As(err, &ce) || ce.Code != engine.ExitUsage || !strings.Contains(err.Error(), "docker-agent or agentry-harness") || !strings.Contains(err.Error(), bad) {
			t.Errorf("use %s: %v", bad, err)
		}
	}
	if _, pol = readPolicy(t, path); pol.ContainerHarness != policy.AgentryHarnessAdapter {
		t.Fatalf("a refused name changed the policy to %q", pol.ContainerHarness)
	}

	var back harnessOut
	if err := json.Unmarshal([]byte(cli(t, "--json", "harness", "use", "docker-agent")), &back); err != nil {
		t.Fatal(err)
	}
	if back.Harness != policy.DockerAgentAdapter || !back.Default || back.Previous != policy.AgentryHarnessAdapter {
		t.Fatalf("use docker-agent --json: %+v", back)
	}
	// Going back to the default says nothing of older readers: the key is gone.
	cli(t, "harness", "use", "agentry-harness")
	if _, errOut, err := runCLI(t, strings.NewReader(""), "harness", "use", "docker-agent"); err != nil || strings.Contains(errOut, "refuses a policy") {
		t.Fatalf("use docker-agent: %q, %v", errOut, err)
	}
	if text, pol = readPolicy(t, path); pol.ContainerHarness != "" || strings.Contains(text, "containerHarness") {
		t.Fatalf("the default is written as a key:\n%s", text)
	}
	if out := cli(t, "harness", "use", "docker-agent"); !strings.Contains(out, "(unchanged)") {
		t.Errorf("choosing the harness already chosen: %q", out)
	}
}

// A default model paid by a model sign-in runs on its vendor's harness
// whatever the policy chooses, and the command says so.
func TestTheContainerHarnessSaysWhenASignInDecides(t *testing.T) {
	_, path := modelsFixture(t)
	pol, _, err := policy.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	pol.Model = policy.Model{Provider: "openai", Name: "gpt-5.5", APIKeyKind: policy.KeyKindChatGPTPlan, ProxyAllow: []string{"chatgpt.com:443"}}
	pol.ContainerHarness = policy.AgentryHarnessAdapter
	if err := policy.Save(path, pol); err != nil {
		t.Fatal(err)
	}
	out := cli(t, "harness")
	if !strings.Contains(out, "harness: agentry-harness") || !strings.Contains(out, "the default model is paid by a ChatGPT plan sign-in: runs on it use codex-image whatever this says") {
		t.Fatalf("harness beside a sign-in: %q", out)
	}
	var shown harnessOut
	if err := json.Unmarshal([]byte(cli(t, "--json", "harness")), &shown); err != nil || shown.RunsOn != "codex-image" || shown.Harness != policy.AgentryHarnessAdapter {
		t.Fatalf("harness --json beside a sign-in: %+v, %v", shown, err)
	}
}

// Setup's summary names the harness runs use, and keeps the choice a
// policy already made.
func TestSetupSaysTheHarnessAndKeepsTheChoice(t *testing.T) {
	stdout, written, err := setupPolicy(t, "--yes", "--provider", "openai")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "harness: docker-agent (agentry harness use <docker-agent|agentry-harness> changes it)") || strings.Contains(written, "\ncontainerHarness:") {
		t.Fatalf("a first setup: %q\n%s", stdout, written)
	}

	_, path := modelsFixture(t)
	cli(t, "harness", "use", "agentry-harness")
	out := cli(t, "--json", "setup", "--skip-images", "--policy", path, "--yes", "--provider", "openai")
	var res setupOutput
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Harness != policy.AgentryHarnessAdapter {
		t.Fatalf("setup --json: %s (%v)", out, err)
	}
	if _, pol := readPolicy(t, path); pol.ContainerHarness != policy.AgentryHarnessAdapter || pol.Model.Provider != "openai" {
		t.Fatalf("setup dropped the choice: %q on %s", pol.ContainerHarness, pol.Model.Provider)
	}
	if out := cli(t, "setup", "--skip-images", "--policy", path, "--yes", "--provider", "openai"); !strings.Contains(out, "harness: agentry-harness (agentry harness use") {
		t.Errorf("setup's summary: %q", out)
	}
}

// Doctor names the harness runs use. On the runner's own harness it asks
// for no harness image, and it fails a model that harness does not reach.
func TestDoctorNamesTheContainerHarness(t *testing.T) {
	policyPath, fake := doctorFixture(t)
	fr := newFakeRegistry(t)
	out, _, err := runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if err != nil || !strings.Contains(out, "ok    harness") || !strings.Contains(out, "docker-agent (Docker Agent, the default") || !strings.Contains(out, "ok    image:harness") {
		t.Fatalf("doctor on the default: %v\n%s", err, out)
	}

	pol, _, err := policy.Load(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	pol.ContainerHarness = policy.AgentryHarnessAdapter
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
	// The docker-agent image is not on this machine, and is not asked for.
	fake.ExistingImages = map[string]bool{pol.Images.Gateway: true, pol.RunnerImageTag(runnerimage.ContentVersion()): true}
	out, _, err = runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if err != nil || !strings.Contains(out, "agentry-harness (the harness built into the runner") || strings.Contains(out, "image:harness") || !strings.Contains(out, "ready") {
		t.Fatalf("doctor on the runner's own harness: %v\n%s", err, out)
	}

	pol.Model = policy.Model{Provider: "amazon-bedrock", Name: "some-model", APIKeyEnv: "DOCTOR_TEST_KEY", ProxyAllow: []string{"bedrock.example:443"}}
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
	out, _, err = runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if err == nil || !strings.Contains(out, "FAIL  harness") || !strings.Contains(out, "agentry harness use docker-agent") {
		t.Fatalf("doctor on a model the harness does not reach: %v\n%s", err, out)
	}
}
