package compile

import (
	"errors"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter/signintest"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// compileSignIn compiles the pure-text fixture for adapterName under a
// policy naming kind; it returns the compile's error rather than failing.
func compileSignIn(t *testing.T, adapterName, kind string, edit func(*policy.Policy)) (*spec.Plan, error) {
	t.Helper()
	signintest.Register()
	m := parseManifest(t, pureTextYAML)
	pol := testPolicy()
	signintest.Policy(pol, kind)
	if edit != nil {
		edit(pol)
	}
	return Compile(Inputs{
		Manifest: m, Mode: manifest.ModeAutonomous, Digest: "sha256:0123456789abcdef", Source: "local",
		WorkspaceDir: workspaceFor(t, m, nil), Policy: pol, PolicySHA: "fixedpolicysha",
		RunnerImage: "agentry-runner:test", AdapterName: adapterName,
	}, t.TempDir())
}

// A token-file sign-in: the agent container mounts the plan and the run's
// sign-in folder read-only, and nothing else (I4); it names no key; the
// plan records the kind, the delivery and the image's pin, and the
// enforcement says what reaches the harness and that the run reports
// tokens (I8, D13).
func TestATokenFileSignInMountsOneReadOnlyFolderBesideThePlan(t *testing.T) {
	plan, err := compileSignIn(t, signintest.TokenAdapter, policy.KeyKindChatGPTPlan, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []spec.Mount{
		{Source: spec.PHCfg + "/harness", Target: spec.PlanMount, ReadOnly: true},
		{Source: spec.PHModelSignIn, Target: spec.ModelSignInMount, ReadOnly: true},
	}
	if got := plan.Harness.Agent.Mounts; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("agent mounts %+v, want %+v", got, want)
	}
	msi := plan.Harness.ModelSignIn
	if msi == nil || msi.Kind != policy.KeyKindChatGPTPlan || msi.Provider != policy.ProviderOpenAI ||
		msi.Delivery != spec.ModelSignInToken || msi.Path != spec.ModelSignInMount+"/"+spec.ModelSignInTokenFile {
		t.Fatalf("modelSignIn %+v", msi)
	}
	if p := plan.Harness.ImagePin; p == nil || p.By != policy.PinDigest || !strings.HasPrefix(p.Value, "sha256:1111") {
		t.Fatalf("imagePin %+v", p)
	}
	for _, e := range plan.Harness.Agent.Env {
		if e.FromHostEnv != "" || e.FromKeyring != "" || e.ExpectedKind != nil {
			t.Fatalf("a sign-in plan names a key: %+v", e)
		}
	}
	if l := plan.Enforcement["model.signIn"]; !strings.Contains(l, "read-only file at /run/agentry/model-signin/token") || !strings.Contains(l, "no refresh token") {
		t.Fatalf("model.signIn %q", l)
	}
	if l := plan.Enforcement["model.cost"]; !strings.Contains(l, "tokens, not dollars") {
		t.Fatalf("model.cost %q", l)
	}
	if _, ok := plan.Enforcement["model.apiKey"]; ok {
		t.Fatal("a sign-in plan claims an API key")
	}
}

// A config-directory sign-in mounts that directory read-write at the
// harness's path, and only it beside the plan.
func TestAConfigDirectorySignInMountsItReadWrite(t *testing.T) {
	plan, err := compileSignIn(t, signintest.DirAdapter, policy.KeyKindClaudeSignIn, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := plan.Harness.Agent.Mounts
	if len(got) != 2 || got[1] != (spec.Mount{Source: spec.PHModelSignIn, Target: signintest.ConfigDir}) {
		t.Fatalf("agent mounts %+v", got)
	}
	if l := plan.Enforcement["model.signIn"]; !strings.Contains(l, "read-write at "+signintest.ConfigDir) {
		t.Fatalf("model.signIn %q", l)
	}
}

// The same adapter under an API key mounts no sign-in folder: the folder
// is there only when a sign-in needs it (I4).
func TestAKeyPlanMountsNoSignInFolder(t *testing.T) {
	plan, err := compileSignIn(t, signintest.TokenAdapter, policy.KeyKindChatGPTPlan, func(p *policy.Policy) {
		p.Model.APIKeyKind, p.Model.APIKeyEnv = "", "OPENAI_API_KEY"
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Harness.ModelSignIn != nil || len(plan.Harness.Agent.Mounts) != 1 {
		t.Fatalf("a key plan: modelSignIn %+v, mounts %+v", plan.Harness.ModelSignIn, plan.Harness.Agent.Mounts)
	}
	if _, ok := plan.Enforcement["model.signIn"]; ok {
		t.Fatal("a key plan claims a sign-in")
	}
}

// An adapter that cannot spend the sign-in the policy names refuses the
// plan by policy (exit 11), naming the adapter and the kind: docker-agent
// for every kind (D8), and a host adapter, which drives the person's own
// installation, until its milestone gives it a delivery (codex's came with
// agentry-cli#739).
func TestAnAdapterThatCannotSpendASignInRefusesByPolicy(t *testing.T) {
	for _, c := range []struct{ adapter, kind string }{
		{"docker-agent", policy.KeyKindChatGPTPlan},
		{"docker-agent", policy.KeyKindClaudeSignIn},
		{"docker-agent", policy.KeyKindCopilotSignIn},
		{"claude", policy.KeyKindClaudeSignIn},
		{"codex-image", policy.KeyKindClaudeSignIn},
	} {
		_, err := compileSignIn(t, c.adapter, c.kind, nil)
		var refusal *PolicyRefusal
		if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "the "+c.adapter+" adapter cannot spend") || !strings.Contains(err.Error(), c.kind) {
			t.Errorf("%s with %s: %v", c.adapter, c.kind, err)
		}
	}
}

// compileCodexPlan compiles the pure-text fixture for a codex adapter under
// a ChatGPT plan.
func compileCodexPlan(t *testing.T, adapterName string, edit func(*policy.Policy)) (*spec.Plan, error) {
	t.Helper()
	return compileSignIn(t, adapterName, policy.KeyKindChatGPTPlan, func(p *policy.Policy) {
		p.Model.Name = "gpt-fixture"
		if edit != nil {
			edit(p)
		}
	})
}

// A run paid by a ChatGPT plan reaches OpenAI's inference API alone (I2):
// a proxy allowlist naming anything else, or nothing, is refused by policy.
func TestAChatGPTPlanRunReachesOpenAIsAPIAlone(t *testing.T) {
	for _, allow := range [][]string{
		{"api.openai.com:443", "auth.openai.com:443"},
		{"chatgpt.com:443"},
		{"api.openai.com:443", "api.openai.com:443"},
	} {
		_, err := compileCodexPlan(t, "codex-image", func(p *policy.Policy) { p.Model.ProxyAllow = allow })
		var refusal *PolicyRefusal
		if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "reaches api.openai.com:443 alone") {
			t.Errorf("allow %v: %v", allow, err)
		}
	}
	plan, err := compileCodexPlan(t, "codex-image", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(plan.Proxy.Allow, ",") != "api.openai.com:443" {
		t.Fatalf("proxy allow %v", plan.Proxy.Allow)
	}
}

// The Codex image on a ChatGPT plan: the default pinned image by digest,
// the plan and the token folder read-only and nothing else (I4, I8), the
// autonomous run one app-server turn.
func TestTheCodexImageCompilesAPlanRun(t *testing.T) {
	plan, err := compileCodexPlan(t, "codex-image", func(p *policy.Policy) { p.Images.HarnessImages = nil })
	if err != nil {
		t.Fatal(err)
	}
	h := plan.Harness
	def, _ := policy.Default().Images.HarnessImageFor("codex-image")
	if h.Agent.Image != def.Ref || h.ImagePin == nil || h.ImagePin.By != policy.PinDigest {
		t.Fatalf("image %q pin %+v", h.Agent.Image, h.ImagePin)
	}
	if len(h.Agent.Mounts) != 2 || h.Agent.Mounts[1] != (spec.Mount{Source: spec.PHModelSignIn, Target: spec.ModelSignInMount, ReadOnly: true}) {
		t.Fatalf("mounts %+v", h.Agent.Mounts)
	}
	if h.OneTurn == nil || h.Agent.StdinTask || h.Agent.Entrypoint != "/bin/sh" {
		t.Fatalf("one turn %+v, stdin %v, entrypoint %q", h.OneTurn, h.Agent.StdinTask, h.Agent.Entrypoint)
	}
}

// The installed Codex on a ChatGPT plan reads the token in the run's own
// folder: no mount, no key, the host line in the receipt (D14).
func TestTheInstalledCodexCompilesAPlanRun(t *testing.T) {
	plan, err := compileCodexPlan(t, "codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := plan.Harness
	if h.Host == nil || h.Agent.Image != "" || len(h.Agent.Mounts) != 0 || h.ImagePin != nil {
		t.Fatalf("host %+v, agent %+v", h.Host, h.Agent)
	}
	if h.ModelSignIn == nil || h.ModelSignIn.Path != spec.PHModelSignIn+"/"+spec.ModelSignInTokenFile {
		t.Fatalf("modelSignIn %+v", h.ModelSignIn)
	}
	for _, e := range h.Host.Env {
		if e.FromHostEnv != "" || e.FromKeyring != "" {
			t.Fatalf("a plan run on the host names a key: %+v", e)
		}
	}
	if l := plan.Enforcement["model.signIn"]; !strings.Contains(l, "one file in this run's own folder") {
		t.Fatalf("model.signIn %q", l)
	}
}
