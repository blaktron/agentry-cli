package mcpinstall

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// fakeRegistries serves just enough npm + PyPI metadata for resolution.
func fakeRegistries(t *testing.T) policy.MCPRegistries {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /@scope%2Fserver-x/1.2.3", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"@scope/server-x","bin":{"server-x":"dist/cli.js"},"dist":{"tarball":"t","integrity":"sha512-AAA"}}`))
	})
	mux.HandleFunc("GET /no-bin/1.0.0", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"no-bin","dist":{"tarball":"t","integrity":"sha512-BBB"}}`))
	})
	mux.HandleFunc("GET /pypi/mcp-server-fetch/0.6.2/json", func(w http.ResponseWriter, _ *http.Request) {
		// A platform wheel listed first, then the pure-Python one, then the
		// sdist: the pin must prefer none-any and record all three.
		_, _ = w.Write([]byte(`{"urls":[
		  {"packagetype":"bdist_wheel","filename":"mcp_server_fetch-0.6.2-cp312-manylinux_x86_64.whl","digests":{"sha256":"beef"}},
		  {"packagetype":"bdist_wheel","filename":"mcp_server_fetch-0.6.2-py3-none-any.whl","digests":{"sha256":"cafe"}},
		  {"packagetype":"sdist","filename":"mcp_server_fetch-0.6.2.tar.gz","digests":{"sha256":"f00d"}}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return policy.MCPRegistries{NPM: srv.URL, PyPI: srv.URL}
}

func foregroundCalls(f *dockerx.Fake) int {
	n := 0
	for _, c := range f.Calls {
		if c.Op == "RunForeground" {
			n++
		}
	}
	return n
}

func decl(npm, pypi, version string) manifest.MCPServer {
	return manifest.MCPServer{NPM: npm, PyPI: pypi, Version: version}
}

func TestResolveAll(t *testing.T) {
	regs := fakeRegistries(t)
	res, err := ResolveAll(t.Context(), nil, regs, []manifest.MCPServer{
		decl("@scope/server-x", "", "1.2.3"),
		decl("", "mcp-server-fetch", "0.6.2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].BinName != "server-x" || res[0].Integrity != "sha512-AAA" {
		t.Fatalf("npm resolution = %+v", res[0])
	}
	if res[0].ID != "npm-scope-server-x" {
		t.Fatalf("npm id = %q", res[0].ID)
	}
	if got := res[0].Command(); got[0] != "/opt/agentry-mcp/npm-scope-server-x/node_modules/.bin/server-x" {
		t.Fatalf("npm command = %v", got)
	}
	if res[1].Integrity != "sha256:cafe" || res[1].BinName != "mcp-server-fetch" {
		t.Fatalf("pypi resolution = %+v", res[1])
	}
	if len(res[1].Artifacts) != 3 || res[1].Artifacts["mcp_server_fetch-0.6.2-cp312-manylinux_x86_64.whl"] != "beef" {
		t.Fatalf("pypi artifacts = %v, want all three advertised files", res[1].Artifacts)
	}
	if got := res[1].Command(); !strings.HasSuffix(got[0], "/venv/bin/mcp-server-fetch") {
		t.Fatalf("pypi command = %v", got)
	}
}

func TestResolveRefusesBinlessPackage(t *testing.T) {
	regs := fakeRegistries(t)
	_, err := ResolveAll(t.Context(), nil, regs, []manifest.MCPServer{decl("no-bin", "", "1.0.0")})
	if err == nil || !strings.Contains(err.Error(), "no bin") {
		t.Fatalf("binless package not refused: %v", err)
	}
}

func TestEnsureInstalledRunsBuilderOnceAndReuses(t *testing.T) {
	regs := fakeRegistries(t)
	res, err := ResolveAll(t.Context(), nil, regs, []manifest.MCPServer{decl("@scope/server-x", "", "1.2.3")})
	if err != nil {
		t.Fatal(err)
	}
	store := t.TempDir()
	fake := &dockerx.Fake{}
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}

	if err := EnsureInstalled(t.Context(), fake, res, store, regs, images, io.Discard); err != nil {
		t.Fatal(err)
	}
	if n := foregroundCalls(fake); n != 1 {
		t.Fatalf("builder ran %d times, want 1", n)
	}
	// The installer's name is fixed per server: it must remove itself, or
	// the next agent declaring the same server cannot install it (#106).
	if run, ok := fake.FindRun("agentry-mcpinstall-"); !ok || !run.AutoRemove {
		t.Fatalf("installer container not auto-removed: %+v", run)
	}
	if _, err := os.Stat(filepath.Join(store, "npm-scope-server-x.json")); err != nil {
		t.Fatalf("resolution not recorded: %v", err)
	}

	// Second install of the SAME resolution: pure reuse, no docker.
	res2, _ := ResolveAll(t.Context(), nil, regs, []manifest.MCPServer{decl("@scope/server-x", "", "1.2.3")})
	if err := EnsureInstalled(t.Context(), fake, res2, store, regs, images, io.Discard); err != nil {
		t.Fatal(err)
	}
	if n := foregroundCalls(fake); n != 1 {
		t.Fatalf("reuse still ran the builder (count %d)", n)
	}
}

func TestEnsureInstalledRefusesDrift(t *testing.T) {
	store := t.TempDir()
	fake := &dockerx.Fake{}
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}
	regs := policy.MCPRegistries{NPM: "http://unused.invalid", PyPI: "http://unused.invalid"}

	first := []Resolved{{Source: "npm", Package: "server-x", Version: "1.0.0", ID: "npm-server-x", BinName: "server-x", Integrity: "sha512-OLD"}}
	if err := EnsureInstalled(t.Context(), fake, first, store, regs, images, io.Discard); err != nil {
		t.Fatal(err)
	}
	drifted := []Resolved{{Source: "npm", Package: "server-x", Version: "1.0.0", ID: "npm-server-x", BinName: "server-x", Integrity: "sha512-NEW"}}
	err := EnsureInstalled(t.Context(), fake, drifted, store, regs, images, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "trust decision") {
		t.Fatalf("integrity drift not refused: %v", err)
	}
}

func TestSpecsCarryManifestFilters(t *testing.T) {
	servers := []manifest.MCPServer{{
		NPM: "@scope/server-x", Version: "1.2.3",
		Tools: []string{"list_issues"}, Secrets: []string{"GITHUB_PAT"}, Egress: []string{"api.github.com"},
	}}
	res := []Resolved{{Source: "npm", Package: "@scope/server-x", Version: "1.2.3", ID: "npm-scope-server-x", BinName: "server-x"}}
	specs := Specs(res, servers, policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"})
	s := specs[0]
	if s.Name != "mcp-npm-scope-server-x" || s.Image != "node:test" {
		t.Fatalf("spec = %+v", s)
	}
	if len(s.Tools) != 1 || len(s.Secrets) != 1 || s.Egress[0] != "api.github.com" {
		t.Fatalf("filters not carried: %+v", s)
	}
}

// PyPI installs are wheel-only and verify the pinned artifact against the
// registry's advertised digests before installing (agentry-cli#4); policy
// can opt back into source distributions.
func TestEnsureInstalledPyPIVerifiesWheels(t *testing.T) {
	regs := fakeRegistries(t)
	res, err := ResolveAll(t.Context(), nil, regs, []manifest.MCPServer{decl("", "mcp-server-fetch", "0.6.2")})
	if err != nil {
		t.Fatal(err)
	}
	store := t.TempDir()
	fake := &dockerx.Fake{}
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}
	if err := EnsureInstalled(t.Context(), fake, res, store, regs, images, io.Discard); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, c := range fake.Calls {
		if c.Op == "RunForeground" && c.Run.Image == "python:test" {
			script = strings.Join(c.Run.Argv, " ")
		}
	}
	for _, want := range []string{
		"pip download", "--no-deps", "--only-binary=:all:", "sha256sum -c",
		"--find-links", "mcp-server-fetch==0.6.2", "artifacts.sha256",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("builder script lacks %q:\n%s", want, script)
		}
	}
	if strings.Count(script, "--only-binary=:all:") != 2 {
		t.Errorf("download and install must both be wheel-only:\n%s", script)
	}
	sums, err := os.ReadFile(filepath.Join(store, res[0].ID, "artifacts.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sums), "cafe  mcp_server_fetch-0.6.2-py3-none-any.whl") ||
		!strings.Contains(string(sums), "beef  mcp_server_fetch-0.6.2-cp312-manylinux_x86_64.whl") {
		t.Fatalf("artifact digests file = %q", sums)
	}

	// Policy opt-in: source distributions allowed → no --only-binary.
	regs.PyPIAllowSourceDists = true
	if s := pipScript(&res[0], regs); strings.Contains(s, "--only-binary") {
		t.Fatalf("pypiAllowSourceDists still wheel-only: %s", s)
	}
}

// A PyPI server's launchable entry is the console script the installed
// distribution declares, not the project name (agentry-cli#13 / DR-11):
// the builder records entry points, the host adopts the right one, and a
// later reuse of the store carries the recorded name.
func TestEnsureInstalledPyPIAdoptsDeclaredConsoleScript(t *testing.T) {
	store := t.TempDir()
	fake := &dockerx.Fake{
		ExistingImages: map[string]bool{"python:test": true},
		Foreground: map[string]*dockerx.FakeForeground{
			"agentry-mcpinstall-pypi-some-tool": {Before: func(r dockerx.RunSpec) {
				if err := os.WriteFile(filepath.Join(r.Mounts[0].Source, consoleScriptsFile), []byte("some-tool-cli\nsome_tool\n"), 0o600); err != nil {
					t.Error(err)
				}
			}},
		},
	}
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}
	regs := policy.MCPRegistries{NPM: "http://unused.invalid", PyPI: "http://unused.invalid"}
	res := []Resolved{{Source: "pypi", Package: "Some.Tool", Version: "1.0.0", ID: "pypi-some-tool", BinName: "some-tool", Integrity: "sha256:aa"}}
	if err := EnsureInstalled(t.Context(), fake, res, store, regs, images, io.Discard); err != nil {
		t.Fatal(err)
	}
	if res[0].BinName != "some_tool" {
		t.Fatalf("BinName = %q, want the declared script matching the project name", res[0].BinName)
	}
	if got := res[0].Command()[0]; !strings.HasSuffix(got, "/venv/bin/some_tool") {
		t.Fatalf("command = %q", got)
	}
	// Reuse: the fresh resolution only knows the guess; the store knows better.
	again := []Resolved{{Source: "pypi", Package: "Some.Tool", Version: "1.0.0", ID: "pypi-some-tool", BinName: "some-tool", Integrity: "sha256:aa"}}
	if err := EnsureInstalled(t.Context(), fake, again, store, regs, images, io.Discard); err != nil {
		t.Fatal(err)
	}
	if again[0].BinName != "some_tool" {
		t.Fatalf("reused BinName = %q", again[0].BinName)
	}
	if n := len(fake.Ops()); n != 2 { // ImageExists + RunForeground once; reuse touched docker not at all
		t.Fatalf("docker ops on reuse: %v", fake.Ops())
	}
}

func TestChooseConsoleScript(t *testing.T) {
	cases := []struct {
		pkg   string
		names []string
		want  string
		ok    bool
	}{
		{"mcp-server-fetch", []string{"mcp-server-fetch"}, "mcp-server-fetch", true},
		{"mcp_server_fetch", []string{"other", "mcp-server-fetch"}, "mcp-server-fetch", true},
		{"Some.Tool", []string{"sometool-cli"}, "sometool-cli", true},
		{"pkg", []string{"zeta", "alpha", ""}, "alpha", true},
		{"pkg", []string{"", "  "}, "", false},
		{"pkg", nil, "", false},
	}
	for _, c := range cases {
		got, ok := chooseConsoleScript(c.pkg, c.names)
		if got != c.want || ok != c.ok {
			t.Errorf("chooseConsoleScript(%q, %v) = %q,%v want %q,%v", c.pkg, c.names, got, ok, c.want, c.ok)
		}
	}
	if normalizePyPIName("Mcp__Server.Fetch-") != "mcp-server-fetch" {
		t.Fatal(normalizePyPIName("Mcp__Server.Fetch-"))
	}
}

// The builder script records the dist's console scripts after installing.
func TestPipScriptRecordsConsoleScripts(t *testing.T) {
	r := Resolved{Source: "pypi", Package: "mcp-server-fetch", Version: "0.6.2", ID: "pypi-mcp-server-fetch", BinName: "mcp-server-fetch"}
	s := pipScript(&r, policy.MCPRegistries{PyPI: "http://pypi.invalid"})
	if !strings.Contains(s, "console_scripts") || !strings.Contains(s, "/"+consoleScriptsFile) {
		t.Fatalf("pip script does not record console scripts:\n%s", s)
	}
	if strings.Index(s, "pip install") > strings.Index(s, "console_scripts") {
		t.Fatal("console scripts must be read after pip install")
	}
}
