//go:build !windows

package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// hostTestAdapter is a host-run harness (plan runner-m-r4-adapters §5)
// whose "installation" is a shell script the test puts on PATH: it proves
// the engine's host path — published ports, rendered addresses, the clean
// environment, the task on stdin, receipts and the envelope — without any
// real harness.
type hostTestAdapter struct{}

func (hostTestAdapter) Name() string { return "hosttest" }

func (hostTestAdapter) Compile(in adapter.Input) (adapter.Result, error) {
	return adapter.Result{
		Files: map[string][]byte{"config.txt": []byte("gateway=" + spec.PHHostGatewayURL + "\nproxy=" + spec.PHHostProxyURL + "\n")},
		Host: &spec.HostProcessSpec{
			Command: "agentry-hosttest",
			Argv:    []string{"--gateway", spec.PHHostGatewayURL},
			Env: []spec.EnvVar{
				{Name: spec.GatewayTokenEnv, FromRunToken: true},
				{Name: in.Model.APIKeyEnv, FromHostEnv: in.Model.APIKeyEnv},
				{Name: "HTTPS_PROXY", Value: spec.PHHostProxyURL},
				{Name: "HTTP_PROXY", Value: spec.PHHostProxyURL},
				{Name: "NO_PROXY", Value: "127.0.0.1,localhost"},
			},
			InheritEnv:  []string{"PATH", "HOME", "AGENTRY_HOSTTEST_ENVFILE", "AGENTRY_HOSTTEST_TASKFILE"},
			VersionArgv: []string{"--version"},
			StdinTask:   in.Mode != manifest.ModeInteractive,
		},
		Model:      in.Model.Provider + "/" + in.Model.Name,
		ResultKind: "text",
	}, nil
}

func (hostTestAdapter) Events(line []byte) []adapter.Event {
	da, _ := adapter.Get(adapter.DefaultName)
	return da.Events(line)
}

func (hostTestAdapter) Session(context.Context, io.ReadWriteCloser, adapter.SessionOptions) (adapter.SessionDriver, error) {
	return nil, errors.New("hosttest: no session")
}

func init() { adapter.Register(hostTestAdapter{}) }

