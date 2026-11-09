package compile

import (
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// twoProviderPolicy is the test policy with a second provider, an endpoint
// of the person's own with its own key, and a saved model on it.
func twoProviderPolicy(t *testing.T) (*policy.Policy, policy.SavedModel) {
	t.Helper()
	pol := testPolicy()
	pol.Model.APIKeySource = "keyring"
	if err := pol.AddProvider("custom-lab", policy.Provider{
		Name: "Lab", APIKeyEnv: "LAB_API_KEY", APIKeySource: "keyring",
		BaseURL: "https://lab.example:8443/v1", ProxyAllow: []string{"lab.example:8443"},
	}); err != nil {
		t.Fatal(err)
	}
	lab, err := pol.AddModel(policy.SavedModel{Name: "Lab Qwen", Provider: "custom-lab", Model: "qwen3", ContextWindow: 32768})
	if err != nil {
		t.Fatal(err)
	}
	return pol, lab
}

// keySources are the plan's host-resolved variables and the keychain
// accounts they fall back to: every credential the run can resolve.
func keySources(plan *spec.Plan) (envs, accounts []string) {
	vars := slices.Clone(plan.Harness.Agent.Env)
	if plan.Harness.Host != nil {
		vars = append(vars, plan.Harness.Host.Env...)
	}
	for _, v := range vars {
		if v.FromHostEnv != "" {
			envs = append(envs, v.FromHostEnv)
		}
		if v.FromKeyring != "" {
			accounts = append(accounts, v.FromKeyring)
		}
	}
	return envs, accounts
}

// A run on a saved model resolves that model's provider's credential and no
// other's, reaches its endpoint and nothing else, and its plan names it.
func TestARunOnASavedModelResolvesOnlyItsOwnCredentialAndEndpoint(t *testing.T) {
	m := parseManifest(t, pureTextYAML)
	ws := workspaceFor(t, m, nil)
	pol, lab := twoProviderPolicy(t)

	view, err := pol.ForRun(lab)
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := compileWithPolicy(t, m, ws, view)
	if err != nil {
		t.Fatal(err)
	}
	envs, accounts := keySources(plan)
	if !slices.Equal(envs, []string{"LAB_API_KEY"}) || !slices.Equal(accounts, []string{"provider:custom-lab"}) {
		t.Fatalf("a run on %s resolves %v from the keychain accounts %v; want only the lab's", lab.Name, envs, accounts)
	}
	if !slices.Equal(plan.Proxy.Allow, []string{"lab.example:8443"}) {
		t.Fatalf("the proxy admits %v; want the lab's endpoint alone", plan.Proxy.Allow)
	}
	if sm := plan.Harness.SavedModel; sm == nil || sm.ID != lab.ID || sm.Name != "Lab Qwen" {
		t.Fatalf("the plan names the saved model %+v", plan.Harness.SavedModel)
	}
	if !strings.Contains(plan.Harness.Model, "qwen3") {
		t.Fatalf("the plan's model is %q", plan.Harness.Model)
	}
	if cw := plan.Harness.ContextWindow; cw == nil || cw.Tokens != 32768 {
		t.Fatalf("the plan's context window is %+v", cw)
	}
	b, err := plan.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{"ANTHROPIC_API_KEY", "provider:anthropic", "api.anthropic.com"} {
		if strings.Contains(string(b), other) {
			t.Errorf("the plan of a run on %s names the default provider's %s", lab.Name, other)
		}
	}

	// The default, compiled the same way, is the default provider's alone.
	def, err := pol.ForRun(pol.DefaultModel())
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err = compileWithPolicy(t, m, ws, def)
	if err != nil {
		t.Fatal(err)
	}
	envs, accounts = keySources(plan)
	if !slices.Equal(envs, []string{"ANTHROPIC_API_KEY"}) || !slices.Equal(accounts, []string{"provider:anthropic"}) ||
		!slices.Equal(plan.Proxy.Allow, []string{"api.anthropic.com:443"}) {
		t.Fatalf("the default resolves %v from %v and reaches %v", envs, accounts, plan.Proxy.Allow)
	}
	if sm := plan.Harness.SavedModel; sm == nil || sm.ID != pol.DefaultModel().ID {
		t.Fatalf("the default's plan names %+v", sm)
	}
}

// A harness block's key belongs to the provider it was written for: a run
// on another provider's saved model on that harness reads none of it.
func TestAHarnessBlocksKeyReachesNoRunOnAnotherProvider(t *testing.T) {
	m := parseManifest(t, pureTextYAML)
	ws := workspaceFor(t, m, nil)
	pol, lab := twoProviderPolicy(t)
	pol.Harness = map[string]policy.HarnessPolicy{policy.DockerAgentAdapter: {APIKeyEnv: "TEAM_ANTHROPIC_KEY"}}

	view, err := pol.ForRun(lab)
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := compileWithPolicy(t, m, ws, view)
	if err != nil {
		t.Fatal(err)
	}
	if envs, _ := keySources(plan); !slices.Equal(envs, []string{"LAB_API_KEY"}) {
		t.Fatalf("a run on %s resolves %v", lab.Name, envs)
	}
	def, err := pol.ForRun(pol.DefaultModel())
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err = compileWithPolicy(t, m, ws, def)
	if err != nil {
		t.Fatal(err)
	}
	if envs, _ := keySources(plan); !slices.Equal(envs, []string{"TEAM_ANTHROPIC_KEY"}) {
		t.Fatalf("the default, which the block was written for, resolves %v", envs)
	}
}

// A run on a sign-in's saved model spends that sign-in alone: no key of
// the default provider's is resolved, and the proxy admits the sign-in's
// endpoint alone.
func TestARunOnASignInsSavedModelResolvesNoKey(t *testing.T) {
	m := parseManifest(t, pureTextYAML)
	ws := workspaceFor(t, m, nil)
	pol := testPolicy()
	if err := pol.AddProvider("openai", policy.Provider{APIKeyKind: policy.KeyKindChatGPTPlan, ProxyAllow: []string{"api.openai.com:443"}}); err != nil {
		t.Fatal(err)
	}
	sm, err := pol.AddModel(policy.SavedModel{Name: "Plan", Provider: "openai", Model: "gpt-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	view, err := pol.ForRun(sm)
	if err != nil {
		t.Fatal(err)
	}
	planDir := t.TempDir()
	plan, err := Compile(Inputs{
		Manifest: m, Digest: "sha256:0123456789abcdef", Source: "local", WorkspaceDir: ws,
		Policy: view, PolicySHA: "fixedpolicysha", RunnerImage: "agentry-runner:test", AdapterName: "codex-image",
	}, planDir)
	if err != nil {
		t.Fatal(err)
	}
	if msi := plan.Harness.ModelSignIn; msi == nil || msi.Kind != policy.KeyKindChatGPTPlan {
		t.Fatalf("the plan spends %+v", plan.Harness.ModelSignIn)
	}
	if envs, accounts := keySources(plan); len(envs) != 0 || len(accounts) != 0 {
		t.Fatalf("a run on the ChatGPT plan resolves %v from %v", envs, accounts)
	}
	if !slices.Equal(plan.Proxy.Allow, []string{"api.openai.com:443"}) {
		t.Fatalf("the proxy admits %v", plan.Proxy.Allow)
	}
	if plan.Harness.SavedModel == nil || plan.Harness.SavedModel.Name != "Plan" {
		t.Fatalf("the plan names %+v", plan.Harness.SavedModel)
	}
}
