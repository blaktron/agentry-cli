package docpages

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Tools are the absolute paths of poppler's programs Render runs; zero
// values take the decode image's.
type Tools struct {
	PDFToPPM, PDFInfo, PDFFonts, PDFImages string
}

func (t Tools) withDefaults() Tools {
	if t.PDFToPPM == "" {
		t.PDFToPPM = "/usr/bin/pdftoppm"
	}
	if t.PDFInfo == "" {
		t.PDFInfo = "/usr/bin/pdfinfo"
	}
	if t.PDFFonts == "" {
		t.PDFFonts = "/usr/bin/pdffonts"
	}
	if t.PDFImages == "" {
		t.PDFImages = "/usr/bin/pdfimages"
	}
	return t
}

// The time limits of one document's reading.
const (
	// PageTimeout stops one page's render, and each of pdfinfo's and
	// pdffonts' runs.
	PageTimeout = 60 * time.Second
	// maxToolOutput caps what Render reads of pdfinfo's and pdffonts'
	// output.
	maxToolOutput = 64 << 10
	// maxDocxPart caps one XML part of a Word document as read, and
	// maxDocxParts how many entries its archive may hold.
	maxDocxPart  = 2 << 20
	maxDocxParts = 10000
)

// Render reads the document at src, of kind, and writes FactsFile with
// what it found beside the pages (PDF) or the pictures (Word) under out;
// scratch is where a page is rendered before its capped copy is made. A
// document that cannot be read is a Facts with its Failure, never an
// error: the error is this role's own failure to write.
func Render(ctx context.Context, src string, kind Kind, out, scratch string, t Tools) (Facts, error) {
	t = t.withDefaults()
	f := Facts{Schema: Schema, Kind: kind}
	var err error
	switch kind {
	case KindPDF:
		err = renderPDF(ctx, src, out, scratch, t, &f)
	case KindDOCX:
		err = readDocx(src, out, &f)
	default:
		return f, fmt.Errorf("pages: unknown kind %q", kind)
	}
	if err != nil {
		return f, err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return f, err
	}
	return f, os.WriteFile(filepath.Join(out, FactsFile), append(b, '\n'), 0o644) //nolint:gosec // read by the host
}

// renderPDF counts the pages, lists the fonts of the first MaxPages,
// renders each of them in turn, stopping at the first that fails, and
// writes the pictures the rendered pages draw.
func renderPDF(ctx context.Context, src, out, scratch string, t Tools, f *Facts) error {
	last := strconv.Itoa(MaxPages)
	info, kind := runPoppler(ctx, t.PDFInfo, "-f", "1", "-l", last, src)
	if kind != "" {
		f.Failure = kind
		return nil
	}
	pages, sizes := ParseInfo(info)
	f.Pages = pages
	if pages == 0 {
		f.Failure = FailureEmpty
		return nil
	}
	if fonts, kind := runPoppler(ctx, t.PDFFonts, "-f", "1", "-l", last, src); kind == "" {
		f.Fonts = ParseFonts(fonts)
	}
	root := filepath.Join(scratch, "page")
	for n := 1; n <= min(pages, MaxPages); n++ {
		// The crop area bounds the image whatever size the page claims;
		// a page it cannot see is scaled to fit rather than rendered at
		// full size.
		side := strconv.Itoa(MaxPageSide)
		args := []string{"-f", strconv.Itoa(n), "-l", strconv.Itoa(n), "-png", "-singlefile", "-x", "0", "-y", "0", "-W", side, "-H", side}
		if long, ok := sizes[n]; !ok || long*RenderPPI/72 > MaxPageSide {
			args = append(args, "-scale-to", strconv.Itoa(MaxPageSide))
		} else {
			args = append(args, "-r", strconv.Itoa(RenderPPI))
		}
		_ = os.Remove(root + ".png")
		_, kind := runPoppler(ctx, t.PDFToPPM, append(args, src, root)...)
		if kind == "" {
			var err error
			if kind, err = copyCapped(root+".png", filepath.Join(out, PageFile(n)), MaxPageBytes); err != nil {
				return err
			}
		}
		_ = os.Remove(root + ".png")
		if kind != "" {
			if n == 1 {
				f.Failure, f.Fonts = kind, nil
			} else {
				f.Stopped = kind
			}
			break
		}
		f.Rendered = n
	}
	if f.Rendered == 0 {
		return nil
	}
	return pdfPictures(ctx, src, out, scratch, t, f.Rendered, f)
}

// copyCapped copies a page from scratch to out within limit; a page
// missing or past the limit is the kind of failure it is.
func copyCapped(from, to string, limit int64) (string, error) {
	fi, err := os.Lstat(from)
	if err != nil || !fi.Mode().IsRegular() {
		return FailureMalformed, nil
	}
	if fi.Size() > limit {
		return FailureMemory, nil
	}
	b, err := os.ReadFile(from) //nolint:gosec // this role's own scratch file
	if err != nil {
		return "", err
	}
	return "", os.WriteFile(to, b, 0o644) //nolint:gosec // read by the host
}

