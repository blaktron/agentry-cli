package localrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/drafter"
	"github.com/blaktron/agentry-cli/pkg/examiner"
	"github.com/blaktron/agentry-cli/pkg/knowledge/creator"
	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// shippedRun executes r on the fake docker client, keeping the run's
// directory, and returns the envelope and the knowledge packs its rendered
// tools spec grants.
func shippedRun(t *testing.T, r Run) (*receipt.Envelope, []string, error) {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
	if r.Policy == nil {
		pol := policy.Default()
		pol.Images.Runner = "agentry-runner:test"
		r.Policy = pol
	}
	r.PolicySHA, r.Build, r.KeepRunDir, r.Stderr = "sha", "agentry 9.9.9 (test)", true, os.Stderr
	if r.OutDir == "" {
		r.OutDir = t.TempDir()
	}
	// The Drafter and the Examiner return structured output: the least
	// answer each schema takes.
	answer := `{"manifest":{"schemaVersion":"2","name":"x","summary":"x","modes":["autonomous"]}}`
	if r.Builtin.Name() == examiner.Name {
		answer = `{"summary":"x","findings":[],"notRead":[]}`
	}
	reply, _ := json.Marshal(answer)
	f := &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
		"-probe": {ExitCode: 0},
		"-agent": {Stdout: `{"type":"agent_choice","message":{"content":` + string(reply) + `}}` + "\n" +
			`{"type":"stream_stopped","usage":{"input_tokens":10,"output_tokens":3}}` + "\n"},
	}}
	env, err := r.Execute(context.Background(), f)
	if err != nil {
		return env, nil, err
	}
	_, dir, mErr := r.Builtin.Materialize(r.Home, r.Build)
	if mErr != nil {
		t.Fatal(mErr)
	}
	raw, rErr := os.ReadFile(filepath.Join(dir, "runs", r.RunID, "cfg", "gateway", "toolspec.json")) //nolint:gosec // the test's own run
	if rErr != nil {
		t.Fatal(rErr)
	}
	var ts struct {
		Knowledge []struct {
			Name string `json:"name"`
		} `json:"knowledge"`
	}
	if err := json.Unmarshal(raw, &ts); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, k := range ts.Knowledge {
		names = append(names, k.Name)
	}
	return env, names, nil
}

// TestEveryDrafterRunGetsItsDraftingGuide: the pack the Drafter ships is
// mounted in every run of it, read from its own manifest inside Execute, so
// even the repair turn, which RepairRun builds with no packs at all, has it. It
// comes before the person's packs, and the receipt names it as the built-in's
// own, with no scan record of its own.
func TestEveryDrafterRunGetsItsDraftingGuide(t *testing.T) {
	home := t.TempDir()
	first := Run{Builtin: Drafter(), Home: home}

	repair := RepairRun(first, drafter.RepairTurn{Task: "Repair the draft.", OutDir: t.TempDir(), RunID: "feed0001"})
	if len(repair.Knowledge) != 0 {
		t.Fatalf("the repair turn was built with packs %v", repair.Knowledge)
	}
	env, packs, err := shippedRun(t, repair)
	if err != nil {
		t.Fatalf("the repair turn: %v", err)
	}
	if len(packs) != 1 || packs[0] != drafter.GuideName {
		t.Fatalf("the repair turn's packs %v, want [%s]", packs, drafter.GuideName)
	}
	line := env.CapabilitiesEnforced["knowledge"]
	if !strings.HasPrefix(line, drafter.GuideName+" shipped with the release (built-in package tree sha256:") || strings.Contains(line, "scan:") {
		t.Fatalf("knowledge line %q", line)
	}

	// With the person's pack: the guide first, then theirs.
	person := knowledgetest.Make(t, "runbooks", nil)
	withPerson := RepairRun(first, drafter.RepairTurn{Task: "Repair the draft.", OutDir: t.TempDir(), RunID: "feed0002"})
	withPerson.Knowledge = []engine.AttachedPack{engine.AttachPack(person)}
	if _, packs, err = shippedRun(t, withPerson); err != nil {
		t.Fatalf("with the person's pack: %v", err)
	}
	if strings.Join(packs, ",") != drafter.GuideName+",runbooks" {
		t.Fatalf("packs %v, want the guide then the person's", packs)
	}
}

