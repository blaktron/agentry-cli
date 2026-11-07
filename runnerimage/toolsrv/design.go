package toolsrv

// The Report Designer's tools: render_preview lays out a fixed sample report
// in a report design, a custom cover included, with the renderer export_pdf
// uses and under the same confinement and caps, and answers one page as a
// PNG the model looks at, with Typst's diagnostics as text; check_design
// holds a design document to the core's rules (pkg/reportdesign) and
// answers what fails and the nearest colour that passes; view_reference
// shows the model one of the reference images the person gave the run.
// They are granted to the Report Designer built-in alone, and write nothing
// under reports/: a preview is kept under the run's receipts folder, where
// the CLI takes the thumbnail of the cover it keeps.

import (
	"bytes"
	"crypto/sha256"
	_ "embed" // the sample report
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/blaktron/agentry-cli/runnerimage/beneath"
	"github.com/blaktron/agentry-cli/runnerimage/reportdesign"
)

// The Report Designer's tools.
const (
	ToolRenderPreview = "render_preview"
	ToolCheckDesign   = "check_design"
	ToolViewReference = "view_reference"
)

// The limits of the Designer's tools.
const (
	// MaxPreviews is how many times render_preview renders in one run.
	MaxPreviews = 12
	// PreviewPPI is the resolution of a preview's PNG.
	PreviewPPI = 110
	// MaxReferenceViews is how many times view_reference shows an image in
	// one run.
	MaxReferenceViews = 12
	// maxAssetBytes caps an asset or a reference read from the inputs: the
	// CLI stages each as the core's re-encoding, under the core's caps.
	maxAssetBytes = 4 << 20
	// maxFactRunes caps each sample fact a preview takes.
	maxFactRunes = 200
)

// The inputs a Designer run is given, by their names under the inputs
// folder: the design to start from and its custom cover's code, the
// design's own images (the logo, the cover image, the images a custom cover
// may draw), and the reference images the person gave it to look at.
const (
	DesignInputDir       = "design"
	DesignInputFile      = "design/design.json"
	DesignInputCover     = "design/cover.typ"
	DesignAssetsDir      = "assets"
	DesignLogoAsset      = "assets/logo.png"
	DesignCoverImageBase = "assets/cover-image"
	DesignImagesDir      = "assets/images"
	DesignReferencesDir  = "references"
)

// The files a Designer run writes with write_report.
const (
	DesignDraftFile = "design.json"
	DesignCoverFile = "cover.typ"
)

// Design is the Report Designer's grant: render_preview, check_design,
// view_reference, document_pages, crop_logo and take_picture. The compiler sets Enabled
// for that built-in alone; the engine stamps the ledgers and the folder
// the previews are kept in.
type Design struct {
	Enabled bool `json:"enabled"`
	// Ledger is the container path each preview is recorded in, under
	// OutDir and outside reports/.
	Ledger string `json:"ledger,omitempty"`
	// Previews is the container folder each preview's PNG is kept in, as
	// preview-<n>.png, under OutDir and outside reports/.
	Previews string `json:"previews,omitempty"`
	// DocumentLedger is the container path each image document_pages
	// shows, each crop crop_logo takes and each picture take_picture takes
	// is recorded in, under OutDir and outside reports/.
	DocumentLedger string `json:"documentLedger,omitempty"`
}

// PreviewEntry is one line of the preview ledger: a render render_preview
// made.
type PreviewEntry struct {
	// N is the preview's number in the run, from 1.
	N int `json:"n"`
	// Page is the page rendered: "cover" or "body".
	Page string `json:"page"`
	// Cover is the cover style drawn.
	Cover string `json:"cover,omitempty"`
	// CoverSHA256 is the SHA-256 of the custom cover's source drawn, empty
	// when none was.
	CoverSHA256 string `json:"coverSha256,omitempty"`
	// DesignSHA256 is the SHA-256 of the design document drawn, without its
	// assets.
	DesignSHA256 string `json:"designSha256"`
	// PNG is the preview's file under Previews, empty when the page did not
	// render.
	PNG string `json:"png,omitempty"`
	// CoverFailure is the kind of failure that would pass the custom cover
	// over in an export, empty when it would be drawn.
	CoverFailure string `json:"coverFailure,omitempty"`
}

