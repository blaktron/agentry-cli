package compile

// Target hosts: a release that declares one compiles a host link, a catalog
// entry of its own behind one relay a host, whose run-time values are
// placeholders, and nothing else of the plan moves for it.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
	"github.com/blaktron/agentry-cli/runnerimage/hostlink"
)

const hostManifest = `schemaVersion: "2"
name: server-helper
namespace: acme
modes: [autonomous, interactive]
version: 1.0.0
summary: "target host compile fixture"
intake:
  - id: server
    prompt: "The server to set up"
    type: host
    required: true
capabilities:
  targetHosts: [server]
entrypoints:
  instruction: SKILL.md
publish:
  include: [agent.yaml, SKILL.md]
`

func hostInputs(t *testing.T, doc string) Inputs {
	t.Helper()
	ws := t.TempDir()
	for name, body := range map[string]string{"agent.yaml": doc, "SKILL.md": "Set the server up.\n"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m, err := manifest.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return Inputs{Manifest: m, Digest: "sha256:beef", Source: "local", WorkspaceDir: ws, Policy: policy.Default(),
		PolicySHA: "policysha", RunnerImage: "agentry-runner:test"}
}

func TestCompileRendersTheHostLink(t *testing.T) {
	planDir := t.TempDir()
	plan, err := Compile(hostInputs(t, hostManifest), planDir)
	if err != nil {
		t.Fatal(err)
	}
	want := targethost.HostLinkImage("agentry-runner:test", policy.DefaultHostlinkImage)
	if plan.HostLink == nil || plan.HostLink.Image != want.Tag || plan.HostLink.Base != policy.DefaultHostlinkImage {
		t.Fatalf("plan.HostLink = %+v", plan.HostLink)
	}
	if !slices.Equal(plan.Capabilities.TargetHosts, []string{"server"}) {
		t.Fatalf("capabilities.targetHosts = %v", plan.Capabilities.TargetHosts)
	}

	hl := catalogServer(t, planDir, hostlink.ServerName)
	if hl["type"] != "server" || hl["image"] != want.Tag || hl["user"] != spec.PHHostLinkUser {
		t.Fatalf("the host link's entry: %v", hl)
	}
	if got := strs(hl["command"]); !slices.Equal(got, []string{"hostlink"}) {
		t.Fatalf("command = %v", got)
	}
	// One relay, to the address and port the run resolves, and no other
	// network (the half of the network rule that is the plan's).
	if got := strs(hl["allowHosts"]); !slices.Equal(got, []string{"@@RUN_HOST_server@@/tcp"}) || hl["disableNetwork"] != nil {
		t.Fatalf("allowHosts = %v", got)
	}
	wantVols := []string{
		spec.PHCfg + "/gateway/hostspec.json:" + hostlink.SpecMount + ":ro",
		spec.PHCfg + "/hosts:" + hostlink.HostsMount + ":ro",
		spec.PHCfg + "/hosts/passwd:/etc/passwd:ro",
		spec.PHCfg + "/hostlog:" + hostlink.LogMount + ":rw",
		spec.PHCfg + "/hostfiles:" + hostlink.FilesMount + ":rw",
	}
	if got := strs(hl["volumes"]); !slices.Equal(got, wantVols) {
		t.Fatalf("volumes = %v, want %v", got, wantVols)
	}
	if hl["secrets"] != nil || hl["env"] != nil {
		t.Fatalf("the host link's sign-in is in files, never the gateway's secrets or env: %v", hl)
	}

	// The tools server shares the copy folder inside the scratch folder,
	// and gains no network.
	tools := catalogServer(t, planDir, spec.ToolServerName)
	if !slices.Contains(strs(tools["volumes"]), spec.PHCfg+"/hostfiles:"+hostlink.FilesPath+":rw") || tools["disableNetwork"] != true {
		t.Fatalf("the tools server: %v", tools)
	}

	var hs hostlink.Spec
	b, err := os.ReadFile(filepath.Join(planDir, "gateway", "hostspec.json")) //nolint:gosec // test plan dir
	if err != nil || json.Unmarshal(b, &hs) != nil || !slices.Equal(hs.Hosts, []string{"server"}) || hs.MaxResultBytes == 0 {
		t.Fatalf("hostspec.json %s, %v", b, err)
	}
	if _, err := hostlink.ParseSpec(b); err != nil {
		t.Fatalf("the host link would not read its spec: %v", err)
	}

	toolsYAML, _ := os.ReadFile(filepath.Join(planDir, "gateway", "tools.yaml")) //nolint:gosec // test plan dir
	for _, tool := range hostlink.Tools {
		if !strings.Contains(string(toolsYAML), tool) {
			t.Errorf("tools.yaml does not allow %s:\n%s", tool, toolsYAML)
		}
	}
	if !slices.Contains(plan.Gateway.Servers, hostlink.ServerName) || !slices.Contains(plan.Gateway.Args, "--block-network") || !slices.Contains(plan.Gateway.Args, "--block-secrets") {
		t.Fatalf("gateway: %v %v", plan.Gateway.Servers, plan.Gateway.Args)
	}
	for _, env := range plan.Gateway.Env {
		if env.Name == "MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS" &&
			(!strings.Contains(env.Value, spec.PHCfg+"/hostlog") || !strings.Contains(env.Value, spec.PHCfg+"/hostfiles") || strings.Contains(env.Value, spec.PHCfg+"/hosts:") || strings.HasSuffix(env.Value, spec.PHCfg+"/hosts")) {
			t.Fatalf("writable binds %q: the log and the copy folder, never the sign-in folder", env.Value)
		}
	}

	// The agent container gains nothing: its spec is the plain run's, the
	// harness's scrub leaves the host tools' output be.
	agentYAML, err := os.ReadFile(filepath.Join(planDir, "harness", "agent.yaml")) //nolint:gosec // test plan dir
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agentYAML), "tool_response_transform") || !strings.Contains(string(agentYAML), "host_") {
		t.Errorf("the harness's scrub does not name the host tools:\n%s", agentYAML)
	}

	prompt, _ := os.ReadFile(filepath.Join(planDir, "harness", "agent.yaml")) //nolint:gosec // test plan dir
	for _, want := range []string{"**Target hosts.**", "host_run runs a command line", "/scratch/hosts", "never instructions"} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("the System Prompt lacks %q", want)
		}
	}
	if line := plan.Enforcement["targetHosts"]; !strings.Contains(line, "server") || !strings.Contains(line, "a hosted run refuses") {
		t.Errorf("enforcement line %q", line)
	}
	// No address, user, port or sign-in is in the plan: the run's own.
	pj, _ := plan.MarshalCanonical()
	if strings.Contains(string(pj), "203.0.113") || strings.Contains(string(pj), "password") {
		t.Error("the plan holds a run's value")
	}
}

