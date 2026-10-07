package drafter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// The repair turn's bounds: the most lines of the rules' report it is given,
// and the most characters of each; the notes carry fewer and shorter.
const (
	maxReportLines = 40
	maxReportLine  = 400
	maxNoteLines   = 10
	maxNoteLine    = 200
)

// RepairReport is what sends a draft to its one repair turn (plan
// builder-simplify.md §5.2 steps 3 and 4, as quickagents-that-run.md D6
// narrowed it): the rules' report on it, AI Review's check flagging it as
// not doing the task (Check.Repairable, off_task alone), or both. A draft has one repair turn whatever sent
// it. Task is the task the Drafter's first turn was given, which a turn
// with a flag is also given.
type RepairReport struct {
	Rules   []string
	Flagged []string
	Task    string
}

// Empty says there is nothing to repair.
func (r RepairReport) Empty() bool { return len(r.Rules) == 0 && len(r.Flagged) == 0 }

// Which of the reports a repair carries: the rules', the check's, or both.
const (
	byRules = iota
	byCheck
	byBoth
)

func (r RepairReport) by() int {
	switch {
	case len(r.Rules) > 0 && len(r.Flagged) > 0:
		return byBoth
	case len(r.Flagged) > 0:
		return byCheck
	}
	return byRules
}

// repairWords is what the repair turn and the notes say, by what sent the
// draft there, so it is worded in one place. Each is a constant: the only
// text in the turn's task besides the opening and the framing is the
// reports, the Drafter's own draft and, with a flag, the task it was given
// (RepairTask).
type repairWords struct {
	// opening is the task's first sentence; repaired opens the "Repaired
	// once" note; refused the "Not repaired" one; announce the line on
	// stderr (its verbs take the reports' lengths).
	opening, repaired, refused, announce string
}

var repairWordsOf = [...]repairWords{
	byRules: {
		opening: "This is your one repair turn. The Agentry rules refused the draft you returned, and their report is below, " +
			"with your manifest (or, when it was not a draft, your last message) and your instructions as you wrote them. ",
		repaired: "The rules refused the Drafter's first answer, so it was given their report and its own manifest " +
			"and instructions, and nothing else but its drafting guide, for one more turn without its web tools. This draft is its second answer. " +
			"What the rules said of the first:",
		refused:  "The rules refused this answer. The Drafter was given their report",
		announce: "The rules refused the draft (%[1]d problem(s)). The Drafter has one repair turn, given only their report and its own draft, without its web tools.",
	},
	byCheck: {
		opening: "This is your one repair turn. The draft you returned passed the Agentry rules, but AI Review's check of the finished draft " +
			"flagged it, and what it flagged is below, with the task you were given, your manifest and your instructions as you wrote them. ",
		repaired: "AI Review's check flagged the Drafter's first answer, so it was given what the check flagged, " +
			"its task and its own manifest and instructions, and nothing else but its drafting guide, for one more turn without its web tools. " +
			"This draft is its second answer. What the check said of the first:",
		refused:  "AI Review's check flagged this answer. The Drafter was given what it flagged",
		announce: "AI Review's check flagged the draft (%[2]d thing(s) it can fix). The Drafter has one repair turn, given what it flagged, its task and its own draft, without its web tools.",
	},
	byBoth: {
		opening: "This is your one repair turn. The Agentry rules refused the draft you returned, and AI Review's check of the finished draft " +
			"flagged it. The rules' report and what AI Review flagged are below, with the task you were given, your manifest and your instructions as you wrote them. ",
		repaired: "The rules refused the Drafter's first answer and AI Review's check flagged it, so it was given the rules' report, " +
			"what the check flagged, its task and its own manifest and instructions, and nothing else but its drafting guide, for one more turn without its web tools. " +
			"This draft is its second answer. What was said of the first:",
		refused:  "The rules refused this answer and AI Review's check flagged it. The Drafter was given both",
		announce: "The rules refused the draft (%d problem(s)) and AI Review's check flagged it (%d thing(s) it can fix). The Drafter has one repair turn, given both, its task and its own draft, without its web tools.",
	},
}

// RepairAnnouncement is the line a client prints as the repair turn starts.
func RepairAnnouncement(r RepairReport) string {
	return fmt.Sprintf(repairWordsOf[r.by()].announce, len(r.Rules), len(r.Flagged))
}

// repairFraming is the rest of the repair turn's framing, the same
// whatever sent the draft there.
const repairFraming = "You have no web tools this turn: fix the draft from what is here, " +
	"and from your drafting guide where a fix needs one of its pages (the bounds are in manifest/manifest.md).\n\n" +
	"- Change what the report names, and only what a fix to it needs. Keep every choice it does not name.\n" +
	"- Never remove a declared MCP server, egress host, command or secret, or a step of the instructions, that the report does not name: " +
	"the task needs them, and an answer that drops one is not kept. A report about the wording of the instructions is fixed by rewording it.\n" +
	"- Your last message is the whole manifest again, corrected: `{\"manifest\": {…}, \"matches\": [], \"sources\": [], \"openQuestions\": []}`. " +
	"The lists you returned before are kept, so leave them empty.\n" +
	"- If the report names the instruction file (AGENT.md, the instructions, or a rule S1, S3 or S4), write the whole file again with `write_report`, " +
	"`path` `AGENT.md`. Otherwise do not write it: the one you wrote before is kept. Do not write notes.md.\n"

