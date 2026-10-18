package engine

// A run's deliverables: the files it was for. The agent marks one with
// write_report's or export_pdf's deliverable argument and the tools server
// records the mark in receipts/deliverables.ndjson. That ledger is the agent's
// word and nothing more. Once the agent has ended, the runner reads it back
// and checks every path and title again, hashes each file itself, and writes
// the envelope's deliverables block; only that block names a deliverable.
// When the agent marked none, the runner picks: the PDFs the run exported,
// else the files in reports/ (the browser's screenshots aside), else the final
// message, written to reports/deliverable.md. A run that failed lists none.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/pkg/runner/deliver"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// DeliverablesLedgerFile is the deliverables ledger under the run's receipts.
const DeliverablesLedgerFile = "deliverables.ndjson"

// DeliverableMessageFile is where the runner writes a run's final message
// when it is all the run produced.
const DeliverableMessageFile = "reports/deliverable.md"

// maxPickedPaths bounds how many files of reports/ the runner looks at when it
// picks: it reads them in path order and lists the first
// receipt.MaxDeliverables, so a folder of millions costs no more than this.
var maxPickedPaths = 10000

// maxPickedVisits bounds the entries of reports/, folders included, it walks.
var maxPickedVisits = 50000

// messageTitle is the title of a final message the runner wrote down.
const messageTitle = "Result"

// grantDeliverables stamps the deliverables ledger prepareRunDirs made into
// the run's rendered tools spec.
func grantDeliverables(gatewayDir string, rd *runDirs) error {
	if rd.deliverablesLedger == "" {
		return nil
	}
	specPath := filepath.Join(gatewayDir, "toolspec.json")
	b, err := os.ReadFile(specPath) //nolint:gosec // our own rendered config
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var ts toolsrv.Spec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ts); err != nil {
		return fmt.Errorf("the rendered tools spec does not decode as the tools server reads it: %w", err)
	}
	ts.Deliverables.Ledger = path.Join(spec.OutMount, "receipts", DeliverablesLedgerFile)
	out, err := json.MarshalIndent(&ts, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(specPath, append(out, '\n'), 0o644) //nolint:gosec // read by the tools container
}

// The most a deliverables ledger is read as: its bytes and its lines.
const (
	maxLedgerBytes = 8 << 20
	maxLedgerLines = 10000
)

