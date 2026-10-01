package runnerimage

import (
	"net/url"
	"slices"
	"strings"
	"testing"

	corearchive "github.com/blaktron/agentry/pkg/archive"
	coremanifest "github.com/blaktron/agentry/pkg/manifest"

	imagejudge "github.com/blaktron/agentry-cli/runnerimage/judge"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// TestTheReviewsRestatementsAreTheCores holds what this stdlib-only image
// restates to the core it cannot import: S1's text sniff, the packs a
// manifest ships, and the host the judge asks.
func TestTheReviewsRestatementsAreTheCores(t *testing.T) {
	for _, head := range [][]byte{
		nil, []byte("plain text\n"), {0x89, 'P', 'N', 'G', 0x00}, []byte("a\x01b\x02c\x03d\x04e"),
		[]byte(strings.Repeat("x", 97) + "\x01\x02\x03"), []byte(strings.Repeat("x", 95) + "\x01\x02\x03\x04\x05"),
		[]byte("tabs\tand\r\nlines\f"),
	} {
		if got, want := toolsrv.LooksText(head), corearchive.LooksText(head); got != want {
			t.Errorf("LooksText(%q) = %v, the core's %v", head, got, want)
		}
	}
	for _, doc := range []string{
		`{"knowledge":{"ships":[{"name":"docs"}]}}`, `{}`, `not json`,
		`{"knowledge":{"ships":[{"name":"b"},{"name":"a"},{"name":"a"},{"name":"../x"},{"name":"Up"},{"name":"-x"},{"name":""}]}}`,
	} {
		if got, want := toolsrv.ShippedPacks([]byte(doc)), coremanifest.ShippedPacksOf([]byte(doc)); !slices.Equal(got, want) {
			t.Errorf("shippedPacks(%s) = %v, the core's %v", doc, got, want)
		}
	}
	u, err := url.Parse(imagejudge.DefaultBaseURL)
	if err != nil || u.Hostname() != toolsrv.ReviewHost || u.Scheme != "https" || (u.Port() != "" && u.Port() != "443") {
		t.Fatalf("ReviewHost %q, the judge asks %s", toolsrv.ReviewHost, imagejudge.DefaultBaseURL)
	}
}
