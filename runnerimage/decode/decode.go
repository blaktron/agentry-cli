// Package decode is the runner image's `decode` role (plan knowledge.md
// §4.2, D14, D21; agentry-cli#318, and agentry-cli#321 for a hosted run's
// inputs): it turns the documents under one directory into plain text
// under another, before any agent sees them, so a run reads text and no
// model ever runs a decoder. Each family of file has one fixed decoder
// with a fixed argv — pdftotext for PDF, pandoc (in its sandbox mode) for
// DOCX, ODT, EPUB and RTF, tesseract's OCR for images, and this package's
// own spreadsheet and slide readers, plus Tika for the long tail. Nothing a
// document contains reaches an argv but its own path. Text formats pass
// as they are. The caller runs this in a container with no network and no
// capabilities, the inputs mounted read-only.
//
// This package is part of the embedded runner-image build context and so
// imports nothing outside the standard library.
package decode

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Schema is the report's format; ReportFile is where Decode writes it,
// beside the texts.
const (
	Schema     = "decode/v1"
	ReportFile = "decode.json"
)

// Outcome is what happened to one file.
type Outcome string

// The outcomes.
const (
	// OutcomeDecoded: text was written for it.
	OutcomeDecoded Outcome = "decoded"
	// OutcomeText: it is text already and is read as it is.
	OutcomeText Outcome = "text"
	// OutcomeUndecodable: no decoder applies, or the decoder failed.
	OutcomeUndecodable Outcome = "undecodable"
	// OutcomeRefused: not a regular file, or past the file cap.
	OutcomeRefused Outcome = "refused"
)

// Family is the kind of document a file is, which picks its decoder.
type Family string

// The families.
const (
	FamilyText         Family = "text"
	FamilyPDF          Family = "pdf"
	FamilyDocument     Family = "document"
	FamilyPresentation Family = "presentation"
	FamilySpreadsheet  Family = "spreadsheet"
	FamilyImage        Family = "image"
	FamilyUnknown      Family = "unknown"
)

