package reportthumbs

import (
	"bytes"
	"image/png"
	"testing"
)

// TestEveryBuiltinHasAThumbnail: a page PNG and a cover PNG for each of
// the ten built-in themes of the core's pkg/reportdesign, and nothing for
// anything else.
func TestEveryBuiltinHasAThumbnail(t *testing.T) {
	if len(IDs()) != 10 {
		t.Fatalf("%d built-ins, want ten", len(IDs()))
	}
	for _, id := range IDs() {
		for name, read := range map[string]func(string) ([]byte, bool){"page": PNG, "cover": CoverPNG} {
			b, ok := read(id)
			if !ok {
				t.Fatalf("%s has no %s thumbnail", id, name)
			}
			img, err := png.Decode(bytes.NewReader(b))
			if err != nil || img.Bounds().Dx() < 400 {
				t.Errorf("%s %s: %v, %v", id, name, img.Bounds(), err)
			}
		}
	}
	for _, bad := range []string{"../plain", "plain-cover", "formal"} {
		if _, ok := PNG(bad); ok {
			t.Errorf("%q was read as a thumbnail", bad)
		}
		if _, ok := CoverPNG(bad); ok {
			t.Errorf("%q was read as a cover thumbnail", bad)
		}
	}
}
