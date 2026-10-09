package reportthumbs

import (
	"bytes"
	"image/png"
	"testing"
)

// TestEveryBuiltinHasAThumbnail: a PNG for each built-in theme of the core's
// pkg/reportdesign, and nothing for anything else.
func TestEveryBuiltinHasAThumbnail(t *testing.T) {
	for _, id := range IDs() {
		b, ok := PNG(id)
		if !ok {
			t.Fatalf("%s has no thumbnail", id)
		}
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil || img.Bounds().Dx() < 400 {
			t.Errorf("%s: %v, %v", id, img.Bounds(), err)
		}
	}
	if _, ok := PNG("../plain"); ok {
		t.Error("a path was read as a thumbnail")
	}
}
