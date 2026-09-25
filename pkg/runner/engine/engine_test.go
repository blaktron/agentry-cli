// Engine lifecycle tests against the fake docker client: ordering, env
// and mount resolution, budget enforcement, failure classification, and
// the §R11 invariants (teardown on every path, secrets scoping, agent
// container topology). These run a REAL compiled plan — compile + engine
// together — so drift between the two layers fails here, not on a docker
// host (RUNNER-DEVIATIONS DR-7).
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

const engineManifest = `schemaVersion: "2"
name: engine-test
namespace: demo
modes: [autonomous]
version: 1.0.0
summary: "engine lifecycle fixture"
entrypoints:
  instruction: SKILL.md
capabilities:
  network:
    egress: [api.example.com]
  filesystem:
    read: workspace
    write: workspace
  exec: { commands: [echo] }
  secrets: [ENGINE_TEST_TOKEN]
publish:
  include: [agent.yaml, SKILL.md]
`

// fixture compiles a real plan into a temp install layout.
func fixture(t *testing.T) (Params, *dockerx.Fake) {
	t.Helper()
	return fixtureWith(t, nil)
}

// fixtureWith is fixture with the policy edited before the compile —
// the way an operator's policy file shapes a plan.
func fixtureWith(t *testing.T, edit func(*policy.Policy)) (Params, *dockerx.Fake) {
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
	pol.Exec.Packages = map[string]string{"echo": "busybox"} // the fixture declares exec: [echo]
	if edit != nil {
		edit(pol)
	}
	planDir := filepath.Join(installDir, "plan")
	plan, err := compile.Compile(compile.Inputs{
		Manifest: m, Digest: "sha256:feedface", Source: "local",
		WorkspaceDir: ws, Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test",
	}, planDir)
	if err != nil {
		t.Fatal(err)
	}
	ad, err := adapter.Get(adapter.DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENGINE_TEST_TOKEN", "sekret-value-1234")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
	return Params{
		Plan:         plan,
		PlanDir:      planDir,
		WorkspaceSrc: ws,
		RunRoot:      installDir,
		OutDir:       t.TempDir(),
		Task:         strings.NewReader("the task text"),
		Adapter:      ad,
		Policy:       pol,
		Budgets:      spec.Budgets{TimeoutSeconds: 30, MaxTurns: 10},
		Attestation:  receipt.Attestation{State: "local"},
		Stderr:       os.Stderr,
		RunID:        "cafe0000",
	}, &dockerx.Fake{}
}

// happyFake scripts a successful run: probe ok, agent emits two tool
// calls, usage, and a final message.
func happyFake(f *dockerx.Fake) {
	f.Foreground = map[string]*dockerx.FakeForeground{
		"-probe": {ExitCode: 0},
		"-agent": {Stdout: strings.Join([]string{
			`{"type":"tool_call","tool":"fs_read"}`,
			`{"type":"tool_call","tool":"write_report"}`,
			`{"type":"agent_choice","message":{"content":"final answer"}}`,
			`{"type":"stream_stopped","usage":{"input_tokens":100,"output_tokens":40,"cost":0.05}}`,
		}, "\n") + "\n"},
	}
	f.LogsByName = map[string]string{"agentry-cafe0000-gateway": "gateway said hi\n"}
}

func TestRunHappyPath(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if env.Status != "ok" || env.ExitCode != ExitOK {
		t.Fatalf("envelope status=%s code=%d", env.Status, env.ExitCode)
	}
	if string(env.Result) != `"final answer"` {
		t.Errorf("result = %s", env.Result)
	}
	if env.Usage.Turns != 2 || env.Usage.InputTokens != 100 || env.Usage.OutputTokens != 40 || env.Usage.CostUSD != 0.05 {
		t.Errorf("usage = %+v", env.Usage)
	}
	if env.Receipts.RunPlanSHA256 == "" {
		t.Error("envelope lacks plan hash")
	}

	// Execute order (§R6): network → proxy (+bridge) → gateway → probe →
	// agent; teardown afterwards in reverse. The gateway never joins the
	// bridge: the MCP servers it launches inherit its networks, and a
	// user-defined network beside the bridge is a mix docker refuses
	// (agentry-cli#105).
	ops := f.Ops()
	var idx = map[string]int{}
	for i, op := range ops {
		if _, seen := idx[op]; !seen {
			idx[op] = i
		}
	}
	order := []string{
		"NetworkCreate:agentry-cafe0000-net(internal=true)",
		"RunDetached:agentry-cafe0000-proxy",
		"NetworkConnect:bridge<-agentry-cafe0000-proxy",
		"RunDetached:agentry-cafe0000-gateway",
		"RunForeground:agentry-cafe0000-probe",
		"RunForeground:agentry-cafe0000-agent",
		"Stop:agentry-cafe0000-gateway",
		"NetworkRemove:agentry-cafe0000-net",
	}
	last := -1
	for _, want := range order {
		at, ok := idx[want]
		if !ok {
			t.Fatalf("missing op %q in %v", want, ops)
		}
		if at < last {
			t.Fatalf("op %q out of order in %v", want, ops)
		}
		last = at
	}

	// §R11 invariant 2: the agent container joins only the internal run
	// network, mounts only /plan, and never sees the docker socket.
	agent, ok := f.FindRun("-agent")
	if !ok {
		t.Fatal("agent run not recorded")
	}
	if agent.Network != "agentry-cafe0000-net" {
		t.Errorf("agent network = %q", agent.Network)
	}
	if len(agent.Mounts) != 1 || agent.Mounts[0].Target != "/plan" || !agent.Mounts[0].ReadOnly {
		t.Errorf("agent mounts = %+v", agent.Mounts)
	}
	for _, op := range ops {
		if op == "NetworkConnect:bridge<-agentry-cafe0000-agent" {
			t.Error("agent container was connected to the bridge network")
		}
		if op == "NetworkConnect:bridge<-agentry-cafe0000-gateway" {
			t.Error("gateway was connected to the bridge network: its MCP servers would inherit the mix docker refuses")
		}
	}
	if gw, ok := f.FindRun("-gateway"); !ok || gw.Network != "agentry-cafe0000-net" || len(gw.Publish) != 0 {
		t.Errorf("gateway run = %+v: must sit on the run network only and publish nothing", gw)
	} else if gw.Hostname == "" || gw.Hostname == gw.Name || strings.Contains(gw.Hostname, "agentry-cafe0000") {
		// The gateway finds "its" networks by looking up a container
		// named like its hostname; a hostname that is any run container's
		// name would make every MCP server inherit that network.
		t.Errorf("gateway hostname %q must not be a run container's name", gw.Hostname)
	}
	if !strings.Contains(agent.Mounts[0].Source, "cfg") || strings.Contains(agent.Mounts[0].Source, "@@") {
		t.Errorf("agent mount source unresolved: %q", agent.Mounts[0].Source)
	}
	if agent.Env["MCP_GATEWAY_TOKEN"] == "" || len(agent.Env["MCP_GATEWAY_TOKEN"]) != 32 {
		t.Errorf("agent gateway token = %q", agent.Env["MCP_GATEWAY_TOKEN"])
	}
	if agent.Env["ANTHROPIC_API_KEY"] != "sk-test-key" {
		t.Errorf("model key not resolved: %q", agent.Env["ANTHROPIC_API_KEY"])
	}
	if got := agent.Env["HTTPS_PROXY"]; got != "http://llm-proxy:3128" {
		t.Errorf("HTTPS_PROXY = %q", got)
	}

	// §R11 invariant 3: the manifest secret's VALUE appears in no docker
	// argv or env — it lives only in secrets.env (deleted by teardown).
	for _, c := range f.Calls {
		for k, v := range c.Run.Env {
			if strings.Contains(v, "sekret-value-1234") && k != "ENGINE_TEST_TOKEN" {
				t.Errorf("secret value leaked into %s env %s", c.Run.Name, k)
			}
			if k == "ENGINE_TEST_TOKEN" {
				t.Errorf("manifest secret handed to container %s directly", c.Run.Name)
			}
		}
		for _, a := range c.Run.Argv {
			if strings.Contains(a, "sekret-value-1234") {
				t.Errorf("secret value leaked into %s argv", c.Run.Name)
			}
		}
	}

	// Gateway env: token + resolved bind allowlists.
	gw, _ := f.FindRun("gateway")
	if gw.Env["MCP_GATEWAY_AUTH_TOKEN"] != agent.Env["MCP_GATEWAY_TOKEN"] {
		t.Error("gateway and agent tokens differ")
	}
	if strings.Contains(gw.Env["MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS"], "@@") {
		t.Errorf("bind allowlist unresolved: %q", gw.Env["MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS"])
	}
	// The gateway's daemon socket is the run's filter, never the daemon's
	// own (agentry-cli#219), and the receipt says so.
	var sockMounts int
	for _, m := range gw.Mounts {
		if m.Target != DockerSocket {
			continue
		}
		sockMounts++
		if m.Source == DockerSocket || !strings.HasSuffix(m.Source, "/docker.sock") {
			t.Errorf("gateway mounts %q at the daemon socket, want the run's filter", m.Source)
		}
	}
	if sockMounts != 1 {
		t.Errorf("gateway has %d daemon-socket mounts, want 1", sockMounts)
	}
	if !gw.DisableSELinuxLabel {
		t.Error("the gateway lost its SELinux exemption for the socket")
	}
	if line := env.CapabilitiesEnforced["daemon"]; !strings.Contains(line, "only through this run's filter") {
		t.Errorf("receipt daemon line = %q", line)
	}

	// Task reached the agent's stdin.
	if got := f.Foreground["-agent"].GotStdin; got != "the task text" {
		t.Errorf("agent stdin = %q", got)
	}

	// §R11 invariant 6: run dir gone, secrets.env gone; receipts remain.
	if _, err := os.Stat(filepath.Join(p.RunRoot, "runs", "cafe0000")); !os.IsNotExist(err) {
		t.Error("run dir survived teardown")
	}
	for _, rel := range []string{"result.json", "receipts/events.ndjson", "receipts/run-plan.json", "receipts/meta.json", "receipts/task.txt", "receipts/gateway.log", "reports"} {
		if _, err := os.Stat(filepath.Join(p.OutDir, rel)); err != nil {
			t.Errorf("output folder missing %s: %v", rel, err)
		}
	}
	// §R11 invariant 7: no receipt file carries the token or secret.
	filepath.Walk(p.OutDir, func(path string, info os.FileInfo, err error) error { //nolint:errcheck
		if err != nil || info.IsDir() {
			return err
		}
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), "sekret-value-1234") || strings.Contains(string(b), agent.Env["MCP_GATEWAY_TOKEN"]) {
			t.Errorf("receipt %s leaks a secret or token", path)
		}
		return nil
	})
}

