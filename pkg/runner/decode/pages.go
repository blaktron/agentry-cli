package decode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/plainpath"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/docpages"
)

// Pages is what the runner image's pages role read of the Report
// Designer's document, held to its rules: the facts, and each page and
// picture as this process encoded it again from its decoded pixels, so no
// chunk or metadata the renderer or the document wrote survives.
type Pages struct {
	Facts docpages.Facts
	// Full are the rendered pages at docpages.RenderPPI, page 1 first, and
	// View the same pages at docpages.ViewPPI, each a PNG.
	Full, View [][]byte
	// Media are the document's pictures, each a PNG within
	// docpages.MaxPictureSide and docpages.MaxStagedMediaBytes, halved
	// until it fits: a Word document's, or the pictures a PDF's pages draw.
	Media [][]byte
	// Image is the decode image that read it.
	Image Image
}

// PagesTimeout ends the whole pages role: twelve pages at their limit,
// and the reads before them.
const PagesTimeout = 15 * time.Minute

// RunPages reads the one document at doc, of kind, with the pages role
// in the decode image: no network, no capabilities, the document's
// folder mounted read-only, the pages rendered in a bounded /tmp and
// written to out, a folder the caller made for it. A document the role
// could not read is a Pages with its Facts.Failure; the error is the
// container's own failure, or what it wrote breaking the rules.
func RunPages(ctx context.Context, d dockerx.Client, img Image, doc string, kind docpages.Kind, out string, lim policy.Limits, stderr io.Writer) (*Pages, error) {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return nil, err
	}
	name := "agentry-pages-" + hex.EncodeToString(suffix)
	user, err := dockerx.PersonUser(ctx, d)
	if err != nil {
		return nil, err
	}
	r := dockerx.RunSpec{
		Name:     name,
		Image:    img.Tag,
		Argv:     []string{"pages", "--in", "/in/" + filepath.Base(doc), "--kind", string(kind), "--out", "/out"},
		Network:  "none",
		Hostname: "localhost",
		Mounts: []dockerx.Mount{
			{Source: filepath.Dir(doc), Target: "/in", ReadOnly: true, Relabel: true},
			{Source: out, Target: "/out", Relabel: true},
		},
		User:       user,
		AutoRemove: true,
		Labels:     map[string]string{"agentry.decode": "1"},
	}.WithPosture(lim.Memory, lim.CPUs, lim.Pids)
	r.ReadOnly, r.Tmpfs = true, []string{"/tmp:size=512m,mode=1777"}
	ctx, cancel := context.WithTimeout(ctx, PagesTimeout)
	defer cancel()
	defer func() { _ = d.RemoveForce(context.WithoutCancel(ctx), name) }()
	var errOut bytes.Buffer
	code, err := d.RunForeground(ctx, r, nil, io.Discard, io.MultiWriter(stderr, &errOut))
	switch {
	case err != nil && ctx.Err() == context.DeadlineExceeded:
		return &Pages{Facts: docpages.Facts{Schema: docpages.Schema, Kind: kind, Failure: docpages.FailureTime}, Image: img}, nil
	case err != nil:
		return nil, fmt.Errorf("the pages container: %w", err)
	case code == 137:
		// The container's memory limit ended the role itself.
		return &Pages{Facts: docpages.Facts{Schema: docpages.Schema, Kind: kind, Failure: docpages.FailureMemory}, Image: img}, nil
	case code != 0:
		return nil, fmt.Errorf("the pages container failed (exit %d): %s", code, strings.TrimSpace(tail(errOut.String(), 400)))
	}
	p, err := CheckPages(out, kind)
	if err != nil {
		return nil, err
	}
	p.Image = img
	return p, nil
}