// TestEveryExaminerRunGetsItsReviewGuide: the Examiner's own pack is mounted
// in its run beside the review grant, and the receipt names it as the
// built-in's own, with no scan record of its own. The package under review
// reaches the run only as its inputs.
func TestEveryExaminerRunGetsItsReviewGuide(t *testing.T) {
	in := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(in, []byte(`{"schemaVersion":"2"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	env, packs, err := shippedRun(t, Run{Builtin: Examiner(), Home: t.TempDir(), Task: strings.NewReader("Examine."), RunID: "feed0005",
		Inputs: []string{in}, Review: &engine.ReviewRun{JevKey: "jev-test-key"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) != 1 || packs[0] != examiner.GuideName {
		t.Fatalf("the Examiner's packs %v, want [%s]", packs, examiner.GuideName)
	}
	line := env.CapabilitiesEnforced["knowledge"]
	if !strings.HasPrefix(line, examiner.GuideName+" shipped with the release (built-in package tree sha256:") || strings.Contains(line, "scan:") {
		t.Fatalf("knowledge line %q", line)
	}
}

// TestAPersonsPackCannotTakeTheGuidesName: both would be mounted at
// /knowledge/drafting-guide, so the run is refused before anything is
// compiled, naming the pack and the built-in.
func TestAPersonsPackCannotTakeTheGuidesName(t *testing.T) {
	impostor := knowledgetest.Make(t, drafter.GuideName, nil)
	r := RepairRun(Run{Builtin: Drafter(), Home: t.TempDir()}, drafter.RepairTurn{Task: "Repair the draft.", OutDir: t.TempDir(), RunID: "feed0003"})
	r.Knowledge = []engine.AttachedPack{engine.AttachPack(impostor)}
	_, _, err := shippedRun(t, r)
	var ce *engine.ClassifiedError
	if err == nil || !errors.As(err, &ce) || ce.Code != engine.ExitCLI ||
		!strings.Contains(err.Error(), "the knowledge pack "+drafter.GuideName+": "+Drafter().Ref()+" ships a knowledge pack of that name, mounted at "+spec.KnowledgeMount+"/"+drafter.GuideName) {
		t.Fatalf("a person's pack named %s: %v", drafter.GuideName, err)
	}
}

// TestABuiltinThatShipsNothingMountsNoPack: the loader is general, the
// content each built-in's own; the Voice Writer ships nothing and its run
// has no knowledge line.
func TestABuiltinThatShipsNothingMountsNoPack(t *testing.T) {
	env, packs, err := shippedRun(t, Run{Builtin: VoiceWriter(), Home: t.TempDir(), Task: strings.NewReader("Write the voice."), RunID: "feed0004"})
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) != 0 || env.CapabilitiesEnforced["knowledge"] != "" {
		t.Fatalf("the Voice Writer's run mounted %v (%q)", packs, env.CapabilitiesEnforced["knowledge"])
	}
}

// TestEveryCreatorRunGetsItsPackWritingGuide: the Knowledge Creator's own
// guide is mounted in its run, beside the pack a build extends.
func TestEveryCreatorRunGetsItsPackWritingGuide(t *testing.T) {
	_, packs, err := shippedRun(t, Run{Builtin: KnowledgeCreator().WithoutWebTools(), Home: t.TempDir(), Task: strings.NewReader("Ingest."), RunID: "feed0006"})
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) != 1 || packs[0] != creator.GuideName {
		t.Fatalf("the Creator's packs %v, want [%s]", packs, creator.GuideName)
	}
}

// TestABuiltinsRunCopyIsHeldToItsEmbeddedTree: the engine is handed a check
// that refuses a run copy whose tree is not the embedded one, so a pack or
// an instruction changed in the runner home after Materialize is never
// staged under the embedded tree's name.
func TestABuiltinsRunCopyIsHeldToItsEmbeddedTree(t *testing.T) {
	meta, dir, err := Drafter().Materialize(t.TempDir(), "agentry 9.9.9 (test)")
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(dir, "workspace")
	check := builtinCopyCheck(meta.Digest)
	if err := check(workspace); err != nil {
		t.Fatalf("the embedded tree: %v", err)
	}
	page := filepath.Join(workspace, "knowledge", drafter.GuideName, "wiki", "guide", "start-here.md")
	if err := os.WriteFile(page, []byte("changed\n"), 0o644); err != nil { //nolint:gosec // the test's own copy
		t.Fatal(err)
	}
	if err := check(workspace); err == nil || !strings.Contains(err.Error(), "not its embedded tree") {
		t.Fatalf("a changed pack page: %v", err)
	}
}
