package toolsrv

// The version-3 design as the template draws it: the running header and
// its pages, the slots and their tokens, the six cover styles and when a
// cover is drawn, the subtitle, and the version-2 look held. The tests
// named TestRealRender… run the real renderer from the pinned image, as the
// typst-image CI job does.

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/runnerimage/reportdesign"
)

// TestSlotParts: a slot is split as pkg/reportdesign's ExpandSlot reads it:
// the static tokens filled as text, the page tokens kept for the template,
// anything else between braces literal, braces included; an empty slot has
// no parts.
func TestSlotParts(t *testing.T) {
	facts := map[string]string{"title": "T", "author": "A", "date": "D", "brand": "B", "confidentiality": "C"}
	page := map[string]string{"page": "<page>", "pages": "<pages>", "section": "<section>"}
	all := map[string]string{}
	for k, v := range facts {
		all[k] = v
	}
	for k, v := range page {
		all[k] = v
	}
	for _, slot := range []string{
		"", "plain text", "{title}", "{page} of {pages}", "{brand} · {section}", "{{page}}", "{nope} {title", "}{", "{", "a{b}c{page}d",
		"{Title} {PAGE}", "{confidentiality}{author}{date}", "#panic(\"x\") {page}", "{page}{page}",
	} {
		parts := slotParts(slot, facts)
		var joined strings.Builder
		for _, p := range parts {
			switch {
			case p.Text != "" && p.Token != "", p.Text == "" && p.Token == "":
				t.Errorf("%q: a part with text %q and token %q", slot, p.Text, p.Token)
			case p.Token != "":
				if !slices.Contains(pageTokens, p.Token) {
					t.Errorf("%q: the token %q is not one the template fills", slot, p.Token)
				}
				joined.WriteString(page[p.Token])
			default:
				joined.WriteString(p.Text)
			}
		}
		if want := reportdesign.ExpandSlot(slot, all); joined.String() != want {
			t.Errorf("%q: parts %+v read %q, ExpandSlot %q", slot, parts, joined.String(), want)
		}
		if slot == "" && len(parts) != 0 {
			t.Errorf("an empty slot has parts %+v", parts)
		}
	}
	for i, p := range slotParts("a{b}c{page}d", facts) {
		want := []slotPart{{Text: "a{b}c"}, {Token: "page"}, {Text: "d"}}[i]
		if p != want {
			t.Errorf("part %d = %+v, want %+v", i, p, want)
		}
	}
}

// TestASubtitle: a report's subtitle is one line of emphasis on its own
// right after the opening heading, taken out of the body; anything else
// stays as the agent wrote it, and a memo or a letter has none.
func TestASubtitle(t *testing.T) {
	for _, c := range []struct {
		name, src, form, subtitle, text string
	}{
		{"an italic line", "# Title\n\n*What was found*\n\nBody.\n", FormReport, "What was found", "\n\n\n\nBody.\n"},
		{"an underscore line", "# Title\n_What was found_\n\nBody.\n", FormReport, "What was found", "\n\n\nBody.\n"},
		{"with inline markup", "# Title\n\n*The **third** quarter*\n\nBody.\n", FormReport, "The third quarter", "\n\n\n\nBody.\n"},
		{"a bold line is not one", "# Title\n\n**Bold**\n\nBody.\n", FormReport, "", "\n\n**Bold**\n\nBody.\n"},
		{"a paragraph is not one", "# Title\n\n*Starts in italics* and goes on.\n\nBody.\n", FormReport, "", "\n\n*Starts in italics* and goes on.\n\nBody.\n"},
		{"a second line is not one", "# Title\n\n*Line one*\nline two\n\nBody.\n", FormReport, "", "\n\n*Line one*\nline two\n\nBody.\n"},
		{"plain text is not one", "# Title\n\nBody.\n", FormReport, "", "\n\nBody.\n"},
		{"no title, no subtitle", "*Italic*\n\nBody.\n", FormReport, "", "*Italic*\n\nBody.\n"},
		{"a memo has none", "# Title\n\n*Italic*\n\nBody.\n", FormMemo, "", "\n\n*Italic*\n\nBody.\n"},
		{"a letter has none", "# Title\n\n*Italic*\n\nBody.\n", FormLetter, "", "\n\n*Italic*\n\nBody.\n"},
	} {
		doc := reportMarkdown(c.src, c.form)
		if doc.subtitle != c.subtitle || doc.text != c.text {
			t.Errorf("%s: subtitle %q, text %q; want %q, %q", c.name, doc.subtitle, doc.text, c.subtitle, c.text)
		}
	}
}

