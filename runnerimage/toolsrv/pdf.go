package toolsrv

// export_pdf: the agent turns a Markdown file it
// wrote, in reports/ or the scratch folder, into a PDF in reports/, laid out
// by the run's report design in the form the agent chose (report, letter or
// memo). Typst renders it
// inside the tools container, from a fresh render root under the bounded
// /tmp that holds the Markdown, the design, the server's own template and
// copies of the local images the Markdown names, and nothing else:
//
//   - Typst runs with a fixed argv and an empty environment, through the
//     runner binary's confine role: it may write nothing (Landlock), it may
//     not open a TCP connection where Landlock can refuse one (ABI 4), and
//     it gets a 1 GiB address space and 60 seconds; the PDF comes back on
//     its stdout. As root the server starts it as a user of its own
//     (typstID), which owns nothing; where there is no Landlock (gVisor)
//     that user is all that holds it.
//   - Its binary was built without a package downloader, and the
//     template imports only the vendored cmarker, with raw Typst off.
//   - Every file the server copies in is opened beneath its folder
//     through a beneath.Dir, and a name with a link anywhere in it is
//     refused; every name in the render root is made new through a
//     beneath.Dir on it. A declared command may write the scratch folder
//     and /tmp, and the server is root.
//
// The result is checked to be a PDF, copied into reports/ and recorded in
// the PDF ledger with its SHA-256, which the receipt repeats; with
// write_report refusing a PDF, it is the only way one lands in reports/.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // the cover image's format, for its size
	"image/png"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/runnerimage/beneath"
	"github.com/blaktron/agentry-cli/runnerimage/confine"
	"github.com/blaktron/agentry-cli/runnerimage/reportdesign"
)

// ToolExportPDF renders a Markdown file to a PDF in reports/.
const ToolExportPDF = "export_pdf"

// The limits of one export.
const (
	MaxPDFSourceBytes = 1 << 20
	MaxPDFImages      = 20
	MaxPDFImageBytes  = 10 << 20
	// MaxPDFBytes caps the PDF itself.
	MaxPDFBytes      = 64 << 20
	PDFRenderTimeout = 60 * time.Second
	PDFRenderMemory  = 1 << 30
)

// TypstDir is where the runner image holds the renderer (copied by digest
// from agentry-typst): the binary, the fonts and
// the vendored packages. It is outside every bin directory, so an exec
// image's minimize keeps it, and no declared command finds it on a PATH.
const TypstDir = "/opt/agentry/typst"

// TypstVersion is the Typst release in the pinned image, for the plan's
// enforcement text and the tool's errors.
const TypstVersion = "0.15.1"

// typstID is the uid and gid Typst runs as when the tools server is root:
// it owns nothing in the container, and it is not nobody, which a declared
// command runs as where there is no Landlock, so that one cannot signal or
// trace the other.
const typstID = 65533

// PDF is export_pdf's grant. The compiler sets Enabled with the scratch
// folder, for every package but a built-in; the engine stamps the
// rest into the run's own copy of the spec, never the compiled plan.
type PDF struct {
	Enabled bool `json:"enabled"`
	// Theme is the run's report design;
	// without one, the built-in default, Plain.
	Theme *ThemeStamp `json:"theme,omitempty"`
	// Author names the QuickAgent that wrote the report,
	// namespace/name@version, for the PDF's metadata: never the
	// person.
	Author string `json:"author,omitempty"`
	// Ledger is the container path each exported PDF is recorded in,
	// under OutDir.
	Ledger string `json:"ledger,omitempty"`
}

// PDFEntry is one line of the PDF ledger: a PDF export_pdf wrote. Path is
// relative to the output folder (reports/…).
type PDFEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Pages  int    `json:"pages"`
	Bytes  int64  `json:"bytes"`
	// Form is the form the agent chose for it.
	Form string `json:"form,omitempty"`
	// Logo says the person's logo was drawn in its title block.
	Logo bool `json:"logo,omitempty"`
	// Cover is the style of the cover page drawn before it, or empty when
	// it has none.
	Cover string `json:"cover,omitempty"`
	// CustomCover says what became of the design's custom cover when the
	// design asked for one: CustomCoverDrawn, or CustomCoverPassedOver
	// when the cover failed and Cover names the built-in style drawn in its
	// place. Empty when the design has none or no cover was drawn.
	CustomCover string `json:"customCover,omitempty"`
	// CoverFailure is the kind of failure a passed-over custom cover had,
	// one of CoverFailures: never the compiler's text, which could carry
	// the cover code's own words.
	CoverFailure string `json:"coverFailure,omitempty"`
}

// What became of a design's custom cover.
const (
	CustomCoverDrawn      = "drawn"
	CustomCoverPassedOver = "passedOver"
)

// CoverFailures are the kinds of failure a custom cover is passed over
// for, each with the words the answer gives it.
var CoverFailures = map[string]string{
	"compile": "its code did not compile",
	"import":  "it imports a package, which the renderer does not have",
	"altText": "it draws an image without alt text, which a tagged PDF refuses",
	"pages":   "it tries to draw more than one page",
	"title":   "it does not set the report's title with facts.title",
	"time":    "it took longer than the render's time limit",
	"memory":  "it ran out of the render's memory",
	"image":   "one of its images could not be re-encoded",
}

func validatePDF(spec *Spec) error {
	p := spec.PDF
	if !p.Enabled && (p.Theme != nil || p.Ledger != "") {
		return errors.New("pdf: a report design or a ledger without the export_pdf grant")
	}
	if p.Theme != nil {
		if _, err := p.Theme.check(); err != nil {
			return err
		}
	}
	if l := p.Ledger; l != "" {
		if rel, err := filepath.Rel(spec.OutDir, l); !filepath.IsAbs(l) || err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("pdf.ledger %q must be an absolute path under outDir %q", l, spec.OutDir)
		}
	}
	return nil
}

const exportPDFDescription = "Turn a Markdown file you wrote into a PDF in reports/, laid out in the person's report design. source is the Markdown: reports/<path> for one written with write_report, scratch/<path> for one in the scratch folder. path is the PDF under reports/ (default: the source's name with .pdf). form is the document's shape: report (the default; its first heading, when it opens the file, is drawn as the title, and a long titled report gets a cover page), letter (its first heading is the letterhead and each short line is kept, for addresses and a signature) or memo (its first heading is the title, and To:, From:, Cc:, Date:, Subject: or Re: lines opening the body become the memo's header). Images are drawn when they are local PNG or JPEG files, named relative to the Markdown file or as /scratch/…, /inputs/… or /out/reports/…; any other image, a web address included, is drawn as its alt text. You cannot choose the design: its colours, fonts and layout are the person's. Set deliverable to true, with a title, when the PDF is what the person asked this run for."

