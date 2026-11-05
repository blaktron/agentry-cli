package toolsrv

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/runnerimage/reportdesign"
)

// A custom cover that draws a full-width accent panel, the title in white on
// it with facts.title, the subtitle and the date, and its one image with alt
// text.
const goodCover = `#let cover(facts, design) = {
  place(top + left, rect(width: 100%, height: 45%, fill: facts.accent))
  place(top + left, dx: 18mm, dy: 40mm, block(width: 150mm, (facts.title)(size: 34pt, fill: white)))
  place(bottom + left, dx: 18mm, dy: -30mm, {
    text(size: 14pt, fill: facts.ink, facts.subtitle)
    linebreak()
    text(size: 10pt, fill: facts.muted, facts.date)
  })
  if facts.images.len() > 0 {
    place(bottom + right, dx: -18mm, dy: -20mm, image(facts.images.at(0), width: 40mm, alt: "A gradient"))
  }
}
`

// customDesign is Boardroom with a custom cover of source and images,
// always drawn.
func customDesign(t *testing.T, source string, images ...reportdesign.Image) reportdesign.Design {
	t.Helper()
	d := branded(t, "boardroom")
	d.Cover.Style = "custom"
	d.Cover.When = "always"
	if images == nil {
		images = []reportdesign.Image{}
	}
	d.CustomCover = reportdesign.CustomCover{Source: source, Images: images}
	return d
}