// coverPNG is a 400 by 300 PNG with a gradient, a cover image a person
// might upload.
func coverPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 400, 300))
	for y := 0; y < 300; y++ {
		for x := 0; x < 400; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(x * 255 / 400), G: 90, B: uint8(y * 255 / 300), A: 255}) //nolint:gosec // bounded
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// branded is a built-in with a brand block and the facts a cover can show.
func branded(t *testing.T, theme string) reportdesign.Design {
	t.Helper()
	d, ok := reportdesign.Builtin(theme)
	if !ok {
		t.Fatalf("no theme %s", theme)
	}
	d.Brand = reportdesign.Brand{Name: "Acme Assurance", Strapline: "Independent reviews", Contact: []string{"14 Harbour Road", "+31 10 123 4567"}, Website: "acme.example", Confidentiality: "Confidential"}
	d.Cover.Shows = reportdesign.CoverFacts
	return d
}

// headerZone is the top of a page where the running header is drawn:
// above the text, inside the margin.
func headerZone(p image.Image) image.Rectangle {
	b := p.Bounds()
	zone := 13.0 / 25.4 * pngPPI
	return image.Rect(b.Min.X, b.Min.Y, b.Max.X, b.Min.Y+int(zone))
}

// inkIn counts the pixels in r that are not white.
func inkIn(p image.Image, r image.Rectangle) int {
	n := 0
	r = r.Intersect(p.Bounds())
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			cr, cg, cb, _ := p.At(x, y).RGBA()
			if cr>>8 < 240 || cg>>8 < 240 || cb>>8 < 240 {
				n++
			}
		}
	}
	return n
}

// sameIn says two pages are pixel for pixel the same inside r.
func sameIn(a, b image.Image, r image.Rectangle) bool {
	r = r.Intersect(a.Bounds()).Intersect(b.Bounds())
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if a.At(x, y) != b.At(x, y) {
				return false
			}
		}
	}
	return true
}

// threePages is a titled document that runs to three pages, short of a
// long report's cover, with a section opening its second page.
var threePages = "# A titled document\n\nOpening paragraph.\n\n## Alpha\n\n" + strings.Repeat(strings.Repeat("A line of the body text. ", 20)+"\n\n", 7) +
	"## Beta\n\n" + strings.Repeat(strings.Repeat("A line of the body text. ", 20)+"\n\n", 12)

// TestRealRenderTheHeaderPages: a running header is drawn from page 2 on a
// report, on every page of a memo and never on a letter, in every style
// but none, which draws nothing on any page.
func TestRealRenderTheHeaderPages(t *testing.T) {
	typst := realTypst(t)
	for _, style := range reportdesign.HeaderStyles {
		for _, form := range Forms {
			d := branded(t, "modern")
			d.Cover.When = "never"
			d.Header.Style = style
			d.Header.Slots = [3]string{"{brand}", "{section}", "{page} of {pages}"}
			pages := renderPNGs(t, typst, d.Canonical(), form, threePages)
			if len(pages) < 3 {
				t.Fatalf("%s/%s: %d pages, want three", style, form, len(pages))
			}
			for i, p := range pages {
				drawn := style != "none" && (form == FormMemo || (form == FormReport && i > 0))
				if got := inkIn(p, headerZone(p)) > 0; got != drawn {
					t.Errorf("%s/%s: page %d's header zone has ink %v, want %v", style, form, i+1, got, drawn)
				}
			}
		}
	}
}

// TestRealRenderSlotTokens: a slot's tokens are filled on each page:
// {section} is the heading of the current section, {page} the page's
// number, and a brace pair around anything else is literal, so that
// "{{page}}" is "{" + the number + "}". The header of the page drawn from
// the token is the same, pixel for pixel, as the header drawn from the
// literal text it stands for.
func TestRealRenderSlotTokens(t *testing.T) {
	typst := realTypst(t)
	render := func(slot string) []image.Image {
		d := branded(t, "modern")
		d.Cover.When = "never"
		d.Header.Style = "titleAndPage"
		d.Header.Slots = [3]string{"", slot, ""}
		return renderPNGs(t, typst, d.Canonical(), FormReport, threePages)
	}
	for _, c := range []struct{ token, literal string }{
		{"{section}", "Alpha"},
		{"{page}", "2"},
		{"{{page}}", "{2}"},
		{"{title} · {page} of {pages}", "A titled document · 2 of 3"},
		{"{brand}: {confidentiality}", "Acme Assurance: Confidential"},
		{"{nope} {Page}", "{nope} {Page}"},
	} {
		got, want := render(c.token), render(c.literal)
		if len(got) != 3 || len(want) != 3 {
			t.Fatalf("%q: %d and %d pages, want three", c.token, len(got), len(want))
		}
		if !sameIn(got[1], want[1], headerZone(got[1])) {
			t.Errorf("%q on page 2 is not drawn as %q", c.token, c.literal)
		}
	}
	// The section changes with the page, and before any section there is
	// none.
	pages := render("{section}")
	if sameIn(pages[1], pages[2], headerZone(pages[1])) {
		t.Error("{section} draws the same on page 2 and page 3, whose sections differ")
	}
	beta := render("Beta")
	if !sameIn(pages[2], beta[2], headerZone(pages[2])) {
		t.Error("{section} on page 3 is not Beta")
	}
	none := render("")
	first := strings.Replace(threePages, "## Alpha", "Alpha", 1)
	d := branded(t, "modern")
	d.Cover.When = "never"
	d.Header.Style = "titleAndPage"
	d.Header.Slots = [3]string{"", "{section}", ""}
	noSection := renderPNGs(t, typst, d.Canonical(), FormReport, first)
	if !sameIn(noSection[1], none[1], headerZone(none[1])) {
		t.Error("{section} before any section draws something")
	}
}