func TestRunKeepRunDirStillDeletesSecrets(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	p.KeepRunDir = true
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(p.RunRoot, "runs", "cafe0000")
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("run dir should be kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "cfg", "gateway", "secrets.env")); !os.IsNotExist(err) {
		t.Fatal("secrets.env survived --keep-run-dir")
	}
	// Rendered catalog must carry real paths.
	b, err := os.ReadFile(filepath.Join(runDir, "cfg", "gateway", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "@@RUN_") {
		t.Error("rendered catalog still has placeholders")
	}
	if !strings.Contains(string(b), runDir) {
		t.Error("rendered catalog lacks the run workspace path")
	}
}

// TestInputsStagedIntoRun pins RUNNER-PLAN-2 §P4: --file items land under
// runs/<id>/inputs, basename-rooted, and the rendered plan carries no
// unresolved inputs placeholder.
func TestInputsStagedIntoRun(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	p.KeepRunDir = true
	inputFile := filepath.Join(t.TempDir(), "incident.json")
	if err := os.WriteFile(inputFile, []byte(`{"id":42}`), 0o644); err != nil {
		t.Fatal(err)
	}
	inputDir := filepath.Join(t.TempDir(), "attachments")
	if err := os.MkdirAll(inputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inputDir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.InputPaths = []string{inputFile, inputDir}
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(p.RunRoot, "runs", "cafe0000")
	for _, rel := range []string{"inputs/incident.json", "inputs/attachments/a.txt"} {
		if _, err := os.Stat(filepath.Join(runDir, rel)); err != nil {
			t.Errorf("staged input missing: %s (%v)", rel, err)
		}
	}
	b, err := os.ReadFile(filepath.Join(runDir, "cfg", "gateway", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), runDir+"/inputs:/inputs:ro") {
		t.Errorf("rendered catalog lacks the ro inputs mount:\n%s", b)
	}
}

func TestInputsDuplicateBasenameRefused(t *testing.T) {
	p, f := fixture(t)
	d1, d2 := t.TempDir(), t.TempDir()
	for _, d := range []string{d1, d2} {
		if err := os.WriteFile(filepath.Join(d, "same.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p.InputPaths = []string{filepath.Join(d1, "same.txt"), filepath.Join(d2, "same.txt")}
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitCLI || !strings.Contains(err.Error(), "basename") {
		t.Fatalf("duplicate basenames must refuse with exit %d, got %v", ExitCLI, err)
	}
}

// TestSecretOverridesBeatEnvironment pins the P2 §P2 resolution order: the
// MCP profile's value (SecretOverrides) satisfies a secret the environment
// does not have.
func TestSecretOverridesBeatEnvironment(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	_ = os.Unsetenv("ENGINE_TEST_TOKEN")
	p.SecretOverrides = map[string]string{"ENGINE_TEST_TOKEN": "from-profile"}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("override should satisfy the secret: %v", err)
	}
	if env.Status != "ok" {
		t.Fatalf("status = %s", env.Status)
	}
	// The override value must obey invariant 3 exactly like env-sourced
	// values: never in any container env or argv.
	for _, c := range f.Calls {
		for k, v := range c.Run.Env {
			if strings.Contains(v, "from-profile") {
				t.Errorf("profile secret leaked into %s env %s", c.Run.Name, k)
			}
		}
	}
}

func TestMissingSecretRefusesBeforeDocker(t *testing.T) {
	p, f := fixture(t)
	_ = os.Unsetenv("ENGINE_TEST_TOKEN")
	env, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitPolicy {
		t.Fatalf("want exit %d, got %v", ExitPolicy, err)
	}
	if env.Status != "error" || !strings.Contains(env.Error, "ENGINE_TEST_TOKEN") {
		t.Fatalf("envelope must name the missing secret: %+v", env)
	}
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "Run") || strings.HasPrefix(op, "Network") {
			t.Fatalf("docker touched before secret check: %v", f.Ops())
		}
	}
}

func TestMissingModelKeyRefuses(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	_ = os.Unsetenv("ANTHROPIC_API_KEY")
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitPolicy {
		t.Fatalf("want exit %d, got %v", ExitPolicy, err)
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("error must name the variable: %v", err)
	}
}

func TestGatewayProbeFailureClassifiedAndTornDown(t *testing.T) {
	p, f := fixture(t)
	f.Foreground = map[string]*dockerx.FakeForeground{"-probe": {ExitCode: 1}}
	f.LogsByName = map[string]string{"agentry-cafe0000-gateway": "bind: cannot listen\n"}
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitGateway {
		t.Fatalf("want exit %d, got %v", ExitGateway, err)
	}
	if !strings.Contains(err.Error(), "cannot listen") {
		t.Errorf("gateway diagnostics missing from error: %v", err)
	}
	ops := f.Ops()
	joined := strings.Join(ops, " ")
	for _, want := range []string{"Stop:agentry-cafe0000-gateway", "RemoveForce:agentry-cafe0000-proxy", "NetworkRemove:agentry-cafe0000-net"} {
		if !strings.Contains(joined, want) {
			t.Errorf("teardown after failure missing %s: %v", want, ops)
		}
	}
}

func TestAgentNonZeroExitIsHarnessClass(t *testing.T) {
	p, f := fixture(t)
	f.Foreground = map[string]*dockerx.FakeForeground{
		"-probe": {ExitCode: 0},
		"-agent": {Stdout: `{"type":"error","message":{"content":"model refused"}}` + "\n", ExitCode: 3},
	}
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitHarness {
		t.Fatalf("want exit %d, got %v", ExitHarness, err)
	}
	if !strings.Contains(err.Error(), "model refused") {
		t.Errorf("harness error detail missing: %v", err)
	}
}

func TestTurnBudgetKillsRun(t *testing.T) {
	p, f := fixture(t)
	p.Budgets.MaxTurns = 2
	many := strings.Repeat(`{"type":"tool_call"}`+"\n", 5)
	f.Foreground = map[string]*dockerx.FakeForeground{
		"-probe": {ExitCode: 0},
		"-agent": {Stdout: many, Block: true},
	}
	env, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitBudget {
		t.Fatalf("want exit %d, got %v", ExitBudget, err)
	}
	if env.Usage.Turns < 3 {
		t.Errorf("turns = %d, want >2", env.Usage.Turns)
	}
}

func TestTimeoutKillsRun(t *testing.T) {
	p, f := fixture(t)
	p.Budgets.TimeoutSeconds = 1
	f.Foreground = map[string]*dockerx.FakeForeground{
		"-probe": {ExitCode: 0},
		"-agent": {Block: true},
	}
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitTimeout {
		t.Fatalf("want exit %d, got %v", ExitTimeout, err)
	}
}

func TestJSONResultKindEnforced(t *testing.T) {
	p, _ := fixture(t)
	// Recompile with an output schema so ResultKind flips to json.
	if err := os.WriteFile(filepath.Join(p.WorkspaceSrc, compile.OutputSchemaFile), []byte(`{"type":"object","required":["answer"],"properties":{"answer":{"type":"integer"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse([]byte(engineManifest))
	if err != nil {
		t.Fatal(err)
	}
	planDir := filepath.Join(t.TempDir(), "plan")
	plan, err := compile.Compile(compile.Inputs{
		Manifest: m, Digest: "sha256:feedface", Source: "local",
		WorkspaceDir: p.WorkspaceSrc, Policy: p.Policy, PolicySHA: "sha",
		RunnerImage: "agentry-runner:test",
	}, planDir)
	if err != nil {
		t.Fatal(err)
	}
	p.Plan, p.PlanDir = plan, planDir

	t.Run("garbage output is exit 17", func(t *testing.T) {
		f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
			"-probe": {ExitCode: 0},
			"-agent": {Stdout: `{"type":"agent_choice","message":{"content":"not json at all"}}` + "\n"},
		}}
		q := p
		q.OutDir = t.TempDir()
		_, err := Run(context.Background(), f, q)
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != ExitOutputSchema {
			t.Fatalf("want exit %d, got %v", ExitOutputSchema, err)
		}
	})
	// DR-8 closed (agentry-cli#11): valid JSON that violates the schema is
	// exit 17 too, with the violation named.
	t.Run("schema violation is exit 17", func(t *testing.T) {
		line, _ := json.Marshal(map[string]any{"type": "agent_choice", "message": map[string]any{"content": `{"answer":"forty-two"}`}})
		f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
			"-probe": {ExitCode: 0},
			"-agent": {Stdout: string(line) + "\n"},
		}}
		q := p
		q.OutDir = t.TempDir()
		_, err := Run(context.Background(), f, q)
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != ExitOutputSchema || !strings.Contains(err.Error(), "does not satisfy") {
			t.Fatalf("want exit %d naming the violation, got %v", ExitOutputSchema, err)
		}
	})
	t.Run("fenced json accepted", func(t *testing.T) {
		final := map[string]any{
			"type":    "agent_choice",
			"message": map[string]any{"content": "```json\n{\"answer\":42}\n```"},
		}
		line, err := json.Marshal(final)
		if err != nil {
			t.Fatal(err)
		}
		f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
			"-probe": {ExitCode: 0},
			"-agent": {Stdout: string(line) + "\n"},
		}}
		q := p
		q.OutDir = t.TempDir()
		env, err := Run(context.Background(), f, q)
		if err != nil {
			t.Fatalf("fenced JSON should parse: %v", err)
		}
		if string(env.Result) != `{"answer":42}` {
			t.Errorf("result = %s", env.Result)
		}
	})
}

