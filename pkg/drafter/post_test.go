package drafter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
	"github.com/blaktron/agentry/pkg/draftcheck"
	"github.com/blaktron/agentry/pkg/manifest"
)

// postDraft is a first answer the rules refuse (a choice question with no
// choices, which no correction can fill without inventing them), carrying
// in every field but its manifest and instructions a marker that must never
// reach the repair turn.
const postDraft = `{
  "manifest": {"schemaVersion": "2", "name": "meeting-actions", "namespace": "draft", "version": "0.1.0",
    "summary": "Turns meeting notes into decisions and action items.", "license": "Apache-2.0",
    "modes": ["autonomous"], "entrypoints": {"instruction": "AGENT.md"},
    "intake": [{"id": "format", "prompt": "Which format?", "type": "choice"}],
    "capabilities": {"network": {"egress": "none"}, "filesystem": {"read": "none", "write": "none"}, "exec": "none"},
    "publish": {"include": ["agent.yaml", "AGENT.md", "README.md", "LICENSE"]}},
  "instructions": "# Meeting actions\n\nRead the notes under /inputs and list the decisions and the actions.\n",
  "notes": "NOTES-MARKER", "matches": [{"ref": "acme/MATCH-MARKER", "why": "WHY-MARKER"}],
  "sources": [{"url": "https://SOURCE-MARKER.example/", "used": "USED-MARKER"}], "openQuestions": ["QUESTION-MARKER?"]
}`

// postRepaired is the repair turn's answer: the manifest again without
// the refused question, its lists empty.
const postRepaired = `{
  "manifest": {"schemaVersion": "2", "name": "meeting-actions", "namespace": "draft", "version": "0.1.0",
    "summary": "Turns meeting notes into decisions and action items.", "license": "Apache-2.0",
    "modes": ["autonomous"], "entrypoints": {"instruction": "AGENT.md"},
    "capabilities": {"network": {"egress": "none"}, "filesystem": {"read": "none", "write": "none"}, "exec": "none"},
    "publish": {"include": ["agent.yaml", "AGENT.md", "README.md", "LICENSE"]}},
  "matches": [], "sources": [], "openQuestions": []
}`

const postLedger = `{"tool":"web_read","source":"page","url":"https://WITHHELD-MARKER.example/","verdict":"withheld","score":3,"flagged":["injection"]}
`

// firstTurn is the first turn's envelope over result, with its ledger in
// out's receipts.
func firstTurn(t *testing.T, out, result string) *receipt.Envelope {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(out, "receipts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "receipts", engine.WebLedgerFile), []byte(postLedger), 0o600); err != nil {
		t.Fatal(err)
	}
	return &receipt.Envelope{Status: "ok", RunID: "run-1", Result: json.RawMessage(result),
		Usage:   receipt.Usage{InputTokens: 1000, Turns: 3, WallMS: 20000},
		Harness: receipt.Harness{Model: "openai/qwen3.8-flash-next"},
		Web:     &receipt.Web{Scan: "jev", Read: 1, Withheld: 1, Model: "jev-1.13.0", QuestionSet: "sha256:q"}}
}

type postChecker struct{ asked []draftcheck.Package }

func (c *postChecker) Check(_ context.Context, p draftcheck.Package) (*Check, error) {
	c.asked = append(c.asked, p)
	return &Check{Model: "jev-1.13.0", Requests: 5, InputTokens: 4000,
		Rubric: &CheckRubric{Verdict: CheckPass, Findings: []CheckFinding{}, RubricHash: "sha256:r"},
		Draft:  &CheckDraft{Verdict: CheckPass, Flagged: []string{}, QuestionSet: "sha256:d"}}, nil
}

