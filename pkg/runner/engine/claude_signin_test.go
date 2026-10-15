package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// claudeResets is when the fake Claude Code says the plan's limit lifts.
var claudeResets = time.Date(2026, 10, 10, 15, 0, 0, 0, time.UTC)

// playClaudePrint plays `claude -p` (an autonomous run) on Claude Code's
// own sign-in: it reads the task, keeps state in its config directory as
// Claude Code does, and answers with the directory it was given and the
// key variables that reached it. A task asking for the limit gets Claude
// Code's rejected rate_limit_event and its error result instead.
func playClaudePrint(in io.Reader, out io.Writer) int {
	task, _ := io.ReadAll(in)
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir != "" {
		_ = os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"numStartups":1}`), 0o600)
	}
	if strings.Contains(string(task), "limit") {
		sayLine(out, map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{
			"status": "rejected", "resetsAt": claudeResets.Unix(), "rateLimitType": "five_hour"}})
		sayLine(out, map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true,
			"result": "You've hit your session limit", "usage": map[string]any{"input_tokens": 3, "output_tokens": 0}, "total_cost_usd": 0.02})
		return 1
	}
	var keys []string
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if _, ok := os.LookupEnv(k); ok {
			keys = append(keys, k)
		}
	}
	reply := fmt.Sprintf("config=%s keys=%s", dir, strings.Join(keys, ","))
	sayLine(out, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": reply}}}})
	sayLine(out, map[string]any{"type": "result", "subtype": "success", "result": reply, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}, "total_cost_usd": 0.01})
	return 0
}

// claudePlanFixture compiles the engine fixture for adapterName on a Claude
// subscription through Claude Code; the installed claude is this test
// binary (TestMain plays it). The grant is a directory of Agentry's own
// under the runner home for the host, the image's volume for the image.
func claudePlanFixture(t *testing.T, adapterName string) (Params, *dockerx.Fake) {
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
	pol.Model = policy.Model{Provider: policy.ProviderAnthropic, Name: "claude-sonnet-4-6", APIKeyKind: policy.KeyKindClaudeSignIn,
		ProxyAllow: policy.SignInProxyAllow(policy.KeyKindClaudeSignIn)}
	pol.Harness = map[string]policy.HarnessPolicy{adapter.ClaudeAdapterName: {Path: self}}
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
	grant := &ModelSignInGrant{Kind: policy.KeyKindClaudeSignIn, ConfigDir: "agentry-claude-signin"}
	if plan.Harness.Host != nil {
		grant.ConfigDir = filepath.Join(installDir, "model-signins", "claude-code")
		if err := os.MkdirAll(grant.ConfigDir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f := &dockerx.Fake{
		Foreground: map[string]*dockerx.FakeForeground{
			"-probe": {Handler: probeListing()},
			"-agent": {Handler: func(_ context.Context, _ dockerx.RunSpec, stdin io.Reader, stdout, _ io.Writer) (int, error) {
				return playClaudePrint(stdin, stdout), nil
			}},
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

// The installed Claude Code on a Claude subscription: it runs in Agentry's
// own directory for it, never the person's ~/.claude, with no key variable
// of the person's reaching it, and the runner leaves what Claude Code
// keeps there alone; the receipt names the kind, no dollars.
func TestTheInstalledClaudeCodeRunsOnItsOwnSignIn(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-person-0000")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-person")
	p, f := claudePlanFixture(t, adapter.ClaudeAdapterName)
	env, err := Run(context.Background(), f, p)
	if err != nil || env.Status != "ok" {
		t.Fatalf("run: %v (%+v)", err, env)
	}
	got := string(env.Result)
	if !strings.Contains(got, "config="+p.ModelSignIn.ConfigDir+" ") || !strings.HasSuffix(strings.TrimSpace(strings.Trim(got, `"`)), "keys=") {
		t.Fatalf("result %s, want the sign-in's directory and no key", got)
	}
	if b, err := os.ReadFile(filepath.Join(p.ModelSignIn.ConfigDir, ".claude.json")); err != nil || len(b) == 0 { //nolint:gosec // the test's dir
		t.Fatalf("what Claude Code keeps in its directory is gone: %v", err)
	}
	if env.Harness.CredentialKind != policy.KeyKindClaudeSignIn || env.Usage.CostUSD != 0 {
		t.Fatalf("receipt harness %+v, cost %v", env.Harness, env.Usage.CostUSD)
	}
	for _, c := range f.Calls {
		if strings.HasSuffix(c.Run.Name, "-agent") {
			t.Fatal("an agent container ran for a host harness")
		}
	}
}

