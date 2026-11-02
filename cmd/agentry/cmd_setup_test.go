// `agentry setup`: the policy-writing half, which needs no docker.
// Provider tables fill gaps under --yes, a custom endpoint derives the
// proxy allowlist from --base-url (and refuses to guess one), and
// re-running preserves everything outside the model block.
package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
)

// setupPolicy runs `agentry setup --skip-images` against a temp policy and
// returns the written policy file text.
func setupPolicy(t *testing.T, extra ...string) (string, string, error) {
	t.Helper()
	stdout, _, written, err := setupPolicyErr(t, extra...)
	return stdout, written, err
}

// setupPolicyErr is setupPolicy with the command's stderr as well.
func setupPolicyErr(t *testing.T, extra ...string) (stdout, stderr, written string, err error) {
	t.Helper()
	isolateEnv(t)
	isolateRunnerHome(t)
	t.Setenv("OPENAI_API_KEY", "")
	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	args := append([]string{"setup", "--skip-images", "--policy", policyPath}, extra...)
	stdout, stderr, err = runCLI(t, strings.NewReader(""), args...)
	raw, _ := os.ReadFile(policyPath)
	return stdout, stderr, string(raw), err
}

func TestSetupYesWritesProviderDefaults(t *testing.T) {
	stdout, stderr, written, err := setupPolicyErr(t, "--yes", "--provider", "openai")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "setup complete (policy only; images skipped)") {
		t.Errorf("setup banner = %q", stdout)
	}
	// The variable the policy names is unset here: setup says so, with
	// the two ways to supply the key, even under --yes.
	if !strings.Contains(stderr, "warning: $OPENAI_API_KEY is not set in this environment") || !strings.Contains(stderr, "store the key in the OS keychain") {
		t.Errorf("no warning for the unset variable: %q", stderr)
	}
	// Setup ends with what comes next, on stderr: doctor, then search or run.
	if !strings.Contains(stderr, "next steps:\n  1. agentry doctor\n") || !strings.Contains(stderr, "agentry search <words>") || !strings.Contains(stderr, "agentry run <ns/name>") {
		t.Errorf("next steps = %q", stderr)
	}
	// The table row for openai, and the generated-file warning header.
	for _, want := range []string{
		"provider: openai", "name: gpt-6.1-sol", "apiKeyEnv: OPENAI_API_KEY",
		"api.openai.com:443", "comments are not preserved",
	} {
		if !strings.Contains(written, want) {
			t.Errorf("policy lacks %q:\n%s", want, written)
		}
	}
	// Round-trip: the written file must load under the closed schema.
	reload := filepath.Join(t.TempDir(), "reload.yaml")
	if err := os.WriteFile(reload, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := policy.Load(reload); err != nil {
		t.Errorf("the policy setup wrote does not load back: %v", err)
	}
}

// TestSetupCustomProvider: a custom endpoint must derive the agent
// container's ONLY egress from --base-url — and refuse to proceed without
// one rather than inventing an allowlist.
func TestSetupCustomProvider(t *testing.T) {
	// Custom has no table row: under --yes there is no model default to
	// fall back to, and no base URL to derive the allowlist from. Each gap
	// is refused with its flag named, in that order.
	_, _, err := setupPolicy(t, "--yes", "--provider", "custom")
	if err == nil || !strings.Contains(err.Error(), "--model") {
		t.Fatalf("custom without --model: %v", err)
	}
	_, _, err = setupPolicy(t, "--yes", "--provider", "custom", "--model", "local-llm")
	if err == nil || !strings.Contains(err.Error(), "--base-url") {
		t.Fatalf("custom without --base-url: %v", err)
	}
	// Setup failures are policy-class (exit 11) so scripts can branch.
	wantClassified(t, err, engine.ExitPolicy)

	asked := ""
	probe := probeContextWindow
	probeContextWindow = func(_ context.Context, baseURL, model, _ string) (setup.ContextWindow, error) {
		asked = baseURL + " " + model
		return setup.ContextWindow{Tokens: 131072, Source: baseURL + "/models"}, nil
	}
	t.Cleanup(func() { probeContextWindow = probe })
	_, written, err := setupPolicy(t, "--yes", "--provider", "custom",
		"--model", "local-llm", "--api-key-env", "LOCAL_KEY", "--base-url", "https://llm.internal:8443/v1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"provider: custom", "name: local-llm", "apiKeyEnv: LOCAL_KEY",
		"baseUrl: https://llm.internal:8443/v1", "llm.internal:8443", "contextWindow: 131072"} {
		if !strings.Contains(written, want) {
			t.Errorf("custom policy lacks %q:\n%s", want, written)
		}
	}
	if asked != "https://llm.internal:8443/v1 local-llm" {
		t.Errorf("the endpoint was asked %q", asked)
	}
	// The flag wins over what the endpoint says.
	_, written, err = setupPolicy(t, "--yes", "--provider", "custom", "--context-window", "262144",
		"--model", "local-llm", "--api-key-env", "LOCAL_KEY", "--base-url", "https://llm.internal:8443/v1")
	if err != nil || !strings.Contains(written, "contextWindow: 262144") {
		t.Fatalf("--context-window: %v\n%s", err, written)
	}
}

