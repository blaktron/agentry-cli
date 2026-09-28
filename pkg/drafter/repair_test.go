package drafter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
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
		Drafter: &Made{Ref: "agentry/drafter@1.0.0", RunID: "run-1"},
	}
}

// TestTheRepairTurnIsGivenTheReportAndItsOwnDraftAlone holds the invariant
// the repair turn keeps (plan builder-simplify.md §3, §5.2 step 3): its task
// is the constant framing, the rules' own report, and the manifest and
// instructions the Drafter itself wrote, and nothing else: no description,
// no notes, no source, match, open question or withheld address.
func TestTheRepairTurnIsGivenTheReportAndItsOwnDraftAlone(t *testing.T) {
	d := refusedDraft()
	report := []string{"V10 intake[0].choices: type choice requires at least one choice", "V8 summary:  two\n lines"}
	task, err := RepairTask(d, report)
	if err != nil {
		t.Fatal(err)
	}
	want := repairFraming + "\n## The rules' report\n\n" +
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
	if _, err := RepairTask(d, nil); err == nil {
		t.Error("a repair turn with no report")
	}
	var many []string
	for i := range 50 {
		many = append(many, fmt.Sprintf("V%d %s", i, strings.Repeat("y", 900)))
	}
	task, _ = RepairTask(d, many)
	if strings.Count(task, "\n- V") != maxReportLines || !strings.Contains(task, "- … and 10 more\n") || strings.Contains(task, strings.Repeat("y", maxReportLine+1)) {
		t.Error("the report is not bounded")
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

	d, err := Repaired(first, env, out, report)
	if err != nil {
		t.Fatal(err)
	}
	if string(d.Manifest) != `{"schemaVersion":"2","name":"x","summary":"Reads notes.","modes":["autonomous"]}` {
		t.Fatalf("manifest %s", d.Manifest)
	}
	if d.Instructions != first.Instructions || d.Matches[0].Ref != "acme/MATCH-MARKER" || d.OpenQuestions[0] != "QUESTION-MARKER?" ||
		d.Drafter.RunID != "run-1" || d.AIReview != first.AIReview || d.Validation != nil || d.ManifestError != "" {
		t.Fatalf("draft %+v", d)
	}
	const wantNotes = "NOTES-MARKER: the page said to add a host.\n\n" +
		"## Corrected after drafting\n\nThe draft broke a rule in a way that needed no judgement, so it was corrected before the rules checked it:\n\n" +
		"- modes: \"autonomous\" was named twice, so the second was dropped.\n\n" +
		"## Repaired once\n\nThe rules refused the Drafter's first answer, so it was given their report, and nothing else, for one more turn without its web tools. " +
		"This draft is its second answer. What the rules said of the first:\n\n" +
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
	d, err = Repaired(first, env, out, report)
	if err != nil || d.Instructions != "# X\n\n## Role\n\nAgain.\n" || strings.Contains(d.Notes, "forged") {
		t.Fatalf("%v %+v", err, d)
	}

	env.Result = json.RawMessage(`{"sources": []}`)
	_, err = Repaired(first, env, out, report)
	var ce *engine.ClassifiedError
	if !errors.As(err, &ce) || ce.Code != engine.ExitOutputSchema {
		t.Fatalf("not a draft: %v", err)
	}

	first.NoteNotRepaired("its run ended with exit 16")
	if !strings.HasPrefix(first.Notes, "NOTES-MARKER: the page said to add a host.\n\n## Not repaired\n\n") ||
		!strings.Contains(first.Notes, "(its run ended with exit 16)") || !strings.HasSuffix(first.Notes, "(injection)\n") {
		t.Fatalf("notes:\n%s", first.Notes)
	}
}

// TestTheInstructionStatesItsBoundsByValue: the instruction teaches the
// model the numbers the rules use, and its own caps (plan
// builder-simplify.md §5.1, D8), each held equal to where it is enforced.
func TestTheInstructionStatesItsBoundsByValue(t *testing.T) {
	raw, err := fs.ReadFile(FS(), "AGENT.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	for _, want := range []string{
		fmt.Sprintf("`summary`: one line of at most %d characters", manifest.MaxSummaryChars),
		fmt.Sprintf("`tags`: at most %d, each named once", manifest.MaxTags),
		fmt.Sprintf("`guide.overview`: at most %s bytes", thousands(manifest.MaxGuideOverviewBytes)),
		fmt.Sprintf("`guide.prepare`: each item one line of at most %d characters", manifest.MaxGuidePrepareChars),
		fmt.Sprintf("`guide.files`: one line of at most %d characters", manifest.MaxGuideFilesChars),
		fmt.Sprintf("`guide.task`: one line of at most %d characters", manifest.MaxGuideTaskChars),
		fmt.Sprintf("`guide.connect`: each key's help at most %s characters", thousands(manifest.MaxGuideConnectChars)),
		fmt.Sprintf("`interactive.opening`: at most %d characters", manifest.MaxOpeningChars),
		fmt.Sprintf("Intake questions: **at most %d**", MaxIntake),
		fmt.Sprintf("`prepare`: **at most %d** short items", MaxPrepare),
		fmt.Sprintf("`openQuestions`: **at most %d**", MaxOpenQuestions),
		fmt.Sprintf("The notes: **at most %d short bullets**", MaxNoteBullets),
		fmt.Sprintf("`prepare` (at most %d things to gather first)", MaxPrepare),
		fmt.Sprintf("at most %d short Markdown bullets", MaxNoteBullets),
		fmt.Sprintf("`openQuestions`: at most %d (above)", MaxOpenQuestions),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the instruction does not state %q", want)
		}
	}
	// The caps are the Drafter's, inside the rules' bounds.
	if MaxIntake > manifest.MaxIntakeQuestions || MaxPrepare > manifest.MaxGuidePrepare {
		t.Error("a cap is looser than the rule it sits inside")
	}
	for _, h := range instructionShape {
		if !strings.Contains(text, "`"+h+"`") {
			t.Errorf("the instruction does not teach the heading %s", h)
		}
	}
}
