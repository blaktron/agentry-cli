package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/drafter"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// reviseRelease is a release folder to revise, and the files of the
// person's two texts.
func reviseRelease(t *testing.T) (dir, instructions, problems string) {
	t.Helper()
	dir = t.TempDir()
	for name, body := range map[string]string{
		"agent.yaml": "schemaVersion: \"2\"\nname: meeting-minutes\nnamespace: acme\nversion: 1.2.0\n" +
			"summary: Turns meeting notes into decisions and action items.\nlicense: Apache-2.0\nmodes: [autonomous]\n" +
			"entrypoints:\n  instruction: AGENT.md\ncapabilities:\n  network: {egress: none}\n  filesystem: {read: none, write: none}\n  exec: none\n" +
			"publish:\n  include: [agent.yaml, AGENT.md, README.md, LICENSE]\n",
		"AGENT.md":  "# Meeting minutes\n\nRELEASE-INSTRUCTION-MARKER\n",
		"README.md": "# Meeting minutes\n",
		"LICENSE":   "Apache-2.0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	texts := t.TempDir()
	instructions, problems = filepath.Join(texts, "instructions.txt"), filepath.Join(texts, "problems.txt")
	for path, body := range map[string]string{instructions: "Add the due date to each action.\n", problems: "It drops actions with no owner.\n"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, instructions, problems
}

// TestDraftReviseGivesTheDrafterTheReleaseAndScansTheTexts: a revision's
// task, the person's two texts in the third person, is what AI Review reads
// before the Drafter starts; the release reaches the Drafter as files under
// /inputs/revise and is not in the task; the draft keeps the release's name
// and records the release in revises, with the digest `agentry publish`
// makes of the folder.
func TestDraftReviseGivesTheDrafterTheReleaseAndScansTheTexts(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	t.Setenv(policy.DefaultJudgeKeyEnv, testJev)
	sc, runs, _ := draftSeams(t, webscan.Result{Verdict: webscan.Pass, Model: "jev-1.13.0"}, goodDraft, draftLedger)
	rel, instructions, problems := reviseRelease(t)
	out := filepath.Join(t.TempDir(), "draft-out")
	_, stderr, err := runCLI(t, nil, "draft", "--revise", rel, "--instructions-file", instructions, "--problems-file", problems,
		"--out", out, "--policy", policyPath)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	// AI Review read the task, then the release's two files, before the
	// Drafter started.
	if len(*runs) != 1 || len(sc.read) != 2 {
		t.Fatalf("runs %d scans %d", len(*runs), len(sc.read))
	}
	if r := sc.read[1]; !strings.HasPrefix(r, "input_file|") || !strings.Contains(r, "RELEASE-INSTRUCTION-MARKER") || !strings.Contains(r, "name: meeting-minutes") {
		t.Fatalf("the release was not read first: %q", r)
	}
	scanned := sc.read[0]
	for _, want := range []string{"description|Revise the QuickAgent acme/meeting-minutes@1.2.0", "Add the due date to each action.",
		"It drops actions with no owner.", "/inputs/revise/AGENT.md"} {
		if !strings.Contains(scanned, want) {
			t.Errorf("AI Review did not read %q:\n%s", want, scanned)
		}
	}
	if strings.Contains(scanned, "RELEASE-INSTRUCTION-MARKER") {
		t.Error("the release's instruction file is in the task")
	}
	r := (*runs)[0]
	if !slices.Equal(r.Inputs, []string{filepath.Join(out, drafter.ReviseInput)}) {
		t.Fatalf("inputs %v", r.Inputs)
	}
	staged, err := os.ReadFile(filepath.Join(out, drafter.ReviseInput, "AGENT.md"))
	if err != nil || !strings.Contains(string(staged), "RELEASE-INSTRUCTION-MARKER") {
		t.Fatalf("the release was not staged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, drafter.ReviseInput, "README.md")); err == nil {
		t.Fatal("more of the release than its manifest and instruction file was staged")
	}
	m, err := localrun.ManifestFrom(rel)
	if err != nil {
		t.Fatal(err)
	}
	info, err := localrun.PackWorkspace(rel, m, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(out, "draft.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d drafter.Draft
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if d.Revises == nil || d.Revises.Ref != "acme/meeting-minutes@1.2.0" || d.Revises.Digest != info.Digest {
		t.Fatalf("revises %+v, want digest %s", d.Revises, info.Digest)
	}
	var mm map[string]any
	if err := json.Unmarshal(d.Manifest, &mm); err != nil || mm["name"] != "meeting-minutes" {
		t.Fatalf("manifest %s", d.Manifest)
	}
	pkgYAML, err := os.ReadFile(filepath.Join(out, "package", "agent.yaml"))
	if err != nil || !strings.Contains(string(pkgYAML), "name: meeting-minutes") {
		t.Fatalf("the rendered package does not keep the name: %v\n%s", err, pkgYAML)
	}
	if !strings.Contains(d.Notes, "name: kept as meeting-minutes") {
		t.Fatalf("notes %q", d.Notes)
	}
}

// TestDraftReviseRefusals: a revision whose texts AI Review does not pass
// stops at stage description before the Drafter starts; a command line
// that mixes a revision with a new draft's inputs, or names no text, is a
// usage error before anything is scanned.
func TestDraftReviseRefusals(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	t.Setenv(policy.DefaultJudgeKeyEnv, testJev)
	rel, instructions, problems := reviseRelease(t)

	sc, runs, _ := draftSeams(t, webscan.Result{Verdict: webscan.Withheld, Score: 3, Flagged: []string{"injection"}}, goodDraft, "")
	_, _, err := runCLI(t, nil, "draft", "--revise", rel, "--problems-file", problems, "--policy", policyPath, "--out", filepath.Join(t.TempDir(), "o"))
	wantClassified(t, err, engine.ExitPolicy)
	if len(*runs) != 0 || len(sc.read) != 1 {
		t.Fatalf("runs %d scans %d", len(*runs), len(sc.read))
	}

	// The texts pass and the release does not: the revision stops at stage
	// description, before the Drafter starts, in the form a sandbox host
	// reads.
	sc, runs, _ = draftSeams(t, webscan.Result{Verdict: webscan.Pass}, goodDraft, "")
	sc.flag = "RELEASE-INSTRUCTION-MARKER"
	stdout, _, err := runCLI(t, nil, "draft", "--revise", rel, "--problems-file", problems, "--output-format", "json",
		"--policy", policyPath, "--out", filepath.Join(t.TempDir(), "o"))
	wantClassified(t, err, engine.ExitPolicy)
	// The task, the release's chunk, then each file alone (the pack scan's
	// rule), and no run.
	if len(*runs) != 0 || len(sc.read) < 3 || !strings.HasPrefix(sc.read[1], "input_file|") {
		t.Fatalf("a release AI Review did not pass: runs %d scans %d", len(*runs), len(sc.read))
	}
	wantField(t, draftEnvelope(t, stdout), "stage", `"description"`)

	// A folder a run before left a release in is not staged into again.
	out := filepath.Join(t.TempDir(), "o")
	if err := os.MkdirAll(filepath.Join(out, drafter.ReviseInput), 0o755); err != nil {
		t.Fatal(err)
	}
	sc, runs, _ = draftSeams(t, webscan.Result{Verdict: webscan.Pass}, goodDraft, "")
	_, _, err = runCLI(t, nil, "draft", "--revise", rel, "--problems-file", problems, "--policy", policyPath, "--out", out)
	wantClassified(t, err, engine.ExitUsage)
	if len(*runs) != 0 || len(sc.read) != 0 {
		t.Fatalf("a reused folder: runs %d scans %d", len(*runs), len(sc.read))
	}

	for _, args := range [][]string{
		{"draft", "A description.", "--revise", rel, "--problems-file", problems},
		{"draft", "--revise", rel, "--revise-ref", "acme/meeting-minutes@1.2.0", "--problems-file", problems},
		{"draft", "--revise", rel, "--hint", "a hint", "--problems-file", problems},
		{"draft", "--revise", rel},
		{"draft", "A description.", "--instructions-file", instructions},
		{"draft", "--revise-ref", "acme/meeting-minutes", "--problems-file", problems},
		{"draft", "--hosted", "--revise", rel, "--problems-file", problems},
	} {
		sc, runs, _ := draftSeams(t, webscan.Result{Verdict: webscan.Pass}, goodDraft, "")
		_, _, err := runCLI(t, nil, append(args, "--policy", policyPath, "--out", filepath.Join(t.TempDir(), "o"))...)
		wantClassified(t, err, engine.ExitUsage)
		if len(*runs) != 0 || len(sc.read) != 0 {
			t.Fatalf("%v: runs %d scans %d", args, len(*runs), len(sc.read))
		}
	}
}

// TestDraftReviseRefFetchesTheReleaseHeldToItsDigest: --revise-ref fetches
// the published release as `agentry run` does, records the digest the
// registry resolved, and refuses an archive that is not that digest before
// anything is scanned or run.
func TestDraftReviseRefFetchesTheReleaseHeldToItsDigest(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	t.Setenv(policy.DefaultJudgeKeyEnv, testJev)
	rel, _, problems := reviseRelease(t)
	files := map[string]string{}
	for _, name := range []string{"agent.yaml", "AGENT.md", "README.md", "LICENSE"} {
		b, err := os.ReadFile(filepath.Join(rel, name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = string(b)
	}
	fr := newFakeRegistry(t)
	fv := fr.addAgent("acme", "meeting-minutes", "autonomous", "minutes").addVersion(t, "1.2.0", files)

	sc, runs, _ := draftSeams(t, webscan.Result{Verdict: webscan.Pass, Model: "jev-1.13.0"}, goodDraft, draftLedger)
	out := filepath.Join(t.TempDir(), "draft-out")
	_, stderr, err := runCLI(t, nil, "--registry", fr.url(), "draft", "--revise-ref", "acme/meeting-minutes@1.2.0",
		"--problems-file", problems, "--out", out, "--policy", policyPath)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if len(*runs) != 1 || len(sc.read) != 2 {
		t.Fatalf("runs %d scans %d", len(*runs), len(sc.read))
	}
	staged, err := os.ReadFile(filepath.Join(out, drafter.ReviseInput, "AGENT.md"))
	if err != nil || !strings.Contains(string(staged), "RELEASE-INSTRUCTION-MARKER") {
		t.Fatalf("the fetched release was not staged: %v", err)
	}
	var d drafter.Draft
	b, _ := os.ReadFile(filepath.Join(out, "draft.json"))
	if err := json.Unmarshal(b, &d); err != nil || d.Revises == nil || d.Revises.Ref != "acme/meeting-minutes@1.2.0" || d.Revises.Digest != fv.digest {
		t.Fatalf("revises %+v (%v)", d.Revises, err)
	}

	fv.digest = "sha256:" + strings.Repeat("0", 64)
	sc, runs, _ = draftSeams(t, webscan.Result{Verdict: webscan.Pass}, goodDraft, "")
	_, _, err = runCLI(t, nil, "--registry", fr.url(), "draft", "--revise-ref", "acme/meeting-minutes@1.2.0",
		"--problems-file", problems, "--out", filepath.Join(t.TempDir(), "o"), "--policy", policyPath)
	wantClassified(t, err, engine.ExitVerify)
	if len(*runs) != 0 || len(sc.read) != 0 {
		t.Fatalf("a release that is not its digest: runs %d scans %d", len(*runs), len(sc.read))
	}
}
