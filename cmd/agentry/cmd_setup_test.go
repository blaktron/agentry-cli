// `agentry setup`: the policy-writing half, which needs no docker.
// Provider tables fill gaps under --yes, a custom endpoint derives the
// proxy allowlist from --base-url (and refuses to guess one), and
// re-running preserves everything outside the model block.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// setupPolicy runs `agentry setup --skip-images` against a temp policy and
// returns the written policy file text.
func setupPolicy(t *testing.T, extra ...string) (string, string, error) {
	t.Helper()
	isolateEnv(t)
	isolateRunnerHome(t)
	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	args := append([]string{"setup", "--skip-images", "--policy", policyPath}, extra...)
	stdout, _, err := runCLI(t, strings.NewReader(""), args...)
	raw, _ := os.ReadFile(policyPath)
	return stdout, string(raw), err
}

func TestSetupYesWritesProviderDefaults(t *testing.T) {
	stdout, written, err := setupPolicy(t, "--yes", "--provider", "openai")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "setup complete (policy only; images skipped)") {
		t.Errorf("setup banner = %q", stdout)
	}
	// The table row for openai, and the generated-file warning header.
	for _, want := range []string{
		"provider: openai", "name: gpt-5.2", "apiKeyEnv: OPENAI_API_KEY",
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

	_, written, err := setupPolicy(t, "--yes", "--provider", "custom",
		"--model", "local-llm", "--api-key-env", "LOCAL_KEY", "--base-url", "https://llm.internal:8443/v1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"provider: custom", "name: local-llm", "apiKeyEnv: LOCAL_KEY",
		"baseUrl: https://llm.internal:8443/v1", "llm.internal:8443"} {
		if !strings.Contains(written, want) {
			t.Errorf("custom policy lacks %q:\n%s", want, written)
		}
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
	if got.Model.Provider != "google" || got.Model.Name != "gemini-3-pro" {
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
