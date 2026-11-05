package reportdesigner

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
	"github.com/blaktron/agentry/pkg/reportdesign"
)

// guideDir is the design guide in this package's tree, the directory the
// embed reads.
var guideDir = filepath.Join(manifest.KnowledgeDir, GuideName)

// update rewrites the guide's generated files, as the repo's golden tests do
// (-update).
var update = flag.Bool("update", false, "rewrite the design guide's generated files")

// updateGuide is the command that rewrites the guide's generated files.
const updateGuide = "go test ./pkg/reportdesigner -run TestTheDesignGuideIsCurrent -update"

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

// listsPage is the guide's page generated from the design package's closed
// lists.
const listsPage = "wiki/design/the-lists.md"

// listsPageText is that page: every name a design's fields accept, as the
// registry's own package lists them.
func listsPageText() string {
	var b strings.Builder
	b.WriteString("---\ntitle: The closed lists\ntype: reference\nsummary: Every name a report design's fields accept - the faces by role, the cover styles and the colours each sets text on, the header and footer styles, the title blocks, the heading styles and scales, the margins, the cover facts and the tokens - generated from the design package itself.\nsources: [web:https://docs.secureagentry.net/runner/]\nupdated: 2026-11-05\n---\n\n")
	b.WriteString("# The closed lists\n\n")
	b.WriteString("A design's fields take names from these lists and nothing else; a name not here is refused. This page is generated from the rules every design is held to, so it is exactly what they accept [src: web:https://docs.secureagentry.net/runner/].\n\n")
	b.WriteString("## Faces\n\n")
	for _, role := range []string{"headings", "body", "code"} {
		fmt.Fprintf(&b, "- **%s:** %s\n", role, strings.Join(reportdesign.FontsFor(role), ", "))
	}
	b.WriteString("\n## Cover styles\n\n| Style | Carries text in |\n| --- | --- |\n")
	for _, s := range reportdesign.CoverStyles() {
		carries := "no colour"
		switch {
		case s.AccentOnText:
			carries = "the accent"
		case s.SecondaryOnText:
			carries = "the secondary (the accent when there is none)"
		}
		fmt.Fprintf(&b, "| `%s` | %s |\n", s.Name, carries)
	}
	b.WriteString("\n## The other fields\n\n")
	for _, l := range []struct {
		field string
		names []string
	}{
		{"paper", reportdesign.Papers},
		{"titleBlock", reportdesign.TitleBlocks},
		{"headings", reportdesign.HeadingStyles},
		{"headingScale", reportdesign.HeadingScales},
		{"margins", reportdesign.MarginPresets},
		{"quotes", reportdesign.QuoteStyles},
		{"header.style", reportdesign.HeaderStyles},
		{"footer.pageNumbers", reportdesign.PageNumberStyles},
		{"cover.shows", reportdesign.CoverFacts},
		{"cover.when", reportdesign.CoverWhens},
	} {
		fmt.Fprintf(&b, "- `%s`: %s\n", l.field, "`"+strings.Join(l.names, "`, `")+"`")
	}
	b.WriteString("\n## Tokens\n\nA header or footer slot may hold: `{" + strings.Join(reportdesign.Tokens, "}`, `{") + "}`.\n")
	return b.String()
}

// TestTheDesignGuideIsCurrent holds the guide's lists page to the design
// package, and its index and manifest.json to its pages. With -update it
// writes them instead.
func TestTheDesignGuideIsCurrent(t *testing.T) {
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
	check(listsPage, []byte(listsPageText()))
	index, err := knowledge.GenerateIndex(guideDir, GuideName)
	if err != nil {
		t.Fatal(err)
	}
	for rel, b := range index {
		check(rel, b)
	}
	check(knowledge.ManifestFile, guideManifest(t, guideDir))
}

// TestTheDesignGuideLoadsFromTheEmbeddedTree: the bytes the CLI embeds
// load as a pack under its name, within the cap a shipped pack is held to,
// and pass the pack lint.
func TestTheDesignGuideLoadsFromTheEmbeddedTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, FS()); err != nil {
		t.Fatal(err)
	}
	packDir := filepath.Join(dir, guideDir)
	p, err := knowledge.Load(packDir)
	if err != nil {
		t.Fatalf("the embedded guide does not load: %v (run: %s)", err, updateGuide)
	}
	if p.Manifest.Name != GuideName || p.Manifest.Pages < 8 {
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

// TestTheReportDesignerShipsTheGuide: the manifest declares the pack,
// publish takes every embedded file, and it is the package's only pack.
func TestTheReportDesignerShipsTheGuide(t *testing.T) {
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
	page := regexp.MustCompile("`((?:guide|design|cover|review)/[a-z0-9-]+\\.md)`")
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

// TestTheGuidesDocumentIsAValidDesign: the whole document the guide gives
// passes the rules it describes.
func TestTheGuidesDocumentIsAValidDesign(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(guideDir, "wiki", "design", "the-document.md")) //nolint:gosec // inside the package
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(raw), "```json\n")
	doc, _, ok2 := strings.Cut(rest, "```")
	if !ok || !ok2 {
		t.Fatal("the page has no json block")
	}
	if _, err := reportdesign.Parse([]byte(doc)); err != nil {
		t.Fatalf("the guide's document does not pass: %v", err)
	}
}
