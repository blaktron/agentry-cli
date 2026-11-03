package engine

// The model key from the OS keychain: the environment wins when set;
// the keychain account the plan names is the fallback; a missing entry
// is a refusal that names the variable and the fix; and a key read from
// the keychain is redacted like one from the environment.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
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

	// A failed read is remembered as well: the run is refused with the error
	// the redactor already met, and the keychain is asked once.
	failures := 0
	resetKeyringOnce()
	keyringLookup = func(string) (string, error) { failures++; return "", errors.New("the OS keychain is locked") }
	_ = hostSecretValues([]spec.EnvVar{v})
	if _, err := hostSecret(v); err == nil || !strings.Contains(err.Error(), "the OS keychain is locked") || failures != 1 {
		t.Fatalf("a failed read: %v after %d lookups, want one", err, failures)
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
	// Without a keyring account the old refusal stands, untouched: naming the
	// name the harness reads it by only when that is another name.
	_, err = hostSecret(spec.EnvVar{Name: "K", FromHostEnv: "TEST_MODEL_KEY"})
	if err == nil || !strings.Contains(err.Error(), "required environment variable TEST_MODEL_KEY is not set (the policy names it; the harness reads it as K)") {
		t.Fatalf("err = %v", err)
	}
	_, err = hostSecret(spec.EnvVar{Name: "TEST_MODEL_KEY", FromHostEnv: "TEST_MODEL_KEY"})
	if err == nil || !strings.HasSuffix(err.Error(), "required environment variable TEST_MODEL_KEY is not set (the policy names it)") {
		t.Fatalf("err = %v", err)
	}
	if vals := hostSecretValues([]spec.EnvVar{v}); len(vals) != 0 {
		t.Fatalf("a failed lookup must add nothing to redact: %q", vals)
	}
	// Nor does a keychain entry that holds nothing: an empty value in the
	// redactor would mask every character.
	fakeKeyring(t, map[string]string{"provider:anthropic": ""})
	if vals := hostSecretValues([]spec.EnvVar{v}); len(vals) != 0 {
		t.Fatalf("an empty keychain value must add nothing to redact: %q", vals)
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
	// The receipt names the kind of the credential, never its value: an API
	// key for a key read from the environment, the kind the plan was compiled
	// for when it names one, the sign-in's kind for a sign-in; the harness on
	// this computer is read by its own environment.
	if env.Harness.CredentialKind != policy.KeyKindAPI {
		t.Fatalf("receipt credential kind = %q", env.Harness.CredentialKind)
	}
	keyVar := spec.EnvVar{Name: "K", FromHostEnv: "TEST_MODEL_KEY"}
	subscription := spec.EnvVar{Name: "T", FromHostEnv: "TEST_MODEL_KEY", ExpectedKind: &spec.CredentialKind{Provider: "anthropic", Kind: "subscription"}}
	inContainer := func(vars ...spec.EnvVar) *spec.Plan {
		return &spec.Plan{Harness: spec.HarnessSpec{Agent: spec.ContainerSpec{Env: vars}}}
	}
	onHost := func(vars ...spec.EnvVar) *spec.Plan {
		return &spec.Plan{Harness: spec.HarnessSpec{Host: &spec.HostProcessSpec{Env: vars}, Agent: spec.ContainerSpec{Env: []spec.EnvVar{keyVar}}}}
	}
	for name, c := range map[string]struct {
		plan *spec.Plan
		want string
	}{
		"a key from the environment":             {inContainer(keyVar), policy.KeyKindAPI},
		"a kind the plan was compiled for":       {inContainer(keyVar, subscription), "subscription"},
		"no key at all":                          {inContainer(spec.EnvVar{Name: "A", Value: "literal"}), ""},
		"a key for the harness on this computer": {onHost(subscription), "subscription"},
		"a sign-in":                              {&spec.Plan{Harness: spec.HarnessSpec{ModelSignIn: &spec.ModelSignInSpec{Kind: policy.KeyKindChatGPTPlan}, Agent: spec.ContainerSpec{Env: []spec.EnvVar{keyVar}}}}, policy.KeyKindChatGPTPlan},
	} {
		if got := credentialKind(c.plan); got != c.want {
			t.Errorf("%s: credential kind %q, want %q", name, got, c.want)
		}
	}
	for _, v := range []string{p.Plan.Harness.Agent.Env[0].Value} {
		if v != "" && strings.Contains(line, v) {
			t.Fatalf("a value reached the receipt line: %q", line)
		}
	}
}

// The plan fixed the harness's argv and variable by the credential's
// kind; a value of another kind is refused by name rather than run
// against the wrong plan, from the environment and from the keychain
// alike. A shape this build does not know passes as an API key.
func TestHostSecretHoldsTheValueAgainstTheKindThePlanExpects(t *testing.T) {
	fakeKeyring(t, map[string]string{"provider:anthropic": "sk-ant-oat01-fromkeyring"})
	expectAPI := &spec.CredentialKind{Provider: "anthropic", Kind: "api"}
	expectSub := &spec.CredentialKind{Provider: "anthropic", Kind: "subscription"}

	t.Setenv("TEST_MODEL_KEY", "sk-ant-oat01-secret")
	_, err := hostSecret(spec.EnvVar{Name: "ANTHROPIC_AUTH_TOKEN", FromHostEnv: "TEST_MODEL_KEY", ExpectedKind: expectAPI})
	if err == nil || !strings.Contains(err.Error(), "policy says the credential is api") || !strings.Contains(err.Error(), "$TEST_MODEL_KEY is subscription") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("subscription token under an api plan: %v", err)
	}
	if got, err := hostSecret(spec.EnvVar{Name: "CLAUDE_CODE_OAUTH_TOKEN", FromHostEnv: "TEST_MODEL_KEY", ExpectedKind: expectSub}); err != nil || got != "sk-ant-oat01-secret" {
		t.Fatalf("subscription token under a subscription plan: %q %v", got, err)
	}

	t.Setenv("TEST_MODEL_KEY", "sk-ant-api03-secret")
	_, err = hostSecret(spec.EnvVar{Name: "CLAUDE_CODE_OAUTH_TOKEN", FromHostEnv: "TEST_MODEL_KEY", ExpectedKind: expectSub})
	if err == nil || !strings.Contains(err.Error(), "is api") {
		t.Fatalf("api key under a subscription plan: %v", err)
	}
	t.Setenv("TEST_MODEL_KEY", "sk-ant-admin01-odd")
	if _, err := hostSecret(spec.EnvVar{Name: "ANTHROPIC_AUTH_TOKEN", FromHostEnv: "TEST_MODEL_KEY", ExpectedKind: expectAPI}); err != nil {
		t.Fatalf("an unknown shape passes as an api key: %v", err)
	}

	// The keychain value is held to the same rule, and named as the source.
	t.Setenv("TEST_MODEL_KEY", "")
	_, err = hostSecret(spec.EnvVar{Name: "ANTHROPIC_AUTH_TOKEN", FromHostEnv: "TEST_MODEL_KEY", FromKeyring: "provider:anthropic", ExpectedKind: expectAPI})
	if err == nil || !strings.Contains(err.Error(), "the OS keychain entry provider:anthropic is subscription") {
		t.Fatalf("keychain: %v", err)
	}
	// No expectation: any value passes (the other providers).
	t.Setenv("TEST_MODEL_KEY", "anything")
	if got, err := hostSecret(spec.EnvVar{Name: "OPENAI_API_KEY", FromHostEnv: "TEST_MODEL_KEY"}); err != nil || got != "anything" {
		t.Fatalf("no expectation: %q %v", got, err)
	}
}
