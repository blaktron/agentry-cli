package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// stepsHarness plays a docker-agent that works a checklist of steps, one
// tool call a step, keeping its progress in its session store under the
// --session id it is started with, and plays the tools server's half too:
// the calls ledger and the copy of the list in the run's receipts. A start
// given a finish check's message carries on its stored conversation; a start
// given anything else takes it as the task and begins from the first step.
// Start n works perStart[n] steps (all that are left past the end), then
// answers. executed counts each step's tool runs, so a step run twice shows.
type stepsHarness struct {
	steps    []string
	perStart []int
	// noList: the agent keeps no checklist. dropRest: a start that stops
	// short drops the rest with a reason. blocked: the agent ends blocked.
	// exitCode: every start exits so.
	noList, dropRest, blocked bool
	exitCode                  int
	receipts                  string

	mu       sync.Mutex
	store    map[string]int
	starts   []harnessStart
	executed map[int]int
	cost     map[string]float64
}

func (h *stepsHarness) handler(_ context.Context, r dockerx.RunSpec, stdin io.Reader, stdout, _ io.Writer) (int, error) {
	msg, _ := io.ReadAll(stdin)
	conversation := ""
	for i, a := range r.Argv {
		if a == "--session" && i+1 < len(r.Argv) {
			conversation = r.Argv[i+1]
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.store == nil {
		h.store, h.executed, h.cost = map[string]int{}, map[int]int{}, map[string]float64{}
	}
	n := len(h.starts)
	h.starts = append(h.starts, harnessStart{conversation: conversation, message: string(msg)})
	ledger := []string{}
	if n == 0 {
		ledger = append(ledger, startLine)
	}
	done := h.store[conversation]
	if !strings.HasPrefix(string(msg), "Your checklist has open steps:") {
		done = 0
	}
	upto := len(h.steps)
	if n < len(h.perStart) {
		upto = min(len(h.steps), done+h.perStart[n])
	}
	for done < upto {
		h.executed[done]++
		done++
		h.store[conversation] = done
		_, _ = fmt.Fprintf(stdout, `{"type":"tool_call","tool":"read_input","step":%d}`+"\n", done)
		ledger = append(ledger, `{"tool":"read_input","ok":true}`)
	}
	if !h.noList {
		var list []toolsrv.Step
		for i, s := range h.steps {
			st := toolsrv.Step{Text: s, Status: toolsrv.StepPending}
			switch {
			case i < done:
				st.Status = toolsrv.StepDone
			case i == done:
				st.Status = toolsrv.StepInProgress
			}
			if h.dropRest && i >= done {
				st.Status, st.Reason = toolsrv.StepDropped, "out of time"
			}
			list = append(list, st)
		}
		var b strings.Builder
		b.WriteString("# Checklist\n\n")
		for _, st := range list {
			box := map[string]string{toolsrv.StepPending: "- [ ] ", toolsrv.StepInProgress: "- [>] ", toolsrv.StepDone: "- [x] ", toolsrv.StepDropped: "- [-] "}[st.Status]
			b.WriteString(box + st.Text)
			if st.Reason != "" {
				b.WriteString(" (dropped: " + st.Reason + ")")
			}
			b.WriteString("\n")
		}
		_ = os.WriteFile(filepath.Join(h.receipts, ChecklistCopyFile), []byte(b.String()), 0o644)
	}
	if h.blocked {
		ledger = append(ledger, `{"tool":"end_blocked","ok":true,"blocked":"the input is missing"}`)
	}
	appendLines(filepath.Join(h.receipts, CallsLedgerFile), ledger)
	// The conversation's running cost, as docker-agent reports it.
	h.cost[conversation] += 0.01
	cost := h.cost[conversation]
	_, _ = fmt.Fprintf(stdout, `{"type":"stream_stopped","usage":{"input_tokens":100,"output_tokens":10,"cost":%g}}`+"\n", cost)
	_, _ = fmt.Fprintf(stdout, `{"type":"agent_choice","message":{"content":"did %d of %d steps"}}`+"\n", done, len(h.steps))
	return h.exitCode, nil
}

func appendLines(file string, lines []string) {
	if len(lines) == 0 {
		return
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // a test's receipt
	if err != nil {
		return
	}
	_, _ = f.WriteString(strings.Join(lines, "\n") + "\n")
	_ = f.Close()
}

func (h *stepsHarness) snapshot() ([]harnessStart, map[int]int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.starts), h.executed
}

// stepsFixture is a release's one-shot run whose agent container is h.
func stepsFixture(t *testing.T, h *stepsHarness) (Params, *dockerx.Fake) {
	t.Helper()
	shortResumeWaits(t)
	shortCapacityWait(t)
	was := finishCheckMargin
	finishCheckMargin = time.Millisecond
	t.Cleanup(func() { finishCheckMargin = was })
	p, f := fixture(t)
	happyFake(f)
	p.EndBlocked = true
	p.Task = strings.NewReader("work the three steps")
	h.receipts = filepath.Join(p.OutDir, "receipts")
	if h.steps == nil {
		h.steps = []string{"take the inventory", "query each package", "write the report"}
	}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	return p, f
}

// A run that answers with steps open is started again on its conversation
// with the finish check's message, carries on, and ends ok: no step runs
// twice, the usage counts both starts, the ask is announced and kept in the
// receipts, and the envelope says all steps are done.
func TestARunThatAnswersWithOpenStepsIsAskedAboutThem(t *testing.T) {
	h := &stepsHarness{perStart: []int{1}}
	p, f := stepsFixture(t, h)
	var stderr bytes.Buffer
	p.Stderr = &stderr
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, stderr.String())
	}
	starts, executed := h.snapshot()
	if len(starts) != 2 {
		t.Fatalf("the harness started %d times, want 2: %+v", len(starts), starts)
	}
	if starts[1].conversation != starts[0].conversation {
		t.Fatalf("the finish check started conversation %q, not the run's %q", starts[1].conversation, starts[0].conversation)
	}
	want := "Your checklist has open steps:\n- query each package (in progress)\n- write the report (pending)\nContinue them, or drop each one with its reason and finish."
	if starts[1].message != want {
		t.Fatalf("the finish check's message:\n%s\nwant:\n%s", starts[1].message, want)
	}
	for step, n := range executed {
		if n != 1 {
			t.Fatalf("step %d ran %d times", step+1, n)
		}
	}
	if string(env.Result) != `"did 3 of 3 steps"` {
		t.Fatalf("result %s", env.Result)
	}
	if env.Usage.Turns != 3 || env.Usage.InputTokens != 200 {
		t.Fatalf("usage %+v, want the two starts' sum", env.Usage)
	}
	c := env.Checklist
	if c == nil || c.Ending != endingAllDone || c.Steps != 3 || len(c.Open) != 0 || len(c.FinishChecks) != 1 || c.FinishChecks[0].Open != 2 || c.FinishChecks[0].Message != want {
		t.Fatalf("checklist record %+v", c)
	}
	if !strings.Contains(stderr.String(), "finished with 2 checklist step(s) open; starting the harness again on its conversation to ask about them (1 of 2)") {
		t.Fatalf("the ask was not announced: %q", stderr.String())
	}
	agentStderr, _ := os.ReadFile(filepath.Join(p.OutDir, "receipts", "agent-stderr.log"))
	if !strings.Contains(string(agentStderr), "for a finish check (1 of 2): Your checklist has open steps:") {
		t.Fatalf("the harness's receipt does not name the finish check:\n%s", agentStderr)
	}
	events, _ := os.ReadFile(filepath.Join(p.OutDir, "receipts", "events.ndjson"))
	if !strings.Contains(string(events), "did 1 of 3 steps") || !strings.Contains(string(events), "did 3 of 3 steps") {
		t.Fatalf("the events receipt lost a start:\n%s", events)
	}
}

