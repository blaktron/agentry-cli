package drafter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
	"github.com/blaktron/agentry/pkg/manifest"
)

// No unit test reaches a package registry: a PostStep without Servers
// resolves every declared server as one that declares no runtime range.
func TestMain(m *testing.M) {
	registryResolver = func(policy.MCPRegistries) ServerResolver {
		return func(_ context.Context, srv manifest.MCPServer) (mcpinstall.Resolved, error) {
			return mcpinstall.Resolved{Source: sourceOf(srv), Version: srv.Version}, nil
		}
	}
	os.Exit(m.Run())
}

func sourceOf(srv manifest.MCPServer) string {
	if srv.NPM != "" {
		return "npm"
	}
	return "pypi"
}

// withServer is a draft answer declaring the OSV server at version.
func withServer(answer, version string) string {
	return strings.Replace(answer, `"exec": "none"}`,
		`"exec": "none", "mcpServers": [{"npm": "@cyanheads/osv-advisory-mcp-server", "version": "`+version+`", "tools": ["osv_query_batch"], "egress": ["api.osv.dev"]}]}`, 1)
}

// fakeServers resolves the OSV server's versions to the runtime ranges
// given, and anything else to an error.
func fakeServers(ranges map[string]string) ServerResolver {
	return func(_ context.Context, srv manifest.MCPServer) (mcpinstall.Resolved, error) {
		rng, ok := ranges[srv.Version]
		if !ok {
			return mcpinstall.Resolved{}, errors.New("registry did not answer")
		}
		return mcpinstall.Resolved{Source: sourceOf(srv), Version: srv.Version, DeclaredRuntime: rng}, nil
	}
}

