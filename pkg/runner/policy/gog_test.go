package policy

import "testing"

func TestGogImageIsPinnedByDigest(t *testing.T) {
	if !PinnedByDigest(DefaultGogImage) {
		t.Fatalf("DefaultGogImage %q is not pinned by digest", DefaultGogImage)
	}
}
