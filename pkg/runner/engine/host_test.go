//go:build !windows

// The host-harness tests are built off Windows only: the test harness they
// put on PATH is a shell script, which Windows cannot execute
// (agentry-cli#446). So no test covers the engine's host path on Windows.

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

// hostTestRefuse is what the test adapter names as ambient context to
// refuse over (agentry-cli#171); nil for every other test.
var hostTestRefuse []string

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
			InheritEnv:          []string{"PATH", "HOME", "AGENTRY_HOSTTEST_ENVFILE", "AGENTRY_HOSTTEST_TASKFILE"},
			VersionArgv:         []string{"--version"},
			StdinTask:           in.Mode != manifest.ModeInteractive,
			RefuseAncestorFiles: hostTestRefuse,
		},
		Model:      in.Model.Provider + "/" + in.Model.Name,
		ResultKind: "text",
	}, nil
}

// The test adapter is a HostHarness too, with one usual install location
// under the home directory, so the envelope test below can prove a run
// names the well-known path it actually ran (agentry-cli#176).
func (hostTestAdapter) Executable() string    { return "agentry-hosttest" }
func (hostTestAdapter) ProbeVersion() string  { return "" }
func (hostTestAdapter) VersionArgv() []string { return []string{"--version"} }
func (hostTestAdapter) InstallURL() string    { return "https://example.test/hosttest" }
func (hostTestAdapter) DisplayName() string   { return "Host Test" }
func (hostTestAdapter) MCPCommand() string    { return "" }
func (hostTestAdapter) WellKnownPaths(string) []string {
	return []string{"~/.hosttest/bin/agentry-hosttest"}
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
	pol.Exec.Packages = map[string]string{"echo": "busybox"} // the fixture declares exec: [echo]
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
		Ports:      map[string]int{"agentry-cafe0001-proxy:3128": 41001, "agentry-cafe0001-proxy:8811": 41002},
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
	// The proxy container — the one with a bridge leg — publishes both
	// doors on 127.0.0.1, ephemeral: its own CONNECT port and a forward to
	// the gateway, which publishes nothing and stays on the run network.
	// No agent container ran.
	proxy, ok := f.FindRun("-proxy")
	if !ok || len(proxy.Publish) != 2 || proxy.Publish[0].HostIP != "127.0.0.1" || proxy.Publish[0].HostPort != 0 || proxy.Publish[0].ContainerPort != spec.ProxyPort ||
		proxy.Publish[1].HostIP != "127.0.0.1" || proxy.Publish[1].HostPort != 0 || proxy.Publish[1].ContainerPort != spec.GatewayPort {
		t.Errorf("proxy publish = %+v", proxy.Publish)
	}
	if !strings.Contains(strings.Join(proxy.Argv, " "), "--forward :8811=gateway:8811") {
		t.Errorf("proxy argv lacks the gateway forward: %v", proxy.Argv)
	}
	gw, ok := f.FindRun("-gateway")
	if !ok || len(gw.Publish) != 0 || gw.Network != "agentry-cafe0001-net" {
		t.Errorf("gateway run = %+v: must publish nothing and stay on the run network", gw)
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
	t.Setenv("HOME", t.TempDir()) // and not in its usual location either
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

// With nothing on PATH and no policy path, the run finds the harness in
// its usual install location and the envelope names that path — the
// receipt says what ran (agentry-cli#176).
func TestRunHostHarnessWellKnownLocation(t *testing.T) {
	p, f, envFile, _ := hostFixture(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	known := filepath.Join(home, ".hosttest", "bin", "agentry-hosttest")
	if err := os.MkdirAll(filepath.Dir(known), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(known, []byte(hostScript), 0o755); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run with a well-known location: %v", err)
	}
	if !strings.HasPrefix(env.Harness.Host, known) {
		t.Errorf("harness.host = %q, want the well-known path", env.Harness.Host)
	}
	if _, err := os.Stat(envFile); err != nil {
		t.Error("the well-known harness did not run")
	}
}

// A real host adapter resolves through harnessprobe.Find, so a run finds
// the harness where doctor found it: here the native installer's
// ~/.local/bin, with nothing on PATH (agentry-cli#167). The policy's path
// still comes first, and a policy path that is not there is the refusal
// it always was.
func TestResolveHostCommandUsesTheWellKnownLocations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	exe := filepath.Join(home, ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho 2.1.263\n"), 0o755); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}
	got, err := resolveHostCommand(policy.Default(), adapter.ClaudeAdapterName, "claude")
	if err != nil || got != exe {
		t.Fatalf("resolve = %q, %v", got, err)
	}
	pol := policy.Default()
	pol.Harness = map[string]policy.HarnessPolicy{adapter.ClaudeAdapterName: {Path: filepath.Join(home, "nowhere")}}
	_, err = resolveHostCommand(pol, adapter.ClaudeAdapterName, "claude")
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitCompile || !strings.Contains(err.Error(), "policy harness.claude.path") {
		t.Fatalf("a missing policy path must refuse, not fall back: %v", err)
	}
	if err := os.Remove(exe); err != nil {
		t.Fatal(err)
	}
	_, err = resolveHostCommand(policy.Default(), adapter.ClaudeAdapterName, "claude")
	if !errors.As(err, &ce) || ce.Code != ExitCompile || !strings.Contains(err.Error(), "usual install locations") {
		t.Fatalf("nothing anywhere: %v", err)
	}
}

// A harness that reads context from its ancestors names the files in
// the plan; the engine refuses the run when one is there, naming it
// (agentry-cli#171): the invariant is measured, not assumed.
func TestRunHostHarnessRefusesAmbientContextInAnAncestor(t *testing.T) {
	if found, err := ancestorFile(t.TempDir(), nil); err != nil || found != "" {
		t.Fatalf("nothing named: %q %v", found, err)
	}
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "harness")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if found, err := ancestorFile(deep, []string{"CLAUDE.md", ".claude/rules"}); err != nil || found != "" {
		t.Fatalf("clean tree: %q %v", found, err)
	}
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("# mine"), 0o644); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}
	if found, _ := ancestorFile(deep, []string{"CLAUDE.md", ".claude/rules"}); found != filepath.Join(root, "CLAUDE.md") {
		t.Fatalf("an ancestor's CLAUDE.md: %q", found)
	}
	if err := os.Remove(filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "a", ".claude", "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if found, _ := ancestorFile(deep, []string{"CLAUDE.md", ".claude/rules"}); found != filepath.Join(root, "a", ".claude", "rules") {
		t.Fatalf("a rules directory: %q", found)
	}

	// Through the engine: the run is refused before the process starts,
	// as a policy refusal naming the file, and nothing is left behind.
	// (The fixture compiles the plan, so the adapter must name the file
	// before it runs.)
	hostTestRefuse = []string{"CLAUDE.md"}
	t.Cleanup(func() { hostTestRefuse = nil })
	p, f, envFile, _ := hostFixture(t)
	if err := os.WriteFile(filepath.Join(p.RunRoot, "CLAUDE.md"), []byte("# ambient"), 0o644); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitPolicy || !strings.Contains(err.Error(), "CLAUDE.md would enter the run") {
		t.Fatalf("ambient context: %v", err)
	}
	if _, err := os.Stat(envFile); err == nil {
		t.Error("the harness ran")
	}
	if f.Leftover != nil {
		t.Errorf("leftovers: %v", f.Leftover)
	}
}

