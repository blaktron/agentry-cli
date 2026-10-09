package setup

import (
	"errors"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// fakeChatGPT is the sign-in setup reaches; kept is the account of a kept
// sign-in ("" for none), and every SignIn call's freshness is recorded.
type fakeChatGPT struct {
	kept   string
	calls  []bool
	models []string
	err    error
}

func (f *fakeChatGPT) offer() ChatGPTOffer {
	return ChatGPTOffer{
		Kept: func() string { return f.kept },
		SignIn: func(fresh bool) (string, []string, error) {
			f.calls = append(f.calls, fresh)
			return "person@example.com", f.models, f.err
		},
	}
}

func TestSetupSignsInWithChatGPTAndWritesThePlanModel(t *testing.T) {
	pol := policy.Default()
	f := &fakeChatGPT{models: []string{"gpt-fixture", "gpt-fixture-mini"}}
	p := &scriptPrompter{answers: []string{"openai", "chatgpt", "gpt-fixture-mini"}}
	if err := ConfigureModel(p, pol, ModelOpts{ChatGPT: f.offer()}); err != nil {
		t.Fatal(err)
	}
	m := pol.Model
	if m.Provider != "openai" || m.Name != "gpt-fixture-mini" || m.APIKeyKind != policy.KeyKindChatGPTPlan ||
		m.APIKeyEnv != "" || m.BaseURL != "" || strings.Join(m.ProxyAllow, ",") != "api.openai.com:443" {
		t.Fatalf("model block %+v", m)
	}
	if len(f.calls) != 1 || !f.calls[0] {
		t.Fatalf("sign-in calls %v: want one fresh sign-in", f.calls)
	}
	if err := pol.Validate(); err != nil {
		t.Fatalf("the ChatGPT policy is invalid: %v", err)
	}
	said := strings.Join(p.said, "\n")
	for _, want := range []string{"ChatGPT plan pays", "hosted runs never use it", "api.openai.com:443 alone"} {
		if !strings.Contains(said, want) {
			t.Errorf("setup never said %q:\n%s", want, said)
		}
	}
}

func TestSetupReusesAKeptChatGPTSignIn(t *testing.T) {
	pol := policy.Default()
	f := &fakeChatGPT{kept: "person@example.com", models: []string{"gpt-fixture"}}
	p := &scriptPrompter{answers: []string{"openai", "chatgpt"}, confirm: true}
	if err := ConfigureModel(p, pol, ModelOpts{ChatGPT: f.offer()}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0] {
		t.Fatalf("sign-in calls %v: want the kept one used", f.calls)
	}
	if pol.Model.Name != "gpt-fixture" {
		t.Errorf("model %q: the plan's first", pol.Model.Name)
	}
}

func TestSetupWithAChatGPTKindAndYesAsksNothing(t *testing.T) {
	pol := policy.Default()
	f := &fakeChatGPT{kept: "person@example.com", models: []string{"gpt-fixture"}}
	p := &scriptPrompter{}
	if err := ConfigureModel(p, pol, ModelOpts{Provider: "openai", APIKeyKind: "chatgpt-plan", Yes: true, ChatGPT: f.offer()}); err != nil {
		t.Fatal(err)
	}
	if pol.Model.APIKeyKind != policy.KeyKindChatGPTPlan || pol.Model.Name != "gpt-fixture" || len(f.calls) != 1 || f.calls[0] {
		t.Fatalf("model %+v, calls %v", pol.Model, f.calls)
	}
}

func TestSetupStillTakesAnOpenAIKeyWhenChatGPTIsOffered(t *testing.T) {
	pol := policy.Default()
	pol.Model = policy.Model{Provider: "openai", Name: "gpt-fixture", APIKeyKind: policy.KeyKindChatGPTPlan, ProxyAllow: []string{"api.openai.com:443"}}
	f := &fakeChatGPT{}
	p := &scriptPrompter{answers: []string{"openai", "key"}}
	if err := ConfigureModel(p, pol, ModelOpts{ChatGPT: f.offer(), StoreKey: func(string, string) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Error("a key setup signed in")
	}
	m := pol.Model
	if m.APIKeyKind != "" || m.APIKeyEnv != "OPENAI_API_KEY" || m.Name != "gpt-5.2" {
		t.Fatalf("leaving the plan for a key kept the plan's setup: %+v", m)
	}
}

func TestSetupRefusesAChatGPTKindForAnotherProvider(t *testing.T) {
	err := ConfigureModel(&scriptPrompter{}, policy.Default(), ModelOpts{Provider: "anthropic", APIKeyKind: "chatgpt-plan", Yes: true})
	if err == nil || !strings.Contains(err.Error(), "needs provider openai") {
		t.Fatalf("err %v", err)
	}
}

func TestASignInSetupCouldNotFinishWritesNothing(t *testing.T) {
	pol := policy.Default()
	before := pol.Model
	f := &fakeChatGPT{err: errors.New("ChatGPT says this account cannot use its plan in other apps")}
	err := ConfigureModel(&scriptPrompter{answers: []string{"openai", "chatgpt"}}, pol, ModelOpts{ChatGPT: f.offer()})
	if err == nil || pol.Model.Provider != before.Provider || pol.Model.APIKeyKind != before.APIKeyKind {
		t.Fatalf("err %v, model %+v", err, pol.Model)
	}
}
