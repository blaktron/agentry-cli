package toolsrv

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func checklistSpec(t *testing.T) *Spec {
	t.Helper()
	spec := scratchSpec(t)
	spec.Checklist = filepath.Join(spec.ScratchDir, ChecklistFile)
	return spec
}

func checklistCall(t *testing.T, s *server, steps []Step) (string, bool) {
	t.Helper()
	if steps == nil {
		return s.toolChecklist(rawArgs(t, map[string]any{}))
	}
	return s.toolChecklist(rawArgs(t, map[string]any{"steps": steps}))
}

// TestTheChecklistFollowsItsGrant: the checklist tool exists exactly when
// the spec names a checklist file.
func TestTheChecklistFollowsItsGrant(t *testing.T) {
	spec := scratchSpec(t)
	if slices.Contains(spec.GrantedTools(), ToolChecklist) {
		t.Fatal("no checklist file, but the checklist tool is granted")
	}
	spec.Checklist = filepath.Join(spec.ScratchDir, ChecklistFile)
	if !slices.Contains(spec.GrantedTools(), ToolChecklist) {
		t.Fatalf("checklist file set, tool missing from %v", spec.GrantedTools())
	}
	s := newTestServer(t, spec)
	if _, ok := s.tools[ToolChecklist]; !ok {
		t.Fatal("the checklist tool is granted but not registered")
	}
}

// TestTheChecklistReplacesReadsAndCountsOpenSteps: one call replaces the
// whole list, a call without steps reads it back, and every answer gives
// the list with the count of open steps. The file is Markdown that
// scratch_read reads as it is.
func TestTheChecklistReplacesReadsAndCountsOpenSteps(t *testing.T) {
	spec := checklistSpec(t)
	s := newTestServer(t, spec)
	if got, isErr := checklistCall(t, s, nil); isErr || !strings.HasPrefix(got, "No checklist yet") {
		t.Fatalf("an empty checklist read answered %q (error %v)", got, isErr)
	}
	steps := []Step{
		{Text: "Read the lockfile to the end", Status: StepDone},
		{Text: "Query OSV for every package", Status: StepInProgress},
		{Text: "Trace the dev-only packages", Status: StepDropped, Reason: "they never ship"},
		{Text: "Write the report", Status: StepPending},
	}
	got, isErr := checklistCall(t, s, steps)
	if isErr {
		t.Fatalf("a valid list was refused: %s", got)
	}
	for _, want := range []string{"Saved.", "2 of 4 steps open; in progress: Query OSV for every package", "- [-] Trace the dev-only packages (dropped: they never ship)", "Before your final answer every step is done, or dropped with its reason."} {
		if !strings.Contains(got, want) {
			t.Errorf("the answer lacks %q:\n%s", want, got)
		}
	}
	file, err := os.ReadFile(spec.Checklist)
	if err != nil {
		t.Fatal(err)
	}
	want := "# Checklist\n\n- [x] Read the lockfile to the end\n- [>] Query OSV for every package\n- [-] Trace the dev-only packages (dropped: they never ship)\n- [ ] Write the report\n"
	if string(file) != want {
		t.Fatalf("the file holds\n%s\nwant\n%s", file, want)
	}
	if read, isErr := s.toolScratchRead(rawArgs(t, map[string]any{"path": ChecklistFile})); isErr || read != want {
		t.Fatalf("scratch_read of the checklist answered %q (error %v)", read, isErr)
	}
	if again, isErr := checklistCall(t, s, nil); isErr || again != strings.TrimPrefix(got, "Saved. ") {
		t.Fatalf("reading the list back answered\n%s\nnot\n%s", again, strings.TrimPrefix(got, "Saved. "))
	}
	if fi, err := os.Stat(spec.Checklist); err != nil || fi.Mode().Perm() != scratchFileMode {
		t.Fatalf("the checklist file is %v (%v), want %v like any scratch file", fi.Mode().Perm(), err, scratchFileMode)
	}

	done := []Step{{Text: "Read the lockfile to the end", Status: StepDone}, {Text: "Write the report", Status: StepDropped, Reason: "the inputs held no lockfile"}}
	if got, isErr := checklistCall(t, s, done); isErr || !strings.Contains(got, "All 2 steps are done or dropped") || strings.Contains(got, "Before your final answer") {
		t.Fatalf("a finished list answered %q (error %v)", got, isErr)
	}
}

