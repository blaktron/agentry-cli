package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runhost/remote"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
	"github.com/blaktron/agentry-cli/pkg/schedule"
)

// modelsLab is a paired runhost "lab" whose own policy is a file apart from
// this computer's, and a computer with two saved models of its own on two
// providers: Qwen, at an endpoint of the person's own that takes no key,
// and GPT, on OpenAI, its key in this computer's keychain. offered is what
// the OpenAI listing offers, to this computer and the runhost alike.
type modelsLab struct {
	srv      *runhostServer
	home     string
	rhPolicy string
	offered  *[]string
	lab      string
}

func newModelsLab(t *testing.T) *modelsLab {
	t.Helper()
	srv, home := newRunOnLab(t)
	t.Setenv("AGENTRY_NO_KEYRING", "")
	rh := filepath.Join(srv.dir, "runhost-policy.yaml")
	oldPath, oldList := runhostPolicyPath, listModelsToCheck
	t.Cleanup(func() { runhostPolicyPath, listModelsToCheck = oldPath, oldList })
	runhostPolicyPath = func() (string, error) { return rh, nil }
	own := policy.Default()
	own.Docker.Runtime, own.Docker.GatewayRuntime = runhost.RunscRuntime, runhost.RunscUDSRuntime
	if err := policy.Save(rh, own); err != nil {
		t.Fatal(err)
	}
	offered := []string{"gpt-5.5"}
	listModelsToCheck = func(ctx context.Context, provider, base, key string) (*setup.Catalog, error) {
		if provider == "openai" {
			if key != "sk-test-openai" {
				return nil, os.ErrPermission
			}
			cat := &setup.Catalog{}
			for _, id := range offered {
				cat.Models = append(cat.Models, setup.Model{ID: id})
			}
			return cat, nil
		}
		return setup.ListModelsToCheck(ctx, provider, base, key)
	}
	lab := modelEndpoint(t, 32768, "qwen3")
	cli(t, "providers", "add", "custom", "--name", "Lab", "--base-url", lab.URL+"/v1")
	cli(t, "models", "add", "Qwen", "--provider", "custom-lab", "--model", "qwen3")
	if _, errOut, err := runCLI(t, strings.NewReader("sk-test-openai\n"), "providers", "add", "openai", "--key-stdin"); err != nil {
		t.Fatalf("providers add openai: %v\n%s", err, errOut)
	}
	cli(t, "models", "add", "GPT", "--provider", "openai", "--model", "gpt-5.5")
	// What the runhost keeps from here on is what it was sent: this
	// computer's keychain is the test's one keychain, and the key is read
	// from it now, so it starts empty for the runhost.
	return &modelsLab{srv: srv, home: home, rhPolicy: rh, offered: &offered, lab: lab.URL}
}

// keychainHolds says whether the test's keychain holds value under any
// model key's account.
func keychainHolds(t *testing.T, provider string) string {
	t.Helper()
	v, err := keyring.Get(modelkey.Service, modelkey.Account(provider))
	if err != nil {
		return ""
	}
	return v
}

// forgetDeviceKey takes the computer's OpenAI key out of the keychain once
// it is read, so the keychain then holds only what the runhost keeps.
func forgetDeviceKey(t *testing.T) {
	t.Helper()
	if err := modelkey.Delete("openai"); err != nil {
		t.Fatal(err)
	}
}

