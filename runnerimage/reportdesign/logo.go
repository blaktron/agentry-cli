package reportdesign

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/png"
	"strings"

	// The formats an upload may be in, registered for image.DecodeConfig.
	_ "image/jpeg"
)

// The logo: a person's own image, drawn in the title block, on a cover
// whose facts include it and in a logoLeft header. It comes from the
// person, never from the model and never from a URL: a design carries its
// pixels, as a PNG this package wrote from them.
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
// base64 of MaxLogoBytes.
var maxLogoField = base64.StdEncoding.EncodedLen(MaxLogoBytes)

// Logo is a design's logo. The zero Logo is none, and a design without one
// has no "logo" key.
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

func logoErr(kind error, field, msg string) error {
	return &FieldError{Field: field, Message: msg, kind: kind}
}

// logo reads the optional logo field: nil is none; a png, not empty, and
// never a jpeg.
func (w *wireImage) logo() (Logo, error) {
	if w == nil {
		return Logo{}, nil
	}
	if w.JPEG != nil {
		return Logo{}, fieldErr("logo", "is a PNG under a png key; it takes no jpeg")
	}
	if w.PNG == nil {
		return Logo{}, fieldErr("logo.png", "is required")
	}
	if *w.PNG == "" {
		return Logo{}, fieldErr("logo.png", "must not be empty; a design without a logo has no logo key")
	}
	return Logo{PNG: *w.PNG}, nil
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