// At most two asks: the third ending stands, the second ask says it is the
// last and asks the answer to name what is left, and the run is ok with the
// open steps in its receipt.
func TestTheThirdEndingStands(t *testing.T) {
	h := &stepsHarness{perStart: []int{0, 0, 0, 0}}
	p, f := stepsFixture(t, h)
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	starts, _ := h.snapshot()
	if len(starts) != 1+finishChecks {
		t.Fatalf("the harness started %d times, want %d", len(starts), 1+finishChecks)
	}
	if strings.Contains(starts[1].message, "last time") || !strings.HasSuffix(starts[2].message, "This is the last time you are asked: name in your answer every step you leave open, and why.") {
		t.Fatalf("the asks:\n%s\n---\n%s", starts[1].message, starts[2].message)
	}
	c := env.Checklist
	if env.ExitCode != 0 || c == nil || len(c.FinishChecks) != 2 || c.Steps != 3 || len(c.Open) != 3 ||
		c.Ending != "finished with 3 open steps: take the inventory; query each package; write the report" {
		t.Fatalf("exit %d, checklist record %+v", env.ExitCode, c)
	}
}

// No ask for a list whose open steps were all dropped with a reason, a run
// that kept no checklist, an agent that ended blocked, or a harness that
// failed; and none that the harness's time left could not hold.
func TestARunIsNotAskedWhenNothingIsOpenOrItCannotBe(t *testing.T) {
	for name, tc := range map[string]struct {
		h        *stepsHarness
		margin   time.Duration
		ending   string
		wantCode int
	}{
		"every open step dropped": {h: &stepsHarness{perStart: []int{1}, dropRest: true}, ending: endingAllDone},
		"no checklist":            {h: &stepsHarness{perStart: []int{1}, noList: true}, ending: endingNoChecklist},
		"ended blocked":           {h: &stepsHarness{perStart: []int{1}, blocked: true}, wantCode: int(ExitBlocked)},
		"a harness failure":       {h: &stepsHarness{perStart: []int{1}, exitCode: 3}, wantCode: int(ExitHarness)},
		"no time left":            {h: &stepsHarness{perStart: []int{1}}, margin: time.Hour, ending: "finished with 2 open steps: query each package; write the report"},
	} {
		t.Run(name, func(t *testing.T) {
			h := tc.h
			p, f := stepsFixture(t, h)
			if tc.margin > 0 {
				finishCheckMargin = tc.margin
			}
			env, err := Run(context.Background(), f, p)
			var ce *ClassifiedError
			switch {
			case tc.wantCode == 0 && err != nil:
				t.Fatalf("run failed: %v", err)
			case tc.wantCode != 0 && (!errors.As(err, &ce) || int(ce.Code) != tc.wantCode):
				t.Fatalf("err %v, want code %d", err, tc.wantCode)
			}
			if starts, _ := h.snapshot(); len(starts) != 1 {
				t.Fatalf("the harness started %d times, want 1", len(starts))
			}
			if env.Checklist == nil || len(env.Checklist.FinishChecks) != 0 {
				t.Fatalf("checklist record %+v", env.Checklist)
			}
			if tc.ending != "" && env.Checklist.Ending != tc.ending {
				t.Fatalf("ending %q, want %q", env.Checklist.Ending, tc.ending)
			}
		})
	}
}