// ReadDeliverablesLedger reads a run's deliverables ledger. A missing
// ledger is none, and so is anything that is not a regular file; a line that
// does not decode is skipped, and a ledger is read only to its caps.
func ReadDeliverablesLedger(file string) ([]toolsrv.DeliverableEntry, error) {
	fi, err := os.Lstat(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, nil
	}
	f, err := os.Open(file) //nolint:gosec // a run's own receipt
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []toolsrv.DeliverableEntry
	sc := bufio.NewScanner(io.LimitReader(f, maxLedgerBytes))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() && len(out) < maxLedgerLines {
		var e toolsrv.DeliverableEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// deliverablesRecord is the envelope's deliverables block for a run that
// succeeded: the marked files, else the runner's pick. text is the run's final
// message when it is plain text a deliverable.md may hold, and "" when there is
// none or it must not become a file (a session's chat, a structured result).
// Nil when the run has nothing to list.
func deliverablesRecord(rd *runDirs, pdfs *receipt.PDFs, text string) *receipt.Deliverables {
	pdfDigest := map[string]string{}
	if pdfs != nil {
		for _, f := range pdfs.Files {
			pdfDigest[f.Path] = f.SHA256
		}
	}
	files := markedDeliverables(rd, pdfDigest)
	if len(files) == 0 {
		files = pickedDeliverables(rd, pdfs)
	}
	if len(files) == 0 && strings.TrimSpace(text) != "" {
		if f, ok := writeMessage(rd, text); ok {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return nil
	}
	rec := &receipt.Deliverables{}
	if len(files) > receipt.MaxDeliverables {
		rec.Truncated = true
		files = files[:receipt.MaxDeliverables]
	}
	rec.Files = files
	return rec
}

// markedDeliverables are the ledger's files the runner can stand behind, in
// the order they were first marked. A mark taken off, a path that is not a
// regular file reached by no link beneath reports/, a PDF the PDF ledger does
// not vouch for with the same digest, and a title that is not a line of plain
// text (it falls back to the file's name) are each handled here, not trusted.
func markedDeliverables(rd *runDirs, pdfDigest map[string]string) []receipt.Deliverable {
	if rd.deliverablesLedger == "" {
		return nil
	}
	entries, _ := ReadDeliverablesLedger(rd.deliverablesLedger)
	var order []string
	last := map[string]toolsrv.DeliverableEntry{}
	for _, e := range entries {
		p := path.Clean(e.Path)
		if _, seen := last[p]; !seen {
			order = append(order, p)
		}
		last[p] = e
	}
	var files []receipt.Deliverable
	for _, p := range order {
		e := last[p]
		if !e.Marked {
			continue
		}
		f, ok := describe(rd, p, e.Title)
		if !ok {
			continue
		}
		if strings.EqualFold(path.Ext(p), ".pdf") && pdfDigest[p] != f.SHA256 {
			continue
		}
		f.Marked = true
		files = append(files, f)
		if len(files) > receipt.MaxDeliverables {
			break
		}
	}
	return files
}

// pickedDeliverables are what the runner lists when the agent marked none:
// every PDF the run exported (the PDF ledger's), else every other regular file
// in reports/ outside reports/browser/, sorted by path.
func pickedDeliverables(rd *runDirs, pdfs *receipt.PDFs) []receipt.Deliverable {
	if pdfs != nil && len(pdfs.Files) > 0 {
		var files []receipt.Deliverable
		for _, pf := range pdfs.Files {
			if f, ok := describe(rd, pf.Path, ""); ok && f.SHA256 == pf.SHA256 {
				files = append(files, f)
			}
		}
		if len(files) > 0 {
			return files
		}
	}
	reports := filepath.Join(rd.outDir, "reports")
	var paths []string
	visited := 0
	_ = filepath.WalkDir(reports, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		visited++
		if visited > maxPickedVisits {
			return fs.SkipAll
		}
		rel, rerr := filepath.Rel(rd.outDir, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir() && rel == "reports/browser":
			return fs.SkipDir
		case d.IsDir() || !d.Type().IsRegular():
			// A link or a device is never listed, and a link to a
			// folder is not walked into.
			return nil
		case strings.EqualFold(path.Ext(rel), ".pdf"):
			// A PDF counts only when the PDF ledger vouches for it.
			return nil
		}
		paths = append(paths, rel)
		if len(paths) >= maxPickedPaths {
			return fs.SkipAll
		}
		return nil
	})
	sort.Strings(paths)
	var files []receipt.Deliverable
	for _, p := range paths {
		if f, ok := describe(rd, p, ""); ok {
			files = append(files, f)
		}
		if len(files) > receipt.MaxDeliverables {
			break
		}
	}
	return files
}

// describe is the block's entry for rel (reports/…): its digest and size, the
// runner's own, from a regular file reached by no link. A file whose path
// holds the value of one of the run's secrets is left out, as the ledgers'
// masking would have changed it. A title that is not a line of plain text, or
// none, is the file's name without its extension, made one line of plain
// text.
func describe(rd *runDirs, rel, title string) (receipt.Deliverable, bool) {
	rel = path.Clean(rel)
	if !strings.HasPrefix(rel, "reports/") || strings.HasPrefix(rel, "reports/browser/") {
		return receipt.Deliverable{}, false
	}
	if rd.redact != nil && rd.redact.String(rel) != rel {
		return receipt.Deliverable{}, false
	}
	sum, size, err := reportDigest(rd.outDir, rel)
	if err != nil {
		return receipt.Deliverable{}, false
	}
	title = strings.TrimSpace(title)
	if title == "" || toolsrv.CheckDeliverableTitle(title) != nil {
		title = plainLine(strings.TrimSuffix(path.Base(rel), path.Ext(rel)))
	}
	return receipt.Deliverable{
		Path:   rel,
		Title:  title,
		SHA256: sum,
		Bytes:  size,
		Type:   fileType(rel),
	}, true
}

// plainLine makes a file's name a title: every control, format, line-break
// and bidirectional formatting character an underscore, cut to the longest
// title.
func plainLine(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == toolsrv.MaxDeliverableTitleRunes {
			break
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Bidi_Control, r) || r == '\u2028' || r == '\u2029' || r == utf8.RuneError {
			r = '_'
		}
		b.WriteRune(r)
		n++
	}
	if out := strings.TrimSpace(b.String()); out != "" {
		return out
	}
	return "file"
}

// fileTypeRE is what a type may be: a short extension of letters and digits.
var fileTypeRE = regexp.MustCompile(`^[a-z0-9]{1,10}$`)

// fileType is the file name's extension in lower case, without its dot, when
// it is a short run of letters and digits, and empty otherwise.
func fileType(rel string) string {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(rel), "."))
	if fileTypeRE.MatchString(ext) {
		return ext
	}
	return ""
}