func validateDesign(spec *Spec) error {
	d := spec.Design
	if !d.Enabled && (d.Ledger != "" || d.Previews != "" || d.DocumentLedger != "") {
		return errors.New("design: a preview ledger without the Report Designer's grant")
	}
	for _, p := range []string{d.Ledger, d.Previews, d.DocumentLedger} {
		if p == "" {
			continue
		}
		if rel, err := filepath.Rel(spec.OutDir, p); !filepath.IsAbs(p) || err != nil || rel == "." || strings.HasPrefix(rel, "..") || rel == "reports" || strings.HasPrefix(rel, "reports"+string(filepath.Separator)) {
			return fmt.Errorf("design: %q must be an absolute path under outDir %q and outside reports/", p, spec.OutDir)
		}
	}
	if d.Enabled && spec.InputsDir == "" {
		return errors.New("design: the Report Designer's grant without an inputs folder")
	}
	return nil
}

//go:embed pdf/sample.md
var sampleReport string

const renderPreviewDescription = "Lay out a fixed sample report in a report design and look at one page of it as a PNG image, with Typst's own diagnostics as text: the renderer, fonts and confinement export_pdf uses. design is the design document (reports/design.json, scratch/<path> or inputs/<path>; default reports/design.json when you wrote one, else the design you were given). cover is a custom cover's Typst source (reports/cover.typ or scratch/<path>): when you give one, the cover is drawn from it, as an export would draw the design with that custom cover; a design whose cover style is custom draws reports/cover.typ when you wrote one. The logo, cover image and cover pictures you took from the person's document are drawn as the design's. page is cover (the default) or body (the first page after the cover). title, subtitle and author replace the sample's own. The answer says whether an export would pass the custom cover over and why. At most 12 renders a run; each answer says how many are left."

func renderPreviewSchema() map[string]any {
	return objSchema(map[string]any{
		"design":   strProp("the design document: reports/design.json, scratch/<path> or inputs/<path>"),
		"cover":    strProp("a custom cover's Typst source: reports/cover.typ or scratch/<path>"),
		"page":     map[string]any{"type": "string", "enum": []string{"cover", "body"}, "description": "the page to look at: cover (the default) or body"},
		"title":    strProp("the sample report's title, in place of its own"),
		"subtitle": strProp("the sample report's subtitle, in place of its own"),
		"author":   strProp("the author line, in place of the sample's"),
	})
}

var renderPreviewArgs = []string{"design", "cover", "page", "title", "subtitle", "author"}

const checkDesignDescription = "Check a report design document against the rules every design is held to before it is saved: the fields, the closed lists of faces and styles, the header and footer tokens, and the contrast rule (4.5:1 on white where a colour carries text, 3:1 where it only colours shapes). path is the document (default reports/design.json). The answer lists what fails, each colour's contrast and the minimum it needs, and the nearest shade of a failing colour that passes."

func checkDesignSchema() map[string]any {
	return objSchema(map[string]any{
		"path": strProp("the design document: reports/design.json (the default), scratch/<path> or inputs/<path>"),
	})
}

const viewReferenceDescription = "Look at one of the reference images the person gave this run, under inputs/references/, as an image. name is its name there (references/1.png). Read references for their look: the colours, the type, the layout. Never copy one into a design or a cover. At most 12 views a run."

func viewReferenceSchema() map[string]any {
	return objSchema(map[string]any{
		"name": strProp("the reference image: references/<n>.png or .jpg"),
	}, "name")
}

// designDoc reads a design document named as an argument, or the default:
// reports/design.json when the agent wrote one, else the given design, else
// Plain. It answers the design, the name it was read from and an error the
// agent can fix.
func (s *server) designDoc(arg string) (reportdesign.Design, string, error) {
	if arg == "" {
		reports := s.reportsRoot()
		if _, err := os.Lstat(filepath.Join(reports.dir, DesignDraftFile)); err == nil {
			arg = "reports/" + DesignDraftFile
		} else if _, err := os.Lstat(filepath.Join(s.spec.InputsDir, filepath.FromSlash(DesignInputFile))); err == nil {
			arg = "inputs/" + DesignInputFile
		} else {
			return reportdesign.Default(), "the built-in Plain design", nil
		}
	}
	from, rel, err := s.designSourceRoot(arg)
	if err != nil {
		return reportdesign.Design{}, arg, errAgentFixable{err.Error()}
	}
	src, err := readSource(from, rel)
	if err != nil {
		return reportdesign.Design{}, arg, err
	}
	name := from.name + "/" + path.Clean(rel)
	d, err := reportdesign.Parse([]byte(src))
	if err != nil {
		return reportdesign.Design{}, name, errAgentFixable{name + " is not a valid design: " + err.Error() + "; check it with check_design"}
	}
	return d, name, nil
}

