package examiner

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
	"github.com/blaktron/agentry/pkg/manifest"
	"github.com/blaktron/agentry/pkg/toolgrant"
)

// guideDir is the review guide in this package's tree, the directory the
// embed reads.
var guideDir = filepath.Join(manifest.KnowledgeDir, GuideName)

// update rewrites the guide's generated files, as the repo's golden tests do
// (-update).
var update = flag.Bool("update", false, "rewrite the review guide's generated files")

// updateGuide is the command that rewrites the guide's generated files.
const updateGuide = "go test ./pkg/examiner -run TestTheReviewGuideIsCurrent -update"

// runnerTools is sources/runner-tools.md: every runner tool a contract line
// grants, with the line, and the tools no line grants, read from the
// registry's own table (pkg/toolgrant), so the guide cannot drift from what
// the registry's scripts stage reads.
func runnerTools() []byte {
	var b bytes.Buffer
	b.WriteString("# Runner tools\n\n")
	b.WriteString("Generated from the registry's own table of the runner's tools by the Agentry CLI's tests; do not edit by hand.\n\n")
	b.WriteString("## The tools a contract line grants\n\n")
	b.WriteString("An instruction that calls for one of these needs its line in the manifest; without it a run does not have the tool, and the registry reports the line as missing.\n\n")
	for _, g := range toolgrant.Grants() {
		tools := toolgrant.ToolsOf(g)
		if len(tools) == 0 {
			continue
		}
		fmt.Fprintf(&b, "- `%s`: %s.\n", g.Path(), strings.Join(codeSpans(tools), ", "))
	}
	b.WriteString("- `capabilities.mcpServers`: a declared server's own tools, named `<server>__<tool>`.\n")
	b.WriteString("- `capabilities.browser.secrets`: a browser that may type secrets has every browser tool but `browser_evaluate`.\n\n")
	b.WriteString("## The tools no line grants\n\n")
	b.WriteString("The runner gives these without a contract line: the inputs, the report, the scratch folder and its search, and `export_pdf` to every run; the knowledge and voice tools to a run a person attaches a pack or a voice to; the rest to the application's own agents. An instruction that calls for one is never a contract breach.\n\n")
	fmt.Fprintf(&b, "- %s.\n", strings.Join(codeSpans(toolgrant.NotContract()), ", "))
	return b.Bytes()
}

func codeSpans(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = "`" + s + "`"
	}
	return out
}

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

// TestTheReviewGuideIsCurrent holds the guide's generated files to what its
// pages and the code make: the runner tools, the index, and manifest.json,
// in that order, since each feeds the next. With -update it writes them
// instead.
func TestTheReviewGuideIsCurrent(t *testing.T) {
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
	check(knowledge.SourcesDir+"/runner-tools.md", runnerTools())
	index, err := knowledge.GenerateIndex(guideDir, GuideName)
	if err != nil {
		t.Fatal(err)
	}
	for rel, b := range index {
		check(rel, b)
	}
	check(knowledge.ManifestFile, guideManifest(t, guideDir))
}

// TestTheReviewGuideLoadsFromTheEmbeddedTree: the bytes the CLI embeds load
// as a pack under its name, within the cap a shipped pack is held to, and
// pass the pack lint (frontmatter, links, citations, the index).
func TestTheReviewGuideLoadsFromTheEmbeddedTree(t *testing.T) {
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

// TestTheExaminerShipsTheGuide: the manifest declares the pack, publish
// takes every embedded file, and it is the package's only shipped pack.
func TestTheExaminerShipsTheGuide(t *testing.T) {
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
// start page, and every page it names is a page of the guide.
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
	page := regexp.MustCompile("(?:^|[\\s`(])([a-z0-9-]+/[a-z0-9-]+\\.md)\\b")
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