// TestTheChecklistHoldsItsShape: a second step in progress, a drop without
// a reason, an unknown status, a step of more than one line and a reason on
// a step that is not dropped are refused, each saying why, and leave the
// kept list as it was.
func TestTheChecklistHoldsItsShape(t *testing.T) {
	spec := checklistSpec(t)
	s := newTestServer(t, spec)
	kept := []Step{{Text: "One", Status: StepInProgress}}
	if got, isErr := checklistCall(t, s, kept); isErr {
		t.Fatal(got)
	}
	before, _ := os.ReadFile(spec.Checklist)
	many := make([]Step, maxChecklistSteps+1)
	for i := range many {
		many[i] = Step{Text: fmt.Sprintf("step %d", i), Status: StepPending}
	}
	cases := []struct {
		steps []Step
		want  string
	}{
		{[]Step{{Text: "a", Status: StepInProgress}, {Text: "b", Status: StepInProgress}}, "2 steps are in_progress; keep one in progress"},
		{[]Step{{Text: "a", Status: StepDropped}}, "step 1 is dropped without a reason"},
		{[]Step{{Text: "a", Status: "blocked"}}, `step 1 has status "blocked"`},
		{[]Step{{Text: "a\nb", Status: StepPending}}, "step 1 is more than one line"},
		{[]Step{{Text: "a", Status: StepDropped, Reason: "x\ny"}}, "step 1 is more than one line"},
		{[]Step{{Text: "a", Status: StepDone, Reason: "because"}}, "step 1 has a reason but is done"},
		{[]Step{{Text: "  ", Status: StepPending}}, "step 1 has no text"},
		{[]Step{{Text: "a (dropped: b)", Status: StepPending}}, "which the list uses to mark a drop"},
		{[]Step{{Text: strings.Repeat("x", maxStepBytes+1), Status: StepPending}}, "longer than 500 bytes"},
		{many, "101 steps is more than the 100"},
	}
	for _, c := range cases {
		got, isErr := checklistCall(t, s, c.steps)
		if !isErr || !strings.Contains(got, c.want) {
			t.Errorf("%v answered %q (error %v), want %q", c.steps, got, isErr, c.want)
		}
	}
	if after, _ := os.ReadFile(spec.Checklist); string(after) != string(before) {
		t.Fatalf("a refused list changed the file:\n%s", after)
	}
}

// TestTheChecklistRefusesARunSecret: a step holding a granted secret's value
// is refused as scratch_write refuses it, naming the secret and not its
// value, and nothing is written.
func TestTheChecklistRefusesARunSecret(t *testing.T) {
	t.Setenv("FETCH_TOKEN", "checklist-fixture-value")
	spec := checklistSpec(t)
	spec.Fetch = Fetch{Enabled: true, Hosts: []string{"api.example.com"}, SecretEnv: []string{"FETCH_TOKEN"}}
	s := newTestServer(t, spec)
	got, isErr := checklistCall(t, s, []Step{{Text: "Try checklist-fixture-value", Status: StepPending}})
	if !isErr || !strings.Contains(got, "FETCH_TOKEN") || strings.Contains(got, "checklist-fixture") {
		t.Fatalf("a run secret in a step answered %q (error %v)", got, isErr)
	}
	if _, err := os.Stat(spec.Checklist); !os.IsNotExist(err) {
		t.Fatalf("a refused list left a file: %v", err)
	}
}

// TestAChecklistWrittenByHandReadsAsFarAsItKeepsTheShape: the file is the
// agent's to write with scratch_write too; reading it back takes every line
// in a status's box and passes over the rest.
func TestAChecklistWrittenByHandReadsAsFarAsItKeepsTheShape(t *testing.T) {
	spec := checklistSpec(t)
	mustWrite(t, spec.Checklist, []byte("# My list\nsome prose\n- [x] first\n- [ ] second\n- [-] third (dropped: no time)\n- [-] fourth\n* [ ] not a step\n"))
	s := newTestServer(t, spec)
	got, isErr := checklistCall(t, s, nil)
	if isErr {
		t.Fatal(got)
	}
	for _, want := range []string{"1 of 4 steps open, none in progress.", "- [x] first", "- [ ] second", "- [-] third (dropped: no time)", "- [-] fourth (dropped: no reason given)"} {
		if !strings.Contains(got, want) {
			t.Errorf("the hand-written list reads without %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "not a step") || strings.Contains(got, "prose") {
		t.Errorf("a line outside the shape was read as a step:\n%s", got)
	}
}

// TestTheChecklistNeverFollowsALinkOut: a declared command may put a link
// where the file is; the checklist neither reads nor writes through it.
func TestTheChecklistNeverFollowsALinkOut(t *testing.T) {
	spec := checklistSpec(t)
	outside := filepath.Join(t.TempDir(), "target.md")
	mustWrite(t, outside, []byte("- [ ] outside\n"))
	if err := os.Symlink(outside, spec.Checklist); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, spec)
	if got, isErr := checklistCall(t, s, nil); !isErr && strings.Contains(got, "outside") {
		t.Fatalf("reading the checklist followed a link out: %q", got)
	}
	_, _ = checklistCall(t, s, []Step{{Text: "mine", Status: StepPending}})
	if b, _ := os.ReadFile(outside); string(b) != "- [ ] outside\n" {
		t.Fatalf("writing the checklist wrote through a link: %q", b)
	}
}

// TestTheChecklistIsWrittenOnlyAsAFile: a link to another scratch file put
// where the file is is refused on a write and a read alike, and the file it
// points to is left as it was.
func TestTheChecklistIsWrittenOnlyAsAFile(t *testing.T) {
	spec := checklistSpec(t)
	other := filepath.Join(spec.ScratchDir, "findings.md")
	mustWrite(t, other, []byte("my findings\n"))
	if err := os.Symlink("findings.md", spec.Checklist); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, spec)
	for _, steps := range [][]Step{nil, {{Text: "mine", Status: StepPending}}} {
		if got, isErr := checklistCall(t, s, steps); !isErr || !strings.Contains(got, "a link, a folder or a pipe") {
			t.Errorf("a link in the checklist's place answered %q (error %v)", got, isErr)
		}
	}
	if b, _ := os.ReadFile(other); string(b) != "my findings\n" {
		t.Fatalf("the checklist wrote through a link to another scratch file: %q", b)
	}
}

