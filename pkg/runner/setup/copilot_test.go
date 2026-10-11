package setup

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

type fakeCopilot struct {
	kept    string
	signins int
	// models is what the list answers, listErr its failure; listed the
	// plans it was asked for.
	models  []string
	listErr error
	listed  []string
}

func (f *fakeCopilot) offer() CopilotOffer {
	return CopilotOffer{
		Kept:   func() string { return f.kept },
		SignIn: func() (string, error) { f.signins++; return "octo-fixture", nil },
		Models: func(plan string) ([]string, error) {
			f.listed = append(f.listed, plan)
			return f.models, f.listErr
		},
	}
}

// Choosing Copilot signs in with GitHub and writes the policy's model
// block: no key variable, no base URL, GitHub's API and the plan's Copilot
// host the only egress (I2); the plan is asked before the model, and auto is
// the default model.
func TestSetupChoosesTheCopilotPlanThroughASignInWithGitHub(t *testing.T) {
	pol := policy.Default()
	f := &fakeCopilot{}
	p := &askingPrompter{scriptPrompter: scriptPrompter{answers: []string{"copilot", "business", ""}}}
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

// Setup offers the models Copilot CLI lists for the plan, auto first, and
// takes none it does not offer (a model the plan disables is not listed);
// a list that fails is said and the model typed instead; --yes and a model
// named on the command line list nothing (agentry-cli#808).
func TestSetupOffersTheModelsTheCopilotPlanLists(t *testing.T) {
	pol := policy.Default()
	f := &fakeCopilot{models: []string{"claude-sonnet-5.5", "gpt-5-mini", "claude-sonnet-5.5"}}
	p := &askingPrompter{scriptPrompter: scriptPrompter{answers: []string{"copilot", "business", "claude-opus-5.5", "gpt-5-mini"}}}
	if err := ConfigureModel(p, pol, ModelOpts{Copilot: f.offer()}); err != nil {
		t.Fatal(err)
	}
	if pol.Model.Name != "gpt-5-mini" || !slices.Equal(f.listed, []string{"business"}) {
		t.Fatalf("model %q, listed for %v", pol.Model.Name, f.listed)
	}
	if q := p.asked[len(p.asked)-1]; !strings.Contains(q, "your plan offers auto, claude-sonnet-5.5, gpt-5-mini;") {
		t.Fatalf("asked %q", q)
	}
	if !slices.ContainsFunc(p.said, func(s string) bool { return strings.Contains(s, `does not offer "claude-opus-5.5"`) }) {
		t.Fatalf("said %v", p.said)
	}

	// The kept model is the default while the plan still offers it, and
	// auto once it does not.
	f = &fakeCopilot{kept: "octo-fixture", models: []string{"gpt-5-mini"}}
	p = &askingPrompter{scriptPrompter: scriptPrompter{confirm: true, answers: []string{"copilot", "", ""}}}
	if err := ConfigureModel(p, pol, ModelOpts{Copilot: f.offer()}); err != nil || pol.Model.Name != "gpt-5-mini" {
		t.Fatalf("kept: %v, %q", err, pol.Model.Name)
	}
	f.models = []string{"claude-sonnet-5.5"}
	p = &askingPrompter{scriptPrompter: scriptPrompter{confirm: true, answers: []string{"copilot", "", ""}}}
	if err := ConfigureModel(p, pol, ModelOpts{Copilot: f.offer()}); err != nil || pol.Model.Name != "auto" {
		t.Fatalf("no longer offered: %v, %q", err, pol.Model.Name)
	}

	f = &fakeCopilot{listErr: errors.New("docker is not running")}
	p = &askingPrompter{scriptPrompter: scriptPrompter{answers: []string{"copilot", "", "claude-sonnet-5.5"}}}
	if err := ConfigureModel(p, policy.Default(), ModelOpts{Copilot: f.offer()}); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(p.said, func(s string) bool {
		return strings.Contains(s, "could not list your plan's models (docker is not running)")
	}) {
		t.Fatalf("said %v", p.said)
	}

	f = &fakeCopilot{models: []string{"gpt-5-mini"}}
	if err := ConfigureModel(&scriptPrompter{}, policy.Default(), ModelOpts{Provider: "copilot", Copilot: f.offer(), Yes: true}); err != nil || len(f.listed) != 0 {
		t.Fatalf("--yes listed: %v, %v", err, f.listed)
	}
	if err := ConfigureModel(&scriptPrompter{answers: []string{"business"}}, policy.Default(), ModelOpts{Provider: "copilot", Model: "gpt-5-mini", Copilot: f.offer()}); err != nil || len(f.listed) != 0 {
		t.Fatalf("--model listed: %v, %v", err, f.listed)
	}
}
