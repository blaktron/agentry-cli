package engine

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter/copilottest"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// The fixture's Copilot tokens: the one the run is given, and the one a
// renewal hands out.
const (
	copilotToken   = "ghu_copilotfixture0000000000000000"
	copilotRenewed = "ghu_copilotrenewed0000000000000000"
)

// copilotPlanFixture compiles the engine fixture for the Copilot image
// under a Copilot plan and hands the run its access token and a
// renewal. The agent container is srv, a fake Copilot CLI.
func copilotPlanFixture(t *testing.T, srv *copilottest.Server) (Params, *dockerx.Fake, *int) {
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
	pol := policy.Default()
	pol.Images.Runner = "agentry-runner:test"
	pol.Exec.Packages = map[string]string{"echo": "busybox"}
	pol.Model = policy.Model{Provider: policy.ProviderCopilot, Name: "auto", APIKeyKind: policy.KeyKindCopilotSignIn,
		ProxyAllow: policy.SignInProxyAllow(policy.KeyKindCopilotSignIn)}
	planDir := filepath.Join(installDir, "plan")
	plan, err := compile.Compile(compile.Inputs{
		Manifest: m, Digest: "sha256:feedface", Source: "local", AdapterName: adapter.CopilotImageAdapterName,
		WorkspaceDir: ws, Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test",
	}, planDir)
	if err != nil {
		t.Fatal(err)
	}
	ad, err := adapter.Get(adapter.CopilotImageAdapterName)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENGINE_TEST_TOKEN", "sekret-value-1234")
	f := &dockerx.Fake{
		Foreground: map[string]*dockerx.FakeForeground{
			"-probe": {Handler: probeListing("write_report")},
			"-agent": {Handler: func(_ context.Context, _ dockerx.RunSpec, stdin io.Reader, stdout, _ io.Writer) (int, error) {
				if err := srv.Serve(stdin, stdout); err != nil {
					return 1, err
				}
				return 0, nil
			}},
		},
		LogsByName: map[string]string{"agentry-cafe0000-gateway": "gateway said hi\n"},
	}
	renewals := new(int)
	var mu sync.Mutex
	return Params{
		Plan: plan, PlanDir: planDir, WorkspaceSrc: ws, RunRoot: installDir, OutDir: t.TempDir(),
		Task: strings.NewReader("the task text"), Adapter: ad, Policy: pol,
		Budgets:     spec.Budgets{TimeoutSeconds: 30, MaxTurns: 10},
		Attestation: receipt.Attestation{State: "local"}, Stderr: io.Discard, RunID: "cafe0000",
		RunnerHome: installDir,
		ModelSignIn: &ModelSignInGrant{Kind: policy.KeyKindCopilotSignIn,
			Token: ConnectionToken{AccessToken: copilotToken, ExpiresAt: time.Now().Add(8 * time.Hour)}},
		RenewModelSignIn: func(context.Context) (ConnectionToken, error) {
			mu.Lock()
			defer mu.Unlock()
			*renewals++
			return ConnectionToken{AccessToken: copilotRenewed, ExpiresAt: time.Now().Add(8 * time.Hour)}, nil
		},
	}, f, renewals
}

