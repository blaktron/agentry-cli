package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// loadDefault writes the default policy file and reads it back.
func loadDefault(t *testing.T) (*Policy, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	p, _, err := EnsureDefault(path)
	if err != nil {
		t.Fatal(err)
	}
	return p, path
}

// saveAndRead saves p and returns the file's text and the policy read back.
func saveAndRead(t *testing.T, p *Policy, path string) (string, *Policy) {
	t.Helper()
	if err := Save(path, p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	back, _, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save: %v\n%s", err, b)
	}
	return string(b), back
}

func customProvider(url string) Provider {
	return Provider{Name: "Home lab", APIKeyEnv: "HOME_LAB_API_KEY", BaseURL: url, ProxyAllow: []string{strings.TrimPrefix(strings.TrimSuffix(url, "/v1"), "http://")}}
}

func TestAVersion1PolicyReadsAsOneProviderAndTheDefaultSavedModel(t *testing.T) {
	p, _ := loadDefault(t)
	if p.HasSavedModels() {
		t.Fatal("a version 1 policy read holds lists in memory")
	}
	providers, models, def := p.SavedModels()
	if len(providers) != 1 || len(models) != 1 {
		t.Fatalf("providers %v, models %v: want one each", providers, models)
	}
	pr, ok := providers["anthropic"]
	if !ok || pr.APIKeyEnv != "ANTHROPIC_API_KEY" || !reflect.DeepEqual(pr.ProxyAllow, []string{"api.anthropic.com:443"}) {
		t.Fatalf("provider = %+v", providers)
	}
	m := models[0]
	if m.ID != def || m.Name != "anthropic" || m.Provider != "anthropic" || m.Model != DefaultModelName {
		t.Fatalf("saved model = %+v (default %q)", m, def)
	}
	if got := p.DefaultModel(); got != m {
		t.Fatalf("DefaultModel = %+v", got)
	}
	// The same file read twice gives the same id, so what names it keeps
	// naming it.
	q, _ := loadDefault(t)
	if q.DefaultModel().ID != m.ID {
		t.Fatal("the migrated default's id changed between reads")
	}
}

func TestEachHarnessBlockThatNamesAModelIsASavedModelOfItsOwn(t *testing.T) {
	p := Default()
	p.Harness = map[string]HarnessPolicy{
		"claude":      {Model: "claude-opus-5-5", APIKeyKind: KeyKindSubscription},
		"codex":       {Path: "/opt/codex"},
		"claude-plus": {Model: "claude-haiku-5-5"},
	}
	_, models, def := p.SavedModels()
	if len(models) != 3 || models[0].ID != def {
		t.Fatalf("models = %+v", models)
	}
	if models[1].Name != "claude" || models[1].Model != "claude-opus-5-5" || models[1].Provider != "anthropic" {
		t.Fatalf("claude's saved model = %+v", models[1])
	}
	if models[2].Name != "claude-plus" || models[2].Model != "claude-haiku-5-5" {
		t.Fatalf("claude-plus's saved model = %+v", models[2])
	}
	// Nothing changed, so the file stays version 1 with its blocks.
	path := filepath.Join(t.TempDir(), "policy.yaml")
	text, back := saveAndRead(t, p, path)
	if !strings.Contains(text, `policyVersion: "1"`) || strings.Contains(text, "\nmodels:") {
		t.Fatalf("written:\n%s", text)
	}
	if !reflect.DeepEqual(back.Harness, p.Harness) {
		t.Fatalf("harness blocks after a save: %+v", back.Harness)
	}
}

func TestAVersion1PolicyStaysVersion1WithOneProviderAndOneUnnamedModel(t *testing.T) {
	p, path := loadDefault(t)
	// A change through the lists that version 1 can say: the model.
	def := p.DefaultModel()
	def.Model = "claude-opus-5-5"
	if _, err := p.EditModel(def.ID, def); err != nil {
		t.Fatal(err)
	}
	text, back := saveAndRead(t, p, path)
	if !strings.Contains(text, `policyVersion: "1"`) {
		t.Fatalf("written:\n%s", text)
	}
	for _, key := range []string{"\nproviders:", "\nmodels:", "\ndefault:"} {
		if strings.Contains(text, key) {
			t.Fatalf("a version 1 file holds %q:\n%s", key, text)
		}
	}
	if back.Model.Name != "claude-opus-5-5" || back.DefaultModel().ID != def.ID {
		t.Fatalf("read back: model %+v, default %+v", back.Model, back.DefaultModel())
	}
}

