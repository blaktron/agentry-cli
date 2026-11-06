// Package build runs the Report Designer: it stages what the person gave it
// (the design to start from without its images, the design's own images and
// theirs, each re-encoded from its pixels by pkg/reportdesign, their
// reference images, and the text of the web pages they named, read by AI
// Review first), runs the built-in over them (pkg/runner/localrun), and
// collects what it wrote: a design document, or a custom cover for a
// design. The result is held to the core's rules before anything keeps it,
// and it carries only images the person gave or the design already had:
// the model's document never brings an image of its own. The CLI writes the
// result to the run's output folder and, when asked, adds it to the
// account's library as a new design.
package build

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/blaktron/agentry/pkg/reportdesign"

	"github.com/blaktron/agentry-cli/pkg/plainpath"
	"github.com/blaktron/agentry-cli/pkg/reportdesigner"
	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/inference"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/sources"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	voicebuild "github.com/blaktron/agentry-cli/pkg/voice/build"
	"github.com/blaktron/agentry-cli/runnerimage/docpages"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// Job is which of the Report Designer's two jobs a run is.
type Job string

const (
	// Design writes a whole design document from a brief.
	Design Job = "design"
	// Cover writes a custom cover page for a design.
	Cover Job = "cover"
)

// The limits of one run's inputs.
const (
	// MaxBriefBytes caps the brief.
	MaxBriefBytes = 4 << 10
	// MaxReferences caps a Design run's reference images.
	MaxReferences = 4
	// MaxURLs caps a Design run's web addresses.
	MaxURLs = 2
)

// Options configure one run.
type Options struct {
	Job Job
	// Brief is what the person wants; required.
	Brief string
	// From is the design to start from (Design, optional) or the design
	// the cover is for (Cover, required), with its images.
	From *reportdesign.Design
	// Logo is a logo the person gave a Design run, as uploaded; it takes
	// the place of From's.
	Logo []byte
	// Images are a Design run's reference images, or the images a Cover
	// run's cover may draw beside From's own cover images, as uploaded.
	Images [][]byte
	// URLs are a Design run's web addresses (voicebuild.ParseURLs), read by
	// AI Review before the run: they take Review and Web.
	URLs []*sources.Source
	// Document is the one document a Design run builds its design
	// around, a PDF or a Word document; it goes through the decode step as
	// any input does.
	Document *Document
	Review   webscan.Scanner
	// Web is the web tools' configuration with the same scanner, for a page
	// whose text is drawn by scripts.
	Web *engine.WebRun

	Home       string
	Policy     *policy.Policy
	PolicySHA  string
	PolicyFile *policy.File
	Build      string
	// OutDir receives the run's reports and receipts, and the result under
	// design/.
	OutDir  string
	Budgets *spec.Budgets
	RunID   string
	Stderr  io.Writer
	OnUsage func(receipt.Usage)
	Credits *inference.Client
}

// Result is what a run made.
type Result struct {
	Envelope *receipt.Envelope
	// Design is the design the run made, with its images, valid by the
	// core's rules: never anyone's default until the person makes it so.
	Design reportdesign.Design
	// Looked is what the Designer says of its own work: that it saw its
	// renders. It is the model's word, never checked; false when it said it
	// could not read images, or wrote no notes.
	Looked bool
	// Thumbnail says a Cover run's cover carries the image of its last
	// render; false when the last render was not of the cover kept.
	Thumbnail bool
	// Previews is how many renders the run made.
	Previews int
	// Dir is the folder the result was written to: design/ under OutDir.
	Dir string
	// DocumentPages is how many images of the document the run looked
	// at; LogoFromDocument says the design's logo is a crop of it.
	DocumentPages    int
	LogoFromDocument bool
	// Pages are the web addresses given, and what became of each.
	Pages []*voicebuild.Page
}

// ErrNoDesign is a run that wrote no design or cover: the model's failure,
// never the person's.
var ErrNoDesign = errors.New("the Report Designer wrote no design")

