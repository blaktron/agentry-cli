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
	doc := `schemaVersion: "2"
name: needs-servers
namespace: acme
modes: [autonomous]
version: 1.0.0
summary: "mcp server compile fixture"
entrypoints:
  instruction: SKILL.md
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
		"api.github.com:443/tcp",
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
	// The gateway refuses a bind source outside its allowed roots; the
	// signature's server store must be one of them, or the server never
	// starts anywhere but under /tmp (agentry-cli#105).
	allowed := ""
	for _, ev := range plan.Gateway.Env {
		if ev.Name == "MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS" {
			allowed = ev.Value
		}
	}
	if !strings.Contains(allowed, spec.PHServers) {
		t.Fatalf("gateway bind roots lack the server store: %q", allowed)
	}
}

// TestCompileRefusesServerWildcardEgress (agentry-cli#105, DR-20): a
// declared server's "*" is refused whatever the policy says — the gateway
// gives such a server no relay and the run gives it no network of its
// own, so it would land on docker's default bridge, unconstrained. The
// agent's own "*" stays policy-gated (the llm-proxy still fronts it).
func TestCompileRefusesServerWildcardEgress(t *testing.T) {
	srv := githubServerSpec()
	srv.Egress = []string{"*"}
	for _, allow := range []bool{false, true} {
		pol := policy.Default()
		pol.AllowWildcardEgress = allow
		_, err := Compile(mcpFixtureInputs(t, []spec.MCPServerSpec{srv}, pol), t.TempDir())
		var refusal *PolicyRefusal
		if err == nil || !errorsAs(err, &refusal) || !strings.Contains(err.Error(), "declare the hosts") {
			t.Fatalf("allowWildcardEgress=%v: wildcard server egress not refused: %v", allow, err)
		}
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
	for _, ev := range plan.Gateway.Env {
		if ev.Name == "MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS" && strings.Contains(ev.Value, spec.PHServers) {
			t.Fatal("a plan without servers names the server store among the gateway's bind roots")
		}
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

// TestCompileRendersDeclaredServerEnv (agentry-cli#99): a declared server's
// plain env map lands in its catalog entry as the gateway's `env:
// [{name, value}]` list (docker/mcp-gateway v0.43.3, pkg/catalog/types.go
// `Server.Env []Env`, applied in pkg/gateway/clientpool.go as `-e NAME` with
// the value), sorted by name and separate from the secrets list; the
// enforcement line names the keys and never a value.
func TestCompileRendersDeclaredServerEnv(t *testing.T) {
	srv := githubServerSpec()
	srv.Env = map[string]string{"LOGS_DIR": "/tmp/logs", "GITHUB_PERMISSION": "readonly-v7"}
	planDir := t.TempDir()
	plan, err := Compile(mcpFixtureInputs(t, []spec.MCPServerSpec{srv}, policy.Default()), planDir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(planDir, "gateway", "catalog.yaml")) //nolint:gosec // test plan dir
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(b)
	want := "    env:\n      - name: GITHUB_PERMISSION\n        value: readonly-v7\n      - name: LOGS_DIR\n        value: /tmp/logs\n"
	if !strings.Contains(catalog, want) {
		t.Fatalf("catalog.yaml lacks the sorted env list:\n%s", catalog)
	}
	if !strings.Contains(catalog, "mcp-npm-scope-server-x.github_pat") {
		t.Fatalf("secrets left the catalog when env arrived:\n%s", catalog)
	}
	enf := plan.Enforcement["mcpServers"]
	if !strings.Contains(enf, "env keys GITHUB_PERMISSION/LOGS_DIR") || strings.Contains(enf, "readonly-v7") || strings.Contains(enf, "/tmp/logs") {
		t.Fatalf("enforcement must name the env keys and no value: %q", enf)
	}
	if plan.MCPServers[0].Env["LOGS_DIR"] != "/tmp/logs" {
		t.Fatalf("plan lost the env map: %+v", plan.MCPServers[0])
	}
}

