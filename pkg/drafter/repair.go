package drafter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// The Drafter's own caps (plan builder-simplify.md §5.1, D8): tighter than
// the manifest's bounds, which stay as they are for a BriefAgent written by
// hand. AGENT.md states each by value (TestTheInstructionStatesItsCapsByValue)
// and the fixtures' shape check holds a draft to them (CheckShape).
const (
	MaxIntake        = 3
	MaxPrepare       = 5
	MaxOpenQuestions = 2
	MaxNoteBullets   = 5
)

// The repair turn's bounds: the most lines of the rules' report it is given,
// and the most characters of each.
const (
	maxReportLines = 40
	maxReportLine  = 400
)

// repairFraming opens the repair turn's task. It is a constant: the only
// text in the task besides it is the rules' report and the Drafter's own
// draft (RepairTask).
const repairFraming = "This is your one repair turn. The Agentry rules refused the draft you returned, and their report is below, " +
	"with your manifest and your instructions as you wrote them. You have no web tools this turn and nothing new to read: " +
	"fix the draft from what is here.\n\n" +
	"- Change what the report names, and only what a fix to it needs. Keep every choice it does not name.\n" +
	"- Your last message is the whole manifest again, corrected: `{\"manifest\": {…}, \"matches\": [], \"sources\": [], \"openQuestions\": []}`. " +
	"The lists you returned before are kept, so leave them empty.\n" +
	"- If the report names the instruction file (AGENT.md, or a rule S1, S3 or S4), write the whole file again with `write_report`, " +
	"`path` `AGENT.md`. Otherwise do not write it: the one you wrote before is kept. Do not write notes.md.\n"

// RepairTask is the repair turn's task (plan builder-simplify.md §5.2 step
// 3): the constant framing, the rules' own report on the draft, and the
// draft's manifest and instructions as the Drafter wrote them (after the
// post-step's corrections). Nothing else goes in: not the description, not
// the notes, the sources, the matches or AI Review's record, and no text the
// Drafter read. The turn runs without the web tools, so the report is the
// only thing in it the model did not write itself.
func RepairTask(d *Draft, report []string) (string, error) {
	if len(report) == 0 {
		return "", fmt.Errorf("a repair turn needs the rules' report")
	}
	var b strings.Builder
	b.WriteString(repairFraming)
	b.WriteString("\n## The rules' report\n\n")
	for _, line := range reportLines(report) {
		b.WriteString("- " + line + "\n")
	}
	var m bytes.Buffer
	if err := json.Indent(&m, d.Manifest, "", "  "); err != nil {
		return "", fmt.Errorf("the draft's manifest is not JSON: %w", err)
	}
	b.WriteString("\n## Your manifest\n\n")
	b.Write(m.Bytes())
	b.WriteString("\n\n## Your instructions (AGENT.md)\n\n")
	if strings.TrimSpace(d.Instructions) == "" {
		b.WriteString("(You wrote none. Write them this turn.)\n")
	} else {
		b.WriteString(d.Instructions)
		if !strings.HasSuffix(d.Instructions, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String(), nil
}

// reportLines is the report as the repair turn and the notes carry it: one
// line each, bounded, at most maxReportLines of them.
func reportLines(report []string) []string {
	var out []string
	for i, line := range report {
		if i == maxReportLines {
			out = append(out, fmt.Sprintf("… and %d more", len(report)-maxReportLines))
			break
		}
		line = strings.Join(strings.Fields(line), " ")
		if utf8.RuneCountInString(line) > maxReportLine {
			line = string([]rune(line)[:maxReportLine]) + ellipsis
		}
		out = append(out, line)
	}
	return out
}

// Repaired is the post-step over the repair turn's answer (§5.2 step 3):
// the first draft with the manifest the second answer returned and, when
// the turn wrote one, its instruction file. Everything else is the first
// answer's (the matches, the sources, the open questions, the notes, AI
// Review's record and what made it), since the repair turn read nothing.
// Its manifest is corrected as the first's was, and its notes say it was
// repaired and what the rules said of the first answer. outDir is the
// repair run's own output folder. The caller checks the result as it
// checked the first, and keeps it whether or not it passes; an answer that
// is not a draft is exit 17, and the caller keeps the first draft.
func Repaired(first *Draft, env *receipt.Envelope, outDir string, report []string) (*Draft, error) {
	ans, err := Parse(env.Result)
	if err != nil {
		return nil, engine.Classify(engine.ExitOutputSchema, err)
	}
	d := *first
	d.Manifest, d.Validation, d.ManifestError = ans.Manifest, nil, ""
	// Only the instruction file is read back: the turn was told the notes
	// are kept.
	again := Draft{Instructions: ans.Instructions, Notes: "-"}
	if err := FromReports(&again, filepath.Join(outDir, "reports")); err != nil {
		d.ManifestError = err.Error()
	}
	if strings.TrimSpace(again.Instructions) != "" {
		d.Instructions = again.Instructions
	}
	if strings.TrimSpace(d.Instructions) == "" && d.ManifestError == "" {
		d.ManifestError = "the Drafter wrote no instructions (AGENT.md)"
	}
	d.Manifest, d.corrections = Correct(d.Manifest)
	var s strings.Builder
	s.WriteString("## Repaired once\n\nThe rules refused the Drafter's first answer, so it was given their report, and nothing else, " +
		"for one more turn without its web tools. This draft is its second answer. What the rules said of the first:\n\n")
	for _, line := range reportLines(report) {
		s.WriteString("- " + line + "\n")
	}
	d.composeNotes(s.String())
	return &d, nil
}

// NoteNotRepaired says in the notes that the rules refused this draft and
// its repair turn returned no draft, with why in the CLI's own words.
func (d *Draft) NoteNotRepaired(why string) {
	d.composeNotes("## Not repaired\n\nThe rules refused this answer. The Drafter was given their report for one more turn, " +
		"and that turn returned no draft (" + why + "), so this is its first answer, with its problems.\n")
}

// Corrections is what the post-step corrected in the draft's manifest
// without the model (Correct), in the order it was done.
func (d *Draft) Corrections() []string { return d.corrections }

// composeNotes writes the notes: the Drafter's own, then the post-step's
// sections (what it corrected, then extra, then what AI Review withheld).
func (d *Draft) composeNotes(extra string) {
	d.Notes = d.ownNotes
	if len(d.corrections) > 0 {
		var b strings.Builder
		b.WriteString("## Corrected after drafting\n\nThe draft broke a rule in a way that needed no judgement, so it was corrected before the rules checked it:\n\n")
		for _, c := range d.corrections {
			b.WriteString("- " + c + "\n")
		}
		d.noteSection(b.String())
	}
	if extra != "" {
		d.noteSection(extra)
	}
	d.NoteWithheld()
}

// noteSection adds one section to the notes.
func (d *Draft) noteSection(s string) {
	notes := strings.TrimRight(d.Notes, "\n")
	if notes != "" {
		notes += "\n\n"
	}
	d.Notes = notes + s
}
