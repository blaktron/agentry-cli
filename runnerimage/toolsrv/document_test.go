package toolsrv

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/runnerimage/docpages"
	"github.com/blaktron/agentry-cli/runnerimage/reportdesign"
)

// pagePNG is a w by h white page with a dark green rectangle at r, in
// the page's own pixels.
func pagePNG(t *testing.T, w, h int, r image.Rectangle) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.NRGBA{255, 255, 255, 255}
			if image.Pt(x, y).In(r) {
				c = color.NRGBA{11, 93, 75, 255}
			}
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// stageDocument writes st, and for a PDF its pages and their texts, as the
// CLI stages a document under the inputs folder: each view 200 by 280,
// each full page twice that, a logo at 20,20 40x20 of the view.
func stageDocument(t *testing.T, spec *Spec, st docpages.Staged) {
	t.Helper()
	b, _ := json.Marshal(st)
	mustWrite(t, filepath.Join(spec.InputsDir, filepath.FromSlash(docpages.StagedFile)), b)
	for n := 1; n <= st.Rendered; n++ {
		mustWrite(t, filepath.Join(spec.InputsDir, filepath.FromSlash(docpages.ViewFile(n))), pagePNG(t, 200, 280, image.Rect(20, 20, 60, 40)))
		mustWrite(t, filepath.Join(spec.InputsDir, filepath.FromSlash(docpages.FullFile(n))), pagePNG(t, 400, 560, image.Rect(40, 40, 120, 80)))
	}
	for n := 1; n <= st.Texts; n++ {
		mustWrite(t, filepath.Join(spec.InputsDir, filepath.FromSlash(docpages.TextFile(n))), []byte("Harbour & Vale annual review, page text "+string(rune('0'+n))+"\n"))
	}
	for n := 1; n <= st.Media; n++ {
		mustWrite(t, filepath.Join(spec.InputsDir, filepath.FromSlash(docpages.MediaFileOf(n))), pagePNG(t, 60, 30, image.Rect(0, 0, 60, 30)))
	}
}

func documentSpec(t *testing.T) *Spec {
	t.Helper()
	spec := designSpec(t)
	spec.Design.DocumentLedger = filepath.Join(spec.OutDir, "receipts", "document.ndjson")
	return spec
}

var pdfStaged = docpages.Staged{Kind: docpages.KindPDF, Name: "annual-review.pdf", Pages: 14, Rendered: 3, Texts: 3,
	Fonts:   []docpages.Font{{Name: "SourceSerif4Roman-Regular"}, {Name: "Inter-Regular"}},
	Colours: []docpages.Colour{{Hex: "#0b5d4b", Share: 41}, {Hex: "#d97a1e", Share: 6}}}

