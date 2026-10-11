// Package decode is the runner image's `decode` role (plan knowledge.md
// §4.2, D14, D21; agentry-cli#318, and agentry-cli#321 for a hosted run's
// inputs): it turns the documents under one directory into plain text
// under another, before any agent sees them, so a run reads text and no
// model ever runs a decoder. Each family of file has one fixed decoder
// with a fixed argv — pdftotext for PDF, pandoc (in its sandbox mode) for
// DOCX, ODT, EPUB and RTF, tesseract's OCR for images, and this package's
// own spreadsheet and slide readers, plus Tika for the long tail. An
// archive's members are unpacked and decoded in turn (archive.go). Before
// any decoder reads a file, every file and member is checked by ClamAV in
// one clamscan run, and a file it finds is never decoded (clamav.go).
// A git repository's internals, everything under a .git directory, are
// listed and checked but never detected or decoded (InRepository).
// Nothing a document contains reaches an argv but its own path. Any other
// UTF-8 text (code, markup, configuration, data) passes as it is, byte
// for byte, told without a JVM (detect; agentry-cli#630). The caller runs this in a container with no
// network and no capabilities, the inputs mounted read-only.
//
// This package is part of the embedded runner-image build context and so
// imports nothing outside the standard library.
package decode

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// OutcomeRefused: not a regular file, or past the file cap; for an
	// archive or a member, Why says which.
	OutcomeRefused Outcome = "refused"
	// OutcomeUnpacked: an archive whose members follow it in the report,
	// each named <archive>/<member>; Why, when set, says why only part of
	// it was unpacked.
	OutcomeUnpacked Outcome = "unpacked"
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
	FamilyArchive      Family = "archive"
	FamilyUnknown      Family = "unknown"
)