// Run runs the Report Designer for o.Job and returns what it made.
func Run(ctx context.Context, d dockerx.Client, o Options) (*Result, error) {
	if err := o.check(); err != nil {
		return nil, engine.Classify(engine.ExitUsage, err)
	}
	logw := o.Stderr
	if logw == nil {
		logw = io.Discard
	}
	in, err := o.inputs()
	if err != nil {
		return nil, engine.Classify(engine.ExitUsage, err)
	}
	workRoot := filepath.Join(o.Home, "work")
	if err := os.MkdirAll(workRoot, 0o700); err != nil {
		return nil, engine.Classify(engine.ExitCLI, err)
	}
	work, err := os.MkdirTemp(workRoot, "design-build-")
	if err != nil {
		return nil, engine.Classify(engine.ExitCLI, err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	inputs, err := in.stage(filepath.Join(work, "in"))
	if err != nil {
		return nil, engine.Classify(engine.ExitCLI, err)
	}

	res := &Result{}
	pkg, web := localrun.ReportDesigner().WithoutWebTools(), (*engine.WebRun)(nil)
	var enforced map[string]string
	var doc *readDocument
	var decoded *decode.Result
	if o.Document != nil {
		if doc, err = o.readTheDocument(ctx, d, work, logw); err != nil {
			return res, err
		}
		inputs = append(inputs, doc.dir)
		decoded = doc.decoded
		enforced = map[string]string{"design.document": doc.pages.PagesLine()}
	}
	var pages *voicebuild.PagesResult
	if len(o.URLs) > 0 {
		pages, err = voicebuild.ReadPages(ctx, d, voicebuild.PagesOptions{
			URLs: o.URLs, Review: o.Review, NoRender: o.noRender(), Home: o.Home, Policy: o.Policy,
			RunID: o.RunID, OutDir: o.OutDir, Work: filepath.Join(work, "web"), Stderr: logw,
		})
		if pages != nil {
			res.Pages = pages.Pages
		}
		if err != nil {
			return res, err
		}
		if pages.Inputs != "" {
			inputs = append(inputs, pages.Inputs)
		}
		if len(pages.Given) > 0 {
			w := *o.Web
			w.Search, w.SearchKey, w.Given = "", "", pages.Given
			pkg, web = localrun.ReportDesigner(), &w
		}
		if pages.Decoded != nil {
			lines := voicebuild.PageLines(pages.Pages, pages.Decoded.Report.Files, "the Report Designer")
			if enforced == nil {
				enforced = lines
			} else {
				for k, v := range lines {
					enforced[k] = v
				}
			}
		}
	}

	env, runErr := localrun.Run{
		Builtin: pkg, Home: o.Home, Policy: o.Policy, PolicySHA: o.PolicySHA, PolicyFile: o.PolicyFile, Build: o.Build,
		Task: strings.NewReader(o.task(in, pages, doc)), Inputs: inputs, Decoded: decoded, Enforced: enforced, OutDir: o.OutDir, Budgets: o.Budgets,
		RunID: o.RunID, Stderr: logw, OnUsage: o.OnUsage, Web: web, Credits: o.Credits,
	}.Execute(ctx, d)
	res.Envelope = env
	if env != nil {
		res.Previews, res.DocumentPages = env.Previews, env.DocumentPages
	}
	made, err := o.collect(in, doc)
	if err != nil {
		if runErr != nil {
			return res, runErr
		}
		return res, engine.Classify(engine.ExitHarness, err)
	}
	if runErr != nil {
		_, _ = fmt.Fprintf(logw, "warning: the Report Designer stopped before it finished (%v); it had written its files, which are kept\n", runErr)
	}
	res.Design, res.Looked, res.Thumbnail, res.LogoFromDocument = made.design, made.looked, made.thumbnail, made.logoFromDocument
	if res.Dir, err = writeResult(o.OutDir, made.design); err != nil {
		return res, engine.Classify(engine.ExitCLI, err)
	}
	return res, nil
}

func (o Options) check() error {
	brief := strings.TrimSpace(o.Brief)
	switch {
	case brief == "":
		return errors.New("the Report Designer works from a brief: say what the reports are and how they should look (--brief)")
	case len(o.Brief) > MaxBriefBytes:
		return fmt.Errorf("a brief is at most %d KiB", MaxBriefBytes>>10)
	case !utf8.ValidString(o.Brief):
		return errors.New("a brief is UTF-8 text")
	}
	switch o.Job {
	case Design:
		if len(o.Images) > MaxReferences {
			return fmt.Errorf("a design takes at most %d reference images", MaxReferences)
		}
		if len(o.URLs) > MaxURLs {
			return fmt.Errorf("a design takes at most %d web addresses", MaxURLs)
		}
		if len(o.URLs) > 0 && (o.Review == nil || o.Web == nil || (o.Web.JevKey == "" && o.Web.JobToken == "")) {
			return voicebuild.ErrNoReviewKey
		}
		if o.Document != nil {
			if _, err := DocumentKind(o.Document.Name, o.Document.Data); err != nil {
				return err
			}
		}
	case Cover:
		if o.From == nil {
			return errors.New("a cover is written for one of your designs (--design)")
		}
		if o.Logo != nil || len(o.URLs) > 0 || o.Document != nil {
			return errors.New("a cover takes the design's own logo, no web addresses and no document")
		}
		if n := len(o.From.CustomCover.Images) + len(o.Images); n > reportdesign.MaxCoverImages {
			return fmt.Errorf("a cover draws at most %d images, and the design's cover already has %d", reportdesign.MaxCoverImages, len(o.From.CustomCover.Images))
		}
	default:
		return fmt.Errorf("unknown Report Designer job %q", o.Job)
	}
	return nil
}

// noRender is why this run cannot render a page drawn by scripts, "" when
// it can.
func (o Options) noRender() string {
	if o.Policy.Docker.Runtime != "" && len(o.Policy.Docker.Resolvers) == 0 {
		return "the runner cannot render a page under its runtime without the host's name servers (docker.resolvers)"
	}
	return ""
}

// staged is what a run is given, each image the core's re-encoding: the
// design to start from, its logo and cover image, the images a custom cover
// may draw, and the references.
type staged struct {
	from       *reportdesign.Design
	logo       reportdesign.Logo
	coverImage reportdesign.Image
	// coverImages are the images a cover may draw: From's own, then the
	// person's.
	coverImages []reportdesign.Image
	references  []reportdesign.Image
}

// inputs re-encodes everything the person gave, refusing what the core
// would refuse.
func (o Options) inputs() (staged, error) {
	var in staged
	if o.From != nil {
		f := *o.From
		in.from = &f
		if f.Logo != (reportdesign.Logo{}) {
			raw, err := f.Logo.Decode()
			if err != nil {
				return in, fmt.Errorf("the design's logo: %w", err)
			}
			if in.logo, err = reportdesign.NormalizeLogo(raw); err != nil {
				return in, fmt.Errorf("the design's logo: %w", err)
			}
		}
		if f.CoverImage != (reportdesign.Image{}) {
			raw, _, err := f.CoverImage.Decode()
			if err != nil {
				return in, fmt.Errorf("the design's cover image: %w", err)
			}
			if in.coverImage, err = reportdesign.NormalizeCoverImage(raw); err != nil {
				return in, fmt.Errorf("the design's cover image: %w", err)
			}
		}
		if o.Job == Cover {
			for i, img := range f.CustomCover.Images {
				raw, _, err := img.Decode()
				if err != nil {
					return in, fmt.Errorf("the design's cover image %d: %w", i+1, err)
				}
				n, err := reportdesign.NormalizeCustomCoverImage(raw)
				if err != nil {
					return in, fmt.Errorf("the design's cover image %d: %w", i+1, err)
				}
				in.coverImages = append(in.coverImages, n)
			}
		}
	}
	if o.Logo != nil {
		var err error
		if in.logo, err = reportdesign.NormalizeLogo(o.Logo); err != nil {
			return in, fmt.Errorf("--logo: %w", err)
		}
	}
	for i, raw := range o.Images {
		switch o.Job {
		case Cover:
			n, err := reportdesign.NormalizeCustomCoverImage(raw)
			if err != nil {
				return in, fmt.Errorf("image %d: %w", i+1, err)
			}
			in.coverImages = append(in.coverImages, n)
		default:
			n, err := reportdesign.NormalizeCoverImage(raw)
			if err != nil {
				return in, fmt.Errorf("reference image %d: %w", i+1, err)
			}
			in.references = append(in.references, n)
		}
	}
	return in, nil
}

// imageFile is an image's file name under a folder: n.png or n.jpg.
func imageFile(n int, img reportdesign.Image) string {
	if img.Format() == "jpeg" {
		return fmt.Sprintf("%d.jpg", n)
	}
	return fmt.Sprintf("%d.png", n)
}

// stage writes the inputs under dir in the layout the Designer's tools read
// (toolsrv's Design* names) and answers the folders to hand the run.
func (in staged) stage(dir string) ([]string, error) {
	write := func(rel string, b []byte) error {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, b, 0o644) //nolint:gosec // an input the run reads
	}
	decoded := func(img reportdesign.Image) []byte {
		b, _, _ := img.Decode()
		return b
	}
	if in.from != nil {
		if err := write(toolsrv.DesignInputFile, in.from.WithoutAssets().Canonical()); err != nil {
			return nil, err
		}
		if in.from.CustomCover.Source != "" {
			if err := write(toolsrv.DesignInputCover, []byte(in.from.CustomCover.Source)); err != nil {
				return nil, err
			}
		}
	}
	if in.logo != (reportdesign.Logo{}) {
		b, _ := in.logo.Decode()
		if err := write(toolsrv.DesignLogoAsset, b); err != nil {
			return nil, err
		}
	}
	if in.coverImage != (reportdesign.Image{}) {
		ext := ".png"
		if in.coverImage.Format() == "jpeg" {
			ext = ".jpg"
		}
		if err := write(toolsrv.DesignCoverImageBase+ext, decoded(in.coverImage)); err != nil {
			return nil, err
		}
	}
	for i, img := range in.coverImages {
		if err := write(toolsrv.DesignImagesDir+"/"+imageFile(i+1, img), decoded(img)); err != nil {
			return nil, err
		}
	}
	for i, img := range in.references {
		if err := write(toolsrv.DesignReferencesDir+"/"+imageFile(i+1, img), decoded(img)); err != nil {
			return nil, err
		}
	}
	var out []string
	for _, name := range []string{toolsrv.DesignInputDir, toolsrv.DesignAssetsDir, toolsrv.DesignReferencesDir} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out, nil
}

// task is the Report Designer's task: the job, what it was given, the brief.
func (o Options) task(in staged, pages *voicebuild.PagesResult, doc *readDocument) string {
	var b strings.Builder
	switch o.Job {
	case Design:
		b.WriteString("Job: Design. Write a whole report design, reports/" + reportdesigner.DesignFile + ", and reports/" + reportdesigner.NotesFile + ".\n")
		if in.from != nil {
			fmt.Fprintf(&b, "Start from the design %q, at inputs/%s.\n", in.from.Name, toolsrv.DesignInputFile)
		}
	case Cover:
		fmt.Fprintf(&b, "Job: Cover. Write a custom cover for the design %q (inputs/%s), reports/%s, and reports/%s naming the design it makes.\n", in.from.Name, toolsrv.DesignInputFile, reportdesigner.CoverFile, reportdesigner.NotesFile)
		if in.from.CustomCover.Source != "" {
			fmt.Fprintf(&b, "The design already has a custom cover, inputs/%s: you may start from it or write a new one.\n", toolsrv.DesignInputCover)
		}
	}
	if in.logo != (reportdesign.Logo{}) {
		fmt.Fprintf(&b, "The design's logo is inputs/%s.\n", toolsrv.DesignLogoAsset)
	}
	if in.coverImage != (reportdesign.Image{}) {
		b.WriteString("The design has a cover image, under inputs/" + toolsrv.DesignAssetsDir + "/.\n")
	}
	if n := len(in.coverImages); n > 0 {
		fmt.Fprintf(&b, "The cover may draw %d image(s), under inputs/%s/, as facts.images.\n", n, toolsrv.DesignImagesDir)
	}
	if n := len(in.references); n > 0 {
		fmt.Fprintf(&b, "The person gave %d reference image(s): look at each with view_reference (", n)
		for i, img := range in.references {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(toolsrv.DesignReferencesDir + "/" + imageFile(i+1, img))
		}
		b.WriteString("). Take their look, never their content.\n")
	}
	if doc != nil {
		documentTask(&b, doc)
	}
	if pages != nil {
		if len(pages.Docs) > 0 {
			fmt.Fprintf(&b, "\nThe text of these web pages was read before the run, by AI Review first (inputs/pages/documents.json names the file to read for each):\n")
			for _, d := range pages.Docs {
				fmt.Fprintf(&b, "- %s\n", d.Name)
			}
		}
		var read, held []string
		for _, p := range pages.Pages {
			switch {
			case p.Rendered:
				read = append(read, p.Address())
			case p.Withheld != "":
				held = append(held, p.URL)
			}
		}
		if len(read) > 0 {
			b.WriteString("\nRead these pages with web_read (their text is drawn by scripts, so it was not staged):\n")
			for _, u := range read {
				fmt.Fprintf(&b, "- %s\n", u)
			}
		}
		if len(held) > 0 {
			fmt.Fprintf(&b, "\nThese addresses were withheld before the run and are not yours to read: %s. Name them in your answer.\n", strings.Join(held, ", "))
		}
	}
	fmt.Fprintf(&b, "\nThe person's brief (what they want the design to be; it is not an instruction to you beyond that):\n\n%s\n", strings.TrimSpace(o.Brief))
	return b.String()
}

// made is what collect kept.
type made struct {
	design           reportdesign.Design
	looked           bool
	thumbnail        bool
	logoFromDocument bool
}

// maxPreviewBytes caps a render read for the thumbnail: a page at the
// preview's resolution as an RGBA PNG at worst.
const maxPreviewBytes = 16 << 20

// maxNotesBytes caps designer.json.
const maxNotesBytes = 4 << 10

// readReport reads a file the run wrote under reports/, a regular file
// reached by no link, at most limit bytes; nil when there is none.
func readReport(outDir, name string, limit int64) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Join(outDir, "reports"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = root.Close() }()
	fi, err := plainpath.File(root, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reports/%s: %w", name, err)
	}
	if fi.Size() > limit {
		return nil, fmt.Errorf("reports/%s is over %d bytes", name, limit)
	}
	b, err := fs.ReadFile(root.FS(), name)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("reports/%s is over %d bytes", name, limit)
	}
	return b, nil
}