// TestCompileRefusesInterpretedEnvValue: the gateway expands `$NAME` against
// the server's environment (secrets included) and evaluates `{{…}}` as a
// template, so such a value would not reach the server as written and
// could copy a secret into a plain variable. The compiler refuses it: a
// declared value is literal or it is not declared.
func TestCompileRefusesInterpretedEnvValue(t *testing.T) {
	for _, v := range []string{"$GITHUB_PAT", "${HOME}/x", "{{config.dir}}", "a$b", "@@RUN_SECRETS@@/x"} {
		srv := githubServerSpec()
		srv.Env = map[string]string{"LOGS_DIR": v}
		_, err := Compile(mcpFixtureInputs(t, []spec.MCPServerSpec{srv}, policy.Default()), t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "LOGS_DIR") || !strings.Contains(err.Error(), "literal") {
			t.Fatalf("value %q not refused as non-literal: %v", v, err)
		}
	}
	srv := githubServerSpec()
	srv.Env = map[string]string{"LOGS_DIR": ""}
	if _, err := Compile(mcpFixtureInputs(t, []spec.MCPServerSpec{srv}, policy.Default()), t.TempDir()); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty value not refused (the gateway would skip it and the receipt would still name the key): %v", err)
	}
	srv = githubServerSpec()
	srv.Env = map[string]string{"PRICE": "50$", "NOTE": "a {b} c"}
	if _, err := Compile(mcpFixtureInputs(t, []spec.MCPServerSpec{srv}, policy.Default()), t.TempDir()); err != nil {
		t.Fatalf("literal values refused: %v", err)
	}
}

// TestLiteralEnvRuleIsTheRunnersExpectation (agentry-cli#131): the runner
// refuses `$` and `{{ }}` values through the registry's own rule,
// manifest.LiteralEnvValue, rather than a copy. This pins that rule to
// what the gateway would interpret: os.Expand rewrites `$` followed by a
// name character, `{` or a special parameter, and a value holding both
// `{{` and `}}` is evaluated as a template. A core change that loosened
// the rule fails here before the runner inherits it.
func TestLiteralEnvRuleIsTheRunnersExpectation(t *testing.T) {
	interpreted := []string{"$GITHUB_PAT", "${HOME}/x", "a$b", "$1", "$_", "$*", "$#", "$@", "$!", "$?", "$$", "$-", "{{config.dir}}", "}} and {{"}
	for _, v := range interpreted {
		if manifest.LiteralEnvValue(v) {
			t.Errorf("manifest.LiteralEnvValue(%q) = true; the gateway would interpret it", v)
		}
	}
	literal := []string{"read-only", "50$", "$ x", "$.", "a {b} c", "{{ only", "only }}", "@@RUN_SECRETS@@"}
	for _, v := range literal {
		if !manifest.LiteralEnvValue(v) {
			t.Errorf("manifest.LiteralEnvValue(%q) = false; the gateway would pass it through as written", v)
		}
	}
}

// TestCompileDeclaredServerGolden pins the rendered catalog for a server
// with both secrets and env, the shape the plan's examples ship with.
func TestCompileDeclaredServerGolden(t *testing.T) {
	srv := githubServerSpec()
	srv.Env = map[string]string{"GITHUB_PERMISSION": "read-only"}
	planDir := t.TempDir()
	if _, err := Compile(mcpFixtureInputs(t, []spec.MCPServerSpec{srv}, policy.Default()), planDir); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "declared-server", planDir, []string{"plan.json", "gateway/catalog.yaml"})
}

// TestCompileDeclaredServerEgressIsATCPRelay (agentry-cli#105): a declared
// host becomes "host:443/tcp" in the catalog, so the gateway fronts it
// with its L4 relay (reached by the host's own name, TLS end to end) and
// not the L7 CONNECT proxy, which is handed to the server as a
// scheme-less proxy variable Node's fetch refuses and other clients ignore.
func TestCompileDeclaredServerEgressIsATCPRelay(t *testing.T) {
	planDir := t.TempDir()
	if _, err := Compile(mcpFixtureInputs(t, []spec.MCPServerSpec{githubServerSpec()}, policy.Default()), planDir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(planDir, "gateway", "catalog.yaml")) //nolint:gosec // test plan dir
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "    allowHosts:\n      - api.github.com:443/tcp\n") {
		t.Fatalf("declared host is not a TCP relay in the catalog:\n%s", b)
	}
}