// designSourceRoot is pdfSourceRoot with the inputs folder beside reports
// and scratch.
func (s *server) designSourceRoot(arg string) (pdfRoot, string, error) {
	trimmed := strings.TrimPrefix(arg, "/")
	if s.spec.InputsDir != "" {
		if rel, ok := strings.CutPrefix(arg, s.spec.InputsDir+"/"); ok {
			return pdfRoot{"inputs", s.spec.InputsDir}, rel, nil
		}
		if rel, ok := strings.CutPrefix(trimmed, "inputs/"); ok {
			return pdfRoot{"inputs", s.spec.InputsDir}, rel, nil
		}
	}
	from, rel, err := s.pdfSourceRoot(arg)
	if err != nil {
		return pdfRoot{}, "", err
	}
	if _, err := resolveUnder(from.dir, rel); err != nil {
		return pdfRoot{}, "", err
	}
	return from, rel, nil
}

// readAsset reads an image the CLI staged under the inputs folder, or nil
// when there is none.
func (s *server) readAsset(rel string) ([]byte, error) {
	root, err := beneath.Open(s.spec.InputsDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	f, fi, err := openNoLinks(root, rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if fi.Size() > maxAssetBytes {
		return nil, fmt.Errorf("inputs/%s is over %d MiB", rel, maxAssetBytes>>20)
	}
	return io.ReadAll(io.LimitReader(f, maxAssetBytes))
}

// assetNames lists the files of an inputs folder, sorted, or none.
func (s *server) assetNames(dir string) []string {
	entries, err := os.ReadDir(filepath.Join(s.spec.InputsDir, filepath.FromSlash(dir)))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			names = append(names, dir+"/"+e.Name())
		}
	}
	slices.SortFunc(names, func(a, b string) int {
		na, _ := strconv.Atoi(strings.TrimSuffix(path.Base(a), path.Ext(a)))
		nb, _ := strconv.Atoi(strings.TrimSuffix(path.Base(b), path.Ext(b)))
		if na != nb {
			return na - nb
		}
		return strings.Compare(a, b)
	})
	return names
}

// withAssets is d with the design's own images the run was given: the logo,
// the cover image and a custom cover's images, each as the CLI staged it
// (the core's re-encoding). A document the agent wrote carries none of its
// own; what it drew from is these.
func (s *server) withAssets(d reportdesign.Design) (reportdesign.Design, error) {
	if b, err := s.readAsset(DesignLogoAsset); err != nil {
		return d, err
	} else if b != nil {
		logo, err := reportdesign.NormalizeLogo(b)
		if err != nil {
			return d, fmt.Errorf("the logo: %w", err)
		}
		d.Logo = logo
	}
	// A logo taken from the person's document is drawn in place of the
	// design's own (crop_logo refuses one while they gave theirs), and so
	// is a cover image taken from it.
	if s.docLogo != (reportdesign.Logo{}) {
		d.Logo = s.docLogo
	}
	if s.docCoverImage != (reportdesign.Image{}) {
		d.CoverImage = s.docCoverImage
		return d, nil
	}
	for _, ext := range []string{".png", ".jpg"} {
		b, err := s.readAsset(DesignCoverImageBase + ext)
		if err != nil {
			return d, err
		}
		if b != nil {
			img, err := reportdesign.NormalizeCoverImage(b)
			if err != nil {
				return d, fmt.Errorf("the cover image: %w", err)
			}
			d.CoverImage = img
			break
		}
	}
	return d, nil
}