// A saved model made available on a runhost is checked there and kept
// there, under this computer's id, with only its own credential: an
// endpoint that takes no key is given its address alone, a key goes to the
// runhost's store and never into its policy, and nothing of another saved
// model's is sent. A model the runhost's check refuses, or cannot
// reach, is not kept, and what was there before stays.
func TestASavedModelIsMadeAvailableOnARunhostAndCheckedThere(t *testing.T) {
	ml := newModelsLab(t)
	_, devicePol := readPolicy(t, mustDefaultPath(t))
	qwen, _ := devicePol.FindModel("Qwen")

	out := cli(t, "runhost", "model", "lab")
	if !strings.Contains(out, "holds no saved model") {
		t.Fatalf("a runhost given none:\n%s", out)
	}
	forgetDeviceKey(t)
	out = cli(t, "runhost", "model", "lab", "--add", "Qwen", "--yes")
	if !strings.Contains(out, `holds the saved model "Qwen" (custom-lab/qwen3), its default`) {
		t.Fatalf("add said:\n%s", out)
	}
	_, rh := readPolicy(t, ml.rhPolicy)
	if got, ok := rh.FindModel("Qwen"); !ok || got.ID != qwen.ID || rh.Default != qwen.ID || got.ContextWindow != 32768 {
		t.Fatalf("the runhost's list %+v (default %q)", rh.Models, rh.Default)
	}
	if v := keychainHolds(t, "openai"); v != "" {
		t.Fatal("making Qwen available sent GPT's key too")
	}

	// GPT's key is read here (its variable wins over the keychain) and
	// kept in the runhost's store, the test's one keychain, alone.
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	if _, errOut, err := runCLI(t, nil, "runhost", "model", "lab", "--add", "GPT"); err == nil {
		t.Fatalf("a key kept with nobody to agree:\n%s", errOut)
	}
	cli(t, "runhost", "model", "lab", "--add", "GPT", "--yes")
	text, rh := readPolicy(t, ml.rhPolicy)
	if strings.Contains(text, "sk-test-openai") {
		t.Fatalf("the key is in the runhost's policy:\n%s", text)
	}
	if gpt, ok := rh.FindModel("GPT"); !ok || gpt.Provider != "openai" || rh.Providers["openai"].APIKeySource != modelkey.SourceKeyring {
		t.Fatalf("the runhost's GPT: %+v %+v", rh.Models, rh.Providers)
	}
	if v := keychainHolds(t, "openai"); v != "sk-test-openai" {
		t.Fatalf("the runhost's store holds %q for OpenAI", v)
	}
	if v := keychainHolds(t, "anthropic"); v != "" {
		t.Fatal("the runhost holds a key of a saved model never made available there")
	}
	out = cli(t, "runhost", "list")
	if !strings.Contains(out, "*Qwen, GPT") {
		t.Fatalf("runhost list:\n%s", out)
	}

	// The runhost's own check refuses a model its listing does not offer,
	// and keeps nothing of it.
	*ml.offered = []string{"gpt-5.5", "gpt-5.5-pro"}
	cli(t, "models", "add", "Pro", "--provider", "openai", "--model", "gpt-5.5-pro")
	*ml.offered = []string{"gpt-5.5"}
	_, _, err := runCLI(t, nil, "runhost", "model", "lab", "--add", "Pro", "--yes")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), `"Pro" was not kept on this runhost`) || !strings.Contains(err.Error(), "does not offer") {
		t.Fatalf("refusal: %v", err)
	}
	if _, rh = readPolicy(t, ml.rhPolicy); len(rh.Models) != 2 {
		t.Fatalf("a refused model was kept: %+v", rh.Models)
	}
	if v := keychainHolds(t, "openai"); v != "sk-test-openai" {
		t.Fatalf("a refused check left %q for OpenAI", v)
	}

	// An endpoint the runhost cannot reach is refused there too.
	gone := modelEndpoint(t, 4096, "tiny")
	cli(t, "providers", "add", "custom", "--name", "Gone", "--base-url", gone.URL+"/v1")
	cli(t, "models", "add", "Tiny", "--provider", "custom-gone", "--model", "tiny")
	gone.Close()
	_, _, err = runCLI(t, nil, "runhost", "model", "lab", "--add", "Tiny", "--yes")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "could not check") {
		t.Fatalf("an unreachable endpoint: %v", err)
	}
	if _, rh = readPolicy(t, ml.rhPolicy); len(rh.Models) != 2 || len(rh.Providers) != 2 {
		t.Fatalf("an unreachable model was kept: %+v %+v", rh.Models, rh.Providers)
	}

	// Taking GPT away takes its key from the runhost's store too.
	cli(t, "runhost", "model", "lab", "--default", "GPT")
	_, _, err = runCLI(t, nil, "runhost", "model", "lab", "--remove", "GPT")
	wantClassified(t, err, engine.ExitUsage)
	out = cli(t, "runhost", "model", "lab", "--remove", "GPT", "--move-to", "Qwen")
	if !strings.Contains(out, "key is removed") || !strings.Contains(out, "default: Qwen") {
		t.Fatalf("remove said:\n%s", out)
	}
	if v := keychainHolds(t, "openai"); v != "" {
		t.Fatal("the removed model's key stayed in the runhost's store")
	}
}