// serverFixture is fixture with one declared MCP server in the plan and a
// server store, the shape a run with capabilities.mcpServers has.
func serverFixture(t *testing.T) (Params, *dockerx.Fake) {
	t.Helper()
	p, f := fixture(t)
	srv := spec.MCPServerSpec{
		Name: "mcp-npm-scope-server-x", Source: "npm", Package: "@scope/server-x", Version: "1.2.3",
		Image: "node:22-alpine", Command: []string{"/opt/agentry-mcp/npm-scope-server-x/node_modules/.bin/server-x"},
		StoreSubdir: "npm-scope-server-x", Egress: []string{"api.example"},
	}
	plan, err := compile.Compile(compile.Inputs{
		Manifest: mustManifest(t), Digest: "sha256:feedface", Source: "local",
		WorkspaceDir: p.WorkspaceSrc, Policy: p.Policy, PolicySHA: "sha", RunnerImage: "agentry-runner:test",
		MCPServers: []spec.MCPServerSpec{srv},
	}, p.PlanDir)
	if err != nil {
		t.Fatal(err)
	}
	p.Plan = plan
	p.ServersDir = t.TempDir()
	return p, f
}

func mustManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(engineManifest))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestDeclaredServerThatDidNotStartFailsTheRun (agentry-cli#105): the
// gateway logs "Can't start <server>" and carries on without it; the
// engine must not — a run without a declared server is not the run the
// contract described, so it is the gateway/environment failure class,
// with the gateway's own line in the error and no agent container run.
func TestDeclaredServerThatDidNotStartFailsTheRun(t *testing.T) {
	p, f := serverFixture(t)
	happyFake(f)
	f.LogsByName = map[string]string{"agentry-cafe0000-gateway": strings.Join([]string{
		"- Those servers are enabled: agentry-tools, mcp-npm-scope-server-x",
		"- mcp-npm-scope-server-x: Error: cannot open /data: read-only file system",
		"  > Can't start mcp-npm-scope-server-x: failed to connect: calling \"initialize\": EOF",
		"> 3 tools listed in 1.2s",
	}, "\n") + "\n"}
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitGateway {
		t.Fatalf("want exit %d, got %v", ExitGateway, err)
	}
	if !strings.Contains(err.Error(), "@scope/server-x") || !strings.Contains(err.Error(), "Can't start mcp-npm-scope-server-x") || !strings.Contains(err.Error(), "read-only file system") {
		t.Errorf("error must name the server, quote the gateway and carry the server's own output: %v", err)
	}
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "RunForeground:agentry-cafe0000-agent") {
			t.Error("the agent ran without its declared server")
		}
	}
}

