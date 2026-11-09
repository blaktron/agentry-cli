package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// modelEndpoint is an OpenAI-compatible server of the person's own listing
// models, each with a context window.
func modelEndpoint(t *testing.T, window int, models ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		data := []map[string]any{}
		for _, m := range models {
			data = append(data, map[string]any{"id": m, "max_model_len": window})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// modelsFixture is a runner home with the default policy and no keychain,
// where the OpenAI listing answers from a fake and every other listing is
// the real one.
func modelsFixture(t *testing.T) (home, policyPath string) {
	t.Helper()
	isolateEnv(t)
	home, policyPath = isolateRunnerHome(t)
	t.Setenv("AGENTRY_NO_KEYRING", "1")
	prev := listModelsToCheck
	listModelsToCheck = func(ctx context.Context, provider, base, key string) (*setup.Catalog, error) {
		if provider == "openai" {
			if key != "sk-test-openai" {
				return nil, &os.PathError{Op: "list", Path: "openai", Err: os.ErrPermission}
			}
			return &setup.Catalog{Models: []setup.Model{{ID: "gpt-5.5"}, {ID: "gpt-5.5-mini"}}}, nil
		}
		return setup.ListModelsToCheck(ctx, provider, base, key)
	}
	t.Cleanup(func() { listModelsToCheck = prev })
	if _, _, err := policy.EnsureDefault(policyPath); err != nil {
		t.Fatal(err)
	}
	return home, policyPath
}

func cli(t *testing.T, args ...string) string {
	t.Helper()
	out, errOut, err := runCLI(t, strings.NewReader(""), args...)
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, errOut)
	}
	return out
}

func readPolicy(t *testing.T, path string) (string, *policy.Policy) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := policy.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), p
}

// Two endpoints of the person's own and one key provider, three saved
// models on them, the default kept and the windows filled from the
// endpoints.
func TestProvidersAndSavedModelsFromTheTerminal(t *testing.T) {
	_, path := modelsFixture(t)
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	one := modelEndpoint(t, 32768, "qwen3", "llama4")
	two := modelEndpoint(t, 131072, "gemma4")
	cli(t, "providers", "add", "custom", "--name", "Lab one", "--base-url", one.URL+"/v1")
	cli(t, "--json", "providers", "add", "custom", "--name", "Lab two", "--base-url", two.URL+"/v1")
	cli(t, "providers", "add", "openai")
	cli(t, "models", "add", "Qwen", "--provider", "custom-lab-one", "--model", "qwen3")
	cli(t, "models", "add", "Gemma", "--provider", "custom-lab-two", "--model", "gemma4", "--context-window", "65536")
	out := cli(t, "--json", "models", "add", "GPT", "--provider", "openai", "--model", "gpt-5.5")
	var added struct {
		ID, Name, Provider, Model string
	}
	if err := json.Unmarshal([]byte(out), &added); err != nil || added.Name != "GPT" || added.ID == "" {
		t.Fatalf("models add --json: %s (%v)", out, err)
	}

	text, pol := readPolicy(t, path)
	if !strings.Contains(text, `policyVersion: "2"`) || strings.Contains(text, "sk-test-openai") {
		t.Fatalf("policy:\n%s", text)
	}
	for name, want := range map[string]struct {
		provider string
		window   int64
	}{"Qwen": {"custom-lab-one", 32768}, "Gemma": {"custom-lab-two", 65536}, "GPT": {"openai", 0}} {
		sm, ok := pol.FindModel(name)
		if !ok || sm.Provider != want.provider || sm.ContextWindow != want.window {
			t.Errorf("%s = %+v, %t; want %+v", name, sm, ok, want)
		}
	}
	if pol.DefaultModel().Name != "anthropic" || pol.Model.Name != policy.DefaultModelName {
		t.Fatalf("the default moved: %+v", pol.DefaultModel())
	}
	var list struct {
		Default string
		Max     int
		Models  []savedModelOut
	}
	if err := json.Unmarshal([]byte(cli(t, "--json", "models", "list")), &list); err != nil || len(list.Models) != 4 || list.Max != 10 {
		t.Fatalf("models list: %+v (%v)", list, err)
	}
	cli(t, "models", "default", "Qwen")
	_, pol = readPolicy(t, path)
	if pol.Model.Provider != "custom" || pol.Model.KeyProvider() != "custom-lab-one" || pol.Model.ContextWindow != 32768 || pol.Model.BaseURL != one.URL+"/v1" {
		t.Fatalf("the default as a run reads it: %+v", pol.Model)
	}
	provs := cli(t, "providers", "list")
	for _, want := range []string{"custom-lab-one (Lab one,", "custom-lab-two (Lab two,", "openai", "models: GPT"} {
		if !strings.Contains(provs, want) {
			t.Errorf("providers list lacks %q:\n%s", want, provs)
		}
	}
}

