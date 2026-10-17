package toolsrv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/runnerimage/reportdesign"
)

// deliverableSpec is a run spec with the deliverables ledger.
func deliverableSpec(t *testing.T) *Spec {
	t.Helper()
	spec := baseSpec(t)
	spec.Deliverables = Deliverables{Ledger: filepath.Join(spec.OutDir, "receipts", "deliverables.ndjson")}
	return spec
}

func readDeliverables(t *testing.T, spec *Spec) []DeliverableEntry {
	t.Helper()
	b, err := os.ReadFile(spec.Deliverables.Ledger)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []DeliverableEntry
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var e DeliverableEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("ledger line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

func TestWriteReportMarksADeliverable(t *testing.T) {
	spec := deliverableSpec(t)
	s := newTestServer(t, spec)

	got, isErr := s.toolWriteReport(rawArgs(t, map[string]any{"path": "sub/summary.md", "content": "# S\n", "deliverable": true, "title": "  Weekly summary "}))
	if isErr || got != "report written: reports/sub/summary.md (4 bytes); marked as a deliverable: Weekly summary" {
		t.Fatalf("write_report = (%q, %v)", got, isErr)
	}
	got, isErr = s.toolWriteReport(rawArgs(t, map[string]any{"path": "notes.md", "content": "n"}))
	if isErr || got != "report written: reports/notes.md (1 bytes)" {
		t.Fatalf("an unmarked report = (%q, %v)", got, isErr)
	}
	got, isErr = s.toolWriteReport(rawArgs(t, map[string]any{"path": "plain.md", "content": "n", "deliverable": true}))
	if isErr || !strings.HasSuffix(got, "; marked as a deliverable") {
		t.Fatalf("a mark without a title = (%q, %v)", got, isErr)
	}
	// A bool sent as text is the value meant.
	if got, isErr := s.toolWriteReport(rawArgs(t, map[string]any{"path": "text.md", "content": "n", "deliverable": "true"})); isErr || !strings.Contains(got, "marked") {
		t.Fatalf("deliverable as text = (%q, %v)", got, isErr)
	}
	// Taking the mark off is recorded, and the last line for a path wins.
	if got, isErr := s.toolWriteReport(rawArgs(t, map[string]any{"path": "plain.md", "content": "n", "deliverable": false})); isErr || !strings.HasSuffix(got, "; no longer marked as a deliverable") {
		t.Fatalf("taking the mark off = (%q, %v)", got, isErr)
	}
	want := []DeliverableEntry{
		{Path: "reports/sub/summary.md", Title: "Weekly summary", Marked: true},
		{Path: "reports/plain.md", Marked: true},
		{Path: "reports/text.md", Marked: true},
		{Path: "reports/plain.md", Marked: false},
	}
	got2 := readDeliverables(t, spec)
	if len(got2) != len(want) {
		t.Fatalf("ledger = %+v, want %+v", got2, want)
	}
	for i := range want {
		if got2[i] != want[i] {
			t.Errorf("ledger line %d = %+v, want %+v", i, got2[i], want[i])
		}
	}
}

func TestADeliverableTitleIsOneLineOfPlainText(t *testing.T) {
	spec := deliverableSpec(t)
	s := newTestServer(t, spec)
	bidi := string(rune(0x202e)) // right-to-left override
	for name, args := range map[string]map[string]any{
		"a title without the mark": {"path": "a.md", "content": "x", "title": "Summary"},
		"the mark taken off":       {"path": "a.md", "content": "x", "deliverable": false, "title": "Summary"},
		"a line break":             {"path": "a.md", "content": "x", "deliverable": true, "title": "one\ntwo"},
		"a control character":      {"path": "a.md", "content": "x", "deliverable": true, "title": "a" + string(rune(7)) + "b"},
		"a bidirectional override": {"path": "a.md", "content": "x", "deliverable": true, "title": "txt" + bidi + "fdp.exe"},
		"a line separator":         {"path": "a.md", "content": "x", "deliverable": true, "title": "a" + string(rune(0x2028)) + "b"},
		"121 characters":           {"path": "a.md", "content": "x", "deliverable": true, "title": strings.Repeat("é", 121)},
	} {
		got, isErr := s.toolWriteReport(rawArgs(t, args))
		if !isErr || !strings.Contains(got, "title") {
			t.Errorf("%s = (%q, %v), want refused naming the title", name, got, isErr)
		}
	}
	if _, err := os.Stat(filepath.Join(spec.OutDir, "reports", "a.md")); err == nil {
		t.Error("a call refused for its title still wrote the report")
	}
	if got := readDeliverables(t, spec); len(got) != 0 {
		t.Errorf("a refused call marked %+v", got)
	}
	if got, isErr := s.toolWriteReport(rawArgs(t, map[string]any{"path": "ok.md", "content": "x", "deliverable": true, "title": strings.Repeat("é", 120)})); isErr {
		t.Errorf("a 120-character title = %q", got)
	}
}

func TestADeliverableNeedsALedgerToBeMarked(t *testing.T) {
	spec := baseSpec(t)
	s := newTestServer(t, spec)
	got, isErr := s.toolWriteReport(rawArgs(t, map[string]any{"path": "a.md", "content": "x", "deliverable": true}))
	if isErr || !strings.Contains(got, "not marked as a deliverable: this run records none") {
		t.Fatalf("write_report with no ledger = (%q, %v)", got, isErr)
	}
	if _, err := os.Stat(filepath.Join(spec.OutDir, "reports", "a.md")); err != nil {
		t.Errorf("the report is not written: %v", err)
	}
}

func TestTheDeliverablesLedgerIsUnderTheOutputFolder(t *testing.T) {
	spec := baseSpec(t)
	spec.Deliverables.Ledger = "/elsewhere/deliverables.ndjson"
	if err := validateSpec(spec); err == nil {
		t.Error("a deliverables ledger outside the output folder was accepted")
	}
	spec.Deliverables.Ledger = "receipts/deliverables.ndjson"
	if err := validateSpec(spec); err == nil {
		t.Error("a relative deliverables ledger was accepted")
	}
}

func TestBothToolsDescribeTheMark(t *testing.T) {
	for name, schema := range map[string]map[string]any{"write_report": writeReportSchema(), "export_pdf": exportPDFSchema()} {
		props := schema["properties"].(map[string]any)
		for _, k := range []string{"deliverable", "title"} {
			if _, ok := props[k]; !ok {
				t.Errorf("%s's schema has no %s", name, k)
			}
		}
	}
}

func TestExportPDFMarksADeliverable(t *testing.T) {
	spec := pdfSpec(t)
	spec.Deliverables = Deliverables{Ledger: filepath.Join(spec.OutDir, "receipts", "deliverables.ndjson")}
	s := pdfServer(t, spec, fakeTypst(t, fakePDF))
	mustWrite(t, filepath.Join(spec.OutDir, "reports", "summary.md"), []byte("# Summary\n"))

	got, isErr := exportPDF(t, s, map[string]any{"source": "reports/summary.md", "deliverable": true, "title": "Quarterly summary"})
	if isErr || !strings.HasSuffix(got, "; marked as a deliverable: Quarterly summary") {
		t.Fatalf("export_pdf = (%q, %v)", got, isErr)
	}
	if got := readDeliverables(t, spec); len(got) != 1 || got[0] != (DeliverableEntry{Path: "reports/summary.pdf", Title: "Quarterly summary", Marked: true}) {
		t.Errorf("ledger = %+v", got)
	}
	// A refused title renders nothing.
	before, _ := os.ReadDir(filepath.Join(spec.OutDir, "reports"))
	if got, isErr := exportPDF(t, s, map[string]any{"source": "reports/summary.md", "path": "other.pdf", "title": "x"}); !isErr || !strings.Contains(got, "title") {
		t.Errorf("a title without the mark = (%q, %v)", got, isErr)
	}
	after, _ := os.ReadDir(filepath.Join(spec.OutDir, "reports"))
	if len(before) != len(after) {
		t.Error("a refused call rendered a PDF")
	}
	// The design stays the person's: other arguments are still refused.
	if got, isErr := exportPDF(t, s, map[string]any{"source": "reports/summary.md", "theme": "dark"}); !isErr || !strings.Contains(got, "deliverable and title") {
		t.Errorf("an unknown argument = (%q, %v)", got, isErr)
	}
}

// TestATitleIsHeldToTheDesignsFreeTextRule: the rule is the report design's
// for its own free texts (its name), so a title that a design would refuse
// as a name, for what it holds rather than how long it is, is refused too.
func TestATitleIsHeldToTheDesignsFreeTextRule(t *testing.T) {
	doc, err := BuiltinTheme(DefaultTheme)
	if err != nil {
		t.Fatal(err)
	}
	base, err := reportdesign.Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"Plain words", "Müller & Söhne", " ", "a\nb", "a\tb", "a" + string(rune(7)), "a" + string(rune(0x202e)) + "b",
		"a" + string(rune(0x2066)) + "b", "a" + string(rune(0x2028)) + "b", "a" + string(rune(0x2029)) + "b", "a" + string(rune(0x200f)) + "b",
	} {
		d := base
		d.Name = s
		designOK := d.Validate() == nil
		titleOK := CheckDeliverableTitle(s) == nil
		if designOK != titleOK {
			t.Errorf("%q: the design accepts it as a name: %v; as a title: %v", s, designOK, titleOK)
		}
	}
}
