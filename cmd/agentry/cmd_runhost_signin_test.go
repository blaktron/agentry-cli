package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runhost/job"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/trust"
)

// The runhost's job names its image adapters as the runner does.
func TestTheRunhostsAdaptersAreTheRunnersContainerHarnesses(t *testing.T) {
	for _, name := range job.ImageAdapters {
		if _, err := adapter.Get(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if job.AdapterClaudeImage != adapter.ClaudeImageAdapterName || job.AdapterCodexImage != adapter.CodexImageAdapterName ||
		job.AdapterCopilot != adapter.CopilotImageAdapterName || job.AdapterDockerAgent != adapter.DefaultName {
		t.Fatal("the job's adapter names drifted from the runner's")
	}
	if a, _, _ := job.RunhostAdapter(adapter.ClaudeAdapterName); a != adapter.ClaudeImageAdapterName {
		t.Fatalf("claude runs as %q", a)
	}
	if a, _, _ := job.RunhostAdapter(adapter.CodexAdapterName); a != adapter.CodexImageAdapterName {
		t.Fatalf("codex runs as %q", a)
	}
}

// The runhost keeps its sign-ins' refresh tokens in its encrypted store, and
// nothing else lands there through the keychain.
func TestARunhostKeepsSignInSecretsInItsStore(t *testing.T) {
	newRunhostServer(t)
	k := runhostKeychain{}
	if err := k.Set("model-signin:chatgpt", `{"refreshToken":"PROBE-RT"}`); err != nil {
		t.Fatal(err)
	}
	if v, err := k.Get("model-signin:chatgpt"); err != nil || v != `{"refreshToken":"PROBE-RT"}` {
		t.Fatalf("got %q %v", v, err)
	}
	b, err := os.ReadFile(filepath.Join(runhostStoreDir, "signin-chatgpt.entry"))
	if err != nil || strings.Contains(string(b), "PROBE-RT") {
		t.Fatalf("the entry is not ciphertext: %q %v", b, err)
	}
	for _, other := range []string{"connection:x", "model-signin:openrouter", "model-signin:../x", "provider:anthropic"} {
		if err := k.Set(other, "v"); err == nil {
			t.Errorf("%s kept in the store", other)
		}
	}
	if err := k.Delete("model-signin:chatgpt"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Get("model-signin:chatgpt"); err == nil {
		t.Fatal("deleted, still there")
	}
}

// A job whose model is a plan's sign-in runs on the one the runhost keeps,
// in the vendor's image: refused while it keeps none, and run with the
// adapter the job names and no model key.
func TestASignInJobRunsOnTheRunhostsSignIn(t *testing.T) {
	srv := newRunhostServer(t)
	prev := connectionKeys
	connectionKeys = func() connection.Keychain { return runhostKeychain{} }
	t.Cleanup(func() { connectionKeys = prev })
	claude := func(j *job.Job) {
		j.Run.ModelKey, j.Run.Adapter = "", job.AdapterClaudeImage
		j.Run.Model = job.Model{Provider: "anthropic", Name: "sonnet", APIKeyKind: policy.KeyKindClaudeSignIn,
			ProxyAllow: policy.SignInProxyAllow(policy.KeyKindClaudeSignIn)}
	}
	j, archive := unsignedJob(t, "c1a0de0000000001")
	claude(j)
	if ans := sendJob(t, j, archive); ans.code != engine.ExitPolicy || ans.err == nil || !strings.Contains(ans.err.Error(), "--sign-in claude") {
		t.Fatalf("a Claude job with no sign-in: %d %v", ans.code, ans.err)
	}
	home, err := runnerHome()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := modelsignin.Open(home, nil).Put(modelsignin.Claude, modelsignin.Record{ConfigDir: "agentry-claude-signin", Label: "person@example.com"}, modelsignin.Grant{}); err != nil {
		t.Fatal(err)
	}
	j, archive = unsignedJob(t, "c1a0de0000000002")
	claude(j)
	if ans := sendJob(t, j, archive); ans.code != 0 {
		t.Fatalf("a Claude job: %d %v %v", ans.code, ans.err, ans.out)
	}
	argv, err := os.ReadFile(filepath.Join(srv.dir, "argv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(argv), "\n--adapter\nclaude-image\n") {
		t.Fatalf("the run's argv:\n%s", argv)
	}
	env, err := os.ReadFile(filepath.Join(srv.dir, "env"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), "sk-test-model-key") {
		t.Fatal("a model key reached a sign-in run")
	}
}

// The job's policy takes the runhost's record of the vendors' newest
// images, so a run uses the image its sign-in was made in.
func TestTheJobsPolicyTakesTheRunhostsNewestImages(t *testing.T) {
	newRunhostServer(t)
	path, err := policy.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policy.NewestPathFor(path), []byte(`{"claude-image":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	j, _ := unsignedJob(t, "c1a0de0000000003")
	dir := t.TempDir()
	file := filepath.Join(dir, "policy.yaml")
	if err := runhostJobPolicy(file, filepath.Join(dir, "secrets.env"), j); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(policy.NewestPathFor(file)); err != nil || string(b) != `{"claude-image":{}}` {
		t.Fatalf("the job's record: %q %v", b, err)
	}
}

// A run on a runhost carries no key for a plan's sign-in, which the runhost
// keeps, and refuses a subscription token, which only the installed claude
// takes.
func TestARunOnARunhostCarriesNoKeyForASignIn(t *testing.T) {
	for _, kind := range []string{policy.KeyKindClaudeSignIn, policy.KeyKindChatGPTPlan, policy.KeyKindCopilotSignIn} {
		key, err := runOnModelKey(&policy.Policy{Model: policy.Model{Provider: policy.SignInProvider(kind), Name: "m", APIKeyKind: kind}})
		if err != nil || key != "" {
			t.Errorf("%s: %q %v", kind, key, err)
		}
	}
	if _, err := runOnModelKey(&policy.Policy{Model: policy.Model{Provider: "anthropic", Name: "m", APIKeyEnv: "ANTHROPIC_API_KEY", APIKeyKind: policy.KeyKindSubscription}}); err == nil || !strings.Contains(err.Error(), "--sign-in claude") {
		t.Fatalf("a subscription token: %v", err)
	}
}

// A runhost's run never runs a harness as a host process: the CLI the
// service starts refuses the installed claude and codex.
func TestARunhostsRunRefusesAHostHarness(t *testing.T) {
	isolateEnv(t)
	t.Setenv(policy.HomeEnv, t.TempDir())
	t.Setenv("AGENTRY_REGISTRY", "http://127.0.0.1:1")
	home, _ := policy.Home()
	_, archive, digest := buildTestArchive(t, skillFiles("demo", ""))
	meta := &installdir.Meta{Ref: "acme/demo@1.0.0", Namespace: "acme", Name: "demo", Version: "1.0.0", Digest: digest, Source: "registry"}
	meta.Attestation.State = trust.StateUnsigned
	if err := runhostLayDown(home, meta, archive); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{adapter.ClaudeAdapterName, adapter.CodexAdapterName} {
		_, _, err := runCLI(t, nil, "run", "acme/demo@1.0.0", "--installed", "--pin", digest, "--allow-unsigned", "--dry-run", "--adapter", a)
		if err == nil || !strings.Contains(err.Error(), "host process") {
			t.Errorf("--adapter %s: %v", a, err)
		}
	}
}
