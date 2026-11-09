package savedmodels

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// lab is a runner home whose policy keeps the default saved model and two
// on an endpoint of the person's own, Qwen and Llama.
func lab(t *testing.T) (home, path string, pol *policy.Policy) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("AGENTRY_RUNNER_HOME", home)
	t.Setenv("AGENTRY_NO_KEYRING", "1")
	path = policy.PathIn(home)
	pol, _, err := policy.EnsureDefault(path)
	if err != nil {
		t.Fatal(err)
	}
	id, pr, _, err := CustomProvider("Lab", "http://127.0.0.1:9/v1", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := pol.AddProvider(id, pr); err != nil {
		t.Fatal(err)
	}
	for _, m := range []policy.SavedModel{{Name: "Qwen", Provider: id, Model: "qwen3"}, {Name: "Llama", Provider: id, Model: "llama4"}} {
		if _, err := pol.AddModel(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := policy.Save(path, pol); err != nil {
		t.Fatal(err)
	}
	return home, path, reload(t, path)
}

func reload(t *testing.T, path string) *policy.Policy {
	t.Helper()
	pol, _, err := policy.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return pol
}

func savedModel(t *testing.T, pol *policy.Policy, name string) policy.SavedModel {
	t.Helper()
	sm, ok := pol.FindModel(name)
	if !ok {
		t.Fatalf("no saved model %s", name)
	}
	return sm
}

func scheduleOn(t *testing.T, home, model string) *schedule.Schedule {
	t.Helper()
	sc := &schedule.Schedule{Name: "Morning brief", Local: home, Run: schedule.RunSpec{OutDir: home, Model: model}, Created: time.Now()}
	if err := schedule.StoreIn(home).Create(sc, nil); err != nil {
		t.Fatal(err)
	}
	return sc
}

func agentOn(t *testing.T, home, model string) persistent.Agent {
	t.Helper()
	for _, a := range persistent.AgentsIn(home) {
		if a.DesktopOnly {
			continue
		}
		if _, _, err := persistent.Configure(context.Background(), home, a, persistent.ClientCLI, persistent.Change{Model: &model}); err != nil {
			t.Fatal(err)
		}
		return a
	}
	t.Fatal("no agent the CLI configures")
	return persistent.Agent{}
}

func configured(t *testing.T, home, agent string) string {
	t.Helper()
	c, err := persistent.LoadConfig(home, agent)
	if err != nil {
		t.Fatal(err)
	}
	return c.Model
}

// A removal of a saved model a schedule and an agent name is refused, with
// both listed and nothing changed, until a saved model to move them to is
// named; then both name it, and the policy is written without the model.
func TestARemovalOfAModelSomethingNamesIsRefusedUntilItIsMoved(t *testing.T) {
	home, path, pol := lab(t)
	qwen, llama := savedModel(t, pol, "Qwen"), savedModel(t, pol, "Llama")
	sc := scheduleOn(t, home, qwen.ID)
	a := agentOn(t, home, qwen.ID)

	users, err := Users(home, pol, []policy.SavedModel{qwen})
	if err != nil || len(users) != 2 || users[0].Kind != KindSchedule || users[0].ID != sc.ID || users[1].Kind != KindAgent || users[1].ID != a.Name || users[1].Model != qwen.ID {
		t.Fatalf("users %+v (%v)", users, err)
	}

	remove := func(newDefault string) error { _, err := pol.RemoveModel(qwen.ID, newDefault); return err }
	_, err = Remove(context.Background(), home, pol, path, []policy.SavedModel{qwen}, "", persistent.ClientCLI, remove)
	var used *InUseError
	if !errors.As(err, &used) || len(used.Users) != 2 || !strings.Contains(err.Error(), "schedule Morning brief ("+sc.ID+")") {
		t.Fatalf("refusal: %v", err)
	}
	if _, ok := reload(t, path).FindModel("Qwen"); !ok {
		t.Fatal("the refused removal removed the model")
	}

	r, err := Remove(context.Background(), home, pol, path, []policy.SavedModel{qwen}, "Llama", persistent.ClientCLI, remove)
	if err != nil || len(r.Moved) != 2 || r.MovedTo.ID != llama.ID || len(r.Removed) != 1 || r.Removed[0].ID != qwen.ID {
		t.Fatalf("removal %+v (%v)", r, err)
	}
	if got, err := schedule.StoreIn(home).Get(sc.ID); err != nil || got.Run.Model != llama.ID {
		t.Fatalf("the schedule names %q, want Llama's %q (%v)", got.Run.Model, llama.ID, err)
	}
	if got := configured(t, home, a.Name); got != llama.ID {
		t.Fatalf("the agent names %q, want Llama's %q", got, llama.ID)
	}
	if _, ok := reload(t, path).FindModel("Qwen"); ok {
		t.Fatal("the removal was not written")
	}
}

// The saved model moved to is never one being removed, nor one the computer
// lacks; nothing changes when it is.
func TestAMoveTargetIsNeitherRemovedNorMissing(t *testing.T) {
	home, path, pol := lab(t)
	qwen, llama := savedModel(t, pol, "Qwen"), savedModel(t, pol, "Llama")
	scheduleOn(t, home, qwen.ID)
	provider := qwen.Provider
	remove := func(newDefault string) error { _, err := pol.RemoveProvider(provider, newDefault); return err }

	for _, to := range []string{"Llama", "Gemma"} {
		_, err := Remove(context.Background(), home, pol, path, []policy.SavedModel{qwen, llama}, to, persistent.ClientCLI, remove)
		var ce *engine.ClassifiedError
		if !errors.As(err, &ce) || ce.Code != engine.ExitUsage {
			t.Fatalf("move to %s: %v", to, err)
		}
	}
	if _, ok := reload(t, path).FindModel("Qwen"); !ok {
		t.Fatal("a refused move removed the model")
	}
}

// Removing the default needs the new default; the move target is it.
func TestRemovingTheDefaultMakesTheMoveTargetTheDefault(t *testing.T) {
	home, path, pol := lab(t)
	def, llama := pol.DefaultModel(), savedModel(t, pol, "Llama")
	remove := func(newDefault string) error { _, err := pol.RemoveModel(def.ID, newDefault); return err }
	if _, err := Remove(context.Background(), home, pol, path, []policy.SavedModel{def}, "", persistent.ClientCLI, remove); err == nil || !strings.Contains(err.Error(), "is the default") {
		t.Fatalf("removing the default with no new one: %v", err)
	}
	if _, err := Remove(context.Background(), home, pol, path, []policy.SavedModel{def}, "Llama", persistent.ClientCLI, remove); err != nil {
		t.Fatal(err)
	}
	if got := reload(t, path).DefaultModel(); got.ID != llama.ID {
		t.Fatalf("default %+v, want Llama", got)
	}
}

// A schedule on a harness whose block a saved model was read from, naming
// no model, runs on that saved model, so it is one of its users.
func TestAScheduleOnAHarnessBlocksHarnessUsesItsSavedModel(t *testing.T) {
	home := t.TempDir()
	pol := policy.Default()
	pol.Harness = map[string]policy.HarnessPolicy{"claude": {Model: "claude-opus-5"}}
	sm, ok := pol.HarnessModel("claude")
	if !ok {
		t.Fatal("no saved model read from the claude block")
	}
	sc := &schedule.Schedule{Name: "Nightly", Local: home, Run: schedule.RunSpec{OutDir: home, Adapter: "claude"}, Created: time.Now()}
	if err := schedule.StoreIn(home).Create(sc, nil); err != nil {
		t.Fatal(err)
	}
	users, err := Users(home, pol, []policy.SavedModel{sm})
	if err != nil || len(users) != 1 || users[0].ID != sc.ID || users[0].Model != sm.ID {
		t.Fatalf("users %+v (%v)", users, err)
	}
}

// Each provider's state says whether its credential is here, never what it
// is: a key in neither its variable nor the keychain is missing, a set
// variable is ready, a sign-in not kept or broken needs a sign-in, Agentry
// credits need this computer signed in to the registry, and an endpoint that
// takes no key is ready.
func TestAProvidersStateSaysWhetherItsCredentialIsHere(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTRY_NO_KEYRING", "1")
	t.Setenv("OPENAI_API_KEY", "")
	signedOut := func(string) (string, error) { return "", nil }
	signedIn := func(string) (string, error) { return "tok", nil }

	key := policy.Provider{APIKeyEnv: "OPENAI_API_KEY", APIKeySource: "keyring"}
	if s := ProviderState(home, "openai", key, signedOut); s.Code != KeyMissing || s.Words != "key missing" {
		t.Fatalf("no key: %+v", s)
	}
	t.Setenv("OPENAI_API_KEY", "sk-test")
	if s := ProviderState(home, "openai", key, signedOut); s.Code != Ready || s.Words != "key set" || strings.Contains(s.Kind+s.Words, "sk-test") {
		t.Fatalf("variable set: %+v", s)
	}

	plan := policy.Provider{APIKeyKind: policy.KeyKindChatGPTPlan}
	if s := ProviderState(home, "openai", plan, signedOut); s.Code != NeedsSignIn || s.Vendor != modelsignin.ChatGPT {
		t.Fatalf("no sign-in kept: %+v", s)
	}

	credits := policy.Provider{BaseURL: "https://registry.example" + policy.AgentryInferencePath}
	if s := ProviderState(home, policy.ProviderAgentry, credits, signedOut); s.Code != NeedsSignIn {
		t.Fatalf("credits signed out: %+v", s)
	}
	if s := ProviderState(home, policy.ProviderAgentry, credits, signedIn); s.Code != Ready || s.Account != "https://registry.example" {
		t.Fatalf("credits signed in: %+v", s)
	}

	_, keyless, _, err := CustomProvider("Lab", "http://127.0.0.1:9/v1", false)
	if err != nil {
		t.Fatal(err)
	}
	if s := ProviderState(home, "custom-lab", keyless, signedOut); s.Code != Ready || s.Kind != "takes no key" {
		t.Fatalf("keyless endpoint: %+v", s)
	}
}

// An endpoint of the person's own is a provider named for it, its address
// its proxy's one host; with a key kept in the keychain it names a variable
// of its own name, and without one it takes none. Plain http off this
// computer is said, never refused.
func TestAnEndpointOfYourOwnIsAProviderNamedForIt(t *testing.T) {
	id, pr, warn, err := CustomProvider("Home GPU", "http://gpu-box.lan:8000/v1", true)
	if err != nil {
		t.Fatal(err)
	}
	if id != "custom-home-gpu" || pr.Name != "Home GPU" || pr.APIKeyEnv != "CUSTOM_HOME_GPU_API_KEY" || pr.APIKeySource != "keyring" || len(pr.ProxyAllow) != 1 || pr.ProxyAllow[0] != "gpu-box.lan:8000" {
		t.Fatalf("provider %s %+v", id, pr)
	}
	if !strings.Contains(warn, "plain http") {
		t.Fatalf("warning %q", warn)
	}
	if _, keyless, _, err := CustomProvider("Lab", "https://lab.example/v1", false); err != nil || keyless.APIKeyEnv != "" || keyless.APIKeySource != "" {
		t.Fatalf("keyless %+v (%v)", keyless, err)
	}
	if _, _, _, err := CustomProvider("!!", "https://lab.example/v1", false); err == nil {
		t.Fatal("a name with no letter or digit was taken")
	}
	if _, _, _, err := CustomProvider("Lab", "", false); err == nil {
		t.Fatal("an endpoint with no address was taken")
	}
}

// Agentry Helper, which runs only in Agentry Desktop, can be put on a saved
// model: a removal of that model is refused while it names it, and moving
// it goes through, whichever client asks.
func TestARemovalIsRefusedWhileAgentryHelperNamesTheModelAndMovesIt(t *testing.T) {
	home, path, pol := lab(t)
	qwen, llama := savedModel(t, pol, "Qwen"), savedModel(t, pol, "Llama")
	helper, err := persistent.Lookup("main")
	if err != nil || !helper.DesktopOnly {
		t.Fatalf("Agentry Helper is %+v", helper)
	}
	id := qwen.ID
	if _, _, err := persistent.Configure(context.Background(), home, helper, persistent.ClientDesktop, persistent.Change{Model: &id}); err != nil {
		t.Fatalf("Agentry Helper put on a saved model: %v", err)
	}
	users, err := Users(home, pol, []policy.SavedModel{qwen})
	if err != nil || len(users) != 1 || users[0].Kind != KindAgent || users[0].ID != "main" || users[0].Name != "Agentry Helper" {
		t.Fatalf("users %+v (%v)", users, err)
	}
	remove := func(newDefault string) error { _, err := pol.RemoveModel(qwen.ID, newDefault); return err }
	var used *InUseError
	if _, err := Remove(context.Background(), home, pol, path, []policy.SavedModel{qwen}, "", persistent.ClientCLI, remove); !errors.As(err, &used) {
		t.Fatalf("the removal was not refused: %v", err)
	}
	if _, err := Remove(context.Background(), home, pol, path, []policy.SavedModel{qwen}, "Llama", persistent.ClientCLI, remove); err != nil {
		t.Fatalf("the move: %v", err)
	}
	if got := configured(t, home, "main"); got != llama.ID {
		t.Fatalf("Agentry Helper names %q, want Llama's %q", got, llama.ID)
	}
}
