package docpages

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The bounds of a PDF's pictures.
const (
	// MinPictureSide is the least width and height of a picture worth
	// offering: anything smaller is a bullet, a rule or a texture tile.
	MinPictureSide = 32
	// MaxPicturePixels caps a picture pdfimages wrote, checked before a
	// byte of it is decoded.
	MaxPicturePixels = 4096 * 4096
	// MaxPictureFileBytes caps one file pdfimages wrote, as read.
	MaxPictureFileBytes = 64 << 20
	// PictureJPEGQuality is the quality an opaque picture is written at.
	PictureJPEGQuality = 90
	// MaxPictureSide caps a PDF picture's sides as written: a larger one
	// is halved until it fits, past what a cover image keeps.
	MaxPictureSide = 2048
	// MaxStagedMediaBytes caps a picture as the CLI stages it, a PNG the
	// document tools read: a larger one is halved until it fits.
	MaxStagedMediaBytes = 4 << 20
)

// ImageRow is one row of `pdfimages -list`: the page it is drawn on, its
// number among the images listed (the file pdfimages writes for it), its
// type (image, smask, mask, stencil), its size in pixels and the PDF
// object it is.
type ImageRow struct {
	Page, Num     int
	Type          string
	Width, Height int
	Object        string
}

// ParseImageList reads `pdfimages -list`'s table, the two header lines
// left out and any row it cannot read skipped.
func ParseImageList(s string) []ImageRow {
	var out []ImageRow
	for line := range strings.Lines(s) {
		f := strings.Fields(line)
		if len(f) < 12 {
			continue
		}
		page, err1 := strconv.Atoi(f[0])
		num, err2 := strconv.Atoi(f[1])
		w, err3 := strconv.Atoi(f[3])
		h, err4 := strconv.Atoi(f[4])
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || page < 1 || num < 0 || w < 1 || h < 1 {
			continue
		}
		out = append(out, ImageRow{Page: page, Num: num, Type: f[2], Width: w, Height: h, Object: f[10] + " " + f[11]})
	}
	return out
}

// pictureFile is the file pdfimages writes for the image numbered num
// under prefix: a PNG, or a JPEG for an image the PDF holds as one.
func pictureFile(prefix string, num int) (string, string) {
	base := prefix + "-" + strconv.Itoa(1000 + num)[1:]
	if num >= 1000 {
		base = prefix + "-" + strconv.Itoa(num)
	}
	for _, ext := range []string{".png", ".jpg"} {
		if fi, err := os.Lstat(base + ext); err == nil && fi.Mode().IsRegular() {
			return base + ext, strings.TrimPrefix(ext, ".")
		}
	}
	return "", ""
}

// readPicture decodes one file pdfimages wrote, its size checked before
// its pixels are.
func readPicture(file, ext string) (image.Image, bool) {
	fi, err := os.Lstat(file)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > MaxPictureFileBytes {
		return nil, false
	}
	b, err := os.ReadFile(file) //nolint:gosec // this role's own scratch file
	if err != nil {
		return nil, false
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || (format != "png" && format != "jpeg") || (ext == "jpg") != (format == "jpeg") {
		return nil, false
	}
	if cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > MaxPicturePixels {
		return nil, false
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, false
	}
	return img, true
}

// withMask is img with mask, a grey image of the same size, as its
// alpha: a PDF keeps a picture's transparency (a logo's, most often) as a
// soft mask beside it.
func withMask(img, mask image.Image) image.Image {
	b := img.Bounds()
	if mask.Bounds().Dx() != b.Dx() || mask.Bounds().Dy() != b.Dy() {
		return img
	}
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	mb := mask.Bounds()
	for y := range b.Dy() {
		for x := range b.Dx() {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			a := color.GrayModel.Convert(mask.At(mb.Min.X+x, mb.Min.Y+y)).(color.Gray).Y
			out.SetNRGBA(x, y, color.NRGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(bl >> 8), A: a}) //nolint:gosec // 16-bit channels to 8
		}
	}
	return out
}

// Opaque says every pixel of img is opaque.
func Opaque(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok && o.Opaque() {
		return true
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xffff {
				return false
			}
		}
	}
	return true
}

// EncodePicture is img as the file a picture is written as: scaled by
// halves until no side is over maxSide, then a PNG when it has
// transparency, else a JPEG at PictureJPEGQuality (a photograph stays a
// photograph's size). It answers the bytes and "png" or "jpg".
func EncodePicture(img image.Image, maxSide int) ([]byte, string, error) {
	for img.Bounds().Dx() > maxSide || img.Bounds().Dy() > maxSide {
		img = Half(img)
	}
	var b bytes.Buffer
	if Opaque(img) {
		rgba := image.NewRGBA(image.Rect(0, 0, img.Bounds().Dx(), img.Bounds().Dy()))
		draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
		if err := jpeg.Encode(&b, rgba, &jpeg.Options{Quality: PictureJPEGQuality}); err != nil {
			return nil, "", err
		}
		return b.Bytes(), "jpg", nil
	}
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&b, img); err != nil {
		return nil, "", err
	}
	return b.Bytes(), "png", nil
}

// pdfPictures writes the pictures a PDF draws on its first last pages,
// page by page, beside its pages: each picture once (a header's logo is
// drawn on every page and kept for the first), its soft mask joined to it
// as transparency, none under MinPictureSide, at most MaxMedia, each as
// EncodePicture writes it and named in f.Media with its page in
// f.MediaPages. A page whose pictures cannot be read gives none; a PDF
// with no readable picture is not a failure, its pages are what it gives.
func pdfPictures(ctx context.Context, src, out, scratch string, t Tools, last int, f *Facts) error {
	dir := filepath.Join(scratch, "pictures")
	seen := map[string]bool{}
	for n := 1; n <= last; n++ {
		page := strconv.Itoa(n)
		list, kind := runPoppler(ctx, t.PDFImages, "-list", "-f", page, "-l", page, src)
		if kind != "" {
			continue
		}
		rows := ParseImageList(list)
		fresh := false
		for _, r := range rows {
			if r.Type == "image" && !seen[r.Object] {
				fresh = true
			}
		}
		if !fresh {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		prefix := filepath.Join(dir, "img")
		if _, kind := runPoppler(ctx, t.PDFImages, "-png", "-j", "-f", page, "-l", page, src, prefix); kind != "" {
			continue
		}
		for i, r := range rows {
			if r.Type != "image" || seen[r.Object] {
				continue
			}
			seen[r.Object] = true
			if r.Width < MinPictureSide || r.Height < MinPictureSide {
				continue
			}
			if len(f.Media) == MaxMedia {
				f.MediaSkipped++
				continue
			}
			file, ext := pictureFile(prefix, r.Num)
			img, ok := readPicture(file, ext)
			if !ok {
				f.MediaSkipped++
				continue
			}
			if i+1 < len(rows) && rows[i+1].Type == "smask" && rows[i+1].Object == r.Object {
				if mfile, mext := pictureFile(prefix, rows[i+1].Num); mfile != "" {
					if mask, ok := readPicture(mfile, mext); ok {
						img = withMask(img, mask)
					}
				}
			}
			b, ext, err := EncodePicture(img, MaxPictureSide)
			if err != nil || len(b) > MaxMediaBytes {
				f.MediaSkipped++
				continue
			}
			name := MediaFile(len(f.Media)+1, ext)
			if err := os.WriteFile(filepath.Join(out, name), b, 0o644); err != nil { //nolint:gosec // read by the host
				return err
			}
			f.Media = append(f.Media, name)
			f.MediaPages = append(f.MediaPages, n)
		}
	}
	return os.RemoveAll(dir)
}