// TestThePostStepsRepairTurnIsGivenTheRulesReportAndItsOwnDraftAlone holds,
// at the seam both clients call, the invariant the repair turn keeps: a draft
// the rules refuse gets one turn whose input is the rules' own report and the
// manifest and instructions the Drafter wrote, and nothing else — not the
// description, the task, the notes, a match, a source, an open question or a
// withheld address; its second answer is validated and checked again.
func TestThePostStepsRepairTurnIsGivenTheRulesReportAndItsOwnDraftAlone(t *testing.T) {
	out := t.TempDir()
	checker := &postChecker{}
	var turns []RepairTurn
	p := PostStep{
		OutDir: out, Description: "DESCRIPTION-MARKER: summarize my notes.", Task: "TASK-MARKER: summarize my notes.",
		Scan: webscan.Result{Verdict: webscan.Pass}, Ref: "agentry/drafter@1.1.1", Build: "test",
		Budgets: spec.Budgets{TimeoutSeconds: 900, MaxTurns: 60}, Checker: checker,
		Repair: func(_ context.Context, rt RepairTurn) (*receipt.Envelope, error) {
			turns = append(turns, rt)
			return &receipt.Envelope{Status: "ok", RunID: rt.RunID, Result: json.RawMessage(postRepaired), Usage: receipt.Usage{InputTokens: 200, Turns: 1}}, nil
		},
	}
	d, env, err := p.Run(context.Background(), firstTurn(t, out, postDraft), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("%d repair turn(s)", len(turns))
	}
	rt := turns[0]
	if rt.OutDir != filepath.Join(out, "repair") || rt.RunID == "" || rt.RunID == "run-1" ||
		rt.Budgets.MaxTurns != RepairMaxTurns || rt.Budgets.TimeoutSeconds != RepairTimeoutSeconds {
		t.Fatalf("the repair turn %+v", rt)
	}
	if !strings.HasPrefix(rt.Task, "This is your one repair turn.") ||
		!strings.Contains(rt.Task, "- V10 intake[0].choices: type choice requires at least one choice\n") ||
		!strings.Contains(rt.Task, `"name": "meeting-actions"`) || !strings.Contains(rt.Task, "Read the notes under /inputs") {
		t.Fatalf("the repair task:\n%s", rt.Task)
	}
	for _, not := range []string{"DESCRIPTION-MARKER", "TASK-MARKER", "NOTES-MARKER", "MATCH-MARKER", "WHY-MARKER",
		"SOURCE-MARKER", "USED-MARKER", "QUESTION-MARKER", "WITHHELD-MARKER"} {
		if strings.Contains(rt.Task, not) {
			t.Errorf("the repair task carries %q", not)
		}
	}
	if !d.IsRepaired() || !d.Passed() || len(d.Matches) != 1 || len(d.AIReview.WithheldSources) != 1 {
		t.Fatalf("the draft %+v, validation %s", d, d.Validation)
	}
	if c := d.AIReview.Check; c == nil || c.Checks != 2 || c.Verdict() != CheckPass || len(checker.asked) != 2 ||
		checker.asked[1].Description != p.Description {
		t.Fatalf("the check %+v, asked %d", d.AIReview.Check, len(checker.asked))
	}
	if env.Usage.InputTokens != 1200 || env.Usage.Turns != 4 {
		t.Fatalf("usage %+v", env.Usage)
	}
	if _, err := os.Stat(filepath.Join(out, "package", "agent.yaml")); err != nil {
		t.Fatal("the second answer is not rendered")
	}
}

// flagChecker flags the first answer off_task and passes the second.
type flagChecker struct{ postChecker }

func (c *flagChecker) Check(ctx context.Context, p draftcheck.Package) (*Check, error) {
	ch, err := c.postChecker.Check(ctx, p)
	if len(c.asked) == 1 {
		ch.Draft = &CheckDraft{Verdict: CheckFlagged, Score: 3, Scores: map[string]int{"off_task": 3}, Flagged: []string{"off_task"}, QuestionSet: "sha256:d"}
	}
	return ch, err
}