// The proxy is started with --plain-http only when the plan lists plain
// targets (DR-45): an all-https plan starts it CONNECT-only, as before.
func TestProxyArgvCarriesPlainHTTPTargetsOnlyWhenPlanned(t *testing.T) {
	p, f, _, _ := hostFixture(t)
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("Run: %v", err)
	}
	proxy, _ := f.FindRun("-proxy")
	if strings.Contains(strings.Join(proxy.Argv, " "), "--plain-http") {
		t.Fatalf("an all-https plan's proxy argv carries --plain-http: %v", proxy.Argv)
	}
	p, f, _, _ = hostFixture(t)
	p.Plan.Proxy.PlainHTTP = []string{"llm.lan:8000"}
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("Run: %v", err)
	}
	proxy, _ = f.FindRun("-proxy")
	if !strings.Contains(strings.Join(proxy.Argv, " "), "--plain-http llm.lan:8000") {
		t.Fatalf("proxy argv lacks the plain targets: %v", proxy.Argv)
	}
}

// On Windows a host-run harness also inherits what a Windows program needs
// to find its profile, its npm shims and a temporary directory — and still
// nothing else — and the receipt names the allowlist (agentry-cli#423).
func TestHostHarnessInheritsWindowsProfileVariablesOnWindowsOnly(t *testing.T) {
	for _, goos := range []string{"windows", "linux"} {
		t.Run(goos, func(t *testing.T) {
			old := hostOS
			hostOS = goos
			t.Cleanup(func() { hostOS = old })
			p, f, envFile, _ := hostFixture(t)
			t.Setenv("USERPROFILE", `C:\Users\blakt`)
			t.Setenv("APPDATA", `C:\Users\blakt\AppData\Roaming`)
			t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
			env, err := Run(context.Background(), f, p)
			if err != nil {
				t.Fatal(err)
			}
			seen, err := os.ReadFile(envFile) //nolint:gosec // test file
			if err != nil {
				t.Fatal(err)
			}
			got := string(seen)
			has := strings.Contains(got, "USERPROFILE=") && strings.Contains(got, "APPDATA=") && strings.Contains(got, "PATHEXT=")
			if has != (goos == "windows") {
				t.Errorf("on %s the harness has the Windows profile variables: %v\n%s", goos, has, got)
			}
			for _, leak := range []string{"ENGINE_TEST_TOKEN", "sekret-value-1234", "UNRELATED_HOST_VAR"} {
				if strings.Contains(got, leak) {
					t.Errorf("harness environment leaks %q", leak)
				}
			}
			line := env.CapabilitiesEnforced["harness.env"]
			if !strings.Contains(line, "HOME") || !strings.Contains(line, "PATH") || strings.Contains(line, "USERPROFILE") != (goos == "windows") {
				t.Errorf("receipt harness.env = %q", line)
			}
		})
	}
}