func TestASecondSavedModelWritesVersion2AndRemovingItWritesVersion1(t *testing.T) {
	p, path := loadDefault(t)
	second, err := p.AddModel(SavedModel{Name: "Opus for reports", Provider: "anthropic", Model: "claude-opus-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == "" || second.ID == p.Default {
		t.Fatalf("second = %+v, default %q", second, p.Default)
	}
	text, back := saveAndRead(t, p, path)
	if !strings.Contains(text, `policyVersion: "2"`) {
		t.Fatalf("written:\n%s", text)
	}
	if strings.Contains(text, "\nmodel:") {
		t.Fatalf("a version 2 file holds the model block:\n%s", text)
	}
	// The default is still the first, and Model reads as it.
	if back.Default == second.ID || back.Model.Name != DefaultModelName || back.Model.Provider != "anthropic" || back.Model.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Fatalf("read back: default %q, model %+v", back.Default, back.Model)
	}
	if got, ok := back.FindModel("opus FOR reports"); !ok || got != second {
		t.Fatalf("FindModel by name = %+v, %t", got, ok)
	}
	if got, ok := back.FindModel(second.ID); !ok || got != second {
		t.Fatalf("FindModel by id = %+v, %t", got, ok)
	}
	if _, err := back.RemoveModel(second.Name, ""); err != nil {
		t.Fatal(err)
	}
	text, _ = saveAndRead(t, back, path)
	if !strings.Contains(text, `policyVersion: "1"`) || strings.Contains(text, "\nmodels:") {
		t.Fatalf("one provider and one unnamed model again, written:\n%s", text)
	}
}

func TestACustomProviderOfItsOwnNameWritesVersion2(t *testing.T) {
	p, path := loadDefault(t)
	if err := p.AddProvider("custom-home-lab", customProvider("http://127.0.0.1:11434/v1")); err != nil {
		t.Fatal(err)
	}
	text, back := saveAndRead(t, p, path)
	if !strings.Contains(text, `policyVersion: "2"`) || !strings.Contains(text, "custom-home-lab:") {
		t.Fatalf("written:\n%s", text)
	}
	if back.Providers["custom-home-lab"].Name != "Home lab" {
		t.Fatalf("providers read back: %+v", back.Providers)
	}
}

func TestASavedModelsOwnNameWritesVersion2(t *testing.T) {
	p, path := loadDefault(t)
	def := p.DefaultModel()
	def.Name = "Sonnet"
	if _, err := p.EditModel("anthropic", def); err != nil {
		t.Fatal(err)
	}
	text, back := saveAndRead(t, p, path)
	if !strings.Contains(text, `policyVersion: "2"`) || back.DefaultModel().Name != "Sonnet" {
		t.Fatalf("written:\n%s", text)
	}
}

func TestSeveralCustomProvidersEachHaveTheirOwnKeychainEntry(t *testing.T) {
	p, path := loadDefault(t)
	for _, c := range []struct{ id, url, name string }{
		{"custom-lab-one", "http://127.0.0.1:8001/v1", "Lab one"},
		{"custom-lab-two", "http://127.0.0.1:8002/v1", "Lab two"},
	} {
		pr := customProvider(c.url)
		pr.Name = c.name
		pr.APIKeyEnv = strings.ToUpper(strings.ReplaceAll(c.id, "-", "_")) + "_KEY"
		pr.APIKeySource = "keyring"
		if err := p.AddProvider(c.id, pr); err != nil {
			t.Fatal(err)
		}
		if _, err := p.AddModel(SavedModel{Name: c.name + " qwen", Provider: c.id, Model: "qwen3"}); err != nil {
			t.Fatal(err)
		}
	}
	_, back := saveAndRead(t, p, path)
	for _, want := range []struct{ name, account string }{{"Lab one qwen", "custom-lab-one"}, {"Lab two qwen", "custom-lab-two"}} {
		sm, _ := back.FindModel(want.name)
		view, err := back.ModelView(sm)
		if err != nil {
			t.Fatal(err)
		}
		if view.Provider != ProviderCustom || view.KeyProvider() != want.account || !view.OwnEndpoint() {
			t.Fatalf("%s reads as %+v (keychain %s)", want.name, view, view.KeyProvider())
		}
	}
	if back.Model.KeyProvider() != "anthropic" {
		t.Fatalf("the default's keychain entry is provider:%s", back.Model.KeyProvider())
	}
}

