package compile

// Declared remote MCP servers (plan mcp-oauth-connections.md §5, D3;
// agentry-cli#663): each is an ordinary `type: server` catalog entry on the
// runner image's bridge, whose one allowed host is the address's host, so
// it runs behind one relay like a package server, and the gateway keeps
// --block-network and dials nothing itself (ARCHITECTURE.md §9, the
// runner's row). These pin the rendered gateway files and the receipt
// lines.

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"
	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

func remoteSecretSpec() spec.MCPServerSpec {
	return spec.MCPServerSpec{
		Name: "mcp-url-api-githubcopilot-com-mcp-readonly", Source: spec.SourceURL,
		Package: "https://api.githubcopilot.com/mcp/readonly",
		Tools:   []string{"search_repositories", "get_me"},
		Egress:  []string{"api.githubcopilot.com"},
		Secrets: []string{"GITHUB_TOKEN"},
		Auth:    "secret",
	}
}

func remoteNoneSpec() spec.MCPServerSpec {
	return spec.MCPServerSpec{
		Name: "mcp-url-mcp-deepwiki-com-mcp", Source: spec.SourceURL,
		Package: "https://mcp.deepwiki.com/mcp",
		Egress:  []string{"mcp.deepwiki.com"},
		Auth:    "none",
	}
}

// catalogServer is one server's entry in the rendered catalog.
func catalogServer(t *testing.T, planDir, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(planDir, "gateway", "catalog.yaml")) //nolint:gosec // test plan dir
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Registry map[string]map[string]any `yaml:"registry"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	e, ok := doc.Registry[name]
	if !ok {
		t.Fatalf("catalog has no %s:\n%s", name, b)
	}
	return e
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestCompileRendersRemoteServerOnTheBridge(t *testing.T) {
	planDir := t.TempDir()
	plan, err := Compile(mcpFixtureInputs(t, []spec.MCPServerSpec{remoteSecretSpec(), remoteNoneSpec()}, policy.Default()), planDir)
	if err != nil {
		t.Fatal(err)
	}

	gh := catalogServer(t, planDir, "mcp-url-api-githubcopilot-com-mcp-readonly")
	if gh["type"] != "server" || gh["image"] != "agentry-runner:test" {
		t.Fatalf("a remote server must be a type: server entry on the runner image: %v", gh)
	}
	if got, want := strs(gh["command"]), []string{"bridge", "--url", "https://api.githubcopilot.com/mcp/readonly", "--token-file", spec.ConnectionMount + "/" + spec.BearerFile}; !slices.Equal(got, want) {
		t.Fatalf("command = %v, want %v", got, want)
	}
	if got := strs(gh["allowHosts"]); !slices.Equal(got, []string{"api.githubcopilot.com:443/tcp"}) {
		t.Fatalf("allowHosts = %v: the address's host, on 443, behind its relay, and nothing else", got)
	}
	if gh["disableNetwork"] != nil {
		t.Fatal("a remote server has its one host, not disableNetwork")
	}
	if got := strs(gh["volumes"]); !slices.Equal(got, []string{spec.PHCfg + "/connections/mcp-url-api-githubcopilot-com-mcp-readonly:" + spec.ConnectionMount + ":ro"}) {
		t.Fatalf("volumes = %v: the server's own connection folder, read-only, a folder not a file", got)
	}
	if gh["secrets"] != nil || gh["env"] != nil {
		t.Fatalf("the bearer must not be one of the gateway's secrets or an env value: %v", gh)
	}

	dw := catalogServer(t, planDir, "mcp-url-mcp-deepwiki-com-mcp")
	if got := strs(dw["command"]); !slices.Equal(got, []string{"bridge", "--url", "https://mcp.deepwiki.com/mcp"}) {
		t.Fatalf("auth none: command = %v (no token file)", got)
	}
	if dw["volumes"] != nil {
		t.Fatalf("auth none: no connection folder is mounted: %v", dw["volumes"])
	}

	tools, err := os.ReadFile(filepath.Join(planDir, "gateway", "tools.yaml")) //nolint:gosec // test plan dir
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tools), "mcp-url-api-githubcopilot-com-mcp-readonly:") || !strings.Contains(string(tools), "search_repositories") {
		t.Fatalf("tools.yaml misses the remote server's filter:\n%s", tools)
	}

	// The gateway is unchanged: --block-network always, no network leg,
	// nothing mounted beyond its own two; and with no package server, the
	// signature's server store is no bind root.
	if !slices.Contains(plan.Gateway.Args, "--block-network") {
		t.Fatalf("gateway args lack --block-network: %v", plan.Gateway.Args)
	}
	if len(plan.Gateway.Mounts) != 2 {
		t.Fatalf("gateway mounts = %v, want its config and the daemon socket alone", plan.Gateway.Mounts)
	}
	for _, env := range plan.Gateway.Env {
		if env.Name == "MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS" {
			if strings.Contains(env.Value, spec.PHServers) || !strings.Contains(env.Value, spec.PHCfg) {
				t.Fatalf("bind roots %q: cfg (the connection folders) yes, the server store no", env.Value)
			}
		}
	}

	// The receipt names each remote server and its auth kind, never a value.
	line := plan.Enforcement["mcpServers.remote"]
	for _, want := range []string{
		"https://api.githubcopilot.com/mcp/readonly (auth secret, GITHUB_TOKEN sent as its bearer, tools filtered to get_me/search_repositories, reaching api.githubcopilot.com:443 alone)",
		"https://mcp.deepwiki.com/mcp (auth none, reaching mcp.deepwiki.com:443 alone)",
		"runner image's bridge", "the gateway dials none of them",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("mcpServers.remote misses %q:\n%s", want, line)
		}
	}
	if _, ok := plan.Enforcement["mcpServers"]; ok {
		t.Fatalf("no package server, so no package line: %q", plan.Enforcement["mcpServers"])
	}
	sec := plan.Enforcement["secrets"]
	if !strings.Contains(sec, "never into the gateway's secrets file") || !strings.Contains(sec, "GITHUB_TOKEN") || strings.Contains(sec, "holds every value") {
		t.Fatalf("secrets line does not say where the bearer goes: %q", sec)
	}
}

func TestCompileRemoteServerBesideExecUsesTheRunnerImage(t *testing.T) {
	// The tools server moves to the exec image when commands are declared;
	// the bridge stays on the runner image whatever the tools server runs in.
	pol := policy.Default()
	pol.Exec.Packages = map[string]string{"git": "git"}
	in := mcpFixtureInputs(t, []spec.MCPServerSpec{remoteNoneSpec(), githubServerSpec()}, pol)
	m, err := manifest.Parse([]byte(`schemaVersion: "2"