func TestHostInherit(t *testing.T) {
	if got := hostInherit([]string{"HOME", "PATH"}, "linux"); strings.Join(got, ",") != "HOME,PATH" {
		t.Errorf("linux: %v", got)
	}
	// The exact allowlist: widening it is a decision, not an edit.
	want := "HOME,PATH,temp,USERPROFILE,APPDATA,LOCALAPPDATA,TMP,PATHEXT,ComSpec,SystemRoot"
	got := hostInherit([]string{"HOME", "PATH", "temp"}, "windows")
	if strings.Join(got, ",") != want {
		t.Errorf("windows: %v, want %s (a name the plan has in another case is not added again)", got, want)
	}
	seen := map[string]int{}
	for _, n := range got {
		seen[n]++
		if seen[n] > 1 {
			t.Errorf("windows: %s twice in %v", n, got)
		}
	}
}

// An empty task sends the agent's own instruction to a host harness too
// (on its stdin, agentry-cli#449). It lives here with the other
// host-harness tests, for the reason at the top of this file.
func TestAnEmptyTaskSendsTheOwnInstructionToAHostHarness(t *testing.T) {
	p, f, _, taskFile := hostFixture(t)
	p.Task = nil
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("Run: %v", err)
	}
	task, _ := os.ReadFile(taskFile) //nolint:gosec // test file
	if string(task) != "Do the host test.\n" {
		t.Errorf("task on stdin = %q, want the agent's own instruction", task)
	}
}