// TestSetupNonInteractiveDefaults: with a piped stdin and no --yes, setup
// still cannot prompt (no TTY), so the anthropic defaults land.
func TestSetupNonInteractiveDefaults(t *testing.T) {
	_, written, err := setupPolicy(t)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(written, "provider: anthropic") || !strings.Contains(written, "api.anthropic.com:443") {
		t.Errorf("non-interactive defaults:\n%s", written)
	}
}

// TestSetupPreservesNonModelPolicy: re-running setup edits the model block
// and nothing else (everything else in the policy is preserved).
func TestSetupPreservesNonModelPolicy(t *testing.T) {
	isolateEnv(t)
	isolateRunnerHome(t)
	policyPath := filepath.Join(t.TempDir(), "policy.yaml")

	pol := policy.Default()
	pol.AllowWildcardEgress = true
	pol.Budgets.TimeoutSeconds = 123
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}

	_, _, err := runCLI(t, strings.NewReader(""), "setup", "--skip-images", "--policy", policyPath,
		"--yes", "--provider", "google")
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := policy.Load(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model.Provider != "google" || got.Model.Name != "gemini-4-argon" {
		t.Errorf("model block not updated: %+v", got.Model)
	}
	if !got.AllowWildcardEgress || got.Budgets.TimeoutSeconds != 123 {
		t.Errorf("non-model policy was not preserved: %+v", got)
	}
}

// TestSetupAPIKeySourceFlag: --api-key-source keyring records the
// keychain as the fallback and the JSON report says so; the value is
// nowhere.
func TestSetupAPIKeySourceFlag(t *testing.T) {
	_, written, err := setupPolicy(t, "--yes", "--provider", "anthropic", "--api-key-source", "keyring")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(written, "apiKeySource: keyring") || !strings.Contains(written, "apiKeyEnv: ANTHROPIC_API_KEY") {
		t.Fatalf("policy:\n%s", written)
	}
	_, _, err = setupPolicy(t, "--yes", "--provider", "anthropic", "--api-key-source", "vault")
	if err == nil || !strings.Contains(err.Error(), "must be env or keyring") {
		t.Fatalf("vault: %v", err)
	}
}

// `agentry setup --provider agentry` records the registry this computer is
// signed in to, with no key; signed out, it says to sign in and writes
// nothing.
func TestSetupProviderAgentryPaysThroughTheSignedInRegistry(t *testing.T) {
	isolateEnv(t)
	isolateRunnerHome(t)
	const reg = "https://dev.secureagentry.net"
	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	args := []string{"setup", "--skip-images", "--policy", policyPath, "--registry", reg, "--yes", "--provider", "agentry"}
	if _, _, err := runCLI(t, strings.NewReader(""), args...); err == nil || !strings.Contains(err.Error(), "agentry login") {
		t.Fatalf("signed out: %v", err)
	}
	if b, _ := os.ReadFile(policyPath); strings.Contains(string(b), "provider: agentry") {
		t.Fatal("a signed-out setup wrote provider agentry")
	}
	storeFileToken(t, reg, "agy_fakesigninforthesetuptestonly000000000000000000")
	if _, _, err := runCLI(t, strings.NewReader(""), args...); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(policyPath)
	for _, want := range []string{"provider: agentry", "baseUrl: https://dev.secureagentry.net/api/v1/x/inference/v1", "dev.secureagentry.net:443"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("policy lacks %q:\n%s", want, raw)
		}
	}
	if regexp.MustCompile(`apiKeyEnv: [A-Za-z]`).Match(raw) || strings.Contains(string(raw), "agy_fake") {
		t.Errorf("the policy names a key or holds the sign-in:\n%s", raw)
	}
}
