package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// codexImageRef is the fixture's pinned Codex image.
const codexImageRef = "ghcr.io/blaktron/agentry-codex:fixture@sha256:2222222222222222222222222222222222222222222222222222222222222222"

// codexPlanFixture compiles the engine fixture for a codex adapter under a
// ChatGPT plan and hands the run its access token (agentry-cli#739). The
// host adapter's harness is this test binary (TestMain plays codex).
func codexPlanFixture(t *testing.T, adapterName string, edit func(*policy.Policy)) (Params, *dockerx.Fake) {
	t.Helper()
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
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.Images.Runner = "agentry-runner:test"
	pol.Exec.Packages = map[string]string{"echo": "busybox"}
	pol.Model = policy.Model{Provider: policy.ProviderOpenAI, Name: "gpt-fixture", APIKeyKind: policy.KeyKindChatGPTPlan, ProxyAllow: []string{"api.openai.com:443"}}
	pol.Images.HarnessImages = map[string]policy.HarnessImage{adapter.CodexImageAdapterName: {Ref: codexImageRef}}
	pol.Harness = map[string]policy.HarnessPolicy{adapter.CodexAdapterName: {Path: self}}
	if edit != nil {
		edit(pol)
	}
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
	f := &dockerx.Fake{
		Foreground: map[string]*dockerx.FakeForeground{
			"-probe": {Handler: probeListing()},
			"-agent": {Handler: func(_ context.Context, _ dockerx.RunSpec, stdin io.Reader, stdout, _ io.Writer) (int, error) {
				return playCodex(stdin, stdout), nil
			}},
		},
		LogsByName: map[string]string{"agentry-cafe0000-gateway": "gateway said hi\n"},
	}
	return Params{
		Plan: plan, PlanDir: planDir, WorkspaceSrc: ws, RunRoot: installDir, OutDir: t.TempDir(),
		Task: strings.NewReader("the task text"), Adapter: ad, Policy: pol,
		Budgets:     spec.Budgets{TimeoutSeconds: 30, MaxTurns: 10},
		Attestation: receipt.Attestation{State: "local"}, Stderr: os.Stderr, RunID: "cafe0000",
		RunnerHome: installDir,
		ModelSignIn: &ModelSignInGrant{Kind: policy.KeyKindChatGPTPlan,
			Token: ConnectionToken{AccessToken: signInAccessToken, ExpiresAt: time.Now().Add(time.Hour)}},
	}, f
}

// playCodexRefusing is codex's app-server whose model call is refused with
// status, as codex 0.162.0 reports it (measured against a fake Responses
// endpoint): an error notification, then the failed turn.
func playCodexRefusing(status int) func(io.Reader, io.Writer) int {
	variant, msg := "responseTooManyFailedAttempts", "exceeded retry limit, last status: 429 Too Many Requests"
	if status == 403 {
		variant, msg = "httpConnectionFailed", "unexpected status 403 Forbidden: This account is not eligible for ChatGPT plan usage in this app., url: https://api.openai.com/v1/responses"
	}
	errObj := map[string]any{"message": msg, "codexErrorInfo": map[string]any{variant: map[string]any{"httpStatusCode": status}}}
	return func(in io.Reader, out io.Writer) int {
		sc := bufio.NewScanner(in)
		for sc.Scan() {
			var m struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			switch m.Method {
			case "initialize":
				sayLine(out, map[string]any{"id": m.ID, "result": map[string]any{"userAgent": "codex"}})
			case "thread/start":
				sayLine(out, map[string]any{"id": m.ID, "result": map[string]any{"thread": map[string]any{"id": "t1"}}})
			case "turn/start":
				sayLine(out, map[string]any{"id": m.ID, "result": map[string]any{"turn": map[string]any{"id": "u1", "status": "inProgress"}}})
				sayLine(out, map[string]any{"method": "error", "params": map[string]any{"error": errObj, "willRetry": false, "threadId": "t1", "turnId": "u1"}})
				sayLine(out, map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "t1", "turn": map[string]any{"id": "u1", "status": "failed", "error": errObj}}})
			}
		}
		return 0
	}
}