// coverImages are the images a custom cover may draw: as the CLI staged
// them, the design's own cover images and the ones the person gave; then
// the pictures take_picture took from the person's document for it.
func (s *server) coverImages() ([]reportdesign.Image, error) {
	var out []reportdesign.Image
	for _, name := range s.assetNames(DesignImagesDir) {
		b, err := s.readAsset(name)
		if err != nil {
			return nil, err
		}
		img, err := reportdesign.NormalizeCustomCoverImage(b)
		if err != nil {
			return nil, fmt.Errorf("inputs/%s: %w", name, err)
		}
		out = append(out, img)
	}
	out = append(out, s.docCoverPictures...)
	if out == nil {
		out = []reportdesign.Image{}
	}
	return out, nil
}

// previewText is the sample report with the facts the agent gave in place
// of its own.
func previewText(title, subtitle string) string {
	lines := strings.SplitN(sampleReport, "\n", 4)
	if title != "" {
		lines[0] = "# " + title
	}
	if subtitle != "" && len(lines) > 2 {
		lines[2] = "*" + subtitle + "*"
	}
	return strings.Join(lines, "\n")
}

// factArg is a sample fact as one line of plain text within maxFactRunes.
func factArg(name, v string) (string, error) {
	v = strings.Join(strings.Fields(v), " ")
	if n := len([]rune(v)); n > maxFactRunes {
		return "", fmt.Errorf("%s is %d characters, over the %d a sample fact may be", name, n, maxFactRunes)
	}
	for _, r := range v {
		if r == '*' || r == '_' || r == '#' || r == '`' || r == '<' || r == '[' {
			return "", fmt.Errorf("%s is plain text: it takes no Markdown", name)
		}
	}
	return v, nil
}