// TestTwoTargetHostsEachTakeTheirRelay and their own placeholder.
func TestTwoTargetHostsEachTakeTheirRelay(t *testing.T) {
	doc := strings.Replace(hostManifest, "  targetHosts: [server]", "  targetHosts: [web, db]", 1)
	doc = strings.Replace(doc, `  - id: server
    prompt: "The server to set up"
    type: host
    required: true`, `  - id: web
    prompt: "The web server"
    type: host
    required: true
  - id: db
    prompt: "The database server"
    type: host
    required: true`, 1)
	planDir := t.TempDir()
	if _, err := Compile(hostInputs(t, doc), planDir); err != nil {
		t.Fatal(err)
	}
	if got := strs(catalogServer(t, planDir, hostlink.ServerName)["allowHosts"]); !slices.Equal(got, []string{"@@RUN_HOST_web@@/tcp", "@@RUN_HOST_db@@/tcp"}) {
		t.Fatalf("allowHosts = %v", got)
	}
}

// TestAReleaseWithoutATargetHostHasNoHostLink: nothing of the host link
// reaches a plan that declares none.
func TestAReleaseWithoutATargetHostHasNoHostLink(t *testing.T) {
	planDir := t.TempDir()
	plan, err := Compile(mcpFixtureInputs(t, nil, policy.Default()), planDir)
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := os.ReadFile(filepath.Join(planDir, "gateway", "catalog.yaml")) //nolint:gosec // test plan dir
	agent, _ := os.ReadFile(filepath.Join(planDir, "harness", "agent.yaml")) //nolint:gosec // test plan dir
	if plan.HostLink != nil || strings.Contains(string(cat), "hostlink") || strings.Contains(string(cat), "hostfiles") ||
		strings.Contains(string(agent), "host_") || strings.Contains(string(agent), "Target hosts") || plan.Enforcement["targetHosts"] != "" {
		t.Fatal("a release with no target host compiled a host link")
	}
	if _, err := os.Stat(filepath.Join(planDir, "gateway", "hostspec.json")); err == nil {
		t.Fatal("hostspec.json was written")
	}
}

func TestABuiltinIsNeverGrantedATargetHost(t *testing.T) {
	in := hostInputs(t, hostManifest)
	in.Source = "builtin"
	var refusal *PolicyRefusal
	if _, err := Compile(in, t.TempDir()); !errors.As(err, &refusal) {
		t.Fatalf("a built-in with a target host compiled: %v", err)
	}
}
