package compile

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry/pkg/manifest"
)

const webTextAgent = `schemaVersion: "2"
name: drafter
namespace: agentry
version: 1.0.0
summary: "Drafts a BriefAgent."
license: Apache-2.0
modes: [autonomous]
entrypoints:
  instruction: AGENT.md
capabilities:
  network:
    egress: none
  exec: none
publish:
  include: [agent.yaml, AGENT.md]
`

func compileWeb(t *testing.T, yaml, source string, web bool) (*spec.Plan, string, error) {
	t.Helper()
	m, err := manifest.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "AGENT.md"), []byte("Draft.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.AllowWildcardEgress = true
	pol.Exec.Packages = map[string]string{"git": "git"}
	dir := filepath.Join(t.TempDir(), "plan")
	plan, err := Compile(Inputs{Manifest: m, Digest: "sha256:x", Source: source, Web: web, WorkspaceDir: ws,
		Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test"}, dir)
	return plan, dir, err
}

// TestCompileRefusesTheWebToolsToARelease: the web tools are the built-in
// Drafter's alone (plan builder-rework.md §7.3): a release or a local
// package asking for them is refused by policy, and so is a built-in whose
// contract would run beside them in a container with no allowlist.
func TestCompileRefusesTheWebToolsToARelease(t *testing.T) {
	for _, source := range []string{"registry", "local", ""} {
		_, _, err := compileWeb(t, webTextAgent, source, true)
		var refusal *PolicyRefusal
		if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "built-in Drafter's alone") {
			t.Fatalf("source %q: %v", source, err)
		}
	}
	for name, widen := range map[string]string{
		"egress":  "  network:\n    egress: [api.example.com]\n",
		"any":     "  network:\n    egress: \"*\"\n",
		"exec":    "  exec:\n    commands: [git]\n",
		"write":   "  filesystem:\n    write: workspace\n",
		"secrets": "  secrets: [API_KEY]\n",
		"servers": "  mcpServers:\n    - npm: \"@acme/mcp\"\n      version: 1.0.0\n",
	} {
		y := strings.Replace(webTextAgent, "capabilities:\n  network:\n    egress: none\n  exec: none\n", "capabilities:\n"+widen, 1)
		_, _, err := compileWeb(t, y, "builtin", true)
		var refusal *PolicyRefusal
		if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "declares no egress") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// TestTheWebGrantCompilesItsContainer: the grant leaves the tools container
// its network (web_read holds its own rule), names the three keys as the
// tools server's secrets, grants web_read, and says all of it in the plan's
// enforcement; without the grant the same package has none of it.
func TestTheWebGrantCompilesItsContainer(t *testing.T) {
	plan, dir, err := compileWeb(t, webTextAgent, "builtin", true)
	if err != nil {
		t.Fatal(err)
	}
	catalog, _ := os.ReadFile(filepath.Join(dir, "gateway", "catalog.yaml"))
	if strings.Contains(string(catalog), "disableNetwork") || strings.Contains(string(catalog), "allowHosts") {
		t.Fatalf("the web tools' container:\n%s", catalog)
	}
	for _, env := range toolsrv.WebSecretEnv() {
		if !strings.Contains(string(catalog), "env: "+env) || !strings.Contains(string(catalog), SecretKey(env)) {
			t.Fatalf("catalog lacks %s:\n%s", env, catalog)
		}
	}
	if !plan.Capabilities.WebTools || !slices.Contains(plan.Tools, toolsrv.ToolWebRead) || slices.Contains(plan.Tools, toolsrv.ToolFetch) {
		t.Fatalf("caps %+v tools %v", plan.Capabilities, plan.Tools)
	}
	if !strings.Contains(plan.Enforcement["web"], "public addresses only") || !strings.Contains(plan.Enforcement["network"], "no declared egress") {
		t.Fatalf("%v", plan.Enforcement)
	}
	if len(plan.Secrets) != 0 {
		t.Fatalf("the web keys are not declared secrets: %v", plan.Secrets)
	}

	plain, dir, err := compileWeb(t, webTextAgent, "builtin", false)
	if err != nil {
		t.Fatal(err)
	}
	catalog, _ = os.ReadFile(filepath.Join(dir, "gateway", "catalog.yaml"))
	if !strings.Contains(string(catalog), "disableNetwork: true") || strings.Contains(string(catalog), "AGENTRY_WEB") ||
		plain.Capabilities.WebTools || slices.Contains(plain.Tools, toolsrv.ToolWebRead) || plain.Enforcement["web"] != "" {
		t.Fatalf("without the grant:\n%s\n%+v", catalog, plain.Capabilities)
	}
}
