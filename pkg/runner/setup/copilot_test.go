package setup

import (
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

type fakeCopilot struct {
	kept    string
	signins int
}

func (f *fakeCopilot) offer() CopilotOffer {
	return CopilotOffer{
		Kept:   func() string { return f.kept },
		SignIn: func() (string, error) { f.signins++; return "octo-fixture", nil },
	}
}

// Choosing Copilot signs in with GitHub and writes the policy's model
// block: no key variable, no base URL, GitHub's API and the plan's Copilot
// host the only egress (I2); auto is the default model.
func TestSetupChoosesTheCopilotPlanThroughASignInWithGitHub(t *testing.T) {
	pol := policy.Default()
	f := &fakeCopilot{}
	p := &askingPrompter{scriptPrompter: scriptPrompter{answers: []string{"copilot", "", "business"}}}
	if err := ConfigureModel(p, pol, ModelOpts{Copilot: f.offer()}); err != nil {
		t.Fatal(err)
	}
	m := pol.Model
	if m.Provider != policy.ProviderCopilot || m.Name != "auto" || m.APIKeyKind != policy.KeyKindCopilotSignIn ||
		m.APIKeyEnv != "" || m.BaseURL != "" || !slices.Equal(m.ProxyAllow, []string{"api.github.com:443", "api.business.githubcopilot.com:443"}) {
		t.Fatalf("model block %+v", m)
	}
	if f.signins != 1 {
		t.Fatalf("sign-ins %d", f.signins)
	}
	if err := pol.Validate(); err != nil {
		t.Fatalf("the Copilot policy is invalid: %v", err)
	}
	for _, q := range p.asked {
		if strings.Contains(q, "api key") || strings.Contains(q, "base URL") {
			t.Fatalf("asked %q", q)
		}
	}
}

// A kept sign-in is offered and kept, the policy's plan and model are the
// defaults the second time, and --yes asks nothing; a key kind beside
// provider copilot, or copilot-signin beside another provider, is refused.
func TestSetupKeepsACopilotSignInAndRefusesWhatCannotPay(t *testing.T) {
	pol := policy.Default()
	f := &fakeCopilot{kept: "octo-fixture"}
	if err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Provider: "copilot", Model: "claude-sonnet-5.5", CopilotPlan: "enterprise", Copilot: f.offer(), Yes: true}); err != nil {
		t.Fatal(err)
	}
	if f.signins != 0 || policy.CopilotPlanOf(pol.Model.ProxyAllow) != policy.CopilotPlanEnterprise {
		t.Fatalf("signins %d, allow %v", f.signins, pol.Model.ProxyAllow)
	}
	if err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Provider: "copilot", Copilot: f.offer(), Yes: true}); err != nil {
		t.Fatal(err)
	}
	if pol.Model.Name != "claude-sonnet-5.5" || policy.CopilotPlanOf(pol.Model.ProxyAllow) != policy.CopilotPlanEnterprise {
		t.Fatalf("not kept: %+v", pol.Model)
	}
	if err := ConfigureModel(&scriptPrompter{}, policy.Default(), ModelOpts{Provider: "copilot", APIKeyKind: "api", Copilot: f.offer(), Yes: true}); err == nil {
		t.Fatal("a key for copilot")
	}
	if err := ConfigureModel(&scriptPrompter{}, policy.Default(), ModelOpts{Provider: "openai", APIKeyKind: policy.KeyKindCopilotSignIn, Yes: true}); err == nil {
		t.Fatal("copilot-signin for openai")
	}
	fresh := &fakeCopilot{}
	if err := ConfigureModel(&scriptPrompter{}, policy.Default(), ModelOpts{Provider: "copilot", CopilotPlan: "team", Copilot: fresh.offer(), Yes: true}); err == nil || fresh.signins != 0 {
		t.Fatalf("an unknown plan: %v, sign-ins %d", err, fresh.signins)
	}
}
