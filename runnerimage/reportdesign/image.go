package reportdesign

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"strings"
)

// The images a design carries beyond its logo: the cover image, a custom
// cover's images and its thumbnail. Each is the person's own upload, never
// the model's and never a URL: NormalizeCoverImage decodes it, scales it
// down to fit MaxCoverImageSide and encodes it again from the pixels, a PNG
// as PNG and a JPEG as JPEG (a photograph stays a photograph's size), so no
// chunk, segment, profile or metadata of the upload survives.
const (
	// MaxCoverImageUploadBytes caps an uploaded image before a byte of it
	// is decoded.
	MaxCoverImageUploadBytes = 2 << 20
	// MaxCoverImageBytes caps a design's cover image as encoded again.
	MaxCoverImageBytes = 2 << 20
	// MaxCoverImageSide caps a cover image's width and height in pixels. A
	// larger upload is scaled down to fit; across a page's width this is
	// past 240 dpi.
	MaxCoverImageSide = 2048
	// MaxCoverImageUploadPixels caps what an upload may make the decoder
	// allocate.
	MaxCoverImageUploadPixels = 4096 * 4096
	// JPEGQuality is the quality a JPEG is encoded again at.
	JPEGQuality = 90
)

// The kinds of an image's refusal, for errors.Is on a *FieldError: a server
// answers an upload over a bound differently from one in no format it reads.
var (
	// ErrImageTooLarge is an upload or an image over a byte or pixel bound.
	ErrImageTooLarge = errors.New("image too large")
	// ErrImageFormat is an image that is not a PNG or a JPEG, not the
	// format its place takes, or does not decode.
	ErrImageFormat = errors.New("image not a PNG or JPEG")
)

// ErrLogoTooLarge and ErrLogoFormat are the logo's names for the same two
// kinds.
var (
	ErrLogoTooLarge = ErrImageTooLarge
	ErrLogoFormat   = ErrImageFormat
)

// Image is a design's image in one of the two formats, standard base64 with
// padding, exactly one of the two set. The zero Image is none.
type Image struct {
	// PNG is the image as a PNG.
	PNG string `json:"png,omitempty"`
	// JPEG is the image as a JPEG.
	JPEG string `json:"jpeg,omitempty"`
}

// Format is "png" or "jpeg", or "" for no image.
func (i Image) Format() string {
	switch {
	case i.PNG != "":
		return "png"
	case i.JPEG != "":
		return "jpeg"
	}
	return ""
}

