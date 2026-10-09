// Tests for the build-context contract (RUNNER-PLAN §R7): Materialize
// must produce exactly the standalone layout the Dockerfile builds, and
// every embedded source must stay stdlib-only — the guard that keeps the
// context compilable with no network beyond the base images, forever.

package runnerimage

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterializeLayout(t *testing.T) {
	dir := t.TempDir()
	if err := Materialize(dir); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	// Expected layout, derived from the embed FS itself so a new source
	// file cannot silently be left out of the context.
	want := map[string]bool{
		"Dockerfile":    true,
		"go.mod":        true,
		".dockerignore": true,
	}
	err := fs.WalkDir(contextFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || p == "Dockerfile" || strings.HasSuffix(p, "_test.go") {
			return err
		}
		want["runnerimage/"+p] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The names the task force actually depends on must be in that set.
	for _, p := range []string{
		"runnerimage/main.go",
		"runnerimage/toolsrv/spec.go",
		"runnerimage/toolsrv/mcp.go",
		"runnerimage/toolsrv/paths.go",
		"runnerimage/toolsrv/tools.go",
		"runnerimage/netproxy/proxy.go",
		"runnerimage/minimize/minimize.go",
	} {
		if !want[p] {
			t.Fatalf("embedded context is missing %s", p)
		}
	}

	got := map[string]bool{}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		got[filepath.ToSlash(rel)] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for p := range want {
		if !got[p] {
			t.Errorf("materialized context is missing %s", p)
		}
	}
	for p := range got {
		if !want[p] {
			t.Errorf("materialized context has unexpected file %s", p)
		}
		if strings.HasSuffix(p, "_test.go") {
			t.Errorf("test file %s leaked into the build context", p)
		}
	}

	gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gomod) != contextGoMod {
		t.Errorf("go.mod = %q", gomod)
	}
	di, err := os.ReadFile(filepath.Join(dir, ".dockerignore"))
	if err != nil {
		t.Fatal(err)
	}
	if string(di) != "*_test.go\n" {
		t.Errorf(".dockerignore = %q", di)
	}
}

// TestMaterializeStripsMainConstraint pins both halves of the main.go
// arrangement: the repo copy must carry the ignore tag (or this directory
// would hold two packages) and the materialized copy must not (or the
// context's ./runnerimage would hold no buildable files).
func TestMaterializeStripsMainConstraint(t *testing.T) {
	raw, err := contextFS.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), mainIgnoreTag) {
		t.Fatalf("repo main.go must start with %q", strings.TrimSpace(mainIgnoreTag))
	}

	dir := t.TempDir()
	if err := Materialize(dir); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(filepath.Join(dir, "runnerimage", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	// A live constraint is a LINE of its own before the package clause;
	// prose in the doc comment may mention the tag without arming it.
	head, _, ok := strings.Cut(string(out), "\npackage main")
	if !ok {
		t.Fatal("materialized main.go lost its package clause")
	}
	for _, line := range strings.Split(head, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//go:build") {
			t.Fatalf("materialized main.go still carries a build constraint (%q); the context would not build", line)
		}
	}
}

func TestMaterializeIsRerunnable(t *testing.T) {
	dir := t.TempDir()
	if err := Materialize(dir); err != nil {
		t.Fatalf("first Materialize: %v", err)
	}
	if err := Materialize(dir); err != nil {
		t.Fatalf("second Materialize over the same dir: %v", err)
	}
}

// TestEmbeddedSourcesStdlibOnly enforces the §R7 self-containment rule at
// the import graph: every embedded .go file (tests included — they live
// in the same tree) may import only the standard library and the
// context's own runnerimage/* packages. First path element containing a
// dot ⇒ a module dependency ⇒ the docker build would need the network.
func TestEmbeddedSourcesStdlibOnly(t *testing.T) {
	const internalPrefix = "github.com/blaktron/agentry-cli/runnerimage/"
	fset := token.NewFileSet()
	err := fs.WalkDir(contextFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
			return err
		}
		data, err := contextFS.ReadFile(p)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, p, data, parser.ImportsOnly)
		if err != nil {
			t.Errorf("parse %s: %v", p, err)
			return nil
		}
		for _, imp := range f.Imports {
			ip := strings.Trim(imp.Path.Value, `"`)
			if rest, ok := strings.CutPrefix(ip, internalPrefix); ok {
				// Part of the context itself, so it must be embedded:
				// a package missing from the go:embed line breaks the
				// image build and nothing else (agentry-cli#596).
				if matches, _ := fs.Glob(contextFS, rest+"/*.go"); len(matches) == 0 {
					t.Errorf("%s imports %q, which the embedded context does not carry; add %s/*.go to the go:embed line", p, ip, rest)
				}
				continue
			}
			first := ip
			if i := strings.Index(ip, "/"); i >= 0 {
				first = ip[:i]
			}
			if strings.Contains(first, ".") {
				t.Errorf("%s imports %q — not stdlib; the embedded context must compile with no module downloads (§R7)", p, ip)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestImageTag(t *testing.T) {
	tests := []struct {
		version string
		want    string
	}{
		{"1.2.3", "agentry-runner:1.2.3"},
		{"v0.3.0-rc.1", "agentry-runner:v0.3.0-rc.1"},
		{"dev", "agentry-runner:dev"},
		{"", "agentry-runner:dev"},
		{"1.0+build.5", "agentry-runner:dev"}, // '+' is outside the tag charset
		{"has space", "agentry-runner:dev"},
		{"v1/inject", "agentry-runner:dev"},
		{"-leading", "agentry-runner:dev"},
		{".leading", "agentry-runner:dev"},
		{strings.Repeat("a", 129), "agentry-runner:dev"},
	}
	for _, tc := range tests {
		if got := ImageTag(tc.version); got != tc.want {
			t.Errorf("ImageTag(%q) = %q, want %q", tc.version, got, tc.want)
		}
	}
}

// TestContextGoModFollowsTheModule: the runner context's go.mod names the
// CLI's module path and its go line to the patch, so a builder older than
// the CLI's Go refuses under the Go images' GOTOOLCHAIN=local instead of
// building the runner with an older standard library (agentry-cli#786).
func TestContextGoModFollowsTheModule(t *testing.T) {
	directive := func(gomod, name string) string {
		for _, line := range strings.Split(gomod, "\n") {
			if f := strings.Fields(line); len(f) == 2 && f[0] == name {
				return f[1]
			}
		}
		return ""
	}
	mod, err := os.ReadFile(filepath.Join("..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"module", "go"} {
		want := directive(string(mod), name)
		if want == "" {
			t.Fatalf("the repository's go.mod has no %s line", name)
		}
		if got := directive(contextGoMod, name); got != want {
			t.Errorf("the runner context's go.mod says %s %s; the CLI's says %s %s", name, got, name, want)
		}
	}
}