func (s *server) toolRenderPreview(raw json.RawMessage) (string, bool) {
	var keys map[string]json.RawMessage
	if err := unmarshalArgs(raw, &keys); err != nil {
		return "render_preview: " + err.Error(), true
	}
	for k := range keys {
		if !slices.Contains(renderPreviewArgs, k) {
			return fmt.Sprintf("render_preview: it takes %s, not %q", strings.Join(renderPreviewArgs, ", "), k), true
		}
	}
	var a struct {
		Design, Cover, Page, Title, Subtitle, Author string
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "render_preview: " + err.Error(), true
	}
	if !s.spec.Design.Enabled {
		return "render_preview: previews are not granted to this run", true
	}
	if s.render.refused != "" {
		return "render_preview: " + s.render.refused, true
	}
	if a.Page == "" {
		a.Page = "cover"
	}
	if a.Page != "cover" && a.Page != "body" {
		return fmt.Sprintf("render_preview: page %q is cover or body", a.Page), true
	}
	if s.previews >= MaxPreviews {
		return fmt.Sprintf("render_preview: this run has made its %d renders; write what you have", MaxPreviews), true
	}
	var err error
	for _, f := range []struct {
		name string
		v    *string
	}{{"title", &a.Title}, {"subtitle", &a.Subtitle}, {"author", &a.Author}} {
		if *f.v, err = factArg(f.name, *f.v); err != nil {
			return "render_preview: " + err.Error(), true
		}
	}

	d, designName, err := s.designDoc(a.Design)
	if err != nil {
		return answerFor(err, func(err error) (string, bool) { return s.failed(ToolRenderPreview, err) })
	}
	designSum := sha256.Sum256(d.WithoutAssets().Canonical())
	if d, err = s.withAssets(d); err != nil {
		return s.failed(ToolRenderPreview, err)
	}
	// The cover is drawn whenever the design has one at all, so that it
	// can be looked at; the body page is the first after it.
	if d.Cover.When != "always" {
		d.Cover.When = "always"
	}
	coverSum := ""
	if a.Cover != "" || (d.Cover.Style == "custom" && a.Cover == "") {
		var src string
		var name string
		if a.Cover != "" {
			from, rel, err := s.pdfSourceRoot(a.Cover)
			if err != nil {
				return "render_preview: " + err.Error(), true
			}
			if src, err = readSource(from, rel); err != nil {
				return answerFor(err, func(err error) (string, bool) { return s.failed(ToolRenderPreview, err) })
			}
			name = from.name + "/" + path.Clean(rel)
		} else if b, err := os.ReadFile(filepath.Join(s.reportsRoot().dir, DesignCoverFile)); err == nil && len(b) <= reportdesign.MaxCoverSourceBytes { //nolint:gosec // the run's own reports folder
			// A design whose cover is custom draws the cover this run
			// wrote, else the one it was given.
			src, name = string(b), "reports/"+DesignCoverFile
		} else {
			b, err := s.readAsset(DesignInputCover)
			if err != nil {
				return s.failed(ToolRenderPreview, err)
			}
			src, name = string(b), "inputs/"+DesignInputCover
		}
		if strings.TrimSpace(src) != "" {
			images, err := s.coverImages()
			if err != nil {
				return s.failed(ToolRenderPreview, err)
			}
			cc := reportdesign.CustomCover{Source: src, Images: images}
			if err := cc.Validate(); err != nil {
				return "render_preview: " + name + ": " + err.Error(), true
			}
			d.Cover.Style, d.CustomCover = "custom", cc
			sum := sha256.Sum256([]byte(src))
			coverSum = hex.EncodeToString(sum[:])
		}
	}

	s.previews++
	entry := PreviewEntry{N: s.previews, Page: a.Page, CoverSHA256: coverSum, DesignSHA256: hex.EncodeToString(designSum[:])}
	author := a.Author
	if author == "" {
		author = s.spec.PDF.Author
	}
	doc := reportMarkdown(previewText(a.Title, a.Subtitle), FormReport)
	res, failure, cover, err := s.renderPreview(d, doc, author, a.Page)
	if err != nil {
		var fixable errAgentFixable
		if errors.As(err, &fixable) {
			return "render_preview: " + err.Error(), true
		}
		return s.renderFailedAs(ToolRenderPreview, err)
	}
	entry.Cover, entry.CoverFailure = cover, failure

	var b strings.Builder
	left := MaxPreviews - s.previews
	fmt.Fprintf(&b, "render_preview %d of %d (%d left): the %s page of the sample report in %s (%q)", s.previews, MaxPreviews, left, a.Page, designName, d.Name)
	if cover != "" {
		fmt.Fprintf(&b, ", its cover drawn in the %s style", cover)
	}
	b.WriteString(".\n")
	switch {
	case d.Cover.Style == "custom" && failure == "":
		b.WriteString("The custom cover compiles as a tagged PDF and an export would draw it.\n")
	case failure != "":
		fmt.Fprintf(&b, "An export would pass this custom cover over for the built-in %s cover, because %s.\n", FallbackCoverStyle, CoverFailures[failure])
	}
	if res.diag != "" {
		b.WriteString("Typst's diagnostics:\n" + res.diag + "\n")
	}
	if res.failure != "" || len(res.out) == 0 {
		b.WriteString("The page did not render, so there is no image to look at.")
	} else {
		name := "preview-" + strconv.Itoa(s.previews) + ".png"
		if err := s.keepPreview(name, res.out); err != nil {
			s.logf("render_preview: the preview was not kept: %v", err)
		} else {
			entry.PNG = name
		}
		if cfg, err := pngSize(res.out); err == nil {
			fmt.Fprintf(&b, "The page is attached as a %d by %d PNG at %d dpi. If you cannot see it, say in your answer that you designed without looking at your work.", cfg.X, cfg.Y, PreviewPPI)
		}
		s.callImage = res.out
	}
	if err := s.appendPreview(entry); err != nil {
		s.logf("preview ledger: %v", err)
		return s.failed(ToolRenderPreview, fmt.Errorf("the preview could not be recorded: %w", err))
	}
	return b.String(), false
}