// File is one input's line in the report.
type File struct {
	// Path is the input's slash path relative to the input root.
	Path   string `json:"path"`
	Family Family `json:"family"`
	// Tool is the decoder that ran: pdftotext, pandoc, tesseract, or
	// agentry-runner for this package's own readers, or tika.
	Tool    string  `json:"tool,omitempty"`
	Outcome Outcome `json:"outcome"`
	// Output is the text's slash path relative to the output root.
	Output string `json:"output,omitempty"`
	// Bytes is the size of the text written.
	Bytes  int64  `json:"bytes,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Report is what one Decode did, file by file, in path order.
type Report struct {
	Schema string `json:"schema"`
	Files  []File `json:"files"`
}

// Tools are the absolute paths of the detectors and external decoders; zero
// values take the runner image's.
type Tools struct {
	PDFToText, Pandoc, Tesseract, File, Tika string
}

func (t Tools) withDefaults() Tools {
	if t.PDFToText == "" {
		t.PDFToText = "/usr/bin/pdftotext"
	}
	if t.Pandoc == "" {
		t.Pandoc = "/usr/bin/pandoc"
	}
	if t.Tesseract == "" {
		t.Tesseract = "/usr/bin/tesseract"
	}
	if t.File == "" {
		t.File = "/usr/bin/file"
	}
	if t.Tika == "" {
		t.Tika = "/usr/bin/java"
	}
	return t
}

// Limits bound a decode; zero values take the defaults.
type Limits struct {
	// MaxFiles refuses the files past this many (default 2,000, a release
	// archive's own cap).
	MaxFiles int
	// ScratchDir is where a decoder writes before its text is cut to size
	// and copied out, so no more than MaxOutputBytes of a file's text ever
	// reaches the output directory (default the system temporary
	// directory: in the decode container, a size-limited tmpfs).
	ScratchDir string
	// PerFileSeconds stops a decoder that runs longer (default 120).
	PerFileSeconds int
	// MaxOutputBytes cuts a text past this size (default 10 MiB, a whole
	// knowledge pack).
	MaxOutputBytes int64
	// MaxExpandedBytes refuses an Office file whose parts expand past this
	// (default 64 MiB), so a zip bomb costs a bounded read.
	MaxExpandedBytes int64
}

func (l Limits) withDefaults() Limits {
	if l.MaxFiles <= 0 {
		l.MaxFiles = 2000
	}
	if l.ScratchDir == "" {
		l.ScratchDir = os.TempDir()
	}
	if l.PerFileSeconds <= 0 {
		l.PerFileSeconds = 120
	}
	if l.MaxOutputBytes <= 0 {
		l.MaxOutputBytes = 10 << 20
	}
	if l.MaxExpandedBytes <= 0 {
		l.MaxExpandedBytes = 64 << 20
	}
	return l
}

// Decode decodes every regular file under in into out and writes the
// report to out/decode.json. A file's failure is its outcome; the error is
// for the roots alone.
func Decode(ctx context.Context, in, out string, tools Tools, lim Limits) (Report, error) {
	tools, lim = tools.withDefaults(), lim.withDefaults()
	rep := Report{Schema: Schema, Files: []File{}}
	fi, err := os.Stat(in)
	if err != nil {
		return rep, fmt.Errorf("decode: the input root: %w", err)
	}
	if !fi.IsDir() {
		return rep, fmt.Errorf("decode: the input root %s is not a directory", in)
	}
	seen := 0
	err = filepath.WalkDir(in, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(in, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !d.Type().IsRegular() {
			rep.Files = append(rep.Files, File{Path: rel, Family: FamilyUnknown, Outcome: OutcomeRefused, Reason: "not a regular file; a link is never followed"})
			return nil
		}
		if seen++; seen > lim.MaxFiles {
			rep.Files = append(rep.Files, File{Path: rel, Family: FamilyUnknown, Outcome: OutcomeRefused, Reason: fmt.Sprintf("past the %d-file cap", lim.MaxFiles)})
			return nil
		}
		rep.Files = append(rep.Files, decodeOne(ctx, p, rel, out, tools, lim))
		return nil
	})
	if err != nil {
		return rep, fmt.Errorf("decode: walk %s: %w", in, err)
	}
	sort.Slice(rep.Files, func(i, j int) bool { return rep.Files[i].Path < rep.Files[j].Path })
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return rep, err
	}
	if err := os.WriteFile(filepath.Join(out, ReportFile), append(raw, '\n'), 0o644); err != nil { //nolint:gosec // read by the caller
		return rep, fmt.Errorf("decode: write the report: %w", err)
	}
	return rep, nil
}

func decodeOne(ctx context.Context, src, rel, out string, tools Tools, lim Limits) File {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(lim.PerFileSeconds)*time.Second)
	defer cancel()
	kind, err := detect(ctx, src, tools)
	f := File{Path: rel, Family: kind.family}
	if err != nil {
		f.Tool = "tika"
		return undecodable(f, "content detection failed: "+err.Error())
	}
	if kind.text {
		if isText(src) {
			f.Outcome = OutcomeText
		} else {
			return undecodable(f, "not UTF-8 text")
		}
		return f
	}
	if kind.tool == "" {
		return undecodable(f, "not text, and no decoder reads this kind of file")
	}
	f.Output = rel + ".txt"
	dst := filepath.Join(out, filepath.FromSlash(f.Output))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return undecodable(f, err.Error())
	}
	// A decoder writes into scratch; only its text, cut to size, is copied
	// to the output, so a document that expands without end fills the
	// scratch's bounded space, never the caller's.
	scratch, err := os.MkdirTemp(lim.ScratchDir, "decode-")
	if err != nil {
		return undecodable(f, err.Error())
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	text := filepath.Join(scratch, "out.txt")
	f.Tool = kind.tool
	switch kind.tool {
	case "pdftotext":
		f.Tool = "pdftotext"
		err = runTool(ctx, tools.PDFToText, "-layout", "-enc", "UTF-8", "-q", src, text)
	case "pandoc":
		f.Tool = "pandoc"
		err = runTool(ctx, tools.Pandoc, "--sandbox", "--from="+kind.format, "--to=gfm", "--wrap=none", "--output="+text, src)
	case "tesseract":
		f.Tool = "tesseract"
		// tesseract writes <base>.txt.
		err = runTool(ctx, tools.Tesseract, src, strings.TrimSuffix(text, ".txt"), "-l", "eng")
	case "agentry-runner":
		f.Tool = "agentry-runner"
		var body string
		switch kind.format {
		case "pptx":
			body, err = pptxText(src, lim.MaxExpandedBytes)
		case "xlsx":
			body, err = xlsxText(src, lim.MaxExpandedBytes)
		case "ods":
			body, err = odsText(src, lim.MaxExpandedBytes)
		case "odp":
			body, err = odpText(src, lim.MaxExpandedBytes)
		case "csv":
			body, err = csvText(src, lim.MaxExpandedBytes)
		}
		if err == nil {
			err = os.WriteFile(text, []byte(body), 0o600)
		}
	case "tika":
		err = tikaText(ctx, tools.Tika, src, text, lim.MaxOutputBytes)
	}
	if err != nil {
		return undecodable(f, f.Tool+": "+err.Error())
	}
	return finish(f, text, dst, lim.MaxOutputBytes)
}

func undecodable(f File, reason string) File {
	f.Output, f.Outcome, f.Reason = "", OutcomeUndecodable, reason
	return f
}

// finish reads at most the cap of the decoder's text, holds it to UTF-8,
// and writes it to the output.
func finish(f File, text, dst string, maxBytes int64) File {
	src, err := os.Open(text) //nolint:gosec // the scratch text this decode wrote
	if err != nil {
		return undecodable(f, f.Tool+" wrote no text: "+err.Error())
	}
	defer func() { _ = src.Close() }()
	b, err := io.ReadAll(io.LimitReader(src, maxBytes+1))
	if err != nil {
		return undecodable(f, err.Error())
	}
	if int64(len(b)) > maxBytes {
		total := "more"
		if fi, err := src.Stat(); err == nil {
			total = strconv.FormatInt(fi.Size(), 10)
		}
		b = b[:maxBytes]
		for len(b) > 0 && !utf8.Valid(b) {
			b = b[:len(b)-1]
		}
		f.Reason = fmt.Sprintf("cut at %d bytes of %s", len(b), total)
	}
	b = bytes.ToValidUTF8(b, []byte("\uFFFD"))
	if err := os.WriteFile(dst, b, 0o644); err != nil { //nolint:gosec // the decoded text, read by the run
		return undecodable(f, err.Error())
	}
	f.Outcome, f.Bytes = OutcomeDecoded, int64(len(b))
	return f
}

// runTool runs one decoder with a fixed argv and a bare environment, its
// output discarded but for a short tail of its errors.
func runTool(ctx context.Context, tool string, args ...string) error {
	cmd := exec.CommandContext(ctx, tool, args...) //nolint:gosec // a fixed decoder; the only variable argv is the document's own path
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/tmp", "OMP_THREAD_LIMIT=1", "LC_ALL=C.UTF-8"}
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &limitedWriter{w: &stderr, n: 512}
	if err := cmd.Run(); err != nil {
		if tail := strings.TrimSpace(stderr.String()); tail != "" {
			return fmt.Errorf("%w: %s", err, tail)
		}
		return err
	}
	return nil
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		written, err := l.w.Write(p[:k])
		l.n -= written
		if err != nil {
			return written, err
		}
		if written != k {
			return written, io.ErrShortWrite
		}
	}
	return len(p), nil
}

// isText says whether a file of no known family reads as text: its first
// 64 KiB are UTF-8 (a cut rune at the end aside) with no NUL.
func isText(p string) bool {
	f, err := os.Open(p) //nolint:gosec // an input under the root being decoded
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	b, _ := io.ReadAll(io.LimitReader(f, 64<<10))
	if bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	// A rune the read cut in two at its end is not a reason to call the
	// file binary.
	for i := 0; i < utf8.UTFMax-1 && len(b) > 0 && !utf8.Valid(b); i++ {
		b = b[:len(b)-1]
	}
	return utf8.Valid(b)
}

// ---- PPTX and XLSX: this package's own reader ----

var (
	slideRE = regexp.MustCompile(`^ppt/slides/slide(\d+)\.xml$`)
	sheetRE = regexp.MustCompile(`^xl/worksheets/sheet(\d+)\.xml$`)
)

// ooxmlParts opens an office zip and returns its parts matching re, in the
// numeric order of their first group, reading each in full against one
// budget of expanded bytes shared by the whole file.
func ooxmlParts(src string, re *regexp.Regexp, extra []string, budget int64) (parts []numberedPart, named map[string][]byte, err error) {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return nil, nil, fmt.Errorf("not a readable office file: %w", err)
	}
	defer func() { _ = zr.Close() }()
	if len(zr.File) > 10000 {
		return nil, nil, errors.New("more than 10,000 parts")
	}
	named = map[string][]byte{}
	want := map[string]bool{}
	for _, e := range extra {
		want[e] = true
	}
	read := func(zf *zip.File) ([]byte, error) {
		rc, err := zf.Open()
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		b, err := io.ReadAll(io.LimitReader(rc, budget+1))
		if err != nil {
			return nil, err
		}
		budget -= int64(len(b))
		if budget < 0 {
			return nil, errors.New("the file expands past its cap")
		}
		return b, nil
	}
	for _, zf := range zr.File {
		switch m := re.FindStringSubmatch(zf.Name); {
		case m != nil:
			n := 1
			if len(m) > 1 {
				n, _ = strconv.Atoi(m[1])
			}
			b, err := read(zf)
			if err != nil {
				return nil, nil, err
			}
			parts = append(parts, numberedPart{n: n, data: b})
		case want[zf.Name]:
			b, err := read(zf)
			if err != nil {
				return nil, nil, err
			}
			named[zf.Name] = b
		}
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].n < parts[j].n })
	return parts, named, nil
}

type numberedPart struct {
	n    int
	data []byte
}

// pptxText is each slide's text runs, a line per paragraph, under a
// "Slide N" heading.
func pptxText(src string, budget int64) (string, error) {
	slides, _, err := ooxmlParts(src, slideRE, nil, budget)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for i, s := range slides {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "## Slide %d\n", s.n)
		dec := xml.NewDecoder(bytes.NewReader(s.data))
		var line strings.Builder
		inText := false
		for {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", fmt.Errorf("slide %d: %w", s.n, err)
			}
			switch t := tok.(type) {
			case xml.StartElement:
				inText = t.Name.Local == "t"
			case xml.EndElement:
				switch t.Name.Local {
				case "t":
					inText = false
				case "p":
					if line.Len() > 0 {
						b.WriteString(line.String() + "\n")
						line.Reset()
					}
				}
			case xml.CharData:
				if inText {
					line.Write(t)
				}
			}
		}
		if line.Len() > 0 {
			b.WriteString(line.String() + "\n")
		}
	}
	return b.String(), nil
}

// xlsxText is each sheet's rows, cells tab-separated, under a "Sheet N"
// heading, with shared and inline strings resolved and booleans written
// TRUE or FALSE.
func xlsxText(src string, budget int64) (string, error) {
	const sharedName = "xl/sharedStrings.xml"
	sheets, named, err := ooxmlParts(src, sheetRE, []string{sharedName}, budget)
	if err != nil {
		return "", err
	}
	shared, err := sharedStrings(named[sharedName])
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for i, s := range sheets {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "## Sheet %d\n", s.n)
		if err := sheetRows(&b, s.data, shared); err != nil {
			return "", fmt.Errorf("sheet %d: %w", s.n, err)
		}
		if b.Len() > maxTableBytes {
			return b.String(), nil
		}
	}
	return b.String(), nil
}

func sharedStrings(data []byte) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	var out []string
	var cur strings.Builder
	inT, inSI := false, false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("shared strings: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI = true
				cur.Reset()
			case "t":
				inT = inSI
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inT = false
			case "si":
				out = append(out, cur.String())
				inSI = false
			}
		case xml.CharData:
			if inT {
				cur.Write(t)
			}
		}
	}
}

func sheetRows(b *strings.Builder, data []byte, shared []string) error {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var row []string
	var rows [][]string
	var cellColumn int
	var cellType string
	var val strings.Builder
	inV, inT, inCell := false, false, false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			b.WriteString(markdownTable(rows))
			return nil
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				row = row[:0]
			case "c":
				inCell, cellType = true, ""
				val.Reset()
				cellColumn = len(row)
				for _, a := range t.Attr {
					if a.Name.Local == "t" {
						cellType = a.Value
					}
					if a.Name.Local == "r" {
						cellColumn = columnIndex(a.Value)
					}
				}
			case "v":
				inV = inCell
			case "t":
				inT = inCell
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v":
				inV = false
			case "t":
				inT = false
			case "c":
				if cellColumn >= maxTableColumns {
					return fmt.Errorf("more than 1,024 columns")
				}
				for len(row) <= cellColumn {
					row = append(row, "")
				}
				row[cellColumn] = cellText(cellType, val.String(), shared)
				inCell = false
			case "row":
				rows = append(rows, append([]string(nil), row...))
				if len(rows) > maxTableRows {
					b.WriteString(markdownTable(rows))
					return nil
				}
			}
		case xml.CharData:
			if inV || inT {
				val.Write(t)
			}
		}
	}
}

func cellText(typ, v string, shared []string) string {
	switch typ {
	case "s":
		if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && i >= 0 && i < len(shared) {
			return shared[i]
		}
		return ""
	case "b":
		if strings.TrimSpace(v) == "1" {
			return "TRUE"
		}
		return "FALSE"
	}
	return v
}

func columnIndex(ref string) int {
	n := 0
	for _, c := range ref {
		if c < 'A' || c > 'Z' {
			break
		}
		n = n*26 + int(c-'A') + 1
		if n > maxTableColumns {
			return maxTableColumns
		}
	}
	return max(0, n-1)
}
