package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// labModels is the models fixture with an endpoint of the person's own and
// two saved models on it beside the default.
func labModels(t *testing.T) (home, path string) {
	t.Helper()
	home, path = modelsFixture(t)
	srv := modelEndpoint(t, 32768, "qwen3", "llama4")
	cli(t, "providers", "add", "custom", "--name", "Lab", "--base-url", srv.URL+"/v1")
	cli(t, "models", "add", "Qwen", "--provider", "custom-lab", "--model", "qwen3")
	cli(t, "models", "add", "Llama", "--provider", "custom-lab", "--model", "llama4")
	return home, path
}

// `run --plan --json` names the saved model the run is on, and its plan
// holds that model's endpoint and nothing of the default provider's.
func TestARunPlanIsOnTheSavedModelItNames(t *testing.T) {
	_, path := labModels(t)
	pkg := repoPath(t, "testdata/runner/pure-text")
	out := cli(t, "--json", "run", "--local", pkg, "--plan", "--policy", path, "--model", "Qwen")
	var plan runPlanOutput
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatal(err)
	}
	if sm := plan.Model.SavedModel; sm == nil || sm.Name != "Qwen" || plan.Model.Model != "custom/qwen3" || plan.Model.Adapter != "docker-agent" {
		t.Fatalf("the plan's model = %+v (%v)", plan.Model, plan.Model.SavedModel)
	}
	var compiled spec.Plan
	if err := json.Unmarshal(plan.Plan, &compiled); err != nil {
		t.Fatal(err)
	}
	// The proxy's one entry is the endpoint's, under the name a container
	// reaches this computer's loopback by.
	if compiled.Harness.SavedModel == nil || compiled.Harness.SavedModel.Name != "Qwen" || len(compiled.Proxy.Allow) != 1 || strings.Contains(compiled.Proxy.Allow[0], "anthropic") {
		t.Fatalf("the compiled plan: saved model %+v, proxy %v", compiled.Harness.SavedModel, compiled.Proxy.Allow)
	}
	for _, other := range []string{"ANTHROPIC_API_KEY", "api.anthropic.com"} {
		if strings.Contains(string(plan.Plan), other) {
			t.Errorf("a run on Qwen's plan names the default provider's %s", other)
		}
	}
	// In words, the same model.
	words := cli(t, "run", "--local", pkg, "--plan", "--policy", path, "--model", "Llama")
	if !strings.Contains(words, "model: Llama (custom/llama4) on docker-agent\n") {
		t.Fatalf("the plan in words:\n%s", words)
	}
	// Without --model, the default.
	if err := json.Unmarshal([]byte(cli(t, "--json", "run", "--local", pkg, "--plan", "--policy", path)), &plan); err != nil || plan.Model.SavedModel == nil || plan.Model.SavedModel.Name != "anthropic" {
		t.Fatalf("a run naming no model: %+v (%v)", plan.Model, err)
	}
}

// A run naming a saved model this computer does not have is refused by its
// name, and never runs on the default.
func TestARunOnAMissingSavedModelIsRefusedByName(t *testing.T) {
	_, path := labModels(t)
	pkg := repoPath(t, "testdata/runner/pure-text")
	_, _, err := runCLI(t, strings.NewReader(""), "--json", "run", "--local", pkg, "--plan", "--policy", path, "--model", "Gemma")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), `no saved model "Gemma"`) || !strings.Contains(err.Error(), "agentry models list") {
		t.Fatalf("refusal: %v", err)
	}
}

// --model chooses among this computer's saved models: a run elsewhere is
// refused it.
func TestModelIsRefusedForARunElsewhere(t *testing.T) {
	_, path := labModels(t)
	_, _, err := runCLI(t, strings.NewReader(""), "run", "acme/demo", "--on", "box", "--policy", path, "--model", "Qwen")
	wantClassified(t, err, engine.ExitUsage)
	if !strings.Contains(err.Error(), "remote runhost") {
		t.Fatalf("refusal: %v", err)
	}
	t.Setenv(envHosted, "1")
	if err := refuseModelElsewhere(runOpts{model: "Qwen"}); err == nil || !strings.Contains(err.Error(), "hosted run") {
		t.Fatalf("a hosted run: %v", err)
	}
}

