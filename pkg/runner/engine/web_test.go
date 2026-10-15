package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry/pkg/manifest"
)

const webManifest = `schemaVersion: "2"
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
  filesystem:
    read: none
    write: none
  exec: none
publish:
  include: [agent.yaml, AGENT.md]
`

// The keys a draft carries, and a registry credential in the CLI's own
// environment, none of which may reach the agent container.
const (
	testJobToken  = "agyj_job-credential-0123456789"
	testJevKey    = "jev-person-key-0123456789abcdef"
	testSearchKey = "ydc-person-search-key-0123456789"
	testRegToken  = "agt_registry-token-0123456789abc"
)

// webFixture compiles a plan with the web tools, as localrun does for the
// Drafter, and a run of it with a local draft's keys.
func webFixture(t *testing.T) (Params, *dockerx.Fake) {
	t.Helper()
	return webFixtureFor(t, true)
}

// webFixtureFor is webFixture with registry_search granted or not: the
// Knowledge Creator's web tools are the Drafter's without it.
func webFixtureFor(t *testing.T, registrySearch bool) (Params, *dockerx.Fake) {
	t.Helper()
	m, err := manifest.Parse([]byte(webManifest))
	if err != nil {
		t.Fatal(err)
	}
	installDir := t.TempDir()
	ws := filepath.Join(installDir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "AGENT.md"), []byte("Draft it.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.Images.Runner = "agentry-runner:test"
	planDir := filepath.Join(installDir, "plan")
	plan, err := compile.Compile(compile.Inputs{
		Manifest: m, Digest: "sha256:d4af7", Source: "builtin", Web: true, RegistrySearch: registrySearch, WebSearchedOnly: !registrySearch,
		WorkspaceDir: ws, Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test",
	}, planDir)
	if err != nil {
		t.Fatal(err)
	}
	ad, err := adapter.Get(adapter.DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
	t.Setenv("AGENTRY_TOKEN", testRegToken)
	t.Setenv("SECUREAGENTRY_TOKEN", testRegToken)
	return Params{
		Plan: plan, PlanDir: planDir, WorkspaceSrc: ws, RunRoot: installDir, OutDir: t.TempDir(),
		Task: strings.NewReader("draft this"), Adapter: ad, Policy: pol,
		Budgets:     spec.Budgets{TimeoutSeconds: 30, MaxTurns: 10},
		Attestation: receipt.Attestation{State: "builtin"}, Stderr: os.Stderr, RunID: "d4af0000",
		Web: &WebRun{Scan: toolsrv.WebScanJev, JevKey: testJevKey, Search: "youcom", SearchKey: testSearchKey,
			Registry: "https://registry.example"},
	}, &dockerx.Fake{}
}

// TestADraftsKeysReachTheToolsContainerAlone: the
// agent container's environment holds no registry credential, no Jev key
// and no search key, and neither does any container's command line; the
// keys are in the run's gateway secrets file, scoped to the tools server,
// and gone at teardown.
func TestADraftsKeysReachTheToolsContainerAlone(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		p, f := webFixture(t)
		if hosted {
			p.Web = &WebRun{Scan: toolsrv.WebScanRegistry, Search: toolsrv.WebScanRegistry, Registry: "https://registry.example", JobToken: testJobToken}
		}
		happyFake(f)
		secretsPath := filepath.Join(p.RunRoot, "runs", p.RunID, "cfg", "gateway", "secrets.env")
		var secretsFile string
		f.OnForeground = func(r dockerx.RunSpec) {
			if strings.HasSuffix(r.Name, "-agent") {
				b, _ := os.ReadFile(secretsPath)
				secretsFile = string(b)
			}
		}
		if _, err := Run(context.Background(), f, p); err != nil {
			t.Fatalf("hosted=%v: %v", hosted, err)
		}
		keys := []string{testJobToken, testJevKey, testSearchKey, testRegToken}
		sawAgent := false
		for _, c := range f.Calls {
			r := c.Run
			if r.Name == "" {
				continue
			}
			if strings.HasSuffix(r.Name, "-agent") {
				sawAgent = true
				for name := range r.Env {
					if strings.HasPrefix(name, "AGENTRY_") || strings.Contains(name, "JEV") || strings.Contains(name, "TYPESAFE") || strings.Contains(name, "YDC") {
						t.Errorf("hosted=%v: the agent container's environment names %s", hosted, name)
					}
				}
			}
			for name, v := range r.Env {
				for _, k := range keys {
					if strings.Contains(v, k) {
						t.Errorf("hosted=%v: %s carries a key in %s", hosted, r.Name, name)
					}
				}
			}
			for _, a := range r.Argv {
				for _, k := range keys {
					if strings.Contains(a, k) {
						t.Errorf("hosted=%v: %s carries a key on its command line", hosted, r.Name)
					}
				}
			}
		}
		if !sawAgent {
			t.Fatal("no agent container ran")
		}
		want := testJevKey
		if hosted {
			want = testJobToken
		}
		if !strings.Contains(secretsFile, compile.SecretKey(toolsrv.EnvWebJevKey)+"=") ||
			!strings.Contains(secretsFile, compile.SecretKey(toolsrv.EnvWebJobToken)+"=") ||
			!strings.Contains(secretsFile, want) || strings.Contains(secretsFile, testRegToken) {
			t.Fatalf("hosted=%v: the gateway secrets file during the run: %q", hosted, secretsFile)
		}
		if _, err := os.Stat(secretsPath); !os.IsNotExist(err) {
			t.Fatalf("hosted=%v: secrets.env survived the run", hosted)
		}
	}
}

// TestTheRunsWebHalfIsStamped: the rendered tools spec names the run's
// scanner, search, registry and ledger and no key; the allowlist line grants
// the tools the spec does.
func TestTheRunsWebHalfIsStamped(t *testing.T) {
	p, f := webFixture(t)
	happyFake(f)
	p.KeepRunDir = true
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatal(err)
	}
	gw := filepath.Join(p.RunRoot, "runs", p.RunID, "cfg", "gateway")
	b, err := os.ReadFile(filepath.Join(gw, "toolspec.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ts toolsrv.Spec
	if err := json.Unmarshal(b, &ts); err != nil {
		t.Fatal(err)
	}
	if !ts.Web.Enabled || ts.Web.Scan != toolsrv.WebScanJev || ts.Web.Search != "youcom" || ts.Web.Registry != "https://registry.example" ||
		ts.Web.Ledger != "/out/receipts/"+WebLedgerFile {
		t.Fatalf("web %+v", ts.Web)
	}
	for _, k := range []string{testJevKey, testSearchKey} {
		if strings.Contains(string(b), k) {
			t.Fatal("a key in the tools spec")
		}
	}
	tools, err := os.ReadFile(filepath.Join(gw, "tools.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{toolsrv.ToolWebSearch, toolsrv.ToolWebRead, toolsrv.ToolRegistrySearch} {
		if !strings.Contains(string(tools), name) {
			t.Fatalf("tools.yaml lacks %s:\n%s", name, tools)
		}
	}
	if !slices.Equal(ts.GrantedTools(), []string{toolsrv.ToolListInputs, toolsrv.ToolReadInput, toolsrv.ToolSearchInputs, toolsrv.ToolWebSearch, toolsrv.ToolWebRead, toolsrv.ToolRegistrySearch, toolsrv.ToolWriteReport}) {
		t.Fatalf("granted %v", ts.GrantedTools())
	}
}

// TestACreatorsWebHasNoRegistrySearch: the Knowledge Creator's plan carries
// web_search and web_read and never registry_search, even when its run names
// a registry, as a hosted build's does for its scans.
func TestACreatorsWebHasNoRegistrySearch(t *testing.T) {
	p, f := webFixtureFor(t, false)
	happyFake(f)
	p.KeepRunDir = true
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatal(err)
	}
	gw := filepath.Join(p.RunRoot, "runs", p.RunID, "cfg", "gateway")
	b, err := os.ReadFile(filepath.Join(gw, "toolspec.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ts toolsrv.Spec
	if err := json.Unmarshal(b, &ts); err != nil {
		t.Fatal(err)
	}
	if ts.Web.Registry == "" || ts.Web.RegistrySearch || !ts.Web.SearchedOnly {
		t.Fatalf("web %+v", ts.Web)
	}
	if !slices.Equal(ts.GrantedTools(), []string{toolsrv.ToolListInputs, toolsrv.ToolReadInput, toolsrv.ToolSearchInputs, toolsrv.ToolWebSearch, toolsrv.ToolWebRead, toolsrv.ToolWriteReport}) {
		t.Fatalf("granted %v", ts.GrantedTools())
	}
	tools, err := os.ReadFile(filepath.Join(gw, "tools.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(tools), toolsrv.ToolRegistrySearch) {
		t.Fatalf("tools.yaml grants %s:\n%s", toolsrv.ToolRegistrySearch, tools)
	}
}

// TestAWebPlanRunsOnlyWithItsScanner: no scanner, or a scanner without its
// key, refuses before any container; a plan without the web tools refuses a
// web configuration.
func TestAWebPlanRunsOnlyWithItsScanner(t *testing.T) {
	for name, w := range map[string]*WebRun{
		"none":            nil,
		"no Jev key":      {Scan: toolsrv.WebScanJev},
		"no job token":    {Scan: toolsrv.WebScanRegistry, Registry: "https://r.example"},
		"unknown scanner": {Scan: "static", JevKey: testJevKey},
		"search, no key":  {Scan: toolsrv.WebScanJev, JevKey: testJevKey, Search: "youcom"},
		"registry search": {Scan: toolsrv.WebScanJev, JevKey: testJevKey, Search: toolsrv.WebScanRegistry},
	} {
		p, f := webFixture(t)
		p.Web = w
		_, err := Run(context.Background(), f, p)
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != ExitPolicy {
			t.Fatalf("%s: %v", name, err)
		}
		if len(f.Calls) != 0 {
			t.Fatalf("%s: docker was called: %+v", name, f.Calls)
		}
	}
	p, f := fixture(t)
	p.Web = &WebRun{Scan: toolsrv.WebScanJev, JevKey: testJevKey}
	if _, err := Run(context.Background(), f, p); err == nil {
		t.Fatal("a plan without the web tools took a web configuration")
	}
}

// TestTheWebLedgerIsCountedIntoTheReceipt: what the tools server wrote is
// counted; a line that does not decode is never a pass.
func TestTheWebLedgerIsCountedIntoTheReceipt(t *testing.T) {
	p, f := webFixture(t)
	happyFake(f)
	ledger := strings.Join([]string{
		`{"tool":"web_read","source":"page","url":"https://a.example/","bytes":900,"verdict":"pass","score":0,"model":"jev-1.13.0","questionSet":"sha256:q","requests":1,"inputTokens":300}`,
		`{"tool":"web_read","source":"page","url":"https://b.example/","bytes":900,"verdict":"withheld","score":3,"flagged":["injection"],"requests":4,"inputTokens":1200}`,
		`{"tool":"web_read","url":"http://10.0.0.1/","refused":"10.0.0.1 is not a public address"}`,
		`{"tool":"web_search","search":true,"results":2}`,
		`{"tool":"web_search","source":"search_result","url":"https://c.example/x","verdict":"pass","requests":1,"inputTokens":50}`,
		`{"tool":"web_search","source":"search_result","url":"https://d.example/","verdict":"not_scanned","failure":"timeout"}`,
		`{"tool":"registry_search","source":"registry","url":"/api/v1/search?q=x","verdict":"pass","requests":1,"inputTokens":80}`,
		`{"verdict":"pass"`,
	}, "\n") + "\n"
	f.OnForeground = func(r dockerx.RunSpec) {
		if strings.HasSuffix(r.Name, "-agent") {
			_ = os.WriteFile(filepath.Join(p.OutDir, "receipts", WebLedgerFile), []byte(ledger), 0o644)
		}
	}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	w := env.Web
	if w == nil || w.Read != 5 || w.Pages != 2 || w.Searches != 1 || w.SearchResults != 2 || w.RegistryCalls != 1 || w.Passed != 3 || w.Withheld != 1 ||
		w.NotScanned != 1 || w.Refused != 2 || w.ScanRequests != 7 || w.ScanInputTokens != 1630 || w.Model != "jev-1.13.0" || w.Scan != "jev" {
		t.Fatalf("%+v", w)
	}
	line := env.CapabilitiesEnforced["web.aiReview"]
	if !strings.Contains(line, "5 read (2 pages, 2 search results of 1 searches, 1 registry answers), 3 passed, 1 withheld as flagged, 1 withheld as not scanned") {
		t.Fatalf("%q", line)
	}
	if !strings.Contains(env.CapabilitiesEnforced["web"], "public addresses only") || !strings.Contains(env.CapabilitiesEnforced["network"], "no declared egress") {
		t.Fatalf("%v", env.CapabilitiesEnforced)
	}
}

// TestTheWebToolsResolvConf: a plan with the web tools and name servers
// gets a resolv.conf of the run's own beside its tools spec, one
// nameserver line each, readable by the tools server's user; a plan
// without either gets none; the receipt says which servers resolve the web
// tools' names, and where the filter cannot bind the file, that it is not
// applied.
func TestTheWebToolsResolvConf(t *testing.T) {
	dir := t.TempDir()
	plan := &spec.Plan{Capabilities: spec.CapSummary{WebTools: true}, Docker: &spec.DockerSpec{Runtime: "runsc", Resolvers: []string{"192.168.122.1", "192.0.2.53"}}}
	if err := writeResolvConf(dir, plan); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, resolvConfFile))
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o644) { // Windows has no POSIX modes
		t.Fatalf("the resolv.conf: %v %v", info, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, resolvConfFile))
	var servers []string
	for _, l := range strings.Split(string(b), "\n") {
		if s, ok := strings.CutPrefix(l, "nameserver "); ok {
			servers = append(servers, s)
		}
	}
	if !slices.Equal(servers, []string{"192.168.122.1", "192.0.2.53"}) {
		t.Fatalf("the resolv.conf reads:\n%s", b)
	}
	for name, p := range map[string]*spec.Plan{
		"no web tools": {Docker: &spec.DockerSpec{Resolvers: []string{"192.168.122.1"}}},
		"no servers":   {Capabilities: spec.CapSummary{WebTools: true}, Docker: &spec.DockerSpec{Runtime: "runsc"}},
	} {
		d := t.TempDir()
		if err := writeResolvConf(d, p); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(d, resolvConfFile)); !os.IsNotExist(err) {
			t.Fatalf("%s: a resolv.conf was written", name)
		}
	}
	line := resolversLine(plan, plan.WebResolvers())
	if !strings.Contains(line, "192.168.122.1, 192.0.2.53") || !strings.Contains(line, "docker.resolvers") {
		t.Fatalf("the receipt line: %s", line)
	}
	if filterSupported && !strings.Contains(line, "bound read-only in place of docker's") {
		t.Fatalf("the receipt line on Linux: %s", line)
	}
	// The runtime line says how the web tools' server resolves, too.
	if rl := runtimeLine(Params{Plan: plan}); filterSupported && !strings.Contains(rl, "the web tools' server resolves host names through 192.168.122.1, 192.0.2.53") {
		t.Fatalf("the runtime line: %s", rl)
	}
}
