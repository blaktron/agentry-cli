// Package docpages is the document a Report Designer run is given to build a
// design around: one PDF, whose first pages poppler's page renderer turns into
// PNG images, or one Word document, whose theme part declares its fonts and
// colours and whose headers, footers and pictures are read as they are. The
// runner image's `pages` role does the reading in the decode image (Render),
// with no network, no capabilities and the document mounted read-only; the
// host holds what it wrote to Facts.Validate, re-encodes every image from its
// pixels and stages the result in the layout Staged names, which the tools
// server's document_pages and crop_logo read.
//
// This package is part of the embedded runner-image build context and so
// imports nothing outside the standard library.
package docpages

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Schema is the format of FactsFile.
const Schema = "pages/v1"

// The bounds of one document.
const (
	// MaxDocumentBytes caps the document as given.
	MaxDocumentBytes = 20 << 20
	// MaxPages is how many of a PDF's pages are rendered: the first twelve.
	MaxPages = 12
	// RenderPPI is the resolution a page is rendered at, which a logo is
	// cropped from; ViewPPI, half of it, is the page the model looks at.
	RenderPPI = 220
	ViewPPI   = RenderPPI / 2
	// MaxPageSide caps a rendered page's longer side in pixels: a page
	// larger than about sixteen inches is scaled to fit rather than
	// rendered at RenderPPI.
	MaxPageSide = 3600
	// MaxPageBytes caps one rendered page's PNG.
	MaxPageBytes = 24 << 20
	// MaxMedia and MaxMediaBytes cap a Word document's pictures.
	MaxMedia      = 8
	MaxMediaBytes = 4 << 20
	// MaxMediaSide caps a picture's width and height, checked before it is
	// decoded.
	MaxMediaSide = 4096
	// MaxFonts, MaxColours and MaxRunning cap the lists of Facts;
	// MaxNameRunes a font's name and MaxRunningRunes a header's or a
	// footer's text.
	MaxFonts        = 24
	MaxColours      = 16
	MaxRunning      = 6
	MaxNameRunes    = 64
	MaxRunningRunes = 300
	// MaxFactsBytes caps FactsFile as the host reads it.
	MaxFactsBytes = 256 << 10
)

// Kind is the kind of document.
type Kind string

// The kinds.
const (
	KindPDF  Kind = "pdf"
	KindDOCX Kind = "docx"
)

// The kinds of failure, the only words of a failed read that leave the
// decode image: the decoders' own text never does.
const (
	// FailureMalformed: the document could not be read as its kind.
	FailureMalformed = "malformed"
	// FailureLocked: the document needs a password to open.
	FailureLocked = "locked"
	// FailureTime: a page took longer than its limit to render.
	FailureTime = "time"
	// FailureMemory: the renderer ran out of the container's memory.
	FailureMemory = "memory"
	// FailureEmpty: the document has no page to render.
	FailureEmpty = "empty"
)

// Failures says each kind of failure in words for the model and the
// person.
var Failures = map[string]string{
	FailureMalformed: "it could not be read as a PDF or a Word document",
	FailureLocked:    "it needs a password to open",
	FailureTime:      "a page took longer than its limit to render",
	FailureMemory:    "a page ran out of the renderer's memory",
	FailureEmpty:     "it has no page to render",
}

// FactsFile is what Render writes beside the pages and pictures.
const FactsFile = "pages.json"

// Font is a font the document names: in a PDF, a font a rendered page
// uses; in a Word document, its theme's heading and body faces and the
// fonts its font table lists.
type Font struct {
	Name string `json:"name"`
	// Role is "headings" or "body" for a theme's faces, empty otherwise.
	Role string `json:"role,omitempty"`
}

// Colour is a colour of the document: a Word theme's declared colour by
// its role (accent1, dk2, hlink…), or a colour found on a PDF's rendered
// pages with the share of the inked area it covers, in percent.
type Colour struct {
	Hex   string `json:"hex"`
	Role  string `json:"role,omitempty"`
	Share int    `json:"share,omitempty"`
}

// Facts is FactsFile: what the pages role read of a document.
type Facts struct {
	Schema string `json:"schema"`
	Kind   Kind   `json:"kind"`
	// Pages is a PDF's page count; Rendered how many of its first pages
	// were rendered, as PageFile(1) to PageFile(Rendered).
	Pages    int      `json:"pages,omitempty"`
	Rendered int      `json:"rendered"`
	Fonts    []Font   `json:"fonts,omitempty"`
	Colours  []Colour `json:"colours,omitempty"`
	// Headers and Footers are a Word document's running text, part by
	// part.
	Headers []string `json:"headers,omitempty"`
	Footers []string `json:"footers,omitempty"`
	// Media are a Word document's pictures as written, MediaFile(n, ext);
	// MediaSkipped counts the pictures it has that were not read: a
	// drawing in another format (EMF, WMF, SVG), one past MaxMediaBytes,
	// or one past MaxMedia.
	Media        []string `json:"media,omitempty"`
	MediaSkipped int      `json:"mediaSkipped,omitempty"`
	// Failure is why nothing of the document could be read; Stopped why a
	// PDF's rendering stopped before its last page to render. Each is a
	// kind of failure.
	Failure string `json:"failure,omitempty"`
	Stopped string `json:"stopped,omitempty"`
}

