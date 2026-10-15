package localrun

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/harnessbuild"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// claudeSignInRun is a built-in's run on a machine whose model setup is a
// Claude subscription, on a fake daemon where the Claude Code image is
// already built from its pinned artifact.
func claudeSignInRun(t *testing.T, home string, hosted bool) (*dockerx.Fake, error) {
	t.Helper()
	pol := policy.Default()
	pol.Images.Runner = "agentry-runner:test"
	pol.Model = policy.Model{Provider: policy.ProviderAnthropic, Name: "claude-sonnet-4-6", APIKeyKind: policy.KeyKindClaudeSignIn,
		ProxyAllow: []string{"api.anthropic.com:443", "platform.claude.com:443"}}
	h, _ := pol.Images.HarnessImageFor("claude-image")
	f := &dockerx.Fake{
		ExistingImages: map[string]bool{h.Ref: true},
		Labels:         map[string]map[string]string{h.Ref: {harnessbuild.LabelArtifact: h.Artifact, harnessbuild.LabelRecipe: harnessbuild.ClaudeRecipe}},
		Foreground:     map[string]*dockerx.FakeForeground{"-probe": {ExitCode: 0}},
	}
	_, err := Run{
		Builtin: KnowledgeCreator().WithoutWebTools(), Home: home, Policy: pol, PolicySHA: "sha", Build: "agentry 9.9.9 (test)",
		Task: strings.NewReader("Ingest into pack runbooks."), OutDir: t.TempDir(), Stderr: io.Discard, RunID: "cafe0002", Hosted: hosted,
	}.Execute(context.Background(), f)
	return f, err
}

// A built-in on a Claude subscription follows adapter.DefaultFor onto the
// Claude Code image and is handed the person's sign-in as `agentry run` is:
// its agent container mounts the sign-in's own volume. The engine does
// not refuse such a run as signed out.
func TestABuiltinIsHandedTheModelSignIn(t *testing.T) {
	home := t.TempDir()
	if _, err := modelsignin.Open(home, nil).Put(modelsignin.Claude, modelsignin.Record{ConfigDir: "agentry-claude-signin"}, modelsignin.Grant{}); err != nil {
		t.Fatal(err)
	}
	f, err := claudeSignInRun(t, home, false)
	if r := receipt.ReasonOf(err); r != nil && r.Code == receipt.ReasonSignInMissing {
		t.Fatalf("the built-in was refused as signed out: %v", err)
	}
	agent, ok := f.FindRun("-agent")
	if !ok {
		t.Fatalf("no agent container (err %v); ops %v", err, f.Ops())
	}
	if !slices.ContainsFunc(agent.Mounts, func(m dockerx.Mount) bool { return m.Source == "agentry-claude-signin" }) {
		t.Fatalf("the agent container does not mount the sign-in's volume: %+v", agent.Mounts)
	}
}

// With no sign-in kept, the built-in stops before any container with the
// reason that says so; a hosted built-in never spends one, even when
// the machine keeps one.
func TestABuiltinWithoutTheSignInOrHostedIsRefused(t *testing.T) {
	f, err := claudeSignInRun(t, t.TempDir(), false)
	if r := receipt.ReasonOf(err); r == nil || r.Code != receipt.ReasonSignInMissing {
		t.Fatalf("err = %v", err)
	}
	if _, ok := f.FindRun("-agent"); ok {
		t.Fatal("an agent container started without a sign-in")
	}
	home := t.TempDir()
	if _, err := modelsignin.Open(home, nil).Put(modelsignin.Claude, modelsignin.Record{ConfigDir: "agentry-claude-signin"}, modelsignin.Grant{}); err != nil {
		t.Fatal(err)
	}
	f, err = claudeSignInRun(t, home, true)
	if err == nil || !strings.Contains(err.Error(), "a hosted run never spends a model sign-in") {
		t.Fatalf("hosted: err = %v", err)
	}
	if _, ok := f.FindRun("-agent"); ok {
		t.Fatal("a hosted built-in's agent container started")
	}
}