// RepairTask is the repair turn's task (plan builder-simplify.md §5.2 step
// 3): the constant framing, the rules' own report on the draft, and the
// draft's manifest and instructions as the Drafter wrote them (after the
// post-step's corrections), or, for an answer the output schema refused,
// its last message as it wrote it. Nothing else goes in: not the description, not
// the notes, the sources, the matches or AI Review's record, and no text the
// Drafter read. The turn runs without the web tools, so the report is the
// only thing in it the model did not write itself, beside the drafting
// guide the Drafter ships (agentry-cli#657), which the framing points it
// to for the page a fix needs (agentry-cli#659).
//
// A repair with a flag of AI Review's check is also given r.Task, the task
// the Drafter's first turn was given, which AI Review passed before that
// turn started: a draft flagged as not doing the task cannot be fixed
// without it. It is the one text in the turn the Drafter was given before,
// and nothing it read. What the check flagged is the CLI's own words
// around the question's id and score and the package's own paths.
func RepairTask(d *Draft, r RepairReport) (string, error) {
	switch {
	case r.Empty():
		return "", fmt.Errorf("a repair turn needs a report")
	case len(r.Flagged) > 0 && strings.TrimSpace(r.Task) == "":
		return "", fmt.Errorf("a repair after the check needs the task")
	}
	var b strings.Builder
	b.WriteString(repairWordsOf[r.by()].opening + repairFraming)
	section := func(heading string, lines []string) {
		if len(lines) == 0 {
			return
		}
		b.WriteString("\n" + heading + "\n\n")
		for _, line := range reportLines(lines, maxReportLines, maxReportLine) {
			b.WriteString("- " + line + "\n")
		}
	}
	section("## The rules' report", r.Rules)
	section("## What AI Review flagged", r.Flagged)
	if len(r.Flagged) > 0 {
		b.WriteString("\n## The task you were given\n\n")
		b.WriteString(strings.TrimRight(r.Task, "\n") + "\n")
	}
	if len(d.Manifest) == 0 {
		// An answer the output schema refused (Unparsed): its last message
		// as it wrote it, for it to send again as one JSON object.
		b.WriteString("\n## Your last message\n\n")
		b.WriteString(d.lastMessage)
	} else {
		var m bytes.Buffer
		if err := json.Indent(&m, d.Manifest, "", "  "); err != nil {
			return "", fmt.Errorf("the draft's manifest is not JSON: %w", err)
		}
		b.WriteString("\n## Your manifest\n\n")
		b.Write(m.Bytes())
	}
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

// reportLines is the report as the repair turn or the notes carry it: one
// line each, at most maxChars characters, at most maxLines of them.
func reportLines(report []string, maxLines, maxChars int) []string {
	var out []string
	for i, line := range report {
		if i == maxLines {
			out = append(out, fmt.Sprintf("… and %d more", len(report)-maxLines))
			break
		}
		line = strings.Join(strings.Fields(line), " ")
		if utf8.RuneCountInString(line) > maxChars {
			line = string([]rune(line)[:maxChars]) + ellipsis
		}
		out = append(out, line)
	}
	return out
}

// maxLastMessage is the most of an unparsed last message a repair turn is
// given back: the whole of any draft's, which is a manifest and three lists.
const maxLastMessage = 32 << 10

// Unparsed is the draft of an answer the output schema refused (exit 17,
// why): the Drafter's last message was not one JSON object with a
// manifest. It has no manifest and says why in ManifestError, and it keeps
// what Finish would (its reports, AI Review's record, what made it) and the
// last message, bounded, for its repair turn (RepairTask), the one thing
// the turn is given back besides the error. It is never rendered or
// written as a draft unless that turn returns one.
func Unparsed(env *receipt.Envelope, outDir string, desc webscan.Result, ref, build string, why error) *Draft {
	d := &Draft{Matches: []Match{}, Sources: []Source{}, OpenQuestions: []string{},
		ManifestError: "the Drafter's last message was not a draft: " + why.Error()}
	d.Drafter = made(env, ref, build)
	d.AIReview = reviewOf(env.Web, desc, filepath.Join(outDir, "receipts", engine.WebLedgerFile))
	_ = FromReports(d, filepath.Join(outDir, "reports"))
	var last string
	if json.Unmarshal(env.Result, &last) != nil {
		last = string(env.Result)
	}
	last = strings.TrimSpace(last)
	if len(last) > maxLastMessage {
		last = strings.ToValidUTF8(last[:maxLastMessage], "") + "\n(… cut here)"
	}
	d.lastMessage = last
	d.ownNotes = d.Notes
	d.composeNotes("")
	return d
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
func Repaired(first *Draft, env *receipt.Envelope, outDir string, r RepairReport) (*Draft, error) {
	ans, err := Parse(env.Result)
	if err != nil {
		return nil, engine.Classify(engine.ExitOutputSchema, err)
	}
	d := *first
	d.extras = slices.Clone(first.extras)
	d.Manifest, d.Validation, d.ManifestError, d.repaired = ans.Manifest, nil, "", true
	// Only the instruction file is read back: the turn was told the notes
	// are kept.
	inst := ans.Instructions
	if strings.TrimSpace(inst) == "" {
		inst, err = readReport(filepath.Join(outDir, "reports"), instructionsReport, maxInstructions)
		if err != nil {
			d.ManifestError = err.Error()
		}
	}
	if strings.TrimSpace(inst) != "" {
		d.Instructions = inst
	}
	if strings.TrimSpace(d.Instructions) == "" && d.ManifestError == "" {
		d.ManifestError = "the Drafter wrote no instructions (AGENT.md)"
	}
	d.Manifest, d.corrections = Correct(d.Manifest)
	var s strings.Builder
	s.WriteString("## Repaired once\n\n" + repairWordsOf[r.by()].repaired + "\n\n")
	for _, line := range reportLines(append(slices.Clone(r.Rules), r.Flagged...), maxNoteLines, maxNoteLine) {
		s.WriteString("- " + line + "\n")
	}
	d.composeNotes(s.String())
	return &d, nil
}

// NoteNotRepaired says in the notes that the rules refused this draft and
// its repair turn returned no draft, with why in the CLI's own words.
func (d *Draft) NoteNotRepaired(r RepairReport, why string) {
	d.repairFailure = why
	d.composeNotes("## Not repaired\n\n" + repairWordsOf[r.by()].refused + " for one more turn, " +
		"and that turn returned no draft (" + why + "), so this is its first answer, with its problems.\n")
}

// NoteSecondAnswerRefused says in the notes that the first answer is kept,
// because the second broke a rule the first passed (keepFirst), r being
// what sent the first to its repair turn. A repair does not trade a draft
// the rules pass for one they refuse, nor one within the instruction cap
// for one past it.
func (d *Draft) NoteSecondAnswerRefused(r RepairReport) {
	d.composeNotes("## Not repaired\n\n" + repairWordsOf[r.by()].refused + " for one more turn. " +
		"Its second answer broke rules this one passes, so this is its first answer, with what the check said of it.\n")
}

// maxDroppedNoted is the most of what a second answer dropped that its
// note names.
const maxDroppedNoted = 10

// NoteSecondAnswerDropped says in the notes that the first answer is kept,
// because the second left out declared capabilities the report did not
// name (keepFirst), r being what sent the first to its repair turn. A repair
// does not clear a report by stripping what the task needs
// (agentry-cli#633).
func (d *Draft) NoteSecondAnswerDropped(r RepairReport, dropped []string) {
	var b strings.Builder
	b.WriteString("## Not repaired\n\n" + repairWordsOf[r.by()].refused + " for one more turn. " +
		"Its second answer left out what this one declares and the report did not name, which the task needs, " +
		"so this is its first answer, with what was said of it. It left out:\n\n")
	for _, line := range reportLines(dropped, maxDroppedNoted, maxNoteLine) {
		b.WriteString("- " + line + "\n")
	}
	d.composeNotes(b.String())
}

// Corrections is what the post-step corrected in the draft's manifest
// without the model (Correct), in the order it was done.
func (d *Draft) Corrections() []string { return d.corrections }

// IsRepaired says the draft is the Drafter's second answer, after its
// repair turn (Repaired).
func (d *Draft) IsRepaired() bool { return d.repaired }

// composeNotes writes the notes: the Drafter's own, then the post-step's
// sections (what it corrected, then each extra added so far and extra, in
// that order, then what AI Review withheld).
// They stay within the bound the clients read them at (the output schema's,
// maxNotes): the Drafter's own words give way to the sections, which say
// what was done to the draft.
func (d *Draft) composeNotes(extra string) {
	sections := &Draft{AIReview: d.AIReview}
	if len(d.corrections) > 0 {
		var b strings.Builder
		b.WriteString("## Corrected after drafting\n\nThe draft broke a rule in a way that needed no judgement, so it was corrected before the rules checked it:\n\n")
		for _, c := range d.corrections {
			b.WriteString("- " + c + "\n")
		}
		sections.noteSection(b.String())
	}
	if extra != "" {
		d.extras = append(d.extras, extra)
	}
	for _, e := range d.extras {
		sections.noteSection(e)
	}
	sections.NoteWithheld()
	own := d.ownNotes
	d.Notes = own
	d.noteSection(sections.Notes)
	if over := len(d.Notes) - maxNotes; over > 0 {
		if keep := len(own) - over; keep > len(ellipsis) {
			own = shortenAtWord(own, func(s string) bool { return len(s) <= keep })
		} else {
			own = ""
		}
		d.Notes = own
		d.noteSection(sections.Notes)
	}
}

// noteSection adds one section to the notes.
func (d *Draft) noteSection(s string) {
	if s == "" {
		return
	}
	notes := strings.TrimRight(d.Notes, "\n")
	if notes != "" {
		notes += "\n\n"
	}
	d.Notes = notes + s
}
