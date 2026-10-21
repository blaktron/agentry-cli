package mcpinstall

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// TestPypiBuilderRunsUnderPython runs pypi_builder.py with the host's python3
// against a local PEP 503 index serving one generated wheel, the way the
// runtime image runs it: it installs the pinned wheel, records the dist's
// console scripts, refuses a file the registry did not advertise (exit 3) and
// one whose digest differs (exit 4), and passes a value with shell syntax to
// pip as a literal. Skipped where the host has no python3 with venv.
func TestPypiBuilderRunsUnderPython(t *testing.T) {
	if testing.Short() {
		t.Skip("creates Python virtual environments")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 on this host")
	}
	if out, err := exec.Command(py, "-m", "venv", "--help").CombinedOutput(); err != nil {
		t.Skipf("python3 has no venv: %s", out)
	}
	whl, name := demoWheel(t)
	sum := sha256.Sum256(whl)
	digest := hex.EncodeToString(sum[:])
	mux := http.NewServeMux()
	mux.HandleFunc("GET /simple/demo-mcp/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `<html><body><a href="/files/%s#sha256=%s">%s</a></body></html>`, name, digest, name)
	})
	mux.HandleFunc("GET /files/"+name, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(whl) })
	idx := httptest.NewServer(mux)
	t.Cleanup(idx.Close)

	var archive []byte
	build := func(t *testing.T, pkg string, sums string) (string, int, string) {
		t.Helper()
		// The prefix does not exist yet: the builder makes it, as it does
		// inside the container.
		prefix := filepath.Join(t.TempDir(), "prefix")
		r := Resolved{Package: pkg, Version: "1.0.0"}
		argv := pipArgv(&r, policy.MCPRegistries{PyPI: idx.URL})
		argv[3] = prefix // the in-container prefix, here a host directory
		cmd := exec.Command(py, argv[1:]...)
		cmd.Stdin = strings.NewReader(sums)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		archive = stdout.Bytes()
		var exit *exec.ExitError
		switch {
		case err == nil:
			return prefix, 0, stderr.String()
		case errors.As(err, &exit):
			return prefix, exit.ExitCode(), stderr.String()
		default:
			t.Fatalf("run builder: %v", err)
			return "", 0, ""
		}
	}

	t.Run("installs the pinned wheel and records its console scripts", func(t *testing.T) {
		prefix, code, out := build(t, "demo-mcp", digest+"  "+name+"\n")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		// The sums it read on stdin are kept in the tree; the tree holds no
		// link (the interpreter is a copy, lib64 is gone); and stdout is the
		// archive of the tree alone, which unpacks into a store.
		if kept, err := os.ReadFile(filepath.Join(prefix, artifactSumsFile)); err != nil || string(kept) != digest+"  "+name+"\n" {
			t.Fatalf("artifact sums kept = %q (%v)", kept, err)
		}
		unpacked := t.TempDir()
		if err := unpackStore(bytes.NewReader(archive), unpacked); err != nil {
			t.Fatalf("the builder's stdout is not an archive the store takes: %v\n%s", err, out)
		}
		if fi, err := os.Lstat(filepath.Join(unpacked, "venv", "bin", "python")); err != nil || !fi.Mode().IsRegular() {
			t.Fatalf("venv/bin/python is not a file of its own: %v %v", fi, err)
		}
		if _, err := os.Lstat(filepath.Join(unpacked, "venv", "lib64")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("venv/lib64 is still there: %v", err)
		}
		if fi, err := os.Stat(filepath.Join(unpacked, "venv", "bin", "demo-mcp")); err != nil || fi.Mode().Perm()&0o111 == 0 {
			t.Fatalf("the console script did not keep its executable bits: %v %v", fi, err)
		}
		// The package's modules are compiled to load without a check of
		// their source's time, which the store does not keep: a pyc's
		// flags word is 1 (hash-based, unchecked).
		pycs, _ := filepath.Glob(filepath.Join(unpacked, "venv", "lib", "python3*", "site-packages", "demo_mcp", "__pycache__", "*.pyc"))
		if len(pycs) == 0 {
			t.Fatal("the package's modules were not compiled")
		}
		for _, pyc := range pycs {
			b, err := os.ReadFile(pyc)
			if err != nil || len(b) < 8 || b[4] != 1 || b[5] != 0 || b[6] != 0 || b[7] != 0 {
				t.Fatalf("%s is not an unchecked hash-based pyc: % x (%v)", pyc, b[:min(len(b), 8)], err)
			}
		}
		if got, want := treeListing(t, unpacked), treeListing(t, prefix); got != want {
			t.Fatalf("the archive does not hold the tree:\n%s\nwant\n%s", got, want)
		}
		scripts, err := os.ReadFile(filepath.Join(prefix, consoleScriptsFile))
		if err != nil || string(scripts) != "demo-mcp\n" {
			t.Fatalf("console scripts = %q (%v)", scripts, err)
		}
		if _, err := os.Stat(filepath.Join(prefix, "venv", "bin", "demo-mcp")); err != nil {
			t.Fatalf("console script not installed: %v", err)
		}
	})
	t.Run("a file the registry did not advertise refuses", func(t *testing.T) {
		if _, code, out := build(t, "demo-mcp", digest+"  other-1.0.0-py3-none-any.whl\n"); code != 3 || !strings.Contains(out, "is not an artifact the registry advertised") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("a digest mismatch refuses", func(t *testing.T) {
		if _, code, out := build(t, "demo-mcp", strings.Repeat("0", 64)+"  "+name+"\n"); code != 4 || !strings.Contains(out, "does not match the advertised digest") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("shell syntax in a value is a literal", func(t *testing.T) {
		canary := filepath.Join(t.TempDir(), "canary")
		_, code, _ := build(t, "demo-mcp; touch "+canary, digest+"  "+name+"\n")
		if code == 0 {
			t.Fatal("pip accepted a requirement with shell syntax")
		}
		if _, err := os.Stat(canary); err == nil {
			t.Fatal("the value reached a shell: the canary exists")
		}
	})
}

// treeListing lists a tree's folders and files with their sizes and
// executable bits, one per line in walk order.
func treeListing(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			fmt.Fprintf(&b, "D %s\n", rel)
		} else {
			fmt.Fprintf(&b, "F %s %d %o\n", rel, info.Size(), info.Mode().Perm()&0o111)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// demoWheel builds a minimal pure-Python wheel, demo-mcp 1.0.0, with one
// console script and a RECORD pip accepts.
func demoWheel(t *testing.T) ([]byte, string) {
	t.Helper()
	const di = "demo_mcp-1.0.0.dist-info/"
	files := [][2]string{
		{"demo_mcp/__init__.py", "def main():\n    print('demo')\n"},
		{di + "METADATA", "Metadata-Version: 2.1\nName: demo-mcp\nVersion: 1.0.0\n"},
		{di + "WHEEL", "Wheel-Version: 1.0\nGenerator: agentry-test\nRoot-Is-Purelib: true\nTag: py3-none-any\n"},
		{di + "entry_points.txt", "[console_scripts]\ndemo-mcp = demo_mcp:main\n"},
	}
	var record strings.Builder
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		write(f[0], f[1])
		s := sha256.Sum256([]byte(f[1]))
		_, _ = fmt.Fprintf(&record, "%s,sha256=%s,%d\n", f[0], base64.RawURLEncoding.EncodeToString(s[:]), len(f[1]))
	}
	record.WriteString(di + "RECORD,,\n")
	write(di+"RECORD", record.String())
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), "demo_mcp-1.0.0-py3-none-any.whl"
}