// TestAFlaggedDraftsRepairTurnIsAlsoGivenTheTask: a draft the rules pass
// and AI Review's check flags gets the one repair turn with what the check
// flagged and the task the first turn was given, which AI Review passed
// before that turn, and still nothing else of the first turn's.
func TestAFlaggedDraftsRepairTurnIsAlsoGivenTheTask(t *testing.T) {
	out := t.TempDir()
	var tasks []string
	passing := strings.Replace(postDraft, `"intake": [{"id": "format", "prompt": "Which format?", "type": "choice"}],`, "", 1)
	p := PostStep{
		OutDir: out, Description: "DESCRIPTION-MARKER", Task: "TASK-MARKER: summarize my notes.",
		Scan: webscan.Result{Verdict: webscan.Pass}, Ref: "agentry/drafter@1.1.1", Checker: &flagChecker{},
		Repair: func(_ context.Context, rt RepairTurn) (*receipt.Envelope, error) {
			tasks = append(tasks, rt.Task)
			return &receipt.Envelope{Status: "ok", Result: json.RawMessage(postRepaired)}, nil
		},
	}
	d, _, err := p.Run(context.Background(), firstTurn(t, out, passing), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || !strings.Contains(tasks[0], "## What AI Review flagged\n\n- AI Review off_task, score 3:") ||
		!strings.Contains(tasks[0], "## The task you were given\n\nTASK-MARKER") || strings.Contains(tasks[0], "## The rules' report") {
		t.Fatalf("the repair task(s):\n%s", strings.Join(tasks, "\n---\n"))
	}
	for _, not := range []string{"DESCRIPTION-MARKER", "NOTES-MARKER", "MATCH-MARKER", "SOURCE-MARKER", "QUESTION-MARKER", "WITHHELD-MARKER"} {
		if strings.Contains(tasks[0], not) {
			t.Errorf("the repair task carries %q", not)
		}
	}
	if !d.IsRepaired() || d.AIReview.Check.Checks != 2 || d.AIReview.Check.Verdict() != CheckPass {
		t.Fatalf("the draft's check %+v", d.AIReview.Check)
	}
}

// TestAnInstructionPastTheCapGetsTheRepairTurn: a draft the rules pass
// whose instructions are past the Drafter's cap gets the one repair turn,
// told the size and the cap, and the shortened instructions it writes back
// are kept.
func TestAnInstructionPastTheCapGetsTheRepairTurn(t *testing.T) {
	out := t.TempDir()
	passing := strings.Replace(postDraft, `"intake": [{"id": "format", "prompt": "Which format?", "type": "choice"}],`, "", 1)
	long := "# Meeting actions\n\n## Background\n\n" + strings.Repeat("A decision is a choice the meeting made. ", 1000)
	passing = strings.Replace(passing, `"instructions": "# Meeting actions\n\nRead the notes under /inputs and list the decisions and the actions.\n"`,
		`"instructions": "`+strings.ReplaceAll(long, "\n", `\n`)+`"`, 1)
	short := "# Meeting actions\n\n## Role\n\nLists decisions.\n"
	var tasks []string
	p := PostStep{
		OutDir: out, Description: "Summarize my notes.", Task: "Summarize my notes.",
		Scan: webscan.Result{Verdict: webscan.Pass}, Ref: Namespace + "/" + Name + "@" + Version, Checker: &postChecker{},
		Repair: func(_ context.Context, rt RepairTurn) (*receipt.Envelope, error) {
			tasks = append(tasks, rt.Task)
			if err := os.MkdirAll(filepath.Join(rt.OutDir, "reports"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rt.OutDir, "reports", instructionsReport), []byte(short), 0o600); err != nil {
				t.Fatal(err)
			}
			return &receipt.Envelope{Status: "ok", Result: json.RawMessage(postRepaired)}, nil
		},
	}
	d, _, err := p.Run(context.Background(), firstTurn(t, out, passing), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(long) <= MaxInstructionBytes {
		t.Fatalf("the instructions are %d bytes, not past the cap", len(long))
	}
	want := fmt.Sprintf("- the instructions: %d bytes, past the Drafter's cap of %d bytes;", len(long), MaxInstructionBytes)
	if len(tasks) != 1 || !strings.Contains(tasks[0], "## The rules' report\n\n"+want) {
		t.Fatalf("the repair task(s):\n%.600s", strings.Join(tasks, "\n---\n"))
	}
	if !d.IsRepaired() || d.Instructions != short || !strings.Contains(d.Notes, "the instructions: ") {
		t.Fatalf("the draft: repaired %v, %d bytes of instructions, notes %q", d.IsRepaired(), len(d.Instructions), d.Notes)
	}
	// Within the cap, nothing is sent back.
	if lines, refused := rulesReport(&Draft{Instructions: strings.Repeat("x", MaxInstructionBytes)}); refused || len(lines) != 0 {
		t.Fatalf("an instruction at the cap: %v", lines)
	}
}

// TestAPostStepWithNoCheckerAndNoRepairSaysSo: a client that could make no
// checker, or runs no repair turn, gets a draft that says so, never one
// that reads as checked or as refused with nothing tried.
func TestAPostStepWithNoCheckerAndNoRepairSaysSo(t *testing.T) {
	out := t.TempDir()
	p := PostStep{OutDir: out, Description: "Summarize my notes.", Scan: webscan.Result{Verdict: webscan.Pass}, Ref: "agentry/drafter@1.1.1"}
	d, _, err := p.Run(context.Background(), firstTurn(t, out, postDraft), nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Passed() || d.IsRepaired() || !strings.Contains(d.Notes, "## Not repaired") || !strings.Contains(d.Notes, "this client runs no repair turn") {
		t.Fatalf("notes:\n%s", d.Notes)
	}
	if c := d.AIReview.Check; c == nil || c.Unanswered != "this client asks AI Review for no check" || c.Checks != 0 {
		t.Fatalf("the check %+v", d.AIReview.Check)
	}
}

// TestTheRepairTurnHasOnlyWhatTheFirstLeft: the repair turn's time, tool
// calls and cost are what the first turn left of the draft's, under its own
// caps, and none is made once the first turn spent any of them.
func TestTheRepairTurnHasOnlyWhatTheFirstLeft(t *testing.T) {
	b := spec.Budgets{TimeoutSeconds: 900, MaxTurns: 60, MaxCostUSD: 1}
	r, ok := repairBudgets(b, receipt.Usage{WallMS: 120000, Turns: 20, CostUSD: 0.25})
	if !ok || r.TimeoutSeconds != RepairTimeoutSeconds || r.MaxTurns != RepairMaxTurns || r.MaxCostUSD != 0.75 {
		t.Fatalf("%+v", r)
	}
	r, ok = repairBudgets(b, receipt.Usage{WallMS: 800000, Turns: 57})
	if !ok || r.TimeoutSeconds != 100 || r.MaxTurns != 3 {
		t.Fatalf("%+v", r)
	}
	if r, ok := repairBudgets(spec.Budgets{}, receipt.Usage{WallMS: 1 << 30, Turns: 1000}); !ok || r.TimeoutSeconds != 0 || r.MaxTurns != RepairMaxTurns || r.MaxCostUSD != 0 {
		t.Fatalf("no budgets: %+v", r)
	}
	for _, spent := range []receipt.Usage{{WallMS: 900000}, {Turns: 60}, {CostUSD: 1}} {
		if _, ok := repairBudgets(b, spent); ok {
			t.Errorf("a repair after the first turn spent %+v", spent)
		}
	}
}

func TestANoTimeoutDraftKeepsItsRepairTurnAndCostLimits(t *testing.T) {
	b := spec.Budgets{TimeoutSeconds: 0, MaxTurns: 60, MaxCostUSD: 1}
	for _, tc := range []struct {
		name  string
		spent receipt.Usage
		want  spec.Budgets
		ok    bool
	}{
		{"slow first turn", receipt.Usage{WallMS: 1680000, Turns: 24, CostUSD: 0.25}, spec.Budgets{TimeoutSeconds: 0, MaxTurns: 8, MaxCostUSD: 0.75}, true},
		{"few turns left", receipt.Usage{WallMS: 1680000, Turns: 57, CostUSD: 0.25}, spec.Budgets{TimeoutSeconds: 0, MaxTurns: 3, MaxCostUSD: 0.75}, true},
		{"turns spent", receipt.Usage{Turns: 60}, spec.Budgets{}, false},
		{"cost spent", receipt.Usage{CostUSD: 1}, spec.Budgets{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := repairBudgets(b, tc.spent)
			if ok != tc.ok || (ok && got != tc.want) {
				t.Fatalf("repair budgets = %+v, %t; want %+v, %t", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestARepairKeepsTheFirstAnswerOnlyForARuleItPassed: a second answer is
// not kept when it breaks a rule the first passed (validate's first, then
// the instruction cap), and is kept when it mends what the first broke.
func TestARepairKeepsTheFirstAnswerOnlyForARuleItPassed(t *testing.T) {
	draft := func(passed, long bool) *Draft {
		d := &Draft{Validation: json.RawMessage(fmt.Sprintf(`{"passed":%t}`, passed)), Instructions: "# X\n"}
		if long {
			d.Instructions = strings.Repeat("x", MaxInstructionBytes+1)
		}
		return d
	}
	for _, tc := range []struct {
		name          string
		first, second *Draft
		brokeARule    bool
	}{
		{"a flag's repair that goes past the cap", draft(true, false), draft(true, true), true},
		{"a repair that breaks validate to come within the cap", draft(true, true), draft(false, false), true},
		{"a repair that comes within the cap", draft(true, true), draft(true, false), false},
		{"a repair that mends validate, past the cap", draft(false, false), draft(true, true), false},
		{"a repair that breaks both", draft(true, false), draft(false, true), true},
		{"a repair that changes neither", draft(true, false), draft(true, false), false},
	} {
		if got := brokeARule(tc.first, tc.second); got != tc.brokeARule {
			t.Errorf("%s: brokeARule %v", tc.name, got)
		}
	}
}

// TestADraftThatReadNoPageIsStillHeld: the rules the guide carries are still
// the post-step's, whatever the model read. A first answer from a model that
// called no knowledge tool (its receipts hold no gateway log) and breaks a
// rule of each such page (a bound of manifest/manifest.md, a command off
// manifest/exec.md's list, a choice question without choices from
// manifest/intake-and-guide.md, a pin whose runtime range
// servers/mcp-servers.md excludes) goes to the repair turn with a line for
// each.
func TestADraftThatReadNoPageIsStillHeld(t *testing.T) {
	out := t.TempDir()
	broken := strings.Replace(postDraft, `"summary": "Turns meeting notes into decisions and action items."`,
		`"summary": "`+strings.Repeat("Turns meeting notes into decisions. ", 6)+`"`, 1)
	// A version no registry publishes, given a range no run admits.
	broken = withServer(broken, "99.0.0")
	broken = strings.Replace(broken, `"exec": "none"`, `"exec": {"commands": ["curl"]}`, 1)
	var tasks []string
	p := PostStep{
		OutDir: out, Description: "Summarize my notes.", Task: "Summarize my notes.",
		Scan: webscan.Result{Verdict: webscan.Pass}, Ref: Namespace + "/" + Name + "@" + Version, Checker: &postChecker{},
		Servers: fakeServers(map[string]string{"99.0.0": ">=26.0.0"}),
		Repair: func(_ context.Context, rt RepairTurn) (*receipt.Envelope, error) {
			tasks = append(tasks, rt.Task)
			return &receipt.Envelope{Status: "ok", Result: json.RawMessage(postRepaired)}, nil
		},
	}
	first := firstTurn(t, out, broken)
	if _, err := os.Stat(filepath.Join(out, "receipts", "gateway.log")); !os.IsNotExist(err) {
		t.Fatalf("the first turn has a gateway log: %v", err)
	}
	if _, _, err := p.Run(context.Background(), first, nil); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("%d repair turn(s)", len(tasks))
	}
	_, report, ok := strings.Cut(tasks[0], "## The rules' report")
	if !ok {
		t.Fatalf("the repair task carries no rules' report:\n%s", tasks[0])
	}
	for _, want := range []string{
		// manifest/intake-and-guide.md
		"- V10 intake[0].choices: ",
		// manifest/exec.md's list
		"- capabilities.exec: curl: not a command every machine provides",
		// servers/mcp-servers.md's runtime range
		"- capabilities.mcpServers[0]: npm:@cyanheads/osv-advisory-mcp-server@99.0.0: it needs Node >=26.0.0 (its engines.node), and runs use Node " +
			policy.DefaultNodeVersion,
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the rules' report does not say %q:\n%s", want, report)
		}
	}
	// manifest/manifest.md's bounds are held before the rules run: the
	// summary was shortened at a word, within its bound.
	m := regexp.MustCompile(`"summary": "([^"]*)"`).FindStringSubmatch(report)
	if m == nil || utf8.RuneCountInString(m[1]) > manifest.MaxSummaryChars || !strings.HasSuffix(m[1], "…") {
		t.Errorf("the summary the repair turn is given: %q", m)
	}
}
