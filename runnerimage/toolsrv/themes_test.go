package toolsrv

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestTheBuiltinThemes (agentry-cli#731, plan D13): the five themes ship,
// each parses as the template reads it and is named by its file, and each
// has what the plan gives it: Formal a cover and "n of N", Brief a title
// block and no cover, Technical banded tables, Letter a letterhead and its
// lines kept. Plain stays as agentry-cli#730 shipped it.
func TestTheBuiltinThemes(t *testing.T) {
	if got := BuiltinThemes(); !slices.Equal(got, []string{"plain", "formal", "brief", "technical", "letter"}) {
		t.Fatalf("built-in themes = %q", got)
	}
	themes := map[string]*Theme{}
	for _, name := range BuiltinThemes() {
		doc, err := BuiltinTheme(name)
		if err != nil {
			t.Fatal(err)
		}
		th, err := ParseTheme(doc)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if th.Name != name {
			t.Errorf("%s.json names itself %q", name, th.Name)
		}
		if _, err := StampTheme(doc); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		themes[name] = th
	}
	if p := themes["plain"]; p.Cover || p.Title != nil || p.LineBreaks != "" {
		t.Errorf("plain gained an optional field: %+v", p)
	}
	if f := themes["formal"]; !f.Cover || f.Footer.PageNumbers != "n of N" || f.Fonts.Body != "Noto Serif" {
		t.Errorf("formal is not serif with a cover and n of N: %+v", f)
	}
	if b := themes["brief"]; b.Cover || b.Title == nil || !b.Title.Rule {
		t.Errorf("brief is not a memo with a ruled heading and no cover: %+v", b)
	}
	if tc := themes["technical"]; !tc.Tables.Banded || tc.Sizes.BodyPt >= themes["plain"].Sizes.BodyPt {
		t.Errorf("technical is not denser than plain with banded tables: %+v", tc)
	}
	if l := themes["letter"]; l.Title == nil || l.LineBreaks != "hard" || l.Cover {
		t.Errorf("letter has no letterhead or does not keep its lines: %+v", l)
	}
	if IsBuiltinTheme("Formal") || IsBuiltinTheme("") || IsBuiltinTheme("../plain") {
		t.Error("a name that is not a built-in's was taken for one")
	}
}

func TestParseThemeOptionalFields(t *testing.T) {
	letter, err := BuiltinTheme("letter")
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string][2]string{
		"title align": {`"align": "center",
    "rule"`, `"align": "justify",
    "rule"`},
		"line breaks":   {`"lineBreaks": "hard"`, `"lineBreaks": "raw"`},
		"unknown field": {`"lineBreaks": "hard"`, `"lineBreaks": "hard", "logo": "/etc/passwd"`},
		"cover type":    {`"lineBreaks": "hard"`, `"lineBreaks": "hard", "cover": "yes"`},
	} {
		doc := bytes.Replace(letter, []byte(change[0]), []byte(change[1]), 1)
		if bytes.Equal(doc, letter) {
			t.Fatalf("%s: the change did not apply", name)
		}
		if _, err := ParseTheme(doc); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestHardLineBreaks(t *testing.T) {
	for name, c := range map[string][2]string{
		"an address block": {"Acme\n14 Harbour Road\nRotterdam\n\nDear Dana,", "Acme\\\n14 Harbour Road\\\nRotterdam\n\nDear Dana,"},
		"already broken":   {"a\\\nb  \nc", "a\\\nb  \nc"},
		"a list":           {"- one\n- two\n  more", "- one\n- two\n  more"},
		"a fence":          {"```\nx\ny\n```\nafter\nlast", "```\nx\ny\n```\nafter\\\nlast"},
		"a table":          {"| a | b |\n|---|---|\n| 1 | 2 |", "| a | b |\n|---|---|\n| 1 | 2 |"},
		"a setext heading": {"one\ntwo\n===\nbody", "one\ntwo\n===\nbody"},
		"a heading":        {"# Title\nbody\nmore", "# Title\nbody\\\nmore"},
		"a quote":          {"> a\n> b", "> a\n> b"},
		"html":             {"<div>\nx\n</div>", "<div>\nx\n</div>"},
		"an html block":    {"<table>\n<tr><td>a</td></tr>\nshort\nline\n\nafter\nmore", "<table>\n<tr><td>a</td></tr>\nshort\nline\n\nafter\\\nmore"},
		"a reference":      {"![logo][l]\n[l]: logo.png\n[m]: <x y.png>\nend", "![logo][l]\n[l]: logo.png\n[m]: <x y.png>\nend"},
		"before a fence":   {"text\n```\ncode\n```", "text\n```\ncode\n```"},
		"wrapped prose": {"This report covers the third quarter. It is written to be read once, start to\nfinish, and then kept.",
			"This report covers the third quarter. It is written to be read once, start to\nfinish, and then kept."},
		"a signature after prose": {"Kind regards,\nThe review team\nAcme", "Kind regards,\\\nThe review team\\\nAcme"},
	} {
		if got := hardLineBreaks(c[0]); got != c[1] {
			t.Errorf("%s:\n%q\nwant\n%q", name, got, c[1])
		}
	}
}

// TestExportPDFKeepsALettersLines: under Letter the render gets its lines
// kept, under Plain the Markdown as written; either way the agent's own file
// is untouched, and the render is told the day for a cover page.
func TestExportPDFKeepsALettersLines(t *testing.T) {
	for _, c := range []struct {
		theme string
		kept  bool
	}{{"letter", true}, {"plain", false}} {
		doc, err := BuiltinTheme(c.theme)
		if err != nil {
			t.Fatal(err)
		}
		stamp, err := StampTheme(doc)
		if err != nil {
			t.Fatal(err)
		}
		spec := pdfSpec(t)
		spec.PDF.Theme = stamp
		s := pdfServer(t, spec, fakeTypst(t, fakePDF+`; cat "$ROOT/report.md" "$ROOT/meta.json"`))
		src := "# Letter\n\nAcme\n14 Harbour Road\n\nDear Dana,\n"
		mustWrite(t, filepath.Join(spec.OutDir, "reports", "l.md"), []byte(src))
		if got, isErr := exportPDF(t, s, map[string]any{"source": "l.md"}); isErr {
			t.Fatalf("%s: export = %q", c.theme, got)
		}
		pdf := readReport(t, spec, "l.pdf")
		if kept := bytes.Contains(pdf, []byte("Acme\\\n14 Harbour Road")); kept != c.kept {
			t.Errorf("%s: lines kept = %v, want %v:\n%s", c.theme, kept, c.kept, pdf)
		}
		if want := `"date":"` + time.Now().UTC().Format(time.DateOnly) + `"`; !bytes.Contains(pdf, []byte(want)) {
			t.Errorf("%s: meta.json lacks %s:\n%s", c.theme, want, pdf)
		}
		if got := readReport(t, spec, "l.md"); string(got) != src {
			t.Errorf("%s: the agent's file changed:\n%s", c.theme, got)
		}
	}
}

// TestRealRenderEveryTheme (agentry-cli#731's acceptance): each built-in
// theme renders the sample, every script in it, as a tagged PDF; and drawn
// as images, no page puts ink in its left or right margin, so a long line,
// a wide table or a long token stays inside the page's text.
func TestRealRenderEveryTheme(t *testing.T) {
	typst := realTypst(t)
	sample, err := os.ReadFile(filepath.Join("testdata", "pdf", "sample.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range BuiltinThemes() {
		t.Run(name, func(t *testing.T) {
			doc, err := BuiltinTheme(name)
			if err != nil {
				t.Fatal(err)
			}
			theme, err := ParseTheme(doc)
			if err != nil {
				t.Fatal(err)
			}
			stamp, err := StampTheme(doc)
			if err != nil {
				t.Fatal(err)
			}
			spec := pdfSpec(t)
			spec.PDF.Author = "acme/demo@1.2.3"
			spec.PDF.Theme = stamp
			s := pdfServer(t, spec, typst)
			mustWrite(t, filepath.Join(spec.OutDir, "reports", "r.md"), sample)
			got, isErr := exportPDF(t, s, map[string]any{"source": "r.md"})
			if isErr || !strings.Contains(got, name+" theme") {
				t.Fatalf("export = (%q, %v)", got, isErr)
			}
			pdf := readReport(t, spec, "r.pdf")
			for _, want := range []string{"/StructTreeRoot", "pdfuaid:part", "NotoSansSC", "NotoSansArabic", "NotoSansDevanagari", "NotoSansThai", "NotoSansHebrew"} {
				if !bytes.Contains(pdf, []byte(want)) {
					t.Errorf("the PDF lacks %s", want)
				}
			}
			pages := renderPNGs(t, typst, theme, doc, string(sample))
			if theme.Cover && len(pages) < 2 {
				t.Errorf("a cover theme drew %d pages", len(pages))
			}
			margin := theme.Page.MarginMM / 25.4 * pngPPI
			for i, page := range pages {
				if x, y, ok := inkOutside(page, int(margin)-2); ok {
					t.Errorf("page %d has ink in its side margin at (%d, %d); margin %.0f px of %d", i+1, x, y, margin, page.Bounds().Dx())
				}
			}
		})
	}
}

// pngPPI is the resolution the margin check draws at.
const pngPPI = 48

// renderPNGs draws src under theme as the export would, one PNG a page.
func renderPNGs(t *testing.T, typst typstFiles, theme *Theme, themeDoc []byte, src string) []image.Image {
	t.Helper()
	root, out := t.TempDir(), t.TempDir()
	template, err := pdfFiles.ReadFile("pdf/report.typ")
	if err != nil {
		t.Fatal(err)
	}
	doc := reportMarkdown(src, theme)
	meta, err := json.Marshal(pdfMeta{Title: doc.title, Author: "acme/demo@1.2.3", Images: map[string]pdfImage{}, Date: "2026-10-08"})
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{"main.typ": template, "report.md": []byte(doc.text), "theme.json": themeDoc, "meta.json": meta} {
		mustWrite(t, filepath.Join(root, name), b)
	}
	cmd := exec.Command(typst.bin, "compile", "--root", root, "--ignore-system-fonts", "--font-path", typst.fonts,
		"--package-path", typst.packages, "--package-cache-path", filepath.Join(out, ".packages"),
		"--format", "png", "--ppi", fmt.Sprint(pngPPI), filepath.Join(root, "main.typ"), filepath.Join(out, "page-{0p}.png"))
	cmd.Env = []string{}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("typst png: %v\n%s", err, b)
	}
	names, err := filepath.Glob(filepath.Join(out, "page-*.png"))
	if err != nil || len(names) == 0 {
		t.Fatalf("no pages drawn (%v)", err)
	}
	slices.Sort(names)
	var pages []image.Image
	for _, n := range names {
		f, err := os.Open(n) //nolint:gosec // the test's own output
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(f)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, img)
	}
	return pages
}

// inkOutside finds a pixel that is not the page's white within w pixels of
// the left or right edge.
func inkOutside(img image.Image, w int) (int, int, bool) {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for _, xs := range [][2]int{{b.Min.X, b.Min.X + w}, {b.Max.X - w, b.Max.X}} {
			for x := xs[0]; x < xs[1]; x++ {
				r, g, bl, _ := img.At(x, y).RGBA()
				if r < 0xf000 || g < 0xf000 || bl < 0xf000 {
					return x, y, true
				}
			}
		}
	}
	return 0, 0, false
}