// PageFile is page n's PNG as Render writes it.
func PageFile(n int) string { return "page-" + strconv.Itoa(n) + ".png" }

// MediaFile is picture n's file as Render writes it; ext is png or jpg.
func MediaFile(n int, ext string) string { return "media-" + strconv.Itoa(n) + "." + ext }

var (
	mediaName = regexp.MustCompile(`^media-([1-9][0-9]?)\.(png|jpg)$`)
	hexColour = regexp.MustCompile(`^#[0-9a-f]{6}$`)
	roleName  = regexp.MustCompile(`^[a-zA-Z0-9]{1,16}$`)
)

// Validate holds Facts to what Render can write: the host reads it from a
// container that parsed a document no one checked, so nothing in it is
// trusted past these rules.
func (f Facts) Validate() error {
	if f.Schema != Schema {
		return fmt.Errorf("pages: schema %q, want %q", f.Schema, Schema)
	}
	if f.Kind != KindPDF && f.Kind != KindDOCX {
		return fmt.Errorf("pages: kind %q", f.Kind)
	}
	for _, k := range []string{f.Failure, f.Stopped} {
		if _, ok := Failures[k]; k != "" && !ok {
			return errors.New("pages: a failure that is not a kind")
		}
	}
	if f.Rendered < 0 || f.Rendered > MaxPages || f.Pages < 0 || (f.Kind == KindPDF && f.Rendered > f.Pages) || (f.Kind == KindDOCX && (f.Rendered != 0 || f.Pages != 0)) {
		return fmt.Errorf("pages: %d rendered of %d", f.Rendered, f.Pages)
	}
	if f.MediaSkipped < 0 || f.MediaSkipped > maxDocxParts || (f.Kind == KindPDF && f.MediaSkipped != 0) {
		return errors.New("pages: a count of skipped pictures past its bounds")
	}
	if f.Failure != "" && (f.Rendered != 0 || len(f.Media) != 0) {
		return errors.New("pages: a failure with pages")
	}
	if len(f.Fonts) > MaxFonts || len(f.Colours) > MaxColours || len(f.Headers) > MaxRunning || len(f.Footers) > MaxRunning || len(f.Media) > MaxMedia {
		return errors.New("pages: a list past its cap")
	}
	for _, font := range f.Fonts {
		if !cleanName(font.Name) || (font.Role != "" && font.Role != "headings" && font.Role != "body") {
			return fmt.Errorf("pages: font %q", font.Name)
		}
	}
	for _, c := range f.Colours {
		if !hexColour.MatchString(c.Hex) || (c.Role != "" && !roleName.MatchString(c.Role)) || c.Share < 0 || c.Share > 100 {
			return fmt.Errorf("pages: colour %q", c.Hex)
		}
	}
	for _, s := range append(slices.Clone(f.Headers), f.Footers...) {
		if !cleanText(s, MaxRunningRunes) {
			return errors.New("pages: a header or footer past its cap or not plain text")
		}
	}
	for i, m := range f.Media {
		sub := mediaName.FindStringSubmatch(m)
		if sub == nil || sub[1] != strconv.Itoa(i+1) {
			return fmt.Errorf("pages: media %q", m)
		}
	}
	return nil
}

// cleanName says a font name is one line of printable text within
// MaxNameRunes.
func cleanName(s string) bool {
	return s != "" && strings.TrimSpace(s) == s && cleanText(s, MaxNameRunes)
}

// cleanText says s is valid UTF-8, printable and within max runes.
func cleanText(s string, max int) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// Text is a page's text with every control and format character taken
// out but the line breaks and tabs: what pdftotext and pandoc write can
// carry escape sequences, bidirectional overrides and invisible
// characters a reader of the text would not see.
func Text(s string) string {
	s = strings.ToValidUTF8(s, "")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == '\r':
			return -1
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Co, r), r == unicode.ReplacementChar:
			return -1
		}
		return r
	}, s)
}

// FontName is a font's name as a PDF embeds it, without the six-letter
// subset tag ("ABCDEF+SourceSerif4-Regular" is "SourceSerif4-Regular"),
// cut to one printable line within MaxNameRunes; empty when nothing is
// left.
func FontName(s string) string {
	if i := strings.IndexByte(s, '+'); i == 6 && strings.ToUpper(s[:6]) == s[:6] {
		s = s[7:]
	}
	return Line(s, MaxNameRunes)
}

