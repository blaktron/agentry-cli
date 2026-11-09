package compile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// replacedSentencesBytes is the size of the sentences the checklist
// paragraph took the place of, which kept a long task's notes in a file of
// the scratch folder: the paragraph is no larger, so an autonomous run's
// prompt does not grow.
const replacedSentencesBytes = 631

// TestEveryModeHearsOfTheChecklist: the checklist paragraph closes "Your
// mode" in a run's prompt and in a persistent agent's, says to read the
// list after a summary and to read a page again with fresh: true, and is no
// larger than what it replaced; a plan without the scratch folder, which the
// tool goes with, hears of no checklist.
func TestEveryModeHearsOfTheChecklist(t *testing.T) {
	caps := capsNone(false)
	caps.Scratch = true
	for _, persistent := range []bool{false, true} {
		p := systemPrompt(promptFacts{caps: caps, persistent: persistent})
		if !strings.Contains(p, "short enough to read.\n"+checklistParagraph+"\n## Your environment") {
			t.Errorf("persistent %v: the checklist paragraph does not close the mode section", persistent)
		}
	}
	for _, want := range []string{
		"starts with one: checklist with the steps in order",
		"Keep one step in progress",
		"After a summary of earlier work, read the checklist first and go on from the open step",
		"may come back marked unchanged, so read it again with fresh: true.",
		"every step done, or dropped with the reason given in your answer.",
		"A task of one or two steps needs none.",
	} {
		if !strings.Contains(checklistParagraph, want) {
			t.Errorf("the checklist paragraph does not say %q", want)
		}
	}
	if n := len(checklistParagraph); n > replacedSentencesBytes {
		t.Errorf("the checklist paragraph is %d bytes, more than the %d it replaced", n, replacedSentencesBytes)
	}
	if strings.Contains(systemPrompt(promptFacts{caps: capsNone(false)}), "checklist") {
		t.Error("a plan without the scratch folder hears of the checklist")
	}
	for _, name := range []string{spec.ChecklistForHarness, spec.AgentChecklistForHarness} {
		if strings.ContainsAny(name, "\r\n\"\\'`$@{}") || !strings.HasPrefix(name, spec.ChecklistFile+" ") {
			t.Errorf("the harness's checklist line %q must name %s on one line a config string need not escape", name, spec.ChecklistFile)
		}
	}
	if spec.ChecklistFile != toolsrv.ChecklistFile {
		t.Errorf("the plan names %s and the tool keeps %s", spec.ChecklistFile, toolsrv.ChecklistFile)
	}
}

// TestTheChecklistIsGrantedWhereItsFileLives: a run's tools spec grants the
// checklist in the scratch folder and its harness's summary points there; a
// persistent agent's keeps it in its data folder, since its scratch folder
// lasts one segment.
func TestTheChecklistIsGrantedWhereItsFileLives(t *testing.T) {
	m := parseManifest(t, fullCapsYAML)
	_, dir := compileFixtureMode(t, m, workspaceFor(t, m, nil), "")
	pdir := t.TempDir()
	if _, err := Compile(persistentInputs(t, &spec.AgentFiles{}), pdir); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		dir, file, harness string
	}{
		{dir, spec.ScratchMount + "/" + spec.ChecklistFile, spec.ChecklistForHarness},
		{pdir, spec.AgentRoot + "/" + spec.AgentDataDir + "/" + spec.ChecklistFile, spec.AgentChecklistForHarness},
	} {
		raw, err := os.ReadFile(filepath.Join(c.dir, "gateway", "toolspec.json"))
		if err != nil {
			t.Fatal(err)
		}
		var ts toolsrv.Spec
		if err := json.Unmarshal(raw, &ts); err != nil {
			t.Fatal(err)
		}
		if ts.Checklist != c.file || !slices.Contains(ts.GrantedTools(), toolsrv.ToolChecklist) {
			t.Errorf("the tools spec keeps the checklist at %q (granted: %v), want %q", ts.Checklist, ts.GrantedTools(), c.file)
		}
		cfg, err := os.ReadFile(filepath.Join(c.dir, "harness", "agent.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(cfg), "compaction_notes: "+c.harness+"\n") && !strings.Contains(string(cfg), "compaction_notes: '"+c.harness+"'") {
			t.Errorf("the harness's summary does not point at %q:\n%s", c.harness, cfg)
		}
		if strings.Contains(string(cfg), "notes.md") {
			t.Errorf("the harness config still names notes.md:\n%s", cfg)
		}
	}
}
