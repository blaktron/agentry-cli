package build

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/blaktron/agentry/pkg/reportdesign"

	"github.com/blaktron/agentry-cli/pkg/runner/clamdb"
	"github.com/blaktron/agentry-cli/pkg/runner/decode"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/hybrid"
	"github.com/blaktron/agentry-cli/runnerimage"
	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
	"github.com/blaktron/agentry-cli/runnerimage/docpages"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// Document is the one document a Design run builds its design around: a
// PDF or a Word document, as the person gave it.
type Document struct {
	// Name is its file name, shown to the model and in the receipt.
	Name string
	Data []byte
}

// MaxDocumentBytes caps the document.
const MaxDocumentBytes = docpages.MaxDocumentBytes

// DocumentKind is the kind a document's own bytes say it is: a PDF by
// its header, a Word document by its archive's signature and its .docx
// name. Anything else is refused before the run.
func DocumentKind(name string, b []byte) (docpages.Kind, error) {
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case len(b) > MaxDocumentBytes:
		return "", fmt.Errorf("a document is at most %d MB", MaxDocumentBytes>>20)
	case bytes.Contains(b[:min(len(b), 1024)], []byte("%PDF-")) && ext != ".docx":
		return docpages.KindPDF, nil
	case bytes.HasPrefix(b, []byte("PK\x03\x04")) && ext == ".docx":
		return docpages.KindDOCX, nil
	}
	return "", errors.New("a document is a PDF or a Word document (.docx)")
}

// The decode step's calls, variables so the tests read a document
// without a daemon.
var (
	ensureRunner = func(ctx context.Context, d dockerx.Client, o Options, tag, ctxDir string, logw io.Writer) error {
		return gateway.EnsureInstalled(ctx, d, o.Policy, tag, ctxDir, logw, true)
	}
	ensureDecode     = decode.Ensure
	ensureSignatures = clamdb.Ensure
	runDecode        = decode.Run
	runPages         = decode.RunPages
	hybridClient     = hybrid.FromPolicy
)

// readDocument is what the run was given of the document.
type readDocument struct {
	kind    docpages.Kind
	decoded *decode.Result
	pages   *decode.Pages
	// dir is the staged folder, inputs/document/ in the run.
	dir    string
	staged docpages.Staged
}

