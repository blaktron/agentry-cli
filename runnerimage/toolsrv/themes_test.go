package toolsrv

// The report design and the forms. The tests named TestRealRender… run the real
// renderer from the pinned image, as the typst-image CI job does.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/blaktron/agentry-cli/runnerimage/reportdesign"
)

// TestTheBuiltinThemes: the five themes are pkg/reportdesign's,
// Plain first, and each stamps as its canonical document, byte for byte.
func TestTheBuiltinThemes(t *testing.T) {
	if got := BuiltinThemes(); !slices.Equal(got, []string{"plain", "classic", "modern", "editorial", "technical"}) {
		t.Fatalf("built-in themes = %q", got)
	}
	for _, name := range BuiltinThemes() {
		doc, err := BuiltinTheme(name)
		if err != nil {
			t.Fatal(err)
		}
		st, err := StampTheme(doc)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if st.Document != string(doc) || st.BasedOn != name || st.Version != "1" {
			t.Errorf("%s stamps as %+v, not its own document", name, st)
		}
		d, _ := reportdesign.Builtin(name)
		if st.SHA256 != d.SHA256() {
			t.Errorf("%s: the stamp's SHA-256 %s is not the design's %s", name, st.SHA256, d.SHA256())
		}
	}
	for _, bad := range []string{"Plain", "", "../plain", "formal", "letter", "brief"} {
		if IsBuiltinTheme(bad) {
			t.Errorf("%q was taken for a built-in theme", bad)
		}
		if _, err := BuiltinTheme(bad); err == nil {
			t.Errorf("%q has a built-in document", bad)
		}
	}
}

// TestAHostileDesignNeverReachesTheTemplate: a design is
// held to pkg/reportdesign's rules when it is stamped and again when the
// tools server reads the spec; markup, a path, an unknown key, a version 1
// document and an accent that cannot carry its text are refused; a stamp
// whose document changed, or is not canonical, never serves.
func TestAHostileDesignNeverReachesTheTemplate(t *testing.T) {
	modern, err := BuiltinTheme("modern")
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string][2]string{
		"unknown key":      {`"quotes": "callout",`, `"quotes": "callout", "script": "#read(\"/etc/passwd\")",`},
		"version 1":        {`"agentryTheme": 2`, `"agentryTheme": 1`},
		"a font path":      {`"body": "Inter"`, `"body": "../../etc/passwd"`},
		"a system font":    {`"body": "Inter"`, `"body": "DejaVu Sans"`},
		"a mono body":      {`"body": "Inter"`, `"body": "JetBrains Mono"`},
		"a colour word":    {`"accent": "#1f6feb"`, `"accent": "red"`},
		"a pale accent":    {`"accent": "#1f6feb"`, `"accent": "#7fb3d5"`},
		"a title block":    {`"titleBlock": "band"`, `"titleBlock": "#panic()"`},
		"page numbers":     {`"pageNumbers": "nOfN"`, `"pageNumbers": "n of N"`},
		"a footer newline": {`"text": ""`, `"text": "line\nbreak"`},
		"a footer bidi":    {`"text": ""`, `"text": "a\u202eb"`},
		"paper":            {`"paper": "a4"`, `"paper": "us-letter"`},
	} {
		doc := bytes.Replace(modern, []byte(change[0]), []byte(change[1]), 1)
		if bytes.Equal(doc, modern) {
			t.Fatalf("%s: the change did not apply", name)
		}
		if _, err := StampTheme(doc); err == nil {
			t.Errorf("%s: stamped", name)
		}
	}

	// Any spacing is stamped as the canonical document.
	var compact bytes.Buffer
	if err := json.Compact(&compact, modern); err != nil {
		t.Fatal(err)
	}
	st, err := StampTheme(compact.Bytes())
	if err != nil || st.Document != string(modern) {
		t.Fatalf("a compact design stamped as (%+v, %v), want its canonical form", st, err)
	}

	spec := pdfSpec(t)
	spec.PDF.Theme = st
	if err := validateSpec(spec); err != nil {
		t.Fatalf("a stamped design: %v", err)
	}
	forged := *st
	forged.Document = strings.Replace(forged.Document, `"accent": "#1f6feb"`, `"accent": "#7fb3d5"`, 1)
	spec.PDF.Theme = &forged
	if err := validateSpec(spec); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("a stamp whose document changed = %v", err)
	}
	loose := *st
	loose.Document = compact.String()
	loose.SHA256 = stampSHA(loose.Document)
	spec.PDF.Theme = &loose
	if err := validateSpec(spec); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Errorf("a stamp of a non-canonical document = %v", err)
	}
	renamed := *st
	renamed.Name = "Other"
	spec.PDF.Theme = &renamed
	if err := validateSpec(spec); err == nil {
		t.Error("a stamp naming another design was accepted")
	}
}