// writeMessage writes the final message to reports/deliverable.md, new, and
// lists it. The text is already masked by the run's redactor.
func writeMessage(rd *runDirs, text string) (receipt.Deliverable, bool) {
	root, err := os.OpenRoot(rd.outDir)
	if err != nil {
		return receipt.Deliverable{}, false
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll("reports", 0o755); err != nil {
		return receipt.Deliverable{}, false
	}
	f, err := root.OpenFile(DeliverableMessageFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return receipt.Deliverable{}, false
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	_, werr := f.WriteString(text)
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = root.Remove(DeliverableMessageFile)
		return receipt.Deliverable{}, false
	}
	d, ok := describe(rd, DeliverableMessageFile, messageTitle)
	if !ok {
		return receipt.Deliverable{}, false
	}
	return d, true
}

// finishDeliverables lists a successful run's deliverables in its envelope
// and, when the run was given a folder (Params.DeliverTo), copies them there
// and records each copy; result.json is then written again with both blocks.
// It runs after teardown has stopped the run's containers and found nothing it
// could not delete, so no process of the run can change a file after its
// digest, and a run teardown fails lists and delivers nothing. It never fails
// the run: a copy that was refused or failed is a record, nothing more. text
// is as for deliverablesRecord.
func finishDeliverables(ctx context.Context, d dockerx.Client, env *receipt.Envelope, rd *runDirs, p Params, text string) {
	env.Deliverables = deliverablesRecord(rd, env.PDFs, text)
	toDestinations := len(p.Destinations) > 0 && (env.Deliverables != nil || resultToSend(env) != nil)
	if env.Deliverables == nil && !toDestinations {
		return
	}
	if env.Deliverables != nil && p.DeliverTo != "" {
		env.Deliveries = deliver.ToFolder(p.OutDir, env.Deliverables.Files, deliver.FolderOptions{
			Root:  p.DeliverTo,
			Agent: deliver.AgentName(p.Plan.Ref),
			Label: deliver.Label(env.Deliverables.Files, env.RunID),
		})
	}
	if toDestinations {
		var files []receipt.Deliverable
		if env.Deliverables != nil {
			files = env.Deliverables.Files
		}
		env.Deliveries = append(env.Deliveries, sendToDestinations(ctx, d, deliveryRun{
			RunDir: p.OutDir, RunID: env.RunID, Ref: env.Ref, Digest: env.Digest, Result: resultToSend(env), Files: files,
			Image: p.Plan.Proxy.Image, Limits: p.limits(), Engine: p.engine(), Root: p.RunRoot, Stderr: p.Stderr,
		}, p.Destinations)...)
	}
	if len(env.Deliveries) > 0 {
		if err := deliver.WriteLedger(p.OutDir, env.Deliveries, false); err != nil {
			_, _ = fmt.Fprintf(p.Stderr, "warning: the deliveries could not be recorded in receipts/%s: %v\n", deliver.LedgerFile, err)
		}
	}
	if err := env.Write(p.OutDir); err != nil {
		_, _ = fmt.Fprintf(p.Stderr, "warning: %v\n", err)
	}
}

// plainResultText is a run's final result as the plain text a deliverable.md may
// hold: the string a text result is, and "" for a structured result.
func plainResultText(result json.RawMessage) string {
	var s string
	if json.Unmarshal(result, &s) != nil {
		return ""
	}
	return s
}