// File is one input's line in the report.
type File struct {
	// Path is the input's slash path relative to the input root.
	Path   string `json:"path"`
	Family Family `json:"family"`
	// Tool is the decoder that ran: pdftotext, pandoc, tesseract, or
	// agentry-runner for this package's own readers, or tika; for an
	// archive, what read it: agentry-runner, bsdcat or bsdtar. A file read
	// as text names none; its Markdown view names its own (MarkdownBy).
	Tool    string  `json:"tool,omitempty"`
	Outcome Outcome `json:"outcome"`
	// Output is the text's slash path relative to the output root. A
	// member's text is written there whether it was decoded or is text
	// already, since the member itself stays in the decode container.
	Output string `json:"output,omitempty"`
	// Bytes is the size of the text written.
	Bytes int64 `json:"bytes,omitempty"`
	// Markdown is the slash path, relative to the output root, of a
	// second view of a file read as text: HTML converted to Markdown by
	// MarkdownBy (pandoc), at exactly <path>.md (plan
	// quickagents-that-run.md D2; agentry-cli#631). The file itself stays
	// its own view; a failed conversion leaves this empty and the file read
	// as it is. MarkdownBytes is the size of the Markdown written.
	Markdown      string `json:"markdown,omitempty"`
	MarkdownBy    string `json:"markdownBy,omitempty"`
	MarkdownBytes int64  `json:"markdownBytes,omitempty"`
	Reason        string `json:"reason,omitempty"`
	// In is the archive a member was unpacked from, empty for a file as
	// staged.
	In string `json:"in,omitempty"`
	// Size is a member's unpacked size.
	Size int64 `json:"size,omitempty"`
	// Why is why an archive or a member was not read in full, in the
	// closed vocabulary of archive.go, or WhyFound, or WhyRepository.
	Why Why `json:"why,omitempty"`
	// Check is what the malware check made of it (clamav.go); empty for a
	// file never written to be read (a link, a refused member, past the
	// cap). Signature names what was found; Unchecked says why it was not
	// checked.
	Check     Check     `json:"check,omitempty"`
	Signature string    `json:"signature,omitempty"`
	Unchecked Unchecked `json:"unchecked,omitempty"`
	// MIME is the type the file was detected as, and SHA256 its content's
	// digest, lower-case hex: for every file read, archives and members
	// included, so the host can look the risky ones up on Hybrid Analysis
	// by hash (agentry-cli#564). The host checks a staged file's digest
	// against the file; only a member's is the decode's word alone.
	MIME   string `json:"mime,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// HasText says the decode wrote a text for it at Output: a file it
// decoded, or a member read as text, whose text is copied out since the
// member itself stays in the decode container.
func (f File) HasText() bool {
	return f.Outcome == OutcomeDecoded || (f.Outcome == OutcomeText && f.In != "")
}

// Report is what one Decode did, file by file, in path order.
type Report struct {
	Schema string `json:"schema"`
	Files  []File `json:"files"`
}

// Tools are the absolute paths of the detectors and external decoders; zero
// values take the runner image's.
type Tools struct {
	PDFToText, Pandoc, Tesseract, File, Tika, Bsdtar, Bsdcat, Clamscan string
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
	if t.Bsdtar == "" {
		t.Bsdtar = "/usr/bin/bsdtar"
	}
	if t.Bsdcat == "" {
		t.Bsdcat = "/usr/bin/bsdcat"
	}
	if t.Clamscan == "" {
		t.Clamscan = "/usr/bin/clamscan"
	}
	return t
}

// The decode's fixed bounds on archives (D5), which the host holds a
// report to: how deep archives nest and are still unpacked, and how many
// files and members a decode takes in all.
const (
	MaxDepth        = 3
	DefaultMaxFiles = 2000
	// DefaultSourceMaxFiles is an input source's own cap, its archive
	// members included (plan input-sources.md D7): the fetch step refused
	// a larger source already.
	DefaultSourceMaxFiles = 20000
)

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
	// UnpackDir is where archives are unpacked (default ScratchDir: in the
	// decode container, a tmpfs of its own the size of MaxUnpackedBytes).
	UnpackDir string
	// MaxUnpackedBytes is what every archive in the decode may unpack in
	// all, counted as each member streams (default 512 MiB; D5).
	MaxUnpackedBytes int64
	// ClamDB is the directory of ClamAV signatures the malware check reads
	// (the host's bundle, mounted read-only); empty, nothing is checked and
	// every file says so.
	ClamDB string
	// Sources names the top-level folders that are input sources (plan
	// input-sources.md §5; agentry-cli#833). A source's files count
	// against SourceMaxFiles, apart from the uploads' MaxFiles, and only
	// its executable and binary risky files are checked by ClamAV (D8,
	// SourceRisky); the rest say so (UncheckedNotRisky).
	Sources []string
	// SourceMaxFiles is each source's cap (default 20,000).
	SourceMaxFiles int
}

func (l Limits) withDefaults() Limits {
	if l.MaxFiles <= 0 {
		l.MaxFiles = DefaultMaxFiles
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
	if l.UnpackDir == "" {
		l.UnpackDir = l.ScratchDir
	}
	if l.MaxUnpackedBytes <= 0 {
		l.MaxUnpackedBytes = 512 << 20
	}
	if l.SourceMaxFiles <= 0 {
		l.SourceMaxFiles = DefaultSourceMaxFiles
	}
	return l
}

// Decode decodes every regular file under in into out and writes the
// report to out/decode.json. A file's failure is its outcome; the error is
// for the roots alone. The file cap counts an archive's members with the
// files as staged (D5). It goes in three passes: every file is detected and
// every archive unpacked, the members kept until the end; then everything
// is checked by ClamAV at once; then each file the check did not find is
// decoded.
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
	unpack, err := os.MkdirTemp(lim.UnpackDir, "unpack-")
	if err != nil {
		return rep, fmt.Errorf("decode: the unpack directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(unpack) }()
	d := &decoder{tools: tools, lim: lim, out: out, unpack: unpack, budget: lim.MaxUnpackedBytes, seen: map[string]int{}}
	err = filepath.WalkDir(in, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(in, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !e.Type().IsRegular() {
			d.files = append(d.files, File{Path: rel, Family: FamilyUnknown, Outcome: OutcomeRefused, Reason: "not a regular file; a link is never followed"})
			return nil
		}
		pool := d.poolOf(rel)
		if !d.take(pool) {
			d.files = append(d.files, File{Path: rel, Family: FamilyUnknown, Outcome: OutcomeRefused, Reason: fmt.Sprintf("past the %d-file cap", d.capOf(pool))})
			return nil
		}
		d.file(ctx, p, rel, "", 0, 0)
		return nil
	})
	if err != nil {
		return rep, fmt.Errorf("decode: walk %s: %w", in, err)
	}
	d.scan(ctx)
	for _, p := range d.pending {
		d.decodePending(ctx, p)
	}
	rep.Files = d.files
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

// WhyRepository: the file is inside a git repository's .git directory,
// whose internals are never decoded (InRepository).
const WhyRepository Why = "repository"

// InRepository says whether a report path names a file inside a .git
// directory: some segment before the last is ".git". Its internals (loose
// objects, packs, the index, refs, hooks) are kept as they are: listed,
// checked by ClamAV, never detected, decoded or unpacked. A run that wants
// a repository's history reads it with git; a decode of zlib objects only
// cost a detector start or two per object (agentry-cli#642). A file named
// .git (a worktree's or submodule's pointer) is decoded like any other.
// The host holds a report to the same rule.
func InRepository(rel string) bool {
	segs := strings.Split(rel, "/")
	for _, s := range segs[:len(segs)-1] {
		if s == ".git" {
			return true
		}
	}
	return false
}

// decoder is one Decode: its caps, shared by the files as staged and every
// member of every archive among them.
type decoder struct {
	tools  Tools
	lim    Limits
	out    string
	unpack string
	// seen counts the files and members taken against each pool's cap,
	// by pool (poolOf); budget is what archives may still unpack; seq
	// names each archive's unpack directory.
	seen   map[string]int
	budget int64
	seq    int
	files  []File
	// targets are the files the malware check reads; pending the files
	// left to decode once it has, each holding its place in files.
	targets []scanTarget
	pending []pendingDecode
}

// poolOf is the cap a file counts against: its input source's, by the
// source's folder, or the uploads', "".
func (d *decoder) poolOf(rel string) string {
	if !InSource(rel, d.lim.Sources) {
		return ""
	}
	first, _, _ := strings.Cut(rel, "/")
	return first
}

// capOf is a pool's cap.
func (d *decoder) capOf(pool string) int {
	if pool == "" {
		return d.lim.MaxFiles
	}
	if d.lim.SourceMaxFiles <= 0 {
		return DefaultSourceMaxFiles
	}
	return d.lim.SourceMaxFiles
}

// take counts one file against its pool's cap, or answers false at it.
func (d *decoder) take(pool string) bool {
	if d.seen == nil {
		d.seen = map[string]int{}
	}
	if d.seen[pool] >= d.capOf(pool) {
		return false
	}
	d.seen[pool]++
	return true
}

// target makes a file a target of the malware check: every upload, and in
// an input source only an executable or binary risky file (D8), the rest
// saying why they were not checked.
func (d *decoder) target(idx int, src string) {
	f := &d.files[idx]
	// An archive is checked whole as before, so what it holds below the
	// unpacking's depth and caps is not left unread by ClamAV.
	if InSource(f.Path, d.lim.Sources) && f.Family != FamilyArchive && !SourceRisky(f.MIME, f.Path) {
		f.Check, f.Unchecked = CheckUnchecked, UncheckedNotRisky
		return
	}
	d.targets = append(d.targets, scanTarget{idx: idx, src: src})
}

// pendingDecode is a file detected and waiting for the check before it is
// decoded: its place in the report and what detection made of it.
type pendingDecode struct {
	idx       int
	src, rel  string
	in        string
	size      int64
	kind      contentKind
	detectErr error
	// repository: inside a .git directory, so never detected or decoded.
	repository bool
}

// file takes one file, as staged or unpacked: an archive is unpacked and
// its members taken in turn; anything else waits for the check, then is
// decoded into out (decodePending). Either way the file is a target of the
// check. in is the archive a member came from and depth how many archives
// enclose it. A file inside a .git directory is not detected: it waits for
// the check only.
func (d *decoder) file(ctx context.Context, src, rel, in string, depth int, size int64) {
	repository := InRepository(rel)
	kind, err := contentKind{family: FamilyUnknown}, error(nil)
	if !repository {
		fctx, cancel := context.WithTimeout(ctx, time.Duration(d.lim.PerFileSeconds)*time.Second)
		kind, err = detect(fctx, src, d.tools)
		cancel()
		if err == nil && kind.archive != notArchive {
			d.archive(ctx, src, rel, in, depth, size, kind)
			return
		}
	}
	f := File{Path: rel, Family: kind.family, In: in, MIME: kind.mime, SHA256: sha256File(src)}
	if in != "" {
		f.Size = size
	}
	d.files = append(d.files, f)
	idx := len(d.files) - 1
	d.target(idx, src)
	d.pending = append(d.pending, pendingDecode{idx: idx, src: src, rel: rel, in: in, size: size, kind: kind, detectErr: err, repository: repository})
}

// decodePending decodes one file the check did not find, with a timeout of
// its own; a file it found keeps its place, refused and never read.
func (d *decoder) decodePending(ctx context.Context, p pendingDecode) {
	checked := d.files[p.idx]
	if checked.Check == CheckFound {
		checked.Outcome, checked.Why = OutcomeRefused, WhyFound
		d.files[p.idx] = checked
		return
	}
	if p.repository {
		checked.Outcome, checked.Why = OutcomeUndecodable, WhyRepository
		d.files[p.idx] = checked
		return
	}
	if p.detectErr == nil && p.kind.family == FamilyImage && InSource(p.rel, d.lim.Sources) {
		checked.Outcome, checked.Why = OutcomeUndecodable, WhySourceImage
		d.files[p.idx] = checked
		return
	}
	fctx, cancel := context.WithTimeout(ctx, time.Duration(d.lim.PerFileSeconds)*time.Second)
	f := decodeKind(fctx, p.kind, p.detectErr, p.src, p.rel, d.out, d.tools, d.lim, p.in != "")
	cancel()
	f.In = p.in
	if p.in != "" {
		f.Size = p.size
	}
	f.Check, f.Signature, f.Unchecked = checked.Check, checked.Signature, checked.Unchecked
	f.MIME, f.SHA256 = checked.MIME, checked.SHA256
	d.files[p.idx] = f
}

// archive unpacks one archive into a directory of its own, records it and
// every member, then takes each member that was unpacked. Unpacking shares
// the per-file timeout. The members stay on disk until the decode ends, so
// the check and the decoders read them after every archive is unpacked;
// the unpacked-bytes budget bounds them all together. The archive itself
// is a target of the check, whether or not it was unpacked.
func (d *decoder) archive(ctx context.Context, src, rel, in string, depth int, size int64, kind contentKind) {
	a := File{Path: rel, Family: FamilyArchive, Tool: kind.tool, In: in, MIME: kind.mime, SHA256: sha256File(src)}
	if in != "" {
		a.Size = size
	}
	record := func(a File) {
		d.files = append(d.files, a)
		d.target(len(d.files)-1, src)
	}
	if depth >= MaxDepth {
		a.Outcome, a.Why = OutcomeUndecodable, WhyTooDeep
		record(a)
		return
	}
	dir := filepath.Join(d.unpack, strconv.Itoa(d.seq))
	d.seq++
	if err := os.Mkdir(dir, 0o700); err != nil {
		record(undecodable(a, err.Error()))
		return
	}
	uctx, cancel := context.WithTimeout(ctx, time.Duration(d.lim.PerFileSeconds)*time.Second)
	members, why, opened := d.unpackInto(uctx, src, dir, rel, kind)
	cancel()
	switch {
	case why == WhyUnsafePath:
		a.Outcome = OutcomeRefused
	case !opened:
		a.Outcome = OutcomeUndecodable
	default:
		a.Outcome = OutcomeUnpacked
	}
	a.Why = why
	record(a)
	for _, m := range members {
		mrel := rel + "/" + m.name
		if m.why != "" {
			outcome := OutcomeRefused
			if m.why == WhyEncrypted || m.why == WhyMethod || m.why == WhyDamaged {
				outcome = OutcomeUndecodable
			}
			d.files = append(d.files, File{Path: mrel, Family: FamilyUnknown, Outcome: outcome, In: rel, Why: m.why})
			continue
		}
		d.file(ctx, filepath.Join(dir, filepath.FromSlash(m.name)), mrel, rel, depth+1, m.size)
	}
}

// decodeKind decodes one file of a detected kind into out. A member's text
// is copied out too, cut to size, since the member is not staged.
func decodeKind(ctx context.Context, kind contentKind, err error, src, rel, out string, tools Tools, lim Limits, member bool) File {
	f := File{Path: rel, Family: kind.family}
	if err != nil {
		f.Tool = "tika"
		return undecodable(f, "content detection failed: "+err.Error())
	}
	// The text fast path (detect) has read it as UTF-8 already.
	if kind.text {
		f.Outcome = OutcomeText
		f = markdownView(ctx, f, kind, src, out, tools, lim)
		if !member {
			return f
		}
		f.Output = rel + ".txt"
		dst := filepath.Join(out, filepath.FromSlash(f.Output))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return undecodable(f, err.Error())
		}
		if f = finish(f, src, dst, lim.MaxOutputBytes); f.Outcome == OutcomeDecoded {
			f.Outcome = OutcomeText
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
		err = runTool(ctx, tools.PDFToText, "-layout", "-enc", "UTF-8", "-q", src, text)
	case "pandoc":
		err = runTool(ctx, tools.Pandoc, "--sandbox", "--from="+kind.format, "--to=gfm", "--wrap=none", "--output="+text, src)
	case "tesseract":
		// tesseract writes <base>.txt.
		err = runTool(ctx, tools.Tesseract, src, strings.TrimSuffix(text, ".txt"), "-l", "eng")
	case "agentry-runner":
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

// htmlTypes are the text files that also get a Markdown view (D2): a page
// is code to a reviewer and a document to a reader.
var htmlTypes = map[string]bool{"text/html": true, "application/xhtml+xml": true}

// markdownView converts an HTML file read as text to Markdown with
// sandboxed pandoc, into out/<rel>.md, cut to the text cap. The file stays
// text whatever happens: a conversion that fails only leaves no Markdown.
func markdownView(ctx context.Context, f File, kind contentKind, src, out string, tools Tools, lim Limits) File {
	if !htmlTypes[kind.mime] {
		return f
	}
	scratch, err := os.MkdirTemp(lim.ScratchDir, "decode-")
	if err != nil {
		return f
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	text := filepath.Join(scratch, "out.md")
	if err := runTool(ctx, tools.Pandoc, "--sandbox", "--from=html", "--to=gfm", "--wrap=none", "--output="+text, src); err != nil {
		return f
	}
	rel := f.Path + ".md"
	dst := filepath.Join(out, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return f
	}
	view := finish(File{Path: f.Path}, text, dst, lim.MaxOutputBytes)
	if view.Outcome != OutcomeDecoded {
		_ = os.Remove(dst)
		return f
	}
	f.Markdown, f.MarkdownBy, f.MarkdownBytes = rel, "pandoc", view.Bytes
	return f
}

func undecodable(f File, reason string) File {
	f.Output, f.Outcome, f.Reason = "", OutcomeUndecodable, reason
	f.Markdown, f.MarkdownBy, f.MarkdownBytes = "", "", 0
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

// isText says whether a file that is no document reads as text, the text
// fast path's test in front of Tika (detect): its first 64 KiB are UTF-8
// (a cut rune at the end aside) with no NUL.
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

// sha256File is the lower-case hex SHA-256 of the file at src, read
// through a link never; "" when it cannot be read, and then the host does
// not look it up.
func sha256File(src string) string {
	if fi, err := os.Lstat(src); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	f, err := os.Open(src) //nolint:gosec // a staged input or an unpacked member, read to be hashed
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
