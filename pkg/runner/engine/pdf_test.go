package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestTheReceiptNamesTheTheme (plan scratch-and-pdf-reports.md §3
// invariants 9 and 10, D15, D19; agentry-cli#730; report-designs.md
// invariant 9, agentry-cli#784): a release's run gets export_pdf's run-time
// half in its own tools spec (the design, verbatim, the release as the
// author, the ledger), and its receipt names the design by name, theme,
// version, SHA-256 and what chose it, and each exported PDF whose bytes are
// still the recorded ones with its form, the last record of a path
// exported twice; a PDF changed after its export, or gone, is not named.
func TestTheReceiptNamesTheTheme(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	p.KeepRunDir = true
	reports := filepath.Join(p.OutDir, "reports")
	first, second, changed := []byte("%PDF-1.7 first"), []byte("%PDF-1.7 second"), []byte("%PDF-1.7 changed")
	f.OnForeground = func(r dockerx.RunSpec) {
		if !strings.HasSuffix(r.Name, "-agent") {
			return
		}
		_ = os.MkdirAll(filepath.Join(reports, "sub"), 0o755)
		_ = os.WriteFile(filepath.Join(reports, "sub", "a.pdf"), second, 0o644)
		_ = os.WriteFile(filepath.Join(reports, "b.pdf"), changed, 0o644)
		lines := []string{
			`{"path":"reports/sub/a.pdf","sha256":"` + sha(first) + `","pages":1,"bytes":14}`,
			`{"path":"reports/b.pdf","sha256":"` + sha([]byte("%PDF-1.7 b as exported")) + `","pages":2,"bytes":20}`,
			`{"path":"reports/gone.pdf","sha256":"` + sha(first) + `","pages":1,"bytes":14}`,
			`{"path":"reports/sub/a.pdf","sha256":"` + sha(second) + `","pages":4,"bytes":15,"form":"memo","logo":true}`,
		}
		_ = os.WriteFile(filepath.Join(p.OutDir, "receipts", PDFLedgerFile), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	plain, err := toolsrv.BuiltinTheme(toolsrv.DefaultTheme)
	if err != nil {
		t.Fatal(err)
	}
	if env.PDFs == nil {
		t.Fatal("the receipt has no pdfs block")
	}
	if th := env.PDFs.Theme; th.Name != "Plain" || th.BasedOn != "plain" || th.Version != "1" || th.SHA256 != sha(plain) || th.From != string(ThemeFromDefault) {
		t.Errorf("theme = %+v", th)
	}
	if len(env.PDFs.Files) != 1 {
		t.Fatalf("files = %+v, want sub/a.pdf alone", env.PDFs.Files)
	}
	if got := env.PDFs.Files[0]; got.Path != "reports/sub/a.pdf" || got.SHA256 != sha(second) || got.Pages != 4 || got.Bytes != int64(len(second)) || got.Form != "memo" || !got.Logo {
		t.Errorf("file = %+v", got)
	}
	result, err := os.ReadFile(filepath.Join(p.OutDir, "result.json"))
	if err != nil || !strings.Contains(string(result), `"pdfs"`) {
		t.Errorf("result.json lacks the pdfs block: %v\n%s", err, result)
	}

	b, err := os.ReadFile(filepath.Join(p.RunRoot, "runs", p.RunID, "cfg", "gateway", "toolspec.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ts toolsrv.Spec
	if err := json.Unmarshal(b, &ts); err != nil {
		t.Fatal(err)
	}
	if !ts.PDF.Enabled || ts.PDF.Theme == nil || ts.PDF.Theme.Document != string(plain) || ts.PDF.Theme.SHA256 != sha(plain) ||
		ts.PDF.Author != p.Plan.Ref || ts.PDF.Ledger != "/out/receipts/"+PDFLedgerFile {
		t.Errorf("the run's tools spec pdf = %+v", ts.PDF)
	}
}

// A run that exported no PDF has no pdfs block, and a run without the
// grant gets no ledger.
func TestNoExportNoPDFsBlock(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if env.PDFs != nil {
		t.Errorf("pdfs = %+v", env.PDFs)
	}
	if fi, err := os.Stat(filepath.Join(p.OutDir, "receipts", PDFLedgerFile)); err != nil || fi.Size() != 0 {
		t.Errorf("the granted run's ledger: %v", err)
	}
}

// TestTheRunsReportThemeIsStampedAndNamed (agentry-cli#731, #784): the
// design the caller chose, a built-in by name or a whole document, is the
// one the tools spec carries as its canonical document, and the receipt
// names it and what chose it whether or not a PDF was exported; a run told
// nothing takes Plain, chosen by the default; a design that breaks a rule
// fails the run before it starts.
func TestTheRunsReportThemeIsStampedAndNamed(t *testing.T) {
	modern, err := toolsrv.BuiltinTheme("modern")
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, modern); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		theme      ReportTheme
		want, name string
		from       string
	}{
		{ReportTheme{Name: "classic", From: ThemeFromFlag}, "classic", "Classic", "the --report-theme flag"},
		{ReportTheme{Design: compact.Bytes(), From: ThemeFromAccount}, "modern", "Modern", "the account's report design"},
		{ReportTheme{}, "plain", "Plain", "the default"},
	} {
		p, f := fixture(t)
		happyFake(f)
		p.KeepRunDir = true
		p.ReportTheme = c.theme
		env, err := Run(context.Background(), f, p)
		if err != nil {
			t.Fatalf("%s: run: %v", c.want, err)
		}
		doc, err := toolsrv.BuiltinTheme(c.want)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(p.RunRoot, "runs", p.RunID, "cfg", "gateway", "toolspec.json"))
		if err != nil {
			t.Fatal(err)
		}
		var ts toolsrv.Spec
		if err := json.Unmarshal(b, &ts); err != nil {
			t.Fatal(err)
		}
		if ts.PDF.Theme == nil || ts.PDF.Theme.Name != c.name || ts.PDF.Theme.BasedOn != c.want || ts.PDF.Theme.Document != string(doc) {
			t.Errorf("%s: the tools spec's theme = %+v", c.want, ts.PDF.Theme)
		}
		line := env.CapabilitiesEnforced["pdf.design"]
		if !strings.HasPrefix(line, `"`+c.name+`", based on `+c.want+", version 1 (sha256:"+sha(doc)+")") || !strings.Contains(line, "chosen by "+c.from) ||
			!strings.Contains(line, "only each PDF's form (report, letter, memo)") {
			t.Errorf("%s: the receipt's pdf.design line = %q", c.want, line)
		}
	}

	// A design with the person's logo travels whole, the logo in its
	// document and its hash, and the receipt says the logo is drawn
	// (agentry-cli#816).
	var logo bytes.Buffer
	if err := png.Encode(&logo, image.NewGray(image.Rect(0, 0, 4, 2))); err != nil {
		t.Fatal(err)
	}
	withLogo := bytes.Replace(modern, []byte("\n}\n"), []byte(",\n  \"logo\": {\n    \"png\": \""+base64.StdEncoding.EncodeToString(logo.Bytes())+"\"\n  }\n}\n"), 1)
	{
		p, f := fixture(t)
		happyFake(f)
		p.KeepRunDir = true
		p.ReportTheme = ReportTheme{Design: withLogo, From: ThemeFromAccount}
		env, err := Run(context.Background(), f, p)
		if err != nil {
			t.Fatalf("a design with a logo: run: %v", err)
		}
		b, err := os.ReadFile(filepath.Join(p.RunRoot, "runs", p.RunID, "cfg", "gateway", "toolspec.json"))
		if err != nil {
			t.Fatal(err)
		}
		var ts toolsrv.Spec
		if err := json.Unmarshal(b, &ts); err != nil {
			t.Fatal(err)
		}
		if ts.PDF.Theme == nil || ts.PDF.Theme.Document != string(withLogo) || !ts.PDF.Theme.HasLogo() {
			t.Errorf("the tools spec's theme with a logo = %+v", ts.PDF.Theme)
		}
		line := env.CapabilitiesEnforced["pdf.design"]
		if !strings.Contains(line, "(sha256:"+sha(withLogo)+"), with the person's logo in the title block, chosen by the account's report design") {
			t.Errorf("the receipt's pdf.design line with a logo = %q", line)
		}
	}

	p, f := fixture(t)
	happyFake(f)
	p.ReportTheme = ReportTheme{Design: bytes.Replace(modern, []byte(`"#1f6feb"`), []byte(`"#7fb3d5"`), 1), From: ThemeFromDesignFlag}
	if _, err := Run(context.Background(), f, p); err == nil || !strings.Contains(err.Error(), "accent") {
		t.Errorf("a design whose accent fails the contrast rule ran: %v", err)
	}
}