// TestDeclaredServerStartedRunsTheAgent: the same plan with a clean
// gateway log runs to completion.
func TestDeclaredServerStartedRunsTheAgent(t *testing.T) {
	p, f := serverFixture(t)
	happyFake(f)
	f.LogsByName = map[string]string{"agentry-cafe0000-gateway": "- Those servers are enabled: agentry-tools, mcp-npm-scope-server-x\n> 4 tools listed in 1.2s\n"}
	env, err := Run(context.Background(), f, p)
	if err != nil || env.Status != "ok" {
		t.Fatalf("run with a started server failed: %v", err)
	}
}

// TestReceiptNamesResolvedExecPackages (agentry-cli#100): the resolved
// versions the exec image build recorded are appended to the receipt's
// exec line, after the compiled text, so the receipt names the packages
// as installed; a run with none appends nothing.
func TestReceiptNamesResolvedExecPackages(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	p.Plan.Enforcement["exec"] = "run_command restricted to argv0 allowlist + shim PATH {git}; …"
	p.ExecPackages = []string{"git-2.49.1-r0", "jq-1.8.2-r0"}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	got := env.CapabilitiesEnforced["exec"]
	if !strings.HasPrefix(got, "run_command restricted") || !strings.HasSuffix(got, "; packages resolved at image build: git-2.49.1-r0, jq-1.8.2-r0") {
		t.Fatalf("exec line = %q", got)
	}
	// The plan's own map is not rewritten under the caller's feet.
	if strings.Contains(p.Plan.Enforcement["exec"], "resolved at image build") {
		t.Fatal("the plan's enforcement map was mutated")
	}

	p2, f2 := fixture(t)
	happyFake(f2)
	env2, err := Run(context.Background(), f2, p2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(env2.CapabilitiesEnforced["exec"], "resolved at image build") {
		t.Fatalf("no packages, yet the receipt claims a build: %q", env2.CapabilitiesEnforced["exec"])
	}
}

// TestRunInterruptedTearsDownAndWritesEnvelope (agentry-cli#124): a
// cancelled context — main's SIGINT — ends the agent, runs the teardown
// on the way out, and writes result.json with status error and the word
// "interrupted" under the cli class, so an interrupted run leaves its
// receipts where a process killed by the signal left nothing.
func TestRunInterruptedTearsDownAndWritesEnvelope(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	f.Foreground["-agent"] = &dockerx.FakeForeground{Block: true}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	env, err := Run(ctx, f, p)
	if err == nil {
		t.Fatal("an interrupted run must fail")
	}
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitCLI || ce.Err.Error() != "interrupted" {
		t.Fatalf("classification: %v", err)
	}
	if env == nil || env.Status != "error" || env.Error != "interrupted" {
		t.Fatalf("envelope: %+v", env)
	}
	b, rerr := os.ReadFile(filepath.Join(p.OutDir, "result.json"))
	if rerr != nil {
		t.Fatalf("result.json not written: %v", rerr)
	}
	if !strings.Contains(string(b), `"exitClass": "cli"`) || !strings.Contains(string(b), `"error": "interrupted"`) {
		t.Fatalf("result.json: %s", b)
	}
	ops := f.Ops()
	joined := strings.Join(ops, "\n")
	if !strings.Contains(joined, "Stop:agentry-cafe0000-gateway") || !strings.Contains(joined, "NetworkRemove:agentry-cafe0000-net") {
		t.Fatalf("teardown did not run: %v", ops)
	}
}

// TestReceiptsRedactSecretsFromLogs (agentry-cli#109): a declared server
// that prints its environment, an agent whose stderr echoes a credential
// and a gateway line that quotes one all leave their values out of the
// receipts — gateway.log, proxy.log, agent-stderr.log, result.json and the
// run's error — while a receipt that carries none is written byte for
// byte as the container produced it.
func TestReceiptsRedactSecretsFromLogs(t *testing.T) {
	const secret, apiKey, serverSecret = "sekret-value-1234", "sk-test-key", "server-sekret-5678"
	t.Run("start failure quotes the server's output masked", func(t *testing.T) {
		p, f := serverFixture(t)
		happyFake(f)
		// The server's own secret is resolved from the same source as the
		// agent's and masked the same way.
		p.Plan.MCPServers[0].Secrets = []string{"SERVER_TEST_TOKEN"}
		t.Setenv("SERVER_TEST_TOKEN", serverSecret)
		f.LogsByName = map[string]string{"agentry-cafe0000-gateway": strings.Join([]string{
			"- Those servers are enabled: agentry-tools, mcp-npm-scope-server-x",
			"- mcp-npm-scope-server-x: env: ENGINE_TEST_TOKEN=" + secret + " SERVER_TEST_TOKEN=" + serverSecret,
			"- mcp-npm-scope-server-x: Error: 401 from api.example with token " + serverSecret,
			"  > Can't start mcp-npm-scope-server-x: failed to connect: calling \"initialize\": EOF",
		}, "\n") + "\n"}
		env, err := Run(context.Background(), f, p)
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != ExitGateway {
			t.Fatalf("want exit %d, got %v", ExitGateway, err)
		}
		for name, text := range map[string]string{"error": err.Error(), "envelope": env.Error} {
			if strings.Contains(text, secret) || strings.Contains(text, serverSecret) {
				t.Errorf("%s carries a secret: %s", name, text)
			}
			if !strings.Contains(text, "ENGINE_TEST_TOKEN=[redacted] SERVER_TEST_TOKEN=[redacted]") || !strings.Contains(text, "token [redacted]") || !strings.Contains(text, "Can't start mcp-npm-scope-server-x") {
				t.Errorf("%s lost the masked tail or the gateway's line: %s", name, text)
			}
		}
		b, rerr := os.ReadFile(filepath.Join(p.OutDir, "result.json"))
		if rerr != nil || strings.Contains(string(b), secret) || strings.Contains(string(b), serverSecret) {
			t.Errorf("result.json: %v %s", rerr, b)
		}
	})
	t.Run("receipts of a completed run", func(t *testing.T) {
		p, f := fixture(t)
		happyFake(f)
		f.Foreground["-agent"].Stdout = strings.Replace(f.Foreground["-agent"].Stdout, "final answer", "the key is "+apiKey+" and the token "+secret, 1)
		f.Foreground["-agent"].Stderr = "harness boot\nDEBUG env ENGINE_TEST_TOKEN=" + secret + "\nANTHROPIC_API_KEY=" + apiKey + "\n"
		f.LogsByName = map[string]string{
			"agentry-cafe0000-gateway": "- Those servers are enabled: agentry-tools\n- agentry-tools: fetch: Authorization: Bearer " + secret + "\n",
			"agentry-cafe0000-proxy":   "allow api.example:443\n",
		}
		env, err := Run(context.Background(), f, p)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if got, want := string(env.Result), `"the key is [redacted] and the token [redacted]"`; got != want {
			t.Errorf("result = %s, want %s", got, want)
		}
		read := func(name string) string {
			b, err := os.ReadFile(filepath.Join(env.Receipts.Dir, name))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return string(b)
		}
		if got := read("gateway.log"); strings.Contains(got, secret) || !strings.Contains(got, "Bearer [redacted]") {
			t.Errorf("gateway.log = %q", got)
		}
		if got := read("agent-stderr.log"); got != "harness boot\nDEBUG env ENGINE_TEST_TOKEN=[redacted]\nANTHROPIC_API_KEY=[redacted]\n" {
			t.Errorf("agent-stderr.log = %q", got)
		}
		// The event stream is the run's primary receipt: masked line by
		// line as it arrives, the harness's JSON still parseable.
		if got := read("events.ndjson"); strings.Contains(got, secret) || strings.Contains(got, apiKey) || !strings.Contains(got, `"content":"the key is [redacted] and the token [redacted]"`) {
			t.Errorf("events.ndjson = %q", got)
		}
		// Nothing to mask: byte-identical to what the container produced.
		if got := read("proxy.log"); got != "allow api.example:443\n" {
			t.Errorf("proxy.log = %q", got)
		}
		b, err := os.ReadFile(filepath.Join(p.OutDir, "result.json"))
		if err != nil || strings.Contains(string(b), secret) || strings.Contains(string(b), apiKey) {
			t.Errorf("result.json leaks: %v %s", err, b)
		}
	})
}

// A policy runtime reaches every container class that holds under it
// (DR-23, agentry-cli#221): the agent — the stranger's code — and the
// gateway's health probe, each with the run network's aliases pinned; the
// runner's own proxy and gateway stay on the daemon's default, and the
// receipt names each class and why; without one, no container is asked
// for a runtime and the receipt says nothing.
func TestRunUnderAPolicyRuntime(t *testing.T) {
	p, f := fixtureWith(t, func(pol *policy.Policy) { pol.Docker.Runtime = "runsc" })
	happyFake(f)
	// A plan compiled before agentry-cli#221 says less; the receipt's line
	// is the run's own.
	p.Plan.Enforcement["runtime"] = "runsc: a plan's older line"
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if p.Plan.Runtime() != "runsc" {
		t.Fatalf("plan runtime = %q", p.Plan.Runtime())
	}
	for _, sandboxed := range []string{"-agent", "-probe"} {
		c, ok := f.FindRun(sandboxed)
		if !ok || c.Runtime != "runsc" {
			t.Fatalf("%s runtime = %q (found %v), want runsc", sandboxed, c.Runtime, ok)
		}
		if len(c.ExtraHosts) != 2 || !strings.HasPrefix(c.ExtraHosts[0], "gateway:") || !strings.HasPrefix(c.ExtraHosts[1], "llm-proxy:") {
			t.Fatalf("%s extra hosts = %v, want the gateway and llm-proxy aliases pinned", sandboxed, c.ExtraHosts)
		}
	}
	for _, infra := range []string{"-proxy", "-gateway"} {
		c, ok := f.FindRun(infra)
		if !ok {
			t.Fatalf("no %s container ran", infra)
		}
		if c.Runtime != "" || len(c.ExtraHosts) != 0 {
			t.Errorf("%s holds no sandbox (DR-23) and must stay on the daemon's default: runtime=%q hosts=%v", c.Name, c.Runtime, c.ExtraHosts)
		}
	}
	line := env.CapabilitiesEnforced["runtime"]
	for _, want := range []string{"runsc: created with docker --runtime runsc: the agent container", "the gateway's health probe",
		"the tools server", "given runsc by the run's docker filter", "daemon's default runtime", "the llm-proxy (", "the gateway ("} {
		if !strings.Contains(line, want) {
			t.Errorf("receipt runtime line lacks %q: %q", want, line)
		}
	}
	if strings.Contains(line, "older line") || strings.Contains(line, "relays") {
		t.Errorf("receipt runtime line = %q: the plan's text, or relays a run without servers never had", line)
	}
}

// The receipt's runtime line follows the run: relays and servers when the
// plan declares them, and, where the docker filter does not run, the
// gateway's containers on the daemon's default.
func TestTheRuntimeLineNamesEachClass(t *testing.T) {
	p, _ := fixtureWith(t, func(pol *policy.Policy) { pol.Docker.Runtime = "runsc" })
	p.Plan.MCPServers = []spec.MCPServerSpec{{Name: "osv", Egress: []string{"api.osv.dev"}}}
	line := runtimeLine(p)
	for _, want := range []string{"the MCP server installer", "the tools server and the declared MCP servers", "relay links pinned", "the MCP server install proxy (", "the relays ("} {
		if !strings.Contains(line, want) {
			t.Errorf("line lacks %q: %q", want, line)
		}
	}
	// An interactive session names its store's one-shot only when it runs
	// one; a host harness has no agent container, and says so.
	p.Plan.Mode = "interactive"
	p.Plan.Harness.Session = &spec.SessionSpec{}
	if line := runtimeLine(p); strings.Contains(line, "one-shot") {
		t.Errorf("a session without a store owner: %q", line)
	}
	p.Plan.Harness.Session.StoreOwner = "agent"
	if line := runtimeLine(p); !strings.Contains(line, "the session store's one-shot") {
		t.Errorf("a session with a store owner: %q", line)
	}
	p.Plan.Mode, p.Plan.Harness.Session = "autonomous", nil
	p.Plan.Harness.Host = &spec.HostProcessSpec{}
	if line := runtimeLine(p); strings.Contains(line, "the agent container") || !strings.Contains(line, "a host process, under no container runtime") {
		t.Errorf("a host harness: %q", line)
	}
	p.Plan.Harness.Host = nil
	defer func(was bool) { filterSupported = was }(filterSupported)
	filterSupported = false
	if line := runtimeLine(p); !strings.Contains(line, "take the daemon's default: the docker filter") || strings.Contains(line, "given runsc") {
		t.Errorf("without the filter: %q", line)
	}
}

func TestRunWithoutAPolicyRuntimeAsksForNone(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c.Op, "Run") && (c.Run.Runtime != "" || len(c.Run.ExtraHosts) != 0) {
			t.Errorf("%s created with a runtime or pinned hosts nobody set: %q %v", c.Run.Name, c.Run.Runtime, c.Run.ExtraHosts)
		}
		if c.Op == "ContainerIP" {
			t.Errorf("a run without a runtime inspected a container's address")
		}
	}
	if _, ok := env.CapabilitiesEnforced["runtime"]; ok {
		t.Fatal("the receipt names a runtime the policy never set")
	}
	if p.Plan.Docker != nil {
		t.Fatal("a plan compiled without docker.runtime carries a docker block")
	}
}