// runPoppler runs one of poppler's programs with a fixed argv under
// PageTimeout and answers its output, or the kind of failure it ended in.
// Its own messages are never answered: they describe a document no one
// checked.
func runPoppler(ctx context.Context, tool string, args ...string) (string, string) {
	ctx, cancel := context.WithTimeout(ctx, PageTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, tool, args...) //nolint:gosec // a fixed program; the only variable argv is the document's own path
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/tmp", "LC_ALL=C.UTF-8"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &capped{w: &stdout, n: maxToolOutput}
	cmd.Stderr = &capped{w: &stderr, n: 4 << 10}
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	switch {
	case err == nil:
		return stdout.String(), ""
	case ctx.Err() == context.DeadlineExceeded:
		return "", FailureTime
	case strings.Contains(strings.ToLower(stderr.String()), "password"):
		return "", FailureLocked
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL {
			return "", FailureMemory
		}
	}
	return "", FailureMalformed
}

// capped keeps the first n bytes written and discards the rest.
type capped struct {
	w io.Writer
	n int
}

func (c *capped) Write(p []byte) (int, error) {
	if c.n > 0 {
		k := min(len(p), c.n)
		if _, err := c.w.Write(p[:k]); err != nil {
			return 0, err
		}
		c.n -= k
	}
	return len(p), nil
}

// ParseInfo reads pdfinfo's answer: the page count, and each listed
// page's longer side in points.
func ParseInfo(s string) (int, map[int]float64) {
	pages, sizes := 0, map[int]float64{}
	for line := range strings.Lines(s) {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(k)
		switch {
		case len(fields) == 1 && fields[0] == "Pages":
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				pages = n
			}
		case len(fields) == 3 && fields[0] == "Page" && fields[2] == "size":
			n, err := strconv.Atoi(fields[1])
			dims := strings.Fields(v)
			if err != nil || len(dims) < 3 || dims[1] != "x" {
				continue
			}
			w, errW := strconv.ParseFloat(dims[0], 64)
			h, errH := strconv.ParseFloat(dims[2], 64)
			if errW == nil && errH == nil {
				sizes[n] = max(w, h)
			}
		}
	}
	return pages, sizes
}

// ParseFonts reads pdffonts' table: each font's name, the subset tag
// taken off, once each, in the order listed, within MaxFonts.
func ParseFonts(s string) []Font {
	var out []Font
	seen := map[string]bool{}
	for i, line := range slices.Collect(strings.Lines(s)) {
		fields := strings.Fields(line)
		if i < 2 || len(fields) == 0 || fields[0] == "[none]" {
			continue
		}
		name := FontName(fields[0])
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, Font{Name: name})
		if len(out) == MaxFonts {
			break
		}
	}
	return out
}

// readDocx reads a Word document's theme, font table, headers, footers
// and pictures; one that does not open as a Word document is malformed.
func readDocx(src, out string, f *Facts) error {
	z, err := zip.OpenReader(src)
	if err != nil || len(z.File) > maxDocxParts {
		f.Failure = FailureMalformed
		if z != nil {
			_ = z.Close()
		}
		return nil
	}
	defer func() { _ = z.Close() }()
	byName := map[string]*zip.File{}
	var names []string
	for _, zf := range z.File {
		byName[zf.Name] = zf
		names = append(names, zf.Name)
	}
	slices.SortFunc(names, naturalOrder)
	if byName["word/document.xml"] == nil {
		f.Failure = FailureMalformed
		return nil
	}
	part := func(name string) []byte {
		zf := byName[name]
		if zf == nil || zf.UncompressedSize64 > maxDocxPart {
			return nil
		}
		b, err := readEntry(zf, maxDocxPart)
		if err != nil {
			return nil
		}
		return b
	}
	for _, name := range names {
		if strings.HasPrefix(name, "word/theme/theme") && strings.HasSuffix(name, ".xml") {
			fonts, colours := ParseTheme(part(name))
			f.Fonts, f.Colours = fonts, colours
			break
		}
	}
	for _, font := range ParseFontTable(part("word/fontTable.xml")) {
		if len(f.Fonts) < MaxFonts && !slices.ContainsFunc(f.Fonts, func(x Font) bool { return x.Name == font.Name }) {
			f.Fonts = append(f.Fonts, font)
		}
	}
	for _, name := range names {
		dir, base := path.Split(name)
		if dir != "word/" || !strings.HasSuffix(base, ".xml") {
			continue
		}
		var into *[]string
		switch {
		case strings.HasPrefix(base, "header"):
			into = &f.Headers
		case strings.HasPrefix(base, "footer"):
			into = &f.Footers
		default:
			continue
		}
		if len(*into) == MaxRunning {
			continue
		}
		if text := RunningText(part(name)); text != "" && !slices.Contains(*into, text) {
			*into = append(*into, text)
		}
	}
	for _, name := range names {
		zf := byName[name]
		if !strings.HasPrefix(name, "word/media/") || strings.HasSuffix(name, "/") {
			continue
		}
		if len(f.Media) == MaxMedia || zf.UncompressedSize64 > MaxMediaBytes {
			f.MediaSkipped++
			continue
		}
		b, err := readEntry(zf, MaxMediaBytes)
		if err != nil {
			f.MediaSkipped++
			continue
		}
		ext := ""
		switch {
		case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
			ext = "png"
		case bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}):
			ext = "jpg"
		default:
			f.MediaSkipped++
			continue
		}
		file := MediaFile(len(f.Media)+1, ext)
		if err := os.WriteFile(filepath.Join(out, file), b, 0o644); err != nil { //nolint:gosec // read by the host
			return err
		}
		f.Media = append(f.Media, file)
	}
	return nil
}

