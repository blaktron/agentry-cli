package spec

import "testing"

// The receipt's pin line for each way a harness image is pinned.
func TestImagePinString(t *testing.T) {
	for _, c := range []struct {
		pin  *ImagePin
		want string
	}{
		{nil, ""},
		{&ImagePin{By: "digest", Value: "sha256:ab"}, "digest sha256:ab"},
		{&ImagePin{By: "artifact", Value: "sha256:cd"}, "artifact sha256:cd"},
		{&ImagePin{By: "tag"}, "tag (not pinned: the tag can move)"},
	} {
		if got := c.pin.String(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.pin, got, c.want)
		}
	}
}