// Every container the engine starts drops every capability, cannot gain
// privileges and is bounded by the policy's limits, the defaults when it
// sets none; the proxy, the probe and the agent run on a read-only root
// (the agent's writable paths the adapter's); the gateway passes the
// limits to what it launches; the receipt names it all (agentry-cli#163).
func TestRunContainersAreHardened(t *testing.T) {
	for _, tc := range []struct {
		name string
		lim  policy.Limits
		want policy.Limits
	}{
		{"defaults", policy.Limits{}, policy.Limits{}.WithDefaults()},
		{"the policy's", policy.Limits{Memory: "512m", CPUs: 3, Pids: 64}, policy.Limits{Memory: "512m", CPUs: 3, Pids: 64}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, f := fixtureWith(t, func(pol *policy.Policy) { pol.Docker.Limits = tc.lim })
			happyFake(f)
			env, err := Run(context.Background(), f, p)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			for _, name := range []string{"-proxy", "-gateway", "-probe", "-agent"} {
				c, ok := f.FindRun(name)
				if !ok {
					t.Fatalf("no %s container ran", name)
				}
				wantAdd := ""
				if name == "-gateway" {
					wantAdd = "DAC_READ_SEARCH"
				}
				if len(c.CapDrop) != 1 || c.CapDrop[0] != "ALL" || strings.Join(c.CapAdd, ",") != wantAdd || !c.NoNewPrivileges {
					t.Errorf("%s: cap-drop %v cap-add %v no-new-privileges %v", c.Name, c.CapDrop, c.CapAdd, c.NoNewPrivileges)
				}
				if c.Memory != tc.want.Memory || c.CPUs != tc.want.CPUs || c.PidsLimit != tc.want.Pids {
					t.Errorf("%s: memory %q cpus %d pids %d, want %+v", c.Name, c.Memory, c.CPUs, c.PidsLimit, tc.want)
				}
				wantRO := name != "-gateway"
				if c.ReadOnly != wantRO || (wantRO && len(c.Tmpfs) == 0) {
					t.Errorf("%s: read-only %v tmpfs %v, want read-only %v", c.Name, c.ReadOnly, c.Tmpfs, wantRO)
				}
			}
			agent, _ := f.FindRun("-agent")
			if strings.Join(agent.Tmpfs, ",") != strings.Join(p.Plan.Harness.Agent.Tmpfs, ",") || len(agent.Tmpfs) == 0 {
				t.Errorf("agent tmpfs %v, want the adapter's %v", agent.Tmpfs, p.Plan.Harness.Agent.Tmpfs)
			}
			gw, _ := f.FindRun("-gateway")
			args := strings.Join(gw.Argv, " ")
			if !strings.Contains(args, fmt.Sprintf("--cpus %d --memory %s", tc.want.CPUs, tc.want.Memory)) {
				t.Errorf("gateway argv does not bound what it launches: %s", args)
			}
			line := env.CapabilitiesEnforced["containers"]
			if !strings.Contains(line, "all capabilities dropped, no-new-privileges") ||
				!strings.Contains(line, fmt.Sprintf("memory %s, cpus %d, pids %d", tc.want.Memory, tc.want.CPUs, tc.want.Pids)) ||
				!strings.Contains(line, "the agent (tmpfs /tmp, /work, /home/docker-agent)") || !strings.Contains(line, "DAC_READ_SEARCH alone") {
				t.Errorf("receipt containers line = %q", line)
			}
		})
	}
}