// TestExportPDFTakesAFormAndNothingElse: export_pdf's
// arguments are source, path and form; a call naming the look is refused,
// not ignored; the render gets the stamped design byte for byte and the
// form; the ledger records the form.
func TestExportPDFTakesAFormAndNothingElse(t *testing.T) {
	props := exportPDFSchema()["properties"].(map[string]any)
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"form", "path", "source"}) || !slices.Equal(exportPDFArgs, []string{"source", "path", "form"}) {
		t.Fatalf("export_pdf takes %v", keys)
	}
	if enum := props["form"].(map[string]any)["enum"]; !slices.Equal(enum.([]string), []string{"report", "letter", "memo"}) {
		t.Errorf("form's enum = %v", enum)
	}

	doc, err := BuiltinTheme("editorial")
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := StampTheme(doc)
	if err != nil {
		t.Fatal(err)
	}
	spec := pdfSpec(t)
	spec.PDF.Theme = stamp
	s := pdfServer(t, spec, fakeTypst(t, fakePDF+`; cat "$ROOT/design.json" "$ROOT/meta.json"`))
	mustWrite(t, filepath.Join(spec.OutDir, "reports", "r.md"), []byte("# R\n\nTo: Dana\nFrom: Sam\n\nBody.\n"))
	for _, extra := range []map[string]any{{"theme": "modern"}, {"accent": "#ff0000"}, {"design": map[string]any{}}, {"font": "Lora"}, {"logo": "/scratch/logo.png"}} {
		args := map[string]any{"source": "r.md"}
		for k, v := range extra {
			args[k] = v
		}
		got, isErr := exportPDF(t, s, args)
		if !isErr || !strings.Contains(got, "report design") {
			t.Errorf("a call with %v = (%q, %v), want refused", extra, got, isErr)
		}
	}
	if got, isErr := exportPDF(t, s, map[string]any{"source": "r.md", "form": "poster"}); !isErr || !strings.Contains(got, "report, letter, memo") {
		t.Errorf("an unknown form = (%q, %v)", got, isErr)
	}
	got, isErr := exportPDF(t, s, map[string]any{"source": "r.md", "form": "memo"})
	if isErr || !strings.Contains(got, "the memo form, in the Editorial report design") {
		t.Fatalf("export = (%q, %v)", got, isErr)
	}
	pdf := readReport(t, spec, "r.pdf")
	if !bytes.Contains(pdf, doc) {
		t.Errorf("the render did not get the stamped design:\n%s", pdf)
	}
	for _, want := range []string{`"form":"memo"`, `"titleBlock":true`, `"memo":[{"label":"To","value":"Dana"},{"label":"From","value":"Sam"}]`, `"tint":"#f6e5e4"`, `"notoHeadings":"Noto Serif"`, `"notoBody":"Noto Sans"`} {
		if !bytes.Contains(pdf, []byte(want)) {
			t.Errorf("meta.json lacks %s:\n%s", want, pdf)
		}
	}
	b, err := os.ReadFile(spec.PDF.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	var e PDFEntry
	if err := json.Unmarshal(bytes.TrimSpace(b), &e); err != nil || e.Form != FormMemo {
		t.Errorf("ledger entry = %+v (%v)", e, err)
	}
	if got := readReport(t, spec, "r.md"); string(got) != "# R\n\nTo: Dana\nFrom: Sam\n\nBody.\n" {
		t.Errorf("the agent's file changed:\n%s", got)
	}
}