// TestADraftPinningAServerItsRunsCannotRunGetsTheRepairTurn
// (agentry-cli#609): a draft pinning a release whose own runtime range
// excludes the run's goes to the repair turn with the rule's line, and a
// second answer pinning one it admits is kept, with nothing to fix in its
// notes.
func TestADraftPinningAServerItsRunsCannotRunGetsTheRepairTurn(t *testing.T) {
	out := t.TempDir()
	passing := strings.Replace(postDraft, `"intake": [{"id": "format", "prompt": "Which format?", "type": "choice"}],`, "", 1)
	var tasks []string
	p := PostStep{
		OutDir: out, Description: "Audit my dependencies against OSV.", Task: "Audit my dependencies against OSV.",
		Scan: webscan.Result{Verdict: webscan.Pass}, Ref: Namespace + "/" + Name + "@" + Version, Checker: &postChecker{},
		Servers: fakeServers(map[string]string{"0.1.15": ">=26.0.0", "0.1.13": ">=24.0.0"}),
		Repair: func(_ context.Context, rt RepairTurn) (*receipt.Envelope, error) {
			tasks = append(tasks, rt.Task)
			if err := os.MkdirAll(filepath.Join(rt.OutDir, "reports"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rt.OutDir, "reports", instructionsReport), []byte("# Audit\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return &receipt.Envelope{Status: "ok", Result: json.RawMessage(withServer(postRepaired, "0.1.13"))}, nil
		},
	}
	d, _, err := p.Run(context.Background(), firstTurn(t, out, withServer(passing, "0.1.15")), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "- capabilities.mcpServers[0]: npm:@cyanheads/osv-advisory-mcp-server@0.1.15: it needs Node >=26.0.0 (its engines.node), and runs use Node 24."
	if len(tasks) != 1 || !strings.Contains(tasks[0], want) {
		t.Fatalf("the repair task(s) do not carry %q:\n%.1200s", want, strings.Join(tasks, "\n---\n"))
	}
	if !d.IsRepaired() || len(d.serverProblems) != 0 || strings.Contains(d.Notes, "MCP servers") {
		t.Fatalf("the draft: repaired %v, problems %v, notes %q", d.IsRepaired(), d.serverProblems, d.Notes)
	}
}

// A server whose metadata cannot be read is this computer's reason, not
// the draft's: no repair turn for it, and the notes name it.
func TestAServerThatCannotBeCheckedIsNotedNotRefused(t *testing.T) {
	out := t.TempDir()
	passing := strings.Replace(postDraft, `"intake": [{"id": "format", "prompt": "Which format?", "type": "choice"}],`, "", 1)
	repaired := false
	p := PostStep{
		OutDir: out, Description: "Audit my dependencies against OSV.", Task: "Audit my dependencies against OSV.",
		Scan: webscan.Result{Verdict: webscan.Pass}, Ref: Namespace + "/" + Name + "@" + Version, Checker: &postChecker{},
		Servers: fakeServers(nil),
		Repair: func(context.Context, RepairTurn) (*receipt.Envelope, error) {
			repaired = true
			return nil, errors.New("no repair expected")
		},
	}
	d, _, err := p.Run(context.Background(), firstTurn(t, out, withServer(passing, "0.1.15")), nil)
	if err != nil {
		t.Fatal(err)
	}
	if repaired || len(d.serverProblems) != 0 {
		t.Fatalf("an unchecked server was refused: repaired %v, problems %v", repaired, d.serverProblems)
	}
	if !strings.Contains(d.Notes, "## MCP servers not checked") || !strings.Contains(d.Notes, "npm:@cyanheads/osv-advisory-mcp-server@0.1.15") {
		t.Fatalf("the notes do not name the unchecked server: %q", d.Notes)
	}
}

// brokeARule keeps a first answer the server rule passed over a second it
// refuses, and a second that mends the first.
func TestKeepFirstHoldsTheServerRule(t *testing.T) {
	clean := &Draft{Validation: json.RawMessage(`{"passed":true}`), Instructions: "# X\n"}
	broken := &Draft{Validation: json.RawMessage(`{"passed":true}`), Instructions: "# X\n", serverProblems: []string{"capabilities.mcpServers[0]: …"}}
	if !brokeARule(clean, broken) || brokeARule(broken, clean) {
		t.Fatal("brokeARule does not hold the server rule")
	}
}

// The Drafter is told the runtimes' versions the rule checks against, so
// the instruction and the code cannot drift apart.
func TestTheInstructionNamesTheRuntimeVersions(t *testing.T) {
	b, err := os.ReadFile("AGENT.md")
	if err != nil {
		t.Fatal(err)
	}
	want := "npm servers run\n     on Node " + policy.DefaultNodeVersion + " and PyPI servers on Python " + policy.DefaultPythonVersion + "."
	if !strings.Contains(string(b), want) {
		t.Fatalf("AGENT.md does not say %q", want)
	}
}

// A pin its registry does not publish is the draft's fault and a rules
// line; a registry that does not answer is not, and after its first
// failure the other pins are noted without asking it again.
func TestCheckServersTellsAPinFaultFromANetworkFault(t *testing.T) {
	two := func(a, b string) json.RawMessage {
		return json.RawMessage(`{"capabilities": {"mcpServers": [{"npm": "a-mcp", "version": "` + a + `"}, {"npm": "b-mcp", "version": "` + b + `"}]}}`)
	}
	calls := 0
	p := PostStep{serverChecks: map[string]serverCheck{}, Servers: func(_ context.Context, srv manifest.MCPServer) (mcpinstall.Resolved, error) {
		calls++
		switch srv.Version {
		case "9.9.9":
			return mcpinstall.Resolved{}, fmt.Errorf("resolve: %w", &mcpinstall.PinError{Err: errors.New("not found in the configured registry")})
		case "down":
			return mcpinstall.Resolved{}, errors.New("dial tcp: i/o timeout")
		}
		return mcpinstall.Resolved{Source: "npm", DeclaredRuntime: ">=20"}, nil
	}}
	d := &Draft{Manifest: two("9.9.9", "1.0.0")}
	p.checkServers(context.Background(), d)
	if len(d.serverProblems) != 1 || !strings.Contains(d.serverProblems[0], "npm:a-mcp@9.9.9: not found in the configured registry, so a run could not install it") || len(d.serversUnchecked) != 0 {
		t.Fatalf("pin fault: problems %q, unchecked %q", d.serverProblems, d.serversUnchecked)
	}
	calls = 0
	d = &Draft{Manifest: two("down", "2.0.0")}
	p.checkServers(context.Background(), d)
	if calls != 1 || len(d.serverProblems) != 0 || strings.Join(d.serversUnchecked, ",") != "npm:a-mcp@down,npm:b-mcp@2.0.0" {
		t.Fatalf("network fault: %d calls, problems %q, unchecked %q", calls, d.serverProblems, d.serversUnchecked)
	}
	// A pin already resolved in this post-step is not fetched again.
	calls = 0
	p.checkServers(context.Background(), &Draft{Manifest: two("9.9.9", "1.0.0")})
	if calls != 0 {
		t.Fatalf("a repair keeping its pins fetched them again (%d calls)", calls)
	}
}

// Without its own resolver the post-step reads the registries the person's
// runs install from.
func TestCheckServersReadsTheRunsRegistries(t *testing.T) {
	saved := registryResolver
	t.Cleanup(func() { registryResolver = saved })
	var got policy.MCPRegistries
	registryResolver = func(regs policy.MCPRegistries) ServerResolver {
		got = regs
		return func(context.Context, manifest.MCPServer) (mcpinstall.Resolved, error) {
			return mcpinstall.Resolved{}, nil
		}
	}
	m := json.RawMessage(`{"capabilities": {"mcpServers": [{"npm": "a-mcp", "version": "1.0.0"}]}}`)
	mirror := policy.MCPRegistries{NPM: "https://npm.mirror.example", PyPI: "https://pypi.mirror.example"}
	PostStep{Registries: mirror}.checkServers(context.Background(), &Draft{Manifest: m})
	if got != mirror {
		t.Fatalf("resolved against %+v, want the run's %+v", got, mirror)
	}
	PostStep{}.checkServers(context.Background(), &Draft{Manifest: m})
	if got != policy.Default().MCPRegistries {
		t.Fatalf("with none given, resolved against %+v, want the defaults", got)
	}
}

// A draft that declares a remote server (agentry-cli#663) is not looked up
// in any registry: it has no package and no runtime range, and its
// address is not a pin a registry could answer.
func TestCheckServersLeavesARemoteServerAlone(t *testing.T) {
	calls := 0
	p := PostStep{serverChecks: map[string]serverCheck{}, Servers: func(_ context.Context, srv manifest.MCPServer) (mcpinstall.Resolved, error) {
		calls++
		return mcpinstall.Resolved{Source: "npm"}, nil
	}}
	d := &Draft{Manifest: json.RawMessage(`{"capabilities": {"mcpServers": [{"url": "https://mcp.deepwiki.com/mcp", "auth": "none"}, {"npm": "a-mcp", "version": "1.0.0"}]}}`)}
	p.checkServers(context.Background(), d)
	if calls != 1 || len(d.serverProblems) != 0 || len(d.serversUnchecked) != 0 {
		t.Fatalf("%d lookups, problems %q, unchecked %q: only the package server is looked up", calls, d.serverProblems, d.serversUnchecked)
	}
	// The registry resolver itself answers a remote server with an error,
	// never a panic.
	if _, err := mcpinstall.ResolveAll(context.Background(), nil, policy.Default().MCPRegistries, []manifest.MCPServer{{URL: "https://mcp.deepwiki.com/mcp"}}); err != nil {
		t.Fatalf("ResolveAll on a remote server: %v", err)
	}
}
