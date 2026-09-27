package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	pkgarchive "github.com/blaktron/agentry/pkg/archive"
	"github.com/blaktron/agentry/pkg/manifest"
)

// A release that ships a knowledge pack (plan knowledge.md §4.6,
// agentry-cli#320): the pack is loaded from the release's own files and
// mounted as an attached one is, its name is in the contract the consent
// shows with its size, and publish refuses a release that leaves it out.

// shippingPackage is the pure-text fixture shipping the pack name, in a
// fresh directory.
func shippingPackage(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	src := repoPath(t, "testdata/runner/pure-text")
	for _, f := range []string{"AGENT.md", "agent.yaml"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatal(err)
		}
		if f == "agent.yaml" {
			b = []byte(strings.Replace(string(b), "publish:\n  include: [agent.yaml, AGENT.md]",
				"knowledge:\n  ships:\n    - name: "+name+"\npublish:\n  include: [agent.yaml, AGENT.md, knowledge/**]", 1))
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, manifest.KnowledgeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := knowledge.CopyPack(handMadePack(t, name), filepath.Join(dir, manifest.KnowledgeDir, name)); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunMountsTheReleasesShippedPack(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	dry := func(pkg string, extra ...string) (string, string, error) {
		args := append([]string{"run", "--local", pkg, "--allow-unsigned", "--dry-run", "--policy", policyPath}, extra...)
		return runCLI(t, strings.NewReader(""), args...)
	}

	t.Run("the shipped pack is named as the release's", func(t *testing.T) {
		_, stderr, err := dry(shippingPackage(t, "runbooks"))
		if err != nil {
			t.Fatalf("%v (stderr %q)", err, stderr)
		}
		if !strings.Contains(stderr, "knowledge pack runbooks: shipped with the release, 1 page") ||
			!strings.Contains(stderr, "knowledge: ships runbooks (") {
			t.Fatalf("stderr does not name the shipped pack and its size: %q", stderr)
		}
	})
	t.Run("a shipped pack changed after its manifest", func(t *testing.T) {
		pkg := shippingPackage(t, "runbooks")
		if err := os.WriteFile(filepath.Join(pkg, "knowledge", "runbooks", "sources", "notes.txt"), []byte("changed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err := dry(pkg)
		wantClassified(t, err, engine.ExitVerify)
		if !strings.Contains(err.Error(), "the release ships the knowledge pack runbooks") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("an attached pack cannot take a shipped pack's name", func(t *testing.T) {
		_, _, err := dry(shippingPackage(t, "runbooks"), "--knowledge", handMadePack(t, "runbooks"))
		wantClassified(t, err, engine.ExitCLI)
		if !strings.Contains(err.Error(), "the release ships a knowledge pack of that name") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("an attached pack beside a shipped one", func(t *testing.T) {
		_, stderr, err := dry(shippingPackage(t, "runbooks"), "--knowledge", handMadePack(t, "notes"))
		if err != nil {
			t.Fatalf("%v (stderr %q)", err, stderr)
		}
		if !strings.Contains(stderr, "knowledge pack runbooks: shipped with the release") || !strings.Contains(stderr, "knowledge pack notes: 1 page") {
			t.Fatalf("stderr = %q", stderr)
		}
	})
}

func TestShippedPackSizesFromTheArchive(t *testing.T) {
	pkg := shippingPackage(t, "runbooks")
	files, err := pkgarchive.WalkWorkspace(pkg)
	if err != nil {
		t.Fatal(err)
	}
	m, err := readWorkspaceManifest(pkg)
	if err != nil {
		t.Fatal(err)
	}
	var arch bytes.Buffer
	if _, err := pkgarchive.BuildArchive(pkg, manifest.ResolveInclude(m.Publish.Include, files), &arch); err != nil {
		t.Fatal(err)
	}
	sizes, err := archivePackSizes(arch.Bytes(), m.ShippedPacks())
	if err != nil {
		t.Fatal(err)
	}
	p, err := knowledge.Load(filepath.Join(pkg, "knowledge", "runbooks"))
	if err != nil {
		t.Fatal(err)
	}
	if sizes["runbooks"] != p.Manifest.Bytes {
		t.Fatalf("archive size %d, pack bytes %d", sizes["runbooks"], p.Manifest.Bytes)
	}
	lines := contractLines(m.Contract(), sizes)
	if got := lines[len(lines)-1]; got != "knowledge: ships runbooks ("+byteSize(p.Manifest.Bytes)+")" {
		t.Fatalf("contract line = %q", got)
	}
	if got := capabilityLines(m.Contract()); got[len(got)-1] != "knowledge: ships runbooks" {
		t.Fatalf("capability lines = %v", got)
	}
}

func TestPublishRefusesAShippedPackLeftOutOfTheArchive(t *testing.T) {
	pkg := shippingPackage(t, "runbooks")
	m, err := readWorkspaceManifest(pkg)
	if err != nil {
		t.Fatal(err)
	}
	files, err := pkgarchive.WalkWorkspace(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if err := shippedPacksIncluded(m, manifest.ResolveInclude(m.Publish.Include, files)); err != nil {
		t.Fatalf("an included pack was refused: %v", err)
	}
	if err := shippedPacksIncluded(m, manifest.ResolveInclude([]string{"agent.yaml", "AGENT.md"}, files)); err == nil || !strings.Contains(err.Error(), "V11") {
		t.Fatalf("a pack left out of the archive: err = %v", err)
	}
}

func TestByteSize(t *testing.T) {
	for n, want := range map[int64]string{216: "216 bytes", 48_000: "48.0 KB", 4_200_000: "4.2 MB", 10 << 20: "10.5 MB"} {
		if got := byteSize(n); got != want {
			t.Errorf("byteSize(%d) = %q, want %q", n, got, want)
		}
	}
}