func ledgerEntries(t *testing.T, spec *Spec) []DocumentEntry {
	t.Helper()
	b, err := os.ReadFile(spec.Design.DocumentLedger)
	if err != nil {
		t.Fatal(err)
	}
	var out []DocumentEntry
	for line := range strings.Lines(string(b)) {
		var e DocumentEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// TestDocumentPagesShowsAPageWithWhatTheDocumentDeclares: a page is an
// image item beside the page's text, the fonts and the colours, as the
// person's own input; each view is recorded, and nothing is written
// under reports/.
func TestDocumentPagesShowsAPageWithWhatTheDocumentDeclares(t *testing.T) {
	spec := documentSpec(t)
	stageDocument(t, spec, pdfStaged)
	s := newTestServer(t, spec)
	items, isErr := callForItems(t, s, ToolDocumentPages, map[string]any{"page": 2})
	if isErr || len(items) != 2 {
		t.Fatalf("document_pages = %v (error %v), want its text and an image", items, isErr)
	}
	if items[0]["type"] != "resource" {
		t.Errorf("the text is %v, want the person's input", items[0]["type"])
	}
	text := items[0]["resource"].(map[string]any)["text"].(string)
	for _, want := range []string{"1 of 12 (11 left)", "page 2 of the person's document \"annual-review.pdf\"", "200 by 280 PNG at 110 dpi", "a PDF of 14 pages; pages 1 to 3 were rendered",
		"SourceSerif4Roman-Regular, Inter-Regular", "#0b5d4b (41%)", "page text 2", "designed without looking at the document"} {
		if !strings.Contains(text, want) {
			t.Errorf("the answer lacks %q:\n%s", want, text)
		}
	}
	if items[1]["type"] != "image" {
		t.Fatalf("image item %v", items[1])
	}
	b, _ := base64.StdEncoding.DecodeString(items[1]["data"].(string))
	staged, _ := os.ReadFile(filepath.Join(spec.InputsDir, filepath.FromSlash(docpages.ViewFile(2))))
	if !bytes.Equal(b, staged) {
		t.Error("the image is not the staged view of page 2")
	}
	if e := ledgerEntries(t, spec); len(e) != 1 || e[0].Tool != ToolDocumentPages || e[0].N != 1 || e[0].Page != 2 || e[0].SHA256 == "" {
		t.Errorf("ledger %+v", e)
	}
	if entries, _ := os.ReadDir(filepath.Join(spec.OutDir, "reports")); len(entries) != 0 {
		t.Errorf("reports/ holds %d files", len(entries))
	}
	for _, args := range []map[string]any{{"page": 4}, {"page": 0, "media": 1}, {"page": 1, "media": 1}, {"pages": "1-3"}} {
		if got, isErr := s.toolDocumentPages(rawArgs(t, args)); !isErr {
			t.Errorf("document_pages(%v) = %q", args, got)
		}
	}
	s.documentViews = MaxDocumentViews
	if got, isErr := s.toolDocumentPages(rawArgs(t, map[string]any{"page": 1})); !isErr || !strings.Contains(got, "work from what you saw") {
		t.Errorf("a view past the cap: %q", got)
	}
}

// TestADocumentThatCouldNotBeReadIsAKindOfFailure: the tools answer the
// kind, as an error the run goes on past, and nothing of a decoder.
func TestADocumentThatCouldNotBeReadIsAKindOfFailure(t *testing.T) {
	spec := documentSpec(t)
	s := newTestServer(t, spec)
	if got, isErr := s.toolDocumentPages(rawArgs(t, map[string]any{})); !isErr || !strings.Contains(got, "given no document") {
		t.Errorf("no document: %q", got)
	}
	stageDocument(t, spec, docpages.Staged{Kind: docpages.KindPDF, Name: "broken.pdf", Failure: docpages.FailureMalformed})
	for _, call := range []func() (string, bool){
		func() (string, bool) { return s.toolDocumentPages(rawArgs(t, map[string]any{"page": 1})) },
		func() (string, bool) {
			return s.toolCropLogo(rawArgs(t, map[string]any{"page": 1, "x": 1, "y": 1, "width": 20, "height": 20}))
		},
	} {
		got, isErr := call()
		if !isErr || !strings.Contains(got, "could not be read as a PDF or a Word document") || !strings.Contains(got, "Design from the brief") {
			t.Errorf("a malformed document: %q", got)
		}
	}
	// A staged record past its rules is the tool's failure, never shown.
	mustWrite(t, filepath.Join(spec.InputsDir, filepath.FromSlash(docpages.StagedFile)), []byte(`{"kind":"pdf","name":"x","rendered":1,"failure":"Syntax Error: xref"}`))
	if got, isErr := s.toolDocumentPages(rawArgs(t, map[string]any{})); !isErr || strings.Contains(got, "Syntax Error") {
		t.Errorf("a record past its rules: %q", got)
	}
}

// TestAWordDocumentAnswersItsTextAndPictures: no page is rendered; the
// text answers alone, and a picture is an image.
func TestAWordDocumentAnswersItsTextAndPictures(t *testing.T) {
	spec := documentSpec(t)
	stageDocument(t, spec, docpages.Staged{Kind: docpages.KindDOCX, Name: "letterhead.docx", Texts: 1, Media: 2,
		Fonts:   []docpages.Font{{Name: "Fraunces", Role: "headings"}},
		Colours: []docpages.Colour{{Hex: "#d97a1e", Role: "accent1"}},
		Headers: []string{"Harbour & Vale"}, Footers: []string{"Confidential"}})
	s := newTestServer(t, spec)
	items, isErr := callForItems(t, s, ToolDocumentPages, map[string]any{})
	if isErr || len(items) != 1 {
		t.Fatalf("document_pages = %v, %v", items, isErr)
	}
	text := items[0]["resource"].(map[string]any)["text"].(string)
	for _, want := range []string{"Its layout is not rendered", "Fraunces (headings)", "accent1 #d97a1e", "running header: Harbour & Vale", "running footer: Confidential", "2 pictures", "The text of the document"} {
		if !strings.Contains(text, want) {
			t.Errorf("the answer lacks %q:\n%s", want, text)
		}
	}
	if s.documentViews != 0 {
		t.Error("a text answer counted as an image")
	}
	if items, isErr := callForItems(t, s, ToolDocumentPages, map[string]any{"media": 2}); isErr || len(items) != 2 || items[1]["type"] != "image" {
		t.Errorf("media 2 = %v, %v", items, isErr)
	}
	if got, isErr := s.toolDocumentPages(rawArgs(t, map[string]any{"page": 1})); !isErr {
		t.Errorf("a page of a Word document = %q", got)
	}
}

// TestCropLogoTakesTheBoxFromTheFullPage: the crop comes from the page
// rendered at twice the view's resolution, becomes the logo previews
// draw, and is recorded with its box; a person's own logo is never
// replaced.
func TestCropLogoTakesTheBoxFromTheFullPage(t *testing.T) {
	spec := documentSpec(t)
	stageDocument(t, spec, pdfStaged)
	s := newTestServer(t, spec)
	items, isErr := callForItems(t, s, ToolCropLogo, map[string]any{"page": 1, "x": 18, "y": 18, "width": 44, "height": 24})
	if isErr || len(items) != 2 || items[1]["type"] != "image" {
		t.Fatalf("crop_logo = %v, %v", items, isErr)
	}
	b, _ := base64.StdEncoding.DecodeString(items[1]["data"].(string))
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width != 88 || cfg.Height != 48 {
		t.Errorf("the crop is %+v (%v), want the box at twice the view's pixels", cfg, err)
	}
	if s.docLogo == (reportdesign.Logo{}) {
		t.Fatal("the crop is not the logo")
	}
	d, err := s.withAssets(reportdesign.Default())
	if err != nil || d.Logo != s.docLogo {
		t.Errorf("previews do not draw the crop (%v)", err)
	}
	e := ledgerEntries(t, spec)
	if len(e) != 1 || e[0].Tool != ToolCropLogo || e[0].Page != 1 || e[0].Box == nil || *e[0].Box != (docpages.Box{X: 18, Y: 18, Width: 44, Height: 24}) {
		t.Errorf("ledger %+v", e)
	}
	for _, args := range []map[string]any{{"page": 1}, {"page": 9, "x": 1, "y": 1, "width": 20, "height": 20}, {"page": 1, "x": 500, "y": 500, "width": 20, "height": 20}, {"page": 1, "x": 0, "y": 0, "width": 4, "height": 4}, {"x": 1}} {
		if got, isErr := s.toolCropLogo(rawArgs(t, args)); !isErr {
			t.Errorf("crop_logo(%v) = %q", args, got)
		}
	}

	given := pdfStaged
	given.LogoGiven = true
	stageDocument(t, spec, given)
	if got, isErr := s.toolCropLogo(rawArgs(t, map[string]any{"page": 1, "x": 18, "y": 18, "width": 44, "height": 24})); !isErr || !strings.Contains(got, "logo of their own") {
		t.Errorf("a crop over the person's logo: %q", got)
	}
}

// TestTakePictureTakesArtworkForTheCover: a box of a page becomes the
// cover image previews draw, and pictures fill the custom cover's slots
// in order, a slot taken again replaced; each take is recorded with what
// it was taken as, and what cannot be kept is refused.
func TestTakePictureTakesArtworkForTheCover(t *testing.T) {
	spec := documentSpec(t)
	st := pdfStaged
	st.Media, st.MediaPages = 2, []int{1, 1}
	stageDocument(t, spec, st)
	s := newTestServer(t, spec)
	items, isErr := callForItems(t, s, ToolTakePicture, map[string]any{"as": AsCoverImage, "page": 1, "x": 0, "y": 0, "width": 200, "height": 100})
	if isErr || len(items) != 2 || items[1]["type"] != "image" {
		t.Fatalf("take_picture = %v, %v", items, isErr)
	}
	if s.docCoverImage == (reportdesign.Image{}) {
		t.Fatal("the picture is not the cover image")
	}
	raw, _, _ := s.docCoverImage.Decode()
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(raw)); err != nil || cfg.Width != 400 || cfg.Height != 200 {
		t.Errorf("the cover image is %+v (%v), want the box at twice the view's pixels", cfg, err)
	}
	d, err := s.withAssets(reportdesign.Default())
	if err != nil || d.CoverImage != s.docCoverImage {
		t.Errorf("previews do not draw the cover image (%v)", err)
	}
	for _, args := range []map[string]any{{"as": AsCover, "media": 1}, {"as": AsCover, "media": 2}, {"as": AsCover, "slot": 1, "media": 2}} {
		if got, isErr := s.toolTakePicture(rawArgs(t, args)); isErr {
			t.Fatalf("take_picture(%v) = %q", args, got)
		}
	}
	if len(s.docCoverPictures) != 2 || s.docCoverPictures[0] != s.docCoverPictures[1] {
		t.Errorf("the cover's pictures are %d, want two, the first replaced by the second", len(s.docCoverPictures))
	}
	images, err := s.coverImages()
	if err != nil || len(images) != 2 {
		t.Errorf("a custom cover's preview draws %d images (%v)", len(images), err)
	}
	e := ledgerEntries(t, spec)
	if len(e) != 4 || e[0].Tool != ToolTakePicture || e[0].As != AsCoverImage || e[0].Box == nil || e[3].As != AsCover || e[3].Slot != 1 || e[3].Media != 2 || e[3].Box != nil {
		t.Errorf("ledger %+v", e)
	}
	for _, args := range []map[string]any{
		{"media": 1},
		{"as": "logo", "media": 1},
		{"as": AsCoverImage, "slot": 2, "media": 1},
		{"as": AsCover, "slot": 4, "media": 1},
		{"as": AsCover, "page": 1},
		{"as": AsCover, "media": 3},
		{"as": AsCover, "page": 1, "media": 1},
	} {
		if got, isErr := s.toolTakePicture(rawArgs(t, args)); !isErr {
			t.Errorf("take_picture(%v) = %q", args, got)
		}
	}
}

// TestCropLogoTrimsAndTakesAPDFPicture: a generous box keeps the logo
// with a small margin, and a PDF's picture is taken whole.
func TestCropLogoTrimsAndTakesAPDFPicture(t *testing.T) {
	spec := documentSpec(t)
	st := pdfStaged
	st.Media, st.MediaPages = 1, []int{1}
	stageDocument(t, spec, st)
	s := newTestServer(t, spec)
	// The view's logo is at 20,20 40x20; the box takes in 80 by 60.
	items, isErr := callForItems(t, s, ToolCropLogo, map[string]any{"page": 1, "x": 5, "y": 5, "width": 80, "height": 60})
	if isErr || len(items) != 2 {
		t.Fatalf("crop_logo = %v, %v", items, isErr)
	}
	b, _ := base64.StdEncoding.DecodeString(items[1]["data"].(string))
	if cfg, err := png.DecodeConfig(bytes.NewReader(b)); err != nil || cfg.Width != 88 || cfg.Height != 48 {
		t.Errorf("the crop is %+v (%v), want the 80 by 40 logo with a 4 pixel margin", cfg, err)
	}
	if got, isErr := s.toolCropLogo(rawArgs(t, map[string]any{"media": 1})); isErr {
		t.Errorf("crop_logo of a PDF's picture = %q", got)
	}
	items, _ = callForItems(t, s, ToolDocumentPages, map[string]any{"media": 1})
	text := items[0]["resource"].(map[string]any)["text"].(string)
	if !strings.Contains(text, "picture 1 of 1 (first drawn on page 1)") || !strings.Contains(text, "Its pages draw 1 picture (1 on page 1)") {
		t.Errorf("document_pages of a PDF's picture:\n%s", text)
	}
}
