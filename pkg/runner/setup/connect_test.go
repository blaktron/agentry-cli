package setup

import (
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

type fakeConnect struct {
	kept     map[string]string
	connects []string
}

func (f *fakeConnect) offer() ConnectOffer {
	return ConnectOffer{
		Kept: func(p string) string { return f.kept[p] },
		Connect: func(p string) (string, error) {
			f.connects = append(f.connects, p)
			return "person@example.com", nil
		},
	}
}

// Choosing connect for each provider connects the account and writes a
// policy whose key is the provider's keychain entry (where the connect kept
// it), the variable docker-agent reads, no base URL, the provider's host
// the only egress, and the model id the person typed (D33): never a value.
func TestSetupConnectsEachProvidersAccount(t *testing.T) {
	t.Setenv(modelkey.EnvNoKeyring, "")
	cases := []struct{ provider, model, env, allow string }{
		{policy.ProviderOpenRouter, "anthropic/claude-sonnet-4.5", "OPENROUTER_API_KEY", "openrouter.ai:443"},
		{policy.ProviderPoe, "Claude-Sonnet-4.6", "POE_API_KEY", "api.poe.com:443"},
		{policy.ProviderHuggingFace, "openai/gpt-oss-120b:groq", "HF_TOKEN", "router.huggingface.co:443"},
	}
	for _, c := range cases {
		pol := policy.Default()
		f := &fakeConnect{}
		p := &askingPrompter{scriptPrompter: scriptPrompter{answers: []string{c.provider, "connect", c.model}}}
		if err := ConfigureModel(p, pol, ModelOpts{Connect: f.offer()}); err != nil {
			t.Fatalf("%s: %v", c.provider, err)
		}
		m := pol.Model
		if m.Provider != c.provider || m.Name != c.model || m.APIKeyEnv != c.env || m.APIKeySource != modelkey.SourceKeyring ||
			m.APIKeyKind != "" || m.BaseURL != "" || !slices.Equal(m.ProxyAllow, []string{c.allow}) {
			t.Fatalf("%s: model block %+v", c.provider, m)
		}
		if !slices.Equal(f.connects, []string{c.provider}) {
			t.Fatalf("%s: connects %v", c.provider, f.connects)
		}
		if err := pol.Validate(); err != nil {
			t.Fatalf("%s: the policy is invalid: %v", c.provider, err)
		}
		if !strings.Contains(p.asked[1], "connect = Connect your account") || !strings.Contains(p.asked[2], "does not list them") {
			t.Fatalf("%s: asked %q", c.provider, p.asked)
		}
	}
}

// A kept connect is offered and kept; --connect with --yes asks nothing
// but needs the model; a key kind other than api beside it is refused, and
// so is --connect for another provider.
func TestSetupKeepsAConnectAndRefusesWhatCannotPay(t *testing.T) {
	t.Setenv(modelkey.EnvNoKeyring, "")
	pol := policy.Default()
	f := &fakeConnect{kept: map[string]string{"poe": "person@example.com"}}
	p := &askingPrompter{scriptPrompter: scriptPrompter{answers: []string{"poe", "connect", "GPT-5.4"}, confirm: true}}
	if err := ConfigureModel(p, pol, ModelOpts{Connect: f.offer()}); err != nil {
		t.Fatal(err)
	}
	if len(f.connects) != 0 || pol.Model.Name != "GPT-5.4" {
		t.Fatalf("connects %v, model %+v", f.connects, pol.Model)
	}
	if err := ConfigureModel(&scriptPrompter{}, policy.Default(), ModelOpts{Provider: "openrouter", ConnectAccount: true, Connect: f.offer(), Yes: true}); err == nil || !strings.Contains(err.Error(), "--model") {
		t.Fatalf("no model: %v", err)
	}
	if err := ConfigureModel(&scriptPrompter{}, policy.Default(), ModelOpts{Provider: "openrouter", Model: "x/y", ConnectAccount: true, Connect: f.offer(), Yes: true}); err != nil || !slices.Equal(f.connects, []string{"openrouter"}) {
		t.Fatalf("--connect --yes: %v (connects %v)", err, f.connects)
	}
	if err := ConfigureModel(&scriptPrompter{}, policy.Default(), ModelOpts{Provider: "poe", Model: "x", APIKeyKind: "subscription", ConnectAccount: true, Connect: f.offer(), Yes: true}); err == nil {
		t.Fatal("a subscription kind beside a connect")
	}
	if err := ConfigureModel(&scriptPrompter{}, policy.Default(), ModelOpts{Provider: "openai", ConnectAccount: true, Connect: f.offer(), Yes: true}); err == nil {
		t.Fatal("--connect for openai")
	}
	if err := ConfigureModel(&scriptPrompter{}, policy.Default(), ModelOpts{Provider: "poe", Model: "x", ConnectAccount: true, Yes: true}); err == nil {
		t.Fatal("--connect with no way to connect")
	}
	t.Setenv(modelkey.EnvNoKeyring, "1")
	if err := ConfigureModel(&scriptPrompter{}, policy.Default(), ModelOpts{Provider: "poe", Model: "x", ConnectAccount: true, Connect: f.offer(), Yes: true}); err == nil || !strings.Contains(err.Error(), modelkey.EnvNoKeyring) {
		t.Fatalf("a connect with no keychain: %v", err)
	}
}

// The key path for a connect provider is a key path: the question answered
// key, or --yes with no --connect, keeps the variable and asks no base URL,
// as for any provider in the table (the API-key path unchanged).
func TestSetupPastesAKeyForAConnectProvider(t *testing.T) {
	pol := policy.Default()
	f := &fakeConnect{}
	p := &askingPrompter{scriptPrompter: scriptPrompter{answers: []string{"huggingface", "key", "Qwen/Qwen3-Coder-480B-A35B-Instruct:novita", ""}}}
	if err := ConfigureModel(p, pol, ModelOpts{Connect: f.offer()}); err != nil {
		t.Fatal(err)
	}
	if len(f.connects) != 0 || pol.Model.APIKeyEnv != "HF_TOKEN" || pol.Model.APIKeySource != "" || !slices.Equal(pol.Model.ProxyAllow, []string{"router.huggingface.co:443"}) {
		t.Fatalf("connects %v, model %+v", f.connects, pol.Model)
	}
	for _, q := range p.asked {
		if strings.Contains(q, "base URL") {
			t.Fatalf("asked %q", q)
		}
	}
	pol = policy.Default()
	if err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Provider: "openrouter", Model: "x/y", Connect: f.offer(), Yes: true}); err != nil || len(f.connects) != 0 || pol.Model.APIKeyEnv != "OPENROUTER_API_KEY" {
		t.Fatalf("--yes: %v %+v", err, pol.Model)
	}
}

