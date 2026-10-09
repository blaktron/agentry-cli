package drafter

import (
	"path/filepath"
	"regexp"
	"strings"
)

// namedFile is a file name as an instruction names one.
var namedFile = regexp.MustCompile(`[A-Za-z0-9_./-]+\.[A-Za-z0-9]{1,5}\b`)

// negated opens a sentence that tells the run not to do what follows.
var negated = regexp.MustCompile(`(?i)\b(never|not|don't|do not|no)\b`)

// printToPDF is an instruction handing the person a page to print to PDF
// themselves, which the run can now do with export_pdf.
var printToPDF = regexp.MustCompile(`(?i)\b(save|print)(s|ing)? (it |them |the [a-z]+ )?(as|to) (a )?pdf\b|\bprint-ready html\b`)

// pdfProblems is the Drafter's rule on PDFs (agentry-cli#787), one line
// each, in the rules' report's words. Every run has export_pdf, which turns
// a Markdown file into a PDF in reports/ in the person's report theme
// (DR-81), and write_report refuses a PDF, so a draft that has its run make
// a PDF any other way fails on every run while the run still reads
// succeeded: the operator's redraft of resume-reformat told its agent that
// it had "no program that makes PDFs" and to write one by hand, as ASCII,
// with write_report. The rule refuses instructions that have the run
// write_report a .pdf, or that leave the person to print a page to PDF. A
// PDF written by hand can reach the person only through write_report, so
// the first case covers it; PDF syntax alone is not refused, since a
// QuickAgent that examines PDF files has reason to name it. Nil when the
// instructions do neither.
func pdfProblems(instructions string) []string {
	const how = "have the run write the document in Markdown with write_report, then call export_pdf on that file, which makes the PDF in reports/ in the person's report theme (runtime/how-a-run-works.md)"
	var out []string
	for _, s := range instructionSentences(instructions) {
		if p := reportsAPDF(s); p != "" {
			out = append(out, "the instructions: they have the run write "+p+" with write_report, which refuses a PDF; "+how)
			break
		}
	}
	if !strings.Contains(instructions, "export_pdf") && printToPDF.MatchString(instructions) {
		out = append(out, "the instructions: they leave the person to print the result to PDF; "+how)
	}
	return out
}

// reportsAPDF is the .pdf a sentence has the run write with write_report:
// one that calls write_report, is not told not to, does not call
// export_pdf, and names only .pdf files after write_report. A sentence
// that names another file after it ("write_report the issues in
// contract.pdf as issues.md") writes that one.
func reportsAPDF(sentence string) string {
	before, after, ok := strings.Cut(sentence, "write_report")
	if !ok || strings.Contains(sentence, "export_pdf") || negated.MatchString(before) {
		return ""
	}
	names := namedFile.FindAllString(after, -1)
	for _, n := range names {
		if !strings.EqualFold(filepath.Ext(n), ".pdf") {
			return ""
		}
	}
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// instructionSentences splits instructions at line ends and at a full stop
// followed by a space, which is enough to keep a write_report and a .pdf
// apart when they are in different sentences.
func instructionSentences(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		out = append(out, strings.Split(line, ". ")...)
	}
	return out
}

// notePDFProblems says in the notes that the finished draft still has its
// run make a PDF in a way no run can, so the person fixes it before
// publishing.
func (d *Draft) notePDFProblems() {
	probs := pdfProblems(d.Instructions)
	if len(probs) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("## PDF output to fix before a run\n\nThis draft's instructions make a PDF in a way its runs cannot:\n\n")
	for _, p := range probs {
		b.WriteString("- " + p + "\n")
	}
	d.composeNotes(b.String())
}