// The Codex image on a ChatGPT plan: one app-server turn in the agent
// container, which starts through the image's shell (the configuration
// into its writable home), reads the access token from the one read-only
// folder beside the plan (I4), and is pinned by digest (I8). The token is
// in no docker call (I3).
func TestACodexImageRunOnAChatGPTPlanIsOneAppServerTurn(t *testing.T) {
	p, f := codexPlanFixture(t, adapter.CodexImageAdapterName, nil)
	var seen string
	f.Foreground["-agent"].Before = func(r dockerx.RunSpec) {
		for _, m := range r.Mounts {
			if m.Target == spec.ModelSignInMount {
				b, _ := os.ReadFile(filepath.Join(m.Source, spec.ModelSignInTokenFile)) //nolint:gosec // the test's run dir
				seen = string(b)
			}
		}
	}
	env, err := Run(context.Background(), f, p)
	if err != nil || env.Status != "ok" {
		t.Fatalf("run: %v (%+v)", err, env)
	}
	if got := string(env.Result); !strings.Contains(got, "You said: the task text") {
		t.Fatalf("result %s", got)
	}
	agent := agentCall(t, f)
	if agent.Image != codexImageRef || agent.Entrypoint != "/bin/sh" || !agent.Interactive || agent.User != "" {
		t.Fatalf("agent container %+v", agent)
	}
	if len(agent.Mounts) != 2 || agent.Mounts[0].Target != spec.PlanMount || agent.Mounts[1].Target != spec.ModelSignInMount ||
		!agent.Mounts[0].ReadOnly || !agent.Mounts[1].ReadOnly {
		t.Fatalf("agent mounts %+v", agent.Mounts)
	}
	if seen != signInAccessToken {
		t.Fatalf("the container's token file held %q", seen)
	}
	for k, v := range agent.Env {
		if strings.Contains(v, signInAccessToken) {
			t.Fatalf("the agent's environment carries the token in %s", k)
		}
	}
	if !strings.HasPrefix(env.Harness.ImagePin, "digest sha256:2222") || env.Harness.CredentialKind != policy.KeyKindChatGPTPlan {
		t.Fatalf("receipt harness %+v", env.Harness)
	}
	if env.Usage.InputTokens != 10 || env.Usage.CostUSD != 0 {
		t.Fatalf("usage %+v", env.Usage)
	}
	events, err := os.ReadFile(filepath.Join(p.OutDir, "receipts", "events.ndjson"))
	if err != nil || !strings.Contains(string(events), "turn/completed") || !strings.Contains(string(events), "mcpServer/elicitation/request") {
		t.Fatalf("events receipt: %v\n%s", err, events)
	}
}

// A 429 on a plan's turn is the plan's limit: the run ends with its own
// outcome, in the budget class, with codex's words and no reset time, and
// the turn is never asked again (I9).
func TestAPlanLimitEndsAnAppServerTurnWithItsOwnOutcome(t *testing.T) {
	p, f := codexPlanFixture(t, adapter.CodexImageAdapterName, nil)
	turns := 0
	play := playCodexRefusing(429)
	f.Foreground["-agent"].Handler = func(_ context.Context, _ dockerx.RunSpec, stdin io.Reader, stdout, _ io.Writer) (int, error) {
		pr, pw := io.Pipe()
		go func() {
			sc := bufio.NewScanner(stdin)
			for sc.Scan() {
				if strings.Contains(sc.Text(), `"turn/start"`) {
					turns++
				}
				_, _ = pw.Write(append(sc.Bytes(), '\n'))
			}
			_ = pw.Close()
		}()
		return play(pr, stdout), nil
	}
	env, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitBudget || env.Reason == nil || env.Reason.Code != receipt.ReasonPlanLimit {
		t.Fatalf("want the plan-limit outcome, got %v (%+v)", err, env.Reason)
	}
	if !strings.Contains(err.Error(), "exceeded retry limit, last status: 429") || strings.Contains(err.Error(), "resets") {
		t.Fatalf("%v", err)
	}
	if turns != 1 {
		t.Fatalf("the turn was sent %d times", turns)
	}
}

// A 403 on a plan's turn is OpenAI refusing plan usage for the account:
// the run fails as the harness's, and says to use an API key.
func TestAnIneligiblePlanTurnSaysToUseAKey(t *testing.T) {
	p, f := codexPlanFixture(t, adapter.CodexImageAdapterName, nil)
	play := playCodexRefusing(403)
	f.Foreground["-agent"].Handler = func(_ context.Context, _ dockerx.RunSpec, stdin io.Reader, stdout, _ io.Writer) (int, error) {
		return play(stdin, stdout), nil
	}
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitHarness {
		t.Fatalf("want exit %d, got %v", ExitHarness, err)
	}
	for _, s := range []string{"not eligible for ChatGPT plan usage", "use an OpenAI API key"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("the error does not say %q: %v", s, err)
		}
	}
}

