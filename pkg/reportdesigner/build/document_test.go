package build

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/reportdesign"

	"github.com/blaktron/agentry-cli/pkg/reportdesigner"
	"github.com/blaktron/agentry-cli/pkg/runner/clamdb"
	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
	"github.com/blaktron/agentry-cli/runnerimage/docpages"
	"github.com/blaktron/agentry-cli/runnerimage/docpages/docpagestest"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// pageWithLogo is a white w by h page with a green logo block at r.
func pageWithLogo(t *testing.T, w, h int, r image.Rectangle) []byte {
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

// fakeDecode replaces the decode step's calls for one test: the decode
// answers file as the report's one line, and the pages role answers
// pages (nil plays the real role over a Word document).
func fakeDecode(t *testing.T, file runnerdecode.File, text string, pages *decode.Pages) *int {
	t.Helper()
	calls := 0
	saved := []any{ensureRunner, ensureDecode, ensureSignatures, runDecode, runPages}
	t.Cleanup(func() {
		ensureRunner = saved[0].(func(context.Context, dockerx.Client, Options, string, string, io.Writer) error)
		ensureDecode = saved[1].(func(context.Context, dockerx.Client, string, *policy.Policy, string, io.Writer) (decode.Image, error))
		ensureSignatures = saved[2].(func(context.Context, string, string, io.Writer) clamdb.DB)
		runDecode = saved[3].(func(context.Context, dockerx.Client, decode.Image, string, string, policy.Limits, io.Writer) (*decode.Result, error))
		runPages = saved[4].(func(context.Context, dockerx.Client, decode.Image, string, docpages.Kind, string, policy.Limits, io.Writer) (*decode.Pages, error))
	})
	ensureRunner = func(context.Context, dockerx.Client, Options, string, string, io.Writer) error { return nil }
	ensureDecode = func(context.Context, dockerx.Client, string, *policy.Policy, string, io.Writer) (decode.Image, error) {
		return decode.Image{Tag: "agentry-decode:test", Files: "ghcr.io/blaktron/agentry-files:test"}, nil
	}
	ensureSignatures = func(context.Context, string, string, io.Writer) clamdb.DB { return clamdb.DB{} }
	runDecode = func(_ context.Context, _ dockerx.Client, img decode.Image, in, out string, _ policy.Limits, _ io.Writer) (*decode.Result, error) {
		calls++
		if file.Output != "" {
			if err := os.WriteFile(filepath.Join(out, file.Output), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return &decode.Result{Report: runnerdecode.Report{Schema: runnerdecode.Schema, Files: []runnerdecode.File{file}}, Image: img, In: in, Out: out}, nil
	}
	runPages = func(_ context.Context, _ dockerx.Client, img decode.Image, doc string, kind docpages.Kind, out string, _ policy.Limits, _ io.Writer) (*decode.Pages, error) {
		calls++
		if pages != nil {
			return pages, nil
		}
		if _, err := docpages.Render(context.Background(), doc, kind, out, t.TempDir(), docpages.Tools{}); err != nil {
			t.Fatal(err)
		}
		return decode.CheckPages(out, kind)
	}
	return &calls
}

func documentOptions(t *testing.T, doc *Document) Options {
	t.Helper()
	return Options{Job: Design, Brief: "Reports like our annual review.", Document: doc, Home: t.TempDir(), Policy: policy.Default(), OutDir: t.TempDir()}
}

// TestADocumentIsCheckedBeforeTheRun: one PDF or .docx within its cap,
// by its own bytes; anything else, and a document on a Cover run, is
// refused before anything runs.
func TestADocumentIsCheckedBeforeTheRun(t *testing.T) {
	pdf := docpagestest.PDF(1, "0 0 0")
	if k, err := DocumentKind("review.pdf", pdf); err != nil || k != docpages.KindPDF {
		t.Errorf("a PDF: %v, %v", k, err)
	}
	if k, err := DocumentKind("letterhead.docx", docpagestest.Docx(t)); err != nil || k != docpages.KindDOCX {
		t.Errorf("a .docx: %v, %v", k, err)
	}
	for name, b := range map[string][]byte{
		"notes.txt":       []byte("plain text"),
		"deck.pptx":       docpagestest.Docx(t),
		"renamed.docx":    pdf,
		"big.pdf":         append(append([]byte{}, pdf...), make([]byte, MaxDocumentBytes)...),
		"letterhead.docm": docpagestest.Docx(t),
	} {
		if _, err := DocumentKind(name, b); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	o := documentOptions(t, &Document{Name: "notes.txt", Data: []byte("plain")})
	if err := o.check(); err == nil {
		t.Error("a text document passed the run's check")
	}
	o = documentOptions(t, &Document{Name: "review.pdf", Data: pdf})
	o.Job, o.From = Cover, boardroom(t)
	if err := o.check(); err == nil || !strings.Contains(err.Error(), "no document") {
		t.Errorf("a cover with a document: %v", err)
	}
}

// TestADocumentGoesThroughTheDecodeStep: the document is decoded and
// checked as any input is, then read by the pages role, and what was read
// is staged in the document tools' layout without the document itself.
func TestADocumentGoesThroughTheDecodeStep(t *testing.T) {
	calls := fakeDecode(t, runnerdecode.File{Path: "document.docx", Family: runnerdecode.FamilyDocument, Tool: "pandoc", Outcome: runnerdecode.OutcomeDecoded,
		Output: "document.docx.txt", MIME: "application/vnd.openxmlformats-officedocument.wordprocessingml.document"}, "# Annual Review\n\nOur year.\n", nil)
	o := documentOptions(t, &Document{Name: "Harbour letterhead.docx", Data: docpagestest.Docx(t)})
	work := t.TempDir()
	doc, err := o.readTheDocument(context.Background(), &dockerx.Fake{}, work, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Errorf("%d decode calls, want the decode step and the pages role", *calls)
	}
	var st docpages.Staged
	b, err := os.ReadFile(filepath.Join(filepath.Dir(doc.dir), filepath.FromSlash(docpages.StagedFile)))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if st.Kind != docpages.KindDOCX || st.Name != "Harbour letterhead.docx" || st.Media != 1 || st.Texts != 1 || st.Headers[0] != "Harbour & Vale Annual Review" || st.LogoGiven {
		t.Errorf("staged %+v", st)
	}
	text, _ := os.ReadFile(filepath.Join(filepath.Dir(doc.dir), filepath.FromSlash(docpages.TextFile(1))))
	if !strings.Contains(string(text), "Our year.") {
		t.Errorf("the text staged is %q", text)
	}
	_ = filepath.WalkDir(doc.dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (strings.HasSuffix(p, ".docx") || strings.HasSuffix(p, ".pdf")) {
			t.Errorf("the document itself was staged: %s", p)
		}
		return nil
	})
	var task strings.Builder
	documentTask(&task, doc)
	if !strings.Contains(task.String(), "document_pages") || !strings.Contains(task.String(), "no rendered page") {
		t.Errorf("task %q", task.String())
	}
	if line := doc.pages.PagesLine(); !strings.Contains(line, "no Word layout is rendered") {
		t.Errorf("receipt line %q", line)
	}
}

// TestTheDecodeStepRefusesWhatItWouldRefuseForAnyInput: a malware finding
// refuses the run, and a document whose content is another type is
// refused as usage; neither reaches the pages role.
func TestTheDecodeStepRefusesWhatItWouldRefuseForAnyInput(t *testing.T) {
	pdf := docpagestest.PDF(1, "0 0 0")
	for name, f := range map[string]runnerdecode.File{
		"found":     {Path: "document.pdf", Family: runnerdecode.FamilyPDF, Outcome: runnerdecode.OutcomeRefused, Why: runnerdecode.WhyFound, Check: runnerdecode.CheckFound, Signature: "Eicar-Test-Signature"},
		"not a PDF": {Path: "document.pdf", Family: runnerdecode.FamilyText, Outcome: runnerdecode.OutcomeText, MIME: "text/plain"},
	} {
		calls := fakeDecode(t, f, "", nil)
		o := documentOptions(t, &Document{Name: "review.pdf", Data: pdf})
		_, err := o.readTheDocument(context.Background(), &dockerx.Fake{}, t.TempDir(), io.Discard)
		want := engine.ExitUsage
		if name == "found" {
			want = engine.ExitPolicy
		}
		var classified *engine.ClassifiedError
		if !errors.As(err, &classified) || classified.Code != want {
			t.Errorf("%s: %v, want exit %d", name, err, want)
		}
		if *calls != 1 {
			t.Errorf("%s: the pages role ran", name)
		}
	}
}

// TestADocumentTheDecoderCouldNotReadIsStagedAsItsFailure: the run goes on
// with the brief, and the tools answer the kind.
func TestADocumentTheDecoderCouldNotReadIsStagedAsItsFailure(t *testing.T) {
	fakeDecode(t, runnerdecode.File{Path: "document.pdf", Family: runnerdecode.FamilyPDF, Tool: "pdftotext", Outcome: runnerdecode.OutcomeUndecodable, Reason: "pdftotext: Syntax Error", MIME: "application/pdf"}, "", nil)
	o := documentOptions(t, &Document{Name: "broken.pdf", Data: []byte("%PDF-1.7 garbage")})
	doc, err := o.readTheDocument(context.Background(), &dockerx.Fake{}, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if doc.staged.Failure != docpages.FailureMalformed {
		t.Errorf("staged %+v", doc.staged)
	}
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(doc.dir), filepath.FromSlash(docpages.StagedFile)))
	if strings.Contains(string(b), "Syntax Error") {
		t.Error("the decoder's words were staged")
	}
	var task strings.Builder
	documentTask(&task, doc)
	if !strings.Contains(task.String(), "could not be read") || !strings.Contains(task.String(), "Design from the brief") {
		t.Errorf("task %q", task.String())
	}
}

// pdfPages is a read PDF of one page: a 400 by 560 render with a logo
// block at 40,40 80x40, its view half that.
func pdfPages(t *testing.T) *decode.Pages {
	t.Helper()
	full := pageWithLogo(t, 400, 560, image.Rect(40, 40, 120, 80))
	view := pageWithLogo(t, 200, 280, image.Rect(20, 20, 60, 40))
	return &decode.Pages{Facts: docpages.Facts{Schema: docpages.Schema, Kind: docpages.KindPDF, Pages: 1, Rendered: 1}, Full: [][]byte{full}, View: [][]byte{view}}
}

// TestALogoCroppedFromTheDocumentIsCroppedAgainHere: the design keeps
// this process's own crop of the box the ledger records, never a file the
// run wrote; a person's own logo is never replaced; and the result holds
// no page of the document.
func TestALogoCroppedFromTheDocumentIsCroppedAgainHere(t *testing.T) {
	fakeDecode(t, runnerdecode.File{Path: "document.pdf", Family: runnerdecode.FamilyPDF, Tool: "pdftotext", Outcome: runnerdecode.OutcomeDecoded, Output: "document.pdf.txt", MIME: "application/pdf"}, "Harbour & Vale\fpage two", pdfPages(t))
	o := documentOptions(t, &Document{Name: "review.pdf", Data: docpagestest.PDF(1, "0 0 0")})
	doc, err := o.readTheDocument(context.Background(), &dockerx.Fake{}, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if doc.staged.Texts != 1 {
		t.Errorf("%d texts staged for one rendered page", doc.staged.Texts)
	}
	d, _ := reportdesign.Builtin("boardroom")
	writeReport(t, o.OutDir, reportdesigner.DesignFile, d.WithoutAssets().Canonical())
	box := docpages.Box{X: 18, Y: 18, Width: 44, Height: 24}
	ledger := filepath.Join(o.OutDir, "receipts", engine.DocumentLedgerFile)
	if err := os.MkdirAll(filepath.Dir(ledger), 0o755); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(toolsrv.DocumentEntry{Tool: toolsrv.ToolCropLogo, Page: 1, Box: &box, SHA256: "not what is kept"})
	if err := os.WriteFile(ledger, append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := o.inputs()
	if err != nil {
		t.Fatal(err)
	}
	got, err := o.collect(in, doc)
	if err != nil {
		t.Fatal(err)
	}
	if !got.logoFromDocument {
		t.Fatal("the crop was not kept as the logo")
	}
	want, err := docpages.CropLogo(doc.pages.Full[0], &box, 2)
	if err != nil {
		t.Fatal(err)
	}
	wantLogo, err := reportdesign.NormalizeLogo(want)
	if err != nil {
		t.Fatal(err)
	}
	if got.design.Logo != wantLogo {
		t.Error("the logo is not this process's crop of the box")
	}
	dir, err := writeResult(o.OutDir, got.design)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "design.json" {
		t.Errorf("the result folder holds %v, want design.json alone", entries)
	}

	// With the person's own logo, the crop is never taken.
	o.Logo = pngOf(t, 120, 60, color.NRGBA{200, 0, 0, 255}, false)
	doc.staged.LogoGiven = true
	in, err = o.inputs()
	if err != nil {
		t.Fatal(err)
	}
	got, err = o.collect(in, doc)
	if err != nil {
		t.Fatal(err)
	}
	if got.logoFromDocument || got.design.Logo != in.logo {
		t.Error("a crop replaced the person's own logo")
	}
}

// TestADesignFromADocumentKeepsItsCoverArtwork: the pictures the ledger
// records are taken again here, the last of each slot, as the cover image
// and as a custom cover's images; a cover.typ the run wrote becomes the
// design's custom cover, with the thumbnail of its last render.
func TestADesignFromADocumentKeepsItsCoverArtwork(t *testing.T) {
	pages := pdfPages(t)
	pages.Media = [][]byte{pngOf(t, 120, 60, color.NRGBA{11, 93, 75, 255}, false)}
	pages.Facts.Media, pages.Facts.MediaPages = []string{"media-1.png"}, []int{1}
	fakeDecode(t, runnerdecode.File{Path: "document.pdf", Family: runnerdecode.FamilyPDF, Tool: "pdftotext", Outcome: runnerdecode.OutcomeDecoded, Output: "document.pdf.txt", MIME: "application/pdf"}, "Harbour & Vale", pages)
	o := documentOptions(t, &Document{Name: "review.pdf", Data: docpagestest.PDF(1, "0 0 0")})
	doc, err := o.readTheDocument(context.Background(), &dockerx.Fake{}, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if doc.staged.Media != 1 || fmt.Sprint(doc.staged.MediaPages) != "[1]" {
		t.Fatalf("staged %+v", doc.staged)
	}
	d, _ := reportdesign.Builtin("boardroom")
	d.Cover.Style = "custom"
	written := d.WithoutAssets().Canonical()
	writeReport(t, o.OutDir, reportdesigner.DesignFile, written)
	src := []byte("#let cover(facts, design) = image(facts.images.at(0), alt: \"The band\")\n")
	writeReport(t, o.OutDir, reportdesigner.CoverFile, src)
	band := docpages.Box{X: 0, Y: 0, Width: 200, Height: 60}
	var lines []byte
	for _, e := range []toolsrv.DocumentEntry{
		{Tool: toolsrv.ToolTakePicture, Page: 1, Box: &band, As: toolsrv.AsCoverImage},
		{Tool: toolsrv.ToolTakePicture, Media: 1, As: toolsrv.AsCover, Slot: 1},
		{Tool: toolsrv.ToolTakePicture, Page: 1, Box: &band, As: toolsrv.AsCover, Slot: 2},
		{Tool: toolsrv.ToolTakePicture, Media: 1, As: toolsrv.AsCover, Slot: 2},
	} {
		line, _ := json.Marshal(e)
		lines = append(append(lines, line...), '\n')
	}
	ledger := filepath.Join(o.OutDir, "receipts", engine.DocumentLedgerFile)
	if err := os.MkdirAll(filepath.Dir(ledger), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger, lines, 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := o.inputs()
	if err != nil {
		t.Fatal(err)
	}
	got, err := o.collect(in, doc)
	if err != nil {
		t.Fatal(err)
	}
	if !got.coverImageFromDocument || !got.coverFromDocument || got.coverPictures != 2 {
		t.Fatalf("made %+v", got)
	}
	b, _, err := docpages.TakePicture(doc.pages.Full[0], &band, 2, reportdesign.MaxCoverImageSide)
	if err != nil {
		t.Fatal(err)
	}
	want, err := reportdesign.NormalizeCoverImage(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.design.CoverImage != want {
		t.Error("the cover image is not this process's take of the box")
	}
	cc := got.design.CustomCover
	if got.design.Cover.Style != "custom" || cc.Source != string(src) || len(cc.Images) != 2 || cc.Images[0] != cc.Images[1] {
		t.Errorf("the custom cover is %q with %d images", got.design.Cover.Style, len(cc.Images))
	}
	if err := got.design.Validate(); err != nil {
		t.Error(err)
	}

	// Without a cover.typ, the cover's pictures are dropped and the
	// design keeps its style.
	if err := os.Remove(filepath.Join(o.OutDir, "reports", reportdesigner.CoverFile)); err != nil {
		t.Fatal(err)
	}
	d.Cover.Style = "plate"
	writeReport(t, o.OutDir, reportdesigner.DesignFile, d.WithoutAssets().Canonical())
	if got, err = o.collect(in, doc); err != nil {
		t.Fatal(err)
	}
	if got.coverFromDocument || !got.design.CustomCover.IsZero() || got.design.Cover.Style != "plate" || got.design.CoverImage != want {
		t.Errorf("without a cover: %+v", got)
	}
}
