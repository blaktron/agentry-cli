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
// runtime image runs it (agentry-cli#91): it installs the pinned tarball
// when it matches the recorded integrity (SRI sha512, or the legacy sha1
// form), refuses a mismatch (exit 4) and a missing integrity (exit 3)
// before anything is installed, and passes a value with shell syntax to npm
// as a literal. Skipped where the host has no node and npm.
func TestNpmBuilderRunsUnderNode(t *testing.T) {
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
	tgz := demoTarball(t)
	s512 := sha512.Sum512(tgz)
	sri := "sha512-" + base64.StdEncoding.EncodeToString(s512[:])
	s1 := sha1.Sum(tgz) //nolint:gosec // see import
	mux := http.NewServeMux()
	mux.HandleFunc("GET /demo-mcp", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "demo-mcp", "dist-tags": map[string]string{"latest": "1.0.0"},
			"versions": map[string]any{"1.0.0": map[string]any{
				"name": "demo-mcp", "version": "1.0.0", "bin": map[string]string{"demo-mcp": "cli.js"},
				"dist": map[string]string{"tarball": "http://" + req.Host + "/demo-mcp/-/demo-mcp-1.0.0.tgz", "integrity": sri, "shasum": hex.EncodeToString(s1[:])},
			}},
		})
	})
	mux.HandleFunc("GET /demo-mcp/-/demo-mcp-1.0.0.tgz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tgz) })
	reg := httptest.NewServer(mux)
	t.Cleanup(reg.Close)

	var buildIn func(t *testing.T, prefix, pkg, integrity string) (string, int, string)
	build := func(t *testing.T, pkg, integrity string) (string, int, string) {
		t.Helper()
		return buildIn(t, t.TempDir(), pkg, integrity)
	}
	buildIn = func(t *testing.T, prefix, pkg, integrity string) (string, int, string) {
		t.Helper()
		home := t.TempDir()
		r := Resolved{Package: pkg, Version: "1.0.0", Integrity: integrity}
		argv := npmArgv(&r, policy.MCPRegistries{NPM: reg.URL})
		// argv: env HOME=… npm_config_cache=… node -e <script> PREFIX PKG VER REG INTEGRITY
		if argv[3] != "node" || argv[4] != "-e" {
			t.Fatalf("argv shape changed: %q", argv[:5])
		}
		argv[6] = prefix // the in-container prefix, here a host directory
		cmd := exec.Command(node, argv[4:]...)
		cmd.Env = append(os.Environ(), "HOME="+home, "npm_config_cache="+filepath.Join(home, "cache"), "npm_config_update_notifier=false")
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		switch {
		case err == nil:
			return prefix, 0, string(out)
		case errors.As(err, &exit):
			return prefix, exit.ExitCode(), string(out)
		default:
			t.Fatalf("run builder: %v", err)
			return "", 0, ""
		}
	}
	installed := func(prefix string) bool {
		_, err := os.Stat(filepath.Join(prefix, "node_modules", ".bin", "demo-mcp"))
		return err == nil
	}

	t.Run("installs the pinned tarball when it matches the recorded SRI", func(t *testing.T) {
		prefix, code, out := build(t, "demo-mcp", sri)
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if !installed(prefix) {
			t.Fatal("the bin was not linked")
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
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range [][2]string{
		{"package/package.json", `{"name":"demo-mcp","version":"1.0.0","bin":{"demo-mcp":"cli.js"}}`},
		{"package/cli.js", "#!/usr/bin/env node\nconsole.log('demo')\n"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o644, Size: int64(len(f[1])), Typeflag: tar.TypeReg}); err != nil {
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
	res := []Resolved{{Source: "npm", Package: "server-x", Version: "1.0.0", ID: "npm-server-x", BinName: "server-x", Integrity: "sha512-RECORDED"}}
	regs := policy.MCPRegistries{NPM: "http://npm.invalid", PyPI: "http://unused.invalid"}
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
	if got := a[6:]; got[0] != "/opt/agentry-mcp/npm-server-x" || got[1] != "server-x" || got[2] != "1.0.0" || got[3] != "http://npm.invalid" || got[4] != "sha512-RECORDED" {
		t.Fatalf("builder values = %q", got)
	}
}
