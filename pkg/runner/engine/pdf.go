package engine

// export_pdf's run-time half (plan scratch-and-pdf-reports.md §5, D13-D15,
// D19; agentry-cli#730). The compiler grants the tool with the scratch
// folder; the engine stamps into the run's own tools spec what the plan
// cannot hold: the run's theme, the QuickAgent the PDF names as its author,
// and the ledger each exported PDF is recorded in. After the agent ends,
// the receipt's pdfs block names the theme and every PDF whose bytes are
// still the ones the ledger recorded: the record the site and the desktop
// hold a PDF to before they open it in place (D15).

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// PDFLedgerFile is the PDF ledger under the run's receipts.
const PDFLedgerFile = "pdfs.ndjson"

// ReportTheme is a run's report theme: a built-in theme's name, and what
// chose it.
type ReportTheme struct {
	Name string
	From ThemeSource
}

// ThemeSource is what chose a run's report theme, as the receipt says it.
type ThemeSource string

// What can choose a run's report theme (agentry-cli#731, DR-82).
const (
	ThemeFromFlag    ThemeSource = "the --report-theme flag"
	ThemeFromAccount ThemeSource = "the account's report theme"
	ThemeFromConfig  ThemeSource = "report.theme in the CLI's config"
	ThemeFromDefault ThemeSource = "the default"
)

// stamp is the theme's document as the tools spec carries it.
func (t ReportTheme) stamp() (*toolsrv.ThemeStamp, error) {
	name := t.Name
	if name == "" {
		name = toolsrv.DefaultTheme
	}
	doc, err := toolsrv.BuiltinTheme(name)
	if err != nil {
		return nil, err
	}
	return toolsrv.StampTheme(doc)
}

// from is what chose the theme, for the receipt.
func (t ReportTheme) from() ThemeSource {
	if t.Name == "" || t.From == "" {
		return ThemeFromDefault
	}
	return t.From
}

// grantPDF stamps export_pdf's run-time half into the run's rendered tools
// spec when the plan grants the tool: the run's theme, the plan's ref as
// the author, and the ledger prepareRunDirs made.
func grantPDF(gatewayDir string, plan *spec.Plan, theme ReportTheme, rd *runDirs) error {
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
	if !ts.PDF.Enabled {
		return nil
	}
	stamp, err := theme.stamp()
	if err != nil {
		return err
	}
	rd.pdfTheme = stamp
	ts.PDF.Theme = stamp
	ts.PDF.Author = plan.Ref
	if rd.pdfLedger != "" {
		ts.PDF.Ledger = path.Join(spec.OutMount, "receipts", PDFLedgerFile)
	}
	out, err := json.MarshalIndent(&ts, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(specPath, append(out, '\n'), 0o644) //nolint:gosec // read by the tools container
}

// withReportTheme adds the receipt's line for the run's report theme when
// the plan grants export_pdf: which theme, its version and digest, and what
// chose it, whether or not the run exports a PDF.
// A theme that does not stamp adds no line: grantPDF fails that run.
func withReportTheme(enforced map[string]string, tools []string, theme ReportTheme) map[string]string {
	if !slices.Contains(tools, toolsrv.ToolExportPDF) {
		return enforced
	}
	st, err := theme.stamp()
	if err != nil {
		return enforced
	}
	out := maps.Clone(enforced)
	if out == nil {
		out = map[string]string{}
	}
	out["pdf.theme"] = fmt.Sprintf("%s %s (sha256:%s), chosen by %s; the model cannot choose another", st.Name, st.Version, st.SHA256, theme.from())
	return out
}

// ReadPDFLedger reads a run's PDF ledger. A missing ledger is none; a line
// that does not decode is skipped.
func ReadPDFLedger(file string) ([]toolsrv.PDFEntry, error) {
	f, err := os.Open(file) //nolint:gosec // a run's own receipt
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []toolsrv.PDFEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e toolsrv.PDFEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// pdfRecord is the receipt's pdfs block: the run's theme and each PDF the
// ledger names (its last record, where one path was exported twice) whose
// file under reports/ still has the recorded SHA-256. Nil when the run
// exported none.
func pdfRecord(rd *runDirs) *receipt.PDFs {
	if rd.pdfLedger == "" || rd.pdfTheme == nil {
		return nil
	}
	entries, err := ReadPDFLedger(rd.pdfLedger)
	if err != nil || len(entries) == 0 {
		return nil
	}
	last := map[string]toolsrv.PDFEntry{}
	for _, e := range entries {
		last[e.Path] = e
	}
	rec := &receipt.PDFs{Theme: receipt.PDFTheme{Name: rd.pdfTheme.Name, Version: rd.pdfTheme.Version, SHA256: rd.pdfTheme.SHA256}}
	for p, e := range last {
		sum, size, err := reportDigest(rd.outDir, p)
		if err != nil || sum != e.SHA256 {
			continue
		}
		rec.Files = append(rec.Files, receipt.PDFFile{Path: p, SHA256: sum, Pages: e.Pages, Bytes: size})
	}
	if len(rec.Files) == 0 {
		return nil
	}
	sort.Slice(rec.Files, func(i, j int) bool { return rec.Files[i].Path < rec.Files[j].Path })
	return rec
}

// reportDigest hashes the regular file at rel (reports/…) beneath outDir,
// reached by no link.
func reportDigest(outDir, rel string) (string, int64, error) {
	rel = path.Clean(rel)
	if !strings.HasPrefix(rel, "reports/") {
		return "", 0, fmt.Errorf("%q is not a report", rel)
	}
	root, err := os.OpenRoot(outDir)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = root.Close() }()
	parts := strings.Split(rel, "/")
	for i := range parts {
		fi, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil {
			return "", 0, err
		}
		if fi.Mode()&fs.ModeSymlink != 0 || (i == len(parts)-1 && !fi.Mode().IsRegular()) {
			return "", 0, fmt.Errorf("%q is not a regular file reached by no link", rel)
		}
	}
	f, err := root.Open(rel)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%q is not a regular file", rel)
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