func TestReportMarkdownForms(t *testing.T) {
	for _, c := range []struct {
		name, src, form string
		text            string
		titleBlock      bool
		memo            []memoField
	}{
		{"an opening heading", "# Title\n\nBody.\n", FormReport, "\n\nBody.\n", true, nil},
		{"after blank lines", "\n\n## Title\n\n### Sub\n", FormReport, "\n\n\n\n## Sub\n", true, nil},
		{"a setext title", "Title\n=====\n\nBody.\n", FormReport, "\n\n\nBody.\n", true, nil},
		{"text before it", "Preface\n\n# Title\n", FormReport, "Preface\n\n# Title\n", false, nil},
		{"in a quote", "> # Title\n", FormReport, "> # Title\n", false, nil},
		{"an empty heading", "#\n\nBody.\n", FormReport, "#\n\nBody.\n", false, nil},
		{"a memo", "# Memo\n\n**To:** Dana Okafor\n**From**: Sam\nDate: 9 October 2026\n- Subject: *Next* steps\n\nBody.\n", FormMemo, "\n\n\n\n\n\n\nBody.\n", true,
			[]memoField{{"To", "Dana Okafor"}, {"From", "Sam"}, {"Date", "9 October 2026"}, {"Subject", "Next steps"}}},
		{"a memo with no heading", "To: Dana\nRe: the audit\n\nBody.\n", FormMemo, "\n\n\nBody.\n", false, []memoField{{"To", "Dana"}, {"Re", "the audit"}}},
		{"one header line is not a header", "# M\n\nTo: Dana\n\nBody.\n", FormMemo, "\n\nTo: Dana\n\nBody.\n", true, nil},
		{"a paragraph is not a header", "# M\n\nTo: Dana\nand the team\n", FormMemo, "\n\nTo: Dana\nand the team\n", true, nil},
		{"a report keeps its header lines", "# M\n\nTo: Dana\nFrom: Sam\n", FormReport, "\n\nTo: Dana\nFrom: Sam\n", true, nil},
		{"a letter keeps its lines", "# Acme\n\nAcme\n14 Harbour Road\n\nDear Dana,\n", FormLetter, "\n\nAcme\\\n14 Harbour Road\n\nDear Dana,\n", true, nil},
	} {
		doc := reportMarkdown(c.src, c.form)
		if doc.text != c.text || doc.titleBlock != c.titleBlock || !slices.Equal(doc.memo, c.memo) {
			t.Errorf("%s: (%q, %v, %v), want (%q, %v, %v)", c.name, doc.text, doc.titleBlock, doc.memo, c.text, c.titleBlock, c.memo)
		}
	}
}