// The big catalogues are not listed (D33).
func TestTheConnectProvidersModelsAreNotListed(t *testing.T) {
	for _, p := range []string{"openrouter", "poe", "huggingface"} {
		if _, _, err := modelsEndpoint(p, ""); err == nil || !strings.Contains(err.Error(), "type the model id") {
			t.Errorf("%s: %v", p, err)
		}
	}
}

// A key pasted into the keychain for a connect provider forgets the connect
// kept for it; a key for another provider forgets nothing.
func TestSetupForgetsAConnectAPastedKeyReplaces(t *testing.T) {
	t.Setenv(modelkey.EnvNoKeyring, "")
	var forgot []string
	offer := ConnectOffer{
		Connect: func(string) (string, error) { return "", nil },
		Forget:  func(p string) (string, error) { forgot = append(forgot, p); return "forgotten", nil },
	}
	stored := map[string]string{}
	store := func(p, k string) error { stored[p] = k; return nil }
	pol := policy.Default()
	p := &scriptPrompter{answers: []string{"poe", "key", "GPT-5.4", "POE_API_KEY", "the-pasted-key"}, confirm: true}
	if err := ConfigureModel(p, pol, ModelOpts{Connect: offer, StoreKey: store}); err != nil {
		t.Fatal(err)
	}
	if stored["poe"] != "the-pasted-key" || len(forgot) != 1 || forgot[0] != "poe" {
		t.Fatalf("stored %v, forgot %v", stored, forgot)
	}
	p = &scriptPrompter{answers: []string{"anthropic", "key", "", "", "sk-ant-api03-x"}, confirm: true}
	if err := ConfigureModel(p, policy.Default(), ModelOpts{Connect: offer, StoreKey: store}); err != nil {
		t.Fatal(err)
	}
	if len(forgot) != 1 {
		t.Fatalf("forgot %v for an anthropic key", forgot)
	}
}
