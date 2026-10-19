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
summary: "Drafts a QuickAgent."
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
	return compileWebWith(t, yaml, source, web, nil)
}

// compileWebRegistry is compileWeb with registry_search asked for too, as
// localrun asks for the Drafter.
func compileWebRegistry(t *testing.T, web, registry bool) (*spec.Plan, error) {
	t.Helper()
	return compileWebGrant(t, web, registry, false)
}

// compileWebGrant is compileWebRegistry with the searched-only web_read too.
func compileWebGrant(t *testing.T, web, registry, searchedOnly bool) (*spec.Plan, error) {
	t.Helper()
	m, err := manifest.Parse([]byte(webTextAgent))
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "AGENT.md"), []byte("Draft.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return Compile(Inputs{Manifest: m, Digest: "sha256:x", Source: "builtin", Web: web, RegistrySearch: registry, WebSearchedOnly: searchedOnly, WorkspaceDir: ws,
		Policy: policy.Default(), PolicySHA: "sha", RunnerImage: "agentry-runner:test"}, filepath.Join(t.TempDir(), "plan"))
}

// TestRegistrySearchIsCompiledApart: registry_search is compiled on its
// own, the Drafter's alone, so the Knowledge Creator's web tools are
// web_search and web_read; it is never granted without them, and the plan's
// enforcement and System Prompt name it only when it is granted.
func TestRegistrySearchIsCompiledApart(t *testing.T) {
	_, err := compileWebRegistry(t, false, true)
	var refusal *PolicyRefusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "never granted without them") {
		t.Fatalf("registry_search without the web tools: %v", err)
	}
	creator, err := compileWebRegistry(t, true, false)
	if err != nil {
		t.Fatal(err)
	}
	drafter, err := compileWebRegistry(t, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !creator.Capabilities.WebTools || creator.Capabilities.RegistrySearch || !drafter.Capabilities.RegistrySearch {
		t.Fatalf("creator %+v drafter %+v", creator.Capabilities, drafter.Capabilities)
	}
	if strings.Contains(creator.Enforcement["web"], toolsrv.ToolRegistrySearch) || !strings.Contains(drafter.Enforcement["web"], toolsrv.ToolRegistrySearch) {
		t.Fatalf("enforcement: creator %q drafter %q", creator.Enforcement["web"], drafter.Enforcement["web"])
	}
	if strings.Contains(creator.Enforcement["web"], "Drafter") {
		t.Fatalf("the Creator's enforcement names the Drafter: %q", creator.Enforcement["web"])
	}

	// The searched-only web_read (the Creator's): never without the web
	// tools, compiled into the tools spec, and said in the enforcement and
	// the System Prompt.
	if _, err := compileWebGrant(t, false, false, true); !errors.As(err, &refusal) {
		t.Fatalf("the searched-only web_read without the web tools: %v", err)
	}
	only, err := compileWebGrant(t, true, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !only.Capabilities.WebSearchedOnly || drafter.Capabilities.WebSearchedOnly || creator.Capabilities.WebSearchedOnly {
		t.Fatalf("searched-only: %+v", only.Capabilities)
	}
	if !strings.Contains(only.Enforcement["web"], "opening only an address a web_search of this run returned") ||
		strings.Contains(drafter.Enforcement["web"], "opening only") {
		t.Fatalf("enforcement %q", only.Enforcement["web"])
	}
	if !strings.Contains(systemPrompt(promptFacts{caps: only.Capabilities}), "web_read opens only an address one of your web_search results gave") ||
		strings.Contains(systemPrompt(promptFacts{caps: drafter.Capabilities}), "web_read opens only") {
		t.Fatal("the System Prompt does not say the searched-only rule, or says it to the Drafter")
	}
}

// compileWebWith is compileWeb with the policy edited first.
func compileWebWith(t *testing.T, yaml, source string, web bool, edit func(*policy.Policy)) (*spec.Plan, string, error) {
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
	if edit != nil {
		edit(pol)
	}
	dir := filepath.Join(t.TempDir(), "plan")
	plan, err := Compile(Inputs{Manifest: m, Digest: "sha256:x", Source: source, Web: web, WorkspaceDir: ws,
		Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test"}, dir)
	return plan, dir, err
}

// TestCompileRefusesTheWebToolsToARelease: the web tools are the built-in
// Drafter's alone: a release or a local package asking for them is refused
// by policy, and so is a built-in whose contract would run beside them in a
// container with no allowlist.
func TestCompileRefusesTheWebToolsToARelease(t *testing.T) {
	for _, source := range []string{"registry", "local", ""} {
		_, _, err := compileWeb(t, webTextAgent, source, true)
		var refusal *PolicyRefusal
		if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "the built-ins' alone") {
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

// TestTheWebToolsUnderARuntimeNeedTheHostsResolvers: a sandboxed runtime's
// network stack does not reach docker's embedded DNS on the run's web
// network, so the web tools under a runtime with no docker.resolvers are
// refused by policy rather than left to resolve nothing; with them, the
// plan carries the name servers (and so its hash changes with them); a run
// without the web tools carries none, and one on the daemon's default needs
// none.
func TestTheWebToolsUnderARuntimeNeedTheHostsResolvers(t *testing.T) {
	_, _, err := compileWebWith(t, webTextAgent, "builtin", true, func(p *policy.Policy) { p.Docker.Runtime = "runsc" })
	var refusal *PolicyRefusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "docker.resolvers") || !strings.Contains(refusal.Reason, "runsc") {
		t.Fatalf("the web tools under runsc with no resolvers: %v", err)
	}
	plan, _, err := compileWebWith(t, webTextAgent, "builtin", true, func(p *policy.Policy) {
		p.Docker.Runtime, p.Docker.Resolvers = "runsc", []string{"192.168.122.1", "192.0.2.53"}
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Runtime() != "runsc" || !slices.Equal(plan.WebResolvers(), []string{"192.168.122.1", "192.0.2.53"}) {
		t.Fatalf("the plan's docker: %+v", plan.Docker)
	}
	// On the daemon's default the web tools keep docker's DNS.
	plan, _, err = compileWebWith(t, webTextAgent, "builtin", true, nil)
	if err != nil || plan.WebResolvers() != nil || plan.Docker != nil {
		t.Fatalf("the web tools on the default runtime: %+v %v", plan, err)
	}
	// A run without the web tools reads no resolvers, under any runtime.
	plan, _, err = compileWebWith(t, webTextAgent, "builtin", false, func(p *policy.Policy) {
		p.Docker.Runtime, p.Docker.Resolvers = "runsc", []string{"192.168.122.1"}
	})
	if err != nil || plan.WebResolvers() != nil || plan.Runtime() != "runsc" {
		t.Fatalf("a run without the web tools: %+v %v", plan.Docker, err)
	}
}

// TestTheGivenOnlyReadIsCompiledApart: the given-only web_read (the Voice
// Writer's) is never granted without the web tools nor beside the
// searched-only one; it is compiled into the tools spec and said in the
// enforcement and the System Prompt, which name no web_search for it.
func TestTheGivenOnlyReadIsCompiledApart(t *testing.T) {
	compileGiven := func(web, searched bool) (*spec.Plan, error) {
		t.Helper()
		m, err := manifest.Parse([]byte(webTextAgent))
		if err != nil {
			t.Fatal(err)
		}
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "AGENT.md"), []byte("Write.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return Compile(Inputs{Manifest: m, Digest: "sha256:x", Source: "builtin", Web: web, WebGivenOnly: true, WebSearchedOnly: searched, WorkspaceDir: ws,
			Policy: policy.Default(), PolicySHA: "sha", RunnerImage: "agentry-runner:test"}, filepath.Join(t.TempDir(), "plan"))
	}
	var refusal *PolicyRefusal
	if _, err := compileGiven(false, false); !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "never granted without them") {
		t.Fatalf("the given-only web_read without the web tools: %v", err)
	}
	if _, err := compileGiven(true, true); !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "never both") {
		t.Fatalf("both web_read rules: %v", err)
	}
	given, err := compileGiven(true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !given.Capabilities.WebGivenOnly || given.Capabilities.WebSearchedOnly {
		t.Fatalf("given-only: %+v", given.Capabilities)
	}
	if e := given.Enforcement["web"]; !strings.Contains(e, toolsrv.ToolWebRead+" alone, opening only an address the person gave the run") || strings.Contains(e, toolsrv.ToolWebSearch) {
		t.Fatalf("enforcement %q", e)
	}
	prompt := systemPrompt(promptFacts{caps: given.Capabilities})
	if !strings.Contains(prompt, "web_read opens only an address your task names") || !strings.Contains(prompt, "You have no web_search") || strings.Contains(prompt, "Search only for") {
		t.Fatalf("the System Prompt:\n%s", prompt)
	}
}