// A saved model whose key is not on this computer is refused before the
// run, naming the model, its provider and the fix.
func TestASavedModelWithNoKeyIsRefusedByNameBeforeTheRun(t *testing.T) {
	pol := policy.Default()
	if err := pol.AddProvider("openai", policy.Provider{APIKeyEnv: "OPENAI_API_KEY", APIKeySource: "keyring", ProxyAllow: []string{"api.openai.com:443"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pol.AddModel(policy.SavedModel{Name: "GPT", Provider: "openai", Model: "gpt-5.5"}); err != nil {
		t.Fatal(err)
	}
	rm, err := localrun.ChooseRunModel(pol, "GPT", "")
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	prev := keyKept
	keyKept = func(provider string) (bool, error) { return kept[provider], nil }
	t.Cleanup(func() { keyKept = prev })
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }

	err = savedModelUsable(t.TempDir(), rm, getenv, runOnAnother)
	wantClassified(t, err, engine.ExitPolicy)
	for _, want := range []string{`the saved model "GPT" (provider openai)`, "$OPENAI_API_KEY", "provider:openai", "--model"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
	kept["openai"] = true
	if err := savedModelUsable(t.TempDir(), rm, getenv, runOnAnother); err != nil {
		t.Fatalf("with the key in the keychain: %v", err)
	}
	kept["openai"] = false
	env["OPENAI_API_KEY"] = "sk-test"
	if err := savedModelUsable(t.TempDir(), rm, getenv, runOnAnother); err != nil {
		t.Fatalf("with the key in the environment: %v", err)
	}
}

// A sign-in a saved model spends that is not kept refuses the run by name.
func TestASavedModelWhoseSignInIsGoneIsRefusedByName(t *testing.T) {
	pol := policy.Default()
	if err := pol.AddProvider("openai", policy.Provider{APIKeyKind: policy.KeyKindChatGPTPlan, ProxyAllow: []string{"api.openai.com:443"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pol.AddModel(policy.SavedModel{Name: "Plan", Provider: "openai", Model: "gpt-5.5"}); err != nil {
		t.Fatal(err)
	}
	rm, err := localrun.ChooseRunModel(pol, "Plan", "")
	if err != nil {
		t.Fatal(err)
	}
	prev := signInKept
	signInKept = func(string, string) bool { return false }
	t.Cleanup(func() { signInKept = prev })
	err = savedModelUsable(t.TempDir(), rm, func(string) string { return "" }, runOnAnother)
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), `the saved model "Plan" (provider openai)`) || !strings.Contains(err.Error(), "model-signins add chatgpt") {
		t.Fatalf("refusal: %v", err)
	}
}

// The policy's cost budget holds for every saved model a run is on: a run
// on a sign-in's model is refused it by the model's name.
func TestThePolicysCostBudgetRefusesARunOnASignInsSavedModel(t *testing.T) {
	plan := &spec.Plan{Budgets: spec.Budgets{MaxCostUSD: 5}, Harness: spec.HarnessSpec{
		ModelSignIn: &spec.ModelSignInSpec{Kind: policy.KeyKindChatGPTPlan},
		SavedModel:  &spec.SavedModelRef{ID: "ab12cd34", Name: "Plan"},
	}}
	err := refuseSignInRun(plan, runOpts{maxCost: -1})
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), `budgets.maxCostUsd cannot apply to the saved model "Plan"`) {
		t.Fatalf("refusal: %v", err)
	}
	if err := refuseSignInRun(plan, runOpts{maxCost: 0}); err != nil {
		t.Fatalf("--max-cost 0 lifts the policy's budget: %v", err)
	}
	plan.Harness.ModelSignIn = nil
	if err := refuseSignInRun(plan, runOpts{maxCost: -1}); err != nil {
		t.Fatalf("a run on a key: %v", err)
	}
}

// A schedule keeps the saved model it was given, by id, when the default
// changes, and each fire names it.
func TestAScheduleKeepsItsSavedModelWhenTheDefaultChanges(t *testing.T) {
	_, path := labModels(t)
	sc := schedule.Schedule{Ref: "acme/brief", Version: "1.0.0", Digest: "sha256:abc", Run: schedule.RunSpec{Policy: path, MaxTurns: -1, MaxCost: -1}}
	if err := assignScheduleModel(&sc, "qwen"); err != nil {
		t.Fatal(err)
	}
	_, pol := readPolicy(t, path)
	qwen, _ := pol.FindModel("Qwen")
	if sc.Run.Model != qwen.ID {
		t.Fatalf("the record names %q, want Qwen's id %q", sc.Run.Model, qwen.ID)
	}
	if j := strings.Join(baseRunArgs(sc), " "); !strings.Contains(j, "--model "+qwen.ID) {
		t.Fatalf("a fire's args %q do not name the saved model", j)
	}
	cli(t, "models", "default", "Llama")
	rm, err := scheduleRunModel(sc)
	if err != nil || rm.Saved.ID != qwen.ID || rm.Policy.Model.Name != "qwen3" {
		t.Fatalf("after the default moved, the fire is on %+v (%v)", rm.Saved, err)
	}
	// A schedule naming none follows the default, whichever it is.
	follows := sc
	if err := assignScheduleModel(&follows, ""); err != nil || follows.Run.Model != "" {
		t.Fatalf("a schedule naming no model: %q, %v", follows.Run.Model, err)
	}
	if slices.Contains(baseRunArgs(follows), "--model") {
		t.Fatal("a schedule that follows the default names a model")
	}
	if rm, err := scheduleRunModel(follows); err != nil || rm.Saved.Name != "Llama" {
		t.Fatalf("a schedule following the default is on %+v (%v)", rm.Saved, err)
	}
	// A record naming a saved model that is gone is refused, never run on
	// the default.
	gone := sc
	gone.Run.Model = "0badc0de"
	if _, err := scheduleRunModel(gone); !localrun.IsMissingModel(err) {
		t.Fatalf("a record naming a missing saved model: %v", err)
	}
	if err := assignScheduleModel(&gone, "Gemma"); !localrun.IsMissingModel(err) {
		t.Fatalf("schedule --model Gemma: %v", err)
	}
}

// A harness block that names a model: a schedule on that harness is
// assigned the block's saved model, one whose record names no model runs on
// it too, and removing it is refused while a schedule runs on it.
func TestAScheduleOnAHarnessBlocksHarnessIsAssignedItsSavedModel(t *testing.T) {
	home, path := modelsFixture(t)
	pol, _, err := policy.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	pol.Harness = map[string]policy.HarnessPolicy{"claude": {Model: "sonnet"}}
	if err := policy.Save(path, pol); err != nil {
		t.Fatal(err)
	}
	claude, ok := pol.HarnessModel("claude")
	if !ok {
		t.Fatal("no saved model for the claude block")
	}
	sc := schedule.Schedule{Name: "Brief", Local: home, Created: time.Now(), Run: schedule.RunSpec{Policy: path, OutDir: home, Adapter: "claude"}}
	if err := assignScheduleModel(&sc, ""); err != nil || sc.Run.Model != claude.ID {
		t.Fatalf("a schedule on --adapter claude names %q, want %q (%v)", sc.Run.Model, claude.ID, err)
	}
	unnamed := sc
	unnamed.Run.Model = ""
	if err := schedule.StoreIn(home).Create(&unnamed, nil); err != nil {
		t.Fatal(err)
	}
	if rm, err := scheduleRunModel(unnamed); err != nil || rm.Saved.ID != claude.ID || rm.Adapter != "claude" {
		t.Fatalf("a record naming no model on the claude harness fires on %+v with %s (%v)", rm.Saved, rm.Adapter, err)
	}
	_, _, err = runCLI(t, strings.NewReader(""), "models", "remove", "claude", "--policy", path)
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "schedule Brief") {
		t.Fatalf("removal: %v", err)
	}
}

// An endpoint of the person's own added with no key is saved as taking
// none, and a run on it is given none to find: its plan resolves no key and
// nothing refuses it before the run. An endpoint added with a key variable
// is still refused by that variable's name while it is unset.
func TestAnEndpointAddedWithNoKeyRunsWithNone(t *testing.T) {
	home, path := labModels(t)
	_, pol := readPolicy(t, path)
	if pr := pol.Providers["custom-lab"]; pr.APIKeyEnv != "" || pr.APIKeySource != "" {
		t.Fatalf("the provider added with no key is %+v", pr)
	}
	if out := cli(t, "providers", "list"); !strings.Contains(out, "custom-lab") || !strings.Contains(out, "takes no key") {
		t.Fatalf("providers list:\n%s", out)
	}
	pkg := repoPath(t, "testdata/runner/pure-text")
	var out runPlanOutput
	if err := json.Unmarshal([]byte(cli(t, "--json", "run", "--local", pkg, "--plan", "--policy", path, "--model", "Qwen")), &out); err != nil {
		t.Fatal(err)
	}
	var plan spec.Plan
	if err := json.Unmarshal(out.Plan, &plan); err != nil {
		t.Fatal(err)
	}
	if envs, accounts := planKeySources(&plan); len(envs) != 0 || len(accounts) != 0 {
		t.Fatalf("a run on an endpoint that takes no key resolves %v from %v", envs, accounts)
	}
	given := false
	for _, v := range plan.Harness.Agent.Env {
		if v.Name == "OPENAI_API_KEY" && v.Value != "" {
			given = true
		}
	}
	if !given {
		t.Fatalf("the harness's client is given no stand-in for the key it asks for: %+v", plan.Harness.Agent.Env)
	}
	rm, err := localrun.ChooseRunModel(pol, "Qwen", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := savedModelUsable(home, rm, func(string) string { return "" }, runOnAnother); err != nil {
		t.Fatalf("a run on an endpoint that takes no key is refused: %v", err)
	}

	srv := modelEndpoint(t, 8192, "qwen3")
	cli(t, "providers", "add", "custom", "--name", "Keyed", "--base-url", srv.URL+"/v1", "--api-key-env", "KEYED_LAB_KEY")
	cli(t, "models", "add", "Keyed Qwen", "--provider", "custom-keyed", "--model", "qwen3")
	_, pol = readPolicy(t, path)
	rm, err = localrun.ChooseRunModel(pol, "Keyed Qwen", "")
	if err != nil {
		t.Fatal(err)
	}
	err = savedModelUsable(home, rm, func(string) string { return "" }, runOnAnother)
	wantClassified(t, err, engine.ExitPolicy)
	for _, want := range []string{`the saved model "Keyed Qwen" (provider custom-keyed)`, "$KEYED_LAB_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
	if err := json.Unmarshal([]byte(cli(t, "--json", "run", "--local", pkg, "--plan", "--policy", path, "--model", "Keyed Qwen")), &out); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Plan, &plan); err != nil {
		t.Fatal(err)
	}
	if envs, _ := planKeySources(&plan); !slices.Equal(envs, []string{"KEYED_LAB_KEY"}) {
		t.Fatalf("a run on an endpoint with a key variable resolves %v", envs)
	}
}

// planKeySources are the plan's host-resolved variables and the keychain
// accounts they fall back to.
func planKeySources(plan *spec.Plan) (envs, accounts []string) {
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
