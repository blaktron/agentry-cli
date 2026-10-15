package drafter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
	"github.com/blaktron/agentry/pkg/manifest"
)

// The post-step's list check: a pin the drafting guide's Does not run here
// covers, or one with a listed direct dependency, goes to the repair turn;
// install scripts are notes; the notes say what admitted each server.

// passingDraft is postDraft without the question the rules refuse.
var passingDraft = strings.Replace(postDraft, `"intake": [{"id": "format", "prompt": "Which format?", "type": "choice"}],`, "", 1)

// withServers is a draft answer declaring the servers, a JSON array's
// members.
func withServers(answer, servers string) string {
	return strings.Replace(answer, `"exec": "none"}`, `"exec": "none", "mcpServers": [`+servers+`]}`, 1)
}

const (
	markitdown = `{"pypi": "markitdown-mcp", "version": "0.0.1a7", "tools": ["convert_to_markdown"]}`
	fetchSrv   = `{"pypi": "mcp-server-fetch", "version": "2026.8.18", "tools": ["fetch"]}`
	unlisted   = `{"npm": "unlisted-docs-mcp", "version": "1.0.0", "tools": ["read_doc"]}`
)

// fakeMeta resolves every pin, with the direct dependencies and install
// scripts given by package, and counts its calls.
type fakeMeta struct {
	deps    map[string][]string
	scripts map[string][]string
	calls   int
}

func (f *fakeMeta) resolve(_ context.Context, srv manifest.MCPServer) (mcpinstall.Resolved, error) {
	f.calls++
	pkg := srv.NPM + srv.PyPI
	return mcpinstall.Resolved{Source: sourceOf(srv), Package: pkg, Version: srv.Version,
		Dependencies: f.deps[pkg], InstallScripts: f.scripts[pkg]}, nil
}

func checked(t *testing.T, f *fakeMeta, servers string) *Draft {
	t.Helper()
	d := &Draft{Manifest: json.RawMessage(`{"capabilities": {"mcpServers": [` + servers + `]}}`)}
	PostStep{serverChecks: map[string]serverCheck{}, Servers: f.resolve}.checkServers(context.Background(), d)
	return d
}