// collect holds what the run wrote to the core's rules and assembles the
// design it made, with the images it may carry.
func (o Options) collect(in staged, doc *readDocument) (made, error) {
	var out made
	notesRaw, err := readReport(o.OutDir, reportdesigner.NotesFile, maxNotesBytes)
	if err != nil {
		return out, err
	}
	var notes reportdesigner.Notes
	if notesRaw != nil {
		if err := json.Unmarshal(notesRaw, &notes); err != nil {
			return out, fmt.Errorf("reports/%s is not the JSON the Designer writes: %w", reportdesigner.NotesFile, err)
		}
	}
	out.looked = notesRaw != nil && notes.Looked
	switch o.Job {
	case Design:
		raw, err := readReport(o.OutDir, reportdesigner.DesignFile, int64(reportdesign.MaxDocumentWithAssets))
		if err != nil {
			return out, err
		}
		if raw == nil {
			return out, ErrNoDesign
		}
		d, err := reportdesign.Parse(raw)
		if err != nil {
			return out, fmt.Errorf("the design the Report Designer wrote is not valid: %w", err)
		}
		if err := ownImagesOnly(d, in); err != nil {
			return out, err
		}
		// Its images are the person's and the design's, never the model's;
		// a logo taken from the person's document is cropped again here.
		d.Logo, d.CoverImage, d.CustomCover = in.logo, in.coverImage, reportdesign.CustomCover{}
		if logo, ok, err := o.documentLogo(doc); err != nil {
			return out, err
		} else if ok {
			d.Logo, out.logoFromDocument = logo, true
		}
		if in.from != nil && d.Cover.Style == "custom" && !in.from.CustomCover.IsZero() {
			cc, err := reencodeCover(in.from.CustomCover)
			if err != nil {
				return out, err
			}
			d.CustomCover = cc
		}
		if err := d.Validate(); err != nil {
			return out, fmt.Errorf("the design the Report Designer wrote is not valid: %w", err)
		}
		out.design = d
	case Cover:
		src, err := readReport(o.OutDir, reportdesigner.CoverFile, reportdesign.MaxCoverSourceBytes)
		if err != nil {
			return out, err
		}
		if src == nil {
			return out, errors.New("the Report Designer wrote no cover")
		}
		d := *in.from
		d.Logo, d.CoverImage = in.logo, in.coverImage
		d.Name = strings.TrimSpace(notes.Name)
		if d.Name == "" || utf8.RuneCountInString(d.Name) > reportdesign.MaxNameRunes {
			d.Name = coverName(in.from.Name)
		}
		d.Cover.Style = "custom"
		images := in.coverImages
		if images == nil {
			images = []reportdesign.Image{}
		}
		d.CustomCover = reportdesign.CustomCover{Source: string(src), Images: images}
		if thumb, ok := o.thumbnail(src, in.from.WithoutAssets().Canonical()); ok {
			d.CustomCover.Thumbnail, out.thumbnail = thumb, true
		}
		if err := d.Validate(); err != nil {
			return out, fmt.Errorf("the cover the Report Designer wrote is not valid: %w", err)
		}
		out.design = d
	}
	return out, nil
}

