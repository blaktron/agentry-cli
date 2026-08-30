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

const engineManifest = `schemaVersion: "1"
name: engine-test
namespace: demo
kind: skill
version: 1.0.0
summary: "engine lifecycle fixture"
entrypoints:
  skill: SKILL.md
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

	// Execute order (§R6): network → proxy (+bridge) → gateway (+bridge)
	// → probe → agent; teardown afterwards in reverse.
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
		"NetworkConnect:bridge<-agentry-cafe0000-gateway",
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
	if err := os.WriteFile(filepath.Join(p.WorkspaceSrc, compile.OutputSchemaFile), []byte(`{"type":"object"}`), 0o644); err != nil {
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