// coverStyles are the six built-in cover styles.
var coverStyles = []string{"band", "split", "geometric", "largeType", "framed", "plate"}

// longReport is a titled report past the cover's length threshold, with a
// subtitle.
var longReport = "# A long titled report\n\n*What was checked and what was found*\n\n" + strings.Repeat("A sentence of the report's body, long enough to fill. ", 200)

// TestRealRenderEveryCoverStyle: each cover style draws one page before the
// report, tagged for PDF/UA-1, with the title in white where the style puts
// it on the accent (band, plate) or the brand in white on the secondary
// (split); plate without a cover image draws as band, and custom, which
// this template does not run, draws as band too; and the ledger names the
// style drawn.
func TestRealRenderEveryCoverStyle(t *testing.T) {
	typst := realTypst(t)
	cover := reportdesign.ImageOf(coverPNG(t), "png")
	for _, style := range append(slices.Clone(coverStyles), "custom") {
		for _, withImage := range []bool{false, true} {
			d := branded(t, "boardroom")
			d.Cover.Style = style
			d.Cover.When = "always"
			if withImage {
				d.CoverImage = cover
			}
			if style == "custom" {
				d.CustomCover = reportdesign.CustomCover{Source: "#panic(\"custom covers are not drawn here\")", Images: []reportdesign.Image{}}
			}
			stamp, err := StampTheme(d.Canonical())
			if err != nil {
				t.Fatalf("%s: %v", style, err)
			}
			spec := pdfSpec(t)
			spec.PDF.Theme = stamp
			s := pdfServer(t, spec, typst)
			mustWrite(t, filepath.Join(spec.OutDir, "reports", "r.md"), []byte(longReport))
			got, isErr := exportPDF(t, s, map[string]any{"source": "r.md"})
			if isErr {
				t.Fatalf("%s (image %v): export = %q", style, withImage, got)
			}
			drawn := style
			if style == "custom" || (style == "plate" && !withImage) {
				drawn = "band"
			}
			if !strings.Contains(got, "with a "+drawn+" cover page") {
				t.Errorf("%s (image %v): the answer %q does not name the %s cover", style, withImage, got, drawn)
			}
			pdf := readReport(t, spec, "r.pdf")
			for _, want := range []string{"/StructTreeRoot", "pdfuaid:part"} {
				if !bytes.Contains(pdf, []byte(want)) {
					t.Errorf("%s: the PDF lacks %s", style, want)
				}
			}
			ledger, err := os.ReadFile(spec.PDF.Ledger)
			if err != nil || !bytes.Contains(ledger, []byte(`"cover":"`+drawn+`"`)) {
				t.Errorf("%s (image %v): the ledger %s does not name the %s cover (%v)", style, withImage, ledger, drawn, err)
			}
			if bytes.Contains(pdf, []byte("custom covers are not drawn here")) {
				t.Errorf("%s: the custom cover's code reached the render", style)
			}

			page := renderPNGsAt(t, typst, d.Canonical(), FormReport, longReport, thumbnailPPI, true)[0]
			switch drawn {
			case "band", "plate":
				textOn(t, style, page, d.Accent)
			case "split":
				textOn(t, style, page, d.Secondary)
			}
			if drawn == "plate" {
				// The image is drawn: a column of its pixels down the top
				// half is neither white nor the accent.
				b := page.Bounds()
				if n := inkIn(page, image.Rect(b.Min.X, b.Min.Y, b.Max.X, b.Min.Y+b.Dy()/4)); n < b.Dx()*b.Dy()/8 {
					t.Errorf("plate: the top of the cover has %d inked pixels, the image is not drawn", n)
				}
			}
		}
	}
}

