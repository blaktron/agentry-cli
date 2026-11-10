package managed

import (
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

func TestTheModelRowsHoldARunsModel(t *testing.T) {
	anthropic := RunModel{Provider: "anthropic", Model: "claude-x", Adapter: "docker-agent"}
	for name, c := range map[string]struct {
		settings map[string]managedsettings.Setting
		run      RunModel
		refused  string
	}{
		"a provider left out": {map[string]managedsettings.Setting{managedsettings.ModelProviders: lock(t, []string{"openai"})}, anthropic, "model.providers allows openai"},
		"a provider allowed":  {map[string]managedsettings.Setting{managedsettings.ModelProviders: lock(t, []string{"openai", "anthropic"})}, anthropic, ""},
		"a custom endpoint is custom": {map[string]managedsettings.Setting{managedsettings.ModelProviders: lock(t, []string{"anthropic"})},
			RunModel{Provider: "custom-lab", Model: "m", BaseURL: "https://lab.example/v1"}, "Running on custom"},
		"the org pays, a run on its own key": {map[string]managedsettings.Setting{managedsettings.ModelSource: lock(t, managedsettings.Source{Source: "organization-key"})}, anthropic, "runs use the Agentry provider"},
		"the org pays, a run on Agentry": {map[string]managedsettings.Setting{managedsettings.ModelSource: lock(t, managedsettings.Source{Source: "agentry-credits"})},
			RunModel{Provider: policy.ProviderAgentry, Model: "m"}, ""},
		"the member pays, a run on credits": {map[string]managedsettings.Setting{managedsettings.ModelSource: lock(t, managedsettings.Source{Source: "member"})},
			RunModel{Provider: policy.ProviderAgentry, Model: "m"}, "your own keys and plans"},
		"another model than the locked one": {map[string]managedsettings.Setting{managedsettings.ModelDefault: lock(t, managedsettings.Model{Provider: "anthropic", Model: "claude-y"})}, anthropic, "model.default"},
		"the locked model":                  {map[string]managedsettings.Setting{managedsettings.ModelDefault: lock(t, managedsettings.Model{Provider: "anthropic", Model: "claude-x"})}, anthropic, ""},
		"the locked model on the org's key": {map[string]managedsettings.Setting{
			managedsettings.ModelDefault: lock(t, managedsettings.Model{Provider: "anthropic", Model: "claude-y"}),
			managedsettings.ModelSource:  lock(t, managedsettings.Source{Source: "organization-key"}),
		}, RunModel{Provider: policy.ProviderAgentry, Model: "whatever"}, ""},
		"a default model is no lock": {map[string]managedsettings.Setting{managedsettings.ModelDefault: preset(t, managedsettings.Model{Provider: "openai", Model: "gpt-x"})}, anthropic, ""},
		"a plan not allowed": {map[string]managedsettings.Setting{managedsettings.ModelSignIns: lock(t, []string{"claude-signin"})},
			RunModel{Provider: "openai", Model: "gpt-x", KeyKind: policy.KeyKindChatGPTPlan}, "model.signIns allows claude-signin"},
		"no plan allowed": {map[string]managedsettings.Setting{managedsettings.ModelSignIns: lock(t, []string{})},
			RunModel{Provider: "anthropic", Model: "c", KeyKind: "subscription"}, "allows none"},
		"an API key is no plan": {map[string]managedsettings.Setting{managedsettings.ModelSignIns: lock(t, []string{})}, anthropic, ""},
		"an endpoint of one's own, forbidden": {map[string]managedsettings.Setting{managedsettings.ModelCustomEndpoints: lock(t, "forbidden")},
			RunModel{Provider: "custom", Model: "m", BaseURL: "https://lab.example/v1"}, "model.customEndpoints"},
		"plain http, https only": {map[string]managedsettings.Setting{managedsettings.ModelCustomEndpoints: lock(t, "https-only")},
			RunModel{Provider: "custom-lab", Model: "m", BaseURL: "http://10.0.0.5:8000/v1"}, "plain http"},
		"https, https only": {map[string]managedsettings.Setting{managedsettings.ModelCustomEndpoints: lock(t, "https-only")},
			RunModel{Provider: "custom-lab", Model: "m", BaseURL: "https://lab.example/v1"}, ""},
		"a coding tool left out": {map[string]managedsettings.Setting{managedsettings.CodingToolsAllowed: lock(t, []string{"codex"})},
			RunModel{Provider: "anthropic", Model: "c", Adapter: "claude"}, "The claude-code harness"},
		"the default harness is no coding tool": {map[string]managedsettings.Setting{managedsettings.CodingToolsAllowed: lock(t, []string{"codex"})}, anthropic, ""},
	} {
		got := held("acme", c.settings).ModelRefusal(c.run)
		switch {
		case c.refused == "" && got != "":
			t.Errorf("%s: refused %q", name, got)
		case c.refused != "" && !strings.Contains(got, c.refused):
			t.Errorf("%s: %q, want it to say %q", name, got, c.refused)
		case c.refused != "" && !strings.Contains(got, "acme"):
			t.Errorf("%s: %q does not name the organization", name, got)
		}
	}
}

func TestSetupIsPrefilledOrLockedByTheModelRows(t *testing.T) {
	if got := held("acme", nil).SetupModel(); got.Provider != "" {
		t.Fatalf("no settings pre-filled %+v", got)
	}
	got := held("acme", map[string]managedsettings.Setting{
		managedsettings.ModelSource:  lock(t, managedsettings.Source{Source: "organization-key"}),
		managedsettings.ModelDefault: lock(t, managedsettings.Model{Provider: "anthropic", Model: "claude-x"}),
	}).SetupModel()
	if got.Provider != policy.ProviderAgentry || !got.Locked || !strings.Contains(got.Line, "acme pays with its own model key") {
		t.Fatalf("the organization's key: %+v", got)
	}
	got = held("acme", map[string]managedsettings.Setting{managedsettings.ModelDefault: preset(t, managedsettings.Model{Provider: "openai", Model: "gpt-x"})}).SetupModel()
	if got.Provider != "openai" || got.Model != "gpt-x" || got.Locked || !strings.Contains(got.Line, "organization default") {
		t.Fatalf("a default model: %+v", got)
	}
}

func TestAnEndpointDefaultIsNotReportedAsApplied(t *testing.T) {
	if !AppliesSetting(managedsettings.ModelCustomEndpoints, true) || AppliesSetting(managedsettings.ModelCustomEndpoints, false) {
		t.Fatal("model.customEndpoints: a lock applies and a default does not")
	}
	if !AppliesSetting(managedsettings.ModelDefault, false) {
		t.Fatal("a model.default default is applied by setup")
	}
}
