package toolsrv

// export_pdf (plan scratch-and-pdf-reports.md §5, D6-D9, D14, D15, D17,
// D19, D23, D24, D27; agentry-cli#730): the agent turns a Markdown file it
// wrote, in reports/ or the scratch folder, into a PDF in reports/, laid out
// by the run's theme. Typst renders it inside the tools container, from a
// fresh render root under the bounded /tmp that holds the Markdown, the
// theme, the server's own template and copies of the local images the
// Markdown names, and nothing else:
//
//   - Typst runs with a fixed argv and an empty environment, through the
//     runner binary's confine role: it may write nothing (Landlock), it may
//     not open a TCP connection where Landlock can refuse one (ABI 4), and
//     it gets a 1 GiB address space and 60 seconds; the PDF comes back on
//     its stdout. As root the server starts it as a user of its own
//     (typstID), which owns nothing; where there is no Landlock (gVisor)
//     that user is all that holds it.
//   - Its binary was built without a package downloader (D26), and the
//     template imports only the vendored cmarker, with raw Typst off.
//   - Every file the server copies in is opened beneath its folder's
//     os.Root, and a name with a link anywhere in it is refused; every name
//     in the render root is made new through an os.Root on it. A declared
//     command may write the scratch folder and /tmp, and the server is
//     root.
//
// The result is checked to be a PDF, copied into reports/ and recorded in
// the PDF ledger with its SHA-256, which the receipt repeats (D15); with
// write_report refusing a PDF, it is the only way one lands in reports/.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/runnerimage/confine"
)

// ToolExportPDF renders a Markdown file to a PDF in reports/.
const ToolExportPDF = "export_pdf"

// The limits of one export (D24, D9).
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
// from agentry-typst, agentry-dockerimages#48): the binary, the fonts and
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
// folder, for every package but a built-in (D5); the engine stamps the
// rest into the run's own copy of the spec, never the compiled plan.
type PDF struct {
	Enabled bool `json:"enabled"`
	// Theme is the run's theme (D13, D18-D22); without one, the built-in
	// default.
	Theme *ThemeStamp `json:"theme,omitempty"`
	// Author names the QuickAgent that wrote the report,
	// namespace/name@version, for the PDF's metadata (D14): never the
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
}