// TestRenderMeta: a cover only for a report that opens with its title and
// passes coverWords; the tint is pkg/reportdesign's; Noto Serif behind a
// serif face.
func TestRenderMeta(t *testing.T) {
	long := "# Title\n\n" + strings.Repeat("word ", coverWords)
	short := "# Title\n\n" + strings.Repeat("word ", coverWords-10)
	for _, c := range []struct {
		src, form string
		cover     bool
	}{
		{long, FormReport, true}, {short, FormReport, false}, {long, FormMemo, false}, {long, FormLetter, false},
		{"Preface\n\n" + long, FormReport, false},
	} {
		doc := reportMarkdown(c.src, c.form)
		b, err := renderMeta(reportdesign.Default(), c.form, doc, doc.title, "", map[string]pdfImage{}, nil, fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		var m pdfMeta
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if m.Cover != c.cover || m.Form != c.form || m.Memo == nil {
			t.Errorf("%s, %d words: %+v, want cover %v", c.form, doc.words, m, c.cover)
		}
	}
	for _, d := range reportdesign.Builtins() {
		b, err := renderMeta(d, FormReport, mdDoc{}, "t", "", nil, nil, fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		var m pdfMeta
		_ = json.Unmarshal(b, &m)
		tint, _ := reportdesign.Tint(d.Accent)
		serif := d.BasedOn == "classic"
		if m.Tint != tint || (m.NotoBody == "Noto Serif") != serif || (m.NotoHeadings == "Noto Serif") != (serif || d.BasedOn == "editorial") {
			t.Errorf("%s: %+v", d.BasedOn, m)
		}
	}
}

// TestRealRenderEveryThemeAndForm: each built-in theme renders the sample in
// each form, offline under the render's confinement, every script in it, as a
// tagged PDF; and drawn as images, no page puts ink in its left or right
// margin, so a long line, a wide table, a long token or a band stays inside
// the page's text.
func TestRealRenderEveryThemeAndForm(t *testing.T) {
	typst := realTypst(t)
	sample, err := os.ReadFile(filepath.Join("testdata", "pdf", "sample.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range BuiltinThemes() {
		for _, form := range Forms {
			t.Run(name+"/"+form, func(t *testing.T) {
				doc, err := BuiltinTheme(name)
				if err != nil {
					t.Fatal(err)
				}
				_, pdf := realExportForm(t, name, form, string(sample), nil)
				for _, want := range []string{"/StructTreeRoot", "pdfuaid:part", "NotoSansSC", "NotoSansArabic", "NotoSansDevanagari", "NotoSansThai", "NotoSansHebrew"} {
					if !bytes.Contains(pdf, []byte(want)) {
						t.Errorf("the PDF lacks %s", want)
					}
				}
				d, _ := reportdesign.Builtin(name)
				for _, f := range []string{d.Fonts.Headings, d.Fonts.Body, d.Fonts.Code} {
					if !bytes.Contains(pdf, []byte(strings.ReplaceAll(f, " ", ""))) {
						t.Errorf("the PDF does not use %s", f)
					}
				}
				pages := renderPNGs(t, typst, doc, form, string(sample))
				margin := 22.0
				if form == FormLetter {
					margin = 25
				}
				for i, page := range pages {
					if x, y, ok := inkOutside(page, int(margin/25.4*pngPPI)-2); ok {
						t.Errorf("page %d has ink in its side margin at (%d, %d)", i+1, x, y)
					}
				}
			})
		}
	}
}

// TestRealRenderTextIsNeverTypst: a footer line, a title and
// a memo's header lines that are Typst markup render as text: the render
// succeeds and makes no link.
func TestRealRenderTextIsNeverTypst(t *testing.T) {
	typst := realTypst(t)
	modern, err := BuiltinTheme("modern")
	if err != nil {
		t.Fatal(err)
	}
	doc := bytes.Replace(modern, []byte(`"text": ""`), []byte(`"text": "#panic(\"footer ran\") #link(\"https://footer.example\")[f] #read(\"/etc/passwd\")"`), 1)
	stamp, err := StampTheme(doc)
	if err != nil {
		t.Fatal(err)
	}
	spec := pdfSpec(t)
	spec.PDF.Theme = stamp
	s := pdfServer(t, spec, typst)
	mustWrite(t, filepath.Join(spec.OutDir, "reports", "m.md"), []byte(strings.Join([]string{
		`# #panic("title ran") #link("https://title.example")[t]`,
		"",
		`To: #panic("memo ran")`,
		`From: #link("https://memo.example")[m] $#panic("math")$`,
		"",
		"Body.",
	}, "\n")))
	if got, isErr := exportPDF(t, s, map[string]any{"source": "m.md", "form": "memo"}); isErr {
		t.Fatalf("export = %q", got)
	}
	if got := uris(readReport(t, spec, "m.pdf")); len(got) != 0 {
		t.Errorf("text in the design or the memo ran as Typst: links %q", got)
	}
}

// TestRealRenderACover: a report that opens with its title and passes
// coverWords gets a page before it; a short one, an untitled one and a memo
// do not.
func TestRealRenderACover(t *testing.T) {
	long := strings.Repeat("A sentence of the report's body, long enough to fill. ", 200)
	for _, c := range []struct {
		md, form string
		cover    bool
	}{
		{"# A titled report\n\n" + long, FormReport, true},
		{"# A titled report\n\nOne line.\n", FormReport, false},
		{"Dear Dana,\n\n" + long, FormReport, false},
		{"# A titled memo\n\n" + long, FormMemo, false},
	} {
		_, pdf := realExportForm(t, "classic", c.form, c.md, nil)
		_, without := realExportForm(t, "classic", c.form, strings.Replace(c.md, "# A titled", "A titled", 1), nil)
		if got := pdfPages(pdf) - pdfPages(without); (got == 1) != c.cover {
			t.Errorf("%s %.20q: %d pages, %d without its heading; cover %v", c.form, c.md, pdfPages(pdf), pdfPages(without), c.cover)
		}
	}
}

// TestTheImageCarriesEveryDesignFamily:
// the pinned image lists every family pkg/reportdesign names, and a render
// in each family uses it, under the render's confinement (no network).
func TestTheImageCarriesEveryDesignFamily(t *testing.T) {
	typst := realTypst(t)
	cmd := exec.Command(typst.bin, "fonts", "--ignore-system-fonts", "--font-path", typst.fonts)
	cmd.Env = []string{}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("typst fonts: %v", err)
	}
	listed := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, f := range reportdesign.Families() {
		if !slices.Contains(listed, f.Name) {
			t.Errorf("the image lacks %s", f.Name)
			continue
		}
		d := reportdesign.Default()
		role := "body"
		switch {
		case f.HeadingsOnly:
			role, d.Fonts.Headings = "headings", f.Name
		case f.Kind == reportdesign.Mono:
			role, d.Fonts.Code = "code", f.Name
		default:
			d.Fonts.Body = f.Name
		}
		doc := d.Canonical()
		stamp, err := StampTheme(doc)
		if err != nil {
			t.Fatalf("%s as %s: %v", f.Name, role, err)
		}
		spec := pdfSpec(t)
		spec.PDF.Theme = stamp
		s := pdfServer(t, spec, typst)
		mustWrite(t, filepath.Join(spec.OutDir, "reports", "f.md"), []byte("# Family\n\n## Section\n\nBody *italic* **bold** `code`.\n\n中文 العربية हिन्दी\n"))
		if got, isErr := exportPDF(t, s, map[string]any{"source": "f.md"}); isErr {
			t.Fatalf("%s: export = %q", f.Name, got)
		}
		if pdf := readReport(t, spec, "f.pdf"); !bytes.Contains(pdf, []byte(strings.ReplaceAll(f.Name, " ", ""))) {
			t.Errorf("a render in %s does not use it", f.Name)
		}
	}
}

// TestHardLineBreaks: a letter keeps each short line of a paragraph.
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

// fixedNow is the day the tests render on.
var fixedNow = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// stampSHA is a document's hex SHA-256, as a stamp names it.
func stampSHA(doc string) string {
	sum := sha256.Sum256([]byte(doc))
	return hex.EncodeToString(sum[:])
}

// pngPPI is the resolution the margin check draws at.
const pngPPI = 48

// renderPNGs draws src in form under the design doc as the export would,
// one PNG a page.
func renderPNGs(t *testing.T, typst typstFiles, designDoc []byte, form, src string) []image.Image {
	t.Helper()
	return renderPNGsAt(t, typst, designDoc, form, src, pngPPI, false)
}

// renderPNGsAt is renderPNGs at a resolution, of the first page alone when
// first is set.
func renderPNGsAt(t *testing.T, typst typstFiles, designDoc []byte, form, src string, ppi int, first bool) []image.Image {
	t.Helper()
	root, out := t.TempDir(), t.TempDir()
	d, err := reportdesign.Parse(designDoc)
	if err != nil {
		t.Fatal(err)
	}
	doc := reportMarkdown(src, form)
	logoPNG, logo, err := renderLogo(d)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := renderMeta(d, form, doc, doc.title, "acme/demo@1.2.3", map[string]pdfImage{}, logo, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	template, err := pdfFiles.ReadFile("pdf/report.typ")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"main.typ": template, "report.md": []byte(doc.text), "design.json": renderDesign(d), "meta.json": meta}
	if logo != nil {
		files[logoFile] = logoPNG
	}
	for name, b := range files {
		mustWrite(t, filepath.Join(root, name), b)
	}
	args := []string{"compile", "--root", root, "--ignore-system-fonts", "--font-path", typst.fonts,
		"--package-path", typst.packages, "--package-cache-path", filepath.Join(out, ".packages"),
		"--format", "png", "--ppi", fmt.Sprint(ppi)}
	if first {
		args = append(args, "--pages", "1")
	}
	cmd := exec.Command(typst.bin, append(args, filepath.Join(root, "main.typ"), filepath.Join(out, "page-{0p}.png"))...)
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

// thumbnailPPI is the thumbnails' resolution: an A4 page is 496 by 702.
const thumbnailPPI = 60

// TestThumbnailsAreTheTemplates: each built-in
// theme's thumbnail in pkg/reportthumbs is the first page of the sample
// report as this template and the pinned image draw it, pixel for pixel.
// AGENTRY_UPDATE_THUMBNAILS=1 writes them instead.
func TestThumbnailsAreTheTemplates(t *testing.T) {
	typst := realTypst(t)
	sample, err := os.ReadFile(filepath.Join("testdata", "pdf", "thumbnail.md"))
	if err != nil {
		t.Fatal(err)
	}
	update := os.Getenv("AGENTRY_UPDATE_THUMBNAILS") == "1"
	for _, id := range BuiltinThemes() {
		doc, err := BuiltinTheme(id)
		if err != nil {
			t.Fatal(err)
		}
		page := renderPNGsAt(t, typst, doc, FormReport, string(sample), thumbnailPPI, true)[0]
		file := filepath.Join("..", "..", "pkg", "reportthumbs", id+".png")
		if update {
			var b bytes.Buffer
			if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&b, page); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, b.Bytes(), 0o644); err != nil { //nolint:gosec // a committed asset
				t.Fatal(err)
			}
			continue
		}
		// Read by path: this file is embedded in the image's build context,
		// which imports nothing but the standard library.
		kept, err := os.ReadFile(file) //nolint:gosec // the repository's own asset
		if err != nil {
			t.Errorf("%s has no thumbnail (%v): run with AGENTRY_UPDATE_THUMBNAILS=1", id, err)
			continue
		}
		want, err := png.Decode(bytes.NewReader(kept))
		if err != nil {
			t.Fatal(err)
		}
		if x, y, ok := firstDifference(want, page); ok {
			t.Errorf("%s's thumbnail differs from the template's render at (%d, %d): run with AGENTRY_UPDATE_THUMBNAILS=1 and commit it", id, x, y)
		}
	}
}

// firstDifference is the first pixel where a and b differ, or a difference
// in size at (-1, -1).
func firstDifference(a, b image.Image) (int, int, bool) {
	if a.Bounds() != b.Bounds() {
		return -1, -1, true
	}
	r := a.Bounds()
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			ar, ag, ab, aa := a.At(x, y).RGBA()
			br, bg, bb, ba := b.At(x, y).RGBA()
			if ar != br || ag != bg || ab != bb || aa != ba {
				return x, y, true
			}
		}
	}
	return 0, 0, false
}

