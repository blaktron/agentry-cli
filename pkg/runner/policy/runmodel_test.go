package policy

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// labPolicy is the default policy with a second provider, an endpoint of
// the person's own, and a saved model on it.
func labPolicy(t *testing.T) (*Policy, SavedModel) {
	t.Helper()
	p := Default()
	if err := p.AddProvider("custom-lab", customProvider("http://lab.example:8000/v1")); err != nil {
		t.Fatal(err)
	}
	lab, err := p.AddModel(SavedModel{Name: "Lab", Provider: "custom-lab", Model: "qwen3", ContextWindow: 32768})
	if err != nil {
		t.Fatal(err)
	}
	return p, lab
}

func TestARunIsOnTheSavedModelItNamesOrTheDefault(t *testing.T) {
	p, lab := labPolicy(t)
	for _, ref := range []string{"Lab", "lab", lab.ID} {
		sm, err := p.RunModel(ref, "")
		if err != nil || sm.ID != lab.ID {
			t.Errorf("RunModel(%q) = %+v, %v", ref, sm, err)
		}
	}
	sm, err := p.RunModel("", "")
	if err != nil || sm.ID != p.DefaultModel().ID {
		t.Fatalf("a run naming none is on %+v, %v; want the default", sm, err)
	}
}

// Nothing falls back to the default: a run naming a saved model this
// computer does not have is refused, by the name it gave.
func TestARunNamingAMissingSavedModelIsRefusedNeverRunOnTheDefault(t *testing.T) {
	p, _ := labPolicy(t)
	sm, err := p.RunModel("Gemma", "")
	var missing *MissingModelError
	if !errors.As(err, &missing) || missing.Ref != "Gemma" || sm.ID != "" {
		t.Fatalf("RunModel(Gemma) = %+v, %v", sm, err)
	}
	if !strings.Contains(err.Error(), `"Gemma"`) || !strings.Contains(err.Error(), "never takes the default") {
		t.Fatalf("the refusal says %q", err)
	}
}

func TestARunsViewIsItsSavedModelsProviderAlone(t *testing.T) {
	p, lab := labPolicy(t)
	view, err := p.ForRun(lab)
	if err != nil {
		t.Fatal(err)
	}
	want := Model{Provider: ProviderCustom, ProviderID: "custom-lab", Name: "qwen3", APIKeyEnv: "HOME_LAB_API_KEY",
		BaseURL: "http://lab.example:8000/v1", ContextWindow: 32768, ProxyAllow: []string{"lab.example:8000"}}
	if !reflect.DeepEqual(view.Model, want) {
		t.Fatalf("the view's model = %+v\nwant %+v", view.Model, want)
	}
	if got, ok := view.RunSavedModel(); !ok || got.ID != lab.ID {
		t.Fatalf("RunSavedModel = %+v, %v", got, ok)
	}
	// The policy as read is unchanged, and the view is never written.
	if p.Model.Provider != ProviderAnthropic {
		t.Fatalf("ForRun changed the policy: %+v", p.Model)
	}
	if err := Save(filepath.Join(t.TempDir(), "policy.yaml"), view); err == nil {
		t.Fatal("a run's view of the policy was saved")
	}
	if err := view.Validate(); err != nil {
		t.Fatalf("the view does not validate: %v", err)
	}
}

// A version 1 policy's harness block that names a model is a saved model
// that runs on that harness, with the block's key, as a run with --adapter
// did before the lists existed.
func TestAHarnessBlocksSavedModelRunsAsTheBlockDidBefore(t *testing.T) {
	p := Default()
	p.Model.ContextWindow = 200000
	p.Harness = map[string]HarnessPolicy{
		"claude": {Model: "sonnet", APIKeyEnv: "CLAUDE_CODE_OAUTH_TOKEN", APIKeyKind: KeyKindSubscription, Path: "/opt/claude"},
		"codex":  {Path: "/opt/codex"},
	}
	claude, ok := p.HarnessModel("claude")
	if !ok || claude.Model != "sonnet" || claude.ContextWindow != 200000 {
		t.Fatalf("the claude block's saved model = %+v, %v", claude, ok)
	}
	if _, ok := p.HarnessModel("codex"); ok {
		t.Fatal("a block that names no model is a saved model")
	}
	if p.RunHarness(claude) != "claude" || p.RunHarness(p.DefaultModel()) != "" {
		t.Fatalf("RunHarness: claude's %q, the default's %q", p.RunHarness(claude), p.RunHarness(p.DefaultModel()))
	}
	// A run on the claude harness that names no model is on the block's
	// saved model, as it was.
	if sm, err := p.RunModel("", "claude"); err != nil || sm.ID != claude.ID {
		t.Fatalf("a run on --adapter claude is on %+v, %v", sm, err)
	}
	view, err := p.ForRun(claude)
	if err != nil {
		t.Fatal(err)
	}
	if hp := view.Harness["claude"]; !reflect.DeepEqual(hp, p.Harness["claude"]) {
		t.Fatalf("the block as a run on its own model reads it: %+v", hp)
	}
	if view.Model.Name != "sonnet" || view.Model.ContextWindow != 200000 {
		t.Fatalf("the view's model = %+v", view.Model)
	}
	// The default on the claude harness keeps the block's key and path but
	// is asked for its own model.
	def, err := p.ForRun(p.DefaultModel())
	if err != nil {
		t.Fatal(err)
	}
	if hp := def.Harness["claude"]; hp.Model != "" || hp.APIKeyEnv != "CLAUDE_CODE_OAUTH_TOKEN" || hp.Path != "/opt/claude" {
		t.Fatalf("the block as a run on the default reads it: %+v", hp)
	}
}

// A harness block's model and key are the provider's it was written for: a
// run on another provider's saved model takes the block's path alone.
func TestAHarnessBlockAppliesToItsOwnProviderAlone(t *testing.T) {
	p, lab := labPolicy(t)
	p.Harness = map[string]HarnessPolicy{"codex": {Model: "gpt-5", APIKeyEnv: "OPENAI_TEAM_KEY", BaseURL: "https://proxy.example/v1", Path: "/opt/codex", SignInTokenRoute: TokenRouteRestart}}
	view, err := p.ForRun(lab)
	if err != nil {
		t.Fatal(err)
	}
	if hp := view.Harness["codex"]; !reflect.DeepEqual(hp, HarnessPolicy{Path: "/opt/codex", SignInTokenRoute: TokenRouteRestart}) {
		t.Fatalf("a run on %s reads the codex block as %+v", lab.Name, hp)
	}
	if p.RunHarness(lab) != "" {
		t.Fatalf("a saved model of the person's own runs on %q", p.RunHarness(lab))
	}
}

// Making a saved model on another vendor the default leaves the harness
// blocks valid: their kinds belong to the provider they were written for.
func TestTheDefaultMayMoveOffTheProviderTheHarnessBlocksAreFor(t *testing.T) {
	p, lab := labPolicy(t)
	p.Harness = map[string]HarnessPolicy{"claude": {APIKeyKind: KeyKindClaudeSignIn}}
	if _, err := p.SetDefault(lab.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate after the default moved: %v", err)
	}
}
