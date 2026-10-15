package reportdesign

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/draw"
	"image/png"
	"math"
	"strings"

	// The formats an upload may be in, registered for image.DecodeConfig.
	_ "image/jpeg"
)

// The logo: a person's own image, drawn only in the title block. It comes
// from the person, never from the model and never from a URL: a design
// carries its pixels, as a PNG this package wrote from them.
//
// An upload is a PNG or a JPEG, at most MaxLogoUploadBytes; NormalizeLogo
// decodes it, scales it down to fit MaxLogoSide and encodes it again as PNG
// from the decoded pixels, so no chunk, profile or metadata of the upload
// survives. A design's logo field is held by Validate to a PNG within the
// same bounds; the server stores only what NormalizeLogo wrote.
const (
	// MaxLogoUploadBytes caps an uploaded image before a byte of it is
	// decoded.
	MaxLogoUploadBytes = 512 << 10
	// MaxLogoBytes caps a design's logo, the PNG itself: a PNG encoder
	// can write more than the upload was, and a logo too detailed for
	// this is refused rather than degraded.
	MaxLogoBytes = 512 << 10
	// MaxLogoSide caps a logo's width and height in pixels. A larger upload
	// is scaled down to fit; a title block draws a logo a few centimetres
	// wide, and this is past 300 dpi at that size.
	MaxLogoSide = 1024
	// MaxLogoUploadPixels caps what an upload may make the decoder
	// allocate, as the registry's image proxy does.
	MaxLogoUploadPixels = 4096 * 4096
)

// maxLogoField is the most a design's logo field may take as sent: the
// base64 of MaxLogoBytes. Parse allows a document this much over
// MaxDocumentBytes, and holds the rest of it to MaxDocumentBytes (the
// 16 KB does not count the logo).
var maxLogoField = base64.StdEncoding.EncodedLen(MaxLogoBytes)

// Logo is a design's logo. The zero Logo is none, and a design without one
// has no "logo" key, so every design saved before logos existed keeps its
// canonical form and its SHA-256.
type Logo struct {
	// PNG is the image, standard base64 with padding: a PNG of at most
	// MaxLogoBytes and at most MaxLogoSide pixels each way.
	PNG string `json:"png"`
}

// Decode is the logo's PNG bytes. The base64 must be one unbroken line:
// the decoder would skip a CR or LF, and the same logo would then have more
// than one canonical form, and hash.
func (l Logo) Decode() ([]byte, error) {
	if strings.ContainsAny(l.PNG, "\r\n") {
		return nil, errors.New("a line break in the base64")
	}
	return base64.StdEncoding.Strict().DecodeString(l.PNG)
}

// The kinds of a logo's refusal, for errors.Is on a *FieldError: a server
// answers an upload over a bound differently from one in no format it reads.
var (
	// ErrLogoTooLarge is an upload or a logo over a byte or pixel bound.
	ErrLogoTooLarge = errors.New("logo too large")
	// ErrLogoFormat is an image that is not a PNG or a JPEG (an upload),
	// not a PNG (a design's logo), or does not decode.
	ErrLogoFormat = errors.New("logo not a PNG or JPEG")
)

func logoErr(kind error, field, msg string) error {
	return &FieldError{Field: field, Message: msg, kind: kind}
}

// NormalizeLogo turns an uploaded image into a design's logo: a PNG or a
// JPEG (what the bytes are, not what a file name or a header said), at most
// MaxLogoUploadBytes and MaxLogoUploadPixels, decoded, scaled down to fit
// MaxLogoSide by averaging, and encoded again as PNG from the pixels. A
// refusal is a *FieldError on "logo" whose kind is ErrLogoTooLarge or
// ErrLogoFormat. A JPEG's orientation tag is not read: its pixels are drawn
// as they are stored.
func NormalizeLogo(upload []byte) (Logo, error) {
	if len(upload) > MaxLogoUploadBytes {
		return Logo{}, logoErr(ErrLogoTooLarge, "logo", "a logo is at most 512 KB")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(upload))
	if err != nil || (format != "png" && format != "jpeg") {
		return Logo{}, logoErr(ErrLogoFormat, "logo", "a logo is a PNG or a JPEG")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > MaxLogoUploadPixels {
		return Logo{}, logoErr(ErrLogoTooLarge, "logo", "a logo has at most 16 million pixels")
	}
	img, _, err := image.Decode(bytes.NewReader(upload))
	if err != nil {
		return Logo{}, logoErr(ErrLogoFormat, "logo", "the image does not decode as a PNG or a JPEG")
	}
	var out bytes.Buffer
	if err := png.Encode(&out, scaleToFit(img, MaxLogoSide)); err != nil {
		return Logo{}, logoErr(ErrLogoFormat, "logo", "the image could not be encoded again")
	}
	if out.Len() > MaxLogoBytes {
		return Logo{}, logoErr(ErrLogoTooLarge, "logo", "this image is over 512 KB as a PNG; a simpler or smaller one is needed")
	}
	return Logo{PNG: base64.StdEncoding.EncodeToString(out.Bytes())}, nil
}

// checkLogo holds a design's logo to the bounds NormalizeLogo writes within:
// standard base64 of a PNG of at most MaxLogoBytes and MaxLogoSide pixels
// each way, which decodes whole. It does not require the bytes to be this
// package's own encoding (a PNG encoder's output may change between Go
// releases), so a renderer that wants that runs NormalizeLogo on it again.
func checkLogo(l Logo) error {
	if len(l.PNG) > maxLogoField {
		return logoErr(ErrLogoTooLarge, "logo.png", "a logo is at most 512 KB")
	}
	data, err := l.Decode()
	if err != nil || len(data) == 0 {
		return logoErr(ErrLogoFormat, "logo.png", "must be a PNG in standard base64")
	}
	if len(data) > MaxLogoBytes {
		return logoErr(ErrLogoTooLarge, "logo.png", "a logo is at most 512 KB")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "png" {
		return logoErr(ErrLogoFormat, "logo.png", "must be a PNG in standard base64")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > MaxLogoSide || cfg.Height > MaxLogoSide {
		return logoErr(ErrLogoTooLarge, "logo.png", "a logo is at most 1024 pixels each way")
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		return logoErr(ErrLogoFormat, "logo.png", "the PNG does not decode")
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