// The person's installed Codex on a ChatGPT plan: one app-server turn of
// the host process, the token file in the run's own folder; on the
// restart route the process is started with the token in its environment,
// which the run's receipts mask.
func TestTheInstalledCodexOnAChatGPTPlanIsOneAppServerTurn(t *testing.T) {
	for _, route := range []string{"", policy.TokenRouteRestart} {
		p, f := codexPlanFixture(t, adapter.CodexAdapterName, func(pol *policy.Policy) {
			hp := pol.Harness[adapter.CodexAdapterName]
			hp.SignInTokenRoute = route
			pol.Harness[adapter.CodexAdapterName] = hp
		})
		env, err := Run(context.Background(), f, p)
		if err != nil || env.Status != "ok" {
			t.Fatalf("route %q: %v (%+v)", route, err, env)
		}
		got := string(env.Result)
		if !strings.Contains(got, "You said: the task text") || strings.Contains(got, signInAccessToken) {
			t.Fatalf("route %q: result %s", route, got)
		}
		if marked := strings.Contains(got, "[plan token:"); marked != (route == policy.TokenRouteRestart) {
			t.Fatalf("route %q: started with a token in its environment: %v (%s)", route, marked, got)
		}
		if env.Harness.CredentialKind != policy.KeyKindChatGPTPlan || env.Harness.ImagePin != "" {
			t.Fatalf("route %q: receipt harness %+v", route, env.Harness)
		}
		for _, c := range f.Calls {
			if strings.HasSuffix(c.Run.Name, "-agent") {
				t.Fatalf("route %q: an agent container ran for a host harness", route)
			}
		}
	}
}

// codexSessionOnAPlan is the session fixture recompiled for the Codex image
// on a ChatGPT plan, on route, with an access token about to be renewed.
// The agent handler plays codex and records the token each container read.
func codexSessionOnAPlan(t *testing.T, route string) (Params, *dockerx.Fake, func() []string) {
	t.Helper()
	p, f := sessionFixture(t)
	happyFake(f)
	m, err := manifest.Parse([]byte(sessionManifest))
	if err != nil {
		t.Fatal(err)
	}
	pol := p.Policy
	pol.Model = policy.Model{Provider: policy.ProviderOpenAI, Name: "gpt-fixture", APIKeyKind: policy.KeyKindChatGPTPlan, ProxyAllow: []string{"api.openai.com:443"}}
	pol.Images.HarnessImages = map[string]policy.HarnessImage{adapter.CodexImageAdapterName: {Ref: codexImageRef}}
	pol.Harness = map[string]policy.HarnessPolicy{adapter.CodexImageAdapterName: {SignInTokenRoute: route}}
	planDir := filepath.Join(p.RunRoot, "plan", "codex-image")
	plan, err := compile.Compile(compile.Inputs{
		Manifest: m, Mode: manifest.ModeInteractive, Digest: "sha256:feedface", Source: "local", AdapterName: adapter.CodexImageAdapterName,
		WorkspaceDir: p.WorkspaceSrc, Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test",
	}, planDir)
	if err != nil {
		t.Fatal(err)
	}
	ad, _ := adapter.Get(adapter.CodexImageAdapterName)
	p.Plan, p.PlanDir, p.Adapter, p.Policy = plan, planDir, ad, pol
	p.ModelSignIn = &ModelSignInGrant{Kind: policy.KeyKindChatGPTPlan,
		Token: ConnectionToken{AccessToken: signInAccessToken, ExpiresAt: time.Now().Add(2 * time.Second)}}
	p.RenewModelSignIn = gatedRenewal(t)
	var mu sync.Mutex
	var seen []string
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: func(_ context.Context, r dockerx.RunSpec, stdin io.Reader, stdout, _ io.Writer) (int, error) {
		for _, mt := range r.Mounts {
			if mt.Target == spec.ModelSignInMount {
				b, _ := os.ReadFile(filepath.Join(mt.Source, spec.ModelSignInTokenFile)) //nolint:gosec // the test's run dir
				mu.Lock()
				seen = append(seen, string(b))
				mu.Unlock()
			}
		}
		return playCodex(stdin, stdout), nil
	}}
	codexThreads.take()
	return p, f, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// renewals holds each test's renewal until allowRenewal: on a slow
// machine the renewer would otherwise rewrite the token before the first
// harness starts, which is right, and leaves nothing to restart for.
var renewals = struct {
	sync.Mutex
	allow chan struct{}
}{}