func validatePDF(spec *Spec) error {
	p := spec.PDF
	if !p.Enabled && (p.Theme != nil || p.Ledger != "") {
		return errors.New("pdf: a theme or a ledger without the export_pdf grant")
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

const exportPDFDescription = "Turn a Markdown file you wrote into a PDF in reports/, laid out in the person's chosen report style. source is the Markdown: reports/<path> for one written with write_report, scratch/<path> for one in the scratch folder. path is the PDF under reports/ (default: the source's name with .pdf). Images are drawn when they are local PNG or JPEG files, named relative to the Markdown file or as /scratch/…, /inputs/… or /out/reports/…; any other image, a web address included, is drawn as its alt text. You cannot choose the style."

func exportPDFSchema() map[string]any {
	return objSchema(map[string]any{
		"source": strProp("the Markdown file: reports/<path> or scratch/<path>"),
		"path":   strProp("the PDF's path under reports/, ending in .pdf (default: the source's name with .pdf)"),
	}, "source")
}

// typstFiles is where the renderer is; a test points it elsewhere.
type typstFiles struct{ bin, fonts, packages string }

func defaultTypst() typstFiles {
	return typstFiles{bin: TypstDir + "/typst", fonts: TypstDir + "/fonts", packages: TypstDir + "/packages"}
}

// renderConfinement is how Typst is held (D24), chosen once when the
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
// it: each element is looked at, a link is refused, and the file opened is
// held to be the one looked at. The os.Root keeps the open beneath the
// folder whatever changes in between.
func openNoLinks(root *os.Root, rel string) (*os.File, fs.FileInfo, error) {
	rel = path.Clean(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) {
		return nil, nil, fmt.Errorf("%q is not a file beneath its folder", rel)
	}
	parts := strings.Split(rel, "/")
	var last fs.FileInfo
	for i := range parts {
		fi, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil {
			return nil, nil, err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return nil, nil, errLink{rel}
		}
		last = fi
	}
	if !last.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%q: %w", rel, errNotRegular)
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil || !os.SameFile(fi, last) {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%q changed while it was opened", rel)
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

// pdfMeta is meta.json: what the template is told besides the theme.
type pdfMeta struct {
	Title string `json:"title"`
	// Author is the QuickAgent's ref, or empty: never the person, nor
	// Agentry (D14).
	Author string              `json:"author"`
	Images map[string]pdfImage `json:"images"`
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

// imageRoots are the folders an image may come from (D9), each with the
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
func (s *server) copyImages(doc mdDoc, from pdfRoot, fromDir string, renderRoot *os.Root) (map[string]pdfImage, []string, error) {
	images := map[string]pdfImage{}
	copied := map[string]pdfImage{} // by folder and path, so a file is copied once
	opened := map[string]*os.Root{}
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
				if root, err = os.OpenRoot(r.dir); err != nil {
					why = "its folder could not be opened"
					continue
				}
				opened[r.dir] = root
			}
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
func copyImage(root *os.Root, rel string, renderRoot *os.Root, n int) (pdfImage, error) {
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
	var a struct {
		Source string `json:"source"`
		Path   string `json:"path"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return "export_pdf: " + err.Error(), true
	}
	if a.Source == "" {
		return "export_pdf: source is required: the Markdown file to export, reports/<path> or scratch/<path>", true
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
	src, err := readSource(from, rel)
	if err != nil {
		return answerFor(err, func(err error) (string, bool) {
			return s.failed(ToolExportPDF, fmt.Errorf("%s/%s: %w", from.name, rel, err))
		})
	}
	if env := s.holdsRunSecret(src); env != "" {
		return "export_pdf: " + from.name + "/" + rel + " holds the value of this run's secret " + env + "; take the value out and write what it is and where it was found", true
	}
	theme, themeDoc, err := s.pdfTheme()
	if err != nil {
		return s.failed(ToolExportPDF, err)
	}

	// 2. A fresh render root under /tmp. A declared command may write /tmp
	// (as root, under Landlock) and may have left a process running there,
	// so every name in the root is made new through an os.Root opened on
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
	root, err := os.OpenRoot(renderRoot)
	if err != nil {
		return s.renderFailed(err)
	}
	defer func() { _ = root.Close() }()
	if s.renderRootMade != nil {
		s.renderRootMade(renderRoot)
	}
	doc := scanMarkdown(src)
	if err := root.Mkdir("img", 0o755); err != nil {
		return s.renderFailed(err)
	}
	images, skipped, err := s.copyImages(doc, from, path.Dir(rel), root)
	if err != nil {
		return answerFor(err, s.renderFailed)
	}
	title := doc.title
	if title == "" {
		title = strings.TrimSuffix(path.Base(rel), path.Ext(rel))
	}
	meta, err := json.Marshal(pdfMeta{Title: title, Author: s.spec.PDF.Author, Images: images})
	if err != nil {
		return s.renderFailed(err)
	}
	template, err := pdfFiles.ReadFile("pdf/report.typ")
	if err != nil {
		return s.renderFailed(err)
	}
	for name, b := range map[string][]byte{"main.typ": template, "report.md": []byte(doc.text), "theme.json": themeDoc, "meta.json": meta} {
		if err := createIn(root, name, bytes.NewReader(b)); err != nil {
			return s.renderFailed(err)
		}
	}

	// 3. Typst, which may write nothing: the PDF comes back on its stdout,
	// so no file a declared command could swap stands between the render
	// and reports/.
	pdf, msg, isErr := s.runTypst(renderRoot, from.name+"/"+rel)
	if isErr {
		return msg, true
	}

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
	if err := s.appendPDF(PDFEntry{Path: "reports/" + out, SHA256: hex.EncodeToString(sum[:]), Pages: pages, Bytes: int64(len(pdf))}); err != nil {
		s.logf("pdf ledger: %v", err)
		return s.failed(ToolExportPDF, fmt.Errorf("the PDF was written to reports/%s, but the run could not record it, so it will not be shown as this run's export: %w", out, err))
	}
	count := "? pages"
	if pages == 1 {
		count = "1 page"
	} else if pages > 1 {
		count = strconv.Itoa(pages) + " pages"
	}
	answer := fmt.Sprintf("report written: reports/%s (%s, %d KiB; %s theme)", out, count, (len(pdf)+1023)/1024, theme.Name)
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
	root, err := os.OpenRoot(from.dir)
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

// pdfTheme is the run's theme and its document: the one the engine
// stamped, or the built-in default.
func (s *server) pdfTheme() (*Theme, []byte, error) {
	if st := s.spec.PDF.Theme; st != nil {
		t, err := st.check()
		return t, []byte(st.Document), err
	}
	doc, err := BuiltinTheme(DefaultTheme)
	if err != nil {
		return nil, nil, err
	}
	t, err := ParseTheme(doc)
	return t, doc, err
}

// runTypst renders renderRoot/main.typ to a PDF on Typst's stdout, or
// answers the agent why it could not.
func (s *server) runTypst(root, source string) ([]byte, string, bool) {
	if _, err := os.Stat(s.typst.bin); err != nil {
		s.callFault = true
		return nil, fmt.Sprintf("export_pdf: this runner image has no PDF renderer (%v); the runner image must be rebuilt", err), true
	}
	args := []string{"compile", "--root", root, "--ignore-system-fonts", "--font-path", s.typst.fonts,
		"--package-path", s.typst.packages, "--package-cache-path", filepath.Join(root, ".packages"),
		"--pdf-standard", "ua-1", "--diagnostic-format", "short",
		filepath.Join(root, "main.typ"), "-"}
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
		s.callFault = true
		return nil, "export_pdf: the renderer could not be started confined: " + failed, true
	}
	if err == nil {
		return stdout.buf.Bytes(), "", false
	}
	diag := s.scrubSecrets(strings.TrimSpace(strings.ReplaceAll(stderr.text(), root+"/", "")))
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return nil, fmt.Sprintf("export_pdf: the render took longer than its %s limit; shorten %s or draw fewer, smaller images", s.pdfTimeout, source), true
	case strings.Contains(diag, "memory allocation of"), errors.As(err, &exitErr) && killedBy(exitErr, syscall.SIGABRT, syscall.SIGKILL, syscall.SIGSEGV):
		return nil, fmt.Sprintf("export_pdf: the render ran out of its %d MiB memory limit; shorten %s or draw fewer, smaller images", PDFRenderMemory>>20, source), true
	case errors.As(err, &exitErr):
		return nil, fmt.Sprintf("export_pdf: Typst %s could not render %s:\n%s", TypstVersion, source, diag), true
	default:
		s.callFault = true
		return nil, "export_pdf: the renderer did not run: " + err.Error(), true
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
func createIn(root *os.Root, name string, r io.Reader) error {
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

// writePDF writes the PDF at out beneath reports/, through an os.Root, and
// gives it and any folder made for it to the host user.
func (s *server) writePDF(reportsDir, out string, pdf []byte) error {
	root, err := os.OpenRoot(reportsDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if dir := path.Dir(out); dir != "." {
		if err := mkdirAllIn(root, dir, 0o755); err != nil {
			return err
		}
		s.ownBetween(reportsDir, filepath.Join(reportsDir, filepath.FromSlash(dir)))
	}
	if fi, err := root.Lstat(out); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("reports/%s exists and is not a regular file", out)
	}
	f, err := root.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
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
