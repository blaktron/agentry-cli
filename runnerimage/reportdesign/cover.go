package reportdesign

import (
	"encoding/base64"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The custom cover: the one field of a design that is code. Its source is
// Typst, written by the person or by the Report Designer on their behalf,
// which the runner's template calls as a function of the facts it has (the
// title, the author, the date, the brand, the design's colours and faces,
// the logo's and the images' paths) to draw the cover page when the cover
// style is "custom". The renderer compiles it only under the confinement
// every export runs in, with the render root as its whole world: it can
// read the report, the design, the facts and the images copied there, none
// of which is secret, and it has no shell, no network, no package imports
// and no file writes. A custom cover that fails to compile, draws more or
// fewer than one page, or leaves an image untagged is passed over for the
// design's built-in cover style, and the export goes on.
//
// This package holds the cover to its bounds and carries it only inside a
// document it validated; it does not read the code.
const (
	// MaxCoverSourceBytes caps the custom cover's Typst source.
	MaxCoverSourceBytes = 32 << 10
	// MaxCoverImages caps the images a custom cover carries.
	MaxCoverImages = 6
	// MaxCoverImagesBytes caps a custom cover's images in all, as encoded
	// again.
	MaxCoverImagesBytes = 2 << 20
	// MaxThumbnailBytes caps the thumbnail, a PNG of the cover's last
	// render, kept for the editors.
	MaxThumbnailBytes = 300 << 10
)

// MaxDocumentWithAssets is the most a document may be as sent with every
// image at its largest and the custom cover at its bounds: the base64 of
// each image, the source escaped for JSON (a line break or a quote takes
// two bytes, a control character six) and a margin for the keys around
// them, over MaxDocumentBytes for the document itself. A server reads a
// design's body up to this.
var MaxDocumentWithAssets = MaxDocumentBytes +
	base64.StdEncoding.EncodedLen(MaxLogoBytes) +
	base64.StdEncoding.EncodedLen(MaxCoverImageBytes) +
	6*MaxCoverSourceBytes +
	base64.StdEncoding.EncodedLen(MaxCoverImagesBytes) +
	base64.StdEncoding.EncodedLen(MaxThumbnailBytes) +
	1<<10

// CustomCover is a design's custom cover. The zero CustomCover is none, and
// a design without one has no "customCover" key.
type CustomCover struct {
	// Source is the Typst source, at most MaxCoverSourceBytes.
	Source string `json:"source"`
	// Images are the images the source may draw, each a PNG or a JPEG the
	// core encoded again, in the order the renderer names them; at most
	// MaxCoverImages, and MaxCoverImagesBytes in all.
	Images []Image `json:"images"`
	// Thumbnail is a PNG of the cover's last render, at most
	// MaxThumbnailBytes; the zero Image is none, and is not written.
	Thumbnail Image `json:"thumbnail,omitzero"`
}

// IsZero reports whether the custom cover is none, which the canonical
// document leaves out.
func (c CustomCover) IsZero() bool {
	return c.Source == "" && len(c.Images) == 0 && c.Thumbnail == (Image{})
}

// customCover reads the optional custom cover field: nil is none; a source
// and an images list, and the thumbnail when it has one.
func (w *wireCustomCover) customCover() (CustomCover, error) {
	if w == nil {
		return CustomCover{}, nil
	}
	if w.Source == nil {
		return CustomCover{}, fieldErr("customCover.source", "is required")
	}
	if w.Images == nil {
		return CustomCover{}, fieldErr("customCover.images", "is required")
	}
	out := CustomCover{Source: *w.Source, Images: []Image{}}
	for i, wi := range *w.Images {
		img, err := wi.image(fmt.Sprintf("customCover.images.%d", i))
		if err != nil {
			return CustomCover{}, err
		}
		if img == (Image{}) {
			return CustomCover{}, fieldErr(fmt.Sprintf("customCover.images.%d", i), "must hold a png or a jpeg")
		}
		out.Images = append(out.Images, img)
	}
	thumb, err := w.Thumbnail.image("customCover.thumbnail")
	if err != nil {
		return CustomCover{}, err
	}
	out.Thumbnail = thumb
	return out, nil
}

// Validate holds the custom cover to its bounds: a source that is not
// empty, UTF-8 text with no control character but a tab, a line feed or a
// carriage return, at most MaxCoverSourceBytes; at most MaxCoverImages
// images, each a PNG or a JPEG within the cover image's pixel bound, and
// MaxCoverImagesBytes in all; a thumbnail that is a PNG of at most
// MaxThumbnailBytes.
func (c CustomCover) Validate() error {
	if strings.TrimSpace(c.Source) == "" {
		return fieldErr("customCover.source", "must not be empty; a design without a custom cover has no customCover key")
	}
	if n := len(c.Source); n > MaxCoverSourceBytes {
		return fieldErr("customCover.source", "is %d bytes, over the limit of %d", n, MaxCoverSourceBytes)
	}
	if !utf8.ValidString(c.Source) {
		return fieldErr("customCover.source", "must be UTF-8 text")
	}
	for _, r := range c.Source {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' || r == 0x7f {
			return fieldErr("customCover.source", "must be text: no control character but a tab or a line break")
		}
	}
	if len(c.Images) > MaxCoverImages {
		return fieldErr("customCover.images", "is %d images, over the limit of %d", len(c.Images), MaxCoverImages)
	}
	total := 0
	for i, img := range c.Images {
		field := fmt.Sprintf("customCover.images.%d", i)
		if err := checkImage(field, img, MaxCoverImagesBytes, MaxCoverImageSide, false); err != nil {
			return err
		}
		data, _, _ := img.Decode()
		total += len(data)
	}
	if total > MaxCoverImagesBytes {
		return imageErr(ErrImageTooLarge, "customCover.images", "are over 2 MB in all")
	}
	if c.Thumbnail != (Image{}) {
		return checkImage("customCover.thumbnail", c.Thumbnail, MaxThumbnailBytes, MaxCoverImageSide, true)
	}
	return nil
}

// NormalizeCustomCoverImage turns an uploaded image into one of a custom
// cover's images, by the cover image's rules (NormalizeCoverImage), named
// for its place in the list.
func NormalizeCustomCoverImage(upload []byte) (Image, error) {
	return normalizeImage("customCover.images", upload, MaxCoverImageUploadBytes, MaxCoverImagesBytes, MaxCoverImageSide)
}

// NormalizeThumbnail turns an uploaded image into a custom cover's
// thumbnail: a PNG of at most MaxThumbnailBytes, scaled to fit
// MaxCoverImageSide, encoded again from its pixels.
func NormalizeThumbnail(upload []byte) (Image, error) {
	img, err := normalizeImage("customCover.thumbnail", upload, MaxCoverImageUploadBytes, MaxCoverImageBytes, MaxCoverImageSide)
	if err != nil {
		return Image{}, err
	}
	if img.Format() != "png" {
		return Image{}, imageErr(ErrImageFormat, "customCover.thumbnail", "a thumbnail is a PNG")
	}
	if data, _, _ := img.Decode(); len(data) > MaxThumbnailBytes {
		return Image{}, imageErr(ErrImageTooLarge, "customCover.thumbnail", "a thumbnail is at most 300 KB as a PNG")
	}
	return img, nil
}
