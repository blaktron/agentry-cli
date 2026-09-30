package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/drafter"
	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/knowledge/build"
	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// savePolicyBudgets writes the default policy with these budgets.
func savePolicyBudgets(t *testing.T, policyPath string, b policy.Budgets) {
	t.Helper()
	pol := policy.Default()
	pol.Budgets = b
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
}

// TestADraftOnThisComputerGetsThirtyMinutes (plan scan-and-run-tweaks.md
// §6.1, D14; agentry-cli#500): the Drafter's run is given the longer of the
// policy's timeout and 1,800 seconds; --timeout still wins, shorter or
// longer; the tool-call and cost caps stay the policy's; and a hosted
// draft, whose caps travel in its job, is given no floor.
func TestADraftOnThisComputerGetsThirtyMinutes(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	t.Setenv(policy.DefaultJudgeKeyEnv, testJev)
	for _, tc := range []struct {
		name   string
		policy policy.Budgets
		hosted bool
		args   []string
		want   spec.Budgets
	}{
		{"the default policy's 900 seconds", policy.Default().Budgets, false, nil, spec.Budgets{TimeoutSeconds: 1800, MaxTurns: 60}},
		{"a policy that allows more keeps its own", policy.Budgets{TimeoutSeconds: 2400, MaxTurns: 40, MaxCostUSD: 3}, false, nil,
			spec.Budgets{TimeoutSeconds: 2400, MaxTurns: 40, MaxCostUSD: 3}},
		{"a shorter --timeout wins", policy.Default().Budgets, false, []string{"--timeout", "600"}, spec.Budgets{TimeoutSeconds: 600, MaxTurns: 60}},
		{"a longer --timeout wins", policy.Default().Budgets, false, []string{"--timeout", "5400", "--max-turns", "0", "--max-cost", "1.5"},
			spec.Budgets{TimeoutSeconds: 5400, MaxTurns: 0, MaxCostUSD: 1.5}},
		{"a hosted draft with no --timeout has the policy's", policy.Default().Budgets, true, nil, spec.Budgets{TimeoutSeconds: 900, MaxTurns: 60}},
		{"a hosted draft runs under its job's caps", policy.Default().Budgets, true, []string{"--timeout", "1200", "--max-turns", "300"},
			spec.Budgets{TimeoutSeconds: 1200, MaxTurns: 300}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			savePolicyBudgets(t, policyPath, tc.policy)
			_, runs, _ := draftSeams(t, webscan.Result{Verdict: webscan.Pass}, goodDraft, "")
			args := append([]string{"draft", "Summarize my notes.", "--out", filepath.Join(t.TempDir(), "o"), "--policy", policyPath}, tc.args...)
			if tc.hosted {
				t.Setenv(envJobToken, "agyj_the-job")
				args = append(args, "--hosted")
			}
			if _, stderr, err := runCLI(t, nil, args...); err != nil {
				t.Fatalf("%v\n%s", err, stderr)
			}
			if len(*runs) != 1 || (*runs)[0].Budgets == nil {
				t.Fatalf("%d run(s) of the Drafter, or one with no budgets of its own", len(*runs))
			}
			if got := *(*runs)[0].Budgets; got != tc.want {
				t.Errorf("the Drafter ran under %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestTheRepairTurnIsBoundWithinWhatTheDraftHasLeft: the repair turn keeps
// its own 300 seconds inside the draft's budget, which is now 30 minutes on
// this computer. A first turn that ran past the policy's 900 seconds still
// has its repair turn; one that left less than 300 seconds leaves the
// repair turn that much; one that spent the 30 minutes leaves none.
func TestTheRepairTurnIsBoundWithinWhatTheDraftHasLeft(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	t.Setenv(policy.DefaultJudgeKeyEnv, testJev)
	for _, tc := range []struct {
		name         string
		firstSeconds int64
		repair       int // the repair run's timeout; 0 = no repair run
	}{
		{"past the policy's 900 seconds", 1000, drafter.RepairTimeoutSeconds},
		{"100 seconds left", 1700, 100},
		{"nothing left", 1800, 0},
	} {
		_, runs, _ := draftSeamsN(t, webscan.Result{Verdict: webscan.Pass}, []string{refusedChoice, goodDraft}, "")
		seam := draftRun
		draftRun = func(cmd *cobra.Command, r localrun.Run) (*receipt.Envelope, error) {
			env, err := seam(cmd, r)
			if r.Web != nil {
				env.Usage.WallMS = tc.firstSeconds * 1000
			}
			return env, err
		}
		if _, stderr, err := runCLI(t, nil, "draft", "Summarize my notes.", "--out", filepath.Join(t.TempDir(), "o"), "--policy", policyPath); err != nil {
			t.Fatalf("%s: %v\n%s", tc.name, err, stderr)
		}
		if len(*runs) == 0 || (*runs)[0].Budgets == nil || (*runs)[0].Budgets.TimeoutSeconds != drafter.LocalTimeoutFloorSeconds {
			t.Fatalf("%s: %d run(s); the first turn did not run under the draft's 30 minutes", tc.name, len(*runs))
		}
		if tc.repair == 0 {
			if len(*runs) != 1 {
				t.Errorf("%s: a repair turn ran with nothing left of the draft's time", tc.name)
			}
			continue
		}
		if len(*runs) != 2 || (*runs)[1].Budgets == nil {
			t.Fatalf("%s: %d run(s), want the draft and its repair turn", tc.name, len(*runs))
		}
		if b := *(*runs)[1].Budgets; b.TimeoutSeconds != tc.repair || b.MaxTurns > drafter.RepairMaxTurns {
			t.Errorf("%s: the repair turn ran under %+v, want %d seconds", tc.name, b, tc.repair)
		}
	}
}

// creatorBuilds replaces the Creator's build with one that records the
// options each build was given and answers made, a pack laid down in the
// build's directory, or err.
func creatorBuilds(t *testing.T, env *receipt.Envelope, err error) *[]build.Options {
	t.Helper()
	var got []build.Options
	prev := creatorBuild
	t.Cleanup(func() { creatorBuild = prev })
	creatorBuild = func(_ context.Context, _ dockerx.Client, o build.Options) (*build.Result, error) {
		got = append(got, o)
		if err != nil {
			return nil, err
		}
		if o.OnUsage != nil {
			o.OnUsage(env.Usage)
		}
		made := layDownTestPack(t, o.Name, o.PackDir)
		return &build.Result{Envelope: env, Applied: &knowledge.Result{Manifest: made, Written: []string{"wiki/topics/a.md"}},
			Documents: []string{"notes.md"}}, nil
	}
	return &got
}

// TestAKnowledgeBuildOnThisComputerGetsSixtyMinutes (plan
// scan-and-run-tweaks.md §6.2, D13; agentry-cli#500): the Creator's run is
// given the longer of the policy's timeout and 3,600 seconds, for create
// and for add; --timeout still wins, which is how a sandbox host's job
// carries its own caps; the tool-call and cost caps stay the policy's.
func TestAKnowledgeBuildOnThisComputerGetsSixtyMinutes(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	doc := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(doc, []byte("# notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("the test stops the build here")
	for _, tc := range []struct {
		name   string
		policy policy.Budgets
		args   []string
		want   spec.Budgets
	}{
		{"the default policy's 900 seconds", policy.Default().Budgets, nil, spec.Budgets{TimeoutSeconds: 3600, MaxTurns: 60}},
		{"a policy at the old 1,800 seconds", policy.Budgets{TimeoutSeconds: 1800, MaxTurns: 60}, nil, spec.Budgets{TimeoutSeconds: 3600, MaxTurns: 60}},
		{"a policy that allows more keeps its own", policy.Budgets{TimeoutSeconds: 7200, MaxTurns: 0, MaxCostUSD: 4}, nil,
			spec.Budgets{TimeoutSeconds: 7200, MaxTurns: 0, MaxCostUSD: 4}},
		{"a shorter --timeout wins", policy.Default().Budgets, []string{"--timeout", "900"}, spec.Budgets{TimeoutSeconds: 900, MaxTurns: 60}},
		{"a sandbox host's caps for a Standard build", policy.Default().Budgets, []string{"--timeout", "900", "--max-turns", "300", "--output-format", "json"},
			spec.Budgets{TimeoutSeconds: 900, MaxTurns: 300}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			savePolicyBudgets(t, policyPath, tc.policy)
			got := creatorBuilds(t, nil, stop)
			dir := filepath.Join(t.TempDir(), "notes")
			args := append([]string{"knowledge", "create", "notes", "--dir", dir, "--file", doc, "--out", filepath.Join(t.TempDir(), "out"), "--policy", policyPath}, tc.args...)
			if _, _, err := runCLI(t, nil, args...); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			if len(*got) != 1 || (*got)[0].Budgets == nil {
				t.Fatalf("%d build(s) of the Creator, or one with no budgets of its own", len(*got))
			}
			if b := *(*got)[0].Budgets; b != tc.want {
				t.Errorf("the Creator ran under %+v, want %+v", b, tc.want)
			}
		})
	}

	// add takes the same budgets as create.
	savePolicyBudgets(t, policyPath, policy.Budgets{TimeoutSeconds: 900, MaxTurns: 25, MaxCostUSD: 1})
	p := knowledgetest.Make(t, "notes", nil)
	got := creatorBuilds(t, nil, stop)
	if _, _, err := runCLI(t, nil, "knowledge", "add", p.Dir, "--file", doc, "--out", filepath.Join(t.TempDir(), "out"), "--policy", policyPath); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if len(*got) != 1 || (*got)[0].Budgets == nil {
		t.Fatalf("%d build(s) of the Creator from add, or one with no budgets of its own", len(*got))
	}
	if b, want := *(*got)[0].Budgets, (spec.Budgets{TimeoutSeconds: 3600, MaxTurns: 25, MaxCostUSD: 1}); b != want {
		t.Fatalf("add ran the Creator under %+v, want %+v", b, want)
	}
}

// The two sentences that tell a person a built-in's run on this computer
// costs no credits: `draft`'s "for no credits" and `knowledge`'s "It costs
// no credits". They say the run is not metered in credits at all, which is
// the rule's point, so the narration may carry them; the test below does
// not require them, and nothing else may use the word.
var costsNoCredits = regexp.MustCompile(`(?i)\bfor no credits\b|\bit costs no credits\b`)

// plainTemp is a temporary directory whose path does not carry the test's
// name, for a test that reads its output for a word its own name holds.
func plainTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "local-run-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// wordedInCredits reports whether text uses the word at all.
func wordedInCredits(text string) bool {
	return strings.Contains(strings.ToLower(text), "credit")
}

// narrationInCredits is wordedInCredits for what a command says on stderr,
// where alone a built-in may say that its run costs no credits.
func narrationInCredits(stderr string) bool {
	return wordedInCredits(costsNoCredits.ReplaceAllString(stderr, ""))
}

// TestALocalRunReportsTokensAndNeverCredits (plan scan-and-run-tweaks.md
// invariant 10; agentry-cli#500): a run to the person's own model provider
// reports its usage in tokens, in the envelope and in the narration a
// driver reads, and is never translated into credits. Credits are what a
// hosted run reports, and the registry works those out from this same
// narration; the CLI never does. Stdout, where the envelope and the report
// go, never uses the word; stderr uses it only to say the run costs none.
// The text form prints no usage at all, so its token counts are the json
// form's to show. `agentry run` has no seam for a fake docker client at
// the command: its half is the engine's, TestALocalRunsOutputIsInTokens,
// with the narration line here and the envelope's shape in
// TestNoReceiptFieldIsNamedInCredits.
func TestALocalRunReportsTokensAndNeverCredits(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	t.Setenv(policy.DefaultJudgeKeyEnv, testJev)
	usage := receipt.Usage{InputTokens: 120_000, OutputTokens: 9_000, Turns: 12, CostUSD: 0.42, WallMS: 61_000}
	tokensIn := func(t *testing.T, stdout string) {
		t.Helper()
		raw, ok := runexecLastJSONObject(stdout)
		if !ok {
			t.Fatalf("no envelope on stdout:\n%s", stdout)
		}
		var env struct {
			Usage map[string]json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
		if string(env.Usage["inputTokens"]) != "120000" || string(env.Usage["outputTokens"]) != "9000" {
			t.Fatalf("the envelope's usage is %s", raw)
		}
	}
	narrated := "usage: input=120000 output=9000 total=129000 turns=12\n"

	t.Run("a draft", func(t *testing.T) {
		for _, format := range []string{"text", "json"} {
			draftSeams(t, webscan.Result{Verdict: webscan.Pass}, goodDraft, draftLedger)
			seam := draftRun
			draftRun = func(cmd *cobra.Command, r localrun.Run) (*receipt.Envelope, error) {
				if r.OnUsage != nil {
					r.OnUsage(usage)
				}
				env, err := seam(cmd, r)
				env.Usage = usage
				return env, err
			}
			stdout, stderr, err := runCLI(t, nil, "draft", "Summarize my notes.", "--out", filepath.Join(plainTemp(t), "o"), "--policy", policyPath, "--output-format", format)
			if err != nil {
				t.Fatalf("%v\n%s", err, stderr)
			}
			if wordedInCredits(stdout) || narrationInCredits(stderr) {
				t.Errorf("%s: a local draft's output is worded in credits:\n%s\n%s", format, stdout, stderr)
			}
			if format == "json" {
				tokensIn(t, stdout)
				if !strings.Contains(stderr, narrated) {
					t.Errorf("the narration carries no token line:\n%s", stderr)
				}
			}
		}
	})
	t.Run("a knowledge build", func(t *testing.T) {
		doc := filepath.Join(plainTemp(t), "notes.md")
		if err := os.WriteFile(doc, []byte("# notes\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, format := range []string{"text", "json"} {
			tmp := plainTemp(t)
			creatorBuilds(t, &receipt.Envelope{Agentry: receipt.EnvelopeVersion, Status: "ok", RunID: "r1", Ref: localrun.KnowledgeCreator().Ref(),
				Attestation: receipt.Attestation{State: "builtin"}, Usage: usage, Result: receipt.TextResult("done")}, nil)
			stdout, stderr, err := runCLI(t, nil, "knowledge", "create", "notes", "--dir", filepath.Join(tmp, "notes"), "--file", doc,
				"--out", filepath.Join(tmp, "out"), "--policy", policyPath, "--output-format", format)
			if err != nil {
				t.Fatalf("%v\n%s", err, stderr)
			}
			if wordedInCredits(stdout) || narrationInCredits(stderr) {
				t.Errorf("%s: a local build's output is worded in credits:\n%s\n%s", format, stdout, stderr)
			}
			if format == "json" {
				tokensIn(t, stdout)
				if !strings.Contains(stderr, narrated) {
					t.Errorf("the narration carries no token line:\n%s", stderr)
				}
			}
		}
	})
	t.Run("the narration line", func(t *testing.T) {
		line := usageNarration(usage)
		if line != strings.TrimSuffix(narrated, "\n") || wordedInCredits(line) {
			t.Fatalf("the narration is %q", line)
		}
	})
}