// A Copilot run on a Copilot plan: one session turn in the agent container,
// which mounts nothing and is given no token in its environment; Copilot
// CLI is handed the run's token through its callback, and a renewed one
// when it asks for a refresh; a gateway tool of the grant is approved; the
// receipt names the plan's kind and the image's checksum pin, not signed;
// and neither token is in any docker call, receipt or output file.
func TestACopilotRunIsHandedItsTokenThroughTheCallbackAlone(t *testing.T) {
	srv := &copilottest.Server{Turn: func(c *copilottest.Conn, prompt string) {
		c.AskToken("refresh")
		c.ToolCall("c1", "write_report")
		c.Event("assistant.usage", map[string]any{"inputTokens": 10, "outputTokens": 5})
		c.Event("assistant.message", map[string]any{"content": "You said: " + prompt})
		c.Event("session.idle", map[string]any{})
	}}
	p, f, renewals := copilotPlanFixture(t, srv)
	env, err := Run(context.Background(), f, p)
	if err != nil || env.Status != "ok" {
		t.Fatalf("run: %v (%+v)", err, env)
	}
	if got := string(env.Result); !strings.Contains(got, "You said: the task text") {
		t.Fatalf("result %s", got)
	}
	agent := agentCall(t, f)
	if len(agent.Mounts) != 0 || !agent.Interactive || agent.Entrypoint != "/usr/local/bin/copilot" {
		t.Fatalf("agent container %+v", agent)
	}
	_, _, toks, decisions, _ := srv.Snapshot()
	if len(toks) != 2 || toks[0]["accessToken"] != copilotToken || toks[1]["accessToken"] != copilotRenewed || *renewals != 1 {
		t.Fatalf("tokens %v, renewals %d", toks, *renewals)
	}
	if decisions["p-c1"]["kind"] != "approve-once" {
		t.Fatalf("decisions %v", decisions)
	}
	if env.Harness.CredentialKind != policy.KeyKindCopilotSignIn || !strings.Contains(env.Harness.ImagePin, "pinned by checksum, not signed") ||
		!strings.HasPrefix(env.Harness.ImagePin, "artifact sha256:") {
		t.Fatalf("receipt harness %+v", env.Harness)
	}
	if line := env.CapabilitiesEnforced["model.signIn"]; !strings.Contains(line, "hands the harness over its stdio") {
		t.Fatalf("model.signIn %q", line)
	}
	if env.Usage.InputTokens != 10 || env.Usage.CostUSD != 0 {
		t.Fatalf("usage %+v", env.Usage)
	}
	for _, op := range f.Ops() {
		if strings.Contains(op, copilotToken) || strings.Contains(op, copilotRenewed) {
			t.Fatalf("a docker call carries a token: %s", op)
		}
	}
	for k, v := range agent.Env {
		if strings.Contains(v, "ghu_") {
			t.Fatalf("the agent's environment carries a token in %s", k)
		}
	}
	for _, dir := range []string{p.OutDir, p.RunRoot} {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, _ := os.ReadFile(path) //nolint:gosec // the test's own folders
			if strings.Contains(string(b), copilotToken) || strings.Contains(string(b), copilotRenewed) {
				t.Errorf("%s carries a token", path)
			}
			return nil
		})
	}
}

// A quota error on a Copilot plan's turn is the plan's limit.
func TestACopilotQuotaEndsTheRunWithThePlanLimit(t *testing.T) {
	srv := &copilottest.Server{Turn: func(c *copilottest.Conn, _ string) {
		c.Event("session.error", map[string]any{"errorType": "quota", "errorCode": "quota_exceeded", "message": "You have exceeded your monthly quota."})
	}}
	p, f, _ := copilotPlanFixture(t, srv)
	env, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitBudget || env.Reason == nil || env.Reason.Code != receipt.ReasonPlanLimit {
		t.Fatalf("want the plan-limit outcome, got %v (%+v)", err, env.Reason)
	}
	if !strings.Contains(err.Error(), "quota_exceeded") {
		t.Fatalf("the vendor's words: %v", err)
	}
}

// An organization's policy that turns Copilot CLI off for the account ends
// the run as refused by policy, the policy named.
func TestAnOrganizationsCopilotPolicyEndsTheRunWithThePolicyNamed(t *testing.T) {
	p, f, _ := copilotPlanFixture(t, &copilottest.Server{CreateError: "Error: Access denied by policy settings"})
	env, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitPolicy || env.Reason == nil || env.Reason.Code != receipt.ReasonOrgPolicy ||
		env.Reason.Detail != receipt.OrgPolicyClient {
		t.Fatalf("want the organization-policy outcome, got %v (%+v)", err, env.Reason)
	}
	if !strings.Contains(err.Error(), "Access denied by policy settings") || !strings.Contains(err.Error(), "policy for this client") {
		t.Fatalf("the policy named: %v", err)
	}
}

// Without a renewal from its caller the run's token is the one it was
// given, and the receipt says it is not renewed.
func TestACopilotRunWithoutARenewalSaysSo(t *testing.T) {
	srv := &copilottest.Server{}
	p, f, _ := copilotPlanFixture(t, srv)
	p.RenewModelSignIn = nil
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	if line := env.CapabilitiesEnforced["model.signIn"]; !strings.Contains(line, "not renewed") {
		t.Fatalf("model.signIn %q", line)
	}
}