// renderPreview lays out doc in d and renders page ("cover" or "body") as a
// PNG. A custom cover is first compiled as an export would compile it, as a
// tagged PDF: a failure there is the kind it would be passed over for, and
// its diagnostics are the agent's to read, since the code is its own. The
// page is then rendered with the cover the export would draw.
func (s *server) renderPreview(d reportdesign.Design, doc mdDoc, author, page string) (typstResult, string, string, error) {
	logoPNG, logo, err := renderLogo(d)
	if err != nil {
		return typstResult{}, "", "", fmt.Errorf("the logo: %w", err)
	}
	coverBytes, coverImage, err := renderCoverImage(d)
	if err != nil {
		return typstResult{}, "", "", fmt.Errorf("the cover image: %w", err)
	}
	cover := coverStyle(d, coverImage)
	plan := coverPlan{}
	var customFiles map[string][]byte
	if cover == "custom" {
		var custom *pdfCustomCover
		if customFiles, custom, err = renderCustomCover(d); err != nil {
			return typstResult{}, "image", FallbackCoverStyle, nil
		}
		plan = coverPlan{style: "custom", custom: custom}
	}
	render := func(plan coverPlan, format string) (typstResult, error) {
		renderRoot, err := os.MkdirTemp(os.TempDir(), "agentry-preview-")
		if err != nil {
			return typstResult{}, err
		}
		defer func() { _ = os.RemoveAll(renderRoot) }()
		if err := os.Chmod(renderRoot, 0o755); err != nil {
			return typstResult{}, err
		}
		root, err := beneath.Open(renderRoot)
		if err != nil {
			return typstResult{}, err
		}
		defer func() { _ = root.Close() }()
		meta, err := renderMetaCover(d, FormReport, doc, doc.title, author, map[string]pdfImage{}, logo, coverImage, plan, time.Now())
		if err != nil {
			return typstResult{}, err
		}
		template, err := pdfFiles.ReadFile("pdf/report.typ")
		if err != nil {
			return typstResult{}, err
		}
		files := map[string][]byte{"main.typ": template, "report.md": []byte(doc.text), "design.json": renderDesign(d), "meta.json": meta}
		if logo != nil {
			files[logoFile] = logoPNG
		}
		style := plan.style
		if style == "" {
			style = cover
		}
		addCoverFiles(files, style, coverImage, coverBytes, customFiles)
		if err := root.Mkdir("img", 0o755); err != nil {
			return typstResult{}, err
		}
		if err := writeRenderRoot(root, files); err != nil {
			return typstResult{}, err
		}
		if format == "pdf" {
			return s.compileTypst(renderRoot, "pdf", 0, 0), nil
		}
		n := 1
		if page == "body" {
			n = 2
		}
		return s.compileTypst(renderRoot, "png", PreviewPPI, n), nil
	}
	failure := ""
	var diag string
	if plan.style == "custom" {
		check, err := render(plan, "pdf")
		if err != nil {
			return typstResult{}, "", "", err
		}
		if check.failure == "fault" {
			return typstResult{}, "", "", errors.New(check.fault)
		}
		if check.failure != "" {
			failure, diag = coverFailure(check), check.diag
			cover, plan = FallbackCoverStyle, coverPlan{style: FallbackCoverStyle}
		}
	}
	r, err := render(plan, "png")
	if err != nil {
		return typstResult{}, "", "", err
	}
	if r.failure == "fault" {
		return typstResult{}, "", "", errors.New(r.fault)
	}
	if diag != "" {
		r.diag = strings.TrimSpace(diag + "\n" + r.diag)
	}
	return r, failure, cover, nil
}

// pngSize is a PNG's width and height.
func pngSize(b []byte) (image.Point, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return image.Point{}, err
	}
	return image.Pt(cfg.Width, cfg.Height), nil
}