func exportPDFSchema() map[string]any {
	props := map[string]any{
		"source": strProp("the Markdown file: reports/<path> or scratch/<path>"),
		"path":   strProp("the PDF's path under reports/, ending in .pdf (default: the source's name with .pdf)"),
		"form":   map[string]any{"type": "string", "enum": Forms, "description": "the document's shape: report (the default), letter or memo"},
	}
	for k, v := range deliverableProps() {
		props[k] = v
	}
	return objSchema(props, "source")
}

// exportPDFArgs are export_pdf's arguments, and nothing else: an argument
// the model adds for the look ("theme", "accent", "font") is refused, never
// ignored, so the model learns it cannot choose one.
var exportPDFArgs = []string{"source", "path", "form", "deliverable", "title"}

// typstFiles is where the renderer is; a test points it elsewhere.
type typstFiles struct{ bin, fonts, packages string }

func defaultTypst() typstFiles {
	return typstFiles{bin: TypstDir + "/typst", fonts: TypstDir + "/fonts", packages: TypstDir + "/packages"}
}

// renderConfinement is how Typst is held, chosen once when the
// server starts, as run_command's confinement is.
type renderConfinement struct {
	// landlock is the ABI when Landlock holds Typst's writes, 0 otherwise.
	landlock int
	// noNetwork says Landlock refuses Typst TCP (ABI 4 and later).
	noNetwork bool
	// limitsOnly runs the trampoline with no Landlock (gVisor): Typst is
	// held by its user alone.
	limitsOnly bool
	// asTypst starts Typst as typstID.
	asTypst bool
	// refused, when set, is why no render runs.
	refused string
	// none is the off-Linux debugging path.
	none bool
	// trampoline is the confine role's argv prefix.
	trampoline []string
}

func chooseRenderConfinement(goos string, abi int, abiErr error, euid int, exe string) renderConfinement {
	switch {
	case goos != "linux":
		return renderConfinement{none: true}
	case abiErr == nil:
		return renderConfinement{landlock: abi, noNetwork: confine.NetworkDenied(abi), asTypst: euid == 0, trampoline: []string{exe, ConfineRole}}
	case euid != 0:
		return renderConfinement{refused: fmt.Sprintf("this machine cannot hold the renderer to its render root (%v), and the tools server cannot start it as another user", abiErr)}
	default:
		return renderConfinement{limitsOnly: true, asTypst: true, trampoline: []string{exe, ConfineRole}}
	}
}

func newRenderConfinement() renderConfinement {
	abi, abiErr := confine.ABI()
	exe, err := os.Executable()
	if err != nil && abiErr == nil {
		abiErr = fmt.Errorf("this binary cannot find itself: %w", err)
	}
	return chooseRenderConfinement(runtime.GOOS, abi, abiErr, os.Geteuid(), exe)
}

// argv starts prog with args, held as c says: under Landlock it may write
// nothing at all (but /dev/null), since the PDF comes back on its stdout.
func (c renderConfinement) argv(prog string, args []string) (string, []string) {
	if c.trampoline == nil {
		return prog, args
	}
	argv := append([]string{}, c.trampoline[1:]...)
	if c.limitsOnly {
		argv = append(argv, "--limits-only")
	} else if c.noNetwork {
		argv = append(argv, "--no-network")
	}
	argv = append(argv, "--max-memory", strconv.Itoa(PDFRenderMemory))
	return c.trampoline[0], append(append(argv, "--", prog), args...)
}

// describe is the sentence the server's log gives the render's holding.
func (c renderConfinement) describe() string {
	switch {
	case c.refused != "":
		return "no render runs: " + c.refused
	case c.limitsOnly:
		return fmt.Sprintf("Typst runs as uid %d, which owns nothing (no Landlock, so it may write where anyone may), in a %d GiB address space", typstID, PDFRenderMemory>>30)
	case c.landlock > 0:
		net := "TCP is not refused (Landlock ABI below 4)"
		if c.noNetwork {
			net = "TCP refused"
		}
		return fmt.Sprintf("Typst may write nothing (Landlock ABI %d), %s, in a %d GiB address space", c.landlock, net, PDFRenderMemory>>30)
	default:
		return "Typst is not confined (a debugging server off Linux)"
	}
}

// pdfRoot is a folder export_pdf reads from.
type pdfRoot struct {
	// name is how the answer spells a path beneath it: "reports" or
	// "scratch".
	name string
	dir  string
}

// reportsRoot is the run's reports folder, the one export_pdf writes.
func (s *server) reportsRoot() pdfRoot {
	return pdfRoot{"reports", filepath.Join(s.spec.OutDir, "reports")}
}

// pdfSourceRoot splits the source argument into its folder and the path
// beneath it. A bare path is a report's, as write_report spells them.
func (s *server) pdfSourceRoot(arg string) (pdfRoot, string, error) {
	reports := s.reportsRoot()
	trimmed := strings.TrimPrefix(arg, "/")
	switch {
	case s.spec.ScratchDir != "" && strings.HasPrefix(arg, s.spec.ScratchDir+"/"):
		return pdfRoot{"scratch", s.spec.ScratchDir}, strings.TrimPrefix(arg, s.spec.ScratchDir+"/"), nil
	case strings.HasPrefix(trimmed, "scratch/"):
		if s.spec.ScratchDir == "" {
			return pdfRoot{}, "", errors.New("this run has no scratch folder")
		}
		return pdfRoot{"scratch", s.spec.ScratchDir}, strings.TrimPrefix(trimmed, "scratch/"), nil
	case strings.HasPrefix(arg, s.spec.OutDir+"/reports/"):
		return reports, strings.TrimPrefix(arg, s.spec.OutDir+"/reports/"), nil
	case strings.HasPrefix(trimmed, "reports/"):
		return reports, strings.TrimPrefix(trimmed, "reports/"), nil
	default:
		return reports, arg, nil
	}
}

var (
	errNotRegular = errors.New("not a regular file")
	errNotImage   = errors.New("not a PNG or JPEG")
)

// errLink is a name with a link in it.
type errLink struct{ rel string }

func (e errLink) Error() string {
	return fmt.Sprintf("%q passes through a link, which export_pdf never follows", e.rel)
}

// openNoLinks opens rel beneath root without following a link anywhere in
// it, the last element included, and returns the regular file it names. The
// folder holds the open beneath it, and a link is refused by the same call
// that opens the file, so nothing swapped in between a look and an open can
// take the read elsewhere.
func openNoLinks(root *beneath.Dir, rel string) (*os.File, fs.FileInfo, error) {
	rel = path.Clean(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) {
		return nil, nil, fmt.Errorf("%q is not a file beneath its folder", rel)
	}
	f, err := root.OpenNoLinks(rel)
	if err != nil {
		if errors.Is(err, beneath.ErrLink) {
			return nil, nil, errLink{rel}
		}
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%q: %w", rel, errNotRegular)
	}
	return f, fi, nil
}

