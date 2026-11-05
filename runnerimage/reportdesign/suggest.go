package reportdesign

import (
	"bytes"
	"errors"
	"image"
	"math"
)

// Suggestion is what SuggestColours reads from a logo: its two dominant
// colours as they are, and the nearest shade of each that passes the
// contrast rule for text, which passes it for graphics too. Secondary is
// "" when the logo has one colour.
type Suggestion struct {
	// Accent and Secondary pass TextContrast on white.
	Accent, Secondary string
	// FromAccent and FromSecondary are the colours as read, before any
	// darkening.
	FromAccent, FromSecondary string
}

// ErrNoColour is a logo with nothing to read: every pixel transparent or
// white.
var ErrNoColour = errors.New("the logo has no colour to suggest")

// SuggestColours reads the two dominant colours of a logo (its PNG or JPEG
// bytes, as a design carries them) and offers each as a shade that passes
// the contrast rule for text: the editors show them for the person to take
// as the accent and the secondary, never apply them unasked. The logo is
// held to the logo's upload bounds before a pixel is decoded. Transparent
// and near-white pixels are the background and are not read; the colour
// read most is the accent, and the next most that is far enough from it,
// and at least a twentieth as common, is the secondary.
func SuggestColours(logo []byte) (Suggestion, error) {
	if len(logo) > MaxLogoUploadBytes {
		return Suggestion{}, ErrImageTooLarge
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(logo))
	if err != nil || (format != "png" && format != "jpeg") {
		return Suggestion{}, ErrImageFormat
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > MaxLogoUploadPixels {
		return Suggestion{}, ErrImageTooLarge
	}
	img, _, err := image.Decode(bytes.NewReader(logo))
	if err != nil {
		return Suggestion{}, ErrImageFormat
	}
	b := img.Bounds()
	// Five bits a channel: 32,768 bins, each the sum of its pixels' colour
	// and their weight, so a bin's colour is its pixels' mean.
	type bin struct {
		r, g, b, w float64
	}
	bins := map[uint32]*bin{}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			pr, pg, pb, pa := img.At(x, y).RGBA()
			if pa < 0x8000 {
				continue
			}
			// Un-premultiply to the colour itself.
			r, g, bl := float64(pr)*0xffff/float64(pa)/257, float64(pg)*0xffff/float64(pa)/257, float64(pb)*0xffff/float64(pa)/257
			if r > 240 && g > 240 && bl > 240 {
				continue
			}
			key := uint32(r)>>3<<10 | uint32(g)>>3<<5 | uint32(bl)>>3
			bn := bins[key]
			if bn == nil {
				bn = &bin{}
				bins[key] = bn
			}
			w := float64(pa) / 0xffff
			bn.r += r * w
			bn.g += g * w
			bn.b += bl * w
			bn.w += w
		}
	}
	if len(bins) == 0 {
		return Suggestion{}, ErrNoColour
	}
	var first, second *bin
	for _, bn := range bins {
		if first == nil || bn.w > first.w {
			first = bn
		}
	}
	accent := rgb{first.r / first.w, first.g / first.w, first.b / first.w}
	for _, bn := range bins {
		if bn == first || bn.w < first.w/20 {
			continue
		}
		c := rgb{bn.r / bn.w, bn.g / bn.w, bn.b / bn.w}
		if distance(c, accent) < 80 {
			continue
		}
		if second == nil || bn.w > second.w {
			second = bn
		}
	}
	out := Suggestion{FromAccent: accent.hex()}
	out.Accent, _ = NearestPassing(out.FromAccent, TextContrast)
	if second != nil {
		out.FromSecondary = rgb{second.r / second.w, second.g / second.w, second.b / second.w}.hex()
		out.Secondary, _ = NearestPassing(out.FromSecondary, TextContrast)
	}
	return out, nil
}

// distance is the Euclidean distance of two colours in RGB, 0 to 441.
func distance(a, b rgb) float64 {
	return math.Sqrt((a.r-b.r)*(a.r-b.r) + (a.g-b.g)*(a.g-b.g) + (a.b-b.b)*(a.b-b.b))
}
