package ship

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
	pkgarchive "github.com/blaktron/agentry/pkg/archive"
	"github.com/blaktron/agentry/pkg/manifest"
)

const agentYAML = `schemaVersion: "2"
name: demo
namespace: acme
modes: [autonomous]
version: 1.0.0
summary: "a test package"
license: Apache-2.0
entrypoints:
  instruction: AGENT.md
publish:
  include: [agent.yaml, AGENT.md]
`

// packageDir is a QuickAgent package with no pack.
func packageDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"agent.yaml": agentYAML, "AGENT.md": "# Demo\n\nDo the thing.\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// registryIssues are the problems the registry's own rules find in the
// package at dir once agent.yaml is edited as `agentry knowledge ship` does:
// the manifest rules over the files the include set puts in the archive.
func registryIssues(t *testing.T, dir string) []error {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	walked, err := pkgarchive.WalkWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []error
	for _, issue := range manifest.ValidateInContext(m, manifest.Context{
		Namespace: m.Namespace, Name: m.Name, Files: walked,
		ReadFile: func(p string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, filepath.FromSlash(p))) },
	}) {
		if !manifest.IsWarning(issue) {
			out = append(out, issue)
		}
	}
	return out
}

func shipInto(t *testing.T, dir string, p *knowledge.Pack) Laid {
	t.Helper()
	laid, err := Lay(dir, p)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	e, err := AddShip(raw, p.Manifest.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), e.YAML, 0o644); err != nil {
		t.Fatal(err)
	}
	return laid
}