func gatedRenewal(t *testing.T) func(context.Context) (ConnectionToken, error) {
	t.Helper()
	allow := make(chan struct{})
	renewals.Lock()
	renewals.allow = allow
	renewals.Unlock()
	return func(ctx context.Context) (ConnectionToken, error) {
		select {
		case <-allow:
		case <-ctx.Done():
			return ConnectionToken{}, ctx.Err()
		}
		return ConnectionToken{AccessToken: signInRenewedToken, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
}

// awaitRenewal lets the gated renewal run, and waits for the runner to
// rewrite the token file.
func awaitRenewal(t *testing.T, p Params) {
	t.Helper()
	renewals.Lock()
	close(renewals.allow)
	renewals.Unlock()
	path := filepath.Join(p.RunRoot, "runs", p.RunID, "cfg", "model-signin", spec.ModelSignInTokenFile)
	for range 200 {
		if b, _ := os.ReadFile(path); string(b) == signInRenewedToken { //nolint:gosec // the test's run dir
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the token was not renewed")
}

// The restart route (D10): codex reads its token once, so after the runner
// renews it the next turn starts the harness again on the renewed token and
// resumes the conversation by its thread; the session goes on, and the
// receipt says how often. The default route never restarts: codex re-reads
// the file itself.
func TestTheRestartRouteResumesTheConversationOnARenewedToken(t *testing.T) {
	ctx := context.Background()
	p, f, seen := codexSessionOnAPlan(t, policy.TokenRouteRestart)
	s, err := StartSession(ctx, f, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	awaitRenewal(t, p)
	if err := s.Send(ctx, "second"); err != nil {
		t.Fatalf("the turn after the renewal: %v", err)
	}
	env, err := s.End(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := seen(); len(got) != 2 || got[0] != signInAccessToken || got[1] != signInRenewedToken {
		t.Fatalf("the containers read %q, want the first token then the renewed one", got)
	}
	if got := codexThreads.take(); strings.Join(got, ",") != "start ephemeral=false,resume t1" {
		t.Fatalf("threads %v: a kept thread, resumed once", got)
	}
	if !strings.Contains(env.CapabilitiesEnforced["model.signIn"], "started again 1 time(s) on a renewed token") {
		t.Fatalf("model.signIn %q", env.CapabilitiesEnforced["model.signIn"])
	}
	agents := 0
	for _, c := range f.Calls {
		if c.Op == "RunForeground" && strings.HasSuffix(c.Run.Name, "-agent") {
			agents++
			if c.Run.Env["CODEX_HOME"] != spec.SessionMount {
				t.Errorf("CODEX_HOME %q: the restart route keeps the conversation in the session store", c.Run.Env["CODEX_HOME"])
			}
		}
	}
	if agents != 2 {
		t.Fatalf("the agent ran %d times", agents)
	}

	p, f, seen = codexSessionOnAPlan(t, "")
	s, err = StartSession(ctx, f, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	awaitRenewal(t, p)
	if err := s.Send(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.End(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := seen(); len(got) != 1 {
		t.Fatalf("the default route started codex %d times", len(got))
	}
	if got := codexThreads.take(); strings.Join(got, ",") != "start ephemeral=true" {
		t.Fatalf("threads %v", got)
	}
}

// The installed Codex on the restart route: the process is started again
// on the renewed token, from the run's own token file, and resumes.
func TestTheInstalledCodexRestartsOnARenewedToken(t *testing.T) {
	ctx := context.Background()
	p, f := sessionFixture(t)
	happyFake(f)
	p = hostAdapterPlan(t, p, adapter.CodexAdapterName)
	m, err := manifest.Parse([]byte(sessionManifest))
	if err != nil {
		t.Fatal(err)
	}
	pol := p.Policy
	pol.Model = policy.Model{Provider: policy.ProviderOpenAI, Name: "gpt-fixture", APIKeyKind: policy.KeyKindChatGPTPlan, ProxyAllow: []string{"api.openai.com:443"}}
	hp := pol.Harness[adapter.CodexAdapterName]
	hp.SignInTokenRoute = policy.TokenRouteRestart
	pol.Harness[adapter.CodexAdapterName] = hp
	plan, err := compile.Compile(compile.Inputs{
		Manifest: m, Mode: manifest.ModeInteractive, Digest: "sha256:feedface", Source: "local", AdapterName: adapter.CodexAdapterName,
		WorkspaceDir: p.WorkspaceSrc, Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test",
	}, p.PlanDir)
	if err != nil {
		t.Fatal(err)
	}
	p.Plan, p.Policy = plan, pol
	p.ModelSignIn = &ModelSignInGrant{Kind: policy.KeyKindChatGPTPlan,
		Token: ConnectionToken{AccessToken: signInAccessToken, ExpiresAt: time.Now().Add(2 * time.Second)}}
	p.RenewModelSignIn = gatedRenewal(t)
	s, err := StartSession(ctx, f, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	awaitRenewal(t, p)
	if err := s.Send(ctx, "second"); err != nil {
		t.Fatalf("the turn after the renewal: %v", err)
	}
	env, err := s.End(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	events, _ := os.ReadFile(filepath.Join(p.OutDir, "receipts", "events.ndjson"))
	if !strings.Contains(string(events), `"on":"the renewed token"`) || strings.Contains(string(events), signInRenewedToken) {
		t.Fatalf("the host process did not resume on the renewed token, or the token is in the receipt:\n%s", events)
	}
	if !strings.Contains(env.CapabilitiesEnforced["model.signIn"], "started again 1 time(s)") {
		t.Fatalf("model.signIn %q", env.CapabilitiesEnforced["model.signIn"])
	}
}
