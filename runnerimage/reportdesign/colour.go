package reportdesign

import (
	"fmt"
	"math"
	"strconv"
)

// The contrast a design's accent must reach on white (WCAG 2.2 1.4.3 and
// 1.4.11).
const (
	// TextContrast is the least contrast for an accent that touches text.
	TextContrast = 4.5
	// GraphicsContrast is the least for an accent on graphics only.
	GraphicsContrast = 3.0
)

// White is the page.
const White = "#ffffff"

// TintMix is the accent's share of its tint, the rest white (the
// tint is derived, never stored). The site's editor mixes the same.
const TintMix = 0.12

type rgb struct{ r, g, b float64 } // 0..255

func parseHex(s string) (rgb, error) {
	if !hexColour.MatchString(s) {
		return rgb{}, fmt.Errorf("reportdesign: %q is not #rrggbb in lower-case hex", s)
	}
	v, _ := strconv.ParseUint(s[1:], 16, 32)
	return rgb{float64(v >> 16 & 0xff), float64(v >> 8 & 0xff), float64(v & 0xff)}, nil
}

func (c rgb) hex() string {
	clamp := func(x float64) int { return int(math.Max(0, math.Min(255, math.Round(x)))) }
	return fmt.Sprintf("#%02x%02x%02x", clamp(c.r), clamp(c.g), clamp(c.b))
}

// luminance is WCAG's relative luminance.
func (c rgb) luminance() float64 {
	lin := func(v float64) float64 {
		v /= 255
		if v <= 0.04045 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.r) + 0.7152*lin(c.g) + 0.0722*lin(c.b)
}

// Contrast is the WCAG contrast ratio of two #rrggbb colours, from 1 to 21.
func Contrast(a, b string) (float64, error) {
	ca, err := parseHex(a)
	if err != nil {
		return 0, err
	}
	cb, err := parseHex(b)
	if err != nil {
		return 0, err
	}
	la, lb := ca.luminance(), cb.luminance()
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05), nil
}

// onWhite is a colour's contrast on the page; an invalid colour reads as no
// contrast.
func onWhite(colour string) float64 {
	r, err := Contrast(colour, White)
	if err != nil {
		return 1
	}
	return r
}

// NearestPassing is the shade of a colour that reaches min contrast on white
// with the least change: the colour itself when it passes, else the lightest
// darkening of it (the same hue and saturation, a lower lightness) that does.
// The editor offers it when it refuses an accent.
func NearestPassing(colour string, min float64) (string, error) {
	c, err := parseHex(colour)
	if err != nil {
		return "", err
	}
	if onWhite(colour) >= min {
		return colour, nil
	}
	h, s, l := c.hsl()
	// Contrast on white rises as lightness falls, so search the lightness:
	// lo always passes (black), hi never does.
	lo, hi := 0.0, l
	for range 40 {
		mid := (lo + hi) / 2
		if onWhite(fromHSL(h, s, mid).hex()) >= min {
			lo = mid
		} else {
			hi = mid
		}
	}
	// Rounding to whole channel values can land a hair under the line, so
	// step down until the hex itself passes.
	for l := lo; l >= 0; l -= 0.001 {
		if out := fromHSL(h, s, l).hex(); onWhite(out) >= min {
			return out, nil
		}
	}
	return "#000000", nil
}

// Tint is the accent's tint: TintMix of the accent over white, for tinted
// table headers, banded rows and callouts.
func Tint(accent string) (string, error) {
	c, err := parseHex(accent)
	if err != nil {
		return "", err
	}
	mix := func(v float64) float64 { return TintMix*v + (1-TintMix)*255 }
	return rgb{mix(c.r), mix(c.g), mix(c.b)}.hex(), nil
}

func (c rgb) hsl() (h, s, l float64) {
	r, g, b := c.r/255, c.g/255, c.b/255
	maxV, minV := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	l = (maxV + minV) / 2
	if maxV == minV {
		return 0, 0, l
	}
	d := maxV - minV
	if l > 0.5 {
		s = d / (2 - maxV - minV)
	} else {
		s = d / (maxV + minV)
	}
	switch maxV {
	case r:
		h = (g - b) / d
		if g < b {
			h += 6
		}
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	return h / 6, s, l
}

func fromHSL(h, s, l float64) rgb {
	if s == 0 {
		return rgb{l * 255, l * 255, l * 255}
	}
	var q float64
	if l < 0.5 {
		q = l * (1 + s)
	} else {
		q = l + s - l*s
	}
	p := 2*l - q
	hue := func(t float64) float64 {
		if t < 0 {
			t++
		}
		if t > 1 {
			t--
		}
		switch {
		case t < 1.0/6:
			return p + (q-p)*6*t
		case t < 1.0/2:
			return q
		case t < 2.0/3:
			return p + (q-p)*(2.0/3-t)*6
		}
		return p
	}
	return rgb{hue(h+1.0/3) * 255, hue(h) * 255, hue(h-1.0/3) * 255}
}