func TestALaidPackPassesTheRegistrysRules(t *testing.T) {
	dir := packageDir(t)
	p := knowledgetest.Make(t, "runbooks", nil)
	laid := shipInto(t, dir, p)
	if issues := registryIssues(t, dir); len(issues) > 0 {
		t.Fatalf("the registry would refuse the package: %v", issues)
	}
	if laid.Name != "runbooks" || laid.Pages != 1 || laid.Files != 4 || laid.SHA256 != p.Manifest.SHA256 {
		t.Fatalf("laid %+v", laid)
	}
	// The manifest.json is the registry's to hold to the tree, and carries
	// neither the local pack's id nor a scan record.
	raw, err := os.ReadFile(filepath.Join(Dir(dir, "runbooks"), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{p.Manifest.ID, `"id"`, `"creator"`} {
		if leaked != "" && strings.Contains(string(raw), leaked) {
			t.Errorf("the shipped manifest.json carries %s: %s", leaked, raw)
		}
	}
	if !strings.Contains(string(raw), `"scan": "not scanned"`) {
		t.Errorf("the shipped manifest.json claims a scan: %s", raw)
	}
	if bytes, files, err := DirSize(dir, "runbooks"); err != nil || files != 5 || bytes != laid.Bytes {
		t.Errorf("DirSize = %d bytes, %d files, %v; laid %d bytes", bytes, files, err, laid.Bytes)
	}
}

func TestLayingAgainReplacesThePack(t *testing.T) {
	dir := packageDir(t)
	shipInto(t, dir, knowledgetest.Make(t, "runbooks", nil))
	stale := filepath.Join(Dir(dir, "runbooks"), "sources", "stale.txt")
	if err := os.WriteFile(stale, []byte("left over"), 0o644); err != nil {
		t.Fatal(err)
	}
	p2 := knowledgetest.Make(t, "runbooks", map[string]string{
		"wiki/topics/other.md": "---\ntitle: Other\ntype: topic\nsummary: Another\nsources: [doc:b.txt]\nupdated: 2026-09-25\n---\n\nText [src: doc:b.txt].\n",
		"sources/b.txt":        "b\n",
	})
	laid, err := Lay(dir, p2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a file of the earlier pack stayed: %v", err)
	}
	if laid.SHA256 != p2.Manifest.SHA256 {
		t.Errorf("laid %s, want %s", laid.SHA256, p2.Manifest.SHA256)
	}
	if issues := registryIssues(t, dir); len(issues) > 0 {
		t.Fatalf("the registry would refuse the package: %v", issues)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "knowledge"))
	if len(entries) != 1 {
		t.Errorf("knowledge/ holds %v, want the pack alone", entries)
	}
}

// bigPack makes a pack by hand: files of the given sizes under sources/.
func bigPack(t *testing.T, sizes ...int) *knowledge.Pack {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "big")
	for rel, body := range map[string]string{"wiki/index.md": "# Index\n", "wiki/log.md": "# Log\n"} {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "sources"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, n := range sizes {
		if err := os.WriteFile(filepath.Join(dir, "sources", strings.Repeat("a", 1)+string(rune('a'+i%26))+strings.Repeat("x", i/26)+".bin"), make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	d, err := knowledge.TreeDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	pages, _ := knowledge.Pages(dir)
	m := knowledge.Manifest{Schema: knowledge.Schema, Name: "big", Pages: pages, Bytes: d.Bytes, SHA256: d.SHA256}
	raw, err := shippedManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := knowledge.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAPackPastWhatAReleaseShipsIsRefusedAndNothingIsWritten(t *testing.T) {
	cases := map[string]*knowledge.Pack{
		"past 10 MB":       bigPack(t, 10<<20, 1),
		"past 2,000 files": bigPack(t, slices.Repeat([]int{1}, MaxPackFiles+1)...),
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			dir := packageDir(t)
			for _, err := range []error{Check(p), func() error { _, err := Lay(dir, p); return err }()} {
				if !errors.Is(err, ErrPastCap) {
					t.Fatalf("err = %v, want ErrPastCap", err)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "knowledge")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused pack left knowledge/ behind: %v", err)
			}
		})
	}
	// The caps are inclusive: 2,000 files and a pack just under 10 MB ship.
	if err := Check(bigPack(t, slices.Repeat([]int{1}, MaxPackFiles-2)...)); err != nil {
		t.Errorf("a pack of 2,000 files: %v", err)
	}
	if err := Check(bigPack(t, 10<<20-2048)); err != nil {
		t.Errorf("a pack just under 10 MB: %v", err)
	}
}

func TestARefusedLayLeavesTheEarlierPack(t *testing.T) {
	dir := packageDir(t)
	p := knowledgetest.Make(t, "big", nil)
	shipInto(t, dir, p)
	before, _ := os.ReadFile(filepath.Join(Dir(dir, "big"), "manifest.json"))
	if _, err := Lay(dir, bigPack(t, 11<<20)); !errors.Is(err, ErrPastCap) {
		t.Fatalf("err = %v", err)
	}
	after, err := os.ReadFile(filepath.Join(Dir(dir, "big"), "manifest.json"))
	if err != nil || string(before) != string(after) {
		t.Errorf("the earlier pack changed: %v", err)
	}
}

func TestRemoveTakesThePackAndItsFolderOut(t *testing.T) {
	dir := packageDir(t)
	shipInto(t, dir, knowledgetest.Make(t, "runbooks", nil))
	if err := Remove(dir, "runbooks"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "knowledge")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("knowledge/ stayed: %v", err)
	}
	if err := Remove(dir, "runbooks"); err != nil {
		t.Errorf("removing a pack that is not there: %v", err)
	}
	if err := Remove(dir, "../x"); err == nil {
		t.Error("a path was taken for a pack name")
	}
}

func TestCoversTheArchive(t *testing.T) {
	for _, tc := range []struct {
		include []string
		want    bool
	}{
		{[]string{"agent.yaml"}, false},
		{[]string{"agent.yaml", "knowledge/**"}, true},
		{[]string{"**"}, true},
		{[]string{"knowledge/runbooks/**"}, true},
		{[]string{"knowledge/runbooks/manifest.json"}, false},
		{[]string{"knowledge/other/**"}, false},
	} {
		if got := Covers(tc.include, "runbooks"); got != tc.want {
			t.Errorf("Covers(%v) = %v, want %v", tc.include, got, tc.want)
		}
	}
}
