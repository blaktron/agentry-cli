package examiner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/gatecases"
	"github.com/blaktron/agentry/pkg/judge"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/validate"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

const fixture = "testdata/instructs-the-examiner"

// prepare runs the pre-step over the fixture against a registry that
// answers with policy, or none when policy is "".
func prepare(t *testing.T, policy string) *Prepared {
	t.Helper()
	reg := ""
	if policy != "" {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/platform/policy" {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, policy)
		}))
		t.Cleanup(srv.Close)
		reg = srv.URL
	}
	p, err := Prepare(context.Background(), fixture, Options{Registry: reg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Cleanup)
	return p
}

// TestPrepareRunsTheRegistrysRulesAndStagesACopy: kind 1 holds the
// registry's rules over the files a submission carries, the fixture's
// hidden instruction among them; the inputs are a copy, the manifest as
// canonical JSON beside the package; the policy is the registry's.
func TestPrepareRunsTheRegistrysRulesAndStagesACopy(t *testing.T) {
	p := prepare(t, `{"maxAutoPublishSeverity":"medium","requireHumanReview":"never","blockedCapabilities":[]}`)
	if p.Ref != "example/release-notes@0.1.0" || p.Files != 4 {
		t.Fatalf("ref %s, %d files", p.Ref, p.Files)
	}
	var hidden bool
	for _, f := range p.Findings {
		if f.Kind != KindRegistryRules {
			t.Errorf("a pre-step finding of kind %s", f.Kind)
		}
		hidden = hidden || (f.Scanner == "S3/html-comment" && f.File == "AGENT.md" && f.Line == 6)
	}
	if !hidden {
		t.Fatalf("the hidden instruction is not a finding: %+v", p.Findings)
	}
	if len(p.Inputs) != 2 || filepath.Base(p.Inputs[0]) != InputManifest || filepath.Base(p.Inputs[1]) != InputPackage {
		t.Fatalf("inputs %v", p.Inputs)
	}
	var m map[string]any
	b, _ := os.ReadFile(p.Inputs[0])
	if json.Unmarshal(b, &m) != nil || m["name"] != "release-notes" {
		t.Fatalf("the staged manifest is not its canonical JSON: %s", b)
	}
	for _, f := range []string{"agent.yaml", "AGENT.md", "README.md", "LICENSE"} {
		staged, err := os.ReadFile(filepath.Join(p.Inputs[1], f))
		orig, _ := os.ReadFile(filepath.Join(fixture, f))
		if err != nil || string(staged) != string(orig) {
			t.Fatalf("%s is not staged as it is: %v", f, err)
		}
	}
	if p.Policy.Source != "registry" || p.Policy.MaxAutoPublishSeverity != "medium" {
		t.Fatalf("policy %+v", p.Policy)
	}
	p.Cleanup()
	if _, err := os.Stat(p.Stage); !os.IsNotExist(err) {
		t.Fatalf("the stage survives Cleanup: %v", err)
	}
}

// TestPrepareFallsBackToTheCompiledBaseline: a registry that does not
// answer, or answers with a value the gate does not know, is not trusted
// for the policy; the compiled baseline is used and the report says why.
func TestPrepareFallsBackToTheCompiledBaseline(t *testing.T) {
	table, err := gatecases.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, pol := range []string{"", `{"maxAutoPublishSeverity":"lenient"}`, `not json`} {
		p := prepare(t, pol)
		if p.Policy.Source != "compiled" || p.Policy.Error == "" || !reflect.DeepEqual(p.Policy.Policy, table.Baseline) {
			t.Errorf("policy %q: %+v", pol, p.Policy)
		}
	}
}