// The turn budget counts every start: a finish check that would carry the
// run past it ends the run on the budget, as a resume does.
func TestAFinishCheckCountsInTheTurnBudget(t *testing.T) {
	h := &stepsHarness{steps: []string{"one", "two", "three", "four", "five"}, perStart: []int{3}}
	p, f := stepsFixture(t, h)
	p.Budgets.MaxTurns = 4
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitBudget {
		t.Fatalf("err %v, want the budget class", err)
	}
	if starts, _ := h.snapshot(); len(starts) != 2 {
		t.Fatalf("the harness started %d times, want 2", len(starts))
	}
}

// So does the cost budget: each start's cost adds to the starts' before it.
func TestAFinishCheckCountsInTheCostBudget(t *testing.T) {
	h := &stepsHarness{perStart: []int{1}}
	p, f := stepsFixture(t, h)
	p.Budgets.MaxCostUSD = 0.015
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitBudget || !strings.Contains(err.Error(), "cost budget") {
		t.Fatalf("err %v, want the cost budget", err)
	}
	if starts, _ := h.snapshot(); len(starts) != 2 {
		t.Fatalf("the harness started %d times, want 2", len(starts))
	}
}

// A copy holding more steps than a list may, or a step longer than one may
// be, is read only as far as a list's bounds; one open step is named as one.
func TestAChecklistCopyIsReadWithinAListsBounds(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("# Checklist\n\n")
	for i := range 150 {
		fmt.Fprintf(&b, "- [ ] step %d %s\n", i, strings.Repeat("é", 400))
	}
	file := filepath.Join(dir, ChecklistCopyFile)
	if err := os.WriteFile(file, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	steps, ok := readChecklistCopy(file)
	if !ok || len(steps) != maxChecklistSteps {
		t.Fatalf("read %d steps (ok %v), want %d", len(steps), ok, maxChecklistSteps)
	}
	for _, st := range steps {
		if len(st.Text) > maxStepText || !utf8.ValidString(st.Text) {
			t.Fatalf("a step of %d bytes (valid %v)", len(st.Text), utf8.ValidString(st.Text))
		}
	}
	if err := os.WriteFile(file, []byte("- [x] one\n- [>] two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rd := &runDirs{checklistCopy: file}
	if rec := checklistRecord(rd); rec.Ending != "finished with 1 open step: two" {
		t.Fatalf("ending %q", rec.Ending)
	}
}

// A run that is no release's one-shot run keeps no copy of its list and
// is never asked: the envelope has no checklist record.
func TestOnlyAReleasesOneShotRunIsAsked(t *testing.T) {
	h := &stepsHarness{perStart: []int{1}}
	p, f := stepsFixture(t, h)
	p.EndBlocked = false
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if starts, _ := h.snapshot(); len(starts) != 1 {
		t.Fatalf("the harness started %d times, want 1", len(starts))
	}
	if env.Checklist != nil {
		t.Fatalf("checklist record %+v", env.Checklist)
	}
}

// The copy's file is made new for each run and stamped into the tools
// spec: an earlier run's list in a reused output folder never stands for
// this one's.
func TestAnEarlierRunsChecklistIsNotThisOnes(t *testing.T) {
	h := &stepsHarness{perStart: []int{3}, noList: true}
	p, f := stepsFixture(t, h)
	receipts := filepath.Join(p.OutDir, "receipts")
	if err := os.MkdirAll(receipts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(receipts, ChecklistCopyFile), []byte("# Checklist\n\n- [ ] left from before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.KeepRunDir = true
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if env.Checklist == nil || env.Checklist.Ending != endingNoChecklist {
		t.Fatalf("checklist record %+v", env.Checklist)
	}
	if starts, _ := h.snapshot(); len(starts) != 1 {
		t.Fatalf("the harness started %d times, want 1", len(starts))
	}
}

// A harness that reports its conversation's running cost, as docker-agent
// does when it continues a stored conversation, is not charged twice for
// it: a run asked once costs what its two starts added, and its tokens and
// tool calls still sum.
func TestAFinishCheckCountsTheConversationsCostOnce(t *testing.T) {
	h := &stepsHarness{perStart: []int{1}}
	p, f := stepsFixture(t, h)
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if starts, _ := h.snapshot(); len(starts) != 2 {
		t.Fatalf("the harness started %d times, want 2", len(starts))
	}
	if env.Usage.CostUSD < 0.0199 || env.Usage.CostUSD > 0.0201 {
		t.Fatalf("cost %v, want 0.02: the conversation's first 0.01 was counted twice", env.Usage.CostUSD)
	}
	if env.Usage.InputTokens != 200 || env.Usage.Turns != 3 {
		t.Fatalf("usage %+v", env.Usage)
	}
}

// A cost budget is judged on that same figure: a run whose two starts add
// up to under the budget is not stopped because its running cost was
// counted twice.
func TestTheCostBudgetCountsTheConversationsCostOnce(t *testing.T) {
	h := &stepsHarness{perStart: []int{1}}
	p, f := stepsFixture(t, h)
	p.Budgets.MaxCostUSD = 0.025
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("run failed: %v", err)
	}
}

// The message names each open step with its status; the last ask adds its
// sentence.
func TestTheFinishCheckMessage(t *testing.T) {
	open := []toolsrv.Step{{Text: "a", Status: toolsrv.StepInProgress}, {Text: "b", Status: toolsrv.StepPending}}
	if got := finishCheckMessage(open, false); got != "Your checklist has open steps:\n- a (in progress)\n- b (pending)\nContinue them, or drop each one with its reason and finish." {
		t.Fatalf("%q", got)
	}
	if got := finishCheckMessage(open, true); !strings.HasSuffix(got, "finish. This is the last time you are asked: name in your answer every step you leave open, and why.") {
		t.Fatalf("%q", got)
	}
}

// A copy that is not a regular file is read as none.
func TestAChecklistCopyThatIsNoFileIsNone(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.md")
	if err := os.WriteFile(target, []byte("- [ ] x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ChecklistCopyFile)
	if err := os.Symlink(target, link); err != nil {
		t.Skip("no symlinks here")
	}
	if _, ok := readChecklistCopy(link); ok {
		t.Fatal("a link was read as the checklist")
	}
	if _, ok := readChecklistCopy(filepath.Join(dir, "missing.md")); ok {
		t.Fatal("a missing copy was read as the checklist")
	}
}