// CheckPages reads what the pages role wrote to out and holds it to its
// rules: the facts to docpages.Facts.Validate and to kind, each page and
// picture a regular file reached by no link, within its cap, decoded and
// encoded again. A PDF's colours are the ones its rendered pages show,
// found here.
func CheckPages(out string, kind docpages.Kind) (*Pages, error) {
	root, err := os.OpenRoot(out)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	raw, err := readRegular(root, docpages.FactsFile, docpages.MaxFactsBytes)
	if err != nil {
		return nil, fmt.Errorf("the pages role's %s: %w", docpages.FactsFile, err)
	}
	var f docpages.Facts
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("the pages role's %s does not decode: %w", docpages.FactsFile, err)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	if f.Kind != kind {
		return nil, fmt.Errorf("the pages role read a %s, not a %s", f.Kind, kind)
	}
	p := &Pages{Facts: f}
	var views []image.Image
	for n := 1; n <= f.Rendered; n++ {
		img, err := readImage(root, docpages.PageFile(n), docpages.MaxPageBytes, docpages.MaxPageSide+2, "png")
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", n, err)
		}
		view := docpages.Half(img)
		full, err := encodePNG(img)
		if err != nil {
			return nil, err
		}
		small, err := encodePNG(view)
		if err != nil {
			return nil, err
		}
		p.Full, p.View, views = append(p.Full, full), append(p.View, small), append(views, view)
	}
	for _, name := range f.Media {
		format := "png"
		if strings.HasSuffix(name, ".jpg") {
			format = "jpeg"
		}
		img, err := readImage(root, name, docpages.MaxMediaBytes, docpages.MaxMediaSide, format)
		if err != nil {
			return nil, fmt.Errorf("picture %s: %w", name, err)
		}
		for img.Bounds().Dx() > docpages.MaxPictureSide || img.Bounds().Dy() > docpages.MaxPictureSide {
			img = docpages.Half(img)
		}
		b, err := encodePNG(img)
		for err == nil && len(b) > docpages.MaxStagedMediaBytes {
			img = docpages.Half(img)
			b, err = encodePNG(img)
		}
		if err != nil {
			return nil, err
		}
		p.Media = append(p.Media, b)
	}
	if kind == docpages.KindPDF {
		p.Facts.Colours = docpages.PageColours(views)
	}
	return p, nil
}

// readImage reads one image the pages role wrote: a regular file within
// limit, of format, no side over maxSide, decoded only once its size is
// known.
func readImage(root *os.Root, name string, limit int64, maxSide int, format string) (image.Image, error) {
	if _, err := plainpath.File(root, name); err != nil {
		return nil, err
	}
	b, err := readRegular(root, name, limit)
	if err != nil {
		return nil, err
	}
	cfg, got, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || got != format {
		return nil, fmt.Errorf("not a %s", strings.ToUpper(format))
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxSide || cfg.Height > maxSide {
		return nil, fmt.Errorf("%dx%d is past %d pixels a side", cfg.Width, cfg.Height, maxSide)
	}
	var img image.Image
	if format == "png" {
		img, err = png.Decode(bytes.NewReader(b))
	} else {
		img, err = jpeg.Decode(bytes.NewReader(b))
	}
	if err != nil {
		return nil, fmt.Errorf("not a %s: %w", strings.ToUpper(format), err)
	}
	return img, nil
}

func encodePNG(img image.Image) ([]byte, error) {
	var b bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&b, img); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// PagesLine is the receipt's account of how the document was read.
func (p *Pages) PagesLine() string {
	f := p.Facts
	poppler := "poppler-utils"
	for _, pkg := range p.Image.Packages {
		if strings.HasPrefix(pkg, "poppler-utils-") {
			poppler = pkg
		}
	}
	where := "by the runner's pages role in the decode image (" + p.Image.Files + "), with no network and no capabilities, the document read-only, after the decode step checked it"
	switch {
	case f.Failure != "":
		return fmt.Sprintf("the document was not read: %s (%s); the run designed from the brief", docpages.Failures[f.Failure], where)
	case f.Kind == docpages.KindPDF:
		line := fmt.Sprintf("%d of the PDF's %d pages rendered at %d dpi by pdftoppm and %d picture(s) they draw read by pdfimages (%s) %s, each page and picture encoded again from its pixels by the CLI", f.Rendered, f.Pages, docpages.RenderPPI, len(f.Media), poppler, where)
		if f.Stopped != "" {
			line += fmt.Sprintf("; rendering stopped at page %d because %s", f.Rendered+1, docpages.Failures[f.Stopped])
		}
		return line
	default:
		return fmt.Sprintf("the Word document's theme, headers, footers and %d picture(s) read %s, each picture encoded again from its pixels by the CLI; no Word layout is rendered", len(f.Media), where)
	}
}
