package localrun

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry/pkg/managedsettings"
	"github.com/blaktron/agentry/pkg/manifest"
)

// twoModels is the default policy with a second saved model on a ChatGPT
// plan, whose harness is Codex in its image.
func twoModels(t *testing.T) (*policy.Policy, policy.SavedModel) {
	t.Helper()
	pol := policy.Default()
	pol.Images.Runner = "agentry-runner:test"
	if err := pol.AddProvider("openai", policy.Provider{APIKeyKind: policy.KeyKindChatGPTPlan, ProxyAllow: []string{"api.openai.com:443"}}); err != nil {
		t.Fatal(err)
	}
	plan, err := pol.AddModel(policy.SavedModel{Name: "Plan", Provider: "openai", Model: "gpt-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	return pol, plan
}

func TestTheHarnessFollowsTheSavedModelsCredential(t *testing.T) {
	pol, plan := twoModels(t)
	rm, err := ChooseRunModel(pol, "Plan", "")
	if err != nil {
		t.Fatal(err)
	}
	if rm.Saved.ID != plan.ID || rm.Adapter != adapter.CodexImageAdapterName || rm.Policy.Model.Name != "gpt-5.5" {
		t.Fatalf("a run on the ChatGPT plan's model = %+v on %s", rm.Saved, rm.Adapter)
	}
	rm, err = ChooseRunModel(pol, "", "")
	if err != nil || rm.Adapter != adapter.DefaultName || rm.Saved.ID != pol.DefaultModel().ID {
		t.Fatalf("a run on the default = %+v on %s, %v", rm.Saved, rm.Adapter, err)
	}
	// --adapter still overrides the harness.
	rm, err = ChooseRunModel(pol, "Plan", adapter.CodexAdapterName)
	if err != nil || rm.Adapter != adapter.CodexAdapterName || rm.Saved.ID != plan.ID {
		t.Fatalf("--adapter codex: %+v on %s, %v", rm.Saved, rm.Adapter, err)
	}
}

func TestAMissingSavedModelIsAPolicyRefusal(t *testing.T) {
	pol, _ := twoModels(t)
	_, err := ChooseRunModel(pol, "Gone", "")
	var ce *engine.ClassifiedError
	if !errors.As(err, &ce) || ce.Code != engine.ExitPolicy || !IsMissingModel(err) || !strings.Contains(err.Error(), `"Gone"`) {
		t.Fatalf("a run on a missing saved model: %v", err)
	}
}

// A harness block's saved model runs on that block's harness, and a run on
// any other harness follows the credential.
func TestAHarnessBlocksSavedModelRunsOnItsHarness(t *testing.T) {
	pol := policy.Default()
	pol.Harness = map[string]policy.HarnessPolicy{adapter.ClaudeAdapterName: {Model: "sonnet"}}
	rm, err := ChooseRunModel(pol, "claude", "")
	if err != nil || rm.Adapter != adapter.ClaudeAdapterName || rm.Policy.Model.Name != "sonnet" {
		t.Fatalf("--model claude: %+v on %s, %v", rm.Saved, rm.Adapter, err)
	}
	rm, err = ChooseRunModel(pol, "", adapter.ClaudeAdapterName)
	if err != nil || rm.Saved.Name != "claude" {
		t.Fatalf("--adapter claude: %+v, %v", rm.Saved, err)
	}
	rm, err = ChooseRunModel(pol, "", "")
	if err != nil || rm.Adapter != adapter.DefaultName {
		t.Fatalf("no flags: %+v on %s, %v", rm.Saved, rm.Adapter, err)
	}
	// A harness with no block of its own is on the default.
	rm, err = ChooseRunModel(pol, "", adapter.CodexAdapterName)
	if err != nil || rm.Saved.ID != pol.DefaultModel().ID || rm.Adapter != adapter.CodexAdapterName {
		t.Fatalf("--adapter codex: %+v on %s, %v", rm.Saved, rm.Adapter, err)
	}
}

// A plan compiled on one saved model is never reused for a run on another.
func TestAPlanIsReusedOnlyForTheSavedModelItWasCompiledOn(t *testing.T) {
	home := t.TempDir()
	meta, dir, err := KnowledgeCreator().Materialize(home, "b1")
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(dir, "workspace")
	m, err := ManifestFrom(ws)
	if err != nil {
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.Images.Runner = "agentry-runner:test"
	if err := pol.AddProvider("custom-lab", policy.Provider{APIKeyEnv: "LAB_API_KEY", BaseURL: "https://lab.example/v1", ProxyAllow: []string{"lab.example:443"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pol.AddModel(policy.SavedModel{Name: "Lab", Provider: "custom-lab", Model: "qwen3"}); err != nil {
		t.Fatal(err)
	}
	in := func(ref string) PlanInputs {
		rm, err := ChooseRunModel(pol, ref, "")
		if err != nil {
			t.Fatal(err)
		}
		return PlanInputs{Home: home, Manifest: m, Meta: meta, InstallDir: dir, WorkspaceDir: ws, Policy: rm.Policy,
			PolicySHA: "sha", Adapter: rm.Adapter, Mode: manifest.ModeAutonomous, CompiledBy: "b1"}
	}
	first, _, err := EnsurePlan(in(""))
	if err != nil {
		t.Fatal(err)
	}
	lab, _, err := EnsurePlan(in("Lab"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Harness.SavedModel == nil || lab.Harness.SavedModel == nil || lab.Harness.SavedModel.Name != "Lab" {
		t.Fatalf("the plans name %+v and %+v", first.Harness.SavedModel, lab.Harness.SavedModel)
	}
	if strings.Join(lab.Proxy.Allow, ",") != "lab.example:443" {
		t.Fatalf("the lab's plan reaches %v", lab.Proxy.Allow)
	}
}

// The saved model that runs a provider's model is found by the provider's
// name and the model id, the default first when two are.
func TestSavedModelForFindsTheModelByProviderAndID(t *testing.T) {
	pol, plan := twoModels(t)
	got, ok := SavedModelFor(pol, "openai", "gpt-5.5")
	if !ok || got.ID != plan.ID {
		t.Fatalf("openai's gpt-5.5 = %+v, %v", got, ok)
	}
	def := pol.DefaultModel()
	provider, model, err := RunsOn(pol, def)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pol.AddModel(policy.SavedModel{Name: "Again", Provider: def.Provider, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := SavedModelFor(pol, provider, model); !ok || got.ID != def.ID || got.ID == second.ID {
		t.Fatalf("two saved models of one model: %+v, %v, want the default %s", got, ok, def.ID)
	}
	if _, ok := SavedModelFor(pol, "openai", "gpt-4"); ok {
		t.Fatal("a model no saved model runs was found")
	}
}

// assignedPresetModel holds one document assigning acme/demo with a preset
// model, locked or not.
func assignedPresetModel(t *testing.T, provider, model string, locked bool) managed.Effective {
	t.Helper()
	raw, err := json.Marshal([]managedsettings.Assignment{{Ref: "acme/demo", Version: "latest", Mode: managedsettings.AssignOffered,
		Preset: &managedsettings.Preset{Model: &managedsettings.Model{Provider: provider, Model: model}}, PresetLocked: locked}})
	if err != nil {
		t.Fatal(err)
	}
	d := managedsettings.Document{Org: "acme", Settings: map[string]managedsettings.Setting{managedsettings.QuickAgents: {Value: raw, Locked: true}}}
	return managed.Effective{Documents: []managed.State{{Registry: "https://reg.example", Document: d}}}
}

// A preset model is the run's saved model when none is named; a locked one
// refuses another by a conflict, and one with no saved model by a missing
// model, each typed so a client can say it in its own words.
func TestPresetModelChoosesConflictsAndMisses(t *testing.T) {
	pol, plan := twoModels(t)
	got, warn, err := PresetModel(pol, assignedPresetModel(t, "openai", "gpt-5.5", false), "acme/demo@1.0.0", "", "")
	if err != nil || warn != "" || got != plan.ID {
		t.Fatalf("a default preset model = %q, %q, %v", got, warn, err)
	}
	def := pol.DefaultModel().Name
	if got, _, err := PresetModel(pol, assignedPresetModel(t, "openai", "gpt-5.5", false), "acme/demo", def, ""); err != nil || got != def {
		t.Fatalf("a named model under a default preset = %q, %v", got, err)
	}
	_, _, err = PresetModel(pol, assignedPresetModel(t, "openai", "gpt-5.5", true), "acme/demo", def, "")
	var conflict *PresetModelConflict
	if !errors.As(err, &conflict) || conflict.Organization != "acme" || conflict.Want.Model != "gpt-5.5" {
		t.Fatalf("another model under a locked preset: %v", err)
	}
	got, warn, err = PresetModel(pol, assignedPresetModel(t, "openai", "gpt-4", false), "acme/demo", "", "")
	if err != nil || got != "" || !strings.Contains(warn, "suggest openai's gpt-4") {
		t.Fatalf("a default preset model with no saved model = %q, %q, %v", got, warn, err)
	}
	_, _, err = PresetModel(pol, assignedPresetModel(t, "openai", "gpt-4", true), "acme/demo", "", "")
	var missing *PresetModelMissing
	var ce *engine.ClassifiedError
	if !errors.As(err, &missing) || !errors.As(err, &ce) || ce.Code != engine.ExitPolicy {
		t.Fatalf("a locked preset model with no saved model: %v", err)
	}
	if got, _, err := PresetModel(pol, assignedPresetModel(t, "openai", "gpt-5.5", true), "acme/other", "", ""); err != nil || got != "" {
		t.Fatalf("a release with no assignment = %q, %v", got, err)
	}
}