// The save check refuses an id the provider does not offer, and an
// endpoint that cannot be reached, with the reason, and saves nothing.
func TestTheSaveCheckRefusesAnUnknownIDAndAnUnreachableEndpoint(t *testing.T) {
	_, path := modelsFixture(t)
	srv := modelEndpoint(t, 8192, "qwen3")
	cli(t, "providers", "add", "custom", "--name", "Lab", "--base-url", srv.URL+"/v1")
	before, _ := os.ReadFile(path)
	_, errOut, err := runCLI(t, strings.NewReader(""), "models", "add", "Typo", "--provider", "custom-lab", "--model", "qwen-3")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), `does not offer "qwen-3"`) || !strings.Contains(err.Error(), "qwen3") {
		t.Fatalf("refusal: %v\n%s", err, errOut)
	}
	srv.Close()
	_, _, err = runCLI(t, strings.NewReader(""), "models", "add", "Gone", "--provider", "custom-lab", "--model", "qwen3")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "could not check the model") {
		t.Fatalf("refusal: %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("a refused save changed the policy")
	}
	t.Setenv("OPENAI_API_KEY", "")
	cli(t, "providers", "add", "openai")
	_, _, err = runCLI(t, strings.NewReader(""), "models", "add", "GPT", "--provider", "openai", "--model", "gpt-5.5")
	if err == nil || !strings.Contains(err.Error(), "no key for openai") {
		t.Fatalf("no key: %v", err)
	}
}

func TestTheEleventhSavedModelIsRefused(t *testing.T) {
	_, _ = modelsFixture(t)
	srv := modelEndpoint(t, 8192, "qwen3")
	cli(t, "providers", "add", "custom", "--name", "Lab", "--base-url", srv.URL+"/v1")
	for i := 2; i <= policy.MaxSavedModels; i++ {
		cli(t, "models", "add", "m"+string(rune('a'+i)), "--provider", "custom-lab", "--model", "qwen3")
	}
	_, _, err := runCLI(t, strings.NewReader(""), "models", "add", "eleventh", "--provider", "custom-lab", "--model", "qwen3")
	if err == nil || !strings.Contains(err.Error(), "10 saved models") {
		t.Fatalf("an eleventh: %v", err)
	}
}

// A saved model a schedule names is not removed until the schedule moves,
// and it moves to the model --move-to names, never to the default on its
// own; the default needs a new one named.
func TestRemovingASavedModelAScheduleNamesIsRefusedUntilItMoves(t *testing.T) {
	home, path := modelsFixture(t)
	srv := modelEndpoint(t, 8192, "qwen3", "llama4")
	cli(t, "providers", "add", "custom", "--name", "Lab", "--base-url", srv.URL+"/v1")
	cli(t, "models", "add", "Qwen", "--provider", "custom-lab", "--model", "qwen3")
	cli(t, "models", "add", "Llama", "--provider", "custom-lab", "--model", "llama4")
	_, pol := readPolicy(t, path)
	qwen, _ := pol.FindModel("Qwen")
	llama, _ := pol.FindModel("Llama")
	st := schedule.StoreIn(home)
	sc := &schedule.Schedule{Name: "Morning brief", Local: home, Run: schedule.RunSpec{OutDir: home, Model: qwen.ID}, Created: time.Now()}
	if err := st.Create(sc, nil); err != nil {
		t.Fatal(err)
	}

	_, _, err := runCLI(t, strings.NewReader(""), "models", "remove", "Qwen")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), `the saved model "Qwen" is named by schedule Morning brief (`+sc.ID+")") || !strings.Contains(err.Error(), "--move-to") {
		t.Fatalf("refusal: %v", err)
	}
	if _, pol = readPolicy(t, path); func() bool { _, ok := pol.FindModel("Qwen"); return !ok }() {
		t.Fatal("the refused removal removed the model")
	}
	var out removedOut
	if err := json.Unmarshal([]byte(cli(t, "--json", "models", "remove", "Qwen", "--move-to", "Llama")), &out); err != nil || len(out.Moved) != 1 || out.MovedTo != "Llama" {
		t.Fatalf("remove --move-to: %+v (%v)", out, err)
	}
	got, err := st.Get(sc.ID)
	if err != nil || got.Run.Model != llama.ID {
		t.Fatalf("the schedule names %q, want %q (%v)", got.Run.Model, llama.ID, err)
	}

	_, _, err = runCLI(t, strings.NewReader(""), "models", "remove", "anthropic")
	if err == nil || !strings.Contains(err.Error(), "is the default") {
		t.Fatalf("removing the default with no new one: %v", err)
	}
	cli(t, "models", "remove", "anthropic", "--move-to", "Llama")
	if _, pol = readPolicy(t, path); pol.DefaultModel().ID != llama.ID {
		t.Fatalf("default %+v", pol.DefaultModel())
	}
	// A provider with a model a schedule names is refused the same way.
	_, _, err = runCLI(t, strings.NewReader(""), "providers", "remove", "custom-lab")
	if err == nil || !strings.Contains(err.Error(), "the provider custom-lab has saved models named by schedule Morning brief") {
		t.Fatalf("removing the schedule's provider: %v", err)
	}
}