func mustDefaultPath(t *testing.T) string {
	t.Helper()
	p, err := policy.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A one-off run with --on carries the key of the saved model it names and
// no other: a run on Qwen carries none, a run on GPT carries GPT's;
// and its receipt's policy names the saved model.
func TestARunOnARunhostCarriesOnlyItsSavedModelsKey(t *testing.T) {
	ml := newModelsLab(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	envSeen := func() string {
		b, _ := os.ReadFile(filepath.Join(ml.srv.dir, "env")) //nolint:gosec // the fake CLI's record
		return string(b)
	}
	run := func(model string) {
		t.Helper()
		_ = os.Remove(filepath.Join(ml.srv.dir, "env"))
		_, errOut, err := runCLI(t, strings.NewReader("go"), "--registry", fr.url(), "run", "acme/demo@1.0.0", "--on", "lab", "--model", model,
			"--mode", "autonomous", "--allow-unsigned", "--yes", "--out", t.TempDir())
		if err != nil {
			t.Fatalf("run --on --model %s: %v\n%s", model, err, errOut)
		}
		if !strings.Contains(errOut, `on this computer's saved model "`+model+`"`) {
			t.Fatalf("the run did not say its saved model:\n%s", errOut)
		}
	}
	run("Qwen")
	if env := envSeen(); strings.Contains(env, "sk-test-openai") || strings.Contains(env, "OPENAI_API_KEY") {
		t.Fatalf("a run on Qwen carried GPT's key:\n%s", env)
	}
	run("GPT")
	if env := envSeen(); !strings.Contains(env, "OPENAI_API_KEY=sk-test-openai") {
		t.Fatalf("a run on GPT did not carry its key:\n%s", env)
	}
	_, _, err := runCLI(t, strings.NewReader("go"), "--registry", fr.url(), "run", "acme/demo@1.0.0", "--on", "lab", "--model", "Nope",
		"--mode", "autonomous", "--allow-unsigned", "--yes", "--out", t.TempDir())
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), `no saved model "Nope"`) {
		t.Fatalf("a missing model: %v", err)
	}
}

// sealLab gives the lab runhost a systemd that seals its store's key, so it
// takes schedules.
func sealLab(t *testing.T, srv *runhostServer) {
	t.Helper()
	oldEnc, oldDec, oldDir, oldNow := schedule.EncryptCredential, runhostDecryptCredential, runhostSchedulesDir, runhostScheduleNow
	t.Cleanup(func() {
		schedule.EncryptCredential, runhostDecryptCredential, runhostSchedulesDir, runhostScheduleNow = oldEnc, oldDec, oldDir, oldNow
	})
	schedule.EncryptCredential = func(name string, plain []byte, out string) error {
		return os.WriteFile(out, append([]byte(name+":"), plain...), 0o600)
	}
	runhostDecryptCredential = func(name, file string) ([]byte, error) {
		b, err := os.ReadFile(file) //nolint:gosec // the test's own file
		if err != nil {
			return nil, err
		}
		v, _ := bytes.CutPrefix(b, []byte(name+":"))
		return v, nil
	}
	runhostSystemdVersion = func() int { return 257 }
	runhostSchedulesDir = filepath.Join(srv.dir, "schedules")
}