const hostScript = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "hosttest 1.2.3"; exit 0; fi
env | sort > "$AGENTRY_HOSTTEST_ENVFILE"
echo "argv: $*" >> "$AGENTRY_HOSTTEST_ENVFILE"
echo "cwd: $(pwd)" >> "$AGENTRY_HOSTTEST_ENVFILE"
cat config.txt >> "$AGENTRY_HOSTTEST_ENVFILE"
cat > "$AGENTRY_HOSTTEST_TASKFILE"
echo '{"type":"tool_call","tool":"fs_read"}'
echo '{"type":"agent_choice","message":{"content":"host answer"}}'
echo '{"type":"stream_stopped","usage":{"input_tokens":7,"output_tokens":3,"cost":0.001}}'
`

func hostFixture(t *testing.T) (Params, *dockerx.Fake, string, string) {
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
	if err := os.WriteFile(filepath.Join(ws, "SKILL.md"), []byte("Do the host test.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.Images.Runner = "agentry-runner:test"
	planDir := filepath.Join(installDir, "plan")
	plan, err := compile.Compile(compile.Inputs{
		Manifest: m, Digest: "sha256:feedface", Source: "local", AdapterName: "hosttest",
		WorkspaceDir: ws, Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test",
	}, planDir)
	if err != nil {
		t.Fatal(err)
	}
	ad, err := adapter.Get("hosttest")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "agentry-hosttest"), []byte(hostScript), 0o755); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}
	envFile := filepath.Join(t.TempDir(), "env.txt")
	taskFile := filepath.Join(t.TempDir(), "task.txt")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENTRY_HOSTTEST_ENVFILE", envFile)
	t.Setenv("AGENTRY_HOSTTEST_TASKFILE", taskFile)
	t.Setenv("ENGINE_TEST_TOKEN", "sekret-value-1234")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
	t.Setenv("UNRELATED_HOST_VAR", "must-not-leak")
	f := &dockerx.Fake{
		Foreground: map[string]*dockerx.FakeForeground{"-probe": {ExitCode: 0}},
		Ports:      map[string]int{"agentry-cafe0001-proxy": 41001, "agentry-cafe0001-gateway": 41002},
		LogsByName: map[string]string{"agentry-cafe0001-proxy": "allow model.example:443\n"},
	}
	return Params{
		Plan: plan, PlanDir: planDir, WorkspaceSrc: ws, RunRoot: installDir, OutDir: t.TempDir(),
		Task: strings.NewReader("the host task"), Adapter: ad, Policy: pol,
		Budgets: spec.Budgets{TimeoutSeconds: 30, MaxTurns: 10}, Attestation: receipt.Attestation{State: "local"},
		Stderr: os.Stderr, RunID: "cafe0001",
	}, f, envFile, taskFile
}

func TestRunHostHarness(t *testing.T) {
	p, f, envFile, taskFile := hostFixture(t)
	if p.Plan.Harness.Host == nil || p.Plan.Harness.Agent.Image != "" {
		t.Fatalf("plan harness = %+v", p.Plan.Harness)
	}
	if got := p.Plan.Enforcement["harness.isolation"]; !strings.Contains(got, "host process") {
		t.Errorf("enforcement claim = %q", got)
	}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if env.Status != "ok" || string(env.Result) != `"host answer"` || env.Usage.Turns != 1 || env.Usage.InputTokens != 7 {
		t.Fatalf("envelope = status %s result %s usage %+v", env.Status, env.Result, env.Usage)
	}
	if !strings.Contains(env.Harness.Host, "agentry-hosttest") || !strings.Contains(env.Harness.Host, "hosttest 1.2.3") {
		t.Errorf("harness.host = %q", env.Harness.Host)
	}
	// The run's containers were published on 127.0.0.1, ephemeral, and
	// no agent container ran.
	proxy, ok := f.FindRun("-proxy")
	if !ok || len(proxy.Publish) != 1 || proxy.Publish[0].HostIP != "127.0.0.1" || proxy.Publish[0].HostPort != 0 || proxy.Publish[0].ContainerPort != spec.ProxyPort {
		t.Errorf("proxy publish = %+v", proxy.Publish)
	}
	gw, ok := f.FindRun("-gateway")
	if !ok || len(gw.Publish) != 1 || gw.Publish[0].ContainerPort != spec.GatewayPort {
		t.Errorf("gateway publish = %+v", gw.Publish)
	}
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "RunForeground:agentry-cafe0001-agent") {
			t.Error("an agent container ran beside the host harness")
		}
	}
	// What the harness saw: the rendered addresses, the token, the model
	// key, nothing else from the runner's environment.
	seen, err := os.ReadFile(envFile) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	got := string(seen)
	for _, want := range []string{
		"HTTPS_PROXY=http://127.0.0.1:41001", "HTTP_PROXY=http://127.0.0.1:41001",
		"AGENTRY_GATEWAY_URL=http://127.0.0.1:41002/mcp", "AGENTRY_PROXY_URL=http://127.0.0.1:41001",
		"ANTHROPIC_API_KEY=sk-test-key", "argv: --gateway http://127.0.0.1:41002/mcp",
		"gateway=http://127.0.0.1:41002/mcp", "proxy=http://127.0.0.1:41001",
		"cwd: " + filepath.Join(p.RunRoot, "runs", "cafe0001", "cfg", "harness"),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("harness environment lacks %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "MCP_GATEWAY_TOKEN=") || strings.Contains(got, "MCP_GATEWAY_TOKEN=\n") {
		t.Error("harness got no gateway token")
	}
	for _, leak := range []string{"ENGINE_TEST_TOKEN", "sekret-value-1234", "UNRELATED_HOST_VAR"} {
		if strings.Contains(got, leak) {
			t.Errorf("harness environment leaks %q", leak)
		}
	}
	task, _ := os.ReadFile(taskFile) //nolint:gosec // test file
	if string(task) != "the host task" {
		t.Errorf("task on stdin = %q", task)
	}
	if b, err := os.ReadFile(filepath.Join(p.OutDir, "receipts", "proxy.log")); err != nil || !strings.Contains(string(b), "allow model.example:443") {
		t.Errorf("proxy.log receipt = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(p.OutDir, "receipts", "events.ndjson")); err != nil {
		t.Error("events receipt missing")
	}
}

// The run network is internal, so docker programs a published port only
// once the container has been connected to the bridge — a moment after
// `docker run -d` returns. Reading it once made the gateway's address
// fail intermittently on a real daemon (found by the first host-run
// harness, agentry-cli#80); the engine must wait for the mapping.
func TestRunHostHarnessWaitsForThePublishedPort(t *testing.T) {
	p, f, _, _ := hostFixture(t)
	f.PortFailures = 3 // the first reads find no mapping yet
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if env.Status != "ok" {
		t.Fatalf("envelope status = %s", env.Status)
	}
}

func TestRunHostHarnessNotInstalled(t *testing.T) {
	p, f, _, _ := hostFixture(t)
	t.Setenv("PATH", t.TempDir())
	env, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitCompile || !strings.Contains(err.Error(), "not installed") || !strings.Contains(err.Error(), "harness.hosttest.path") {
		t.Fatalf("missing harness: %v (envelope %+v)", err, env)
	}
	if f.Leftover != nil {
		t.Errorf("leftovers: %v", f.Leftover)
	}
}

func TestRunHostHarnessPolicyPath(t *testing.T) {
	p, f, envFile, _ := hostFixture(t)
	real := filepath.Join(t.TempDir(), "my-harness")
	if err := os.WriteFile(real, []byte(hostScript), 0o755); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	p.Policy.Harness = map[string]policy.HarnessPolicy{"hosttest": {Path: real}}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run with policy path: %v", err)
	}
	if !strings.HasPrefix(env.Harness.Host, real) {
		t.Errorf("harness.host = %q, want the policy path", env.Harness.Host)
	}
	if _, err := os.Stat(envFile); err != nil {
		t.Error("the policy-named harness did not run")
	}
}