// exportCustom exports longReport under d and answers the tool's answer,
// the PDF and the ledger's entry for it.
func exportCustom(t *testing.T, typst typstFiles, d reportdesign.Design, timeout time.Duration) (string, []byte, PDFEntry) {
	t.Helper()
	stamp, err := StampTheme(d.Canonical())
	if err != nil {
		t.Fatal(err)
	}
	spec := pdfSpec(t)
	spec.PDF.Theme = stamp
	s := pdfServer(t, spec, typst)
	if timeout > 0 {
		s.pdfTimeout = timeout
	}
	mustWrite(t, filepath.Join(spec.OutDir, "reports", "r.md"), []byte(longReport))
	got, isErr := exportPDF(t, s, map[string]any{"source": "r.md"})
	if isErr {
		t.Fatalf("export = %q", got)
	}
	ledger, err := os.ReadFile(spec.PDF.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	var e PDFEntry
	if err := json.Unmarshal(bytes.TrimSpace(ledger), &e); err != nil {
		t.Fatalf("ledger %q: %v", ledger, err)
	}
	return got, readReport(t, spec, "r.pdf"), e
}

// TestRealRenderACustomCover: a custom cover is drawn as the first page,
// before the report's own pages, tagged for PDF/UA-1, with its image
// re-encoded into the render root; the answer and the ledger say it was
// drawn.
func TestRealRenderACustomCover(t *testing.T) {
	typst := realTypst(t)
	d := customDesign(t, goodCover, reportdesign.ImageOf(coverPNG(t), "png"))
	got, pdf, e := exportCustom(t, typst, d, 0)
	if !strings.Contains(got, "with a custom cover page") {
		t.Errorf("the answer %q does not name the custom cover", got)
	}
	if e.Cover != "custom" || e.CustomCover != CustomCoverDrawn || e.CoverFailure != "" {
		t.Errorf("ledger entry %+v, want the custom cover drawn", e)
	}
	for _, want := range []string{"/StructTreeRoot", "pdfuaid:part"} {
		if !bytes.Contains(pdf, []byte(want)) {
			t.Errorf("the PDF lacks %s", want)
		}
	}
	// The same report under the band cover has as many pages: the custom
	// cover is one page.
	band := d
	band.Cover.Style = "band"
	band.CustomCover = reportdesign.CustomCover{}
	_, bandPDF, _ := exportCustom(t, typst, band, 0)
	if p, want := pdfPages(pdf), pdfPages(bandPDF); p != want || p < 2 {
		t.Errorf("the custom cover's PDF has %d pages, the band cover's %d", p, want)
	}

	// The accent panel is drawn, with the title in white on it.
	shot := customCoverPNG(t, typst, d)
	textOn(t, "custom", shot, d.Accent)
}

// customCoverPNG renders the export's first page for d as a PNG, through
// the server's own render root and Typst's PNG output.
func customCoverPNG(t *testing.T, typst typstFiles, d reportdesign.Design) image.Image {
	t.Helper()
	spec := pdfSpec(t)
	s := pdfServer(t, spec, typst)
	root := t.TempDir()
	doc := reportMarkdown(longReport, FormReport)
	files, custom, err := renderCustomCover(d)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := renderMetaCover(d, FormReport, doc, doc.title, "", map[string]pdfImage{}, nil, nil, coverPlan{style: "custom", custom: custom}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	template, err := pdfFiles.ReadFile("pdf/report.typ")
	if err != nil {
		t.Fatal(err)
	}
	files["main.typ"], files["report.md"], files["design.json"], files["meta.json"] = template, []byte(doc.text), renderDesign(d), meta
	for name, b := range files {
		mustWrite(t, filepath.Join(root, name), b)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	r := s.compileTypst(root, "png", thumbnailPPI)
	if r.failure != "" {
		t.Fatalf("png render: %s %s %s", r.failure, r.fault, r.diag)
	}
	img, err := png.Decode(bytes.NewReader(r.out))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// TestACustomCoverCannotImportAPackage: a cover that imports a package
// is passed over for the built-in cover, since the renderer has no package
// downloader; the export still succeeds.
func TestACustomCoverCannotImportAPackage(t *testing.T) {
	typst := realTypst(t)
	d := customDesign(t, "#import \"@preview/cetz:0.4.2\": canvas\n"+goodCover)
	got, _, e := exportCustom(t, typst, d, 0)
	if e.Cover != FallbackCoverStyle || e.CustomCover != CustomCoverPassedOver || e.CoverFailure != "import" {
		t.Errorf("ledger entry %+v, want the cover passed over for import", e)
	}
	if !strings.Contains(got, "passed over because "+CoverFailures["import"]) {
		t.Errorf("the answer %q does not say why the cover was passed over", got)
	}
}

// TestACustomCoverWithoutAltTextFallsBack: an image without alt text fails
// the tagged PDF, and the cover is passed over.
func TestACustomCoverWithoutAltTextFallsBack(t *testing.T) {
	typst := realTypst(t)
	src := strings.Replace(goodCover, `, alt: "A gradient"`, "", 1)
	d := customDesign(t, src, reportdesign.ImageOf(coverPNG(t), "png"))
	_, _, e := exportCustom(t, typst, d, 0)
	if e.CustomCover != CustomCoverPassedOver || e.CoverFailure != "altText" {
		t.Errorf("ledger entry %+v, want the cover passed over for altText", e)
	}
}

// TestACustomCoverIsPassedOverForEachKindOfFailure: each way a cover can
// fail names its kind, the export draws the built-in cover, and neither
// the answer nor the ledger carries the compiler's text.
func TestACustomCoverIsPassedOverForEachKindOfFailure(t *testing.T) {
	typst := realTypst(t)
	const marker = "COVER-CODE-WORDS-7731"
	cases := []struct {
		name, src, kind string
		timeout         time.Duration
	}{
		{"a syntax error", "#let cover(facts, design) = { " + marker + " ", "compile", 0},
		{"its own panic", "#let cover(facts, design) = panic(\"" + marker + "\")", "compile", 0},
		{"a page break", "#let cover(facts, design) = { (facts.title)(); pagebreak(); [" + marker + "] }", "pages", 0},
		{"no title", "#let cover(facts, design) = [" + marker + "]", "title", 0},
		{"a file outside the render root", "#let cover(facts, design) = { (facts.title)(); read(\"/etc/hostname\") }", "compile", 0},
		{"a parent's file", "#let cover(facts, design) = { (facts.title)(); read(\"../../etc/hostname\") }", "compile", 0},
		{"no cover function", "#let other(facts, design) = [" + marker + "]", "compile", 0},
		{"the time limit", "#let cover(facts, design) = { let n = 0; for i in range(100000) { for j in range(1000000) { n += 1 } }; (facts.title)() }", "time", 3 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, e := exportCustom(t, typst, customDesign(t, c.src), c.timeout)
			if e.Cover != FallbackCoverStyle || e.CustomCover != CustomCoverPassedOver || e.CoverFailure != c.kind {
				t.Errorf("ledger entry %+v, want passed over for %s", e, c.kind)
			}
			if !strings.Contains(got, "with a band cover page in place of the design's custom cover") {
				t.Errorf("the answer %q does not say the cover was passed over", got)
			}
			for _, leaked := range []string{marker, "error:", "cover.typ", "hostname"} {
				if strings.Contains(got, leaked) {
					t.Errorf("the answer %q carries %q", got, leaked)
				}
			}
		})
	}
}

// TestACustomCoversImagesAreReEncoded: every image reaches the render
// root as the core's re-encoding, named cover/<n>, and one the normalizer
// refuses stops the cover before Typst sees it.
func TestACustomCoversImagesAreReEncoded(t *testing.T) {
	src := append(coverPNG(t), []byte("trailing bytes a decoder ignores")...)
	d := customDesign(t, goodCover, reportdesign.ImageOf(src, "png"))
	files, meta, err := renderCustomCover(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Images) != 1 || meta.Images[0].Path != "cover/1.png" || meta.Images[0].Width != 400 {
		t.Fatalf("images %+v", meta.Images)
	}
	if got := files["cover/1.png"]; bytes.Equal(got, src) || bytes.Contains(got, []byte("trailing bytes")) {
		t.Error("the image reached the render root as given, not re-encoded")
	}
	if string(files[coverSourceFile]) != goodCover {
		t.Error("the source is not the design's")
	}
	big := image.NewNRGBA(image.Rect(0, 0, 5000, 4000))
	big.Set(0, 0, color.NRGBA{A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, big); err != nil {
		t.Fatal(err)
	}
	d.CustomCover.Images = []reportdesign.Image{reportdesign.ImageOf(buf.Bytes(), "png")}
	if _, _, err := renderCustomCover(d); err == nil {
		t.Fatal("an image past the pixel bound re-encoded")
	}
}

// TestCoverFailureReadsTheDiagnostics holds the kinds to the words Typst
// 0.15.1 answers with.
func TestCoverFailureReadsTheDiagnostics(t *testing.T) {
	cases := map[string]typstResult{
		"time":    {failure: "time"},
		"memory":  {failure: "memory"},
		"title":   {failure: "compile", diag: "main.typ:660:7: error: panicked with: \"agentry: the custom cover drew no title\""},
		"altText": {failure: "compile", diag: "cover.typ:4:20: error: PDF/UA-1 error: missing alt text"},
		"pages":   {failure: "compile", diag: "cover.typ:4:21: error: pagebreaks are not allowed inside of containers"},
		"import":  {failure: "compile", diag: "cover.typ:1:8: error: failed to download package (package downloads are disabled)"},
		"compile": {failure: "compile", diag: "cover.typ:1:1: error: unclosed delimiter"},
	}
	for want, r := range cases {
		if got := coverFailure(r); got != want {
			t.Errorf("coverFailure(%q) = %q, want %q", r.diag, got, want)
		}
		if _, ok := CoverFailures[want]; !ok {
			t.Errorf("%s has no words in CoverFailures", want)
		}
	}
}

// TestCoverStyleDrawsACustomCoverOnlyWithOne: custom without a custom
// cover draws the fallback, and a design without the custom style never
// draws one.
func TestCoverStyleDrawsACustomCoverOnlyWithOne(t *testing.T) {
	d := branded(t, "boardroom")
	d.Cover.Style = "custom"
	if got := coverStyle(d, nil); got != FallbackCoverStyle {
		t.Errorf("custom without a cover draws %q", got)
	}
	d.CustomCover = reportdesign.CustomCover{Source: goodCover, Images: []reportdesign.Image{}}
	if got := coverStyle(d, nil); got != "custom" {
		t.Errorf("custom with a cover draws %q", got)
	}
	d.Cover.Style = "split"
	if got := coverStyle(d, nil); got != "split" {
		t.Errorf("split with a custom cover kept draws %q", got)
	}
}