// readTheDocument puts the document through the decode step as any input
// is (the type check, the malware check and its text), then has the pages
// role read its pages or its theme, and stages what was read under work
// in the layout the document tools read. A finding refuses the run; a
// document that cannot be read is staged as its kind of failure, and the
// run goes on with the brief.
func (o Options) readTheDocument(ctx context.Context, d dockerx.Client, work string, logw io.Writer) (*readDocument, error) {
	doc := o.Document
	kind, err := DocumentKind(doc.Name, doc.Data)
	if err != nil {
		return nil, engine.Classify(engine.ExitUsage, err)
	}
	given := filepath.Join(work, "document", "given")
	decodedDir := filepath.Join(work, "document", "decoded")
	pagesDir := filepath.Join(work, "document", "pages")
	for _, dir := range []string{given, decodedDir, pagesDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, engine.Classify(engine.ExitCLI, err)
		}
	}
	base := "document." + string(kind)
	file := filepath.Join(given, base)
	if err := os.WriteFile(file, doc.Data, 0o644); err != nil { //nolint:gosec // a copy the decode containers read
		return nil, engine.Classify(engine.ExitCLI, err)
	}

	runnerTag := o.Policy.RunnerImageTag(runnerimage.ContentVersion())
	ctxDir := filepath.Join(o.Home, "image")
	if err := ensureRunner(ctx, d, o, runnerTag, ctxDir, logw); err != nil {
		return nil, engine.Classify(engine.ExitGateway, err)
	}
	img, err := ensureDecode(ctx, d, runnerTag, o.Policy, ctxDir, logw)
	if err != nil {
		return nil, engine.Classify(engine.ExitGateway, err)
	}
	img.Signatures = ensureSignatures(ctx, o.Home, o.Policy.Images.ClamAVSignaturesRef(), logw)
	limits := o.Policy.Docker.Limits.WithDefaults()
	dec, err := runDecode(ctx, d, img, given, decodedDir, limits, logw)
	if err != nil {
		return nil, engine.Classify(engine.ExitGateway, err)
	}
	lookup, err := hybridClient(o.Policy)
	if err != nil {
		return nil, engine.Classify(engine.ExitPolicy, fmt.Errorf("the Hybrid Analysis key: %w", err))
	}
	dec.LookUp(ctx, lookup)
	if dec.Refused() {
		return nil, engine.Classify(engine.ExitPolicy, fmt.Errorf("the design was refused before the agent started: %s", dec.Refusal()))
	}
	var f runnerdecode.File
	for _, rf := range dec.Report.Files {
		if rf.Path == base {
			f = rf
		}
	}
	if !detectedAs(f, kind) {
		return nil, engine.Classify(engine.ExitUsage, fmt.Errorf("--document %s is not a PDF or a Word document by its content", doc.Name))
	}
	var pages *decode.Pages
	if f.Outcome == runnerdecode.OutcomeDecoded {
		if pages, err = runPages(ctx, d, img, file, kind, pagesDir, limits, logw); err != nil {
			return nil, engine.Classify(engine.ExitGateway, err)
		}
	} else {
		pages = &decode.Pages{Facts: docpages.Facts{Schema: docpages.Schema, Kind: kind, Failure: docpages.FailureMalformed}, Image: img}
	}
	var text []byte
	if pages.Facts.Failure == "" && f.HasText() {
		textFile := filepath.Join(work, "document", "text.txt")
		if err := dec.CopyText(base, textFile); err != nil {
			return nil, engine.Classify(engine.ExitCLI, err)
		}
		if text, err = os.ReadFile(textFile); err != nil { //nolint:gosec // the decode's own text, copied capped
			return nil, engine.Classify(engine.ExitCLI, err)
		}
	}
	r := &readDocument{kind: kind, decoded: dec, pages: pages, dir: filepath.Join(work, "in", docpages.StagedDir)}
	if r.staged, err = stageDocument(r.dir, doc.Name, pages, text, o.Logo != nil); err != nil {
		return nil, engine.Classify(engine.ExitCLI, err)
	}
	return r, nil
}

// detectedAs says the decode step's type check found the document to be
// the kind its bytes claimed.
func detectedAs(f runnerdecode.File, kind docpages.Kind) bool {
	switch kind {
	case docpages.KindPDF:
		return f.Family == runnerdecode.FamilyPDF
	case docpages.KindDOCX:
		return f.Family == runnerdecode.FamilyDocument && f.MIME == "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	}
	return false
}

// stageDocument writes what was read of the document under dir, in
// docpages' layout: the record, the pages at both resolutions, each page's
// text (a PDF's pages are split at the form feeds pdftotext writes), and
// a Word document's pictures. The document itself is never written.
func stageDocument(dir, name string, p *decode.Pages, text []byte, logoGiven bool) (docpages.Staged, error) {
	root := filepath.Dir(dir)
	write := func(rel string, b []byte) error {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, b, 0o644) //nolint:gosec // an input the run reads
	}
	for i := range p.Full {
		if err := write(docpages.FullFile(i+1), p.Full[i]); err != nil {
			return docpages.Staged{}, err
		}
		if err := write(docpages.ViewFile(i+1), p.View[i]); err != nil {
			return docpages.Staged{}, err
		}
	}
	for i, m := range p.Media {
		if err := write(docpages.MediaFileOf(i+1), m); err != nil {
			return docpages.Staged{}, err
		}
	}
	texts, cut := pageTexts(p.Facts, text)
	for i, t := range texts {
		if err := write(docpages.TextFile(i+1), []byte(t)); err != nil {
			return docpages.Staged{}, err
		}
	}
	st := docpages.StagedFrom(p.Facts, name, len(p.Media), len(texts), cut, logoGiven)
	if err := st.Validate(); err != nil {
		return st, err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return st, err
	}
	return st, write(docpages.StagedFile, append(b, '\n'))
}