// TestRealRenderABandTitleIsWhite: a
// band title block sets the title in white on the accent, the colour the
// core's contrast check assumed; no pixel inside the band is darker than the
// accent itself (the near-black body text drawn there by mistake would be).
func TestRealRenderABandTitleIsWhite(t *testing.T) {
	typst := realTypst(t)
	for _, accent := range []string{"#1f6feb", "#0b3d2e"} {
		d, _ := reportdesign.Builtin("modern")
		d.Accent = accent
		page := renderPNGsAt(t, typst, d.Canonical(), FormReport, "# A band title\n\nBody text.\n", thumbnailPPI, true)[0]
		ar, ag, ab := hexRGB(t, accent)
		rgb := func(x, y int) (uint32, uint32, uint32) {
			r, g, bl, _ := page.At(x, y).RGBA()
			return r >> 8, g >> 8, bl >> 8
		}
		isAccent := func(x, y int) bool { r, g, bl := rgb(x, y); return near(r, ar) && near(g, ag) && near(bl, ab) }
		// The band is the box of accent pixels; its title is everything
		// inside it that is not the accent.
		b := page.Bounds()
		x0, y0, x1, y1 := b.Max.X, b.Max.Y, -1, -1
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				if isAccent(x, y) {
					x0, y0, x1, y1 = min(x0, x), min(y0, y), max(x1, x), max(y1, y)
				}
			}
		}
		if x1 < 0 {
			t.Fatalf("%s: no band drawn", accent)
		}
		white, dark := 0, 0
		for y := y0 + 2; y < y1-1; y++ {
			for x := x0 + 2; x < x1-1; x++ {
				r, g, bl := rgb(x, y)
				if r > 240 && g > 240 && bl > 240 {
					white++
				}
				if luma(r, g, bl) < luma(ar, ag, ab)-8 {
					dark++
				}
			}
		}
		if white == 0 || dark > 0 {
			t.Errorf("%s: inside the band %d pixels are white and %d darker than the accent: the band's title is not white", accent, white, dark)
		}
	}
}

func hexRGB(t *testing.T, h string) (uint32, uint32, uint32) {
	t.Helper()
	var r, g, b uint32
	if _, err := fmt.Sscanf(h, "#%02x%02x%02x", &r, &g, &b); err != nil {
		t.Fatal(err)
	}
	return r, g, b
}

func near(a, b uint32) bool { return a+3 >= b && b+3 >= a }

func luma(r, g, b uint32) uint32 { return (299*r + 587*g + 114*b) / 1000 }