func TestASecondModelOnAProviderUsesTheProvidersCredential(t *testing.T) {
	p, _ := loadDefault(t)
	sm, err := p.AddModel(SavedModel{Name: "haiku", Provider: "anthropic", Model: "claude-haiku-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	view, err := p.ModelView(sm)
	if err != nil {
		t.Fatal(err)
	}
	if view.APIKeyEnv != p.Model.APIKeyEnv || view.KeyProvider() != p.Model.KeyProvider() || !reflect.DeepEqual(view.ProxyAllow, p.Model.ProxyAllow) {
		t.Fatalf("second model %+v, default %+v: want the same credential", view, p.Model)
	}
}

func TestTheTenthSavedModelIsTheLast(t *testing.T) {
	p, _ := loadDefault(t)
	for i := 2; i <= MaxSavedModels; i++ {
		if _, err := p.AddModel(SavedModel{Name: fmt.Sprintf("m%d", i), Provider: "anthropic", Model: "claude-haiku-5-5"}); err != nil {
			t.Fatalf("model %d: %v", i, err)
		}
	}
	if _, err := p.AddModel(SavedModel{Name: "eleventh", Provider: "anthropic", Model: "x"}); err == nil || !strings.Contains(err.Error(), "10 saved models") {
		t.Fatalf("an eleventh model: %v", err)
	}
}

func TestSavingAModelIsRefusedByName(t *testing.T) {
	p, _ := loadDefault(t)
	for _, c := range []struct {
		sm   SavedModel
		want string
	}{
		{SavedModel{Name: "ANTHROPIC", Provider: "anthropic", Model: "x"}, "already a saved model"},
		{SavedModel{Name: "other", Provider: "openai", Model: "gpt"}, "does not have"},
		{SavedModel{Name: "", Provider: "anthropic", Model: "x"}, "needs a name"},
		{SavedModel{Name: "no model", Provider: "anthropic"}, "needs a model id"},
		{SavedModel{Name: "tiny", Provider: "anthropic", Model: "x", ContextWindow: 10}, "contextWindow"},
	} {
		if _, err := p.AddModel(c.sm); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("AddModel(%+v) = %v, want %q", c.sm, err, c.want)
		}
	}
}

// v2Text is a version 2 file: the default file's text with its model block
// replaced by lists.
func v2Text(t *testing.T, lists string) string {
	t.Helper()
	start := strings.Index(defaultFileText, "\nmodel:\n")
	end := strings.Index(defaultFileText, "\n# Manifest 'network.egress")
	if start < 0 || end < 0 {
		t.Fatal("the default file's model block moved")
	}
	text := defaultFileText[:start] + "\n" + lists + defaultFileText[end:]
	return strings.Replace(text, `policyVersion: "1"`, `policyVersion: "2"`, 1)
}

func TestLoadHoldsAVersion2FileToItsRules(t *testing.T) {
	provider := "providers:\n  anthropic:\n    apiKeyEnv: ANTHROPIC_API_KEY\n    proxyAllow: [api.anthropic.com:443]\n"
	model := func(id, name, prov string) string {
		return fmt.Sprintf("  - {id: %s, name: %q, provider: %s, model: claude-sonnet-5-5}\n", id, name, prov)
	}
	eleven := ""
	for i := range MaxSavedModels + 1 {
		eleven += model(fmt.Sprintf("m%d", i), fmt.Sprintf("n%d", i), "anthropic")
	}
	for name, c := range map[string]struct{ lists, want string }{
		"an eleventh model":          {provider + "models:\n" + eleven + "default: m0\n", "more than the 10"},
		"a duplicate name":           {provider + "models:\n" + model("a1", "Same", "anthropic") + model("a2", "same", "anthropic") + "default: a1\n", "already a saved model"},
		"a duplicate id":             {provider + "models:\n" + model("a1", "one", "anthropic") + model("a1", "two", "anthropic") + "default: a1\n", "two saved models have the id"},
		"a model naming no provider": {provider + "models:\n" + model("a1", "one", "openai") + "default: a1\n", "does not have"},
		"a default naming no model":  {provider + "models:\n" + model("a1", "one", "anthropic") + "default: zz\n", "names no saved model"},
		"no models":                  {provider + "default: a1\n", "at least one saved model"},
		"a custom provider with no address": {"providers:\n  custom-lab:\n    apiKeyEnv: LAB_KEY\n    proxyAllow: [lab:80]\n  anthropic:\n    apiKeyEnv: ANTHROPIC_API_KEY\n    proxyAllow: [api.anthropic.com:443]\n" +
			"models:\n" + model("a1", "one", "anthropic") + "default: a1\n", "baseUrl is required"},
		"a provider id with a colon": {"providers:\n  \"open:ai\":\n    apiKeyEnv: K\n    proxyAllow: [h:1]\n" + "models:\n" + model("a1", "one", "open:ai") + "default: a1\n", "provider id"},
	} {
		path := filepath.Join(t.TempDir(), "policy.yaml")
		if err := os.WriteFile(path, []byte(v2Text(t, c.lists)), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Load = %v, want %q", name, err, c.want)
		}
	}
	// And a good one reads, its default as the model block.
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(v2Text(t, provider+"models:\n"+model("a1", "Work", "anthropic")+"default: a1\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	p, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Model.Provider != "anthropic" || p.Model.Name != "claude-sonnet-5-5" || p.DefaultModel().Name != "Work" {
		t.Fatalf("read: %+v", p.Model)
	}
}

func TestAVersion2FileHoldsNoModelBlockAndVersion1NoLists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	both := strings.Replace(defaultFileText, `policyVersion: "1"`, `policyVersion: "2"`, 1) +
		"providers:\n  anthropic:\n    apiKeyEnv: ANTHROPIC_API_KEY\n    proxyAllow: [api.anthropic.com:443]\nmodels:\n  - {id: a1, name: w, provider: anthropic, model: x}\ndefault: a1\n"
	if err := os.WriteFile(path, []byte(both), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "model block is version 1's") {
		t.Fatalf("version 2 with a model block: %v", err)
	}
	lists := defaultFileText + "models:\n  - {id: a1, name: w, provider: anthropic, model: x}\ndefault: a1\n"
	if err := os.WriteFile(path, []byte(lists), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "need policyVersion 2") {
		t.Fatalf("version 1 with lists: %v", err)
	}
}

func TestRemovingTheDefaultNeedsANewDefault(t *testing.T) {
	p, _ := loadDefault(t)
	if _, err := p.RemoveModel("anthropic", ""); err == nil || !strings.Contains(err.Error(), "only saved model") {
		t.Fatalf("removing the only model: %v", err)
	}
	second, err := p.AddModel(SavedModel{Name: "haiku", Provider: "anthropic", Model: "claude-haiku-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.RemoveModel("anthropic", ""); err == nil || !strings.Contains(err.Error(), "is the default") {
		t.Fatalf("removing the default with no new one: %v", err)
	}
	if _, err := p.RemoveModel("anthropic", "anthropic"); err == nil || !strings.Contains(err.Error(), "removed too") {
		t.Fatalf("the removed model as the new default: %v", err)
	}
	if _, err := p.RemoveModel("anthropic", "haiku"); err != nil {
		t.Fatal(err)
	}
	if p.Default != second.ID || p.Model.Name != "claude-haiku-5-5" {
		t.Fatalf("default %q, model %+v", p.Default, p.Model)
	}
}

func TestRemovingAProviderRemovesItsModels(t *testing.T) {
	p, path := loadDefault(t)
	if err := p.AddProvider("custom-lab", customProvider("http://127.0.0.1:8001/v1")); err != nil {
		t.Fatal(err)
	}
	lab, err := p.AddModel(SavedModel{Name: "lab", Provider: "custom-lab", Model: "qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.RemoveProvider("anthropic", ""); err == nil || !strings.Contains(err.Error(), "is the default") {
		t.Fatalf("removing the default's provider: %v", err)
	}
	gone, err := p.RemoveProvider("anthropic", "lab")
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 1 || gone[0].Name != "anthropic" {
		t.Fatalf("removed %+v", gone)
	}
	_, back := saveAndRead(t, p, path)
	if back.Default != lab.ID || back.Model.Provider != ProviderCustom || back.Model.KeyProvider() != "custom-lab" {
		t.Fatalf("read back: default %q, model %+v", back.Default, back.Model)
	}
	if _, ok := back.Providers["anthropic"]; ok {
		t.Fatal("the removed provider is still in the file")
	}
}

// A writer that knows only the model block (setup) changes the default
// saved model through it.
func TestAChangeToTheModelBlockMovesTheDefault(t *testing.T) {
	p, path := loadDefault(t)
	if _, err := p.AddModel(SavedModel{Name: "haiku", Provider: "anthropic", Model: "claude-haiku-5-5"}); err != nil {
		t.Fatal(err)
	}
	def := p.Default
	p.Model = Model{Provider: "openai", Name: "gpt-5.5", APIKeyEnv: "OPENAI_API_KEY", ProxyAllow: []string{"api.openai.com:443"}}
	_, back := saveAndRead(t, p, path)
	got := back.DefaultModel()
	if got.ID != def || got.Provider != "openai" || got.Model != "gpt-5.5" || back.Model.APIKeyEnv != "OPENAI_API_KEY" {
		t.Fatalf("default after setup's change: %+v, model %+v", got, back.Model)
	}
	if _, ok := back.FindModel("haiku"); !ok {
		t.Fatal("the other saved model was lost")
	}
}

// No key value reaches the file: a provider names its variable and where
// the value comes from, and has no field that could hold the value.
func TestNoKeyValueIsWrittenToAVersion2Policy(t *testing.T) {
	const value = "sk-test-value-never-written-0123456789"
	t.Setenv("LAB_TWO_KEY", value)
	p, path := loadDefault(t)
	pr := customProvider("http://127.0.0.1:8002/v1")
	pr.APIKeyEnv, pr.APIKeySource = "LAB_TWO_KEY", "keyring"
	if err := p.AddProvider("custom-lab-two", pr); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AddModel(SavedModel{Name: "lab", Provider: "custom-lab-two", Model: "qwen3"}); err != nil {
		t.Fatal(err)
	}
	text, _ := saveAndRead(t, p, path)
	if strings.Contains(text, value) {
		t.Fatal("the key's value is in the policy file")
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(Provider{}), reflect.TypeOf(SavedModel{})} {
		for i := range typ.NumField() {
			f := typ.Field(i)
			if n := strings.ToLower(f.Name); (strings.Contains(n, "key") || strings.Contains(n, "token") || strings.Contains(n, "secret")) &&
				f.Name != "APIKeyEnv" && f.Name != "APIKeySource" && f.Name != "APIKeyKind" {
				t.Errorf("%s.%s could hold a credential", typ.Name(), f.Name)
			}
		}
	}
}

func TestCustomProviderIDIsCustomAndASlug(t *testing.T) {
	for name, want := range map[string]string{
		"Home lab":           "custom-home-lab",
		"  LM Studio (Mac) ": "custom-lm-studio-mac",
		"ollama":             "custom-ollama",
		"Büro 2":             "custom-b-ro-2",
	} {
		got, err := CustomProviderID(name)
		if err != nil || got != want {
			t.Errorf("CustomProviderID(%q) = %q, %v; want %q", name, got, err, want)
		}
		if ProviderVendor(got) != ProviderCustom {
			t.Errorf("ProviderVendor(%q) = %q", got, ProviderVendor(got))
		}
	}
	if _, err := CustomProviderID("!!!"); err == nil {
		t.Error("a name with no letter or digit was given an id")
	}
}

// A saved model read from a harness block keeps that block in step: a run
// with that adapter is asked for the block's model, so removing the saved
// model takes the model out of the block, and the file is version 1 again.
func TestAHarnessBlocksSavedModelKeepsTheBlockInStep(t *testing.T) {
	p, path := loadDefault(t)
	p.Harness = map[string]HarnessPolicy{"claude": {Model: "claude-opus-5-5", Path: "/opt/claude"}}
	claude, ok := p.FindModel("claude")
	if !ok {
		t.Fatal("no saved model for the harness block")
	}
	claude.Model = "claude-haiku-5-5"
	if _, err := p.EditModel("claude", claude); err != nil {
		t.Fatal(err)
	}
	if p.Harness["claude"].Model != "claude-haiku-5-5" {
		t.Fatalf("harness block after the edit: %+v", p.Harness["claude"])
	}
	text, back := saveAndRead(t, p, path)
	if !strings.Contains(text, `policyVersion: "1"`) {
		t.Fatalf("an edit version 1 can say wrote:\n%s", text)
	}
	if _, err := back.RemoveModel("claude", ""); err != nil {
		t.Fatal(err)
	}
	if hp := back.Harness["claude"]; hp.Model != "" || hp.Path != "/opt/claude" {
		t.Fatalf("harness block after the removal: %+v", hp)
	}
	text, back = saveAndRead(t, back, path)
	if !strings.Contains(text, `policyVersion: "1"`) {
		t.Fatalf("written:\n%s", text)
	}
	if _, ok := back.FindModel("claude"); ok {
		t.Fatal("the removed saved model came back from its harness block")
	}
}