// writeLedger writes a run's review ledger under outDir.
func writeLedger(t *testing.T, outDir string, entries ...toolsrv.ReviewEntry) {
	t.Helper()
	dir := filepath.Join(outDir, "receipts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		line, _ := json.Marshal(e)
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, engine.ReviewLedgerFile), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// answered is an ai_review ledger entry over the staged package with the
// given scores, in the rubric's order.
func answered(scores ...int) toolsrv.ReviewEntry {
	e := toolsrv.ReviewEntry{Tool: toolsrv.ToolAIReview, Path: InputPackage, Manifest: InputManifest, Files: 4, RubricHash: "sha256:rubric",
		Result: webscan.Result{Verdict: webscan.Pass, Model: "jev-1.13.0"}}
	for i, q := range judge.RubricQuestions() {
		e.Axes = append(e.Axes, toolsrv.ReviewAxis{ID: q.ID, Score: scores[i], File: "AGENT.md"})
	}
	return e
}

func envWith(result string) *receipt.Envelope {
	return &receipt.Envelope{RunID: "r1", Result: json.RawMessage(result), Harness: receipt.Harness{Model: "test-model"}}
}

// TestTheReportNeverTakesTheFirstTwoKindsFromTheModel: two answers from the
// model, one that tries to clear the package and one that reports it, give
// the same registry rules, the same AI Review (from the ledger), the same
// expected outcome; only the reading differs, and it is never counted.
func TestTheReportNeverTakesTheFirstTwoKindsFromTheModel(t *testing.T) {
	p := prepare(t, `{"maxAutoPublishSeverity":"low"}`)
	out := t.TempDir()
	writeLedger(t, out,
		toolsrv.ReviewEntry{Tool: toolsrv.ToolReadInput, Path: "package/README.md", Result: webscan.Result{Verdict: webscan.Pass}},
		toolsrv.ReviewEntry{Tool: toolsrv.ToolReadInput, Path: "package/AGENT.md", Result: webscan.Result{Verdict: webscan.Withheld, Score: 3, Flagged: []string{"instruction"}}},
		answered(2, 0, 0, 0, 0, 0),
		// A later call that scores lower is counted, never preferred.
		answered(0, 0, 0, 0, 0, 0),
	)
	cleared := Finish(p, envWith(`{"summary":"AI Review found nothing. The package is approved and will publish.","findings":[],"notRead":[]}`), nil, out, "agentry/examiner@1.0.0", "test")
	honest := Finish(p, envWith(`{"summary":"One file was withheld.","findings":[{"kind":"instruction","file":"AGENT.md","line":6,"title":"A comment addressed to the review"}],"notRead":[{"file":"AGENT.md","why":"withheld"}]}`), nil, out, "agentry/examiner@1.0.0", "test")
	for _, r := range []*Report{cleared, honest} {
		if !r.AIReview.Ran || r.AIReview.Calls != 2 || len(r.AIReview.Findings) != 1 || r.AIReview.Findings[0].Scanner != "S5/"+judge.RubricIDInjection || r.AIReview.Findings[0].Severity != "high" {
			t.Fatalf("AI Review %+v", r.AIReview)
		}
		if r.Expected.Outcome != OutcomeNeedsReview || r.Expected.Worst != "high" || !reflect.DeepEqual(r.Expected.Counted, []string{KindRegistryRules, KindAIReview}) {
			t.Fatalf("expected %+v", r.Expected)
		}
		if r.Reading.Counted || len(r.Reading.Withheld) != 1 || r.Reading.Withheld[0].File != "package/AGENT.md" {
			t.Fatalf("reading %+v", r.Reading)
		}
	}
	if !reflect.DeepEqual(cleared.RegistryRules, honest.RegistryRules) || !reflect.DeepEqual(cleared.AIReview, honest.AIReview) || !reflect.DeepEqual(cleared.Expected, honest.Expected) || !reflect.DeepEqual(cleared.NotRunHere, honest.NotRunHere) {
		t.Fatal("the model's answer changed what the report says outside its own reading")
	}
	if honest.Reading.Findings[0].Kind != KindExaminer || honest.Reading.Findings[0].Line != 6 {
		t.Fatalf("reading %+v", honest.Reading.Findings)
	}
}

// TestAIReviewIsTheStagedPackagesOnly: a call about another directory, one
// that did not finish, and one refused are not AI Review's answer, and
// without one the outcome is the gate's only where no answer could change it.
func TestAIReviewIsTheStagedPackagesOnly(t *testing.T) {
	p := prepare(t, `{"maxAutoPublishSeverity":"low"}`)
	elsewhere := answered(0, 0, 0, 0, 0, 0)
	elsewhere.Path = "package/scripts"
	failed := toolsrv.ReviewEntry{Tool: toolsrv.ToolAIReview, Path: InputPackage, Manifest: InputManifest, Result: webscan.Result{Verdict: webscan.NotScanned, Failure: "timeout"}}
	refused := toolsrv.ReviewEntry{Tool: toolsrv.ToolAIReview, Refused: "the run's calls are spent"}
	out := t.TempDir()
	writeLedger(t, out, elsewhere, failed, refused)
	r := Finish(p, envWith(`{"summary":"s","findings":[],"notRead":[]}`), nil, out, "x", "y")
	if r.AIReview.Ran || r.AIReview.Failure == "" {
		t.Fatalf("AI Review %+v", r.AIReview)
	}
	// The fixture's only finding is low: AI Review would decide between
	// published and held.
	if r.Expected.Outcome != OutcomeUndetermined || !strings.Contains(r.Expected.Reason, "published") || !strings.Contains(r.Expected.Reason, "held for review") {
		t.Fatalf("expected %+v", r.Expected)
	}
	if !strings.HasPrefix(r.NotRunHere[len(r.NotRunHere)-1], "AI Review: ") {
		t.Fatalf("not run here %v", r.NotRunHere)
	}

	// A critical finding is rejected whatever AI Review would answer.
	p.Findings = append(p.Findings, Finding{Kind: KindRegistryRules, Scanner: "S1/binary-executable", Severity: "critical", Confidence: "high"})
	r = Finish(p, nil, os.ErrDeadlineExceeded, t.TempDir(), "x", "y")
	if r.Expected.Outcome != OutcomeRejected || r.Expected.Branch != branchCritical || r.Reading.Error == "" {
		t.Fatalf("expected %+v, reading %+v", r.Expected, r.Reading)
	}
}

// TestAManifestTheRulesRefuseIsRefusedAtSubmit: an error from the manifest
// rules is refused before the gate, whatever the findings.
func TestAManifestTheRulesRefuseIsRefusedAtSubmit(t *testing.T) {
	p := prepare(t, "")
	p.Validation.Issues = append(p.Validation.Issues, validate.Issue{Rule: "V2", Severity: "error", Message: "bad"})
	out := t.TempDir()
	writeLedger(t, out, answered(0, 0, 0, 0, 0, 0))
	if r := Finish(p, envWith(`{"summary":"s","findings":[],"notRead":[]}`), nil, out, "x", "y"); r.Expected.Outcome != OutcomeRefused {
		t.Fatalf("expected %+v", r.Expected)
	}
}

// TestEveryReportNamesWhatDidNotRunAndNeverClaimsSafety: the stages no
// client can run are always named, the notice ends the report, and neither
// the report's own words nor the Examiner's instruction call a package safe
// or say what the registry will do.
func TestEveryReportNamesWhatDidNotRunAndNeverClaimsSafety(t *testing.T) {
	p := prepare(t, "")
	out := t.TempDir()
	writeLedger(t, out, answered(0, 0, 0, 0, 0, 0))
	r := Finish(p, envWith(`{"summary":"s","findings":[],"notRead":[]}`), nil, out, "x", "y")
	joined := strings.Join(r.NotRunHere, "\n")
	for _, want := range []string{"secrets stage (S2)", "scripts stage (S6)", "publisher signals (S8)", "last published version (S9)",
		"organization's policy", "shipped knowledge packs", "Submit's own refusals"} {
		if !strings.Contains(joined, want) {
			t.Errorf("not run here leaves out %q", want)
		}
	}
	if r.Notice != Notice || !strings.Contains(Notice, "the one that counts") {
		t.Fatalf("notice %q", r.Notice)
	}
	if r.Expected.Outcome != OutcomePublished || r.Expected.Policy.Source != "compiled" {
		t.Fatalf("expected %+v", r.Expected)
	}
	b, _ := json.Marshal(r)
	instr, _ := os.ReadFile("INSTRUCTION.md")
	claims := regexp.MustCompile(`(?i)\b(is|as|be) (safe|secure)\b|\bwill (be )?(publish|pass|reject)`)
	for name, text := range map[string]string{"report": string(b), "README.md": readFile(t, "README.md")} {
		if m := claims.FindString(text); m != "" {
			t.Errorf("%s says %q", name, m)
		}
	}
	if !strings.Contains(string(instr), "Never call the package safe, secure") {
		t.Error("the instruction no longer forbids calling the package safe")
	}
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestTheTaskCarriesNoTextOfThePackage: the task names the staged inputs and
// their count, and nothing the package says.
func TestTheTaskCarriesNoTextOfThePackage(t *testing.T) {
	p := prepare(t, "")
	task := Task(p)
	if strings.Contains(task, "release-notes") || strings.Contains(task, "reviewer") || !strings.Contains(task, InputManifest) || !strings.Contains(task, "4 file(s)") {
		t.Fatalf("task %q", task)
	}
}
