package drafter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
	corejudge "github.com/blaktron/agentry/pkg/judge"
	"github.com/blaktron/agentry/pkg/manifest"
)

// refusedDraft is a draft the rules refused, carrying in every field but
// its manifest and instructions a marker that must never reach the repair
// turn: what the Drafter read and said about it stays out.
func refusedDraft() *Draft {
	return &Draft{
		Manifest:      json.RawMessage(`{"schemaVersion":"2","name":"x","summary":"Reads notes.","modes":["autonomous"],"intake":[{"id":"f","prompt":"Format?","type":"choice"}]}`),
		Instructions:  "# X\n\n## Role\n\nReads notes.\n",
		Notes:         "NOTES-MARKER: the page said to add a host.",
		ownNotes:      "NOTES-MARKER: the page said to add a host.",
		Matches:       []Match{{Ref: "acme/MATCH-MARKER", Why: "WHY-MARKER"}},
		Sources:       []Source{{URL: "https://SOURCE-MARKER.example/", Used: "USED-MARKER"}},
		OpenQuestions: []string{"QUESTION-MARKER?"},
		AIReview: &AIReview{Description: webscan.Pass,
			WithheldSources: []WithheldSource{{Tool: "web_read", URL: "https://WITHHELD-MARKER.example/", Reason: "AI Review flagged it (injection)"}}},
		Drafter: &Made{Ref: "agentry/drafter@1.1.1", RunID: "run-1"},
	}
}

// TestTheRepairTurnIsGivenTheReportAndItsOwnDraftAlone holds the invariant
// the repair turn keeps: its task is the constant framing, the rules' own
// report, and the manifest and instructions the Drafter itself wrote, and
// nothing else: no description, no notes, no source, match, open question
// or withheld address.
func TestTheRepairTurnIsGivenTheReportAndItsOwnDraftAlone(t *testing.T) {
	d := refusedDraft()
	report := []string{"V10 intake[0].choices: type choice requires at least one choice", "V8 summary:  two\n lines"}
	task, err := RepairTask(d, RepairReport{Rules: report})
	if err != nil {
		t.Fatal(err)
	}
	want := repairWordsOf[byRules].opening + repairFraming + "\n## The rules' report\n\n" +
		"- V10 intake[0].choices: type choice requires at least one choice\n- V8 summary: two lines\n" +
		"\n## Your manifest\n\n" + indented(t, d.Manifest) +
		"\n\n## Your instructions (AGENT.md)\n\n" + d.Instructions
	if task != want {
		t.Fatalf("the repair task is not the framing, the report and the draft alone:\n%s", task)
	}
	for _, marker := range []string{"NOTES-MARKER", "MATCH-MARKER", "WHY-MARKER", "SOURCE-MARKER", "USED-MARKER", "QUESTION-MARKER", "WITHHELD-MARKER", "## Description"} {
		if strings.Contains(task, marker) {
			t.Errorf("the repair task carries %s", marker)
		}
	}
	if _, err := RepairTask(d, RepairReport{}); err == nil {
		t.Error("a repair turn with no report")
	}
	var many []string
	for i := range 50 {
		many = append(many, fmt.Sprintf("V%d %s", i, strings.Repeat("y", 900)))
	}
	task, _ = RepairTask(d, RepairReport{Rules: many})
	if strings.Count(task, "\n- V") != maxReportLines || !strings.Contains(task, "- … and 10 more\n") || strings.Contains(task, strings.Repeat("y", maxReportLine+1)) {
		t.Error("the report is not bounded")
	}
}