// coverName is the name of the design a Cover run makes when the run did
// not name it.
func coverName(from string) string {
	name := from + ", custom cover"
	if utf8.RuneCountInString(name) > reportdesign.MaxNameRunes {
		r := []rune(from)
		name = string(r[:reportdesign.MaxNameRunes-len(", custom cover")]) + ", custom cover"
	}
	return name
}

// ownImagesOnly refuses a document carrying an image that is neither the
// person's nor the design's own: the Designer writes no image.
func ownImagesOnly(d reportdesign.Design, in staged) error {
	allowed := map[string]bool{}
	add := func(b []byte, err error) {
		if err == nil && b != nil {
			sum := sha256.Sum256(b)
			allowed[hex.EncodeToString(sum[:])] = true
		}
	}
	add(in.logo.Decode())
	if in.from != nil {
		add(in.from.Logo.Decode())
		raw, _, err := in.from.CoverImage.Decode()
		add(raw, err)
		for _, img := range in.from.CustomCover.Images {
			raw, _, err := img.Decode()
			add(raw, err)
		}
	}
	raw, _, err := in.coverImage.Decode()
	add(raw, err)
	for _, img := range in.coverImages {
		raw, _, err := img.Decode()
		add(raw, err)
	}
	check := func(field string, b []byte, err error) error {
		if err != nil || len(b) == 0 {
			return nil
		}
		sum := sha256.Sum256(b)
		if !allowed[hex.EncodeToString(sum[:])] {
			return fmt.Errorf("the design the Report Designer wrote carries an image in %s that is neither yours nor the design's; it was not kept", field)
		}
		return nil
	}
	if d.Logo != (reportdesign.Logo{}) {
		b, err := d.Logo.Decode()
		if err := check("logo", b, err); err != nil {
			return err
		}
	}
	if d.CoverImage != (reportdesign.Image{}) {
		b, _, err := d.CoverImage.Decode()
		if err := check("coverImage", b, err); err != nil {
			return err
		}
	}
	for i, img := range d.CustomCover.Images {
		b, _, err := img.Decode()
		if err := check(fmt.Sprintf("customCover.images.%d", i), b, err); err != nil {
			return err
		}
	}
	if d.CustomCover.Thumbnail != (reportdesign.Image{}) {
		return errors.New("the design the Report Designer wrote carries a thumbnail of its own; it was not kept")
	}
	return nil
}

