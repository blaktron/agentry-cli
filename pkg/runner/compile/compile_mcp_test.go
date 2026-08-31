package compile

// Declared MCP servers in the compiled plan (RUNNER-PLAN-3): the catalog
// entry IS the enforcement — image, launch argv, read-only store mount,
// per-server egress, server-scoped secrets — so these tests pin the
// rendered gateway files, not just the plan struct.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

func mcpFixtureInputs(t *testing.T, servers []spec.MCPServerSpec, pol *policy.Policy) Inputs {
	t.Helper()
	ws := t.TempDir()
	doc := `schemaVersion: "1"
name: needs-servers
namespace: acme
kind: skill
version: 1.0.0
summary: "mcp server compile fixture"
entrypoints:
  skill: SKILL.md
publish:
  include: [agent.yaml, SKILL.md]
`
	if err := os.WriteFile(filepath.Join(ws, "agent.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "SKILL.md"), []byte("Do the job.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return Inputs{
		Manifest:     m,
		Digest:       "sha256:beef",
		Source:       "local",
		WorkspaceDir: ws,
		Policy:       pol,
		PolicySHA:    "policysha",
		RunnerImage:  "agentry-runner:test",
		MCPServers:   servers,
	}
}

func githubServerSpec() spec.MCPServerSpec {
	return spec.MCPServerSpec{
		Name: "mcp-npm-scope-server-x", Source: "npm",
		Package: "@scope/server-x", Version: "1.2.3", Integrity: "sha512-AAA",
		Image:       "node:22-alpine",
		Command:     []string{"/opt/agentry-mcp/npm-scope-server-x/node_modules/.bin/server-x"},
		StoreSubdir: "npm-scope-server-x",
		Tools:       []string{"list_issues"},
		Egress:      []string{"api.github.com"},
		Secrets:     []string{"GITHUB_PAT"},
	}
}

func TestCompileRendersDeclaredServer(t *testing.T) {
	planDir := t.TempDir()
	plan, err := Compile(mcpFixtureInputs(t, []spec.MCPServerSpec{githubServerSpec()}, policy.Default()), planDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.MCPServers) != 1 || plan.MCPServers[0].Name != "mcp-npm-scope-server-x" {
		t.Fatalf("plan.MCPServers = %+v", plan.MCPServers)
	}

	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(planDir, rel)) //nolint:gosec // test plan dir
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b)
	}
	catalog := read("gateway/catalog.yaml")
	for _, want := range []string{
		"mcp-npm-scope-server-x:",
		"image: node:22-alpine",
		"/node_modules/.bin/server-x",
		spec.PHServers + "/npm-scope-server-x:" + spec.MCPStorePrefix + "/npm-scope-server-x:ro",
		"api.github.com:443",
		"mcp-npm-scope-server-x.github_pat",
	} {
		if !strings.Contains(catalog, want) {
			t.Fatalf("catalog.yaml misses %q:\n%s", want, catalog)
		}
	}
	if !strings.Contains(read("gateway/registry.yaml"), "mcp-npm-scope-server-x:") {
		t.Fatal("registry.yaml does not enable the declared server")
	}
	tools := read("gateway/tools.yaml")
	if !strings.Contains(tools, "mcp-npm-scope-server-x:") || !strings.Contains(tools, "list_issues") {
		t.Fatalf("tools.yaml misses the per-server filter:\n%s", tools)
	}
	found := false
	for _, s := range plan.Gateway.Servers {
		if s == "mcp-npm-scope-server-x" {
			found = true
		}
	}
	if !found {
		t.Fatalf("gateway servers list misses the declared server: %v", plan.Gateway.Servers)
	}
	if !strings.Contains(plan.Enforcement["mcpServers"], "@scope/server-x@1.2.3") {
		t.Fatalf("enforcement misses the server: %q", plan.Enforcement["mcpServers"])
	}
}

func TestCompileGatesServerWildcardEgress(t *testing.T) {
	srv := githubServerSpec()
	srv.Egress = []string{"*"}
	pol := policy.Default() // AllowWildcardEgress false
	_, err := Compile(mcpFixtureInputs(t, []spec.MCPServerSpec{srv}, pol), t.TempDir())
	var refusal *PolicyRefusal
	if err == nil || !errorsAs(err, &refusal) {
		t.Fatalf("wildcard server egress not refused by policy: %v", err)
	}

	pol.AllowWildcardEgress = true
	if _, err := Compile(mcpFixtureInputs(t, []spec.MCPServerSpec{srv}, pol), t.TempDir()); err != nil {
		t.Fatalf("policy-permitted wildcard refused: %v", err)
	}
}

func TestCompileNoServersIsByteStable(t *testing.T) {
	// A plan without declared servers must not change shape — golden
	// compatibility for every pre-PLAN-3 agent.
	plan, err := Compile(mcpFixtureInputs(t, nil, policy.Default()), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if plan.MCPServers != nil {
		t.Fatalf("empty declaration produced MCPServers = %+v", plan.MCPServers)
	}
	b, err := plan.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "mcpServers") {
		t.Fatal("plan JSON carries an mcpServers key with none declared")
	}
}

// errorsAs avoids importing errors twice in this file's diff context.
func errorsAs(err error, target *(*PolicyRefusal)) bool {
	for e := err; e != nil; {
		if pr, ok := e.(*PolicyRefusal); ok {
			*target = pr
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}
