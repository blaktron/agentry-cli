package writer

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry/pkg/judge"
	"github.com/blaktron/agentry/pkg/manifest"
)

// guideDir is the style guide in this package's tree, the directory
// the embed reads.
var guideDir = filepath.Join(manifest.KnowledgeDir, GuideName)

// update rewrites the guide's generated files, as the repo's golden tests do
// (-update).
var update = flag.Bool("update", false, "rewrite the style guide's generated files")

// updateGuide is the command that rewrites the guide's generated files.
const updateGuide = "go test ./pkg/voice/writer -run TestTheStyleGuideIsCurrent -update"

// guideManifest is the manifest.json the guide's tree makes.
func guideManifest(t *testing.T, dir string) []byte {
	t.Helper()
	d, err := knowledge.TreeDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	pages, err := knowledge.Pages(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(knowledge.Manifest{Schema: knowledge.Schema, Name: GuideName, Pages: pages, Bytes: d.Bytes, SHA256: d.SHA256}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// neverPage is the guide's page generated from AI Review's voice questions.
const neverPage = "wiki/review/what-a-voice-never-says.md"

// neverPageText is that page: each question AI Review asks of a voice
// before a person's account uses it, as the core asks it.
func neverPageText() string {
	var b strings.Builder
	b.WriteString("---\ntitle: What a voice never says\ntype: guide\nsummary: The questions AI Review asks of a Writing Voice before a person's account uses it, as the registry asks them; a voice that answers yes to any is refused. Generated from the registry's question set.\nsources: [web:https://docs.secureagentry.net/voice/]\nupdated: 2026-10-19\n---\n\n")
	b.WriteString("# What a voice never says\n\n")
	b.WriteString("A voice kept on this computer is not reviewed, but one sent to the person's Agentry account is read by AI Review before it is used there, and refused when it answers yes to any of the questions below. A voice is about how to write, and nothing else, so a good one answers no to each [src: web:https://docs.secureagentry.net/voice/].\n\n")
	for _, q := range judge.VoiceQuestions() {
		fmt.Fprintf(&b, "## %s\n\n> %s\n\n", q.ID, q.Text)
	}
	b.WriteString("This page is generated from the questions themselves, so it says exactly what AI Review asks.\n")
	return b.String()
}

// TestTheStyleGuideIsCurrent holds the guide's review page to AI Review's
// voice questions, and its index and manifest.json to its pages. With
// -update it writes them instead.
func TestTheStyleGuideIsCurrent(t *testing.T) {
	check := func(rel string, want []byte) {
		t.Helper()
		p := filepath.Join(guideDir, filepath.FromSlash(rel))
		if *update {
			if err := os.WriteFile(p, want, 0o644); err != nil { //nolint:gosec // a page of the guide
				t.Fatal(err)
			}
			return
		}
		got, err := os.ReadFile(p) //nolint:gosec // inside the package
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s is stale or missing; run: %s", rel, updateGuide)
		}
	}
	check(neverPage, []byte(neverPageText()))
	index, err := knowledge.GenerateIndex(guideDir, GuideName)
	if err != nil {
		t.Fatal(err)
	}
	for rel, b := range index {
		check(rel, b)
	}
	check(knowledge.ManifestFile, guideManifest(t, guideDir))
}

// TestTheStyleGuideLoadsFromTheEmbeddedTree: the bytes the CLI embeds
// load as a pack under its name, within the cap a shipped pack is held to,
// and pass the pack lint (frontmatter, links, citations, the index).
func TestTheStyleGuideLoadsFromTheEmbeddedTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, FS()); err != nil {
		t.Fatal(err)
	}
	packDir := filepath.Join(dir, guideDir)
	p, err := knowledge.Load(packDir)
	if err != nil {
		t.Fatalf("the embedded guide does not load: %v (run: %s)", err, updateGuide)
	}
	if p.Manifest.Name != GuideName || p.Manifest.Pages < 3 {
		t.Fatalf("manifest %+v", p.Manifest)
	}
	raw, err := os.ReadFile(filepath.Join(packDir, knowledge.ManifestFile)) //nolint:gosec // the test's own copy
	if err != nil {
		t.Fatal(err)
	}
	if size := p.Manifest.Bytes + int64(len(raw)); size > knowledge.MaxBytes {
		t.Fatalf("the guide is %d bytes; a shipped pack is at most %d", size, knowledge.MaxBytes)
	}
	findings, err := knowledge.Lint(packDir, GuideName)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("lint: %s", f)
	}
}

// TestTheVoiceWriterShipsTheGuide: the manifest declares the pack, publish takes
// every embedded file, and it is the package's only shipped pack.
func TestTheVoiceWriterShipsTheGuide(t *testing.T) {
	raw, err := fs.ReadFile(FS(), "agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.ShippedPacks(); len(got) != 1 || got[0] != GuideName {
		t.Fatalf("ships %v, want [%s]", got, GuideName)
	}
	var files []manifest.FileInfo
	if err := fs.WalkDir(FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files = append(files, manifest.FileInfo{Path: p})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	included := map[string]bool{}
	for _, f := range manifest.ResolveInclude(m.Publish.Include, files) {
		included[f.Path] = true
	}
	for _, f := range files {
		if !included[f.Path] {
			t.Errorf("%s is embedded but publish.include leaves it out", f.Path)
		}
	}
}

// guidePages are the guide's pages as a page names another: section/slug.md.
func guidePages(t *testing.T) []string {
	t.Helper()
	var out []string
	root := filepath.Join(guideDir, "wiki")
	if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err == nil && strings.Contains(rel, string(filepath.Separator)) {
			out = append(out, filepath.ToSlash(rel))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// TestTheInstructionNamesTheGuide: INSTRUCTION.md names the pack and its
// start page, and every guide page it names is a page of the guide.
func TestTheInstructionNamesTheGuide(t *testing.T) {
	raw, err := fs.ReadFile(FS(), "INSTRUCTION.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "`"+GuideName+"`") || !strings.Contains(text, "`guide/start-here.md`") {
		t.Fatal("the instruction does not name the guide and its start page")
	}
	pages := guidePages(t)
	page := regexp.MustCompile("`((?:guide|voice|review)/[a-z0-9-]+\\.md)`")
	for _, m := range page.FindAllStringSubmatch(text, -1) {
		if !slices.Contains(pages, m[1]) {
			t.Errorf("the instruction names %s, which is no page of the guide", m[1])
		}
	}
}

// TestEveryPageIsOnTheMap: the start page links every other page.
func TestEveryPageIsOnTheMap(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(guideDir, "wiki", "guide", "start-here.md")) //nolint:gosec // inside the package
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range guidePages(t) {
		if p != "guide/start-here.md" && !strings.Contains(string(raw), "../"+p+")") {
			t.Errorf("guide/start-here.md does not link %s", p)
		}
	}
}
