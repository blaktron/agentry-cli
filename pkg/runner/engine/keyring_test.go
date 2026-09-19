package engine

// The model key from the OS keychain (agentry-cli#148): the environment
// wins when set; the keychain account the plan names is the fallback; a
// missing entry is a refusal that names the variable and the fix; and a
// key read from the keychain is redacted like one from the environment.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

func fakeKeyring(t *testing.T, entries map[string]string) {
	t.Helper()
	old := keyringLookup
	resetKeyringOnce()
	keyringLookup = func(account string) (string, error) {
		if v, ok := entries[account]; ok {
			return v, nil
		}
		return "", errors.New("the OS keychain has no entry " + account + ": run `agentry setup` to store the key, or set the environment variable the policy names")
	}
	t.Cleanup(func() { keyringLookup = old; resetKeyringOnce() })
}

func TestKeyringIsReadOnceForTheRedactorAndTheRun(t *testing.T) {
	calls := 0
	old := keyringLookup
	resetKeyringOnce()
	keyringLookup = func(string) (string, error) { calls++; return "from-keyring", nil }
	t.Cleanup(func() { keyringLookup = old; resetKeyringOnce() })
	t.Setenv("TEST_MODEL_KEY", "")
	v := spec.EnvVar{Name: "K", FromHostEnv: "TEST_MODEL_KEY", FromKeyring: "provider:anthropic"}
	_ = hostSecretValues([]spec.EnvVar{v})
	got, err := hostSecret(v)
	if err != nil || got != "from-keyring" || calls != 1 {
		t.Fatalf("got %q err %v calls %d", got, err, calls)
	}
}

func TestHostSecretEnvWinsOverKeyring(t *testing.T) {
	fakeKeyring(t, map[string]string{"provider:anthropic": "from-keyring"})
	t.Setenv("TEST_MODEL_KEY", "from-env")
	v := spec.EnvVar{Name: "ANTHROPIC_API_KEY", FromHostEnv: "TEST_MODEL_KEY", FromKeyring: "provider:anthropic"}
	got, err := hostSecret(v)
	if err != nil || got != "from-env" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestHostSecretFallsBackToKeyring(t *testing.T) {
	fakeKeyring(t, map[string]string{"provider:anthropic": "from-keyring"})
	t.Setenv("TEST_MODEL_KEY", "")
	v := spec.EnvVar{Name: "ANTHROPIC_API_KEY", FromHostEnv: "TEST_MODEL_KEY", FromKeyring: "provider:anthropic"}
	got, err := hostSecret(v)
	if err != nil || got != "from-keyring" {
		t.Fatalf("got %q %v", got, err)
	}
	// The redactor sees the same value.
	vals := hostSecretValues([]spec.EnvVar{v})
	if len(vals) != 1 || vals[0] != "from-keyring" {
		t.Fatalf("redaction values = %q", vals)
	}
}

func TestHostSecretMissingEverywhereNamesTheFix(t *testing.T) {
	fakeKeyring(t, map[string]string{})
	t.Setenv("TEST_MODEL_KEY", "")
	v := spec.EnvVar{Name: "ANTHROPIC_API_KEY", FromHostEnv: "TEST_MODEL_KEY", FromKeyring: "provider:anthropic"}
	_, err := hostSecret(v)
	if err == nil || !strings.Contains(err.Error(), "provider anthropic") || !strings.Contains(err.Error(), "$TEST_MODEL_KEY is not set") || !strings.Contains(err.Error(), "provider:anthropic") || !strings.Contains(err.Error(), "agentry setup") {
		t.Fatalf("err = %v", err)
	}
	// Without a keyring account the old refusal stands, untouched.
	_, err = hostSecret(spec.EnvVar{Name: "K", FromHostEnv: "TEST_MODEL_KEY"})
	if err == nil || !strings.Contains(err.Error(), "required environment variable TEST_MODEL_KEY is not set") {
		t.Fatalf("err = %v", err)
	}
	if vals := hostSecretValues([]spec.EnvVar{v}); len(vals) != 0 {
		t.Fatalf("a failed lookup must add nothing to redact: %q", vals)
	}
}

// TestReceiptNamesTheModelKeySource: the compiler's model.apiKey line
// reaches the receipt's capabilities_enforced, so a reader knows where
// the key came from without ever seeing it.
func TestReceiptNamesTheModelKeySource(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	line := env.CapabilitiesEnforced["model.apiKey"]
	if !strings.HasPrefix(line, "$") || !strings.Contains(line, "from the host environment") {
		t.Fatalf("receipt model.apiKey line = %q", line)
	}
	for _, v := range []string{p.Plan.Harness.Agent.Env[0].Value} {
		if v != "" && strings.Contains(line, v) {
			t.Fatalf("a value reached the receipt line: %q", line)
		}
	}
}
