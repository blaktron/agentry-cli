package drafter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// TestPDFProblems is the Drafter's rule on PDFs: only export_pdf makes a
// PDF, so instructions that have the run write one with write_report,
// write PDF syntax by hand, or leave the person to print a page to PDF are
// refused, and the right way is not.
func TestPDFProblems(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"no PDF at all", "## Steps\n1. `write_report` the summary as `summary.md`.\n", nil},
		{"Markdown then export_pdf", "## Steps\n1. `write_report` the resume as `resume.md`.\n2. `export_pdf` with `source` `reports/resume.md`.\n", nil},
		{"one sentence naming both", "Write `resume.md` with write_report, then export_pdf it to `resume.pdf`.\n", nil},
		{"a PDF input read", "`read_input` the resume, a `.docx` or `resume.pdf`, then `write_report` the review as `review.md`.\n", nil},
		{"two sentences apart", "Read `cv.pdf` with read_input. Then write_report `review.md`.\n", nil},
		// resume-reformat 0.3.0, as the Drafter 1.11.0 wrote it.
		{"the operator's 0.3.0", "You have no program that makes PDFs, so you write the PDF yourself, as plain ASCII text.\n" +
			"9. Assemble the PDF text, compute the byte offsets and stream lengths, and `write_report` it as `resume.pdf`.\n" +
			"The file starts with the line `%PDF-1.4`. A `startxref` line follows the xref table.\n",
			[]string{"the instructions: they have the run write resume.pdf with write_report"}},
		// resume-reformat 0.2.0, written before export_pdf.
		{"the operator's 0.2.0", "Return a reformatted version as a print-ready HTML file the person saves as a PDF.\n8. `write_report` the reformatted resume as `resume.html`.\n",
			[]string{"the instructions: they leave the person to print the result to PDF"}},
		{"a .pdf input named before the report", "`write_report` a list of the issues found in `contract.pdf` as `issues.md`.\n", nil},
		{"told not to", "Never `write_report` a `resume.pdf`: call export_pdf.\n", nil},
		{"a QuickAgent that examines PDFs", "## Background\nA PDF starts with `%PDF-1.7`; each object ends with `endobj`, and `startxref` gives the table's offset. Report a file whose `xref` offsets are wrong.\n6. `write_report` the findings as `findings.md`.\n", nil},
		{"print to PDF", "Tell the person to print it to PDF from their browser.\n", []string{"the instructions: they leave the person to print"}},
		{"print to PDF beside export_pdf", "Call export_pdf; if it fails, the person can still print it to PDF.\n", nil},
	} {
		got := pdfProblems(tc.text)
		if len(got) != len(tc.want) {
			t.Errorf("%s: %q, want %d line(s)", tc.name, got, len(tc.want))
			continue
		}
		for i, w := range tc.want {
			if !strings.HasPrefix(got[i], w) || !strings.Contains(got[i], "export_pdf") {
				t.Errorf("%s: line %d = %q, want it to start %q and name export_pdf", tc.name, i, got[i], w)
			}
		}
	}
}

// TestARepairThatBreaksThePDFRuleKeepsTheFirst: a second answer that has
// the run write_report a PDF the first did not is not kept, as for the
// rule on exec.
func TestARepairThatBreaksThePDFRuleKeepsTheFirst(t *testing.T) {
	first := &Draft{Instructions: "1. `write_report` the resume as `resume.md`, then `export_pdf` it.\n"}
	second := &Draft{Instructions: "1. `write_report` the resume as `resume.pdf`.\n"}
	if !brokeARule(first, second) {
		t.Error("a second answer that breaks the PDF rule replaced a first that kept it")
	}
	if brokeARule(second, first) {
		t.Error("a second answer that mends the PDF rule was refused")
	}
}

// TestADraftThatWritesAPDFItselfGoesToItsRepairTurn: the rule joins the
// rules' report, so the first answer is sent to its repair turn with the
// line, and a second answer that still breaks it is named in the notes.
func TestADraftThatWritesAPDFItselfGoesToItsRepairTurn(t *testing.T) {
	out := t.TempDir()
	broken := strings.Replace(postDraft,
		`"instructions": "# Meeting actions\n\nRead the notes under /inputs and list the decisions and the actions.\n"`,
		`"instructions": "# Meeting actions\n\nRead the notes under /inputs. `+"`write_report`"+` the actions as `+"`actions.pdf`"+`.\n"`, 1)
	if broken == postDraft {
		t.Fatal("the fixture's instructions moved")
	}
	var tasks []string
	p := PostStep{
		OutDir: out, Description: "Summarize my notes as a PDF.", Task: "Summarize my notes as a PDF.",
		Scan: webscan.Result{Verdict: webscan.Pass}, Ref: Namespace + "/" + Name + "@" + Version, Checker: &postChecker{},
		Repair: func(_ context.Context, rt RepairTurn) (*receipt.Envelope, error) {
			tasks = append(tasks, rt.Task)
			// A second answer that still writes the PDF itself.
			return &receipt.Envelope{Status: "ok", Result: json.RawMessage(broken)}, nil
		},
	}
	d, _, err := p.Run(context.Background(), firstTurn(t, out, broken), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("%d repair turn(s)", len(tasks))
	}
	if _, report, _ := strings.Cut(tasks[0], "## The rules' report"); !strings.Contains(report, "- the instructions: they have the run write actions.pdf with write_report, which refuses a PDF") {
		t.Errorf("the rules' report does not name the PDF:\n%s", tasks[0])
	}
	if !strings.Contains(d.Notes, "## PDF output to fix before a run") || !strings.Contains(d.Notes, "export_pdf") {
		t.Errorf("the notes do not name what is left to fix:\n%s", d.Notes)
	}
}
