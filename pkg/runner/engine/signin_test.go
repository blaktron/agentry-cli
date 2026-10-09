package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter/signintest"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

const (
	signInAccessToken  = "plan-access-token-0123456789abcdef"
	signInRenewedToken = "plan-renewed-token-fedcba9876543210"
	// What the person's store holds beside the access token, and the run
	// must never see (I3): the CLI's glue keeps them (modelsignin).
	signInRefreshToken = "plan-refresh-token-never-in-a-run"
)

// signInFixture compiles the engine fixture for one of signintest's
// adapters under a policy naming kind, and hands the run the grant its
// delivery needs.
func signInFixture(t *testing.T, adapterName, kind string) (Params, *dockerx.Fake) {
	t.Helper()
	signintest.Register()
	m, err := manifest.Parse([]byte(engineManifest))
	if err != nil {
		t.Fatal(err)
	}
	installDir := t.TempDir()
	ws := filepath.Join(installDir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "SKILL.md"), []byte("Do the engine test.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.Images.Runner = "agentry-runner:test"
	pol.Exec.Packages = map[string]string{"echo": "busybox"}
	signintest.Policy(pol, kind)
	planDir := filepath.Join(installDir, "plan")
	plan, err := compile.Compile(compile.Inputs{
		Manifest: m, Digest: "sha256:feedface", Source: "local", AdapterName: adapterName,
		WorkspaceDir: ws, Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test",
	}, planDir)
	if err != nil {
		t.Fatal(err)
	}
	ad, err := adapter.Get(adapterName)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENGINE_TEST_TOKEN", "sekret-value-1234")
	grant := &ModelSignInGrant{Kind: kind}
	if plan.Harness.ModelSignIn.Delivery == spec.ModelSignInToken {
		grant.Token = ConnectionToken{AccessToken: signInAccessToken, ExpiresAt: time.Now().Add(time.Hour)}
	} else {
		grant.ConfigDir = filepath.Join(installDir, "model-signins", "claude")
		if err := os.MkdirAll(grant.ConfigDir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f := &dockerx.Fake{
		Foreground: map[string]*dockerx.FakeForeground{
			"-probe": {Handler: probeListing()},
			"-agent": {Stdout: strings.Join([]string{
				signintest.Line(signintest.Event{Type: "tool_call"}),
				signintest.Line(signintest.Event{Type: "text", Text: "final answer"}),
				signintest.Line(signintest.Event{Type: "usage", In: 120, Out: 30, Cost: 0.42}),
			}, "\n") + "\n"},
		},
		LogsByName: map[string]string{"agentry-cafe0000-gateway": "gateway said hi\n"},
	}
	return Params{
		Plan: plan, PlanDir: planDir, WorkspaceSrc: ws, RunRoot: installDir, OutDir: t.TempDir(),
		Task: strings.NewReader("the task text"), Adapter: ad, Policy: pol,
		Budgets:     spec.Budgets{TimeoutSeconds: 30, MaxTurns: 10},
		Attestation: receipt.Attestation{State: "local"}, Stderr: os.Stderr, RunID: "cafe0000",
		RunnerHome: installDir, ModelSignIn: grant,
	}, f
}

// agentCall is the run's agent container as the daemon was asked for it.
func agentCall(t *testing.T, f *dockerx.Fake) dockerx.RunSpec {
	t.Helper()
	for _, c := range f.Calls {
		if strings.HasSuffix(c.Run.Name, "-agent") {
			return c.Run
		}
	}
	t.Fatal("no agent container ran")
	return dockerx.RunSpec{}
}

// A token-file sign-in: the agent container mounts the plan and one
// read-only folder holding the token file and nothing else (I4); the file
// holds the access token alone, 0600, and a renewal rewrites it before
// the token expires; the token is in no docker call, no receipt and no
// output, and dies with the run (I3, I6); the receipt names the kind and
// the image's pin (I8) and reports tokens, not the harness's dollars (D13).
func TestASignInTokenReachesTheAgentAloneAndIsRenewed(t *testing.T) {
	p, f := signInFixture(t, signintest.TokenAdapter, policy.KeyKindChatGPTPlan)
	p.ModelSignIn.Token.ExpiresAt = time.Now().Add(3 * time.Second)
	var stderr lockedBuffer
	p.Stderr = &stderr
	renewed := make(chan struct{})
	var renewals atomic.Int32
	p.RenewModelSignIn = func(context.Context) (ConnectionToken, error) {
		if renewals.Add(1) == 1 {
			defer close(renewed)
		}
		return ConnectionToken{AccessToken: signInRenewedToken, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	dir := filepath.Join(p.RunRoot, "runs", "cafe0000", "cfg", "model-signin")
	tokenPath := filepath.Join(dir, spec.ModelSignInTokenFile)
	var before, after string
	var mode os.FileMode
	var entries int
	f.Foreground["-agent"].Before = func(dockerx.RunSpec) {
		b, _ := os.ReadFile(tokenPath) //nolint:gosec // the test's run dir
		before = string(b)
		if fi, err := os.Stat(tokenPath); err == nil {
			mode = fi.Mode().Perm()
		}
		if es, err := os.ReadDir(dir); err == nil {
			entries = len(es)
		}
		select {
		case <-renewed:
		case <-time.After(10 * time.Second):
			t.Error("the sign-in's token was not renewed before it expired")
		}
		for range 100 {
			if b, _ = os.ReadFile(tokenPath); string(b) == signInRenewedToken { //nolint:gosec // the test's run dir
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		after = string(b)
	}
	f.LogsByName["agentry-cafe0000-gateway"] = "harness said " + signInAccessToken + "\n"

	env, err := Run(context.Background(), f, p)
	if err != nil || env.Status != "ok" {
		t.Fatalf("run: %v (%+v)", err, env)
	}
	if before != signInAccessToken || mode != 0o600 || entries != 1 {
		t.Fatalf("the token file held %q (mode %v, %d entries in its folder)", before, mode, entries)
	}
	if after != signInRenewedToken {
		t.Fatalf("after the renewal the token file held %q", after)
	}
	agent := agentCall(t, f)
	want := []dockerx.Mount{
		{Source: filepath.Join(p.RunRoot, "runs", "cafe0000", "cfg", "harness"), Target: spec.PlanMount, ReadOnly: true},
		{Source: dir, Target: spec.ModelSignInMount, ReadOnly: true},
	}
	if len(agent.Mounts) != len(want) {
		t.Fatalf("agent mounts %+v", agent.Mounts)
	}
	for i := range want {
		if agent.Mounts[i].Source != want[i].Source || agent.Mounts[i].Target != want[i].Target || agent.Mounts[i].ReadOnly != want[i].ReadOnly {
			t.Fatalf("agent mount %d is %+v, want %+v", i, agent.Mounts[i], want[i])
		}
	}
	for _, c := range f.Calls {
		blob := c.Arg + " " + strings.Join(c.Run.Argv, " ")
		for k, v := range c.Run.Env {
			blob += " " + k + "=" + v
		}
		for _, m := range c.Run.Mounts {
			blob += " " + m.Source
		}
		if strings.Contains(blob, signInAccessToken) || strings.Contains(blob, signInRenewedToken) {
			t.Fatalf("a docker call carries the token: %+v", c)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the sign-in's folder outlived the run: %v", err)
	}
	_ = filepath.WalkDir(p.OutDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(path) //nolint:gosec // the test's out dir
			for _, s := range []string{signInAccessToken, signInRenewedToken, signInRefreshToken} {
				if strings.Contains(string(b), s) {
					t.Errorf("%s holds %q", path, s)
				}
			}
		}
		return nil
	})
	if s := stderr.String(); strings.Contains(s, signInAccessToken) || strings.Contains(s, signInRenewedToken) {
		t.Fatalf("stderr carries the token:\n%s", s)
	}
	if env.Harness.CredentialKind != policy.KeyKindChatGPTPlan || !strings.HasPrefix(env.Harness.ImagePin, "digest sha256:1111") {
		t.Fatalf("harness %+v", env.Harness)
	}
	if env.Usage.CostUSD != 0 || env.Usage.InputTokens != 120 || env.Usage.OutputTokens != 30 {
		t.Fatalf("usage %+v: a plan run reports tokens, not the harness's dollars", env.Usage)
	}
	if !strings.Contains(env.CapabilitiesEnforced["model.cost"], "tokens, not dollars") || env.CapabilitiesEnforced["model.signIn"] == "" {
		t.Fatalf("capabilities %v", env.CapabilitiesEnforced)
	}
	if n := renewals.Load(); n != 1 {
		t.Fatalf("renewals: %d", n)
	}
}

// A config-directory sign-in mounts the person's directory read-write at
// the harness's path, writes nothing into it, and leaves it in place.
func TestAConfigDirectorySignInIsMountedAndLeftAlone(t *testing.T) {
	p, f := signInFixture(t, signintest.DirAdapter, policy.KeyKindClaudeSignIn)
	keep := filepath.Join(p.ModelSignIn.ConfigDir, ".credentials.json")
	if err := os.WriteFile(keep, []byte("the harness's own"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := Run(context.Background(), f, p)
	if err != nil || env.Status != "ok" {
		t.Fatalf("run: %v (%+v)", err, env)
	}
	m := agentCall(t, f).Mounts
	if len(m) != 2 || m[1].Source != p.ModelSignIn.ConfigDir || m[1].Target != signintest.ConfigDir || m[1].ReadOnly || m[1].Relabel {
		t.Fatalf("agent mounts %+v", m)
	}
	if b, err := os.ReadFile(keep); err != nil || string(b) != "the harness's own" { //nolint:gosec // the test's dir
		t.Fatalf("the sign-in's directory was touched: %q %v", b, err)
	}
	if env.Harness.CredentialKind != policy.KeyKindClaudeSignIn {
		t.Fatalf("harness %+v", env.Harness)
	}
}

// Only a directory of Agentry's own is mounted read-write: a person's own
// folder, the store itself, a path that climbs out of it, or a volume not
// named agentry-… is refused before the daemon.
func TestAConfigDirectoryMustBeAgentrysOwn(t *testing.T) {
	for _, dir := range []string{"", "home", "store", "climb", "volume"} {
		p, f := signInFixture(t, signintest.DirAdapter, policy.KeyKindClaudeSignIn)
		switch dir {
		case "home":
			p.ModelSignIn.ConfigDir = t.TempDir()
		case "store":
			p.ModelSignIn.ConfigDir = filepath.Join(p.RunnerHome, "model-signins")
		case "climb":
			p.ModelSignIn.ConfigDir = filepath.Join(p.RunnerHome, "model-signins", "..", "runs")
		case "volume":
			p.ModelSignIn.ConfigDir = "claude-config"
		}
		_, err := Run(context.Background(), f, p)
		if dir == "" {
			if err != nil {
				t.Fatalf("the store's own directory: %v", err)
			}
			continue
		}
		if err == nil || len(f.Calls) != 0 {
			t.Errorf("%s: %v (%d docker calls)", dir, err, len(f.Calls))
		}
	}
	if !OwnSignInDir("", "agentry-claude-config") || OwnSignInDir("", "/abs") {
		t.Fatal("volumes")
	}
}

// With no renewal from its client, the receipt says the token was fixed
// for the run rather than claim a renewal that could not happen.
func TestAnUnrenewedTokenIsSaidSo(t *testing.T) {
	p, f := signInFixture(t, signintest.TokenAdapter, policy.KeyKindChatGPTPlan)
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	if l := env.CapabilitiesEnforced["model.signIn"]; !strings.Contains(l, "not renewed") || strings.Contains(l, "rewritten by the runner") {
		t.Fatalf("model.signIn %q", l)
	}
	if p.Plan.Enforcement["model.signIn"] == env.CapabilitiesEnforced["model.signIn"] {
		t.Fatal("the plan's own map was edited")
	}
}

// Every refusal is made before the daemon is touched: no grant (exit 11,
// signin-missing), a grant of another kind (11), a grant handed to a run
// compiled for a key (11), and a cost budget (exit 2, D13).
func TestASignInRunRefusesWhatItWasNotCompiledFor(t *testing.T) {
	for _, c := range []struct {
		name   string
		edit   func(*Params)
		code   int
		reason receipt.ReasonCode
		say    string
	}{
		{"no grant", func(p *Params) { p.ModelSignIn = nil }, ExitPolicy, receipt.ReasonSignInMissing, "was given no sign-in"},
		{"no token", func(p *Params) { p.ModelSignIn.Token.AccessToken = "" }, ExitPolicy, receipt.ReasonSignInMissing, "no access token"},
		{"another kind", func(p *Params) { p.ModelSignIn.Kind = policy.KeyKindCopilotSignIn }, ExitPolicy, "", "compiled for a ChatGPT plan sign-in"},
		{"a cost budget", func(p *Params) { p.Budgets.MaxCostUSD = 2 }, ExitUsage, "", "tokens, not dollars"},
		{"a token of another shape", func(p *Params) { p.ModelSignIn.Token.AccessToken = "a b\n" }, ExitCLI, "", "shape"},
	} {
		p, f := signInFixture(t, signintest.TokenAdapter, policy.KeyKindChatGPTPlan)
		c.edit(&p)
		env, err := Run(context.Background(), f, p)
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != c.code || !strings.Contains(err.Error(), c.say) {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if r := receipt.ReasonOf(err); (c.reason == "") != (r == nil) || (r != nil && r.Code != c.reason) {
			t.Errorf("%s: reason %+v", c.name, r)
		}
		if env.Status != "error" || len(f.Calls) != 0 {
			t.Errorf("%s: status %s, %d docker calls", c.name, env.Status, len(f.Calls))
		}
	}
	p, f := fixture(t)
	p.ModelSignIn = &ModelSignInGrant{Kind: policy.KeyKindChatGPTPlan, Token: ConnectionToken{AccessToken: signInAccessToken}}
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitPolicy || !strings.Contains(err.Error(), "compiled for an API key") || len(f.Calls) != 0 {
		t.Fatalf("a key plan handed a sign-in: %v (%d calls)", err, len(f.Calls))
	}
}

// A plan limit ends the run with its own outcome (I9): the budget class
// with reason plan-limit, the vendor's words and the reset time it gave,
// never a success though the harness exits 0, and the agent ran once.
func TestAPlanLimitEndsTheRunWithItsOwnOutcome(t *testing.T) {
	p, f := signInFixture(t, signintest.TokenAdapter, policy.KeyKindChatGPTPlan)
	resets := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	f.Foreground["-agent"].Stdout = strings.Join([]string{
		signintest.Line(signintest.Event{Type: "text", Text: "working"}),
		signintest.Line(signintest.Event{Type: "plan_limit", Text: "usage limit reached", Resets: resets}),
		signintest.Line(signintest.Event{Type: "text", Text: "done anyway"}),
	}, "\n") + "\n"
	env, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitBudget {
		t.Fatalf("want exit %d, got %v", ExitBudget, err)
	}
	for _, s := range []string{"plan limit reached", "usage limit reached", "2026-10-12T09:00:00Z"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("the error does not say %q: %v", s, err)
		}
	}
	if env.Status != "error" || env.Reason == nil || env.Reason.Code != receipt.ReasonPlanLimit {
		t.Fatalf("envelope %s, reason %+v", env.Status, env.Reason)
	}
	agents := 0
	for _, c := range f.Calls {
		if strings.HasSuffix(c.Run.Name, "-agent") {
			agents++
		}
	}
	if agents != 1 {
		t.Fatalf("the agent ran %d times", agents)
	}
	// No reset time is invented when the vendor gave none.
	p, f = signInFixture(t, signintest.TokenAdapter, policy.KeyKindChatGPTPlan)
	f.Foreground["-agent"].Stdout = signintest.Line(signintest.Event{Type: "plan_limit"}) + "\n"
	_, err = Run(context.Background(), f, p)
	if err == nil || strings.Contains(err.Error(), "resets") {
		t.Fatalf("no reset time: %v", err)
	}
}

// planLimitAdapter is the session fixture's adapter whose driver reports a
// plan limit after its first turn, as a sign-in harness would.
type planLimitAdapter struct{ adapter.Adapter }

func (a planLimitAdapter) Session(ctx context.Context, rw io.ReadWriteCloser, o adapter.SessionOptions) (adapter.SessionDriver, error) {
	d, err := a.Adapter.Session(ctx, rw, o)
	if err != nil {
		return nil, err
	}
	out := make(chan adapter.Event, 64)
	go func() {
		for ev := range d.Events() {
			out <- ev
		}
		close(out)
	}()
	return &planLimitDriver{SessionDriver: d, out: out}, nil
}

type planLimitDriver struct {
	adapter.SessionDriver
	out  chan adapter.Event
	once sync.Once
}

func (d *planLimitDriver) Events() <-chan adapter.Event { return d.out }

func (d *planLimitDriver) Prompt(ctx context.Context, text string) (*adapter.Turn, error) {
	turn, err := d.SessionDriver.Prompt(ctx, text)
	d.once.Do(func() {
		d.out <- adapter.Event{Kind: adapter.EventPlanLimit, Text: "weekly limit reached"}
	})
	return turn, err
}

// A plan limit ends a session with its own reason and outcome (I9): the
// plan_limit end, exit 16 with reason plan-limit, and no further turn.
func TestAPlanLimitEndsASession(t *testing.T) {
	p, f := sessionFixture(t)
	happyFake(f)
	p.Adapter = planLimitAdapter{p.Adapter}
	h := &fakeHarness{}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	get, done := collect(s)
	ctx := context.Background()
	_ = s.Send(ctx, "first turn")
	deadline := time.After(10 * time.Second)
	for {
		if ended, reason := s.Ended(); ended {
			if reason != session.EndPlanLimit {
				t.Fatalf("ended for %s", reason)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("the plan limit never ended the session")
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err := s.Send(ctx, "another"); err != session.ErrEnded {
		t.Fatalf("a turn after the limit: %v", err)
	}
	env, err := s.End(ctx, true)
	<-done
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitBudget || env.Status != "error" || env.Reason == nil || env.Reason.Code != receipt.ReasonPlanLimit ||
		!strings.Contains(err.Error(), "weekly limit reached") {
		t.Fatalf("end = %v (%+v)", err, env.Reason)
	}
	if last := get()[len(get())-1]; last.Kind != session.KindSessionEnd || last.Reason != session.EndPlanLimit {
		t.Fatalf("last event %+v", last)
	}
}