// reencodeCover is a custom cover with each image the core's re-encoding.
func reencodeCover(c reportdesign.CustomCover) (reportdesign.CustomCover, error) {
	out := reportdesign.CustomCover{Source: c.Source, Images: []reportdesign.Image{}}
	for i, img := range c.Images {
		raw, _, err := img.Decode()
		if err != nil {
			return out, fmt.Errorf("the design's cover image %d: %w", i+1, err)
		}
		n, err := reportdesign.NormalizeCustomCoverImage(raw)
		if err != nil {
			return out, fmt.Errorf("the design's cover image %d: %w", i+1, err)
		}
		out.Images = append(out.Images, n)
	}
	if c.Thumbnail != (reportdesign.Image{}) {
		raw, _, err := c.Thumbnail.Decode()
		if err == nil {
			if t, err := reportdesign.NormalizeThumbnail(raw); err == nil {
				out.Thumbnail = t
			}
		}
	}
	return out, nil
}

// thumbnail is the image of the run's last render of the cover page, when
// that render drew exactly src as a custom cover an export would draw, in
// the design the cover is for (design, its document without images),
// scaled down to fit the thumbnail's cap.
func (o Options) thumbnail(src, design []byte) (reportdesign.Image, bool) {
	entries, err := engine.ReadPreviewLedger(filepath.Join(o.OutDir, "receipts", engine.PreviewLedgerFile))
	if err != nil || len(entries) == 0 {
		return reportdesign.Image{}, false
	}
	sum, dsum := sha256.Sum256(src), sha256.Sum256(design)
	want, wantDesign := hex.EncodeToString(sum[:]), hex.EncodeToString(dsum[:])
	var last *toolsrv.PreviewEntry
	for i := range entries {
		e := entries[i]
		if e.Page == "cover" && e.Cover == "custom" && e.CoverSHA256 == want && e.DesignSHA256 == wantDesign && e.CoverFailure == "" && e.PNG != "" {
			last = &e
		}
	}
	if last == nil || filepath.Base(last.PNG) != last.PNG {
		return reportdesign.Image{}, false
	}
	root, err := os.OpenRoot(filepath.Join(o.OutDir, "receipts", engine.PreviewsDir))
	if err != nil {
		return reportdesign.Image{}, false
	}
	defer func() { _ = root.Close() }()
	fi, err := plainpath.File(root, last.PNG)
	if err != nil || fi.Size() > maxPreviewBytes {
		return reportdesign.Image{}, false
	}
	b, err := fs.ReadFile(root.FS(), last.PNG)
	if err != nil {
		return reportdesign.Image{}, false
	}
	return fitThumbnail(b)
}