// pdfImage is a local image the render root holds a copy of.
type pdfImage struct {
	// Path is the copy's path in the render root, Name the file's own name
	// (the alt text of last resort).
	Path string `json:"path"`
	Name string `json:"name"`
}

// pdfMeta is meta.json: what the template is told besides the design.
type pdfMeta struct {
	Title string `json:"title"`
	// Subtitle is the line the report opened with under its title, or
	// empty.
	Subtitle string `json:"subtitle"`
	// Author is the QuickAgent's ref, or empty: never the person, nor
	// Agentry.
	Author string              `json:"author"`
	Images map[string]pdfImage `json:"images"`
	// Date is the day of the render (UTC, YYYY-MM-DD), for a report's cover
	// page: the same in every language.
	Date string `json:"date"`
	// Titled says the title is the report's own first heading, not its
	// file's name.
	Titled bool `json:"titled"`
	// Form is the form the agent chose: report, letter or memo.
	Form string `json:"form"`
	// TitleBlock says the report opened with its title, which the server
	// took out of report.md for the template to draw in the design's
	// title-block style.
	TitleBlock bool `json:"titleBlock"`
	// Cover puts a page of its own before a report that opens with its
	// title, when the design's cover says always, or says long and the
	// report runs past coverWords; the pages after it are numbered from 1.
	// A file name on a title page is worse than none, so an untitled
	// report never has one.
	Cover bool `json:"cover"`
	// CoverStyle is the style the cover is drawn in when Cover is set:
	// the design's, or band where the design's cannot be drawn (plate
	// without a cover image, custom).
	CoverStyle string `json:"coverStyle"`
	// CoverImage is the design's cover image as the server wrote it into
	// the render root, when a plate or a custom cover may draw it; nil
	// otherwise.
	CoverImage *pdfCoverImage `json:"coverImage"`
	// CustomCover is the custom cover's images as the server wrote them
	// into the render root, when the cover is drawn in the custom style;
	// nil otherwise.
	CustomCover *pdfCustomCover `json:"customCover"`
	// Tint is the accent's tint, pkg/reportdesign's Tint: the one the
	// site's editor previews.
	Tint string `json:"tint"`
	// Secondary and SecondaryTint are the design's secondary colour and
	// its tint, or empty when the design has none.
	Secondary     string `json:"secondary"`
	SecondaryTint string `json:"secondaryTint"`
	// Fonts are the family names Typst lists for the design's three faces,
	// which for a variable font can differ from the design's name.
	Fonts pdfFonts `json:"fonts"`
	// Memo is a memo's header, the lines the server took out of report.md.
	Memo []memoField `json:"memo"`
	// NotoBody and NotoHeadings are the Noto family behind the body and the
	// headings faces.
	NotoBody     string `json:"notoBody"`
	NotoHeadings string `json:"notoHeadings"`
	// Logo is the person's logo as the server wrote it into the render
	// root, or nil when the design has none.
	Logo *pdfLogo `json:"logo"`
	// Header and Footer are the design's slots, each split into its parts.
	Header pdfSlots `json:"header"`
	Footer pdfSlots `json:"footer"`
}

// pdfFonts names the Typst family for each face.
type pdfFonts struct {
	Headings string `json:"headings"`
	Body     string `json:"body"`
	Code     string `json:"code"`
}

