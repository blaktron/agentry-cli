package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/examiner"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
	"github.com/blaktron/agentry/pkg/judge"
)

const reviewFixture = "../../pkg/examiner/testdata/instructs-the-examiner"

// reviewSeams replaces the rule-bundle fetch and the run: the run records
// what it was given, writes a review ledger with these scores (none when
// scores is nil) and answers with result. It returns the runs and a
// registry that answers the baseline policy.
func reviewSeams(t *testing.T, scores []int, result string) (*[]localrun.Run, string) {
	t.Helper()
	var runs []localrun.Run
	prevRun, prevUpdate := reviewRun, reviewRulesUpdate
	t.Cleanup(func() { reviewRun, reviewRulesUpdate = prevRun, prevUpdate })
	reviewRulesUpdate = func(*cobra.Command) error { return errors.New("no registry in tests") }
	reviewRun = func(_ *cobra.Command, r localrun.Run) (*receipt.Envelope, error) {
		runs = append(runs, r)
		if err := os.MkdirAll(filepath.Join(r.OutDir, "receipts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if scores != nil {
			e := toolsrv.ReviewEntry{Tool: toolsrv.ToolAIReview, Dir: examiner.InputPackage, Manifest: examiner.InputManifest, Files: 4,
				RubricHash: "sha256:r", Result: webscan.Result{Verdict: webscan.Pass, Model: "jev-1.13.0"}}
			for i, q := range judge.RubricQuestions() {
				e.Axes = append(e.Axes, toolsrv.ReviewAxis{ID: q.ID, Score: scores[i], File: "AGENT.md"})
			}
			b, _ := json.Marshal(e)
			if err := os.WriteFile(filepath.Join(r.OutDir, "receipts", engine.ReviewLedgerFile), append(b, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return &receipt.Envelope{Status: "ok", RunID: r.RunID, Result: json.RawMessage(result), Harness: receipt.Harness{Model: "openai/test"}}, nil
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/platform/policy" {
			_, _ = io.WriteString(w, `{"maxAutoPublishSeverity":"low","requireHumanReview":"never"}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return &runs, srv.URL
}

func TestReviewRefusesWithoutAJevKey(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	t.Setenv(policy.DefaultJudgeKeyEnv, "")
	runs, reg := reviewSeams(t, []int{0, 0, 0, 0, 0, 0}, `{"summary":"s","findings":[],"notRead":[]}`)
	_, _, err := runCLI(t, nil, "--registry", reg, "review", reviewFixture, "--policy", policyPath)
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "$TYPESAFE_API_KEY") || !strings.Contains(err.Error(), "provider:typesafe") {
		t.Fatalf("the refusal does not say where to add a key: %v", err)
	}
	if len(*runs) != 0 {
		t.Fatal("the Examiner ran without a key")
	}
}

// TestReviewRunsTheExaminerAndExitsByTheOutcome: the Examiner runs with the
// review grant, the person's key, a read-only copy of the package and a task
// that carries none of its text; the report is written, printed with the
// model's words cleaned, and the exit says the outcome.
func TestReviewRunsTheExaminerAndExitsByTheOutcome(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	t.Setenv(policy.DefaultJudgeKeyEnv, testJev)
	runs, reg := reviewSeams(t, []int{2, 0, 0, 0, 0, 0},
		`{"summary":"Read three files.\u001b[31m","findings":[{"kind":"instruction","file":"AGENT.md","line":6,"title":"A comment addressed to the review"}],"notRead":[]}`)
	out := filepath.Join(t.TempDir(), "review-out")
	stdout, stderr, err := runCLI(t, nil, "--registry", reg, "review", reviewFixture, "--out", out, "--policy", policyPath)
	wantClassified(t, err, exitReviewHeld)
	if len(*runs) != 1 {
		t.Fatalf("%d runs", len(*runs))
	}
	r := (*runs)[0]
	if r.Builtin.Ref() != localrun.Examiner().Ref() || r.Review == nil || r.Review.JevKey != testJev || r.Web != nil || len(r.Inputs) != 2 || r.OutDir != out {
		t.Fatalf("run %+v", r)
	}
	task, _ := io.ReadAll(r.Task)
	if strings.Contains(string(task), "release-notes") || !strings.Contains(string(task), "ai_review") {
		t.Fatalf("task %q", task)
	}
	var rep examiner.Report
	b, err := os.ReadFile(filepath.Join(out, examiner.ReportFile))
	if err != nil || json.Unmarshal(b, &rep) != nil {
		t.Fatalf("report: %v %s", err, b)
	}
	if rep.Expected.Outcome != examiner.OutcomeNeedsReview || !rep.AIReview.Ran || rep.Reading.Counted || rep.Expected.Policy.Source != "registry" {
		t.Fatalf("report %+v", rep)
	}
	for _, want := range []string{"expected:  held for review", "S3/html-comment AGENT.md:6", "S5/C1_injection", "not counted in the outcome",
		"instruction AGENT.md:6", "secrets stage (S2)", examiner.Notice} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout leaves out %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "\x1b") {
		t.Fatal("the model's control characters reached the terminal")
	}
	if !strings.Contains(stderr, "Nothing of the QuickAgent runs") || !strings.Contains(stderr, "rule bundle could not be fetched") {
		t.Fatalf("stderr %s", stderr)
	}
}

func TestReviewExitCodes(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	t.Setenv(policy.DefaultJudgeKeyEnv, testJev)
	for _, tc := range []struct {
		scores []int
		code   int
	}{
		{[]int{0, 0, 0, 0, 0, 0}, 0},
		{[]int{0, 3, 0, 0, 0, 0}, exitReviewHeld},
		{nil, exitReviewUndetermined},
	} {
		_, reg := reviewSeams(t, tc.scores, `{"summary":"s","findings":[],"notRead":[]}`)
		stdout, _, err := runCLI(t, nil, "--registry", reg, "--json", "review", reviewFixture, "--out", filepath.Join(t.TempDir(), "o"), "--policy", policyPath)
		if tc.code == 0 {
			if err != nil {
				t.Fatalf("%v: %v", tc.scores, err)
			}
		} else {
			wantClassified(t, err, tc.code)
		}
		var rep examiner.Report
		if json.Unmarshal([]byte(stdout), &rep) != nil || rep.SchemaVersion != examiner.ReportSchemaVersion {
			t.Fatalf("--json: %s", stdout)
		}
	}
}

func TestReviewRefusesAFolderWithNoManifest(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	t.Setenv(policy.DefaultJudgeKeyEnv, testJev)
	runs, reg := reviewSeams(t, nil, `{}`)
	_, _, err := runCLI(t, nil, "--registry", reg, "review", t.TempDir(), "--policy", policyPath)
	wantClassified(t, err, engine.ExitUsage)
	if len(*runs) != 0 {
		t.Fatal("ran")
	}
}

// TestReviewRefusesAFolderThatHoldsReceipts: the post-step reads AI Review
// from the run's ledger in the output folder, so a folder that already
// holds a run's receipts is refused, and another run's ledger is never read.
func TestReviewRefusesAFolderThatHoldsReceipts(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	t.Setenv(policy.DefaultJudgeKeyEnv, testJev)
	runs, reg := reviewSeams(t, nil, `{}`)
	out := t.TempDir()
	if err := os.MkdirAll(filepath.Join(out, "receipts"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := runCLI(t, nil, "--registry", reg, "review", reviewFixture, "--out", out, "--policy", policyPath)
	wantClassified(t, err, engine.ExitUsage)
	if len(*runs) != 0 {
		t.Fatal("ran")
	}
}