// Decode is the image's bytes and its format. The base64 must be one
// unbroken line: the decoder would skip a CR or LF, and the same image
// would then have more than one canonical form, and hash.
func (i Image) Decode() ([]byte, string, error) {
	var data string
	switch {
	case i.PNG != "" && i.JPEG != "":
		return nil, "", errors.New("both a png and a jpeg")
	case i.PNG != "":
		data = i.PNG
	case i.JPEG != "":
		data = i.JPEG
	default:
		return nil, "", errors.New("no image")
	}
	if strings.ContainsAny(data, "\r\n") {
		return nil, "", errors.New("a line break in the base64")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(data)
	if err != nil {
		return nil, "", err
	}
	return raw, i.Format(), nil
}

// ImageOf is an image from its bytes and format ("png" or "jpeg").
func ImageOf(data []byte, format string) Image {
	s := base64.StdEncoding.EncodeToString(data)
	if format == "jpeg" {
		return Image{JPEG: s}
	}
	return Image{PNG: s}
}

func imageErr(kind error, field, msg string) error {
	return &FieldError{Field: field, Message: msg, kind: kind}
}

// image reads an optional image field: nil is none; exactly one of png and
// jpeg, and not empty.
func (w *wireImage) image(field string) (Image, error) {
	if w == nil {
		return Image{}, nil
	}
	switch {
	case w.PNG == nil && w.JPEG == nil:
		return Image{}, fieldErr(field, "must hold a png or a jpeg; a design without one has no %s key", field)
	case w.PNG != nil && w.JPEG != nil:
		return Image{}, fieldErr(field, "must hold a png or a jpeg, not both")
	case w.PNG != nil:
		if *w.PNG == "" {
			return Image{}, fieldErr(field+".png", "must not be empty; a design without one has no %s key", field)
		}
		return Image{PNG: *w.PNG}, nil
	default:
		if *w.JPEG == "" {
			return Image{}, fieldErr(field+".jpeg", "must not be empty; a design without one has no %s key", field)
		}
		return Image{JPEG: *w.JPEG}, nil
	}
}

// NormalizeCoverImage turns an uploaded image into a design's cover image, or
// one of a custom cover's: a PNG or a JPEG (what the bytes are, not what a
// file name or a header said), at most MaxCoverImageUploadBytes and
// MaxCoverImageUploadPixels, decoded, scaled down to fit MaxCoverImageSide by
// averaging, and encoded again from the pixels in the format it came in. A
// refusal is a *FieldError on field whose kind is ErrImageTooLarge or
// ErrImageFormat. A JPEG's orientation tag is not read: its pixels are drawn
// as they are stored.
func NormalizeCoverImage(upload []byte) (Image, error) {
	return normalizeImage("coverImage", upload, MaxCoverImageUploadBytes, MaxCoverImageBytes, MaxCoverImageSide)
}

func normalizeImage(field string, upload []byte, maxUpload, maxBytes, maxSide int) (Image, error) {
	if len(upload) > maxUpload {
		return Image{}, imageErr(ErrImageTooLarge, field, "an image is at most 2 MB")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(upload))
	if err != nil || (format != "png" && format != "jpeg") {
		return Image{}, imageErr(ErrImageFormat, field, "an image is a PNG or a JPEG")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > MaxCoverImageUploadPixels {
		return Image{}, imageErr(ErrImageTooLarge, field, "an image has at most 16 million pixels")
	}
	img, _, err := image.Decode(bytes.NewReader(upload))
	if err != nil {
		return Image{}, imageErr(ErrImageFormat, field, "the image does not decode as a PNG or a JPEG")
	}
	img = scaleToFit(img, maxSide)
	var out bytes.Buffer
	if format == "jpeg" {
		err = jpeg.Encode(&out, img, &jpeg.Options{Quality: JPEGQuality})
	} else {
		err = png.Encode(&out, img)
	}
	if err != nil {
		return Image{}, imageErr(ErrImageFormat, field, "the image could not be encoded again")
	}
	if out.Len() > maxBytes {
		if format == "png" {
			return Image{}, imageErr(ErrImageTooLarge, field, "this image is over 2 MB as a PNG; a JPEG, or a simpler or smaller image, is needed")
		}
		return Image{}, imageErr(ErrImageTooLarge, field, "this image is over 2 MB encoded again; a smaller one is needed")
	}
	return ImageOf(out.Bytes(), format), nil
}

// checkImage holds a design's image to the bounds normalizeImage writes
// within: standard base64 of a PNG or a JPEG (a PNG alone when pngOnly) of
// at most maxBytes and maxSide pixels each way, which decodes whole. It does
// not require the bytes to be this package's own encoding (an encoder's
// output may change between Go releases), so a renderer that wants that
// runs the normalizer on it again.
func checkImage(field string, img Image, maxBytes, maxSide int, pngOnly bool) error {
	data, format, err := img.Decode()
	if err != nil || len(data) == 0 {
		return imageErr(ErrImageFormat, field, "must be a PNG or a JPEG in standard base64")
	}
	if pngOnly && format != "png" {
		return imageErr(ErrImageFormat, field, "must be a PNG")
	}
	if len(data) > maxBytes {
		return imageErr(ErrImageTooLarge, field, "is over its byte limit")
	}
	cfg, got, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || got != format {
		return imageErr(ErrImageFormat, field, "must be a "+strings.ToUpper(format)+" under its key")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxSide || cfg.Height > maxSide {
		return imageErr(ErrImageTooLarge, field, "is over its pixel limit each way")
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return imageErr(ErrImageFormat, field, "the image does not decode")
	}
	return nil
}

// scaleToFit returns img when it fits side×side, and otherwise img scaled
// down to fit, its aspect kept, each new pixel the area-weighted average of
// the pixels it covers, in premultiplied alpha so a transparent edge does
// not darken.
func scaleToFit(img image.Image, side int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= side && h <= side {
		return img
	}
	dw, dh := side, side
	if w >= h {
		dh = max(1, int(math.Round(float64(h)*float64(side)/float64(w))))
	} else {
		dw = max(1, int(math.Round(float64(w)*float64(side)/float64(h))))
	}
	src, ok := img.(*image.RGBA)
	if !ok || src.Bounds().Min != (image.Point{}) {
		src = image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(src, src.Bounds(), img, b.Min, draw.Src)
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	sx, sy := float64(w)/float64(dw), float64(h)/float64(dh)
	for y := range dh {
		y0, y1 := float64(y)*sy, float64(y+1)*sy
		for x := range dw {
			x0, x1 := float64(x)*sx, float64(x+1)*sx
			var acc [4]float64
			var total float64
			for iy := int(y0); iy < h && float64(iy) < y1; iy++ {
				wy := math.Min(y1, float64(iy+1)) - math.Max(y0, float64(iy))
				for ix := int(x0); ix < w && float64(ix) < x1; ix++ {
					wt := wy * (math.Min(x1, float64(ix+1)) - math.Max(x0, float64(ix)))
					o := iy*src.Stride + ix*4
					for c := range 4 {
						acc[c] += wt * float64(src.Pix[o+c])
					}
					total += wt
				}
			}
			o := y*dst.Stride + x*4
			for c := range 4 {
				dst.Pix[o+c] = uint8(math.Round(acc[c] / total))
			}
		}
	}
	return dst
}