// TestRunPlanReceiptIsThePlanThatRan (agentry-cli#266): receipts/run-plan.json
// is the plan the run executed, byte for byte the encoding runPlanSha256
// hashes — including the run's own additions to the verified plan (the MCP
// vulnerability verdict lands on Enforcement after the compile) — not
// plan.json re-read from the install dir.
func TestRunPlanReceiptIsThePlanThatRan(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	p.Plan.Enforcement["mcpServers.vulnerabilities"] = "run-time verdict, not on disk"
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("Run: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(p.OutDir, "receipts", "run-plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := p.Plan.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if got := sha256Hex(b); got != want {
		t.Fatalf("run-plan.json hashes to %s, the receipt names %s", got, want)
	}
	if !strings.Contains(string(b), "run-time verdict") {
		t.Fatal("run-plan.json is missing the run's own enforcement line")
	}
}

// TestRunRefusesAPlanFileChangedAfterVerification (agentry-cli#266): a
// generated plan file is held to the verified plan's hash as the run reads
// it, so an edit made after ensurePlan verified the plan — or a file the
// plan never listed — is refused with the verify class, not mounted.
func TestRunRefusesAPlanFileChangedAfterVerification(t *testing.T) {
	for name, edit := range map[string]func(planDir string) error{
		"changed": func(planDir string) error {
			return os.WriteFile(filepath.Join(planDir, "gateway", "catalog.yaml"), []byte("tampered\n"), 0o644)
		},
		"added": func(planDir string) error {
			return os.WriteFile(filepath.Join(planDir, "gateway", "extra.yaml"), []byte("x: 1\n"), 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, f := fixture(t)
			happyFake(f)
			if err := edit(p.PlanDir); err != nil {
				t.Fatal(err)
			}
			_, err := Run(context.Background(), f, p)
			var ce *ClassifiedError
			if !errors.As(err, &ce) || ce.Code != ExitVerify || !strings.Contains(err.Error(), "changed after the plan was verified") {
				t.Fatalf("want the verify-class refusal, got %v", err)
			}
			if _, ok := f.FindRun("gateway"); ok {
				t.Fatal("the gateway was started on a changed plan")
			}
		})
	}
}

// The envelope names the operator policy file the run enforced, how its
// path was chosen, and the digest the plan recorded for it
// (agentry-cli#152): a run under --policy and a run under a relocated
// runner home each say so, and a caller that names no file leaves the
// block out, so older readers see nothing new.
func TestEnvelopeNamesThePolicyFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		flag string
		home bool
		want string
	}{
		{"flag", "custom-policy.yaml", false, receipt.PolicySourceFlag},
		{"env", "", true, receipt.PolicySourceEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv(policy.HomeEnv, "")
			if tc.home {
				t.Setenv(policy.HomeEnv, t.TempDir())
			}
			file, err := policy.Resolve(tc.flag)
			if err != nil {
				t.Fatal(err)
			}
			path := file.Path
			p, f := fixture(t)
			happyFake(f)
			p.PolicyFile = &file
			env, err := Run(context.Background(), f, p)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if env.Policy == nil || env.Policy.Path != path || env.Policy.Source != tc.want || env.Policy.SHA256 != p.Plan.PolicySHA256 || env.Policy.SHA256 == "" {
				t.Fatalf("policy = %+v, want path %s source %s sha %s", env.Policy, path, tc.want, p.Plan.PolicySHA256)
			}
			raw, err := env.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Policy map[string]string `json:"policy"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			if wire.Policy["path"] != path || wire.Policy["source"] != tc.want || wire.Policy["sha256"] != p.Plan.PolicySHA256 {
				t.Fatalf("envelope json policy = %v", wire.Policy)
			}
		})
	}
	p, f := fixture(t)
	happyFake(f)
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, _ := env.Marshal()
	if env.Policy != nil || strings.Contains(string(raw), `"policy":`) {
		t.Fatalf("a caller that names no policy file got a policy block: %s", raw)
	}
}
