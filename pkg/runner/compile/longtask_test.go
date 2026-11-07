package compile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// TestTheLongTaskNotesAreAnAutonomousRunsOwn: the System Prompt's mode section
// carries the long-task placeholder at the end of the autonomous paragraph,
// which the engine fills in for an autonomous run and empties for a session,
// so both modes' plans keep one instruction; a persistent agent, whose notes
// live in its own folders, has neither the placeholder nor the harness's
// notes file.
func TestTheLongTaskNotesAreAnAutonomousRunsOwn(t *testing.T) {
	p := systemPrompt(promptFacts{caps: capsNone(false)})
	if !strings.Contains(p, "say what you assumed, and finish."+spec.PHLongTask+"\n\n**Interactive.**") {
		t.Fatal("the autonomous paragraph does not end with the long-task placeholder")
	}
	for _, want := range []string{
		" A task that reads more than a few pages",
		"keeps notes in notes.md in your scratch folder: the task in one line, a cursor saying what is done and what is next, and the findings so far",
		"after each page",
		"When the conversation shows a summary of earlier work, read notes.md with scratch_read before anything else and go on from its cursor;",
		"a page you read before the summary may come back marked unchanged, so read it again with fresh: true.",
		"Build your answer and your reports from the notes; notes.md stays an unmarked working note, never a deliverable.",
	} {
		if !strings.Contains(LongTaskNotes, want) {
			t.Errorf("the long-task sentences do not say %q", want)
		}
	}
	// It is filled into every harness's form of the instruction: one line,
	// nothing a YAML, JSON or TOML string would have to escape.
	if strings.ContainsAny(LongTaskNotes+NotesFileForHarness, "\r\n\"\\'`$@{}") {
		t.Errorf("the long-task text holds a character a harness file would escape: %q", LongTaskNotes)
	}
	if !strings.HasPrefix(NotesFileForHarness, NotesFile+" ") {
		t.Errorf("the harness's notes file %q does not name %s", NotesFileForHarness, NotesFile)
	}

	pp := systemPrompt(promptFacts{caps: capsNone(false), persistent: true})
	if strings.Contains(pp, spec.PHLongTask) || !strings.Contains(pp, "say what you assumed, and finish.\n\n**Interactive.**") {
		t.Error("a persistent agent's prompt carries the long-task placeholder")
	}
}

// TestTheHarnessNamesTheNotesFileExceptForAPersistentAgent: docker-agent's
// compaction_notes is the run-time notes file in a run's and a session's
// config, and absent from a persistent agent's.
func TestTheHarnessNamesTheNotesFileExceptForAPersistentAgent(t *testing.T) {
	m := parseManifest(t, fullCapsYAML)
	_, dir := compileFixtureMode(t, m, workspaceFor(t, m, nil), "")
	cfg, err := os.ReadFile(filepath.Join(dir, "harness", "agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), "compaction_notes: '"+spec.PHNotesFile+"'") {
		t.Errorf("a run's harness config does not name the notes file:\n%s", cfg)
	}

	pdir := t.TempDir()
	if _, err := Compile(persistentInputs(t, &spec.AgentFiles{}), pdir); err != nil {
		t.Fatal(err)
	}
	pcfg, err := os.ReadFile(filepath.Join(pdir, "harness", "agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(pcfg), "compaction_notes") || strings.Contains(string(pcfg), spec.PHLongTask) {
		t.Errorf("a persistent agent's harness config carries the run's notes:\n%s", pcfg)
	}
}
