package mcpinstall

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha1" //nolint:gosec // the legacy shasum form the resolver records
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// TestNpmBuilderRunsUnderNode runs npm_builder.js with the host's node and
// npm against a local registry serving one generated package, the way the
// runtime image runs it: it installs the pinned tarball
// when it matches the recorded integrity (SRI sha512, or the legacy sha1
// form), refuses a mismatch (exit 4) and a missing integrity (exit 3)
// before anything is installed, and passes a value with shell syntax to npm
// as a literal. Skipped where the host has no node and npm.
func TestNpmBuilderRunsUnderNode(t *testing.T) {
	node := hostNode(t)
	tgz := demoTarball(t)
	s512 := sha512.Sum512(tgz)
	sri := "sha512-" + base64.StdEncoding.EncodeToString(s512[:])
	s1 := sha1.Sum(tgz) //nolint:gosec // see import
	reg := fakeNPMRegistry(t, map[string][]byte{"demo-mcp": tgz})

	var buildIn func(t *testing.T, prefix, pkg, integrity string) (string, int, string)
	build := func(t *testing.T, pkg, integrity string) (string, int, string) {
		t.Helper()
		return buildIn(t, t.TempDir(), pkg, integrity)
	}
	buildIn = func(t *testing.T, prefix, pkg, integrity string) (string, int, string) {
		t.Helper()
		code, out := runNpmBuilder(t, node, reg, prefix, pkg, integrity)
		return prefix, code, out
	}
	installed := func(prefix string) bool {
		_, err := os.Stat(filepath.Join(prefix, "node_modules", ".bin", "demo-mcp"))
		return err == nil
	}

	t.Run("installs the pinned tarball when it matches the recorded SRI", func(t *testing.T) {
		prefix := t.TempDir()
		code, out, archive := runNpmBuilderArchive(t, node, reg, prefix, "demo-mcp", sri)
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if !installed(prefix) {
			t.Fatal("the bin was not installed")
		}
		// The command npm linked is a script that runs its file by its
		// absolute path, so the tree holds no link; and stdout is the
		// archive of the tree alone, which unpacks into a store.
		unpacked := t.TempDir()
		if err := unpackStore(bytes.NewReader(archive), unpacked); err != nil {
			t.Fatalf("the builder's stdout is not an archive the store takes: %v\n%s", err, out)
		}
		bin, err := os.Lstat(filepath.Join(unpacked, "node_modules", ".bin", "demo-mcp"))
		if err != nil || !bin.Mode().IsRegular() || bin.Mode().Perm()&0o111 == 0 {
			t.Fatalf("the unpacked command is not an executable file: %v %v", bin, err)
		}
		script, _ := os.ReadFile(filepath.Join(unpacked, "node_modules", ".bin", "demo-mcp"))
		if want := "#!/bin/sh\nexec '" + filepath.Join(prefix, "node_modules", "demo-mcp", "cli.js") + "' \"$@\"\n"; string(script) != want {
			t.Fatalf("command script = %q, want %q", script, want)
		}
		if run, err := exec.Command(filepath.Join(prefix, "node_modules", ".bin", "demo-mcp")).CombinedOutput(); err != nil || string(run) != "demo\n" {
			t.Fatalf("the command script did not run the command: %q %v", run, err)
		}
		kept, err := os.ReadFile(filepath.Join(prefix, npmDownloadDir, "demo-mcp-1.0.0.tgz"))
		if err != nil || !bytes.Equal(kept, tgz) {
			t.Fatalf("the verified tarball is not the registry's bytes (%v)", err)
		}
		pj, _ := os.ReadFile(filepath.Join(prefix, "package.json"))
		if !strings.Contains(string(pj), "file:"+npmDownloadDir+"/demo-mcp-1.0.0.tgz") {
			t.Fatalf("installed from somewhere other than the verified file: %s", pj)
		}
	})
	t.Run("a tarball left by an earlier attempt is not the one checked", func(t *testing.T) {
		prefix := t.TempDir()
		stale := filepath.Join(prefix, npmDownloadDir)
		if err := os.MkdirAll(stale, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stale, "demo-mcp-0.9.0.tgz"), []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, code, out := buildIn(t, prefix, "demo-mcp", sri); code != 0 || !installed(prefix) {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("the legacy sha1 form is checked too", func(t *testing.T) {
		if prefix, code, out := build(t, "demo-mcp", "sha1:"+hex.EncodeToString(s1[:])); code != 0 || !installed(prefix) {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("the strongest algorithm present decides", func(t *testing.T) {
		// A matching sha1 beside a wrong sha512 must not pass.
		wrong := "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))
		legacySRI := "sha1-" + base64.StdEncoding.EncodeToString(s1[:])
		if prefix, code, out := build(t, "demo-mcp", legacySRI+" "+wrong); code != 4 || installed(prefix) {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("a mismatch refuses before anything is installed", func(t *testing.T) {
		other := sha256.Sum256([]byte("not the tarball"))
		prefix, code, out := build(t, "demo-mcp", "sha256-"+base64.StdEncoding.EncodeToString(other[:]))
		if code != 4 || !strings.Contains(out, "does not match the integrity the registry advertised") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if installed(prefix) {
			t.Fatal("a tarball that failed its check was installed")
		}
	})
	t.Run("no recorded integrity refuses", func(t *testing.T) {
		prefix, code, out := build(t, "demo-mcp", "")
		if code != 3 || !strings.Contains(out, "advertised no integrity") || installed(prefix) {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("shell syntax in a value is a literal", func(t *testing.T) {
		canary := filepath.Join(t.TempDir(), "canary")
		_, code, _ := build(t, "demo-mcp; touch "+canary, sri)
		if code == 0 {
			t.Fatal("npm accepted a package name with shell syntax")
		}
		if _, err := os.Stat(canary); err == nil {
			t.Fatal("the value reached a shell: the canary exists")
		}
	})
}

// TestNpmBuilderChecksEngines runs npm_builder.js under the host's node, as
// TestNpmBuilderRunsUnderNode does, against packages whose engines.node the
// host's Node does or does not satisfy: a mismatch exits
// 5, naming the range, the runtime's Node and the policy key, before
// anything is installed; a satisfied range, or none, installs.
func TestNpmBuilderChecksEngines(t *testing.T) {
	node := hostNode(t)
	pkgJSON := func(name, engines string) string {
		j := `{"name":"` + name + `","version":"1.0.0","bin":{"` + name + `":"cli.js"}`
		if engines != "" {
			j += `,"engines":{"node":"` + engines + `"}`
		}
		return j + "}"
	}
	long := strings.Repeat("d", 160) // past ustar's name and prefix fields
	tarballs := map[string][]byte{
		// Not "package/": npm takes whatever single directory a tarball has.
		"future-mcp": packTarball(t, "future-mcp", pkgJSON("future-mcp", ">=99.0.0")),
		"now-mcp":    packTarball(t, "package", pkgJSON("now-mcp", ">=18.0.0 <99 || ^99.1")),
		"plain-mcp":  packTarball(t, "package", pkgJSON("plain-mcp", "")),
		"bad-mcp":    packTarball(t, "package", pkgJSON("bad-mcp", "not a range")),
		// Two top-level directories: npm strips the first component, so the
		// last package.json written is the one it installs.
		"twice-mcp": packEntries(t, tar.FormatUSTAR,
			[2]string{"package/package.json", pkgJSON("twice-mcp", "")},
			[2]string{"package/cli.js", "console.log('demo')\n"},
			[2]string{"other/package.json", pkgJSON("twice-mcp", ">=99.0.0")}),
		// Names past 100 bytes, as a pax path record and as a GNU long name.
		"pax-mcp": packEntries(t, tar.FormatPAX,
			[2]string{long + "/cli.js", "console.log('demo')\n"},
			[2]string{long + "/package.json", pkgJSON("pax-mcp", ">=99.0.0")}),
		"gnu-mcp": packEntries(t, tar.FormatGNU,
			[2]string{long + "/cli.js", "console.log('demo')\n"},
			[2]string{long + "/package.json", pkgJSON("gnu-mcp", ">=99.0.0")}),
	}
	reg := fakeNPMRegistry(t, tarballs)
	build := func(t *testing.T, pkg string) (string, int, string) {
		t.Helper()
		sum := sha512.Sum512(tarballs[pkg])
		prefix := t.TempDir()
		code, out := runNpmBuilder(t, node, reg, prefix, pkg, "sha512-"+base64.StdEncoding.EncodeToString(sum[:]))
		return prefix, code, out
	}
	installed := func(prefix, pkg string) bool {
		_, err := os.Stat(filepath.Join(prefix, "node_modules", pkg, "package.json"))
		return err == nil
	}

	t.Run("a Node the package excludes refuses before anything is installed", func(t *testing.T) {
		prefix, code, out := build(t, "future-mcp")
		if code != npmEnginesRefused || installed(prefix, "future-mcp") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		for _, want := range []string{"future-mcp@1.0.0 requires Node >=99.0.0", "runs Node v", "images.nodeRuntime"} {
			if !strings.Contains(out, want) {
				t.Errorf("the refusal does not say %q:\n%s", want, out)
			}
		}
	})
	t.Run("an unparseable range refuses, as npm's engine-strict would, and says so", func(t *testing.T) {
		prefix, code, out := build(t, "bad-mcp")
		if code != npmEnginesRefused || installed(prefix, "bad-mcp") || !strings.Contains(out, "not a range npm can read") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("the last package.json, the one npm installs, decides", func(t *testing.T) {
		if prefix, code, out := build(t, "twice-mcp"); code != npmEnginesRefused || installed(prefix, "twice-mcp") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	for _, pkg := range []string{"pax-mcp", "gnu-mcp"} {
		t.Run("a long name is read from its "+pkg[:3]+" record", func(t *testing.T) {
			if prefix, code, out := build(t, pkg); code != npmEnginesRefused || installed(prefix, pkg) {
				t.Fatalf("exit %d:\n%s", code, out)
			}
		})
	}
	for _, pkg := range []string{"now-mcp", "plain-mcp"} {
		t.Run(pkg+": a range the runtime satisfies, or none, installs", func(t *testing.T) {
			if prefix, code, out := build(t, pkg); code != 0 || !installed(prefix, pkg) {
				t.Fatalf("exit %d:\n%s", code, out)
			}
		})
	}
}

// hostNode is the host's node for the builder tests, which skip where the
// host has no node and npm.
func hostNode(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("runs npm")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on this host")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("no npm on this host")
	}
	return node
}

// fakeNPMRegistry serves each tarball as version 1.0.0 of the package it is
// keyed by, with the SRI sha512 and legacy shasum npm's packuments carry.
func fakeNPMRegistry(t *testing.T, tarballs map[string][]byte) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{name}", func(w http.ResponseWriter, req *http.Request) {
		name := req.PathValue("name")
		tgz, ok := tarballs[name]
		if !ok {
			http.NotFound(w, req)
			return
		}
		s512 := sha512.Sum512(tgz)
		s1 := sha1.Sum(tgz) //nolint:gosec // see import
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": name, "dist-tags": map[string]string{"latest": "1.0.0"},
			"versions": map[string]any{"1.0.0": map[string]any{
				"name": name, "version": "1.0.0", "bin": map[string]string{name: "cli.js"},
				"dist": map[string]string{
					"tarball":   "http://" + req.Host + "/" + name + "/-/" + name + "-1.0.0.tgz",
					"integrity": "sha512-" + base64.StdEncoding.EncodeToString(s512[:]),
					"shasum":    hex.EncodeToString(s1[:]),
				},
			}},
		})
	})
	mux.HandleFunc("GET /{name}/-/{file}", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write(tarballs[req.PathValue("name")])
	})
	reg := httptest.NewServer(mux)
	t.Cleanup(reg.Close)
	return reg.URL
}

// runNpmBuilder runs npm_builder.js the way the runtime image does, with
// the host's node, prefix standing in for the in-container one, and
// returns its exit code and what it printed on stderr.
func runNpmBuilder(t *testing.T, node, registry, prefix, pkg, integrity string) (int, string) {
	t.Helper()
	code, out, _ := runNpmBuilderArchive(t, node, registry, prefix, pkg, integrity)
	return code, out
}

// runNpmBuilderArchive is runNpmBuilder that also returns the builder's
// stdout, the archive of the tree it installed.
func runNpmBuilderArchive(t *testing.T, node, registry, prefix, pkg, integrity string) (int, string, []byte) {
	t.Helper()
	home := t.TempDir()
	r := Resolved{Package: pkg, Version: "1.0.0", Integrity: integrity}
	argv := npmArgv(&r, policy.MCPRegistries{NPM: registry})
	// argv: env HOME=… npm_config_cache=… node -e <script> PREFIX PKG VER REG INTEGRITY
	if argv[3] != "node" || argv[4] != "-e" {
		t.Fatalf("argv shape changed: %q", argv[:5])
	}
	argv[6] = prefix // the in-container prefix, here a host directory
	cmd := exec.Command(node, argv[4:]...)
	cmd.Env = append(os.Environ(), "HOME="+home, "npm_config_cache="+filepath.Join(home, "cache"), "npm_config_update_notifier=false")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, stderr.String(), stdout.Bytes()
	case errors.As(err, &exit):
		return exit.ExitCode(), stderr.String(), stdout.Bytes()
	default:
		t.Fatalf("run builder: %v", err)
		return 0, "", nil
	}
}

// The builder's download directory and the Go constant are one name.
func TestNpmBuilderDownloadDirMatches(t *testing.T) {
	m := regexp.MustCompile(`const DOWNLOAD_DIR = "([^"]+)";`).FindStringSubmatch(npmBuilder)
	if m == nil || m[1] != npmDownloadDir {
		t.Fatalf("npm_builder.js DOWNLOAD_DIR = %v, want %q", m, npmDownloadDir)
	}
}

// demoTarball builds demo-mcp 1.0.0 as npm publishes it: a gzipped tar with
// everything under package/.
func demoTarball(t *testing.T) []byte {
	t.Helper()
	return packTarball(t, "package", `{"name":"demo-mcp","version":"1.0.0","bin":{"demo-mcp":"cli.js"}}`)
}

// packTarball builds a one-package tarball under the top-level directory
// dir, with the given package.json and a cli.js.
func packTarball(t *testing.T, dir, packageJSON string) []byte {
	t.Helper()
	return packEntries(t, tar.FormatUSTAR,
		[2]string{dir + "/package.json", packageJSON},
		[2]string{dir + "/cli.js", "#!/usr/bin/env node\nconsole.log('demo')\n"})
}

// packEntries gzips a tar of the given files, in order, in one format.
func packEntries(t *testing.T, format tar.Format, files ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o644, Size: int64(len(f[1])), Typeflag: tar.TypeReg, Format: format}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// EnsureInstalled hands the npm builder the integrity recorded at
// resolution, as the last argument, and runs the embedded program.
func TestEnsureInstalledPassesTheRecordedIntegrityToTheNpmBuilder(t *testing.T) {
	fake := &dockerx.Fake{}
	withRunner(fake)
	res := []Resolved{{Source: "npm", Package: "server-x", Version: "1.0.0", ID: "npm-server-x", BinName: "server-x", Integrity: "sha512-RECORDED"}}
	regs := policy.MCPRegistries{NPM: "https://npm.invalid", PyPI: "https://unused.invalid"}
	images := policy.Images{NodeRuntime: "node:test", PythonRuntime: "python:test"}
	if err := EnsureInstalled(t.Context(), fake, res, t.TempDir(), regs, images, policy.DockerPolicy{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	run, ok := fake.FindRun("agentry-mcpinstall-npm-server-x")
	if !ok {
		t.Fatal("the builder did not run")
	}
	a := run.Argv
	if len(a) != 11 || a[3] != "node" || a[4] != "-e" || a[5] != npmBuilder {
		t.Fatalf("builder argv = %q", a[:min(len(a), 5)])
	}
	if got := a[6:]; got[0] != "/opt/agentry-mcp/npm-server-x" || got[1] != "server-x" || got[2] != "1.0.0" || got[3] != "https://npm.invalid" || got[4] != "sha512-RECORDED" {
		t.Fatalf("builder values = %q", got)
	}
}

// The npm builder's archive writer, run under the host's node on a tree with
// names past the ustar field's 100 bytes (deep node_modules paths are), a
// name that is not ASCII, an empty file, executable bits and a large file:
// Go's reader gets every name, body and bit back.
func TestNpmBuilderArchiveHoldsTheTree(t *testing.T) {
	node := hostNode(t)
	dir := t.TempDir()
	long := strings.Repeat("deep-folder-name/", 8) + "a-file-whose-path-is-longer-than-a-ustar-name.js"
	files := map[string]struct {
		body string
		mode os.FileMode
	}{
		long:               {"module.exports = 1;\n", 0o644},
		"bin/run":          {"#!/bin/sh\necho run\n", 0o755},
		"café/ünïcode.txt": {"text\n", 0o644},
		"empty":            {"", 0o644},
		"big.bin":          {strings.Repeat("x", 3<<20+17), 0o644},
	}
	for name, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.body), f.mode); err != nil {
			t.Fatal(err)
		}
	}
	// The builder's own functions, without its main.
	script := strings.TrimSuffix(strings.TrimSpace(npmBuilder), "main();") + "\narchive(process.argv[1], writeAll);\n"
	cmd := exec.Command(node, "-e", script, dir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("archive: %v\n%s", err, stderr.String())
	}
	tr := tar.NewReader(&stdout)
	got := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Go cannot read the archive: %v", err)
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		want, ok := files[h.Name]
		if !ok {
			t.Fatalf("unexpected entry %q", h.Name)
		}
		body, _ := io.ReadAll(tr)
		if string(body) != want.body || (h.Mode&0o111 != 0) != (want.mode&0o111 != 0) {
			t.Fatalf("%s: %d bytes, mode %o; want %d bytes, mode %o", h.Name, len(body), h.Mode, len(want.body), want.mode)
		}
		got[h.Name] = true
	}
	if len(got) != len(files) {
		t.Fatalf("archive holds %v, want every file of %d", got, len(files))
	}
}