// fitThumbnail scales a render down by halves until the core takes it as a
// thumbnail (at most 300 KB as a PNG).
func fitThumbnail(b []byte) (reportdesign.Image, bool) {
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return reportdesign.Image{}, false
	}
	for range 4 {
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return reportdesign.Image{}, false
		}
		if t, err := reportdesign.NormalizeThumbnail(buf.Bytes()); err == nil {
			return t, true
		}
		img = docpages.Half(img)
	}
	return reportdesign.Image{}, false
}

// writeResult writes the design under outDir/design: design.json, the
// whole design with its images, which `agentry report-design import`
// takes; and, for a custom cover, cover.typ, its images under cover/ and
// the thumbnail, to look at.
func writeResult(outDir string, d reportdesign.Design) (string, error) {
	dir := filepath.Join(outDir, "design")
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	write := func(rel string, b []byte) error {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, b, 0o644) //nolint:gosec // the person's own output
	}
	if err := write("design.json", d.Canonical()); err != nil {
		return "", err
	}
	if !d.CustomCover.IsZero() {
		if err := write("cover.typ", []byte(d.CustomCover.Source)); err != nil {
			return "", err
		}
		for i, img := range d.CustomCover.Images {
			b, _, _ := img.Decode()
			if err := write("cover/"+imageFile(i+1, img), b); err != nil {
				return "", err
			}
		}
		if d.CustomCover.Thumbnail != (reportdesign.Image{}) {
			b, _, _ := d.CustomCover.Thumbnail.Decode()
			if err := write("thumbnail.png", b); err != nil {
				return "", err
			}
		}
	}
	return dir, nil
}
