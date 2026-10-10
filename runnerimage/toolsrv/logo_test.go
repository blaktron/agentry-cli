package toolsrv

// The person's logo in a PDF (plan report-designs.md §4 "The logo", D9,
// invariant 12; agentry-cli#816): it reaches the render root only as the PNG
// reportdesign.NormalizeLogo writes from its pixels, it is drawn in the
// title block and nowhere else, and a design without one renders as before.

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/runnerimage/reportdesign"
)

// logoMarker is what a crafted logo hides in a text chunk and after its
// end: neither may reach the render root.
const logoMarker = "agentry-816-crafted"

// craftedLogo is a w×h PNG in magenta (a colour no built-in theme draws)
// on a transparent ground, with a tEXt chunk after its header and bytes
// after its end, both holding logoMarker: a PNG within the logo's bounds
// that is not reportdesign's own encoding.
func craftedLogo(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := h / 8; y < h-h/8; y++ {
		for x := w / 8; x < w-w/8; x++ {
			img.Set(x, y, color.NRGBA{R: 255, B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	// The signature (8) and the IHDR chunk (25) come first.
	data := []byte("Comment\x00" + logoMarker)
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(data))) //nolint:gosec // a short test chunk
	chunk = append(chunk, "tEXt"...)
	chunk = append(chunk, data...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(append([]byte("tEXt"), data...)))
	out := append(append(append([]byte{}, b[:33]...), chunk...), b[33:]...)
	return append(out, logoMarker...)
}

// logoDesign is a built-in theme with the title block style and a logo.
func logoDesign(t *testing.T, theme, titleBlock string, logo []byte) reportdesign.Design {
	t.Helper()
	d, ok := reportdesign.Builtin(theme)
	if !ok {
		t.Fatalf("no theme %s", theme)
	}
	if titleBlock != "" {
		d.TitleBlock = titleBlock
	}
	d.Logo = reportdesign.Logo{PNG: base64.StdEncoding.EncodeToString(logo)}
	if err := d.Validate(); err != nil {
		t.Fatalf("the test design does not validate: %v", err)
	}
	return d
}

func TestRenderLogoIsReencoded(t *testing.T) {
	crafted := craftedLogo(t, 600, 200)
	if !bytes.Contains(crafted, []byte(logoMarker)) {
		t.Fatal("the crafted logo holds no marker")
	}
	b, logo, err := renderLogo(logoDesign(t, "modern", "", crafted))
	if err != nil {
		t.Fatal(err)
	}
	if logo == nil || logo.Path != logoFile || logo.Width != 600 || logo.Height != 200 {
		t.Fatalf("logo = %+v", logo)
	}
	if bytes.Contains(b, []byte(logoMarker)) || bytes.Contains(b, []byte("tEXt")) {
		t.Error("the text chunk or the trailing bytes reached the render root's logo")
	}
	if _, err := png.Decode(bytes.NewReader(b)); err != nil {
		t.Errorf("the re-encoded logo does not decode: %v", err)
	}
	// A design without a logo has none, and its render document is the
	// design's own canonical form: nothing it drew before changes.
	plain := reportdesign.Default()
	if b, logo, err := renderLogo(plain); b != nil || logo != nil || err != nil {
		t.Errorf("no logo = (%d bytes, %+v, %v)", len(b), logo, err)
	}
	if !bytes.Equal(renderDesign(plain), plain.Canonical()) {
		t.Error("a design without a logo is not written to the render root as its canonical form")
	}
	if doc := renderDesign(logoDesign(t, "modern", "", crafted)); bytes.Contains(doc, []byte(`"logo"`)) {
		t.Error("the render root's design.json carries the logo's bytes")
	}
}

// TestExportPDFDrawsTheReencodedLogo: the render root holds the logo as
// logo.png, re-encoded, and design.json without it; the ledger and the
// answer say a logo was drawn. The fake renderer prints what it found.
func TestExportPDFDrawsTheReencodedLogo(t *testing.T) {
	spec := pdfSpec(t)
	d := logoDesign(t, "classic", "", craftedLogo(t, 300, 300))
	stamp, err := StampTheme(d.Canonical())
	if err != nil {
		t.Fatal(err)
	}
	spec.PDF.Theme = stamp
	s := pdfServer(t, spec, fakeTypst(t, fakePDF+`
if [ -f "$ROOT/logo.png" ]; then echo has-logo-png; fi
if grep -q '"logo"' "$ROOT/design.json"; then echo design-has-logo; fi
if grep -q '`+logoMarker+`' "$ROOT/logo.png" "$ROOT/design.json" "$ROOT/meta.json"; then echo marker-reached-root; fi
grep -o '"logo":{[^}]*}' "$ROOT/meta.json"`))
	mustWrite(t, filepath.Join(spec.OutDir, "reports", "r.md"), []byte("# Title\n\nText.\n"))
	got, isErr := exportPDF(t, s, map[string]any{"source": "r.md"})
	if isErr || !strings.Contains(got, "in the Classic report design, with the person's logo)") {
		t.Fatalf("export = (%q, %v)", got, isErr)
	}
	pdf := string(readReport(t, spec, "r.pdf"))
	for _, want := range []string{"has-logo-png", `"logo":{"path":"logo.png","width":300,"height":300}`} {
		if !strings.Contains(pdf, want) {
			t.Errorf("the render root lacks %s:\n%s", want, pdf)
		}
	}
	for _, bad := range []string{"design-has-logo", "marker-reached-root"} {
		if strings.Contains(pdf, bad) {
			t.Errorf("the render root: %s", bad)
		}
	}
	b, err := os.ReadFile(spec.PDF.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	var e PDFEntry
	if err := json.Unmarshal(bytes.TrimSpace(b), &e); err != nil || !e.Logo {
		t.Errorf("ledger entry = %+v (%v), want the logo recorded", e, err)
	}

	// Without a logo, the ledger says none and the root holds no logo.png.
	spec2 := pdfSpec(t)
	s2 := pdfServer(t, spec2, fakeTypst(t, fakePDF+`
if [ -e "$ROOT/logo.png" ]; then echo has-logo-png; fi
grep -o '"logo":null' "$ROOT/meta.json"`))
	mustWrite(t, filepath.Join(spec2.OutDir, "reports", "r.md"), []byte("# Title\n\nText.\n"))
	if got, isErr := exportPDF(t, s2, map[string]any{"source": "r.md"}); isErr || strings.Contains(got, "logo") {
		t.Fatalf("export without a logo = (%q, %v)", got, isErr)
	}
	pdf = string(readReport(t, spec2, "r.pdf"))
	if strings.Contains(pdf, "has-logo-png") || !strings.Contains(pdf, `"logo":null`) {
		t.Errorf("without a logo the render root holds:\n%s", pdf)
	}
	b, _ = os.ReadFile(spec2.PDF.Ledger)
	if strings.Contains(string(b), `"logo"`) {
		t.Errorf("a PDF without a logo is recorded with one: %s", b)
	}
}

// A logo that will not re-encode is the person's design to fix: the PDF is
// drawn without it, the ledger says it was not drawn, and the answer says
// why, never blaming the agent.
func TestExportPDFWithALogoThatWillNotReencode(t *testing.T) {
	saved := normalizeLogo
	normalizeLogo = func([]byte) (reportdesign.Logo, error) {
		return reportdesign.Logo{}, &reportdesign.FieldError{Field: "logo", Message: "over 512 KB as a PNG"}
	}
	t.Cleanup(func() { normalizeLogo = saved })
	spec := pdfSpec(t)
	stamp, err := StampTheme(logoDesign(t, "classic", "", craftedLogo(t, 200, 200)).Canonical())
	if err != nil {
		t.Fatal(err)
	}
	spec.PDF.Theme = stamp
	s := pdfServer(t, spec, fakeTypst(t, fakePDF+`
if [ -e "$ROOT/logo.png" ]; then echo has-logo-png; fi
grep -o '"logo":null' "$ROOT/meta.json"`))
	mustWrite(t, filepath.Join(spec.OutDir, "reports", "r.md"), []byte("# Title\n\nText.\n"))
	got, isErr := exportPDF(t, s, map[string]any{"source": "r.md"})
	if isErr || !strings.Contains(got, "the person's logo could not be drawn, which is their design's to fix, not yours") {
		t.Fatalf("export = (%q, %v)", got, isErr)
	}
	if pdf := string(readReport(t, spec, "r.pdf")); strings.Contains(pdf, "has-logo-png") || !strings.Contains(pdf, `"logo":null`) {
		t.Errorf("the render root after a failed re-encode:\n%s", pdf)
	}
	if b, _ := os.ReadFile(spec.PDF.Ledger); strings.Contains(string(b), `"logo"`) {
		t.Errorf("a PDF drawn without the logo is recorded with it: %s", b)
	}
}

// magentaIn counts the magenta pixels of img inside r.
func magentaIn(img image.Image, r image.Rectangle) int {
	n := 0
	r = r.Intersect(img.Bounds())
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			cr, cg, cb, _ := img.At(x, y).RGBA()
			if cr>>8 > 200 && cg>>8 < 60 && cb>>8 > 200 {
				n++
			}
		}
	}
	return n
}

// TestRealRenderALogoInEveryTitleBlock (D9, invariant 12; agentry-cli#816's
// acceptance): the logo is drawn in the title block of each style, at its
// right, in the top part of page 1, and on no later page; a long report's
// cover draws it, and a report with no title of its own draws it alone at
// the top of page 1. The PDF stays tagged, and the logo carries its text
// alternative.
func TestRealRenderALogoInEveryTitleBlock(t *testing.T) {
	typst := realTypst(t)
	logo := craftedLogo(t, 900, 300)
	// Two pages and more, short of a cover (coverWords).
	body := "Opening paragraph.\n\n## A section\n\n" + strings.Repeat(strings.Repeat("A line of the report's body text. ", 20)+"\n\n", 9)
	for _, style := range []string{"band", "bar", "ruled", "none"} {
		for _, form := range Forms {
			t.Run(style+"/"+form, func(t *testing.T) {
				d := logoDesign(t, "modern", style, logo)
				pages := renderPNGsAt(t, typst, d.Canonical(), form, "# A title with a logo\n\n"+body, pngPPI, false)
				if len(pages) < 2 {
					t.Fatalf("%d pages, want a second to look at", len(pages))
				}
				b := pages[0].Bounds()
				top := image.Rect(b.Min.X, b.Min.Y, b.Max.X, b.Min.Y+b.Dy()/4)
				right := image.Rect(b.Min.X+b.Dx()/2, b.Min.Y, b.Max.X, b.Max.Y)
				if n := magentaIn(pages[0], top.Intersect(right)); n < 20 {
					t.Errorf("page 1's top right has %d logo pixels: the logo is not in the title block", n)
				}
				if n := magentaIn(pages[0], image.Rect(b.Min.X, b.Min.Y+b.Dy()/4, b.Max.X, b.Max.Y)); n > 0 {
					t.Errorf("page 1 has %d logo pixels below its title block", n)
				}
				for i, p := range pages[1:] {
					if n := magentaIn(p, p.Bounds()); n > 0 {
						t.Errorf("page %d has %d logo pixels: the logo is drawn past the title block", i+2, n)
					}
				}
				margin := 22.0
				if form == FormLetter {
					margin = 25
				}
				if x, y, ok := inkOutside(pages[0], int(margin/25.4*pngPPI)-2); ok {
					t.Errorf("page 1 has ink in its side margin at (%d, %d)", x, y)
				}
			})
		}
	}
	t.Run("cover", func(t *testing.T) {
		d := logoDesign(t, "editorial", "", logo)
		long := "# A long titled report\n\n" + strings.Repeat("word ", coverWords+50) + "\n"
		pages := renderPNGsAt(t, typst, d.Canonical(), FormReport, long, pngPPI, false)
		if n := magentaIn(pages[0], pages[0].Bounds()); n < 20 {
			t.Errorf("the cover has %d logo pixels", n)
		}
		for i, p := range pages[1:] {
			if n := magentaIn(p, p.Bounds()); n > 0 {
				t.Errorf("page %d after the cover has %d logo pixels", i+2, n)
			}
		}
	})
	t.Run("untitled", func(t *testing.T) {
		d := logoDesign(t, "technical", "", logo)
		pages := renderPNGsAt(t, typst, d.Canonical(), FormReport, body, pngPPI, false)
		b := pages[0].Bounds()
		if n := magentaIn(pages[0], image.Rect(b.Min.X+b.Dx()/2, b.Min.Y, b.Max.X, b.Min.Y+b.Dy()/5)); n < 20 {
			t.Errorf("an untitled report's page 1 has %d logo pixels at its top right", n)
		}
	})
	t.Run("tagged", func(t *testing.T) {
		d := logoDesign(t, "plain", "", logo)
		stamp, err := StampTheme(d.Canonical())
		if err != nil {
			t.Fatal(err)
		}
		spec := pdfSpec(t)
		spec.PDF.Theme = stamp
		s := pdfServer(t, spec, typst)
		mustWrite(t, filepath.Join(spec.OutDir, "reports", "r.md"), []byte("# Tagged\n\nText.\n"))
		if got, isErr := exportPDF(t, s, map[string]any{"source": "r.md"}); isErr {
			t.Fatalf("export = %q", got)
		}
		pdf := readReport(t, spec, "r.pdf")
		objects := pdfObjects(pdf)
		for _, want := range []string{"/StructTreeRoot", "pdfuaid:part"} {
			if !bytes.Contains(pdf, []byte(want)) {
				t.Errorf("the PDF lacks %s", want)
			}
		}
		for _, want := range []string{`/Subtype\s*/Image`, `/S\s*/Figure`, `/Alt\s*\(Logo\)`} {
			if !regexp.MustCompile(want).Match(objects) {
				t.Errorf("the PDF's objects lack %s: the logo is not drawn as a tagged figure with its text alternative", want)
			}
		}
		if bytes.Contains(pdf, []byte(logoMarker)) {
			t.Error("the crafted logo's marker is in the PDF")
		}
	})
}

// pdfObjects is the PDF with each of its Flate streams inflated after it,
// so a test can look for what Typst writes into object streams.
func pdfObjects(pdf []byte) []byte {
	out := append([]byte{}, pdf...)
	for _, m := range pdfStream.FindAllIndex(pdf, -1) {
		end := bytes.Index(pdf[m[1]:], []byte("endstream"))
		if end < 0 {
			continue
		}
		r, err := zlib.NewReader(bytes.NewReader(pdf[m[1] : m[1]+end]))
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(r, 64<<20))
		out = append(out, b...)
	}
	return out
}

var pdfStream = regexp.MustCompile(`stream\r?\n`)