// The installed Claude Code reading a rejected rate_limit_event ends the
// run with the plan-limit outcome and Claude Code's own reset time, never
// a success.
func TestAClaudePlanLimitEndsTheRunWithItsResetTime(t *testing.T) {
	for _, name := range []string{adapter.ClaudeAdapterName, adapter.ClaudeImageAdapterName} {
		p, f := claudePlanFixture(t, name)
		p.Task = strings.NewReader("run until the limit")
		env, err := Run(context.Background(), f, p)
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != ExitBudget {
			t.Fatalf("%s: want exit %d, got %v", name, ExitBudget, err)
		}
		for _, s := range []string{"plan limit reached", "five_hour", "2026-10-10T15:00:00Z"} {
			if !strings.Contains(err.Error(), s) {
				t.Errorf("%s: the error does not say %q: %v", name, s, err)
			}
		}
		if env.Status != "error" || env.Reason == nil || env.Reason.Code != receipt.ReasonPlanLimit {
			t.Fatalf("%s: envelope %s, reason %+v", name, env.Status, env.Reason)
		}
	}
}

// The installed Claude Code reads a directory on this computer: the image's
// volume handed to it is refused before anything runs. The refusal of an
// ancestor CLAUDE.md still holds for a sign-in run on the host.
func TestTheInstalledClaudeCodeIsRefusedWhatItCannotRunOn(t *testing.T) {
	p, f := claudePlanFixture(t, adapter.ClaudeAdapterName)
	p.ModelSignIn.ConfigDir = "agentry-claude-signin"
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitPolicy || !strings.Contains(err.Error(), "docker volume") || len(f.Calls) != 0 {
		t.Fatalf("a volume for the host: %v (%d docker calls)", err, len(f.Calls))
	}

	p, f = claudePlanFixture(t, adapter.ClaudeAdapterName)
	if err := os.WriteFile(filepath.Join(p.RunRoot, "CLAUDE.md"), []byte("ambient instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), f, p); err == nil || !strings.Contains(err.Error(), "CLAUDE.md") {
		t.Fatalf("an ancestor CLAUDE.md: %v", err)
	}
}

// The Claude Code image's sign-in is its docker volume, mounted read-write
// at Claude Code's config directory into the agent container, and into no
// other container of the run: not the gateway, not the proxy, not a tool.
func TestTheClaudeImagesVolumeIsMountedIntoTheAgentAlone(t *testing.T) {
	p, f := claudePlanFixture(t, adapter.ClaudeImageAdapterName)
	env, err := Run(context.Background(), f, p)
	if err != nil || env.Status != "ok" {
		t.Fatalf("run: %v (%+v)", err, env)
	}
	agent := agentCall(t, f)
	if agent.Env["CLAUDE_CONFIG_DIR"] != adapter.ClaudeImageConfigDir {
		t.Fatalf("agent env CLAUDE_CONFIG_DIR %q", agent.Env["CLAUDE_CONFIG_DIR"])
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if _, ok := agent.Env[k]; ok {
			t.Fatalf("the agent container is given %s", k)
		}
	}
	want := dockerx.Mount{Source: "agentry-claude-signin", Target: adapter.ClaudeImageConfigDir}
	found := false
	for _, m := range agent.Mounts {
		if m == want {
			found = true
		}
	}
	if !found || len(agent.Mounts) != 2 {
		t.Fatalf("agent mounts %+v", agent.Mounts)
	}
	for _, c := range f.Calls {
		if strings.HasSuffix(c.Run.Name, "-agent") {
			continue
		}
		for _, m := range c.Run.Mounts {
			if m.Source == "agentry-claude-signin" || m.Target == adapter.ClaudeImageConfigDir {
				t.Fatalf("%s mounts the sign-in: %+v", c.Run.Name, m)
			}
		}
	}
	if env.Harness.ImagePin == "" || !strings.Contains(env.Harness.ImagePin, "sha512-") {
		t.Fatalf("the receipt's pin %q", env.Harness.ImagePin)
	}
}