// TestARepairAfterTheCheckIsGivenWhatItFlaggedAndTheTask: a repair the
// check sent is given its own constant opening, what the check flagged in
// the CLI's words, the task the Drafter's first turn was given (which AI
// Review passed before that turn), and the draft the Drafter wrote; still
// no note, source, match, open question or withheld address.
func TestARepairAfterTheCheckIsGivenWhatItFlaggedAndTheTask(t *testing.T) {
	d := refusedDraft()
	report := []string{"AI Review off_task, score 3: an agent that follows the instructions would not do the task the description asks for."}
	task, err := RepairTask(d, RepairReport{Flagged: report, Task: "Turn meeting notes into action items.\n"})
	if err != nil {
		t.Fatal(err)
	}
	want := repairWordsOf[byCheck].opening + repairFraming + "\n## What AI Review flagged\n\n- " + report[0] + "\n" +
		"\n## The task you were given\n\nTurn meeting notes into action items.\n" +
		"\n## Your manifest\n\n" + indented(t, d.Manifest) +
		"\n\n## Your instructions (AGENT.md)\n\n" + d.Instructions
	if task != want {
		t.Fatalf("the repair task after the check:\n%s", task)
	}
	for _, marker := range []string{"NOTES-MARKER", "MATCH-MARKER", "WHY-MARKER", "SOURCE-MARKER", "USED-MARKER", "QUESTION-MARKER", "WITHHELD-MARKER"} {
		if strings.Contains(task, marker) {
			t.Errorf("the repair task carries %s", marker)
		}
	}
	if _, err := RepairTask(d, RepairReport{Flagged: report, Task: " "}); err == nil {
		t.Error("a repair after the check without the task")
	}

	// Both: the rules' report, then what the check flagged, then the task.
	rules := []string{"V10 intake[0].choices: type choice requires at least one choice"}
	task, err = RepairTask(d, RepairReport{Rules: rules, Flagged: report, Task: "Turn meeting notes into action items."})
	if err != nil {
		t.Fatal(err)
	}
	want = repairWordsOf[byBoth].opening + repairFraming + "\n## The rules' report\n\n- " + rules[0] + "\n" +
		"\n## What AI Review flagged\n\n- " + report[0] + "\n" +
		"\n## The task you were given\n\nTurn meeting notes into action items.\n" +
		"\n## Your manifest\n\n" + indented(t, d.Manifest) +
		"\n\n## Your instructions (AGENT.md)\n\n" + d.Instructions
	if task != want {
		t.Fatalf("the repair task with both reports:\n%s", task)
	}
}

// TestRecordCheck: the check goes into aiReview.check with how many were
// asked; a pass adds nothing to the notes, a flag says what it flagged, that
// the rubric's findings are advisory, and what each does at publish by its
// severity, a check with no answer says why; and the notes keep every
// section the post-step added, the repair's among them.
func TestRecordCheck(t *testing.T) {
	pass := &Check{Rubric: &CheckRubric{Verdict: CheckPass, Findings: []CheckFinding{}}, Draft: &CheckDraft{Verdict: CheckPass, Flagged: []string{}}}
	d := refusedDraft()
	d.RecordCheck(pass, 1, nil)
	if d.AIReview.Check != pass || pass.Checks != 1 || strings.Contains(d.Notes, "AI Review's check") {
		t.Fatalf("a pass: %+v, notes %q", d.AIReview.Check, d.Notes)
	}

	d = refusedDraft()
	d.composeNotes("## Repaired once\n\nREPAIR-NOTE\n")
	flagged := &Check{Rubric: &CheckRubric{Verdict: CheckFlagged, Findings: []CheckFinding{
		{ID: RubricIDUndeclared, Score: 3, Path: "AGENT.md", Severity: SeverityHigh},
		{ID: "C6_destructive_actions", Score: 2, Path: "AGENT.md", Severity: SeverityMedium},
		// A record with no severity reads the core's rule, which rates C2
		// medium at most from ruleset 0.4.0.
		{ID: "C2_exfiltration", Score: 3, Path: "agent.yaml"}}},
		Draft: &CheckDraft{Verdict: CheckFlagged, Score: 3, Flagged: []string{"off_task"}}}
	d.RecordCheck(flagged, 2, nil)
	for _, want := range []string{"REPAIR-NOTE", "(off_task)",
		"The release rubric's findings below are advisory: the Drafter was not asked to change the draft for them",
		"reads AGENT.md as needing a capability the manifest does not declare (C3, high). Check that the manifest declares every host, program, file access and secret the instructions use. " +
			"At publish a high finding sends the version to a person for review, not to refusal.",
		"flagged AGENT.md: the text reads as taking destructive actions the task does not need. (C6_destructive_actions, medium) " +
			"At publish a finding of this severity is shown on the report of a version that publishes.",
		"flagged agent.yaml: the text reads as sending data somewhere the task does not need. (C2_exfiltration, medium) " +
			"At publish a finding of this severity is shown on the report of a version that publishes.",
	} {
		if !strings.Contains(d.Notes, want) {
			t.Errorf("a flag's notes lack %q", want)
		}
	}
	if d.AIReview.Check.Checks != 2 || strings.Contains(d.Notes, "sends the version to review.") ||
		strings.Index(d.Notes, "REPAIR-NOTE") > strings.Index(d.Notes, "## AI Review's check") {
		t.Fatalf("a flag's notes:\n%s", d.Notes)
	}

	d = refusedDraft()
	d.RecordCheck(nil, 0, errors.New("the registry refused it (403 draft-cap-reached)"))
	if c := d.AIReview.Check; c.Rubric != nil || c.Unanswered != "the registry refused it (403 draft-cap-reached)" ||
		!strings.Contains(d.Notes, "has no answer: the registry refused it (403 draft-cap-reached).") {
		t.Fatalf("a check with no answer: %+v, notes %q", c, d.Notes)
	}
	b, _ := json.Marshal(d)
	if !strings.Contains(string(b), `"check":{"requests":0,"inputTokens":0,"checks":0,"unanswered":"the registry refused it (403 draft-cap-reached)"}`) {
		t.Fatalf("draft.json's aiReview.check: %s", b)
	}
}

