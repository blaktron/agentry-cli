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
		_, _ = w.Write([]byte(`{"urls":[{"packagetype":"bdist_wheel","digests":{"sha256":"cafe"}}]}`))
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