// Line is s as one line of printable text, spaces collapsed, cut to max
// runes.
func Line(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	for _, r := range s {
		if unicode.IsSpace(r) {
			b.WriteRune(' ')
		} else if unicode.IsPrint(r) {
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if r := []rune(out); len(r) > max {
		out = strings.TrimSpace(string(r[:max]))
	}
	return out
}

// Box is a rectangle on an image the model was shown, in its pixels.
type Box struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// MinCropSide is the least width and height of a crop, in the pixels of
// the image the model was shown.
const MinCropSide = 8

// Crop is the part of img that box names, where box is in pixels of an
// image scale times smaller than img (a page shown at ViewPPI and cropped
// from its render at RenderPPI has scale 2). A box partly outside the
// image is cut to it; one smaller than MinCropSide, or outside, is
// refused.
func Crop(img image.Image, box Box, scale int) (image.Image, error) {
	if scale < 1 {
		return nil, errors.New("crop: a scale below 1")
	}
	if box.Width < MinCropSide || box.Height < MinCropSide {
		return nil, fmt.Errorf("a crop is at least %d by %d pixels", MinCropSide, MinCropSide)
	}
	b := img.Bounds()
	r := image.Rect(box.X*scale, box.Y*scale, (box.X+box.Width)*scale, (box.Y+box.Height)*scale).Add(b.Min).Intersect(b)
	if r.Dx() < MinCropSide*scale || r.Dy() < MinCropSide*scale {
		return nil, fmt.Errorf("the box %d,%d %dx%d is outside the %dx%d image", box.X, box.Y, box.Width, box.Height, b.Dx()/scale, b.Dy()/scale)
	}
	out := image.NewNRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	for y := range r.Dy() {
		for x := range r.Dx() {
			out.Set(x, y, img.At(r.Min.X+x, r.Min.Y+y))
		}
	}
	return out, nil
}

// LogoSide is the most a cropped logo's side may be, a design logo's cap:
// a larger crop is halved until it fits.
const LogoSide = 1024

// CropLogo is the logo a box of src, a PNG, names: the crop (Crop at
// scale; the whole image when box is nil), halved until no side is over
// LogoSide, encoded as a PNG at the best compression. crop_logo makes it
// to show the model and draw in previews; the CLI makes it again from its
// own copy of the page to keep.
func CropLogo(src []byte, box *Box, scale int) ([]byte, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(src))
	if err != nil || format != "png" || cfg.Width > MaxPageSide+2 || cfg.Height > MaxPageSide+2 {
		return nil, errors.New("the image to crop is not a staged PNG")
	}
	img, err := png.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, err
	}
	if box != nil {
		if img, err = Crop(img, *box, scale); err != nil {
			return nil, err
		}
	}
	for img.Bounds().Dx() > LogoSide || img.Bounds().Dy() > LogoSide {
		img = Half(img)
	}
	var b bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&b, img); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Half is img at half its width and height, each pixel the mean of four.
func Half(img image.Image) image.Image {
	b := img.Bounds()
	w, h := max(b.Dx()/2, 1), max(b.Dy()/2, 1)
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			var r, g, bl, a uint32
			for dy := range 2 {
				for dx := range 2 {
					cr, cg, cb, ca := img.At(b.Min.X+min(2*x+dx, b.Dx()-1), b.Min.Y+min(2*y+dy, b.Dy()-1)).RGBA()
					r, g, bl, a = r+cr, g+cg, bl+cb, a+ca
				}
			}
			out.Set(x, y, color.RGBA64{R: uint16(r / 4), G: uint16(g / 4), B: uint16(bl / 4), A: uint16(a / 4)}) //nolint:gosec // a mean of 16-bit values
		}
	}
	return out
}

// maxPageColours is how many colours PageColours answers.
const maxPageColours = 8

// PageColours are the colours that ink the most of pages, white left out:
// each pixel (every other one, each way) falls in a bucket of 16 shades a
// channel, and each of the largest buckets is answered as the mean of its
// pixels with its share of the inked area in percent, from the largest,
// those under one percent left out.
func PageColours(pages []image.Image) []Colour {
	type bucket struct{ r, g, b, n uint64 }
	buckets := map[uint32]*bucket{}
	var inked uint64
	for _, img := range pages {
		bd := img.Bounds()
		for y := bd.Min.Y; y < bd.Max.Y; y += 2 {
			for x := bd.Min.X; x < bd.Max.X; x += 2 {
				r, g, b, a := img.At(x, y).RGBA()
				if a < 0x8000 {
					continue
				}
				r8, g8, b8 := r>>8, g>>8, b>>8
				if r8 >= 240 && g8 >= 240 && b8 >= 240 {
					continue
				}
				inked++
				k := r8>>4<<8 | g8>>4<<4 | b8>>4
				bk := buckets[k]
				if bk == nil {
					bk = &bucket{}
					buckets[k] = bk
				}
				bk.r, bk.g, bk.b, bk.n = bk.r+uint64(r8), bk.g+uint64(g8), bk.b+uint64(b8), bk.n+1
			}
		}
	}
	keys := make([]uint32, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b uint32) int {
		if d := int(buckets[b].n) - int(buckets[a].n); d != 0 {
			return d
		}
		return int(a) - int(b)
	})
	var out []Colour
	for _, k := range keys {
		bk := buckets[k]
		share := int(bk.n * 100 / inked)
		if share < 1 || len(out) == maxPageColours {
			break
		}
		out = append(out, Colour{Hex: fmt.Sprintf("#%02x%02x%02x", bk.r/bk.n, bk.g/bk.n, bk.b/bk.n), Share: share})
	}
	return out
}
