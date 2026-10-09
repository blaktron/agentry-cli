// Package reportthumbs holds a thumbnail of each built-in report theme: the
// first page of one sample report (runnerimage/toolsrv/testdata/pdf/
// thumbnail.md) rendered by the runner's own template and the pinned Typst
// image, as a PNG (plan report-designs.md §4 "The editor", D5;
// agentry-cli#784). The site's and the desktop's Report design pages show
// them, taking them with the CLI version they pin, so a thumbnail cannot
// drift from what a run draws: runnerimage/toolsrv's
// TestThumbnailsAreTheTemplates renders them again in the typst-image CI
// job and fails on any pixel that differs (AGENTRY_UPDATE_THUMBNAILS=1
// rewrites them).
//
// Nothing in the CLI binary imports this package.
package reportthumbs

import (
	"embed"
	"slices"

	"github.com/blaktron/agentry/pkg/reportdesign"
)

//go:embed *.png
var files embed.FS

// IDs are the built-in themes a thumbnail is kept for, in the order they
// are offered: the core's.
func IDs() []string { return reportdesign.BuiltinIDs() }

// PNG is a built-in theme's thumbnail.
func PNG(id string) ([]byte, bool) {
	if !slices.Contains(IDs(), id) {
		return nil, false
	}
	b, err := files.ReadFile(id + ".png")
	return b, err == nil
}