name: needs-servers
namespace: acme
modes: [autonomous]
version: 1.0.0
summary: "mcp server compile fixture"
entrypoints:
  instruction: SKILL.md
capabilities:
  exec:
    commands: [git]
`))
	if err != nil {
		t.Fatal(err)
	}
	in.Manifest = m
	planDir := t.TempDir()
	plan, err := Compile(in, planDir)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ExecImage == nil || catalogServer(t, planDir, spec.ToolServerName)["image"] != plan.ExecImage.Tag {
		t.Fatal("the fixture must put the tools server on the exec image for this test to mean anything")
	}
	if img := catalogServer(t, planDir, "mcp-url-mcp-deepwiki-com-mcp")["image"]; img != "agentry-runner:test" {
		t.Fatalf("bridge image = %v, want the runner image", img)
	}
	if !strings.Contains(plan.Enforcement["mcpServers"], "@scope/server-x@1.2.3") || strings.Contains(plan.Enforcement["mcpServers"], "deepwiki") {
		t.Fatalf("the package line names the package server alone: %q", plan.Enforcement["mcpServers"])
	}
	sec := plan.Enforcement["secrets"]
	if !strings.Contains(sec, "holds every value too") || strings.Contains(sec, "bearer excepted") {
		t.Fatalf("with no bearer, the gateway holds every value: %q", sec)
	}
}

func TestCompileRefusesMalformedRemoteSpecs(t *testing.T) {
	for name, mut := range map[string]func(*spec.MCPServerSpec){
		"http":                func(s *spec.MCPServerSpec) { s.Package = "http://api.githubcopilot.com/mcp/" },
		"dollar":              func(s *spec.MCPServerSpec) { s.Package = "https://api.githubcopilot.com/$HOME" },
		"secret, no name":     func(s *spec.MCPServerSpec) { s.Secrets = nil },
		"none with a secret":  func(s *spec.MCPServerSpec) { s.Auth = "none" },
		"oauth with a secret": func(s *spec.MCPServerSpec) { s.Auth = "oauth" },
		"port 8443":           func(s *spec.MCPServerSpec) { s.Package = "https://api.githubcopilot.com:8443/mcp/" },
	} {
		t.Run(name, func(t *testing.T) {
			srv := remoteSecretSpec()
			mut(&srv)
			if _, err := Compile(mcpFixtureInputs(t, []spec.MCPServerSpec{srv}, policy.Default()), t.TempDir()); err == nil {
				t.Fatal("compiled")
			}
		})
	}
}

func remoteOAuthSpec() spec.MCPServerSpec {
	return spec.MCPServerSpec{
		Name: "mcp-url-mcp-linear-app-mcp", Source: spec.SourceURL,
		Package: "https://mcp.linear.app/mcp",
		Egress:  []string{"mcp.linear.app"},
		Auth:    "oauth",
	}
}

// A remote server signed in to by OAuth (agentry-cli#664) runs on the
// bridge exactly as an auth: secret one does: its access token in its own
// connection folder, read-only, never the gateway's; the receipt says the
// run is given an access token alone.
func TestCompileRendersAnOAuthServerOnTheBridge(t *testing.T) {
	planDir := t.TempDir()
	plan, err := Compile(mcpFixtureInputs(t, []spec.MCPServerSpec{remoteOAuthSpec()}, policy.Default()), planDir)
	if err != nil {
		t.Fatal(err)
	}
	e := catalogServer(t, planDir, "mcp-url-mcp-linear-app-mcp")
	if got, want := strs(e["command"]), []string{"bridge", "--url", "https://mcp.linear.app/mcp", "--token-file", spec.ConnectionMount + "/" + spec.BearerFile}; !slices.Equal(got, want) {
		t.Fatalf("command = %v, want %v", got, want)
	}
	if got := strs(e["volumes"]); !slices.Equal(got, []string{spec.PHCfg + "/connections/mcp-url-mcp-linear-app-mcp:" + spec.ConnectionMount + ":ro"}) {
		t.Fatalf("volumes = %v: the server's own connection folder, read-only", got)
	}
	if got := strs(e["allowHosts"]); !slices.Equal(got, []string{"mcp.linear.app:443/tcp"}) {
		t.Fatalf("allowHosts = %v", got)
	}
	if e["secrets"] != nil || e["env"] != nil {
		t.Fatalf("the access token must not be one of the gateway's secrets or an env value: %v", e)
	}
	if !slices.Contains(plan.Gateway.Args, "--block-network") {
		t.Fatalf("gateway args lack --block-network: %v", plan.Gateway.Args)
	}
	line := plan.Enforcement["mcpServers.remote"]
	if !strings.Contains(line, "https://mcp.linear.app/mcp (auth oauth, the person's connection for this release: an access token alone") ||
		!strings.Contains(line, "the refresh token never leaves the runner's store") {
		t.Fatalf("mcpServers.remote does not say what the run is given:\n%s", line)
	}
}

// A package server's own OAuth sign-in (an oauth block) is agentry-cli#665's:
// refused by name until then.
func TestCompileRefusesAPackageServerOAuthBlock(t *testing.T) {
	in := mcpFixtureInputs(t, nil, policy.Default())
	m, err := manifest.Parse([]byte(`schemaVersion: "2"