// textOn holds every pixel inside the largest box of colour to white or the
// colour itself: the text on it is white, and nothing darker is drawn
// there.
func textOn(t *testing.T, style string, page image.Image, colour string) {
	t.Helper()
	cr, cg, cb := hexRGB(t, colour)
	rgb := func(x, y int) (uint32, uint32, uint32) {
		r, g, bl, _ := page.At(x, y).RGBA()
		return r >> 8, g >> 8, bl >> 8
	}
	isColour := func(x, y int) bool { r, g, bl := rgb(x, y); return near(r, cr) && near(g, cg) && near(bl, cb) }
	b := page.Bounds()
	x0, y0, x1, y1 := b.Max.X, b.Max.Y, -1, -1
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if isColour(x, y) {
				x0, y0, x1, y1 = min(x0, x), min(y0, y), max(x1, x), max(y1, y)
			}
		}
	}
	if x1 < 0 {
		t.Fatalf("%s: no %s panel drawn", style, colour)
	}
	white, dark := 0, 0
	for y := y0 + 2; y < y1-1; y++ {
		for x := x0 + 2; x < x1-1; x++ {
			r, g, bl := rgb(x, y)
			if r > 240 && g > 240 && bl > 240 {
				white++
			}
			if luma(r, g, bl) < luma(cr, cg, cb)-8 {
				dark++
			}
		}
	}
	if white == 0 || dark > 0 {
		t.Errorf("%s: inside the %s panel %d pixels are white and %d darker than it: its text is not white", style, colour, white, dark)
	}
}

// TestRealRenderCoverWhen: the design says when a cover is drawn: never,
// always for a titled report, or long for one past the threshold; a memo
// and a letter never get one.
func TestRealRenderCoverWhen(t *testing.T) {
	typst := realTypst(t)
	short := "# A titled report\n\nOne paragraph.\n"
	for _, c := range []struct {
		when, md, form string
		cover          bool
	}{
		{"long", longReport, FormReport, true},
		{"long", short, FormReport, false},
		{"always", longReport, FormReport, true},
		{"always", short, FormReport, true},
		{"never", longReport, FormReport, false},
		{"never", short, FormReport, false},
		{"always", longReport, FormMemo, false},
		{"always", longReport, FormLetter, false},
		{"always", strings.Replace(longReport, "# A long titled", "A long titled", 1), FormReport, false},
	} {
		d := branded(t, "classic")
		d.Cover.When = c.when
		pages := renderPNGs(t, typst, d.Canonical(), c.form, c.md)
		d.Cover.When = "never"
		without := renderPNGs(t, typst, d.Canonical(), c.form, c.md)
		if got := len(pages) - len(without); (got == 1) != c.cover || got > 1 {
			t.Errorf("%s %s %.20q: %d pages, %d with no cover; cover %v", c.when, c.form, c.md, len(pages), len(without), c.cover)
		}
	}
}

// TestRealRenderTheCoverImage: the plate cover draws the design's cover
// image from the server's re-encoding of it, written as cover.png or
// cover.jpg, and a design without one draws as band with no file.
func TestRealRenderTheCoverImage(t *testing.T) {
	d := branded(t, "field")
	if b, img, err := renderCoverImage(d); b != nil || img != nil || err != nil {
		t.Errorf("no cover image = (%d bytes, %+v, %v)", len(b), img, err)
	}
	d.CoverImage = reportdesign.ImageOf(coverPNG(t), "png")
	b, img, err := renderCoverImage(d)
	if err != nil || img == nil || img.Path != "cover.png" || img.Width != 400 || img.Height != 300 || !bytes.HasPrefix(b, pngMagic) {
		t.Fatalf("a PNG cover image = (%d bytes, %+v, %v)", len(b), img, err)
	}
	if coverStyle(d, img) != "plate" || coverStyle(d, nil) != "band" {
		t.Errorf("plate with an image draws %s, without %s", coverStyle(d, img), coverStyle(d, nil))
	}
	var jpg bytes.Buffer
	src, err := png.Decode(bytes.NewReader(coverPNG(t)))
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&jpg, src, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	d.CoverImage = reportdesign.ImageOf(jpg.Bytes(), "jpeg")
	b, img, err = renderCoverImage(d)
	if err != nil || img == nil || img.Path != "cover.jpg" || img.Width != 400 || img.Height != 300 || !bytes.HasPrefix(b, jpegMagic) {
		t.Fatalf("a JPEG cover image = (%d bytes, %+v, %v)", len(b), img, err)
	}
	typst := realTypst(t)
	d.Cover.When = "always"
	page := renderPNGsAt(t, typst, d.Canonical(), FormReport, longReport, thumbnailPPI, true)[0]
	bounds := page.Bounds()
	if n := inkIn(page, image.Rect(bounds.Min.X, bounds.Min.Y, bounds.Max.X, bounds.Min.Y+bounds.Dy()/4)); n < bounds.Dx()*bounds.Dy()/8 {
		t.Errorf("the JPEG cover image is not drawn: %d inked pixels at the top", n)
	}
}