// A schedule on a runhost names one of the runhost's own saved models: one
// it does not hold is refused by name before anything is asked, and one it
// holds is kept by its id, with no key of this computer's kept beside it;
// each run reads the model and its key from the runhost's list then. Taking
// that model away is refused while the schedule names it, and --move-to
// moves the schedule.
func TestAScheduleOnARunhostNamesOneOfItsSavedModels(t *testing.T) {
	ml := newModelsLab(t)
	sealLab(t, ml.srv)
	cli(t, "runhost", "model", "lab", "--add", "Qwen", "--yes")
	cli(t, "runhost", "model", "lab", "--add", "GPT", "--yes")
	_, rh := readPolicy(t, ml.rhPolicy)
	gpt, _ := rh.FindModel("GPT")
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))

	_, stderr, err := runCLI(t, nil, "--registry", fr.url(), "schedule", "add", "acme/demo@1.0.0", "--on", "lab", "--model", "Llama",
		"--daily", "07:00", "--zone", "UTC", "--allow-unsigned", "--yes")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), `runhost lab holds no saved model "Llama"`) || strings.Contains(stderr, "Its store keeps") {
		t.Fatalf("a model the runhost does not hold: %v\n%s", err, stderr)
	}
	_, stderr, err = runCLI(t, nil, "--registry", fr.url(), "schedule", "add", "acme/demo@1.0.0", "--on", "lab", "--model", "gpt",
		"--daily", "07:00", "--zone", "UTC", "--allow-unsigned", "--yes")
	if err != nil {
		t.Fatalf("schedule add --on --model: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, `lab's saved model "GPT" (openai/gpt-5.5)`) || strings.Contains(stderr, "model-key") {
		t.Fatalf("the consent:\n%s", stderr)
	}
	list, _ := runhostScheduleList()
	if len(list) != 1 || list[0].Model != gpt.ID {
		t.Fatalf("the runhost's schedules %+v", list)
	}
	sc := list[0]
	// The schedule's kept job carries no key; its run reads GPT's from the
	// runhost's store.
	j, _, code, err := runhostScheduledJob(context.Background(), schedule.Schedule{ID: sc.ID})
	if err != nil {
		t.Fatalf("the occurrence's job: %d %v", code, err)
	}
	if j.Run.ModelKey != "sk-test-openai" || j.Run.Model.SavedModelID != gpt.ID || j.Run.Model.Provider != "openai" {
		t.Fatalf("the occurrence's model %+v", j.Run.Model)
	}

	_, _, err = runCLI(t, nil, "runhost", "model", "lab", "--remove", "GPT")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "schedule") || !strings.Contains(err.Error(), sc.ID) {
		t.Fatalf("removal while a schedule names it: %v", err)
	}
	cli(t, "runhost", "model", "lab", "--remove", "GPT", "--move-to", "Qwen")
	list, _ = runhostScheduleList()
	qwen, _ := rh.FindModel("Qwen")
	if list[0].Model != qwen.ID {
		t.Fatalf("the schedule was not moved: %+v", list[0])
	}
	if j, _, _, err = runhostScheduledJob(context.Background(), schedule.Schedule{ID: sc.ID}); err != nil || j.Run.ModelKey != "" || j.Run.Model.SavedModelName != "Qwen" {
		t.Fatalf("the moved schedule's job: %+v %v", j, err)
	}
}