name: needs-servers
namespace: acme
modes: [autonomous]
version: 1.0.0
summary: "connection fixture"
entrypoints:
  instruction: SKILL.md
capabilities:
  mcpServers:
    - pypi: workspace-mcp
      version: 2.0.1
      oauth:
        issuer: https://accounts.google.com
        env: {GOOGLE_TOKEN: "{access_token}"}
`))
	if err != nil {
		t.Fatal(err)
	}
	in.Manifest = m
	_, err = Compile(in, t.TempDir())
	var refusal *PolicyRefusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "pypi:workspace-mcp@2.0.1 takes an OAuth sign-in in its own package") {
		t.Fatalf("err = %v, want a policy refusal naming the package server's OAuth sign-in", err)
	}
}

// A bearer whose name the release also declares as its own secret is that
// secret's value too, in the gateway's file for the tools container: the
// receipt says so rather than that the gateway never holds it.
func TestTheReceiptSaysWhenABearerIsAlsoTheReleasesOwnSecret(t *testing.T) {
	in := mcpFixtureInputs(t, []spec.MCPServerSpec{remoteSecretSpec()}, policy.Default())
	m, err := manifest.Parse([]byte(`schemaVersion: "2"
name: needs-servers
namespace: acme
modes: [autonomous]
version: 1.0.0
summary: "overlap fixture"
entrypoints:
  instruction: SKILL.md
capabilities:
  secrets: [GITHUB_TOKEN]
`))
	if err != nil {
		t.Fatal(err)
	}
	in.Manifest = m
	plan, err := Compile(in, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sec := plan.Enforcement["secrets"]
	if !strings.Contains(sec, "GITHUB_TOKEN is also the release's own secret, so its value is in the gateway's secrets file for the tools container as well, and the gateway holds it") || strings.Contains(sec, "bearer excepted") {
		t.Fatalf("secrets line: %q", sec)
	}
}