// pageTexts is the staged text: a PDF's rendered pages one each, a Word
// document's whole text as one, each within its cap; cut says one was cut.
func pageTexts(f docpages.Facts, text []byte) ([]string, bool) {
	if len(text) == 0 || f.Failure != "" {
		return nil, false
	}
	s := docpages.Text(string(text))
	cut := false
	capTo := func(t string, n int) string {
		if len(t) <= n {
			return t
		}
		cut = true
		t = t[:n]
		for !utf8.ValidString(t) {
			t = t[:len(t)-1]
		}
		return t
	}
	if f.Kind == docpages.KindDOCX {
		return []string{capTo(s, docpages.MaxDocxText)}, cut
	}
	parts := strings.Split(s, "\f")
	var out []string
	for i := 0; i < f.Rendered && i < len(parts); i++ {
		out = append(out, capTo(parts[i], docpages.MaxPageText))
	}
	return out, cut
}

// documentLogo is the logo the run cropped from the document: the last
// crop the document ledger records, cropped again here from this
// process's own copy of the page or the picture and held to the core's
// rules. None when the person gave a logo, or the run cropped none.
func (o Options) documentLogo(doc *readDocument) (reportdesign.Logo, bool, error) {
	if doc == nil || doc.staged.LogoGiven || doc.staged.Failure != "" {
		return reportdesign.Logo{}, false, nil
	}
	entries, err := engine.ReadDocumentLedger(filepath.Join(o.OutDir, "receipts", engine.DocumentLedgerFile))
	if err != nil {
		return reportdesign.Logo{}, false, nil
	}
	var last *toolsrv.DocumentEntry
	for i := range entries {
		if entries[i].Tool == toolsrv.ToolCropLogo {
			last = &entries[i]
		}
	}
	if last == nil {
		return reportdesign.Logo{}, false, nil
	}
	var src []byte
	scale := 1
	switch {
	case last.Page >= 1 && last.Page <= len(doc.pages.Full) && last.Box != nil:
		src, scale = doc.pages.Full[last.Page-1], docpages.RenderPPI/docpages.ViewPPI
	case last.Media >= 1 && last.Media <= len(doc.pages.Media):
		src = doc.pages.Media[last.Media-1]
	default:
		return reportdesign.Logo{}, false, errors.New("the document ledger names a crop of no page or picture the run was given")
	}
	b, err := docpages.CropLogo(src, last.Box, scale)
	if err != nil {
		return reportdesign.Logo{}, false, fmt.Errorf("the logo taken from the document: %w", err)
	}
	logo, err := reportdesign.NormalizeLogo(b)
	if err != nil {
		return reportdesign.Logo{}, false, fmt.Errorf("the logo taken from the document: %w", err)
	}
	return logo, true, nil
}

// documentTask is the task's account of the document.
func documentTask(b *strings.Builder, doc *readDocument) {
	st := doc.staged
	if st.Failure != "" {
		fmt.Fprintf(b, "The person gave a document, %q, to build the design around, but it could not be read, because %s. Design from the brief, and say so in your answer.\n", st.Name, docpages.Failures[st.Failure])
		return
	}
	switch st.Kind {
	case docpages.KindPDF:
		fmt.Fprintf(b, "The person gave a document to build the design around, %q, a PDF of %d page(s), of which the first %d were rendered. Look at it with document_pages before you plan, and read the guide's page on designing from a document.\n", st.Name, st.Pages, st.Rendered)
	default:
		fmt.Fprintf(b, "The person gave a document to build the design around, %q, a Word document: its theme's fonts and colours, its running text, its pictures and its text, but no rendered page. Read it with document_pages before you plan, and read the guide's page on designing from a document.\n", st.Name)
	}
	if st.LogoGiven {
		b.WriteString("They gave their own logo too: keep it, never a crop of the document.\n")
	}
}