// keepPreview writes a preview's PNG under the previews folder, for the
// CLI to take the cover's thumbnail from.
func (s *server) keepPreview(name string, png []byte) error {
	dir := s.spec.Design.Previews
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	s.own(dir)
	root, err := beneath.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	f, err := root.CreateRegular(name, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(png)
	if err := errors.Join(werr, f.Close()); err != nil {
		return err
	}
	s.own(filepath.Join(dir, name))
	return nil
}

// appendPreview records a preview in the ledger, as appendPDF does.
func (s *server) appendPreview(e PreviewEntry) error {
	return s.appendLedger(s.spec.Design.Ledger, e)
}

// appendLedger appends v as one JSON line to the ledger at l, which the
// spec named; none, nothing is recorded.
func (s *server) appendLedger(l string, v any) error {
	if l == "" {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(l, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // the spec's own ledger path
	if err != nil {
		return err
	}
	s.own(l)
	_, werr := f.Write(append(b, '\n'))
	return errors.Join(werr, f.Close())
}

// renderFailedAs is renderFailed for another tool.
func (s *server) renderFailedAs(tool string, err error) (string, bool) {
	if errors.Is(err, syscall.ENOSPC) {
		return tool + ": the tools container's /tmp is full (" + err.Error() + "); remove what a command left there and try again", true
	}
	return s.failed(tool, err)
}

func (s *server) toolCheckDesign(raw json.RawMessage) (string, bool) {
	var a struct {
		Path string `json:"path"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "check_design: " + err.Error(), true
	}
	if !s.spec.Design.Enabled {
		return "check_design: checks are not granted to this run", true
	}
	if a.Path == "" {
		a.Path = "reports/" + DesignDraftFile
	}
	from, rel, err := s.designSourceRoot(a.Path)
	if err != nil {
		return "check_design: " + err.Error(), true
	}
	src, err := readSource(from, rel)
	if err != nil {
		return answerFor(err, func(err error) (string, bool) { return s.failed(ToolCheckDesign, err) })
	}
	name := from.name + "/" + path.Clean(rel)
	return checkDesignText(name, []byte(src)), false
}

// checkDesignText is check_design's answer for a document: what fails, and
// each colour's contrast with the nearest passing shade.
func checkDesignText(name string, src []byte) string {
	var b strings.Builder
	d, err := reportdesign.Parse(src)
	if err != nil {
		fmt.Fprintf(&b, "%s does not pass: %v\n", name, err)
		// The contrast lines still help when the colours parse.
		var loose struct {
			Accent, Secondary, TitleBlock, Headings string
			Cover                                   struct{ Style, When string }
		}
		if json.Unmarshal(src, &loose) != nil || loose.Accent == "" {
			return strings.TrimSpace(b.String())
		}
		d = reportdesign.Default()
		d.Accent, d.Secondary = loose.Accent, loose.Secondary
		if loose.TitleBlock != "" {
			d.TitleBlock = loose.TitleBlock
		}
		if loose.Headings != "" {
			d.Headings = loose.Headings
		}
		if loose.Cover.Style != "" {
			d.Cover.Style, d.Cover.When = loose.Cover.Style, loose.Cover.When
		}
	} else {
		fmt.Fprintf(&b, "%s passes: a valid design, %q.\n", name, d.Name)
	}
	colour := func(role, c string, min float64, carries bool) {
		ratio, err := reportdesign.Contrast(c, "#ffffff")
		if err != nil {
			fmt.Fprintf(&b, "- %s %s: %v\n", role, c, err)
			return
		}
		use := "colours only shapes, rules and tints"
		if carries {
			use = "carries text in this design"
		}
		fmt.Fprintf(&b, "- %s %s: %.2f:1 on white; it %s, so it needs %.1f:1", role, c, ratio, use, min)
		if ratio < min {
			if near, err := reportdesign.NearestPassing(c, min); err == nil {
				fmt.Fprintf(&b, ". It fails; the nearest shade that passes is %s", near)
			}
		}
		b.WriteString(".\n")
	}
	colour("accent", d.Accent, d.MinContrast(), d.AccentOnText())
	if d.Secondary != "" {
		colour("secondary", d.Secondary, d.MinSecondaryContrast(), d.SecondaryOnText())
	}
	return strings.TrimSpace(b.String())
}

func (s *server) toolViewReference(raw json.RawMessage) (string, bool) {
	var a struct {
		Name string `json:"name"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "view_reference: " + err.Error(), true
	}
	if !s.spec.Design.Enabled {
		return "view_reference: references are not granted to this run", true
	}
	names := s.assetNames(DesignReferencesDir)
	name := path.Clean(strings.TrimPrefix(strings.TrimPrefix(a.Name, "/"), "inputs/"))
	if !slices.Contains(names, name) {
		if len(names) == 0 {
			return "view_reference: this run was given no reference images", true
		}
		return fmt.Sprintf("view_reference: %q is not a reference image of this run; they are %s", a.Name, strings.Join(names, ", ")), true
	}
	if s.referenceViews >= MaxReferenceViews {
		return fmt.Sprintf("view_reference: this run has shown its %d references; work from what you saw", MaxReferenceViews), true
	}
	b, err := s.readAsset(name)
	if err != nil {
		return s.failed(ToolViewReference, err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || (format != "png" && format != "jpeg") {
		return fmt.Sprintf("view_reference: %s is not a PNG or a JPEG", name), true
	}
	s.referenceViews++
	s.callImage, s.callImageType = b, "image/"+format
	return fmt.Sprintf("%s, a %d by %d %s, is attached. If you cannot see it, say in your answer that you designed without looking at the references.", name, cfg.Width, cfg.Height, strings.ToUpper(format)), false
}
