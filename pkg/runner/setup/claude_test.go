package setup

import (
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// fakeClaude is Claude Code's sign-in as setup reaches it.
type fakeClaude struct {
	kept    string
	signins int
}

func (f *fakeClaude) offer() ClaudeOffer {
	return ClaudeOffer{
		Kept:   func() string { return f.kept },
		SignIn: func() (string, error) { f.signins++; return "person@example.com (Example)", nil },
	}
}

// askingPrompter records the questions asked.
type askingPrompter struct {
	scriptPrompter
	asked []string
}

func (a *askingPrompter) Ask(q, def string) (string, error) {
	a.asked = append(a.asked, q)
	return a.scriptPrompter.Ask(q, def)
}

// Choosing the Claude subscription starts Claude Code's own sign-in and
// writes the policy's model block: no key variable, no base URL, Anthropic's
// API and the host Claude Code refreshes at the only egress. The
// choice is worded with ClaudeSignInLabel, and the key choice names the Max and
// Team plans' monthly API credits.
func TestSetupChoosesTheClaudeSubscriptionThroughClaudeCode(t *testing.T) {
	pol := policy.Default()
	f := &fakeClaude{}
	p := &askingPrompter{scriptPrompter: scriptPrompter{answers: []string{"anthropic", "claude", "sonnet"}}}
	if err := ConfigureModel(p, pol, ModelOpts{Claude: f.offer()}); err != nil {
		t.Fatal(err)
	}
	m := pol.Model
	if m.Provider != policy.ProviderAnthropic || m.Name != "sonnet" || m.APIKeyKind != policy.KeyKindClaudeSignIn ||
		m.APIKeyEnv != "" || m.BaseURL != "" || !slices.Equal(m.ProxyAllow, []string{"api.anthropic.com:443", "platform.claude.com:443"}) {
		t.Fatalf("model block %+v", m)
	}
	if f.signins != 1 {
		t.Fatalf("sign-ins %d", f.signins)
	}
	if err := pol.Validate(); err != nil {
		t.Fatalf("the Claude policy is invalid: %v", err)
	}
	question := ""
	for _, q := range p.asked {
		if strings.HasPrefix(q, "Pay for Anthropic models with") {
			question = q
		}
	}
	for _, want := range []string{"claude = Use your Claude subscription (runs Claude Code)", "Max and Team plans include monthly API credits"} {
		if !strings.Contains(question, want) {
			t.Errorf("the choice %q does not say %q", question, want)
		}
	}
	if strings.Contains(question, "Sign in with Claude") {
		t.Error("the choice says Sign in with Claude")
	}
	said := strings.Join(p.said, "\n")
	for _, want := range []string{"Claude Code signs you in itself", "hosted runs never use it", "signed in as person@example.com (Example)", "api.anthropic.com:443, platform.claude.com:443 alone"} {
		if !strings.Contains(said, want) {
			t.Errorf("setup never said %q:\n%s", want, said)
		}
	}
}

// A kept sign-in is used without signing in again; under --yes with the
// kind named nothing is asked.
func TestSetupReusesAKeptClaudeSignIn(t *testing.T) {
	pol := policy.Default()
	f := &fakeClaude{kept: "person@example.com"}
	p := &scriptPrompter{answers: []string{"anthropic", "claude"}, confirm: true}
	if err := ConfigureModel(p, pol, ModelOpts{Claude: f.offer()}); err != nil {
		t.Fatal(err)
	}
	if f.signins != 0 || pol.Model.APIKeyKind != policy.KeyKindClaudeSignIn || pol.Model.Name != DefaultsFor("anthropic").Model {
		t.Fatalf("sign-ins %d, model %+v", f.signins, pol.Model)
	}
	pol = policy.Default()
	q := &scriptPrompter{}
	if err := ConfigureModel(q, pol, ModelOpts{Provider: "anthropic", APIKeyKind: policy.KeyKindClaudeSignIn, Yes: true, Claude: f.offer()}); err != nil {
		t.Fatal(err)
	}
	if f.signins != 0 || pol.Model.APIKeyKind != policy.KeyKindClaudeSignIn {
		t.Fatalf("--yes: sign-ins %d, model %+v", f.signins, pol.Model)
	}
	if err := ConfigureModel(q, policy.Default(), ModelOpts{Provider: "openai", APIKeyKind: policy.KeyKindClaudeSignIn, Yes: true, Claude: f.offer()}); err == nil {
		t.Fatal("claude-signin beside openai")
	}
}

// The key path stays: choosing key signs nothing in and leaves none of the
// subscription's setup behind.
func TestSetupStillTakesAnAnthropicKeyWhenClaudeIsOffered(t *testing.T) {
	pol := policy.Default()
	pol.Model = policy.Model{Provider: "anthropic", Name: "sonnet", APIKeyKind: policy.KeyKindClaudeSignIn, ProxyAllow: policy.SignInProxyAllow(policy.KeyKindClaudeSignIn)}
	f := &fakeClaude{}
	p := &scriptPrompter{answers: []string{"anthropic", "key"}}
	if err := ConfigureModel(p, pol, ModelOpts{Claude: f.offer(), StoreKey: func(string, string) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	if f.signins != 0 {
		t.Error("a key setup signed in")
	}
	m := pol.Model
	if m.APIKeyKind != "" || m.APIKeyEnv != "ANTHROPIC_API_KEY" || m.Name == "sonnet" {
		t.Fatalf("leaving the subscription for a key kept its setup: %+v", m)
	}
}
