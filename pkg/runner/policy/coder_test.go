package policy

import (
	"strings"
	"testing"
)

func TestCoderImageIsPinnedByDigest(t *testing.T) {
	if !PinnedByDigest(DefaultCoderImage) {
		t.Fatalf("DefaultCoderImage %q is not pinned by digest", DefaultCoderImage)
	}
	p := Default()
	if p.Images.Coder != "" || p.Images.CoderImage() != DefaultCoderImage {
		t.Fatalf("images.coder %q, resolved %q", p.Images.Coder, p.Images.CoderImage())
	}
	p.Images.Coder = "registry.example/coder:1@sha256:" + strings.Repeat("a", 64)
	if err := p.Validate(); err != nil || p.Images.CoderImage() != p.Images.Coder {
		t.Fatalf("a digest-pinned images.coder: %v", err)
	}
	p.Images.Coder = "registry.example/coder:1"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "images.coder") {
		t.Errorf("a tag-only images.coder was accepted (%v)", err)
	}
}
