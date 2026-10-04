package drafter

import (
	"context"
	"encoding/json"
	"errors"
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
	defaultServerResolver = func(_ context.Context, srv manifest.MCPServer) (mcpinstall.Resolved, error) {
		return mcpinstall.Resolved{Source: sourceOf(srv), Version: srv.Version}, nil
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
		return mcpinstall.Resolved{Source: sourceOf(srv), Version: srv.Version, Runtime: rng}, nil
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

// keepFirst keeps a first answer the server rule passed over a second it
// refuses, and a second that mends the first.
func TestKeepFirstHoldsTheServerRule(t *testing.T) {
	clean := &Draft{Validation: json.RawMessage(`{"passed":true}`), Instructions: "# X\n"}
	broken := &Draft{Validation: json.RawMessage(`{"passed":true}`), Instructions: "# X\n", serverProblems: []string{"capabilities.mcpServers[0]: …"}}
	if !keepFirst(clean, broken) || keepFirst(broken, clean) {
		t.Fatal("keepFirst does not hold the server rule")
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