// A draft pinning markitdown-mcp@0.0.1a7 gets the repair turn with the
// list's line, and a second answer pinning a server on Runs here instead
// is kept, its notes saying what admitted it.
func TestADraftPinningADoesNotRunHereServerGetsTheRepairTurn(t *testing.T) {
	out := t.TempDir()
	var tasks []string
	p := PostStep{
		OutDir: out, Description: "Rebuild my résumé from a Word file.", Task: "Rebuild my résumé from a Word file.",
		Scan: webscan.Result{Verdict: webscan.Pass}, Ref: Namespace + "/" + Name + "@" + Version, Checker: &postChecker{},
		Servers: (&fakeMeta{}).resolve,
		Repair: func(_ context.Context, rt RepairTurn) (*receipt.Envelope, error) {
			tasks = append(tasks, rt.Task)
			if err := os.MkdirAll(filepath.Join(rt.OutDir, "reports"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rt.OutDir, "reports", instructionsReport), []byte("# Résumé\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return &receipt.Envelope{Status: "ok", Result: json.RawMessage(withServers(postRepaired, fetchSrv))}, nil
		},
	}
	d, _, err := p.Run(context.Background(), firstTurn(t, out, withServers(passingDraft, markitdown)), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "- capabilities.mcpServers[0]: pypi:markitdown-mcp@0.0.1a7: does not run here: musl-native: it needs a native binary built for glibc"
	if len(tasks) != 1 || !strings.Contains(tasks[0], want) {
		t.Fatalf("the repair task(s) do not carry %q:\n%.1500s", want, strings.Join(tasks, "\n---\n"))
	}
	if !d.IsRepaired() || len(d.serverProblems) != 0 {
		t.Fatalf("the draft: repaired %v, problems %v", d.IsRepaired(), d.serverProblems)
	}
	if !strings.Contains(d.Notes, "## What admitted each MCP server") || !strings.Contains(d.Notes, "pypi:mcp-server-fetch@2026.8.18: on Runs here, seen to start and list its tools on Python") {
		t.Fatalf("the notes do not say what admitted the server: %q", d.Notes)
	}
}

// Another version of a listed server is covered when the reason is the
// server's design, and not when it is bound to the version seen.
func TestTheListCoversAPackagesOtherVersionsByReason(t *testing.T) {
	l, err := LoadServerList(FS())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := l.NotRunHere("pypi", "Markitdown_MCP", "0.0.2"); !ok {
		t.Fatal("a later markitdown-mcp is not covered: musl-native is the server's design")
	}
	e, ok := l.NotRunHere("pypi", "mcp-server-sqlite", "2025.4.25")
	if !ok || e.Reason != "broken-upstream" {
		t.Fatalf("the version seen broken is not covered: %+v %v", e, ok)
	}
	if _, ok := l.NotRunHere("pypi", "mcp-server-sqlite", "2026.1.1"); ok {
		t.Fatal("another release of a broken-upstream server is covered: the break is the version's")
	}
	if _, ok := l.NotRunHere("npm", "@modelcontextprotocol/server-gitlab", "2026.1.1"); ok {
		t.Fatal("another release of a server whose tool schema the gateway refused is covered: a release can correct a schema")
	}
	if _, ok := l.NotRunHere("npm", "@modelcontextprotocol/server-everything", "2026.8.31"); ok {
		t.Fatal("a Runs here server is covered by Does not run here")
	}
	if _, listed, seen := l.RunsHereEntry("pypi", "mcp_server_fetch", "2026.8.18"); !listed || !seen {
		t.Fatal("the Runs here entry is not found under PyPI's name rule")
	}
	if _, listed, seen := l.RunsHereEntry("pypi", "mcp-server-fetch", "2025.1.1"); !listed || seen {
		t.Fatal("a Runs here entry speaks for a version it was not seen at")
	}
}

// A server whose direct dependencies include a listed package gets the
// rules line; an unlisted server with none passes, judged with the guide;
// a Runs here pin passes.
func TestTheListCheckReadsDirectDependencies(t *testing.T) {
	f := &fakeMeta{deps: map[string][]string{
		"word-reader-mcp":   {"mcp", "onnxruntime"},
		"git-notes-mcp":     {"simple-git", "zod"},
		"mcp-server-fetch":  {"mcp", "httpx"},
		"unlisted-docs-mcp": {"zod"},
	}}
	d := checked(t, f, `{"pypi": "word-reader-mcp", "version": "1.0.0"}, {"npm": "git-notes-mcp", "version": "2.0.0"}, `+fetchSrv+`, `+unlisted)
	if len(d.serverProblems) != 2 ||
		!strings.Contains(d.serverProblems[0], "capabilities.mcpServers[0]: pypi:word-reader-mcp@1.0.0: does not run here: musl-native: it depends directly on onnxruntime, which needs a native binary") ||
		!strings.Contains(d.serverProblems[1], "capabilities.mcpServers[1]: npm:git-notes-mcp@2.0.0: does not run here: spawns-binary: it depends directly on simple-git") {
		t.Fatalf("problems %q", d.serverProblems)
	}
	if len(d.serversAdmitted) != 2 ||
		!strings.HasPrefix(d.serversAdmitted[0], "pypi:mcp-server-fetch@2026.8.18: on Runs here") ||
		d.serversAdmitted[1] != "npm:unlisted-docs-mcp@1.0.0: on neither list, so judged with the drafting guide, not seen to run." {
		t.Fatalf("admitted %q", d.serversAdmitted)
	}
	// A listed dependency holds whatever list the server is on, and a
	// Runs here version gives no install-script sign.
	f = &fakeMeta{deps: map[string][]string{"mcp-server-fetch": {"magika"}}, scripts: map[string][]string{"mcp-server-fetch": {"postinstall"}}}
	if d := checked(t, f, fetchSrv); len(d.serverProblems) != 1 || len(d.serverSigns) != 0 || len(d.serversAdmitted) != 0 {
		t.Fatalf("Runs here with a listed dependency: problems %q, signs %q, admitted %q", d.serverProblems, d.serverSigns, d.serversAdmitted)
	}
	// A dependency in the other ecosystem is not this one's.
	f = &fakeMeta{deps: map[string][]string{"unlisted-docs-mcp": {"onnxruntime"}}}
	if d := checked(t, f, unlisted); len(d.serverProblems) != 0 {
		t.Fatalf("an npm server was held to a PyPI dependency: %q", d.serverProblems)
	}
}

// An npm install script is a sign to check, never a repair.
func TestAnInstallScriptIsANoteNotARepair(t *testing.T) {
	f := &fakeMeta{scripts: map[string][]string{"unlisted-docs-mcp": {"postinstall"}}}
	d := checked(t, f, unlisted)
	if len(d.serverProblems) != 0 || len(d.serverSigns) != 1 ||
		!strings.Contains(d.serverSigns[0], "npm:unlisted-docs-mcp@1.0.0 has an npm postinstall script, which a run's install skips") {
		t.Fatalf("problems %q, signs %q", d.serverProblems, d.serverSigns)
	}
	if rules, refused := rulesReport(d); refused {
		t.Fatalf("an install script refused the draft: %q", rules)
	}
	d.noteServerProblems()
	if !strings.Contains(d.Notes, "## MCP servers to check before a run\n\n- npm:unlisted-docs-mcp@1.0.0 has an npm postinstall script") {
		t.Fatalf("notes %q", d.Notes)
	}
}

// keepFirst keeps a first answer the list rule passed over a second that
// keeps its server and adds one Does not run here covers.
func TestKeepFirstHoldsTheListRule(t *testing.T) {
	f := &fakeMeta{}
	first, second := checked(t, f, unlisted), checked(t, f, unlisted+", "+markitdown)
	first.Validation, second.Validation = json.RawMessage(`{"passed":true}`), json.RawMessage(`{"passed":true}`)
	first.Instructions, second.Instructions = "# X\n", "# X\n"
	broke, dropped := keepFirst(first, second, RepairReport{Flagged: []string{"off_task: it reads no Word file"}})
	if !broke || len(dropped) != 0 {
		t.Fatalf("keepFirst = %v, %v; want the first kept for the broken rule", broke, dropped)
	}
}

// What leaves the machine does not grow: the list is embedded and the
// dependencies come with the resolution the check already made, one per
// pin, and a repair keeping its pins makes none.
func TestTheListCheckAddsNoRequest(t *testing.T) {
	f := &fakeMeta{deps: map[string][]string{"unlisted-docs-mcp": {"zod"}}}
	p := PostStep{serverChecks: map[string]serverCheck{}, Servers: f.resolve}
	servers := markitdown + ", " + fetchSrv + ", " + unlisted
	p.checkServers(context.Background(), &Draft{Manifest: json.RawMessage(`{"capabilities": {"mcpServers": [` + servers + `]}}`)})
	if f.calls != 3 {
		t.Fatalf("%d resolutions for three pins, want one each", f.calls)
	}
	p.checkServers(context.Background(), &Draft{Manifest: json.RawMessage(`{"capabilities": {"mcpServers": [` + servers + `]}}`)})
	if f.calls != 3 {
		t.Fatalf("a repair keeping its pins resolved them again (%d)", f.calls)
	}
}

// A registry that does not answer leaves the list check standing: a pin
// Does not run here covers needs no metadata.
func TestTheListCheckNeedsNoRegistry(t *testing.T) {
	d := &Draft{Manifest: json.RawMessage(`{"capabilities": {"mcpServers": [` + markitdown + `, ` + unlisted + `]}}`)}
	PostStep{serverChecks: map[string]serverCheck{}, Servers: fakeServers(nil)}.checkServers(context.Background(), d)
	if len(d.serverProblems) != 1 || !strings.Contains(d.serverProblems[0], "markitdown-mcp@0.0.1a7: does not run here") {
		t.Fatalf("problems %q", d.serverProblems)
	}
	if strings.Join(d.serversUnchecked, ",") != "pypi:markitdown-mcp@0.0.1a7,npm:unlisted-docs-mcp@1.0.0" {
		t.Fatalf("unchecked %q", d.serversUnchecked)
	}
}