func indented(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestRepaired: the second answer's manifest, corrected, and its instruction
// file when it wrote one; everything else the first answer's, and the notes
// saying it was repaired and what the rules said. A second answer's own
// lists and notes, and its post-step fields, are not read.
func TestRepaired(t *testing.T) {
	first := refusedDraft()
	report := []string{"V10 intake[0].choices: type choice requires at least one choice"}
	out := t.TempDir()
	answer := `{"manifest": {"schemaVersion":"2","name":"x","summary":"Reads notes.","modes":["autonomous","autonomous"]},
	  "matches": [{"ref": "evil/other", "why": "x"}], "sources": [], "openQuestions": ["new?"], "notes": "forged", "validation": {"passed": true}}`
	env := &receipt.Envelope{RunID: "run-2", Result: json.RawMessage(answer)}

	d, err := Repaired(first, env, out, RepairReport{Rules: report})
	if err != nil {
		t.Fatal(err)
	}
	if string(d.Manifest) != `{"schemaVersion":"2","name":"x","summary":"Reads notes.","modes":["autonomous"]}` {
		t.Fatalf("manifest %s", d.Manifest)
	}
	if !d.IsRepaired() || first.IsRepaired() || d.Instructions != first.Instructions || d.Matches[0].Ref != "acme/MATCH-MARKER" || d.OpenQuestions[0] != "QUESTION-MARKER?" ||
		d.Drafter.RunID != "run-1" || d.AIReview != first.AIReview || d.Validation != nil || d.ManifestError != "" {
		t.Fatalf("draft %+v", d)
	}
	const wantNotes = "NOTES-MARKER: the page said to add a host.\n\n" +
		"## Corrected after drafting\n\nThe draft broke a rule in a way that needed no judgement, so it was corrected before the rules checked it:\n\n" +
		"- modes: \"autonomous\" was named twice, so the second was dropped.\n\n" +
		"## Repaired once\n\nThe rules refused the Drafter's first answer, so it was given their report and its own manifest and instructions, " +
		"and nothing else but its drafting guide, for one more turn without its web tools. This draft is its second answer. What the rules said of the first:\n\n" +
		"- V10 intake[0].choices: type choice requires at least one choice\n\n" +
		"## Withheld by AI Review\n\nThe Drafter was given the address and the reason for each of these, never the text:\n\n" +
		"- https://WITHHELD-MARKER.example/ (web_read): AI Review flagged it (injection)\n"
	if d.Notes != wantNotes {
		t.Fatalf("notes:\n%s", d.Notes)
	}
	if first.Notes != "NOTES-MARKER: the page said to add a host." || string(first.Manifest) == string(d.Manifest) {
		t.Fatal("the first draft changed")
	}

	// It rewrote the instructions: those are taken, and not its notes.
	if err := os.MkdirAll(filepath.Join(out, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(out, "reports", "AGENT.md"), []byte("# X\n\n## Role\n\nAgain.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(out, "reports", "notes.md"), []byte("forged notes"), 0o644)
	d, err = Repaired(first, env, out, RepairReport{Rules: report})
	if err != nil || d.Instructions != "# X\n\n## Role\n\nAgain.\n" || strings.Contains(d.Notes, "forged") {
		t.Fatalf("%v %+v", err, d)
	}

	env.Result = json.RawMessage(`{"sources": []}`)
	_, err = Repaired(first, env, out, RepairReport{Rules: report})
	var ce *engine.ClassifiedError
	if !errors.As(err, &ce) || ce.Code != engine.ExitOutputSchema {
		t.Fatalf("not a draft: %v", err)
	}

	first.NoteNotRepaired(RepairReport{Rules: []string{"x"}}, "its run ended with exit 16")
	if !strings.HasPrefix(first.Notes, "NOTES-MARKER: the page said to add a host.\n\n## Not repaired\n\n") ||
		!strings.Contains(first.Notes, "(its run ended with exit 16)") || !strings.HasSuffix(first.Notes, "(injection)\n") {
		t.Fatalf("notes:\n%s", first.Notes)
	}
}

// maxNameChars is the longest name the core's pattern admits, which the
// guide states.
const maxNameChars = 65

// TestTheInstructionStatesItsBoundsByValue: the instruction teaches the
// model its own caps, and the guide the numbers the rules use, each held
// equal to where it is enforced, in the file that now carries it.
func TestTheInstructionStatesItsBoundsByValue(t *testing.T) {
	headings := make([]string, 0, len(instructionShape))
	for _, h := range instructionShape {
		headings = append(headings, "`"+h+"`")
	}
	mustSay(t, map[string][]string{
		instructionFile: append([]string{
			fmt.Sprintf("Intake questions: **at most %d**", MaxIntake),
			fmt.Sprintf("`prepare`: **at most %d** short items", MaxPrepare),
			fmt.Sprintf("`openQuestions`: **at most %d**", MaxOpenQuestions),
			fmt.Sprintf("The notes: **at most %d short bullets**", MaxNoteBullets),
			fmt.Sprintf("at most %d short Markdown bullets", MaxNoteBullets),
			fmt.Sprintf("`openQuestions`: at most %d (above)", MaxOpenQuestions),
			fmt.Sprintf("**at most %s bytes**", thousands(MaxInstructionBytes)),
			fmt.Sprintf("When the report names an instruction file past %s bytes", thousands(MaxInstructionBytes)),
		}, headings...),
		"manifest/manifest.md": {
			fmt.Sprintf("`summary`: one line of at most %d characters", manifest.MaxSummaryChars),
			fmt.Sprintf("`tags`: at most %d, each named once", manifest.MaxTags),
			fmt.Sprintf("`guide.overview`: at most %s bytes", thousands(manifest.MaxGuideOverviewBytes)),
			fmt.Sprintf("`guide.prepare`: each item one line of at most %d characters", manifest.MaxGuidePrepareChars),
			fmt.Sprintf("`guide.files`: one line of at most %d characters", manifest.MaxGuideFilesChars),
			fmt.Sprintf("`guide.task`: one line of at most %d characters", manifest.MaxGuideTaskChars),
			fmt.Sprintf("`guide.connect`: each key's help at most %s characters", thousands(manifest.MaxGuideConnectChars)),
			fmt.Sprintf("`interactive.opening`: at most %d characters", manifest.MaxOpeningChars),
			fmt.Sprintf("`capabilities.mcpServers`: at most %d servers", manifest.MaxMCPServers),
			fmt.Sprintf("`knowledge.accepts.prompt`: one line of at most %d characters", manifest.MaxKnowledgePromptChars),
			fmt.Sprintf("at most %d characters, starting and ending with a letter or a digit", maxNameChars),
		},
		"manifest/intake-and-guide.md": {fmt.Sprintf("`prepare` (at most %d things to gather first)", MaxPrepare)},
		// Where another page repeats a bound, it is held there too.
		"manifest/knowledge-accepts.md": {fmt.Sprintf("one plain line of at most %d characters", manifest.MaxKnowledgePromptChars)},
		"manifest/run-modes.md":         {fmt.Sprintf("at most %d characters", manifest.MaxOpeningChars)},
		"draft/instructions.md":         append([]string{fmt.Sprintf("**at most %s bytes**", thousands(MaxInstructionBytes))}, headings...),
	})
	// The name's bound is the core's name pattern.
	if !manifest.ValidName(strings.Repeat("a", maxNameChars)) || manifest.ValidName(strings.Repeat("a", maxNameChars+1)) {
		t.Errorf("a name of %d characters is not the core's bound", maxNameChars)
	}
	// The caps are the Drafter's, inside the rules' bounds.
	if MaxIntake > manifest.MaxIntakeQuestions || MaxPrepare > manifest.MaxGuidePrepare {
		t.Error("a cap is looser than the rule it sits inside")
	}
	// The instruction's cap is the draft check's instruction bound, and
	// within the report's own read bound, so an over-cap one is read and
	// repaired rather than lost.
	if MaxInstructionBytes != corejudge.DraftMaxInstruction || MaxInstructionBytes >= maxInstructions {
		t.Errorf("the instruction cap %d, the check's bound %d, the report bound %d", MaxInstructionBytes, corejudge.DraftMaxInstruction, maxInstructions)
	}
}

// TestTheNotesStayWithinTheirBound: the Drafter's own notes give way to the
// post-step's sections so the whole stays within the bound the clients read
// (the site's importer refuses notes past it), and sections past the bound
// on their own still end.
func TestTheNotesStayWithinTheirBound(t *testing.T) {
	d := refusedDraft()
	d.ownNotes = strings.Repeat("A long note. ", maxNotes/13+1)
	d.corrections = []string{"summary: shortened."}
	d.composeNotes("## Repaired once\n\n- V8 summary\n")
	if len(d.Notes) > maxNotes || !strings.HasPrefix(d.Notes, "A long note.") || !strings.Contains(d.Notes, "…\n\n## Corrected after drafting") ||
		!strings.Contains(d.Notes, "## Repaired once") || !strings.HasSuffix(d.Notes, "(injection)\n") {
		t.Fatalf("%d bytes, ending %q", len(d.Notes), d.Notes[max(0, len(d.Notes)-300):])
	}
	d.ownNotes = "Short."
	d.composeNotes(strings.Repeat("x", maxNotes+10))
	if strings.HasPrefix(d.Notes, "Short.") {
		t.Fatal("own notes kept past the bound")
	}
}

// TestAnAnswerThatIsNotADraftIsRepairedFromItsOwnLastMessage: an answer the
// output schema refused keeps its reports and AI Review's record, and its
// repair turn is given the error and its own last message, nothing else.
func TestAnAnswerThatIsNotADraftIsRepairedFromItsOwnLastMessage(t *testing.T) {
	out := t.TempDir()
	if err := os.MkdirAll(filepath.Join(out, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(out, "reports", "AGENT.md"), []byte("# X\n\n## Role\n\nReads.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(out, "reports", "notes.md"), []byte("NOTES-MARKER\n"), 0o644)
	last := `{"manifest": {"name": "x"}} Wait, resending. {"manifest": {"name": "x"}}`
	env := &receipt.Envelope{RunID: "run-1", Result: receipt.TextResult("\n\n" + last), Harness: receipt.Harness{Model: "m"}}
	d := Unparsed(env, out, webscan.Result{Verdict: webscan.Pass}, "agentry/drafter@1.1.1", "b", errors.New("the final message is not valid JSON"))
	if len(d.Manifest) != 0 || d.ManifestError != "the Drafter's last message was not a draft: the final message is not valid JSON" ||
		d.Instructions != "# X\n\n## Role\n\nReads.\n" || d.Notes != "NOTES-MARKER\n" || d.Drafter.RunID != "run-1" || d.AIReview == nil {
		t.Fatalf("%+v", d)
	}
	task, err := RepairTask(d, RepairReport{Rules: []string{"the draft: " + d.ManifestError}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(task, "\n## Your last message\n\n"+last+"\n\n## Your instructions (AGENT.md)\n\n# X") ||
		strings.Contains(task, "NOTES-MARKER") || strings.Contains(task, "## Your manifest") {
		t.Fatalf("task:\n%s", task)
	}
	env.Result = receipt.TextResult(strings.Repeat("y", maxLastMessage+10))
	if d := Unparsed(env, out, webscan.Result{}, "", "", errors.New("x")); len(d.lastMessage) > maxLastMessage+20 {
		t.Fatalf("the last message is not bounded: %d", len(d.lastMessage))
	}
}