// naturalOrder sorts header2.xml before header10.xml.
func naturalOrder(a, b string) int {
	ta, na := splitNumber(a)
	tb, nb := splitNumber(b)
	if c := strings.Compare(ta, tb); c != 0 {
		return c
	}
	if na != nb {
		return na - nb
	}
	return strings.Compare(a, b)
}

func splitNumber(s string) (string, int) {
	stem := strings.TrimSuffix(s, path.Ext(s))
	i := len(stem)
	for i > 0 && stem[i-1] >= '0' && stem[i-1] <= '9' {
		i--
	}
	n, _ := strconv.Atoi(stem[i:])
	return stem[:i] + path.Ext(s), n
}

// readEntry reads one archive entry within limit, counting what it
// inflates to rather than trusting the size it declares.
func readEntry(zf *zip.File, limit int64) ([]byte, error) {
	r, err := zf.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("an entry past its cap")
	}
	return b, nil
}

// themeColours are the roles of a theme's colour scheme, in its order.
var themeColours = []string{"dk1", "lt1", "dk2", "lt2", "accent1", "accent2", "accent3", "accent4", "accent5", "accent6", "hlink", "folHlink"}

// ParseTheme reads a Word theme part (DrawingML): the heading and body
// faces of its font scheme and the colours of its colour scheme, each by
// its role.
func ParseTheme(b []byte) ([]Font, []Colour) {
	var fonts []Font
	var colours []Colour
	dec := xml.NewDecoder(bytes.NewReader(b))
	var stack []string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			parent := ""
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			grand := ""
			if len(stack) > 1 {
				grand = stack[len(stack)-2]
			}
			stack = append(stack, t.Name.Local)
			switch {
			case grand == "clrScheme" && slices.Contains(themeColours, parent) && (t.Name.Local == "srgbClr" || t.Name.Local == "sysClr"):
				attr := "val"
				if t.Name.Local == "sysClr" {
					attr = "lastClr"
				}
				if hex := hexAttr(t, attr); hex != "" && len(colours) < MaxColours {
					colours = append(colours, Colour{Hex: hex, Role: parent})
				}
			case grand == "fontScheme" && (parent == "majorFont" || parent == "minorFont") && t.Name.Local == "latin":
				role := "headings"
				if parent == "minorFont" {
					role = "body"
				}
				if name := Line(attrOf(t, "typeface"), MaxNameRunes); name != "" {
					fonts = append(fonts, Font{Name: name, Role: role})
				}
			}
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return fonts, colours
}

// ParseFontTable reads a Word document's font table: the name of each
// font it lists.
func ParseFontTable(b []byte) []Font {
	var out []Font
	dec := xml.NewDecoder(bytes.NewReader(b))
	for len(out) < MaxFonts {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if t, ok := tok.(xml.StartElement); ok && t.Name.Local == "font" {
			if name := Line(attrOf(t, "name"), MaxNameRunes); name != "" && !slices.ContainsFunc(out, func(f Font) bool { return f.Name == name }) {
				out = append(out, Font{Name: name})
			}
		}
	}
	return out
}

// RunningText is the text of a Word header or footer part, its
// paragraphs joined by " / ", as one line within MaxRunningRunes.
func RunningText(b []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(b))
	var paras []string
	var cur strings.Builder
	inText := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				cur.WriteByte(' ')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				if s := Line(cur.String(), MaxRunningRunes); s != "" {
					paras = append(paras, s)
				}
				cur.Reset()
			}
		case xml.CharData:
			if inText {
				cur.Write(t)
			}
		}
	}
	return Line(strings.Join(paras, " / "), MaxRunningRunes)
}

func attrOf(t xml.StartElement, local string) string {
	for _, a := range t.Attr {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

// hexAttr is a colour attribute as #rrggbb, or empty when it is not six
// hex digits.
func hexAttr(t xml.StartElement, local string) string {
	v := strings.ToLower(attrOf(t, local))
	if len(v) != 6 || strings.Trim(v, "0123456789abcdef") != "" {
		return ""
	}
	return "#" + v
}