// The runhost's own run of a job naming one of its saved models takes the
// model and its key from the runhost; one it no longer holds, or holds no
// key for, is refused by name and never run on the default.
func TestAJobOnARunhostsSavedModelIsRefusedByNameWhenItIsGone(t *testing.T) {
	newRunhostServer(t)
	t.Setenv("AGENTRY_NO_KEYRING", "")
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	path := mustDefaultPath(t)
	pol, _, err := policy.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	pol.ResetModels(policy.Model{})
	sm, err := pol.PutModel("openai", policy.Provider{APIKeyEnv: "OPENAI_API_KEY", APIKeySource: modelkey.SourceKeyring, ProxyAllow: []string{"api.openai.com:443"}},
		policy.SavedModel{ID: "0a0b0c0d", Name: "GPT", Model: "gpt-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.Save(path, pol); err != nil {
		t.Fatal(err)
	}
	j, archive := unsignedJob(t, "1111111111111111")
	j.Run.Model, j.Run.ModelKey, j.Run.RunhostModel = job.Model{}, "", sm.ID
	a := sendJob(t, j, archive)
	if a.code != engine.ExitPolicy || !strings.Contains(a.err.Error(), `"GPT" cannot be used on this runhost`) {
		t.Fatalf("no key on the runhost: %d %v", a.code, a.err)
	}
	if err := modelkey.Store("openai", "sk-runhost-openai"); err != nil {
		t.Fatal(err)
	}
	j.ID = "2222222222222222"
	if a = sendJob(t, j, archive); a.code != 0 {
		t.Fatalf("a job on GPT: %d %v %v", a.code, a.err, a.out)
	}
	env, _ := os.ReadFile(filepath.Join(os.Getenv("RUNHOST_TEST_DIR"), "env")) //nolint:gosec // the fake CLI's record
	if !strings.Contains(string(env), "OPENAI_API_KEY=sk-runhost-openai") {
		t.Fatalf("the run's environment:\n%s", env)
	}
	j.ID, j.Run.RunhostModel = "3333333333333333", "deadbeef"
	a = sendJob(t, j, archive)
	if a.code != engine.ExitPolicy || !strings.Contains(a.err.Error(), "no longer holds") || !strings.Contains(a.err.Error(), "never takes the default") {
		t.Fatalf("a model the runhost does not hold: %d %v", a.code, a.err)
	}
}

// A runhost refuses a saved model's credential that does not go with its
// kind: a key for a model a sign-in pays for, or for one paid with Agentry
// credits, so nothing of a sign-in's is ever carried there; and a pasted
// Claude subscription token, which no runhost runs.
func TestARunhostTakesNoCredentialThatDoesNotGoWithItsSavedModel(t *testing.T) {
	newRunhostServer(t)
	t.Setenv("AGENTRY_NO_KEYRING", "")
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	put := func(doc remote.PutModel) (int, error) {
		b, _ := json.Marshal(doc)
		var out bytes.Buffer
		fw := runhost.NewFrameWriter(&out)
		return runhostStoreVerb(context.Background(), runhost.Request{Verb: "store", Args: []string{"put-saved-model"}}, bytes.NewReader(b), fw)
	}
	for _, doc := range []remote.PutModel{
		{ID: "0a0b0c0d", Name: "ChatGPT", ProviderID: "openai", Model: "gpt-5.5", Key: "sk-smuggled",
			Provider: remote.ModelProvider{APIKeyKind: policy.KeyKindChatGPTPlan, ProxyAllow: []string{}}},
		{ID: "0a0b0c0e", Name: "Credits", ProviderID: "agentry", Model: "agentry", Key: "sk-smuggled",
			Provider: remote.ModelProvider{BaseURL: "https://registry.test/api/v1/inference", ProxyAllow: []string{}}},
		{ID: "0a0b0c0f", Name: "Pasted", ProviderID: "anthropic", Model: "claude-x", Key: "sk-ant-oat-x",
			Provider: remote.ModelProvider{APIKeyEnv: "ANTHROPIC_API_KEY", APIKeyKind: policy.KeyKindSubscription, ProxyAllow: []string{}}},
		{ID: "0a0b0c10", Name: "Keyed", ProviderID: "openai", Model: "gpt-5.5", Key: "two words",
			Provider: remote.ModelProvider{APIKeyEnv: "OPENAI_API_KEY", ProxyAllow: []string{}}},
		{ID: "0a0b0c11", Name: "Loader", ProviderID: "openai", Model: "gpt-5.5", Key: "sk-x",
			Provider: remote.ModelProvider{APIKeyEnv: "LD_PRELOAD", ProxyAllow: []string{}}},
	} {
		code, err := put(doc)
		if code != engine.ExitUsage {
			t.Errorf("%s: %d %v", doc.Name, code, err)
		}
	}
	// A sign-in's model with no sign-in kept here is refused by the check,
	// and nothing is kept.
	code, err := put(remote.PutModel{ID: "0a0b0c12", Name: "ChatGPT", ProviderID: "openai", Model: "gpt-5.5",
		Provider: remote.ModelProvider{APIKeyKind: policy.KeyKindChatGPTPlan, ProxyAllow: []string{"chatgpt.com:443"}}})
	if code != engine.ExitPolicy || !strings.Contains(err.Error(), "keeps no chatgpt sign-in") {
		t.Fatalf("a sign-in the runhost does not keep: %d %v", code, err)
	}
	if pol, _, _ := runhostModelsPolicy(); runhostHoldsModels(pol) {
		t.Fatal("a refused model was kept")
	}
	for _, p := range []string{"openai", "agentry", "anthropic"} {
		if v := keychainHolds(t, p); v != "" {
			t.Errorf("the runhost kept a key for %s", p)
		}
	}
}

// A sign-in's and a credits' saved model are sent to a runhost with no
// credential of this computer's at all: the runhost signs in, or is given
// a credits token, of its own.
func TestASignInsSavedModelIsSentWithNothingOfTheSignIn(t *testing.T) {
	for _, m := range []policy.Model{
		{Provider: "openai", APIKeyKind: policy.KeyKindChatGPTPlan},
		{Provider: "anthropic", APIKeyKind: policy.KeyKindClaudeSignIn},
		{Provider: policy.ProviderAgentry},
	} {
		if k := runhostModelKind(m); k == remote.ModelKindKey {
			t.Errorf("%+v is sent a key (kind %s)", m, k)
		}
	}
	if k := runhostModelKind(policy.Model{Provider: "custom", BaseURL: "http://lab/v1"}); k != remote.ModelKindNoKey {
		t.Errorf("a keyless endpoint is %s", k)
	}
}

// fakeModelsCaller answers the store verb's models with a fixed list.
type fakeModelsCaller struct{ list remote.Models }

func (f fakeModelsCaller) Call(_ context.Context, verb string, args []string, _ func(io.Writer) error, _ func(runhost.Frame) error) (runhost.Answer, error) {
	if verb != "store" || !slices.Equal(args, []string{"models"}) {
		return runhost.Answer{Code: 2, Error: "unexpected"}, nil
	}
	b, _ := json.Marshal(f.list)
	return runhost.Answer{Result: b}, nil
}

// agent configure --on --model names one of the runhost's saved models: a
// name is sent as its id, default stays default, and a name the runhost
// does not hold is refused here, by name.
func TestAnAgentOnARunhostNamesOneOfItsSavedModels(t *testing.T) {
	isolateRunnerHome(t)
	c := fakeModelsCaller{list: remote.Models{Default: "0a0b0c0d", Models: []remote.SavedModel{{ID: "0a0b0c0d", Name: "My Qwen"}, {ID: "1a0b0c0d", Name: "GPT"}}}}
	got, err := runhostAgentModelArg(context.Background(), c, "lab", []string{"assistant", "--model=gpt"})
	if err != nil || !slices.Equal(got, []string{"assistant", "--model=1a0b0c0d"}) {
		t.Fatalf("by name: %v %v", got, err)
	}
	if got, err = runhostAgentModelArg(context.Background(), c, "lab", []string{"assistant", "--model=my qwen"}); err != nil || got[1] != "--model=0a0b0c0d" {
		t.Fatalf("a name with a space: %v %v", got, err)
	}
	if got, err = runhostAgentModelArg(context.Background(), c, "lab", []string{"assistant", "--model=default"}); err != nil || got[1] != "--model=default" {
		t.Fatalf("default: %v %v", got, err)
	}
	_, err = runhostAgentModelArg(context.Background(), c, "lab", []string{"assistant", "--model=Claude"})
	wantClassified(t, err, engine.ExitUsage)
	if !strings.Contains(err.Error(), `runhost lab holds no saved model "Claude"`) {
		t.Fatalf("refusal: %v", err)
	}
}

// Taking away a saved model one of the runhost's agents names is refused,
// naming it, and --move-to moves the agent; the last saved model is not
// taken away while an agent follows the default.
func TestRemovingARunhostsSavedModelAnAgentNamesIsRefused(t *testing.T) {
	ml := newModelsLab(t)
	cli(t, "runhost", "model", "lab", "--add", "Qwen", "--yes")
	cli(t, "runhost", "model", "lab", "--add", "GPT", "--yes")
	_, rh := readPolicy(t, ml.rhPolicy)
	gpt, _ := rh.FindModel("GPT")
	qwen, _ := rh.FindModel("Qwen")
	cfg, err := persistent.LoadConfig(ml.home, "assistant")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model = gpt.ID
	if err := persistent.SaveConfig(ml.home, "assistant", cfg); err != nil {
		t.Fatal(err)
	}
	out := cli(t, "runhost", "model", "lab")
	if !strings.Contains(out, "named by agent") {
		t.Fatalf("the list does not say what names GPT:\n%s", out)
	}
	_, _, err = runCLI(t, nil, "runhost", "model", "lab", "--remove", "GPT")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "(assistant)") || !strings.Contains(err.Error(), "--move-to") {
		t.Fatalf("refusal: %v", err)
	}
	cli(t, "runhost", "model", "lab", "--remove", "GPT", "--move-to", "Qwen")
	if got := configuredModel(t, ml.home, "assistant"); got != qwen.ID {
		t.Fatalf("the agent names %q after the move, want %q", got, qwen.ID)
	}
	cfg, _ = persistent.LoadConfig(ml.home, "assistant")
	cfg.Model = ""
	if err := persistent.SaveConfig(ml.home, "assistant", cfg); err != nil {
		t.Fatal(err)
	}
	_, _, err = runCLI(t, nil, "runhost", "model", "lab", "--remove", "Qwen")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "only one") || !strings.Contains(err.Error(), "follow its default") {
		t.Fatalf("the last model while an agent follows the default: %v", err)
	}
}

// A provider's key is kept in the runhost's store under an entry the store
// takes, however long its custom-<slug> id is.
func TestALongProvidersKeyHasAStoreEntry(t *testing.T) {
	for _, p := range []string{"anthropic", "custom-lab", "custom-the-very-long-name-of-an-endpoint-of-mine", "custom-a.b_c"} {
		for _, account := range []string{modelkey.Account(p), "stored:" + modelkey.Account(p)} {
			name, err := runhostKeychainEntry(account)
			if err != nil || !storeEntryRE.MatchString(name) {
				t.Errorf("%s: %q %v", account, name, err)
			}
		}
	}
	if name, _ := runhostKeychainEntry(modelkey.Account("anthropic")); name != "model-key-anthropic" {
		t.Errorf("a vendor's key moved to %q", name)
	}
}
