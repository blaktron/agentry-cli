package reportthumbs

import (
	"bytes"
	"image/png"
	"slices"
	"testing"

	"github.com/blaktron/agentry/pkg/reportdesign"
)

// TestEveryBuiltinHasAThumbnail: one PNG per built-in theme of the core's
// pkg/reportdesign, in its order, and nothing for anything else.
func TestEveryBuiltinHasAThumbnail(t *testing.T) {
	if !slices.Equal(IDs, reportdesign.BuiltinIDs()) {
		t.Fatalf("IDs = %v, the core's built-in themes are %v", IDs, reportdesign.BuiltinIDs())
	}
	for _, id := range IDs {
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
