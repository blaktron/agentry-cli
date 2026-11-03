package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	pkgarchive "github.com/blaktron/agentry/pkg/archive"
	"github.com/blaktron/agentry/pkg/manifest"
)

// A release that ships a knowledge pack: the pack is loaded from the
// release's own files and mounted as an attached one is, its name is in
// the contract the consent shows with its size, and publish refuses a
// release that leaves it out.

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
		pkg := shippingPackage(t, "runbooks")
		_, stderr, err := dry(pkg)
		if err != nil {
			t.Fatalf("%v (stderr %q)", err, stderr)
		}
		if !strings.Contains(stderr, "knowledge pack runbooks: 1 page") || !strings.Contains(stderr, "shipped with the release — mounted read-only at /knowledge/runbooks") ||
			!strings.Contains(stderr, "knowledge: ships runbooks (") {
			t.Fatalf("stderr does not name the shipped pack and its size: %q", stderr)
		}
		pack, err := knowledge.Load(filepath.Join(pkg, "knowledge", "runbooks"))
		if err != nil {
			t.Fatal(err)
		}
		shipped := engine.AttachPack(pack)
		if want := fmt.Sprintf("knowledge pack runbooks: 1 page, %d bytes, %s, shipped with the release — mounted read-only at /knowledge/runbooks\n", shipped.Bytes, shipped.Digest); !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
		if want := "\n  knowledge: ships runbooks (" + byteSize(shipped.Bytes) + ")\n"; !strings.Contains(stderr, want) {
			t.Errorf("the consent does not show the shipped pack's size, want %q:\n%s", want, stderr)
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
	t.Run("a shipped pack past Agentry's cap", func(t *testing.T) {
		pkg := shippingPackage(t, "runbooks")
		dir := filepath.Join(pkg, "knowledge", "runbooks")
		if err := os.WriteFile(filepath.Join(dir, "sources", "big.txt"), bytes.Repeat([]byte("x"), knowledge.MaxBytes+1), 0o644); err != nil {
			t.Fatal(err)
		}
		d, err := knowledge.TreeDigest(dir)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, knowledge.ManifestFile))
		if err != nil {
			t.Fatal(err)
		}
		var m knowledge.Manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		m.SHA256, m.Bytes = d.SHA256, d.Bytes
		raw, _ = json.MarshalIndent(m, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, knowledge.ManifestFile), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err = dry(pkg)
		wantClassified(t, err, engine.ExitVerify)
		if !strings.Contains(err.Error(), "a shipped pack is at most 10 MB") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("an attached pack cannot take a shipped pack's name", func(t *testing.T) {
		_, _, err := dry(shippingPackage(t, "runbooks"), "--knowledge", handMadePack(t, "runbooks"))
		wantClassified(t, err, engine.ExitCLI)
		if want := "--knowledge runbooks: the release ships a knowledge pack of that name, mounted at /knowledge/runbooks; attach a pack with another name"; err.Error() != want {
			t.Fatalf("err = %q, want %q", err, want)
		}
	})
	t.Run("an attached pack beside a shipped one", func(t *testing.T) {
		_, stderr, err := dry(shippingPackage(t, "runbooks"), "--knowledge", handMadePack(t, "notes"))
		if err != nil {
			t.Fatalf("%v (stderr %q)", err, stderr)
		}
		if !strings.Contains(stderr, "shipped with the release — mounted read-only at /knowledge/runbooks") || !strings.Contains(stderr, "knowledge pack notes: 1 page") {
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
	// The receipt names the release's bytes: the archive of an installed
	// release, the tree of a local one.
	for source, want := range map[string]string{"registry": "archive sha256:abc", "local": "local package tree sha256:abc"} {
		packs, err := shippedKnowledge(pkg, m, &installdir.Meta{Source: source, Digest: "sha256:abc"})
		if err != nil || len(packs) != 1 || packs[0].Shipped == nil || packs[0].Shipped.Release != want || packs[0].Shipped.InWorkspace != "knowledge/runbooks" {
			t.Errorf("%s release: %+v, %v; want the release named %q", source, packs, err, want)
		}
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
	lines := capabilityLinesSized(m.Contract(), sizes)
	if got := lines[len(lines)-1]; got != "knowledge: ships runbooks ("+byteSize(p.Manifest.Bytes)+")" {
		t.Fatalf("contract line = %q", got)
	}
	if got := capabilityLines(m.Contract()); got[len(got)-1] != "knowledge: ships runbooks" {
		t.Fatalf("capability lines = %v", got)
	}

	// The tree of a pack is its wiki and its sources; the entries may be
	// named from the archive's root with a leading ./, and another pack's
	// files are not the release's.
	ships := []string{"runbooks"}
	named := tarZst(t, []tarFile{
		{"./knowledge/runbooks/wiki/a.md", 10}, {"./knowledge/runbooks/sources/b.txt", 5}, {"./knowledge/runbooks/manifest.json", 100},
		{"./knowledge/other/wiki/x.md", 7}, {"./AGENT.md", 3},
	})
	if got, err := archivePackSizes(named, ships); err != nil || !reflect.DeepEqual(got, map[string]int64{"runbooks": 15}) {
		t.Errorf("sizes = %v, %v", got, err)
	}
	// Nothing to size: the archive is not read at all.
	if got, err := archivePackSizes([]byte("not an archive"), nil); got != nil || err != nil {
		t.Errorf("no shipped pack: %v, %v", got, err)
	}
	if _, err := archivePackSizes([]byte("not an archive"), ships); err == nil || !strings.Contains(err.Error(), "read the archive's entries") {
		t.Errorf("a damaged archive: %v", err)
	}

	// The bounds of the install's extraction hold for this read too: the file
	// count and the total size are accepted to the limit, and refused past it.
	lim := installExtractLimits()
	many := make([]tarFile, lim.MaxFiles)
	for i := range many {
		many[i] = tarFile{fmt.Sprintf("knowledge/runbooks/wiki/f%d.md", i), 1}
	}
	if got, err := archivePackSizes(tarZst(t, many), ships); err != nil || got["runbooks"] != int64(lim.MaxFiles) {
		t.Errorf("%d files: %v, %v", lim.MaxFiles, got["runbooks"], err)
	}
	many = append(many, tarFile{"AGENT.md", 0})
	if _, err := archivePackSizes(tarZst(t, many), ships); err == nil || !strings.Contains(err.Error(), "past the install's bounds") {
		t.Errorf("%d files: %v", len(many), err)
	}
	const part = 25 << 20
	whole := lim.MaxTotalBytes / part
	if lim.MaxTotalBytes%part != 0 {
		t.Fatalf("fixture: the total %d is not a whole number of %d byte files", lim.MaxTotalBytes, part)
	}
	big := make([]tarFile, whole)
	for i := range big {
		big[i] = tarFile{fmt.Sprintf("knowledge/runbooks/sources/part%d.bin", i), part}
	}
	if got, err := archivePackSizes(tarZst(t, big), ships); err != nil || got["runbooks"] != lim.MaxTotalBytes {
		t.Errorf("a total of %d bytes: %v, %v", lim.MaxTotalBytes, got["runbooks"], err)
	}
	big[0].size++
	if _, err := archivePackSizes(tarZst(t, big), ships); err == nil || !strings.Contains(err.Error(), "past the install's bounds") {
		t.Errorf("a total of %d bytes: %v", lim.MaxTotalBytes+1, err)
	}
}

// tarFile is one regular file of a test archive: its name and size, its bytes
// zeros.
type tarFile struct {
	name string
	size int64
}

// tarZst is a tar archive of the files, compressed as a registry builds one.
func tarZst(t *testing.T, files []tarFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	zeros := make([]byte, 1<<20)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: f.size, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		for left := f.size; left > 0; {
			n := min(left, int64(len(zeros)))
			if _, err := tw.Write(zeros[:n]); err != nil {
				t.Fatal(err)
			}
			left -= n
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
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
	want := "knowledge.ships names runbooks, and publish.include does not put knowledge/runbooks/manifest.json in the archive (V11): add knowledge/** to publish.include, or the pack to knowledge/runbooks/"
	if err := shippedPacksIncluded(m, manifest.ResolveInclude([]string{"agent.yaml", "AGENT.md"}, files)); err.Error() != want {
		t.Fatalf("err = %q, want %q", err, want)
	}
}

func TestByteSize(t *testing.T) {
	for n, want := range map[int64]string{216: "216 bytes", 48_000: "46.9 KiB", 4_200_000: "4.0 MiB", 16 << 20: "16.0 MiB",
		0: "0 bytes", 1023: "1023 bytes", 1024: "1.0 KiB", 1536: "1.5 KiB", 1 << 20: "1.0 MiB", 3 << 19: "1.5 MiB"} {
		if got := byteSize(n); got != want {
			t.Errorf("byteSize(%d) = %q, want %q", n, got, want)
		}
	}
}