// pdfCoverImage is the cover image the template draws: the file the
// server re-encoded it into, and its size in pixels.
type pdfCoverImage struct {
	Path   string `json:"path"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// pdfCustomCover is what the template is told of a custom cover: the
// paths of its images, in the design's order.
type pdfCustomCover struct {
	Images []pdfCoverImage `json:"images"`
}

// pdfSlots are a header's or a footer's three slots, each as its parts.
type pdfSlots struct {
	Slots [reportdesign.SlotCount][]slotPart `json:"slots"`
}

// slotPart is one piece of a slot: literal text the template sets as text,
// or the name of a token the template fills at each page (page, pages,
// section). Exactly one of the two is set.
type slotPart struct {
	Text  string `json:"text,omitempty"`
	Token string `json:"token,omitempty"`
}

// pageTokens are the tokens only the template can fill, page by page; the
// others are facts the server knows and puts in as text.
var pageTokens = []string{"page", "pages", "section"}

// slotParts splits a slot into its parts by pkg/reportdesign's rule for
// tokens (a name of its closed set between a brace pair with no brace
// inside; anything else is literal, the braces included), the static
// tokens filled from facts as text and the page tokens kept as tokens.
// An empty slot has no parts.
func slotParts(slot string, facts map[string]string) []slotPart {
	var parts []slotPart
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			parts = append(parts, slotPart{Text: text.String()})
			text.Reset()
		}
	}
	rest := slot
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			text.WriteString(rest)
			break
		}
		closing := strings.IndexByte(rest[open:], '}')
		if closing < 0 {
			text.WriteString(rest)
			break
		}
		name := rest[open+1 : open+closing]
		if !slices.Contains(reportdesign.Tokens, name) {
			text.WriteString(rest[:open+1])
			rest = rest[open+1:]
			continue
		}
		text.WriteString(rest[:open])
		if slices.Contains(pageTokens, name) {
			flush()
			parts = append(parts, slotPart{Token: name})
		} else {
			text.WriteString(facts[name])
		}
		rest = rest[open+closing+1:]
	}
	flush()
	return parts
}

// slotsOf splits three slots.
func slotsOf(slots [reportdesign.SlotCount]string, facts map[string]string) pdfSlots {
	var out pdfSlots
	for i, slot := range slots {
		out.Slots[i] = slotParts(slot, facts)
		if out.Slots[i] == nil {
			out.Slots[i] = []slotPart{}
		}
	}
	return out
}

// typstFamilies are the design families whose files Typst lists under
// another name: a variable font whose family name carries its default
// optical size. Every other family is listed as the design names it.
var typstFamilies = map[string]string{
	"DM Sans":     "DM Sans 9pt",
	"Nunito Sans": "Nunito Sans 12pt",
}

// TypstFamily is the family name Typst lists for a design family.
func TypstFamily(name string) string {
	if t, ok := typstFamilies[name]; ok {
		return t
	}
	return name
}

// pdfLogo is the logo the template draws: the file the server re-encoded it
// into, and its size in pixels, whose ratio the template keeps when it sizes
// it.
type pdfLogo struct {
	Path   string `json:"path"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// logoFile is the name of the logo in the render root.
const logoFile = "logo.png"

// normalizeLogo is reportdesign.NormalizeLogo; a test makes it fail.
var normalizeLogo = reportdesign.NormalizeLogo

// renderLogo is a design's logo as the render root gets it: the
// PNG reportdesign.NormalizeLogo writes from its decoded pixels, never the
// bytes the design carried. A design from a file or a flag may hold any PNG
// within the bounds (the core's Validate does not require its own
// encoding), so its chunks, profile and trailing bytes never reach Typst.
// Nil when the design has no logo.
func renderLogo(d reportdesign.Design) ([]byte, *pdfLogo, error) {
	if d.Logo == (reportdesign.Logo{}) {
		return nil, nil, nil
	}
	raw, err := d.Logo.Decode()
	if err != nil {
		return nil, nil, err
	}
	n, err := normalizeLogo(raw)
	if err != nil {
		return nil, nil, err
	}
	b, err := n.Decode()
	if err != nil {
		return nil, nil, err
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, nil, err
	}
	return b, &pdfLogo{Path: logoFile, Width: cfg.Width, Height: cfg.Height}, nil
}

// renderDesign is the design's document as the render root holds it: the
// canonical form without its images or its custom cover, which the root
// holds as files where they are drawn (logoFile, the cover image,
// coverSourceFile and the cover's images).
func renderDesign(d reportdesign.Design) []byte {
	return d.WithoutAssets().Canonical()
}

// coverImageFile is the name of the cover image in the render root, with
// its format's extension.
func coverImageFile(format string) string {
	if format == "jpeg" {
		return "cover.jpg"
	}
	return "cover.png"
}

// renderCoverImage is a design's cover image as the render root gets it:
// re-encoded from its decoded pixels by reportdesign.NormalizeCoverImage,
// never the bytes the design carried, so nothing of the upload but its
// pixels reaches Typst. Nil when the design has none.
func renderCoverImage(d reportdesign.Design) ([]byte, *pdfCoverImage, error) {
	if d.CoverImage == (reportdesign.Image{}) {
		return nil, nil, nil
	}
	raw, _, err := d.CoverImage.Decode()
	if err != nil {
		return nil, nil, err
	}
	n, err := reportdesign.NormalizeCoverImage(raw)
	if err != nil {
		return nil, nil, err
	}
	b, format, err := n.Decode()
	if err != nil {
		return nil, nil, err
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, nil, err
	}
	return b, &pdfCoverImage{Path: coverImageFile(format), Width: cfg.Width, Height: cfg.Height}, nil
}

// coverDrawn says whether a cover page is drawn: for a report that opens
// with its title, when the design says always, or says long and the report
// runs past coverWords.
func coverDrawn(d reportdesign.Design, form string, doc mdDoc) bool {
	if form != FormReport || !doc.titleBlock {
		return false
	}
	switch d.Cover.When {
	case "always":
		return true
	case "long":
		return doc.words >= coverWords
	}
	return false
}

// coverStyle is the style a drawn cover takes: the design's, or band where
// the design's cannot be drawn: plate without a cover image, and custom
// without a custom cover.
func coverStyle(d reportdesign.Design, coverImage *pdfCoverImage) string {
	switch d.Cover.Style {
	case "plate":
		if coverImage == nil {
			return "band"
		}
	case "custom":
		if d.CustomCover.IsZero() {
			return "band"
		}
	}
	return d.Cover.Style
}

// FallbackCoverStyle is the built-in style a custom cover that fails is
// passed over for. A design whose style is custom names no other, so it is
// the first of the built-in styles.
const FallbackCoverStyle = "band"

// coverSourceFile is the custom cover's code in the render root, which the
// template imports.
const coverSourceFile = "cover.typ"

// renderCustomCover is a design's custom cover as the render root gets it:
// its source as coverSourceFile and each image re-encoded from its decoded
// pixels by reportdesign.NormalizeCustomCoverImage under cover/<n>, so
// nothing of an upload but its pixels reaches Typst. It returns the files
// by their names in the root and what meta.json tells the template.
func renderCustomCover(d reportdesign.Design) (map[string][]byte, *pdfCustomCover, error) {
	files := map[string][]byte{coverSourceFile: []byte(d.CustomCover.Source)}
	meta := &pdfCustomCover{Images: []pdfCoverImage{}}
	for i, img := range d.CustomCover.Images {
		raw, _, err := img.Decode()
		if err != nil {
			return nil, nil, err
		}
		n, err := reportdesign.NormalizeCustomCoverImage(raw)
		if err != nil {
			return nil, nil, err
		}
		b, format, err := n.Decode()
		if err != nil {
			return nil, nil, err
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
		if err != nil {
			return nil, nil, err
		}
		ext := ".png"
		if format == "jpeg" {
			ext = ".jpg"
		}
		name := "cover/" + strconv.Itoa(i+1) + ext
		files[name] = b
		meta.Images = append(meta.Images, pdfCoverImage{Path: name, Width: cfg.Width, Height: cfg.Height})
	}
	return files, meta, nil
}

// coverFailure is the kind of failure a custom cover's render had, read
// from Typst's diagnostics, which never leave the server.
func coverFailure(r typstResult) string {
	switch r.failure {
	case "time", "memory":
		return r.failure
	}
	switch d := r.diag; {
	case strings.Contains(d, "the custom cover drew no title"):
		return "title"
	case strings.Contains(d, "missing alt text"):
		return "altText"
	case strings.Contains(d, "not allowed inside of containers"):
		return "pages"
	case strings.Contains(d, "failed to download package"), strings.Contains(d, "package downloads are disabled"):
		return "import"
	}
	return "compile"
}

var (
	pngMagic  = []byte("\x89PNG\r\n\x1a\n")
	jpegMagic = []byte("\xff\xd8\xff")
)

// errAgentFixable is an export the agent can fix itself (a limit, a name,
// a source that is not Markdown): its answer, never a fault of the run.
type errAgentFixable struct{ msg string }

func (e errAgentFixable) Error() string { return e.msg }

// answerFor is the tool's answer to err: the agent's own to fix, or a
// fault of the run's machinery through fail.
func answerFor(err error, fail func(error) (string, bool)) (string, bool) {
	var fixable errAgentFixable
	if errors.As(err, &fixable) {
		return ToolExportPDF + ": " + err.Error(), true
	}
	return fail(err)
}

// imageRoots are the folders an image may come from, each with the
// container path that names it absolutely.
func (s *server) imageRoots() []pdfRoot {
	roots := []pdfRoot{s.reportsRoot()}
	if s.spec.ScratchDir != "" {
		roots = append(roots, pdfRoot{"scratch", s.spec.ScratchDir})
	}
	if s.spec.InputsDir != "" {
		roots = append(roots, pdfRoot{"inputs", s.spec.InputsDir})
	}
	return roots
}

// resolveImage finds the folder and the path beneath it that an image
// source names: an absolute container path under one of imageRoots, or a
// path relative to the Markdown's own directory in its folder.
func (s *server) resolveImage(src string, from pdfRoot, fromDir string) (pdfRoot, string, bool) {
	if path.IsAbs(src) {
		for _, r := range s.imageRoots() {
			if rel, ok := strings.CutPrefix(src, r.dir+"/"); ok {
				return r, rel, true
			}
		}
		return pdfRoot{}, "", false
	}
	rel := path.Clean(path.Join(fromDir, src))
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return pdfRoot{}, "", false
	}
	return from, rel, true
}

// copyImages copies each local PNG or JPEG the Markdown names into the
// render root's img/, and says what it drew as alt text instead and why.
func (s *server) copyImages(doc mdDoc, from pdfRoot, fromDir string, renderRoot *beneath.Dir) (map[string]pdfImage, []string, error) {
	images := map[string]pdfImage{}
	copied := map[string]pdfImage{} // by folder and path, so a file is copied once
	opened := map[string]*beneath.Dir{}
	defer func() {
		for _, r := range opened {
			_ = r.Close()
		}
	}()
	var skipped []string
	for _, src := range doc.images {
		candidates := imageCandidates(src)
		if len(candidates) == 0 {
			skipped = append(skipped, src+" (not a local file)")
			continue
		}
		var why string
		found := false
		for _, cand := range candidates {
			r, rel, ok := s.resolveImage(cand, from, fromDir)
			if !ok {
				why = "outside the report, scratch and inputs folders"
				continue
			}
			key := r.name + "/" + rel
			if img, ok := copied[key]; ok {
				images[src] = img
				found = true
				break
			}
			root := opened[r.dir]
			if root == nil {
				var err error
				if root, err = beneath.Open(r.dir); err != nil {
					why = "its folder could not be opened"
					continue
				}
				opened[r.dir] = root
			}
			s.checked(r.name)
			img, err := copyImage(root, rel, renderRoot, len(copied)+1)
			var fixable errAgentFixable
			switch {
			case errors.As(err, &fixable):
				return nil, nil, err
			case err != nil:
				why = imageRefusal(err)
				continue
			}
			if len(copied) == MaxPDFImages {
				return nil, nil, errAgentFixable{fmt.Sprintf("the Markdown names more than %d images, the most export_pdf draws; use fewer", MaxPDFImages)}
			}
			copied[key], images[src] = img, img
			found = true
			break
		}
		if !found {
			skipped = append(skipped, src+" ("+why+")")
		}
	}
	return images, skipped, nil
}

// imageRefusal says, in the tool's words, why an image was not drawn.
func imageRefusal(err error) string {
	var link errLink
	switch {
	case errors.As(err, &link):
		return "reached through a link, which export_pdf never follows"
	case errors.Is(err, errNotImage):
		return "not a PNG or JPEG"
	case errors.Is(err, fs.ErrNotExist):
		return "no such file"
	case errors.Is(err, errNotRegular):
		return "not a regular file"
	default:
		return "it could not be read"
	}
}

// copyImage copies one image beneath root into the render root as img/<n>,
// when it is a regular PNG or JPEG of at most MaxPDFImageBytes reached by
// no link.
func copyImage(root *beneath.Dir, rel string, renderRoot *beneath.Dir, n int) (pdfImage, error) {
	f, fi, err := openNoLinks(root, rel)
	if err != nil {
		return pdfImage{}, err
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, len(pngMagic))
	k, _ := io.ReadFull(f, head)
	head = head[:k]
	ext := ""
	switch {
	case bytes.HasPrefix(head, pngMagic):
		ext = ".png"
	case bytes.HasPrefix(head, jpegMagic):
		ext = ".jpg"
	default:
		return pdfImage{}, errNotImage
	}
	if fi.Size() > MaxPDFImageBytes {
		return pdfImage{}, errAgentFixable{fmt.Sprintf("the image %s is %d bytes, over the %d MiB an image may be; make it smaller", rel, fi.Size(), MaxPDFImageBytes>>20)}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return pdfImage{}, err
	}
	name := "img/" + strconv.Itoa(n) + ext
	if err := createIn(renderRoot, name, io.LimitReader(f, MaxPDFImageBytes)); err != nil {
		return pdfImage{}, err
	}
	return pdfImage{Path: name, Name: path.Base(rel)}, nil
}

// pdfPagesCount matches a page tree node's /Count, in either order.
var pdfPagesCount = regexp.MustCompile(`/Type\s*/Pages\b[^>]*?/Count\s+([0-9]+)|/Count\s+([0-9]+)[^>]*?/Type\s*/Pages\b`)

// pdfPages is the page count the PDF's page tree gives: the largest
// /Count of a /Type /Pages node, which is the root's. 0 when none is
// readable (a compressed object stream).
func pdfPages(b []byte) int {
	n := 0
	for _, m := range pdfPagesCount.FindAllSubmatch(b, -1) {
		for _, g := range m[1:] {
			if v, err := strconv.Atoi(string(g)); err == nil && v > n {
				n = v
			}
		}
	}
	return n
}

func (s *server) toolExportPDF(raw json.RawMessage) (string, bool) {
	var keys map[string]json.RawMessage
	if err := unmarshalArgs(raw, &keys); err != nil {
		return "export_pdf: " + err.Error(), true
	}
	for k := range keys {
		if !slices.Contains(exportPDFArgs, k) {
			return fmt.Sprintf("export_pdf: it takes source, path, form, deliverable and title, not %q; the PDF's colours, fonts and layout are the person's report design, which you cannot choose", k), true
		}
	}
	var a struct {
		Source      string `json:"source"`
		Path        string `json:"path"`
		Form        string `json:"form"`
		Deliverable *bool  `json:"deliverable"`
		Title       string `json:"title"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "export_pdf: " + err.Error(), true
	}
	mark, err := parseDeliverableMark(ToolExportPDF, a.Deliverable, a.Title)
	if err != nil {
		return err.Error(), true
	}
	if a.Source == "" {
		return "export_pdf: source is required: the Markdown file to export, reports/<path> or scratch/<path>", true
	}
	if a.Form == "" {
		a.Form = FormReport
	}
	if !slices.Contains(Forms, a.Form) {
		return fmt.Sprintf("export_pdf: form %q is not one of %s", a.Form, strings.Join(Forms, ", ")), true
	}
	if !s.spec.PDF.Enabled {
		return "export_pdf: PDF export is not granted to this run", true
	}
	if s.render.refused != "" {
		return "export_pdf: " + s.render.refused, true
	}
	from, rel, err := s.pdfSourceRoot(a.Source)
	if err != nil {
		return "export_pdf: " + err.Error(), true
	}
	if _, err := resolveUnder(from.dir, rel); err != nil {
		return "export_pdf: " + err.Error(), true
	}
	rel = path.Clean(rel)
	out, err := pdfOutPath(a.Path, from, rel)
	if err != nil {
		return "export_pdf: " + err.Error(), true
	}
	reportsDir := filepath.Join(s.spec.OutDir, "reports")
	if err := os.MkdirAll(reportsDir, 0o755); err != nil {
		return s.failed(ToolExportPDF, err)
	}
	s.own(reportsDir)

	// 1. The source: a regular file beneath its folder, reached by no
	// link, at most 1 MiB of UTF-8, holding none of the run's secrets.
	s.checked(from.name)
	src, err := readSource(from, rel)
	if err != nil {
		return answerFor(err, func(err error) (string, bool) {
			return s.failed(ToolExportPDF, fmt.Errorf("%s/%s: %w", from.name, rel, err))
		})
	}
	if env := s.holdsRunSecret(src); env != "" {
		return "export_pdf: " + from.name + "/" + rel + " holds the value of this run's secret " + env + "; take the value out and write what it is and where it was found", true
	}
	design, err := s.pdfDesign()
	if err != nil {
		return s.failed(ToolExportPDF, err)
	}
	// The person's logo, re-encoded before the render root exists. One
	// that will not re-encode is the person's design, not the agent's
	// doing: the PDF is drawn without it, and the answer says so.
	logoPNG, logo, logoErr := renderLogo(design)
	if logoErr != nil {
		s.logf("export_pdf: the report design's logo was not drawn: %v", logoErr)
	}
	coverBytes, coverImage, coverErr := renderCoverImage(design)
	if coverErr != nil {
		s.logf("export_pdf: the report design's cover image was not drawn: %v", coverErr)
	}

	// 2. A fresh render root under /tmp. A declared command may write /tmp
	// (as root, under Landlock) and may have left a process running there,
	// so every name in the root is made new through a beneath.Dir opened on
	// it: a link planted in it is never followed out of it, and a name
	// taken first fails the export instead of being reused.
	renderRoot, err := os.MkdirTemp(os.TempDir(), "agentry-pdf-")
	if err != nil {
		return s.renderFailed(err)
	}
	defer func() { _ = os.RemoveAll(renderRoot) }()
	// Readable by Typst's own user; writable by root alone, and in the
	// sticky /tmp, never renamed by another user.
	if err := os.Chmod(renderRoot, 0o755); err != nil {
		return s.renderFailed(err)
	}
	root, err := beneath.Open(renderRoot)
	if err != nil {
		return s.renderFailed(err)
	}
	defer func() { _ = root.Close() }()
	if s.renderRootMade != nil {
		s.renderRootMade(renderRoot)
	}
	doc := reportMarkdown(src, a.Form)
	if err := root.Mkdir("img", 0o755); err != nil {
		return s.renderFailed(err)
	}
	s.checked("render")
	images, skipped, err := s.copyImages(doc, from, path.Dir(rel), root)
	if err != nil {
		return answerFor(err, s.renderFailed)
	}
	title := doc.title
	if title == "" {
		title = strings.TrimSuffix(path.Base(rel), path.Ext(rel))
	}
	now := time.Now()
	cover, plan := "", coverPlan{}
	var customFiles map[string][]byte
	customCover, coverFailed := "", ""
	if coverDrawn(design, a.Form, doc) {
		cover = coverStyle(design, coverImage)
		if cover == "custom" {
			// A custom cover whose images will not re-encode is passed over
			// before the render; one that fails in it, after.
			var custom *pdfCustomCover
			if customFiles, custom, err = renderCustomCover(design); err != nil {
				s.logf("export_pdf: the report design's custom cover was passed over: its images: %v", err)
				cover, customCover, coverFailed = FallbackCoverStyle, CustomCoverPassedOver, "image"
			} else {
				plan = coverPlan{style: "custom", custom: custom}
			}
		}
	}
	meta, err := renderMetaCover(design, a.Form, doc, title, s.spec.PDF.Author, images, logo, coverImage, plan, now)
	if err != nil {
		return s.renderFailed(err)
	}
	template, err := pdfFiles.ReadFile("pdf/report.typ")
	if err != nil {
		return s.renderFailed(err)
	}
	files := map[string][]byte{"main.typ": template, "report.md": []byte(doc.text), "design.json": renderDesign(design), "meta.json": meta}
	if logo != nil {
		files[logoFile] = logoPNG
	}
	if (cover == "plate" || cover == "custom") && coverImage != nil {
		files[coverImage.Path] = coverBytes
	}
	if cover == "custom" {
		if err := root.Mkdir("cover", 0o755); err != nil {
			return s.renderFailed(err)
		}
		maps.Copy(files, customFiles)
	}
	for name, b := range files {
		if err := createIn(root, name, bytes.NewReader(b)); err != nil {
			return s.renderFailed(err)
		}
	}

	// 3. Typst, which may write nothing: the PDF comes back on its stdout,
	// so no file a declared command could swap stands between the render
	// and reports/. A custom cover that fails is passed over: the report is
	// rendered again with the built-in cover in its place, and the answer
	// and the ledger name the kind of failure, never the compiler's words.
	source := from.name + "/" + rel
	r := s.compileTypst(renderRoot, "pdf", 0, 0)
	if cover == "custom" {
		if r.failure == "" {
			customCover = CustomCoverDrawn
		} else if r.failure != "fault" {
			cover, customCover, coverFailed = FallbackCoverStyle, CustomCoverPassedOver, coverFailure(r)
			s.logf("export_pdf: the report design's custom cover was passed over (%s)", coverFailed)
			meta, err := renderMetaCover(design, a.Form, doc, title, s.spec.PDF.Author, images, logo, coverImage, coverPlan{style: cover}, now)
			if err != nil {
				return s.renderFailed(err)
			}
			if err := root.Remove("meta.json"); err != nil {
				return s.renderFailed(err)
			}
			if err := createIn(root, "meta.json", bytes.NewReader(meta)); err != nil {
				return s.renderFailed(err)
			}
			r = s.compileTypst(renderRoot, "pdf", 0, 0)
		}
	}
	if r.failure != "" {
		return s.typstAnswer(r, source), true
	}
	pdf := r.out

	// 4. The result: a PDF under the cap, copied into reports/ and
	// recorded.
	if err := checkPDF(pdf); err != nil {
		return answerFor(err, s.renderFailed)
	}
	if err := s.writePDF(reportsDir, out, pdf); err != nil {
		return s.failed(ToolExportPDF, err)
	}
	sum := sha256.Sum256(pdf)
	pages := pdfPages(pdf)
	if err := s.appendPDF(PDFEntry{Path: "reports/" + out, SHA256: hex.EncodeToString(sum[:]), Pages: pages, Bytes: int64(len(pdf)), Form: a.Form, Logo: logo != nil, Cover: cover, CustomCover: customCover, CoverFailure: coverFailed}); err != nil {
		s.logf("pdf ledger: %v", err)
		return s.failed(ToolExportPDF, fmt.Errorf("the PDF was written to reports/%s, but the run could not record it, so it will not be shown as this run's export: %w", out, err))
	}
	marked, err := s.markDeliverable(out, mark)
	if err != nil {
		s.logf("deliverables ledger: %v", err)
		return s.failed(ToolExportPDF, fmt.Errorf("the PDF was written to reports/%s, but the run could not record it as a deliverable: %w", out, err))
	}
	count := "? pages"
	if pages == 1 {
		count = "1 page"
	} else if pages > 1 {
		count = strconv.Itoa(pages) + " pages"
	}
	withLogo := ""
	switch {
	case logo != nil:
		withLogo = ", with the person's logo"
	case logoErr != nil:
		withLogo = "; the person's logo could not be drawn, which is their design's to fix, not yours"
	}
	withCover := ""
	switch {
	case customCover == CustomCoverPassedOver:
		withCover = fmt.Sprintf(", with a %s cover page in place of the design's custom cover, which was passed over because %s; that is the person's design to fix, not yours", cover, CoverFailures[coverFailed])
	case cover != "":
		withCover = fmt.Sprintf(", with a %s cover page", cover)
	case coverErr != nil:
		withCover = "; the person's cover image could not be drawn, which is their design's to fix, not yours"
	}
	answer := fmt.Sprintf("report written: reports/%s (%s, %d KiB; the %s form, in the %s report design%s%s)%s", out, count, (len(pdf)+1023)/1024, a.Form, design.Name, withLogo, withCover, marked)
	if len(skipped) > 0 {
		answer += "\nDrawn as their alt text, not as images: " + strings.Join(skipped, "; ")
	}
	return answer, false
}

// pdfOutPath is the PDF's path under reports/: the one asked for, or the
// source's name with .pdf (a report's beside it, a scratch file's at the
// top of reports/).
func pdfOutPath(asked string, from pdfRoot, rel string) (string, error) {
	out := strings.TrimPrefix(strings.TrimPrefix(asked, "/"), "reports/")
	if asked == "" {
		out = strings.TrimSuffix(rel, path.Ext(rel)) + ".pdf"
		if from.name != "reports" {
			out = path.Base(out)
		}
	}
	out = path.Clean(out)
	if !strings.EqualFold(path.Ext(out), ".pdf") {
		return "", fmt.Errorf("path %q must end in .pdf", asked)
	}
	if out == ".pdf" || out == ".." || strings.HasPrefix(out, "../") || path.IsAbs(out) {
		return "", fmt.Errorf("path %q is not a file beneath reports/", asked)
	}
	return out, nil
}

func readSource(from pdfRoot, rel string) (string, error) {
	root, err := beneath.Open(from.dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	f, fi, err := openNoLinks(root, rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", errAgentFixable{fmt.Sprintf("%s/%s does not exist; write the report with write_report (or scratch_write) first", from.name, rel)}
		}
		if !isFault(err) {
			return "", errAgentFixable{err.Error()}
		}
		return "", err
	}
	defer func() { _ = f.Close() }()
	if fi.Size() > MaxPDFSourceBytes {
		return "", errAgentFixable{fmt.Sprintf("%s/%s is %d bytes, over the %d MiB a source may be", from.name, rel, fi.Size(), MaxPDFSourceBytes>>20)}
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxPDFSourceBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > MaxPDFSourceBytes {
		return "", errAgentFixable{fmt.Sprintf("%s/%s is over the %d MiB a source may be", from.name, rel, MaxPDFSourceBytes>>20)}
	}
	if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return "", errAgentFixable{fmt.Sprintf("%s/%s is not UTF-8 text; export_pdf takes Markdown", from.name, rel)}
	}
	return string(b), nil
}

// pdfDesign is the run's report design: the one the engine stamped, or the
// built-in default.
func (s *server) pdfDesign() (reportdesign.Design, error) {
	if st := s.spec.PDF.Theme; st != nil {
		return st.check()
	}
	return reportdesign.Default(), nil
}

// coverPlan is how one render draws a drawn cover: in style, with the
// custom cover's images when style is custom. The zero coverPlan draws
// the design's built-in style, a custom one as FallbackCoverStyle.
type coverPlan struct {
	style  string
	custom *pdfCustomCover
}

// renderMeta is meta.json: what the template is told besides the design,
// with a custom cover drawn in its built-in place.
func renderMeta(d reportdesign.Design, form string, doc mdDoc, title, author string, images map[string]pdfImage, logo *pdfLogo, coverImage *pdfCoverImage, now time.Time) ([]byte, error) {
	return renderMetaCover(d, form, doc, title, author, images, logo, coverImage, coverPlan{}, now)
}

// renderMetaCover is meta.json, the cover drawn as plan says.
func renderMetaCover(d reportdesign.Design, form string, doc mdDoc, title, author string, images map[string]pdfImage, logo *pdfLogo, coverImage *pdfCoverImage, plan coverPlan, now time.Time) ([]byte, error) {
	tint, err := reportdesign.Tint(d.Accent)
	if err != nil {
		return nil, err
	}
	secondaryTint := ""
	if d.Secondary != "" {
		if secondaryTint, err = reportdesign.Tint(d.Secondary); err != nil {
			return nil, err
		}
	}
	memo := doc.memo
	if memo == nil {
		memo = []memoField{}
	}
	if images == nil {
		images = map[string]pdfImage{}
	}
	date := now.UTC().Format(time.DateOnly)
	cover := coverDrawn(d, form, doc)
	style := ""
	var custom *pdfCustomCover
	if cover {
		style = coverStyle(d, coverImage)
		switch {
		case plan.style != "":
			style = plan.style
		case style == "custom":
			style = FallbackCoverStyle
		}
		if style == "custom" {
			custom = plan.custom
		}
	}
	// The cover image is in the render root for a plate cover, which draws
	// it, and for a custom one, which may.
	if style != "plate" && style != "custom" {
		coverImage = nil
	}
	// The static tokens, as text: a slot never evaluates anything.
	facts := map[string]string{"title": title, "author": author, "date": date, "brand": d.Brand.Name, "confidentiality": d.Brand.Confidentiality}
	return json.Marshal(pdfMeta{
		Title: title, Subtitle: doc.subtitle, Author: author, Images: images, Date: date, Titled: doc.title != "",
		Form: form, TitleBlock: doc.titleBlock, Cover: cover, CoverStyle: style, CoverImage: coverImage, CustomCover: custom,
		Tint: tint, Secondary: d.Secondary, SecondaryTint: secondaryTint,
		Fonts: pdfFonts{Headings: TypstFamily(d.Fonts.Headings), Body: TypstFamily(d.Fonts.Body), Code: TypstFamily(d.Fonts.Code)},
		Memo:  memo, NotoBody: notoFor(d.Fonts.Body), NotoHeadings: notoFor(d.Fonts.Headings),
		Logo:   logo,
		Header: slotsOf(d.Header.Slots, facts),
		Footer: slotsOf(d.Footer.Slots, facts),
	})
}

// notoFor is the Noto family set behind a design family for the scripts it
// lacks: Noto Serif behind a serif, Noto Sans behind anything else.
func notoFor(family string) string {
	if f, ok := reportdesign.FamilyByName(family); ok && f.Kind == reportdesign.Serif {
		return "Noto Serif"
	}
	return "Noto Sans"
}

// typstResult is one render's outcome: what Typst wrote to its stdout,
// or why it wrote nothing.
type typstResult struct {
	out []byte
	// failure is empty when the render succeeded; otherwise "time",
	// "memory" or "compile" when Typst ran and failed, and "fault" when it
	// could not run or be confined.
	failure string
	// diag is the compiler's diagnostics for a failure, with the render
	// root's path and the run's secrets taken out. It goes to the agent
	// alone, never into a ledger or the receipt.
	diag string
	// fault is the reason for a "fault".
	fault string
}

// compileTypst renders root/main.typ with Typst, held as s.render says, to
// what format names on its stdout: a tagged PDF ("pdf"), or page as a PNG
// at ppi ("png"). Typst may write no file, so a PNG is one page.
func (s *server) compileTypst(root, format string, ppi, page int) typstResult {
	if _, err := os.Stat(s.typst.bin); err != nil {
		return typstResult{failure: "fault", fault: fmt.Sprintf("this runner image has no PDF renderer (%v); the runner image must be rebuilt", err)}
	}
	args := []string{"compile", "--root", root, "--ignore-system-fonts", "--font-path", s.typst.fonts,
		"--package-path", s.typst.packages, "--package-cache-path", filepath.Join(root, ".packages"),
		"--diagnostic-format", "short"}
	if format == "png" {
		args = append(args, "--format", "png", "--ppi", strconv.Itoa(ppi), "--pages", strconv.Itoa(max(page, 1)))
	} else {
		args = append(args, "--pdf-standard", "ua-1")
	}
	args = append(args, filepath.Join(root, "main.typ"), "-")
	ctx, cancel := context.WithTimeout(context.Background(), s.pdfTimeout)
	defer cancel()
	name, argv := s.render.argv(s.typst.bin, args)
	cmd := exec.CommandContext(ctx, name, argv...)
	// An empty environment, not this server's: it holds the run's secrets.
	cmd.Env = []string{}
	cmd.Dir = root
	if s.render.asTypst {
		runAs(cmd, typstID)
	}
	stderr, stdout := &capWriter{max: 16 << 10}, &pdfOut{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	failed, err := s.render.run(cmd)
	if failed != "" {
		return typstResult{failure: "fault", fault: "the renderer could not be started confined: " + failed}
	}
	if err == nil {
		return typstResult{out: stdout.buf.Bytes()}
	}
	diag := s.scrubSecrets(strings.TrimSpace(strings.ReplaceAll(stderr.text(), root+"/", "")))
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return typstResult{failure: "time", diag: diag}
	case strings.Contains(diag, "memory allocation of"), errors.As(err, &exitErr) && killedBy(exitErr, syscall.SIGABRT, syscall.SIGKILL, syscall.SIGSEGV):
		return typstResult{failure: "memory", diag: diag}
	case errors.As(err, &exitErr):
		return typstResult{failure: "compile", diag: diag}
	default:
		return typstResult{failure: "fault", fault: "the renderer did not run: " + err.Error()}
	}
}

// typstAnswer is export_pdf's answer for a failed render of source.
func (s *server) typstAnswer(r typstResult, source string) string {
	switch r.failure {
	case "fault":
		s.callFault = true
		return "export_pdf: " + r.fault
	case "time":
		return fmt.Sprintf("export_pdf: the render took longer than its %s limit; shorten %s or draw fewer, smaller images", s.pdfTimeout, source)
	case "memory":
		return fmt.Sprintf("export_pdf: the render ran out of its %d MiB memory limit; shorten %s or draw fewer, smaller images", PDFRenderMemory>>20, source)
	default:
		return fmt.Sprintf("export_pdf: Typst %s could not render %s:\n%s", TypstVersion, source, r.diag)
	}
}

// killedBy says whether the process ended on one of sigs.
func killedBy(e *exec.ExitError, sigs ...syscall.Signal) bool {
	ws, ok := e.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return false
	}
	for _, s := range sigs {
		if ws.Signal() == s {
			return true
		}
	}
	return false
}

// run starts cmd and waits; through the trampoline, its own failure comes
// back as failed, as for run_command.
func (c renderConfinement) run(cmd *exec.Cmd) (string, error) {
	if c.trampoline == nil {
		return "", cmd.Run()
	}
	return runTrampolined(cmd)
}

// createIn makes name new beneath root, readable by anyone, and fills it
// from r; a name that already exists, a link included, is refused.
func createIn(root *beneath.Dir, name string, r io.Reader) error {
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, werr := io.Copy(f, r)
	return errors.Join(werr, f.Close())
}

// pdfOut keeps what Typst writes to its stdout, up to one byte past
// MaxPDFBytes, so a PDF over the cap is told, never kept.
type pdfOut struct{ buf bytes.Buffer }

func (w *pdfOut) Write(p []byte) (int, error) {
	if room := MaxPDFBytes + 1 - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// checkPDF holds what Typst wrote to be a PDF under the cap.
func checkPDF(b []byte) error {
	if len(b) > MaxPDFBytes {
		return errAgentFixable{fmt.Sprintf("the PDF is over the %d MiB export_pdf writes; draw fewer, smaller images", MaxPDFBytes>>20)}
	}
	if !bytes.HasPrefix(b, []byte("%PDF-")) {
		return errors.New("what the renderer wrote is not a PDF")
	}
	return nil
}

// writePDF writes the PDF at out beneath reports/, through a beneath.Dir, and
// gives it and any folder made for it to the host user.
func (s *server) writePDF(reportsDir, out string, pdf []byte) error {
	root, err := beneath.Open(reportsDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	s.checked("reports")
	if dir := path.Dir(out); dir != "." {
		if err := mkdirAllIn(root, dir, 0o755); err != nil {
			return err
		}
		s.ownBetween(reportsDir, filepath.Join(reportsDir, filepath.FromSlash(dir)))
	}
	f, err := root.CreateRegular(out, 0o644)
	if err != nil {
		if errors.Is(err, beneath.ErrNotRegular) {
			return fmt.Errorf("reports/%s exists and is not a regular file", out)
		}
		return err
	}
	_, werr := f.Write(pdf)
	if err := errors.Join(werr, f.Close()); err != nil {
		return err
	}
	s.own(filepath.Join(reportsDir, filepath.FromSlash(out)))
	return nil
}

// appendPDF records an exported PDF in the ledger, as appendCall does.
func (s *server) appendPDF(e PDFEntry) error {
	l := s.spec.PDF.Ledger
	if l == "" {
		return nil
	}
	b, err := json.Marshal(e)
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

// renderFailed is a failure of the render root under /tmp: a full /tmp is
// the run's own doing (a declared command filled it), any other error the
// machinery's.
func (s *server) renderFailed(err error) (string, bool) {
	if errors.Is(err, syscall.ENOSPC) {
		return "export_pdf: the tools container's /tmp is full (" + err.Error() + "); remove what a command left there and try again", true
	}
	return s.failed(ToolExportPDF, err)
}