// TestAPersistentAgentKeepsItsChecklistInItsDataFolder: where the checklist
// file is in the agent's own folders, it is written there as fs_write
// writes, and the scratch folder holds none.
func TestAPersistentAgentKeepsItsChecklistInItsDataFolder(t *testing.T) {
	spec := scratchSpec(t)
	if err := os.MkdirAll(filepath.Join(spec.WorkspaceDir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec.FSWrite = Access{Enabled: true, Paths: []string{"data"}}
	spec.Checklist = filepath.Join(spec.WorkspaceDir, "data", ChecklistFile)
	s := newTestServer(t, spec)
	if got, isErr := checklistCall(t, s, []Step{{Text: "Answer the person", Status: StepInProgress}}); isErr {
		t.Fatal(got)
	}
	fi, err := os.Stat(spec.Checklist)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("the data folder's checklist is %v, want 0644 as fs_write writes", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(spec.ScratchDir, ChecklistFile)); !os.IsNotExist(err) {
		t.Fatalf("the scratch folder holds a checklist too: %v", err)
	}
	if got, isErr := checklistCall(t, s, nil); isErr || !strings.Contains(got, "in progress: Answer the person") {
		t.Fatalf("reading it back answered %q (error %v)", got, isErr)
	}
}

// TestTheRunnerGetsACopyOfEachListSavedOrRead: with checklistCopy stamped,
// every list the tool saves, and every list it reads back (one written by
// hand included), is copied there whole; a refused list is not.
func TestTheRunnerGetsACopyOfEachListSavedOrRead(t *testing.T) {
	spec := checklistSpec(t)
	spec.ChecklistCopy = filepath.Join(spec.OutDir, "receipts", ChecklistFile)
	if err := os.MkdirAll(filepath.Dir(spec.ChecklistCopy), 0o755); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, spec)
	steps := []Step{{Text: "one", Status: StepDone}, {Text: "two", Status: StepInProgress}}
	if got, isErr := checklistCall(t, s, steps); isErr {
		t.Fatal(got)
	}
	want := "# Checklist\n\n- [x] one\n- [>] two\n"
	if b, _ := os.ReadFile(spec.ChecklistCopy); string(b) != want {
		t.Fatalf("the copy holds %q, want %q", b, want)
	}
	if open := OpenSteps(ParseChecklist(want)); len(open) != 1 || open[0].Text != "two" {
		t.Fatalf("open steps %+v", open)
	}
	if got, isErr := checklistCall(t, s, []Step{{Text: "a", Status: StepInProgress}, {Text: "b", Status: StepInProgress}}); !isErr {
		t.Fatalf("two steps in progress were saved: %s", got)
	}
	if b, _ := os.ReadFile(spec.ChecklistCopy); string(b) != want {
		t.Fatalf("a refused list reached the copy: %q", b)
	}
	if err := os.WriteFile(spec.Checklist, []byte("my notes\n- [x] one\n- [x] two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, isErr := checklistCall(t, s, nil); isErr {
		t.Fatal(got)
	}
	if b, _ := os.ReadFile(spec.ChecklistCopy); string(b) != "# Checklist\n\n- [x] one\n- [x] two\n" {
		t.Fatalf("the copy of a hand-written list holds %q", b)
	}
}

// TestTheChecklistCopyStaysUnderTheOutputFolder: the spec is refused when
// the copy is outside the output folder or the run keeps no checklist.
func TestTheChecklistCopyStaysUnderTheOutputFolder(t *testing.T) {
	spec := checklistSpec(t)
	spec.ChecklistCopy = filepath.Join(spec.ScratchDir, "copy.md")
	if err := validateChecklistCopy(spec); err == nil {
		t.Fatal("a copy outside the output folder was accepted")
	}
	spec.ChecklistCopy = filepath.Join(spec.OutDir, "receipts", ChecklistFile)
	if err := validateChecklistCopy(spec); err != nil {
		t.Fatal(err)
	}
	spec.Checklist = ""
	if err := validateChecklistCopy(spec); err == nil {
		t.Fatal("a copy without a checklist was accepted")
	}
}
