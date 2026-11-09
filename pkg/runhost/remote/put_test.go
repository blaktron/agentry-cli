package remote

import (
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// A device's saved model is prepared for a runhost with its provider and no
// key, and what a runhost cannot take is refused before anything is sent: a
// pasted Claude subscription token, a key variable it does not take, and a
// connected token only the device renews.
func TestASavedModelIsPreparedForARunhostWithoutItsKey(t *testing.T) {
	pol := policy.Default()
	add := func(id string, pr policy.Provider, sm policy.SavedModel) {
		t.Helper()
		if err := pol.AddProvider(id, pr); err != nil {
			t.Fatal(err)
		}
		if _, err := pol.AddModel(sm); err != nil {
			t.Fatal(err)
		}
	}
	add("openai", policy.Provider{APIKeyEnv: "OPENAI_API_KEY", APIKeySource: "keyring", ProxyAllow: []string{"api.openai.com:443"}},
		policy.SavedModel{Name: "GPT", Provider: "openai", Model: "gpt-5.5"})
	add("huggingface", policy.Provider{APIKeyEnv: "HF_TOKEN", APIKeySource: "keyring", ProxyAllow: []string{"router.huggingface.co:443"}},
		policy.SavedModel{Name: "HF", Provider: "huggingface", Model: "qwen"})

	p, err := Prepare(pol, "GPT", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != ModelKindKey || p.Doc.Key != "" || p.Doc.ProviderID != "openai" || p.Doc.Provider.APIKeyEnv != "OPENAI_API_KEY" || p.Doc.Model != "gpt-5.5" || p.Model.KeyProvider() != "openai" {
		t.Fatalf("prepared %+v", p)
	}
	if _, err := Prepare(pol, "HF", func(string) bool { return true }); err == nil || !strings.Contains(err.Error(), "renewed on this computer") {
		t.Fatalf("a connected token renewed here: %v", err)
	}
	if _, err := Prepare(pol, "HF", func(string) bool { return false }); err != nil {
		t.Fatalf("a pasted token: %v", err)
	}
	if _, err := Prepare(pol, "nothing", nil); err == nil {
		t.Fatal("a saved model the computer lacks was prepared")
	}
}
