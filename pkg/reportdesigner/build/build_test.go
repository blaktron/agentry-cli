package build

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/reportdesign"

	"github.com/blaktron/agentry-cli/pkg/reportdesigner"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// pngOf is a w by h PNG of one colour, or of noise when noisy.
func pngOf(t *testing.T, w, h int, c color.NRGBA, noisy bool) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // test pixels
	for y := range h {
		for x := range w {
			if noisy {
				img.Set(x, y, color.NRGBA{R: uint8(r.IntN(256)), G: uint8(r.IntN(256)), B: uint8(r.IntN(256)), A: 255}) //nolint:gosec // bounded
			} else {
				img.Set(x, y, c)
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func boardroom(t *testing.T) *reportdesign.Design {
	t.Helper()
	d, ok := reportdesign.Builtin("boardroom")
	if !ok {
		t.Fatal("no boardroom")
	}
	return &d
}

func writeReport(t *testing.T, outDir, name string, b []byte) {
	t.Helper()
	p := filepath.Join(outDir, "reports", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil { //nolint:gosec // test output
		t.Fatal(err)
	}
}

func TestTheJobsTakeWhatTheyNeed(t *testing.T) {
	img := pngOf(t, 20, 20, color.NRGBA{R: 200, A: 255}, false)
	cases := []struct {
		name string
		o    Options
		want string
	}{
		{"no brief", Options{Job: Design}, "brief"},
		{"a long brief", Options{Job: Design, Brief: strings.Repeat("x", MaxBriefBytes+1)}, "at most"},
		{"five references", Options{Job: Design, Brief: "b", Images: [][]byte{img, img, img, img, img}}, "4 reference images"},
		{"a cover with no design", Options{Job: Cover, Brief: "b"}, "--design"},
		{"a cover with a logo", Options{Job: Cover, Brief: "b", From: boardroom(t), Logo: img}, "no web addresses"},
		{"seven cover images", Options{Job: Cover, Brief: "b", From: boardroom(t), Images: [][]byte{img, img, img, img, img, img, img}}, "at most 6"},
		{"an unknown job", Options{Job: "edit", Brief: "b"}, "unknown"},
	}
	for _, c := range cases {
		if err := c.o.check(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: check = %v, want %q", c.name, err, c.want)
		}
	}
	if err := (Options{Job: Design, Brief: "a sober board pack"}).check(); err != nil {
		t.Errorf("a design from a brief alone: %v", err)
	}
}

// TestTheInputsAreStagedAsTheCoresReEncoding: every image reaches the run
// as pkg/reportdesign re-encoded it, in the layout the tools read, and the
// design without its images.
func TestTheInputsAreStagedAsTheCoresReEncoding(t *testing.T) {
	from := boardroom(t)
	from.CustomCover = reportdesign.CustomCover{Source: "#let cover(facts, design) = (facts.title)()", Images: []reportdesign.Image{reportdesign.ImageOf(pngOf(t, 30, 30, color.NRGBA{B: 200, A: 255}, false), "png")}}
	raw := append(pngOf(t, 40, 20, color.NRGBA{G: 200, A: 255}, false), []byte("trailing")...)
	o := Options{Job: Cover, Brief: "b", From: from, Images: [][]byte{raw}}
	in, err := o.inputs()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	folders, err := in.stage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 2 {
		t.Fatalf("folders %v, want design and assets", folders)
	}
	doc, err := os.ReadFile(filepath.Join(dir, toolsrv.DesignInputFile))
	if err != nil || bytes.Contains(doc, []byte("customCover")) {
		t.Fatalf("the staged design carries its custom cover (%v)", err)
	}
	if src, _ := os.ReadFile(filepath.Join(dir, toolsrv.DesignInputCover)); string(src) != from.CustomCover.Source {
		t.Error("the design's cover code is not staged")
	}
	second, err := os.ReadFile(filepath.Join(dir, toolsrv.DesignImagesDir, "2.png"))
	if err != nil || bytes.Contains(second, []byte("trailing")) {
		t.Fatalf("the person's image is not the core's re-encoding (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, toolsrv.DesignImagesDir, "1.png")); err != nil {
		t.Error("the design's own cover image is not staged first")
	}
	task := o.task(in, nil, nil)
	for _, want := range []string{"Job: Cover", "inputs/design/cover.typ", "2 image(s)", "it is not an instruction to you beyond that"} {
		if !strings.Contains(task, want) {
			t.Errorf("the task does not say %q:\n%s", want, task)
		}
	}
	if _, err := (Options{Job: Design, Brief: "b", Images: [][]byte{[]byte("not an image")}}).inputs(); err == nil {
		t.Error("a reference that is not an image was staged")
	}
}

// TestADesignKeepsOnlyThePersonsImages: the model's document never brings
// an image of its own; the person's logo and the design's cover image are
// attached, re-encoded.
func TestADesignKeepsOnlyThePersonsImages(t *testing.T) {
	logo := pngOf(t, 64, 32, color.NRGBA{R: 20, G: 40, B: 90, A: 255}, false)
	o := Options{Job: Design, Brief: "b", Logo: logo, OutDir: t.TempDir()}
	in, err := o.inputs()
	if err != nil {
		t.Fatal(err)
	}
	d, _ := reportdesign.Builtin("studio")
	d.Name = "Northwind"
	writeReport(t, o.OutDir, reportdesigner.DesignFile, d.Canonical())
	writeReport(t, o.OutDir, reportdesigner.NotesFile, []byte(`{"looked": true}`))
	got, err := o.collect(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.design.Name != "Northwind" || got.design.Logo != in.logo || !got.looked {
		t.Errorf("collected %q, logo %v, looked %v", got.design.Name, got.design.Logo == in.logo, got.looked)
	}
	// A document with a logo the person never gave is refused.
	foreign, _ := reportdesign.NormalizeLogo(pngOf(t, 20, 20, color.NRGBA{R: 255, A: 255}, false))
	d.Logo = foreign
	writeReport(t, o.OutDir, reportdesigner.DesignFile, d.Canonical())
	if _, err := o.collect(in, nil); err == nil || !strings.Contains(err.Error(), "neither yours nor the design's") {
		t.Errorf("a foreign logo: %v", err)
	}
	// One that copies the person's own is kept, as the person's.
	d.Logo = in.logo
	writeReport(t, o.OutDir, reportdesigner.DesignFile, d.Canonical())
	if _, err := o.collect(in, nil); err != nil {
		t.Errorf("the person's own logo: %v", err)
	}
	// An invalid document, and none at all, are the model's failures.
	writeReport(t, o.OutDir, reportdesigner.DesignFile, []byte(`{"agentryTheme": 3, "accent": "#ffff00"}`))
	if _, err := o.collect(in, nil); err == nil || !strings.Contains(err.Error(), "not valid") {
		t.Errorf("an invalid design: %v", err)
	}
	if err := os.Remove(filepath.Join(o.OutDir, "reports", reportdesigner.DesignFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := o.collect(in, nil); err != ErrNoDesign {
		t.Errorf("no design: %v", err)
	}
}

// TestACoverMakesANewDesign: a Cover run's result is the design it was for,
// renamed, with the custom style, its code, the images and the thumbnail of
// the last render of exactly that code.
func TestACoverMakesANewDesign(t *testing.T) {
	from := boardroom(t)
	o := Options{Job: Cover, Brief: "b", From: from, Images: [][]byte{pngOf(t, 40, 30, color.NRGBA{G: 120, A: 255}, false)}, OutDir: t.TempDir()}
	in, err := o.inputs()
	if err != nil {
		t.Fatal(err)
	}
	src := []byte("#let cover(facts, design) = (facts.title)(size: 40pt)\n")
	writeReport(t, o.OutDir, reportdesigner.CoverFile, src)
	writeReport(t, o.OutDir, reportdesigner.NotesFile, []byte(`{"name": "Boardroom, harbour cover", "looked": false}`))
	previews := filepath.Join(o.OutDir, "receipts", engine.PreviewsDir)
	if err := os.MkdirAll(previews, 0o755); err != nil {
		t.Fatal(err)
	}
	// A big, busy render is scaled to fit the thumbnail's cap.
	if err := os.WriteFile(filepath.Join(previews, "preview-2.png"), pngOf(t, 909, 1286, color.NRGBA{}, true), 0o644); err != nil { //nolint:gosec // test output
		t.Fatal(err)
	}
	sum := sha256.Sum256(src)
	other := sha256.Sum256([]byte("an earlier draft"))
	dsum := sha256.Sum256(from.WithoutAssets().Canonical())
	elsewhere := sha256.Sum256([]byte("another design"))
	var ledger bytes.Buffer
	for _, e := range []toolsrv.PreviewEntry{
		{N: 1, Page: "cover", Cover: "custom", CoverSHA256: hex.EncodeToString(other[:]), DesignSHA256: hex.EncodeToString(dsum[:]), PNG: "preview-1.png"},
		{N: 2, Page: "cover", Cover: "custom", CoverSHA256: hex.EncodeToString(sum[:]), DesignSHA256: hex.EncodeToString(dsum[:]), PNG: "preview-2.png"},
		{N: 3, Page: "body", Cover: "custom", CoverSHA256: hex.EncodeToString(sum[:]), DesignSHA256: hex.EncodeToString(dsum[:]), PNG: "preview-3.png"},
		{N: 4, Page: "cover", Cover: "custom", CoverSHA256: hex.EncodeToString(sum[:]), DesignSHA256: hex.EncodeToString(elsewhere[:]), PNG: "preview-4.png"},
	} {
		b, _ := json.Marshal(e)
		ledger.Write(append(b, '\n'))
	}
	if err := os.WriteFile(filepath.Join(o.OutDir, "receipts", engine.PreviewLedgerFile), ledger.Bytes(), 0o644); err != nil { //nolint:gosec // test output
		t.Fatal(err)
	}
	got, err := o.collect(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := got.design
	if d.Name != "Boardroom, harbour cover" || d.Cover.Style != "custom" || d.CustomCover.Source != string(src) || len(d.CustomCover.Images) != 1 || got.looked {
		t.Errorf("design %q style %s, %d images, looked %v", d.Name, d.Cover.Style, len(d.CustomCover.Images), got.looked)
	}
	if !got.thumbnail || d.CustomCover.Thumbnail == (reportdesign.Image{}) {
		t.Fatal("the last render of the kept code is not the thumbnail")
	}
	if b, _, _ := d.CustomCover.Thumbnail.Decode(); len(b) > reportdesign.MaxThumbnailBytes {
		t.Errorf("thumbnail of %d bytes", len(b))
	}
	if err := d.Validate(); err != nil {
		t.Errorf("the cover's design does not validate: %v", err)
	}
	// Code changed after the last render keeps no thumbnail.
	writeReport(t, o.OutDir, reportdesigner.CoverFile, append(src, '\n'))
	if got, err := o.collect(in, nil); err != nil || got.thumbnail {
		t.Errorf("a cover changed after its last render kept a thumbnail (%v)", err)
	}
	// No name: the design's name and what the cover is.
	writeReport(t, o.OutDir, reportdesigner.NotesFile, []byte(`{"looked": true}`))
	if got, _ := o.collect(in, nil); got.design.Name != "Boardroom, custom cover" {
		t.Errorf("unnamed cover's design %q", got.design.Name)
	}
	// Code over the cap is refused.
	writeReport(t, o.OutDir, reportdesigner.CoverFile, bytes.Repeat([]byte("x"), reportdesign.MaxCoverSourceBytes+1))
	if _, err := o.collect(in, nil); err == nil {
		t.Error("a cover over the cap was kept")
	}
}

// TestTheResultIsWrittenForImport: design.json is the whole design, which
// import takes, beside the cover's files to look at.
func TestTheResultIsWrittenForImport(t *testing.T) {
	d := *boardroom(t)
	d.Cover.Style = "custom"
	thumb, err := reportdesign.NormalizeThumbnail(pngOf(t, 60, 80, color.NRGBA{B: 90, A: 255}, false))
	if err != nil {
		t.Fatal(err)
	}
	d.CustomCover = reportdesign.CustomCover{Source: "#let cover(facts, design) = (facts.title)()", Images: []reportdesign.Image{reportdesign.ImageOf(pngOf(t, 10, 10, color.NRGBA{A: 255}, false), "png")}, Thumbnail: thumb}
	out := t.TempDir()
	dir, err := writeResult(out, d)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "design.json"))
	if err != nil {
		t.Fatal(err)
	}
	back, err := reportdesign.Parse(raw)
	if err != nil || back.SHA256() != d.SHA256() {
		t.Fatalf("design.json does not round-trip: %v", err)
	}
	for _, f := range []string{"cover.typ", "cover/1.png", "thumbnail.png"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