// A version 1 policy is read as one provider and its model, and stays
// version 1 on disk until it holds what version 1 cannot say.
func TestAVersion1PolicyStaysVersion1UntilASecondModelIsSaved(t *testing.T) {
	_, path := modelsFixture(t)
	cli(t, "models", "list")
	cli(t, "models", "default", "anthropic")
	if text, _ := readPolicy(t, path); !strings.Contains(text, `policyVersion: "1"`) {
		t.Fatalf("after list and default:\n%s", text)
	}
	srv := modelEndpoint(t, 8192, "qwen3")
	cli(t, "providers", "add", "custom", "--name", "Lab", "--base-url", srv.URL+"/v1")
	if text, _ := readPolicy(t, path); !strings.Contains(text, `policyVersion: "2"`) {
		t.Fatalf("after a second provider:\n%s", text)
	}
	cli(t, "providers", "remove", "custom-lab")
	if text, _ := readPolicy(t, path); !strings.Contains(text, `policyVersion: "1"`) {
		t.Fatalf("one provider and one unnamed model again:\n%s", text)
	}
}

// Setup makes the first provider and saved model, the default; on a
// version 2 policy it changes the default and keeps the rest.
func TestSetupMakesTheDefaultSavedModel(t *testing.T) {
	_, path := modelsFixture(t)
	srv := modelEndpoint(t, 8192, "qwen3")
	cli(t, "setup", "--provider", "openai", "--model", "gpt-5.5", "--yes", "--skip-images", "--policy", path)
	text, pol := readPolicy(t, path)
	if !strings.Contains(text, `policyVersion: "1"`) || pol.DefaultModel().Name != "openai" || pol.DefaultModel().Model != "gpt-5.5" {
		t.Fatalf("after setup: %+v\n%s", pol.DefaultModel(), text)
	}
	cli(t, "providers", "add", "custom", "--name", "Lab", "--base-url", srv.URL+"/v1")
	cli(t, "models", "add", "Qwen", "--provider", "custom-lab", "--model", "qwen3")
	cli(t, "setup", "--provider", "anthropic", "--model", "claude-opus-5-5", "--yes", "--skip-images", "--policy", path)
	_, pol = readPolicy(t, path)
	if pol.Model.Provider != "anthropic" || pol.Model.Name != "claude-opus-5-5" {
		t.Fatalf("the default after setup again: %+v", pol.Model)
	}
	if _, ok := pol.FindModel("Qwen"); !ok {
		t.Fatal("setup lost a saved model")
	}
}

// A key read with --key-stdin goes to the OS keychain as provider:<id>
// alone: never into the policy, the output or the JSON. Adding a provider
// this computer already has is refused before any key is read, so the key
// kept is never replaced.
func TestAProvidersKeyFromTheStandardInputIsKeptInTheKeychainAlone(t *testing.T) {
	_, path := modelsFixture(t)
	t.Setenv("AGENTRY_NO_KEYRING", "")
	keys := fakeKeychain{}
	t.Cleanup(modelkey.UseKeychain(keys))
	const key = "sk-lab-kept-value-9876543210"
	srv := modelEndpoint(t, 8192, "qwen3")
	out, errOut, err := runCLI(t, strings.NewReader(key+"\n"), "--json", "providers", "add", "custom", "--name", "Lab", "--base-url", srv.URL+"/v1", "--key-stdin")
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	if keys[modelkey.Account("custom-lab")] != key {
		t.Fatalf("keychain %v", keys)
	}
	text, pol := readPolicy(t, path)
	if strings.Contains(out+errOut+text, key) {
		t.Fatal("the key reached the output or the policy")
	}
	if pr := pol.Providers["custom-lab"]; pr.APIKeySource != "keyring" || pr.APIKeyEnv != "CUSTOM_LAB_API_KEY" {
		t.Fatalf("provider %+v", pr)
	}
	_, _, err = runCLI(t, strings.NewReader("sk-other\n"), "providers", "add", "custom", "--name", "lab", "--base-url", srv.URL+"/v1", "--key-stdin")
	if err == nil || !strings.Contains(err.Error(), "already has the provider custom-lab") {
		t.Fatalf("a second custom-lab: %v", err)
	}
	if keys[modelkey.Account("custom-lab")] != key {
		t.Fatal("the refused add replaced the kept key")
	}
	cli(t, "providers", "remove", "custom-lab")
	if _, ok := keys[modelkey.Account("custom-lab")]; ok {
		t.Fatal("removing the provider left its key in the keychain")
	}
}