// copilotSessionFixture compiles the session fixture's interactive plan for
// the Copilot image under a Copilot plan, its agent container srv.
func copilotSessionFixture(t *testing.T, srv *copilottest.Server) (Params, *dockerx.Fake, *int) {
	t.Helper()
	m, err := manifest.Parse([]byte(sessionManifest))
	if err != nil {
		t.Fatal(err)
	}
	installDir := t.TempDir()
	ws := filepath.Join(installDir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "AGENT.md"), []byte("Chat helpfully.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.Images.Runner = "agentry-runner:test"
	pol.Sessions = policy.Sessions{IdleTimeoutSeconds: 3600, MaxWallSeconds: 3600, MaxUserTurns: 3}
	pol.Model = policy.Model{Provider: policy.ProviderCopilot, Name: "auto", APIKeyKind: policy.KeyKindCopilotSignIn,
		ProxyAllow: policy.SignInProxyAllow(policy.KeyKindCopilotSignIn)}
	planDir := filepath.Join(installDir, "plan", "interactive")
	plan, err := compile.Compile(compile.Inputs{
		Manifest: m, Mode: manifest.ModeInteractive, Digest: "sha256:feedface", Source: "local",
		WorkspaceDir: ws, Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test", AdapterName: adapter.CopilotImageAdapterName,
	}, planDir)
	if err != nil {
		t.Fatal(err)
	}
	ad, _ := adapter.Get(adapter.CopilotImageAdapterName)
	t.Setenv("ENGINE_TEST_TOKEN", "sekret-value-1234")
	f := &dockerx.Fake{}
	happyFake(f)
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: func(_ context.Context, _ dockerx.RunSpec, stdin io.Reader, stdout, _ io.Writer) (int, error) {
		return 0, srv.Serve(stdin, stdout)
	}}
	renewals := new(int)
	var mu sync.Mutex
	return Params{
		Plan: plan, PlanDir: planDir, WorkspaceSrc: ws, RunRoot: installDir, OutDir: t.TempDir(),
		Task: strings.NewReader(""), Adapter: ad, Policy: pol,
		Budgets: spec.Budgets{TimeoutSeconds: 30, MaxTurns: 10}, Attestation: receipt.Attestation{State: "local"},
		Stderr: io.Discard, RunID: "5e5510", RunnerHome: installDir,
		ModelSignIn: &ModelSignInGrant{Kind: policy.KeyKindCopilotSignIn,
			Token: ConnectionToken{AccessToken: copilotToken, ExpiresAt: time.Now().Add(8 * time.Hour)}},
		RenewModelSignIn: func(context.Context) (ConnectionToken, error) {
			mu.Lock()
			defer mu.Unlock()
			*renewals++
			return ConnectionToken{AccessToken: copilotRenewed, ExpiresAt: time.Now().Add(8 * time.Hour)}, nil
		},
	}, f, renewals
}

// A Copilot session is handed its token through the callback at its start
// and a renewed one when Copilot CLI asks mid-session, and its receipts
// hold neither.
func TestACopilotSessionIsHandedItsTokenThroughTheCallbackAndRenewed(t *testing.T) {
	srv := &copilottest.Server{Turn: func(c *copilottest.Conn, prompt string) {
		c.AskToken("refresh")
		c.Event("assistant.message", map[string]any{"content": "You said: " + prompt})
		c.Event("session.idle", map[string]any{})
	}}
	p, f, renewals := copilotSessionFixture(t, srv)
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	_, done := collect(s)
	if err := s.Send(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	env, err := s.End(context.Background(), false)
	<-done
	if err != nil {
		t.Fatalf("end: %v (%+v)", err, env)
	}
	_, _, toks, _, _ := srv.Snapshot()
	if len(toks) != 2 || toks[0]["accessToken"] != copilotToken || toks[1]["accessToken"] != copilotRenewed || *renewals != 1 {
		t.Fatalf("tokens %v, renewals %d", toks, *renewals)
	}
	if env.Harness.CredentialKind != policy.KeyKindCopilotSignIn || !strings.Contains(env.Harness.ImagePin, "pinned by checksum, not signed") {
		t.Fatalf("receipt %+v", env.Harness)
	}
	for _, dir := range []string{p.OutDir, p.RunRoot} {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, _ := os.ReadFile(path) //nolint:gosec // the test's own folders
			if strings.Contains(string(b), copilotToken) || strings.Contains(string(b), copilotRenewed) {
				t.Errorf("%s carries a token", path)
			}
			return nil
		})
	}
}

// An organization's policy that refuses the session at its start ends it
// refused by that policy, named.
func TestAnOrganizationsPolicyRefusesACopilotSessionAtItsStart(t *testing.T) {
	p, f, _ := copilotSessionFixture(t, &copilottest.Server{CreateError: "Error: Access denied by policy settings"})
	_, err := StartSession(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitPolicy || receipt.ReasonOf(err) == nil || receipt.ReasonOf(err).Detail != receipt.OrgPolicyClient {
		t.Fatalf("want the organization-policy outcome, got %v", err)
	}
}
