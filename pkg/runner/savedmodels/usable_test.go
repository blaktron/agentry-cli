package savedmodels

import (
	"errors"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

func runModel(t *testing.T, pol *policy.Policy, ref string) localrun.RunModel {
	t.Helper()
	rm, err := localrun.ChooseRunModel(pol, ref, "")
	if err != nil {
		t.Fatal(err)
	}
	return rm
}

func wantUnusable(t *testing.T, err error, code string, words ...string) {
	t.Helper()
	var ce *engine.ClassifiedError
	if !errors.As(err, &ce) || ce.Code != engine.ExitPolicy {
		t.Fatalf("want an exit-%d refusal, got %v", engine.ExitPolicy, err)
	}
	u, ok := IsUnusable(err)
	if !ok || u.Code != code {
		t.Fatalf("want an UnusableError %s, got %#v", code, err)
	}
	for _, w := range words {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("refusal %q lacks %q", err, w)
		}
	}
}

// A saved model whose key is in neither its variable nor the keychain is
// refused by its name, its provider and the caller's fix, as key-missing;
// with the key in either place it is usable.
func TestASavedModelWhoseKeyIsNotHereIsRefusedByName(t *testing.T) {
	pol := policy.Default()
	if err := pol.AddProvider("openai", policy.Provider{APIKeyEnv: "OPENAI_API_KEY", APIKeySource: "keyring", ProxyAllow: []string{"api.openai.com:443"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pol.AddModel(policy.SavedModel{Name: "GPT", Provider: "openai", Model: "gpt-5.5"}); err != nil {
		t.Fatal(err)
	}
	rm := runModel(t, pol, "GPT")
	kept, env := map[string]bool{}, map[string]string{}
	here := Here{Getenv: func(k string) string { return env[k] }, KeyKept: func(p string) (bool, error) { return kept[p], nil }}

	err := Usable(t.TempDir(), rm, "pick another", here)
	wantUnusable(t, err, KeyMissing, `the saved model "GPT" (provider openai)`, "$OPENAI_API_KEY", "provider:openai", "pick another")
	if u, _ := IsUnusable(err); u.Saved.Name != "GPT" || u.Provider != "openai" {
		t.Fatalf("unusable %+v", u)
	}
	kept["openai"] = true
	if err := Usable(t.TempDir(), rm, "pick another", here); err != nil {
		t.Fatalf("with the key in the keychain: %v", err)
	}
	kept["openai"], env["OPENAI_API_KEY"] = false, "sk-test"
	if err := Usable(t.TempDir(), rm, "pick another", here); err != nil {
		t.Fatalf("with the key in its variable: %v", err)
	}
}

// A saved model that spends a sign-in no longer kept is refused by name as
// needs-sign-in; an endpoint that takes no key is always usable.
func TestASavedModelWhoseSignInIsNotKeptIsRefusedByName(t *testing.T) {
	_, _, pol := lab(t)
	if err := pol.AddProvider("openai", policy.Provider{APIKeyKind: policy.KeyKindChatGPTPlan, ProxyAllow: []string{"api.openai.com:443"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pol.AddModel(policy.SavedModel{Name: "Plan", Provider: "openai", Model: "gpt-5.5"}); err != nil {
		t.Fatal(err)
	}
	here := Here{SignInKept: func(string, string) bool { return false }}
	wantUnusable(t, Usable(t.TempDir(), runModel(t, pol, "Plan"), "pick another", here), NeedsSignIn,
		`the saved model "Plan" (provider openai)`, "model-signins add chatgpt")
	if err := Usable(t.TempDir(), runModel(t, pol, "Qwen"), "pick another", Here{}); err != nil {
		t.Fatalf("an endpoint that takes no key: %v", err)
	}
}
